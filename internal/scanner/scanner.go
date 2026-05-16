// Package scanner implements a polling watched-folder scanner. It walks
// each configured directory on a fixed interval and pushes anything
// that isn't already an ingested recording onto the manual_import_queue
// with a confidence label. The poll-instead-of-fsnotify shape is
// deliberate: the user's Performances library lives on an NFS mount
// where inotify/kqueue are unreliable.
//
// Folder-as-unit model. The scanner walks the watched dir one level
// deep:
//
//   - A loose video file at the root produces one queue entry pointing
//     at the file (the legacy shape).
//   - A folder produces exactly one queue entry. The "main" file (the
//     largest media file anywhere inside the tree, biased toward video
//     and toward the folder root on size ties) becomes the queue row's
//     FilePath; ExtrasCount records how many other media files live in
//     the source folder. The folder structure stays intact on disk —
//     the import path moves only the main file, leaving the extras
//     where they are for the user to deal with later.
//
// The scanner takes no auto-action — that's policy for the UI/CLI. It
// is purely an observer: enqueue when the file is interesting, remove
// when it's gone, skip when it's already been ingested.
package scanner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/ent/recordingversion"
	"github.com/nicolerenee/promptbook/internal/ingest"
	"github.com/nicolerenee/promptbook/internal/match"
	"github.com/nicolerenee/promptbook/internal/rename"
	"github.com/nicolerenee/promptbook/internal/storage"
	pbsync "github.com/nicolerenee/promptbook/internal/sync"
)

// maxTrackedErrors caps how many errors a single Scan pass remembers. A
// permission-error storm on a borked NFS mount could otherwise pin
// memory growth to walk depth × file count; ten samples is plenty for
// a human to diagnose root cause.
const maxTrackedErrors = 10

// defaultInterval is the polling cadence used when Engine.Interval is
// the zero value. One minute is a friendly default for an NFS-backed
// library: buttonpy enough for human iteration, slow enough that a
// missing file briefly disappearing during a network blip won't cause
// queue churn.
const defaultInterval = time.Minute

// Result records the outcome of a single Scan pass over WatchDirs.
// Counts are always populated; Errors holds at most maxTrackedErrors
// samples so a runaway mount can't OOM the process.
type Result struct {
	Enqueued int
	Removed  int
	Skipped  int
	Errors   []error
}

// Engine bundles the dependencies a polling scanner needs. One Engine
// drives one logical watch loop; callers wanting multiple cadences
// should construct multiple Engines.
//
// IsTracked, when non-nil, is consulted before enqueueing each main
// file so a caller can keep storage details out of the scanner. The
// callback should return true when the file is already represented in
// recording_versions (or any other "we already know about this
// recording" signal). When nil, the scanner falls back to its
// internal recording_versions check — the historical scan-incoming
// behavior. The new scan-library-root job supplies a closure here to
// filter orphans against an existing Jellyfin tree without coupling
// the scanner package to ent.
type Engine struct {
	DB        *ent.Client
	WatchDirs []string
	Interval  time.Duration
	Logger    zerolog.Logger
	IsTracked func(path string) bool
	// Encora, when set, lets the scanner auto-fetch + persist a
	// recording from Encora when the sidecar / folder-name encora id
	// resolves but the recording isn't in the local DB. This is the
	// "I'm importing something that isn't in my Encora wants /
	// collection list yet" path — the scanner widens the local
	// catalog by one row + the queue gets a high-confidence
	// suggestion instead of low-confidence.
	//
	// Nil disables the auto-fetch: unknown encora ids stay as
	// low-confidence suggestions and the user resolves via the modal.
	// Defined as an interface (matching jobs/builtin's
	// EncoraRecordingClient) so the construction doesn't pull
	// internal/encora into every scanner test fixture.
	Encora EncoraRecordingClient
	// SQLDB is the raw *database/sql handle paired with DB. Threaded
	// through PersistRecording so the auto-fetch can stamp the
	// recording's encora id into external_ids. Optional: a nil SQLDB
	// just skips the external_ids upsert, the recording still lands.
	SQLDB *sql.DB
}

// EncoraRecordingClient is the slice of *encora.Client the scanner
// needs for its auto-fetch path. Matches the same-named interface in
// internal/jobs/builtin so the production *encora.Client satisfies
// both call sites with no adapter glue.
type EncoraRecordingClient interface {
	Recording(ctx context.Context, id int64) (encora.Recording, encora.RateLimitInfo, error)
}

// Run loops Scan on a ticker until ctx is cancelled. Each pass logs
// its Result and any tracked errors. Returns ctx.Err() on shutdown.
func (e *Engine) Run(ctx context.Context) error {
	interval := e.Interval
	if interval <= 0 {
		interval = defaultInterval
	}

	// Run an immediate pass so callers don't have to wait Interval for
	// the first observation; afterwards the ticker takes over.
	e.runOne(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			e.runOne(ctx)
		}
	}
}

// runOne executes a single Scan pass and logs the outcome. Pulled out
// of Run so the loop body has a clear single responsibility and Run
// stays under the cyclomatic threshold even if more states accrete.
func (e *Engine) runOne(ctx context.Context) {
	res, err := e.Scan(ctx)
	if err != nil {
		e.Logger.Error().Err(err).Msg("scanner pass failed")
		return
	}
	evt := e.Logger.Info().
		Int("enqueued", res.Enqueued).
		Int("removed", res.Removed).
		Int("skipped", res.Skipped)
	if len(res.Errors) > 0 {
		evt = evt.Int("errors", len(res.Errors))
	}
	evt.Msg("scanner pass complete")
	for _, perr := range res.Errors {
		e.Logger.Warn().Err(perr).Msg("scanner per-file error")
	}
}

// Scan runs one full pass: reconcile the queue against disk (drop
// rows whose file is gone), then walk every WatchDir and enqueue or
// skip each video. Pure over (DB, disk) so it's straightforward to
// test without time.
//
// At the end of the pass, every queue row under one of WatchDirs that
// was NOT touched (i.e. last_seen_at is still older than this pass's
// start) is evicted. This gives rescans full-replace semantics: a
// queue row whose backing file still happens to exist but whose
// classification has gone stale (e.g. the folder's largest media
// file changed between passes after an in-flight download settled)
// gets cleaned out so the fresh walk's row stands alone.
func (e *Engine) Scan(ctx context.Context) (Result, error) {
	res := Result{}
	// Stamp the pass start BEFORE any EnqueueFile call. EnqueueFile
	// upserts last_seen_at to time.Now().UTC(), so rows touched during
	// the walk will have last_seen_at >= scanStartedAt; rows we didn't
	// touch keep their older stamp and become eviction candidates.
	scanStartedAt := time.Now().UTC()

	if err := e.reconcileMissing(ctx, &res); err != nil {
		return res, fmt.Errorf("reconcile queue: %w", err)
	}

	for _, dir := range e.WatchDirs {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		e.walkDir(ctx, dir, &res)
	}

	removed, err := storage.DeleteStaleQueueEntriesUnder(ctx, e.DB, e.WatchDirs, scanStartedAt)
	if err != nil {
		// Per-pass eviction failure shouldn't poison the whole Scan
		// result — the next pass will retry — but record it so the
		// operator can see the sweep didn't complete.
		recordError(&res, fmt.Errorf("evict stale queue rows: %w", err))
	}
	res.Removed += removed

	return res, nil
}

// reconcileMissing iterates the current queue and removes entries
// whose file_path no longer stats. This is the scanner's "garbage
// collection" pass — without it, a deleted file would haunt the queue
// forever.
func (e *Engine) reconcileMissing(ctx context.Context, res *Result) error {
	entries, err := storage.ListQueue(ctx, e.DB)
	if err != nil {
		return fmt.Errorf("list queue: %w", err)
	}
	for _, entry := range entries {
		if err = ctx.Err(); err != nil {
			return err
		}
		_, statErr := os.Stat(entry.FilePath)
		if statErr == nil {
			continue
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			recordError(res, fmt.Errorf("stat queue entry %q: %w", entry.FilePath, statErr))
			continue
		}
		if rmErr := storage.RemoveQueueEntryByPath(ctx, e.DB, entry.FilePath); rmErr != nil {
			recordError(res, fmt.Errorf("remove queue entry %q: %w", entry.FilePath, rmErr))
			continue
		}
		res.Removed++
	}
	return nil
}

// audioExtensions are recognized as ingestable companion audio files
// when descending a folder-as-unit. Lower-case, includes dot. The
// scanner only treats audio as a candidate "main file" — it does not
// independently enqueue audio rows. Top-level loose audio at the
// watched-dir root is intentionally ignored: that path is still
// "video files only" because audio-only files at the root are almost
// certainly per-track exports the user dropped while triaging, not
// recordings to import.
//
//nolint:gochecknoglobals // immutable lookup table.
var audioExtensions = map[string]struct{}{
	".mp3":  {},
	".flac": {},
	".wav":  {},
	".m4a":  {},
	".aac":  {},
	".ogg":  {},
	".opus": {},
}

// walkDir scans dir one level deep. Each top-level FILE that's a video
// is enqueued as today (one queue entry, FilePath = the file). Each
// top-level FOLDER becomes one queue entry, with FilePath pointing at
// the largest media file anywhere inside the tree (the "main"
// recording) and ExtrasCount counting the rest. Folders with no media
// at all are skipped. Per-file errors are tracked on res; a missing
// or unreadable root gets recorded as a single error.
func (e *Engine) walkDir(ctx context.Context, dir string, res *Result) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		recordError(res, fmt.Errorf("read watched dir %q: %w", dir, err))
		return
	}
	for _, entry := range entries {
		if cerr := ctx.Err(); cerr != nil {
			return
		}
		path := filepath.Join(dir, entry.Name())
		if entry.IsDir() {
			e.walkFolderUnit(ctx, path, res)
			continue
		}
		if isInFlightDownload(entry.Name()) {
			// rclone / browser / curl partial-download artifact. Skip
			// outright so we don't bake a path that's about to be
			// renamed into a queue row.
			res.Skipped++
			continue
		}
		if _, ok := ingest.VideoExtensions[strings.ToLower(filepath.Ext(path))]; !ok {
			res.Skipped++
			continue
		}
		e.processFile(ctx, path, res, 0, "")
	}
}

// walkFolderUnit treats folder as one recording-with-extras. It walks
// the entire subtree to find every media file, runs the classifier to
// produce a per-file role suggestion, and enqueues a single row whose
// FilePath points at the heuristic-chosen main file (Parts[0]). The
// full Classification rides on the row as a JSON blob so the queue
// import modal can render its multi-file picker without re-walking
// the folder. Empty folders (no media at all) are silently skipped.
//
// extras_count keeps its legacy meaning: the count of NON-main media
// files in the folder (audio + video). For a multipart classification
// that's len(Parts)-1 + len(Extras); for a single-main shape it's
// len(Extras). The accounting holds because every file the classifier
// sees is either a part or an extra.
func (e *Engine) walkFolderUnit(ctx context.Context, folder string, res *Result) {
	// Externally-managed sentinel pre-check. When the folder carries
	// the .promptbook-externally-managed marker AND a .encora-id
	// sidecar that resolves to a known recording, we treat this as a
	// path-drift event (the external tool — Radarr / Plex — renamed
	// the parent folder out from under us) and update the existing
	// recording_versions rows in place rather than enqueueing the
	// folder. An orphan sentinel (no recording match) falls through
	// to the normal scan path so the user can still import the file.
	if e.maybeHandleExternallyManagedDrift(ctx, folder, res) {
		return
	}
	media, err := collectMediaFiles(folder)
	if err != nil {
		recordError(res, fmt.Errorf("walk folder %q: %w", folder, err))
		return
	}
	if len(media) == 0 {
		// Folder has no files at all — nothing to do, including no
		// error. (Doesn't really happen on healthy filesystems but the
		// guard is cheap.)
		e.Logger.Debug().Str("folder", folder).Msg("scanner: empty folder, skipping")
		return
	}
	cls := classifyFolder(folder, media)
	// External-id tags (TMDB / IMDB) live in the folder basename for
	// Radarr-managed pro-shot drops. Parsing here keeps the JSON blob
	// self-contained — the queue-import modal and the eventual ingest
	// engine read the ids straight off Classification.ExternalIDs
	// without re-parsing the path.
	cls.ExternalIDs = ParseExternalIDsFromName(filepath.Base(folder))
	if len(cls.Parts) == 0 {
		// classifyFolder returns no Parts when the folder has files
		// but none are video / audio — a photos-only or
		// scaffolding-only directory. Skip silently; this is normal,
		// not an error condition.
		e.Logger.Debug().Str("folder", folder).
			Msg("scanner: folder has no video / audio, skipping")
		return
	}
	mainPath := cls.Parts[0].Path
	extrasCount := len(cls.Parts) - 1 + len(cls.Extras)
	clsJSON, err := json.Marshal(cls)
	if err != nil {
		// JSON-encoding a struct of strings + ints can only fail on
		// extreme pathology. Record + fall back to enqueueing without
		// the classification rather than dropping the folder entirely.
		recordError(res, fmt.Errorf("encode classification %q: %w", folder, err))
		e.processFile(ctx, mainPath, res, extrasCount, "")
		return
	}
	e.processFile(ctx, mainPath, res, extrasCount, string(clsJSON))
}

// mediaFile is a single hit from collectMediaFiles — the absolute
// path + size of one file inside a folder-as-unit. Covers every
// extension (image, subtitle, document, …) so nothing in the source
// folder gets dropped on import; isVideo / isAudio still gate which
// entries are eligible to BE the recording vs. always-an-extra.
type mediaFile struct {
	path string
	size int64
	// rootLevel is true when path sits at the very top of the folder
	// being walked (depth 0). Used by the classifier as a tie-breaker
	// so a recording at the folder root wins over a similarly-sized
	// track in audio/.
	rootLevel bool
	// isAudio is true when the file's extension is in audioExtensions.
	// Audio files are eligible main candidates (audio-only recordings
	// exist) but lose to video on close-band ties.
	isAudio bool
	// isVideo is true when the file's extension is in VideoExtensions.
	// Used by the classifier to bias toward video formats when sizes
	// are close.
	isVideo bool
}

// metadataSidecarBasenames is the lowercase set of filenames the
// scanner treats as media-server-managed sidecars rather than user
// content. Same effect as the dot-file skip: the classifier never
// sees these so they don't show up as importable extras in the
// queue modal. Currently covers Kodi/Jellyfin/Plex's common image
// sidecars; the .nfo-suffix check below handles any flavour of
// metadata file (movie.nfo, tvshow.nfo, episode-nfos, etc.).
//
//nolint:gochecknoglobals // immutable lookup table.
var metadataSidecarBasenames = map[string]struct{}{
	"poster.jpg":    {},
	"poster.png":    {},
	"fanart.jpg":    {},
	"fanart.png":    {},
	"backdrop.jpg":  {},
	"backdrop.png":  {},
	"clearart.png":  {},
	"clearlogo.png": {},
	"disc.png":      {},
	"landscape.jpg": {},
	"thumb.jpg":     {},
	"banner.jpg":    {},
}

// isMetadataSidecar reports whether the basename is a media-server
// metadata file (.nfo of any flavor or one of the well-known image
// sidecar names) the scanner should ignore. Lets a re-scan of a
// folder where promptbook (or another tool) already wrote a
// movie.nfo + poster.jpg avoid re-queuing those files as importable
// extras.
func isMetadataSidecar(base string) bool {
	low := strings.ToLower(base)
	if strings.HasSuffix(low, ".nfo") {
		return true
	}
	_, ok := metadataSidecarBasenames[low]
	return ok
}

// inFlightDownloadSuffixes is the lowercase set of trailing-extension
// markers that mean "this file is still being written." Matched against
// the *last* extension of the basename so multi-extension forms like
// `Show.mp4.780b8258.partial` (rclone) and `Show.mp4.crdownload`
// (Chrome) are both caught.
//
//nolint:gochecknoglobals // immutable lookup table.
var inFlightDownloadSuffixes = map[string]struct{}{
	".partial":    {}, // rclone default --partial-suffix.
	".part":       {}, // curl, wget, transmission, firefox.
	".crdownload": {}, // chrome / chromium / edge.
	".download":   {}, // safari.
	".filepart":   {}, // kde / kget.
}

// isInFlightDownload reports whether base names a file that's still
// being written by an external transfer tool (rclone, a browser, curl,
// etc.). The scanner must NOT enqueue or classify these — the path is
// about to be renamed and any row keyed on it would be stranded after
// the rename completes. Also catches rclone's `.rclone-tmp*`
// per-transfer temp files via substring match.
func isInFlightDownload(base string) bool {
	low := strings.ToLower(base)
	if _, ok := inFlightDownloadSuffixes[filepath.Ext(low)]; ok {
		return true
	}
	if strings.Contains(low, ".rclone-tmp") {
		return true
	}
	return false
}

// collectMediaFiles walks folder recursively and returns every file
// it finds (video, audio, image, subtitle, document, …) so the
// classifier and the eventual extras-mover can preserve everything
// in the source folder. The struct's isVideo flag still drives the
// main-candidate heuristic — only video / audio rows are eligible
// to be the recording itself; everything else is automatically an
// extra. Hidden / dot-prefixed files (.DS_Store, ._meta, .encora-id)
// are filtered out as OS scaffolding the user doesn't care about.
//
// Per-entry errors are silently absorbed so a single permission
// glitch can't drop the whole folder; only a fatal walker error
// reaches the caller.
func collectMediaFiles(folder string) ([]mediaFile, error) {
	var out []mediaFile
	walkErr := filepath.WalkDir(folder, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			//nolint:nilerr // intentional: keep walking past per-entry errors so a single permission blip doesn't drop the whole folder.
			return nil
		}
		if d.IsDir() {
			return nil
		}
		base := d.Name()
		if strings.HasPrefix(base, ".") {
			// Skip hidden / OS-scaffolding files. The user explicitly
			// asked for "every file" preserved on import, but .DS_Store
			// and .encora-id are noise the user wouldn't keep around
			// even if asked.
			return nil
		}
		if filepath.Dir(path) == folder && isMetadataSidecar(base) {
			// Skip media-server metadata sidecars (movie.nfo,
			// poster.jpg, fanart.jpg, etc.) that sit at the root of
			// the recording folder so a re-scan of a folder where
			// promptbook or another tool already wrote those files
			// doesn't surface them as importable extras. Only filter
			// at the folder root — a `photos/backdrop.jpg` inside the
			// drop is real user content, not a sidecar.
			return nil
		}
		if isInFlightDownload(base) {
			// Partial-download artifact from rclone / a browser / curl.
			// Including it in the folder's classification would bake a
			// path that's about to be renamed into the queue row's
			// classification_json (or, if it happens to be the largest
			// "media" file, into FilePath itself). Skip so the folder
			// is classified from settled files only — the in-flight
			// transfer will land on the next pass once it finishes.
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		_, isVideo := ingest.VideoExtensions[ext]
		_, isAudio := audioExtensions[ext]
		info, infoErr := d.Info()
		if infoErr != nil {
			//nolint:nilerr // intentional: same rationale — keep walking past entry-level info errors.
			return nil
		}
		out = append(out, mediaFile{
			path:      path,
			size:      info.Size(),
			rootLevel: filepath.Dir(path) == folder,
			isVideo:   isVideo,
			isAudio:   isAudio,
		})
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk: %w", walkErr)
	}
	return out, nil
}

// processFile classifies one main file (loose at the watched-dir root
// or the picked main of a folder-as-unit) and dispatches to the right
// queue mutation. extrasCount is the number of OTHER media files
// alongside path inside its source folder; pass 0 for loose-file
// rows. classificationJSON is the JSON-encoded folder classification
// (per-file role suggestions); empty string for loose-file rows or
// when JSON-encoding failed upstream. It treats every "couldn't make
// sense of this file" case as an error tracked on res rather than a
// hard failure — one bad permission shouldn't abort the rest of the
// walk.
func (e *Engine) processFile(
	ctx context.Context, path string, res *Result, extrasCount int, classificationJSON string,
) {
	info, err := os.Stat(path)
	if err != nil {
		recordError(res, fmt.Errorf("stat %q: %w", path, err))
		return
	}

	already, err := e.alreadyTracked(ctx, path)
	if err != nil {
		recordError(res, fmt.Errorf("check recording_versions for %q: %w", path, err))
		return
	}
	if already {
		// File is already an ingested recording version — silently
		// skip per spec.
		res.Skipped++
		return
	}

	entry := storage.QueueEntry{
		FilePath:           path,
		FileSizeBytes:      info.Size(),
		ExtrasCount:        extrasCount,
		ClassificationJSON: classificationJSON,
	}

	id, _, resolveErr := rename.Resolve(path, 0)
	switch {
	case resolveErr == nil:
		idCopy := id
		entry.SuggestedRecordingID = &idCopy
		entry.SuggestedConfidence = e.confidenceFor(ctx, idCopy, res, path)
		// confidenceFor returns "" when the underlying lookup errored
		// and recorded the error; in that case we still enqueue with no
		// suggestion rather than silently dropping the file.
		if entry.SuggestedConfidence == "" {
			entry.SuggestedRecordingID = nil
		}
	case errors.Is(resolveErr, rename.ErrNoEncoraID):
		// No id anywhere — fall back to filename heuristic matching
		// against the local catalog. Most user-uploaded files follow
		// "Show YYYY-MM-DD.mp4" or similar; the matcher pulls show +
		// date out and scores against ent.Recording rows. Top hit
		// becomes the queue's suggestion (with the matcher's
		// confidence label) when the score clears the medium
		// threshold; lower scores leave the entry unsuggested so the
		// UI prompts the human.
		e.applyMatchSuggestion(ctx, path, &entry, res)
	default:
		recordError(res, fmt.Errorf("resolve %q: %w", path, resolveErr))
		return
	}

	if _, err = storage.EnqueueFile(ctx, e.DB, entry); err != nil {
		recordError(res, fmt.Errorf("enqueue %q: %w", path, err))
		return
	}
	res.Enqueued++
}

// applyMatchSuggestion fills entry's SuggestedRecordingID +
// SuggestedConfidence from the heuristic filename matcher.
//
// The pipeline runs the matcher TWICE — once with the filename's
// show as the guess (filling missing date/tour from the folder)
// and once with the folder's show as the guess (filling missing
// date/tour from the filename). The higher-scoring top candidate
// wins. This handles two failure modes cleanly:
//   - Folder carries the metadata, file is "ACT 1.mp4" / "bows.mp4"
//     — the folder-show variant scores; the filename-show variant
//     either misses or scores lower.
//   - File carries the metadata, folder is "10-26-2023" or empty
//     — the filename-show variant scores.
//
// Errors are recorded on res but don't abort processing — a queue
// entry with no suggestion is still useful (the human picks).
func (e *Engine) applyMatchSuggestion(
	ctx context.Context, path string, entry *storage.QueueEntry, res *Result,
) {
	fileParsed := match.Parse(filepath.Base(path))
	folderParsed := match.Parse(filepath.Base(filepath.Dir(path)))

	matcher := match.New(e.DB)

	withFileShow := mergedWithShow(fileParsed.ShowGuess,
		fileParsed.Tour, fileParsed, folderParsed)
	withFolderShow := mergedWithShow(folderParsed.ShowGuess,
		folderParsed.Tour, fileParsed, folderParsed)

	top, err := bestMatch(ctx, matcher, withFileShow, withFolderShow)
	if err != nil {
		recordError(res, fmt.Errorf("match %q: %w", path, err))
		return
	}
	if top == nil {
		return
	}
	conf := top.Confidence()
	if conf == match.ConfidenceLow {
		// Don't pin a low-confidence guess to the queue row — the
		// UI's "match" button auto-imports against the suggestion,
		// and a wrong auto-import is worse than no suggestion.
		return
	}
	idCopy := top.RecordingID
	entry.SuggestedRecordingID = &idCopy
	entry.SuggestedConfidence = conf
}

// mergedWithShow builds a Parsed using the supplied show + tour
// values, filling everything else from the union of file + folder.
// Lets applyMatchSuggestion try one show value while still using
// the richest possible date / source / flag context.
func mergedWithShow(show, tour string, file, folder match.Parsed) match.Parsed {
	out := match.Parsed{ShowGuess: show, Tour: tour}
	switch {
	case file.Date.HasYear() && folder.Date.HasYear() && file.Date.HasMonth():
		out.Date = file.Date
	case file.Date.HasYear() && folder.Date.HasMonth() && !file.Date.HasMonth():
		// Filename had only year; folder has month — prefer folder.
		out.Date = folder.Date
	case file.Date.HasYear():
		out.Date = file.Date
	case folder.Date.HasYear():
		out.Date = folder.Date
	}
	if file.Source != "" {
		out.Source = file.Source
	} else {
		out.Source = folder.Source
	}
	out.IsMaster = file.IsMaster || folder.IsMaster
	out.IsMatinee = file.IsMatinee || folder.IsMatinee
	out.IsPreview = file.IsPreview || folder.IsPreview
	// Part markers come from the FILE — that's what differentiates
	// sibling rips in the same folder. Folder-level part markers
	// don't really exist.
	out.PartIndex = file.PartIndex
	out.PartKind = file.PartKind
	return out
}

// bestMatch runs the matcher on each candidate Parsed and returns
// whichever produced the highest-scoring top hit. Returns nil when
// neither attempt produced any candidates. Skips empty-show inputs.
func bestMatch(
	ctx context.Context, m *match.Matcher, candidates ...match.Parsed,
) (*match.Candidate, error) {
	var best *match.Candidate
	for _, p := range candidates {
		if p.ShowGuess == "" {
			continue
		}
		results, err := m.Match(ctx, p)
		if err != nil {
			return nil, err
		}
		if len(results) == 0 {
			continue
		}
		if best == nil || results[0].Score > best.Score {
			c := results[0]
			best = &c
		}
	}
	return best, nil
}

// confidenceFor maps a resolved encora id to a confidence label by
// asking the local DB whether we know about that recording. A query
// error is recorded on res and surfaced as the empty confidence so the
// caller can still enqueue the file (just without a suggestion).
//
// When the recording isn't in the local DB AND the engine has an
// Encora client wired, the scanner fans out a per-recording detail
// fetch + persist so the queue row's suggestion is high-confidence.
// Use case: a file lands in the watch dir with a .encora-id pointing
// at a recording the user hasn't collected or wanted on Encora —
// without the auto-fetch the queue row would be a low-confidence
// orphan-with-suggestion that the user has to manually look up.
// Fetch failures (rate limit, network blip, recording removed from
// Encora) fall back to low confidence so the file still enqueues.
func (e *Engine) confidenceFor(ctx context.Context, id int64, res *Result, path string) string {
	_, err := storage.LoadRecording(ctx, e.DB, id)
	if err == nil {
		return storage.ConfidenceHigh
	}
	if errors.Is(err, storage.ErrRecordingNotFound) {
		if e.fetchAndPersistRecording(ctx, id, path) {
			return storage.ConfidenceHigh
		}
		return storage.ConfidenceLow
	}
	recordError(res, fmt.Errorf("load recording %d for %q: %w", id, path, err))
	return ""
}

// fetchAndPersistRecording resolves an Encora recording id against
// the upstream API + persists it locally so the next scanner pass +
// the queue's suggestedRecording resolver find it. Returns true when
// the recording landed in the local DB; false on any failure path
// (no Encora client, fetch error, persist error). Failures log at
// warn — the caller falls back to low-confidence enqueue so the
// file still appears in the queue.
func (e *Engine) fetchAndPersistRecording(ctx context.Context, id int64, path string) bool {
	if e.Encora == nil {
		e.Logger.Debug().
			Int64("recording_id", id).
			Str("path", path).
			Msg("scanner: encora client not configured; cannot auto-fetch unknown id")
		return false
	}
	rec, _, err := e.Encora.Recording(ctx, id)
	if err != nil {
		e.Logger.Warn().Err(err).
			Int64("recording_id", id).
			Str("path", path).
			Msg("scanner: encora detail fetch failed; queue row stays low-confidence")
		return false
	}
	if perr := pbsync.PersistRecording(ctx, e.DB, e.SQLDB, rec, time.Now); perr != nil {
		e.Logger.Warn().Err(perr).
			Int64("recording_id", id).
			Str("path", path).
			Msg("scanner: persist auto-fetched recording failed")
		return false
	}
	e.Logger.Info().
		Int64("recording_id", id).
		Str("path", path).
		Msg("scanner: auto-fetched recording from encora (not in user's wants / collection)")
	return true
}

// alreadyTracked decides whether the scanner should skip path because
// it's already part of a tracked recording. When the Engine has an
// IsTracked callback set, it's the source of truth (the callback is
// expected to consult storage on its own, so the scanner doesn't need
// to know about ent here). Otherwise we fall back to the historical
// inline recording_versions lookup — the scan-incoming default.
func (e *Engine) alreadyTracked(ctx context.Context, path string) (bool, error) {
	if e.IsTracked != nil {
		return e.IsTracked(path), nil
	}
	return versionExistsForPath(ctx, e.DB, path)
}

// versionExistsForPath returns true if any recording_versions row
// already references the exact file_path. The scanner uses this to
// silently skip files that have already been ingested.
func versionExistsForPath(ctx context.Context, client *ent.Client, path string) (bool, error) {
	exists, err := client.RecordingVersion.Query().
		Where(recordingversion.FilePath(path)).
		Exist(ctx)
	if err != nil {
		return false, fmt.Errorf("query recording_versions: %w", err)
	}
	return exists, nil
}

// recordError appends err to res.Errors, capping at maxTrackedErrors
// so a permission-error storm can't OOM the process. Errors past the
// cap are dropped on the floor — the count in the log line still
// communicates the storm to the operator.
func recordError(res *Result, err error) {
	if len(res.Errors) >= maxTrackedErrors {
		return
	}
	res.Errors = append(res.Errors, err)
}

// maybeHandleExternallyManagedDrift reconciles a folder that carries
// the .promptbook-externally-managed sentinel against the local
// recording_versions table. Returns true when the folder was handled
// (the caller skips its normal walk-and-enqueue logic); false when
// the folder lacks the sentinel OR the sidecar's recording id
// doesn't resolve (orphan sentinel — fall through so the user can
// re-import the file via the queue).
//
// Drift handling: for each video file in the folder, if a
// recording_versions row exists for this recording_id whose
// basename matches the on-disk file (so multipart parts get matched
// by their part filename) and whose current file_path differs, the
// row's file_path is updated in place. The sentinel itself is left
// alone — it survives the scan so a future drift event still
// triggers this branch.
func (e *Engine) maybeHandleExternallyManagedDrift(
	ctx context.Context, folder string, res *Result,
) bool {
	sentinel := filepath.Join(folder, ingest.ExternallyManagedSentinel)
	if _, err := os.Stat(sentinel); err != nil {
		return false
	}
	id, _, resolveErr := rename.Resolve(folder, 0)
	if resolveErr != nil {
		// Orphan sentinel (no .encora-id, or unparseable). Fall
		// through to the normal scan so the user can re-establish
		// the link via the queue.
		e.Logger.Debug().Err(resolveErr).Str("folder", folder).
			Msg("scanner: externally-managed sentinel without resolvable encora id; ignoring")
		return false
	}
	if _, lookupErr := storage.LoadRecording(ctx, e.DB, id); lookupErr != nil {
		if errors.Is(lookupErr, storage.ErrRecordingNotFound) {
			// Recording isn't in the local DB. Treat as orphan and
			// fall through so the queue picks it up the regular way.
			e.Logger.Debug().Int64("encora_id", id).Str("folder", folder).
				Msg("scanner: externally-managed sentinel points at unknown recording; ignoring")
			return false
		}
		recordError(res, fmt.Errorf(
			"externally-managed lookup for %d in %q: %w", id, folder, lookupErr))
		// Fail-safe: handled=true so the folder isn't enqueued on a
		// transient DB error. The next scan retries.
		return true
	}
	e.reconcileExternallyManagedFolder(ctx, folder, id, res)
	return true
}

// reconcileExternallyManagedFolder walks every video file inside
// folder and updates the corresponding recording_versions.file_path
// when the row's recorded path differs. Matches by recording_id +
// basename so multipart parts (act-1.mkv, act-2.mkv) each align
// against their own row.
func (e *Engine) reconcileExternallyManagedFolder(
	ctx context.Context, folder string, recordingID int64, res *Result,
) {
	versions, err := storage.ListVersions(ctx, e.DB, recordingID)
	if err != nil {
		recordError(res, fmt.Errorf(
			"list versions for externally-managed recording %d: %w", recordingID, err))
		return
	}
	if len(versions) == 0 {
		// No version rows yet — nothing to drift. The folder may be a
		// stale sentinel from a deleted import; leave it alone rather
		// than enqueueing it (the user explicitly opted into externally-
		// managed mode).
		return
	}
	versionByBase := make(map[string]storage.RecordingVersion, len(versions))
	for _, v := range versions {
		versionByBase[filepath.Base(v.FilePath)] = v
	}

	walkErr := filepath.WalkDir(folder, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			//nolint:nilerr // intentional: keep walking past per-entry errors so a single permission blip doesn't drop the whole folder.
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if _, ok := ingest.VideoExtensions[strings.ToLower(filepath.Ext(path))]; !ok {
			return nil
		}
		base := filepath.Base(path)
		row, ok := versionByBase[base]
		if !ok {
			// No row matches this filename. Could be a new file added
			// to an externally-managed folder; phase 4 only handles
			// drift, so leave the file alone.
			return nil
		}
		if row.FilePath == path {
			res.Skipped++
			return nil
		}
		oldPath := row.FilePath
		if upsertErr := storage.UpdateVersionFilePath(ctx, e.DB, row.ID, path); upsertErr != nil {
			recordError(res, fmt.Errorf(
				"update file_path for recording_versions row %d: %w", row.ID, upsertErr))
			// Per-entry failures don't abort the walk — a single
			// permission blip shouldn't drop every other part. The
			// recordError call above captured the diagnostic.
			return nil
		}
		e.Logger.Info().
			Int64("recording_id", recordingID).
			Str("from", oldPath).
			Str("to", path).
			Msg("scanner: externally-managed path drift — updated recording_versions")
		// Count the drift as a "skip" from the queue's perspective:
		// no enqueue happened, no error happened. The dedicated drift
		// signal lives in the log line.
		res.Skipped++
		return nil
	})
	if walkErr != nil {
		recordError(res, fmt.Errorf(
			"walk externally-managed folder %q: %w", folder, walkErr))
	}
}
