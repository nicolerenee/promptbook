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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/ent/recordingversion"
	"github.com/nicolerenee/promptbook/internal/ingest"
	"github.com/nicolerenee/promptbook/internal/match"
	"github.com/nicolerenee/promptbook/internal/rename"
	"github.com/nicolerenee/promptbook/internal/storage"
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
type Engine struct {
	DB        *ent.Client
	WatchDirs []string
	Interval  time.Duration
	Logger    zerolog.Logger
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
func (e *Engine) Scan(ctx context.Context) (Result, error) {
	res := Result{}

	if err := e.reconcileMissing(ctx, &res); err != nil {
		return res, fmt.Errorf("reconcile queue: %w", err)
	}

	for _, dir := range e.WatchDirs {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		e.walkDir(ctx, dir, &res)
	}

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
		if _, ok := ingest.VideoExtensions[strings.ToLower(filepath.Ext(path))]; !ok {
			res.Skipped++
			continue
		}
		e.processFile(ctx, path, res, 0)
	}
}

// walkFolderUnit treats folder as one recording-with-extras. It walks
// the entire subtree to find every media file, picks the "main" file
// using mainFile's heuristic, and enqueues a single row pointing at
// it. ExtrasCount is the count of OTHER media files (audio + video)
// found alongside the main one. Empty folders (no media at all) are
// silently skipped.
//
// TODO: multi-part folder handling. When two video files inside the
// folder are similar size and their filenames carry part markers
// ({Part}-1, act 1 / act 2, pt-1 / pt-2), this is one recording split
// across two files — both should ride into ingest together. For now
// we still pick the largest as main and let the user sort it out
// during import.
func (e *Engine) walkFolderUnit(ctx context.Context, folder string, res *Result) {
	media, err := collectMediaFiles(folder)
	if err != nil {
		recordError(res, fmt.Errorf("walk folder %q: %w", folder, err))
		return
	}
	if len(media) == 0 {
		// Folder has no media — nothing to do, including no error.
		// This skips per-show poster-only drops and similar scaffolding
		// the user may have left in incoming/.
		e.Logger.Debug().Str("folder", folder).Msg("scanner: empty folder, skipping")
		return
	}
	main := mainFile(media)
	extras := len(media) - 1
	e.processFile(ctx, main, res, extras)
}

// mediaFile is a single hit from collectMediaFiles — the absolute
// path + size of one video/audio file inside a folder-as-unit.
type mediaFile struct {
	path string
	size int64
	// rootLevel is true when path sits at the very top of the folder
	// being walked (depth 0). Used by mainFile as a tie-breaker so a
	// recording at the folder root wins over a similarly-sized track in
	// audio/.
	rootLevel bool
	// isVideo is true when the file's extension is in VideoExtensions.
	// Used by mainFile to bias toward video formats when sizes are
	// close.
	isVideo bool
}

// collectMediaFiles walks folder recursively and returns every video
// + audio file it finds. Non-media files (jpg, txt, srt, nfo, …) are
// skipped — they're never main-file candidates. Per-entry errors are
// silently absorbed so a single permission glitch can't drop the
// whole folder; only a fatal walker error reaches the caller.
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
		ext := strings.ToLower(filepath.Ext(path))
		_, isVideo := ingest.VideoExtensions[ext]
		_, isAudio := audioExtensions[ext]
		if !isVideo && !isAudio {
			return nil
		}
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
		})
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk: %w", walkErr)
	}
	return out, nil
}

// mainFile picks the "main" recording out of a folder-as-unit's media
// hits. Heuristic:
//
//   - Largest by size wins (the recording itself halflings per-track
//     extracts in nearly every real-world drop pattern).
//   - When the next-largest is within 10% of the leader, prefer a
//     file at the folder root over one in a subdirectory (audio/),
//     and then prefer video over audio. The 10% band is wide enough
//     to forgive container-overhead differences without letting a
//     single per-track audio rip masquerade as the main file.
//
// media must be non-empty — callers gate on len(media) == 0 first.
func mainFile(media []mediaFile) string {
	// Find the leader by size first.
	leader := media[0]
	for _, m := range media[1:] {
		if m.size > leader.size {
			leader = m
		}
	}
	// Then sweep for any candidate within 10% of the leader's size and
	// apply the root-level / video-preferred tie-breakers. The 10%
	// band is symmetric: candidate.size * 1.10 >= leader.size.
	const (
		closeBandPct = 10
		percentDenom = 100
	)
	threshold := leader.size - leader.size*closeBandPct/percentDenom
	for _, m := range media {
		if m.path == leader.path {
			continue
		}
		if m.size < threshold {
			continue
		}
		// Within the close band — apply tie-breakers.
		if betterMain(m, leader) {
			leader = m
		}
	}
	return leader.path
}

// betterMain returns true when candidate beats current as the main
// file under the close-size tie-breaker rules: prefer a root-level
// file over a nested one, and prefer a video over an audio file when
// the root-level state is equal.
func betterMain(candidate, current mediaFile) bool {
	if candidate.rootLevel && !current.rootLevel {
		return true
	}
	if !candidate.rootLevel && current.rootLevel {
		return false
	}
	if candidate.isVideo && !current.isVideo {
		return true
	}
	return false
}

// processFile classifies one main file (loose at the watched-dir root
// or the picked main of a folder-as-unit) and dispatches to the right
// queue mutation. extrasCount is the number of OTHER media files
// alongside path inside its source folder; pass 0 for loose-file
// rows. It treats every "couldn't make sense of this file" case as an
// error tracked on res rather than a hard failure — one bad
// permission shouldn't abort the rest of the walk.
func (e *Engine) processFile(ctx context.Context, path string, res *Result, extrasCount int) {
	info, err := os.Stat(path)
	if err != nil {
		recordError(res, fmt.Errorf("stat %q: %w", path, err))
		return
	}

	already, err := versionExistsForPath(ctx, e.DB, path)
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
		FilePath:      path,
		FileSizeBytes: info.Size(),
		ExtrasCount:   extrasCount,
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
func (e *Engine) confidenceFor(ctx context.Context, id int64, res *Result, path string) string {
	_, err := storage.LoadRecording(ctx, e.DB, id)
	if err == nil {
		return storage.ConfidenceHigh
	}
	if errors.Is(err, storage.ErrRecordingNotFound) {
		return storage.ConfidenceLow
	}
	recordError(res, fmt.Errorf("load recording %d for %q: %w", id, path, err))
	return ""
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
