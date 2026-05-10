// Package ingest orchestrates the full library workflow: encora-id
// resolution, recording lookup, canonical rename, optional subtitle
// download, and NFO write — for one file or every video under a tree.
package ingest

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/externalids"
	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/match"
	"github.com/nicolerenee/promptbook/internal/nfo"
	"github.com/nicolerenee/promptbook/internal/probe"
	"github.com/nicolerenee/promptbook/internal/rename"
	"github.com/nicolerenee/promptbook/internal/storage"
	syncpkg "github.com/nicolerenee/promptbook/internal/sync"
)

// VideoExtensions are recognized as ingestable. Lower-case, includes
// dot. Case-insensitive comparison via strings.EqualFold at call site.
// Exported so sibling packages (e.g. scanner) can share the same set
// without re-declaring it.
//
//nolint:gochecknoglobals // immutable lookup table
var VideoExtensions = map[string]struct{}{
	".mp4":  {},
	".mkv":  {},
	".mov":  {},
	".avi":  {},
	".m4v":  {},
	".webm": {},
	".vob":  {},
	".ts":   {},
	".mts":  {},
}

// Client is the encora subset ingest needs. Defined as an interface so
// tests can pass a stub that doesn't open a real network socket.
type Client interface {
	Recording(ctx context.Context, id int64) (encora.Recording, encora.RateLimitInfo, error)
	Subtitles(ctx context.Context, id int64) ([]encora.Subtitle, encora.RateLimitInfo, error)
	AddToCollection(ctx context.Context, id int64) (encora.RateLimitInfo, error)
}

// Engine bundles ingest dependencies. One Engine handles many paths.
type Engine struct {
	DB *ent.Client
	// SQLDB is the underlying *sql.DB sharing DB's connection pool.
	// Used by the externalids persistence path (no ent type for that
	// table — see internal/externalids/externalids.go for the
	// rationale). Optional: when nil, ingest skips the external_ids
	// upsert and logs a debug-level warning. The CLI / server wiring
	// always supplies it; tests that don't exercise the external-id
	// path can leave it nil.
	SQLDB             *sql.DB
	Client            Client
	LibraryRoot       string
	FolderTemplate    string
	FileTemplate      string
	SubtitleFetcher   SubtitleFetcher
	InteractiveReader io.Reader
	Logger            zerolog.Logger
	// ImageCache is the optional on-disk poster/backdrop cache. When
	// configured, the NFO writer emits <thumb> / <fanart> hints pointing
	// at the locally-cached files. nil (or a Disabled cache) leaves
	// those elements omitted — Jellyfin/Plex fall back to upstream
	// scrapes.
	ImageCache *imagecache.Cache
	// PublicURL is the externally-reachable base URL of the promptbook
	// server. When set, NFOs written after a successful ingest carry
	// absolute /images/* URLs for poster, fanart, and per-actor
	// headshots so a remote media server can fetch them over HTTP.
	// Empty preserves the local-sibling-file behaviour for movie images
	// and skips actor thumbs entirely.
	PublicURL string
	// Prober extracts codec/resolution metadata from the source file
	// for the new {Container} / {VideoCodec} / {Quality} rename tokens.
	// Required: ingest fails the item with a probe error if Prober is
	// nil or the probe call returns an error. Tests inject a stub;
	// production wiring is probe.FFProbe{Path: cfg.Library.FFProbePath}.
	Prober probe.Prober
	// ProtectedDirs is the absolute-path set the post-apply cleanup
	// refuses to remove even when empty. Populated with the configured
	// library.root + every library.incomingDirs entry — the engine
	// will happily clean up emptied sub-folders inside those roots
	// (legacy show folders left behind after a rename, etc.) but
	// never the roots themselves. Empty / nil disables the cleanup
	// entirely; tests don't need to opt in.
	ProtectedDirs []string
}

// Options tunes a single Ingest invocation.
type Options struct {
	// FlagEncoraID, if non-zero, overrides discovery for a single-file
	// SRC. Returns an error if SRC is a directory.
	FlagEncoraID int
	DryRun       bool
	Interactive  bool
	// AddToCollection: after a recording lands locally, also POST to
	// /collection/{id}/collect so the upstream collection picks it up.
	// Auto-fetch of unknown ids into the local DB happens regardless of
	// this flag — this only governs the optional Encora WRITE.
	AddToCollection bool
	// SourceFolder, when non-empty, marks this single-file ingest as a
	// folder-as-unit drop. The path is stamped onto the
	// recording_versions row's source_folder column so the recording
	// detail page can later enumerate sibling files (audio/, photos/,
	// etc.) as 'extras' even though Plan.Apply only moves the main
	// file. Empty (default) means the ingest is a loose-file import
	// and the recording's destination folder is the only location with
	// content.
	SourceFolder string
	// FileAssignments, when non-empty, switches the engine into
	// multi-file mode: each entry routes one source path to a role
	// (main / part-N / extra-{kind} / skip) so the importer can land
	// multipart recordings (act-1 + act-2) and typed extras
	// (featurettes, audio rips, photos, ...) in one ingest call.
	//
	// When nil / empty, ingest runs the legacy single-file flow —
	// SRC's whole content is the main file, no extras processed —
	// preserving today's behaviour exactly for callers that don't yet
	// know about multipart/extras.
	FileAssignments []FileAssignment
	// ExternallyManaged, when true, runs the catalog-only ingest
	// flow: the source file is NOT moved, no movie.nfo is written,
	// no subtitles are fetched. The recording_versions row's
	// file_path is the source path verbatim; .encora-id and the
	// .promptbook-externally-managed sentinel get written next to
	// the source so a later scan can recover the link if the
	// external tool renames the folder. The recordings.externally_managed
	// flag is flipped to true on the recording row before / after
	// the upsert.
	//
	// Used when an external tool (Radarr/Plex/Jellyfin) owns the
	// files on disk and promptbook is purely a catalog. Multi-file
	// (parts + extras) is supported — every part stays at its source
	// path; the sentinel goes in the parent folder; extras stay in
	// place as well.
	ExternallyManaged bool
	// ExternalIDs are the third-party provider ids the caller wants
	// stamped onto the recording's external_ids table (TMDB / IMDB /
	// future providers). Each entry's RecordingID is overwritten with
	// the resolved recording id at persistence time, so callers can
	// pass through the scanner-supplied list verbatim (the scanner
	// stamps RecordingID=0 at classify time).
	//
	// Empty / nil leaves the external_ids table untouched. Encora's
	// own row is upserted by the sync hook in sync.PersistRecording
	// independently of this list, so the row is in place regardless
	// of what additional providers the caller supplies. Skipped when
	// ingest didn't successfully resolve a recording id (no row to
	// anchor the upsert on).
	ExternalIDs []externalids.ExternalID
}

// FileAssignment routes one source file to a role inside a multi-
// file ingest. Kind values:
//
//   - "main"            — singleton main file (legacy single-file shape).
//   - "part-1", "part-2", … — parts of one multipart version. All parts
//     share the recording id; each gets its own version row with
//     part_index set.
//   - "extra-featurette", "extra-scene", "extra-behindthescenes",
//     "extra-interview", "extra-trailer", "extra-deletedscenes",
//     "extra-other", "extra-audio", "extra-photo" — non-main media
//     moved into a Jellyfin-shaped subfolder under the canonical
//     recording folder.
//   - "skip"            — leave the file in the source folder untouched.
//     The empty-source-folder cleanup won't fire when skipped files
//     remain.
type FileAssignment struct {
	SourcePath string
	Kind       string
	// Label is the optional user-supplied display label for an
	// extra. Empty for main / part / skip assignments.
	Label string
}

// Action constants for ItemResult.Action.
const (
	ActionMoved     = "moved"
	ActionWouldMove = "would-move"
	ActionSkipped   = "skipped"
)

// ExternallyManagedSentinel is the empty-file marker promptbook drops
// next to the source file when an externally-managed import lands.
// The scanner reads this on a later pass to detect "this folder is
// already an externally-managed recording" — even if the external
// tool (Radarr/Plex) renamed the parent folder since the original
// import — and updates recording_versions.file_path in place rather
// than re-enqueueing the file.
//
// The file is intentionally empty: the .encora-id sidecar already
// carries the recording id, and the sentinel only signals "don't
// enqueue me." Keeping it empty also means the external tool's own
// scanners won't trip on unexpected metadata content.
const ExternallyManagedSentinel = ".promptbook-externally-managed"

// ItemResult records what happened (or would happen) for one video.
type ItemResult struct {
	Source        string
	EncoraID      int64
	ResolvedFrom  rename.ResolveSource
	Recording     *encora.Recording
	Plan          *rename.Plan
	NFOPath       string
	SubtitlePaths []string
	SkippedReason string
	Action        string // "moved" / "would-move" / "skipped"
	Err           error
	// AutoAdded is true when lookupOrAdd auto-fetched the recording from
	// Encora (i.e. it was not previously present in the local DB). Used by
	// history-event recording to phrase the summary correctly.
	AutoAdded bool
	// MediaInfo is the probe.MediaInfo captured during buildPlan. Held
	// here so recordVersion can persist the JSON-encoded blob alongside
	// the new version row without re-running ffprobe. Zero value when
	// the probe didn't run (early-skip pipelines).
	MediaInfo probe.MediaInfo
	// SourceFolder mirrors Options.SourceFolder onto the per-item state
	// so recordVersion can stamp the recording_versions.source_folder
	// column without threading Options through every helper. Empty for
	// loose-file ingests.
	SourceFolder string
	// PartIndex is the 1-based ordinal stamped onto the
	// recording_versions row when this file is one part of a multipart
	// version. Zero (the default) means single-file. Only the multi-
	// file ingest path sets this; the legacy single-file path leaves
	// it at zero so the rename engine's filename-derived Part value
	// stays the source of truth for the {Part} token.
	PartIndex int
	// AppliedExtras is the list of recording_extras rows the multi-
	// file ingest path successfully wrote alongside this main /
	// part-1 file. Used by the GraphQL mutation surface to surface
	// per-extra outcomes; empty for the legacy single-file flow.
	AppliedExtras []AppliedExtra
	// ExternallyManaged mirrors Options.ExternallyManaged onto the
	// per-item state so the apply / recordVersion helpers can branch
	// without threading Options through every call site. The
	// catalog-only path skips file moves, NFO writes, and subtitle
	// fetches; it still probes media info and writes the .encora-id
	// + sentinel sidecars next to the source file.
	ExternallyManaged bool
	// ExternalIDs mirrors Options.ExternalIDs onto the per-item state
	// so the NFO writer (in applyPlan) can emit one <uniqueid> per
	// provider without re-threading Options through every callee.
	// Empty for callers that didn't supply ids; the writer falls back
	// to the legacy single-Encora shape in that case.
	ExternalIDs []externalids.ExternalID
}

// AppliedExtra is one extras row written during a multi-file ingest.
// Surfaced on the main / part-1 ItemResult so the caller (GraphQL
// importQueueEntry mutation, CLI, etc.) can render per-extra
// outcomes without re-querying the DB.
type AppliedExtra struct {
	SourcePath string
	DestPath   string
	Kind       string
	Label      string
	Err        error
}

// Result aggregates per-item outcomes.
type Result struct {
	Items []ItemResult
}

// Ingest walks src and runs each video through the pipeline. src may be
// a file or a directory. Errors on individual items are recorded on the
// item but don't abort the rest of the walk.
//
// When opts.FileAssignments is non-empty, ingest enters multi-file
// mode: each assignment routes one file to a role (main / part-N /
// extra-{kind} / skip) so the same entry point can land a multipart
// recording (act-1 + act-2) plus typed extras (featurettes / audio /
// photos / ...) in one call. The legacy single-file flow stays
// untouched when assignments is empty.
func (e *Engine) Ingest(ctx context.Context, src string, opts Options) (*Result, error) {
	if len(opts.FileAssignments) > 0 {
		res, err := e.ingestWithAssignments(ctx, src, opts)
		if err != nil {
			return res, err
		}
		e.persistExternalIDs(ctx, res, opts)
		return res, nil
	}

	src = filepath.Clean(src)
	info, err := os.Stat(src)
	if err != nil {
		return nil, fmt.Errorf("stat src: %w", err)
	}

	if !info.IsDir() {
		item := e.ingestOne(ctx, src, opts)
		res := &Result{Items: []ItemResult{item}}
		e.persistExternalIDs(ctx, res, opts)
		return res, nil
	}

	if opts.FlagEncoraID != 0 {
		return nil, errors.New("--encora-id only valid for single-file SRC")
	}

	res := &Result{}
	walkErr := filepath.WalkDir(src, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if _, ok := VideoExtensions[strings.ToLower(filepath.Ext(path))]; !ok {
			return nil
		}
		res.Items = append(res.Items, e.ingestOne(ctx, path, opts))
		return nil
	})
	if walkErr != nil {
		return res, fmt.Errorf("walk src: %w", walkErr)
	}
	e.persistExternalIDs(ctx, res, opts)
	return res, nil
}

// persistExternalIDs writes opts.ExternalIDs into the external_ids
// table for every successfully-ingested recording in the batch. Each
// supplied entry has its RecordingID overwritten with the resolved
// id from the first non-zero EncoraID in the result, since one
// ingest call lands one recording (multipart or single-file, but
// always one recording id). Skipped silently when opts.ExternalIDs
// is empty, when no main / part item landed (no recording to anchor
// the rows on), or when SQLDB is nil (test fixtures).
//
// Failures are logged at warn level and dropped; the main ingest
// flow has already moved files + written version rows by the time we
// get here, so a row-write failure shouldn't fail the whole call.
// The next sync / ingest cycle will retry the upsert (UpsertMany is
// idempotent under the composite PK).
func (e *Engine) persistExternalIDs(ctx context.Context, res *Result, opts Options) {
	if len(opts.ExternalIDs) == 0 {
		return
	}
	if e.SQLDB == nil {
		e.Logger.Debug().Msg("ingest: skipping external_ids upsert (SQLDB not configured)")
		return
	}
	var recordingID int64
	for _, item := range res.Items {
		if item.EncoraID > 0 && item.Action != ActionSkipped {
			recordingID = item.EncoraID
			break
		}
	}
	if recordingID == 0 {
		// No recording landed in the batch — no row to anchor the
		// external ids on.
		return
	}
	rows := make([]externalids.ExternalID, 0, len(opts.ExternalIDs))
	for _, eid := range opts.ExternalIDs {
		if eid.Provider == "" || eid.ExternalID == "" {
			continue
		}
		rows = append(rows, externalids.ExternalID{
			RecordingID: recordingID,
			Provider:    eid.Provider,
			ExternalID:  eid.ExternalID,
		})
	}
	if len(rows) == 0 {
		return
	}
	if err := externalids.UpsertMany(ctx, e.SQLDB, rows); err != nil {
		e.Logger.Warn().Err(err).
			Int64("recording_id", recordingID).
			Int("rows", len(rows)).
			Msg("ingest: failed to upsert external_ids")
	}
}

// ingestOne runs the full pipeline for a single video path.
func (e *Engine) ingestOne(ctx context.Context, src string, opts Options) ItemResult {
	return e.ingestOneWithSeed(ctx, src, opts, ItemResult{
		Source:            src,
		SourceFolder:      opts.SourceFolder,
		ExternallyManaged: opts.ExternallyManaged,
		ExternalIDs:       opts.ExternalIDs,
	})
}

// ingestOneWithSeed is ingestOne with a caller-supplied initial
// ItemResult so the multi-file path can pre-populate PartIndex (and
// any other per-file overrides) before the pipeline runs. The legacy
// single-file flow uses ingestOne directly with a zero seed.
func (e *Engine) ingestOneWithSeed(
	ctx context.Context, src string, opts Options, seed ItemResult,
) ItemResult {
	item := seed
	item.Source = src
	// ExternallyManaged is an Options-driven, per-call switch — never a
	// per-file override. Always trust the option so a callerseed that
	// forgot to set it doesn't accidentally drop into the
	// move-the-file flow.
	item.ExternallyManaged = opts.ExternallyManaged
	// ExternalIDs are an Options-level value shared across all parts
	// of a multi-file ingest, but the NFO writer reads them off the
	// per-item state. Stamp them here so the multi-file path
	// (assignments.go) doesn't need to pre-populate the seed.
	item.ExternalIDs = opts.ExternalIDs

	if !e.resolveID(src, opts, &item) {
		e.recordIngestEvent(ctx, opts, &item)
		return item
	}
	if !e.lookupRecording(ctx, opts, &item) {
		e.recordIngestEvent(ctx, opts, &item)
		return item
	}
	if !e.buildPlan(ctx, src, &item) {
		e.recordIngestEvent(ctx, opts, &item)
		return item
	}
	if opts.DryRun {
		e.fillDryRunPaths(&item)
		return item
	}
	e.applyPlan(ctx, &item)
	e.recordIngestEvent(ctx, opts, &item)
	return item
}

// resolveID handles encora-id discovery; returns false to short-circuit
// the pipeline. On false the caller returns item to the result list.
func (e *Engine) resolveID(src string, opts Options, item *ItemResult) bool {
	id, source, resolveErr := rename.Resolve(src, opts.FlagEncoraID)
	if errors.Is(resolveErr, rename.ErrNoEncoraID) && opts.Interactive {
		var promptErr error
		id, source, promptErr = e.promptForID(src)
		if promptErr != nil {
			item.SkippedReason = "no encora id supplied"
			item.Action = ActionSkipped
			return false
		}
		resolveErr = nil
	}
	if resolveErr != nil {
		item.Err = resolveErr
		item.SkippedReason = "no encora id"
		item.Action = ActionSkipped
		return false
	}
	item.EncoraID = id
	item.ResolvedFrom = source
	return true
}

func (e *Engine) lookupRecording(ctx context.Context, opts Options, item *ItemResult) bool {
	recording, autoAdded, err := e.lookupOrAdd(ctx, item.EncoraID, opts)
	if err != nil {
		item.Err = err
		item.SkippedReason = err.Error()
		item.Action = ActionSkipped
		return false
	}
	item.Recording = recording
	item.AutoAdded = autoAdded
	return true
}

func (e *Engine) buildPlan(ctx context.Context, src string, item *ItemResult) bool {
	if e.Prober == nil {
		item.Err = errors.New("ingest: prober not configured")
		item.SkippedReason = "prober not configured"
		item.Action = ActionSkipped
		return false
	}
	info, perr := e.Prober.Probe(ctx, src)
	if perr != nil {
		item.Err = fmt.Errorf("probe %s: %w", src, perr)
		item.SkippedReason = "probe failed"
		item.Action = ActionSkipped
		return false
	}
	item.MediaInfo = info
	// Caller-supplied PartIndex (multi-file ingest) wins over the
	// filename-derived value: the queue modal's per-file picker is
	// the authoritative source when present. Falls back to the
	// legacy match.Parse path for the single-file flow.
	part := item.PartIndex
	if part == 0 {
		part = match.Parse(filepath.Base(src)).PartIndex
	}
	plan, err := rename.BuildPlan(rename.PlanInputs{
		Recording:      *item.Recording,
		Source:         src,
		LibraryRoot:    e.LibraryRoot,
		FolderTemplate: e.FolderTemplate,
		FileTemplate:   e.FileTemplate,
		MediaInfo:      info,
		Part:           part,
	})
	if err != nil {
		item.Err = err
		item.Action = ActionSkipped
		return false
	}
	item.Plan = plan
	return true
}

func (e *Engine) fillDryRunPaths(item *ItemResult) {
	item.Action = ActionWouldMove
	if item.ExternallyManaged {
		// Externally-managed dry-run: no move, no NFO, no subtitles.
		// The "destination" for the upcoming write is the source path
		// itself; the SPA's preview surfaces that as "File stays at
		// source · {src}" so the user sees what's about to happen.
		return
	}
	item.NFOPath = nfo.MovieNFOPathForPlan(*item.Plan)
	if item.Recording.Metadata.HasSubtitles {
		item.SubtitlePaths = e.plannedSubtitlePaths(item.Plan)
	}
}

func (e *Engine) applyPlan(ctx context.Context, item *ItemResult) {
	if item.ExternallyManaged {
		e.applyExternallyManaged(ctx, item)
		return
	}
	// Plan.Apply moves ONLY the source file into the canonical library
	// destination. When the queue row was a folder-as-unit drop (the
	// scanner picked the main recording out of a folder that also
	// holds per-track audio rips, photos, etc.), the extras stay in
	// the source folder untouched. Surfacing them as proper "extras"
	// in Jellyfin is a future feature; for now the user manages those
	// files manually after the main recording lands.
	srcParent := filepath.Dir(item.Plan.Source)
	if _, applyErr := item.Plan.Apply(); applyErr != nil {
		item.Err = applyErr
		item.Action = ActionSkipped
		return
	}
	if sidecarErr := item.Plan.EnsureSidecar(item.EncoraID); sidecarErr != nil {
		e.Logger.Warn().Err(sidecarErr).Msg("failed to write sidecar")
	}
	item.Action = ActionMoved

	// Best-effort cleanup of an emptied source folder. Folder-as-unit
	// drops with leftover extras leave files behind, which protects
	// them; legacy library folders (one .mp4 in a [encora-N] folder
	// being renamed to the new template) end up empty after Apply
	// and would otherwise litter the library forever. Failures are
	// logged at debug — a non-empty parent or a permission issue
	// isn't worth surfacing.
	e.maybeRemoveEmptyDir(srcParent)

	e.recordVersion(ctx, item)

	if item.Recording.Metadata.HasSubtitles && e.SubtitleFetcher != nil {
		paths, subErr := e.SubtitleFetcher.Fetch(ctx, e.Client, *item.Recording, *item.Plan)
		if subErr != nil {
			e.Logger.Warn().Err(subErr).Msg("subtitle fetch failed")
		}
		item.SubtitlePaths = paths
	}

	nfoPath, nfoErr := nfo.WriteRecordingFile(
		ctx,
		item.Plan.AbsoluteFolder(),
		*item.Recording,
		nfo.WriteOptions{
			DB:          e.DB,
			Cache:       e.ImageCache,
			PublicURL:   e.PublicURL,
			ExternalIDs: item.ExternalIDs,
		},
	)
	if nfoErr != nil {
		item.Err = nfoErr
		return
	}
	item.NFOPath = nfoPath
}

// applyExternallyManaged is the catalog-only apply path: leave the
// source file in place, persist a recording_versions row that points
// at the source path, write the .encora-id sidecar + the
// .promptbook-externally-managed sentinel next to the source so a
// later scan can recognize the file as externally managed (even if
// the external tool renames the folder), and flip the recording row's
// externally_managed flag. NFO writing + subtitle fetching are
// skipped — the catalog-only flow is read-only on disk except for the
// two sidecar files.
func (e *Engine) applyExternallyManaged(ctx context.Context, item *ItemResult) {
	srcParent := filepath.Dir(item.Source)
	if sidecarErr := writeEncoraIDSidecar(srcParent, item.EncoraID); sidecarErr != nil {
		e.Logger.Warn().Err(sidecarErr).Str("dir", srcParent).
			Msg("failed to write .encora-id sidecar for externally-managed import")
	}
	if sentinelErr := writeExternallyManagedSentinel(srcParent); sentinelErr != nil {
		e.Logger.Warn().Err(sentinelErr).Str("dir", srcParent).
			Msg("failed to write externally-managed sentinel")
	}
	item.Action = ActionMoved
	e.recordVersion(ctx, item)
	if flagErr := storage.SetRecordingExternallyManaged(
		ctx, e.DB, item.EncoraID, true,
	); flagErr != nil {
		// The file is in place + the version row exists; failing the
		// flag flip would leave a misleading ingest result. Surface as
		// a soft error (item.Err set, action remains "moved") so the
		// caller can retry the toggle without re-importing.
		item.Err = fmt.Errorf("set externally_managed flag: %w", flagErr)
	}
}

// maybeRemoveEmptyDir checks dir and removes it when empty, unless
// dir is one of the configured ProtectedDirs (the watched roots
// themselves) or the path is missing. Errors get debug-logged and
// dropped — emptied-source-folder cleanup is a polish, not load-
// bearing.
func (e *Engine) maybeRemoveEmptyDir(dir string) {
	if dir == "" {
		return
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		e.Logger.Debug().Err(err).Str("dir", dir).
			Msg("post-apply: resolve abs path")
		return
	}
	for _, p := range e.ProtectedDirs {
		pAbs, perr := filepath.Abs(p)
		if perr == nil && pAbs == abs {
			return
		}
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		e.Logger.Debug().Err(err).Str("dir", abs).
			Msg("post-apply: read source dir")
		return
	}
	if len(entries) > 0 {
		return
	}
	if rmErr := os.Remove(abs); rmErr != nil {
		e.Logger.Debug().Err(rmErr).Str("dir", abs).
			Msg("post-apply: remove empty source dir")
		return
	}
	e.Logger.Info().Str("dir", abs).
		Msg("post-apply: removed emptied source folder")
}

// recordVersion writes a recording_versions row for the file we just
// moved into the canonical library. Best-effort: a failure here does not
// roll back the move (the file is already in place and other consumers
// can recover it via a future scan), so we log a warning and move on.
func (e *Engine) recordVersion(ctx context.Context, item *ItemResult) {
	// Externally-managed imports leave the file at the source path;
	// the version row tracks that location verbatim so a later
	// detail-page render or rename-preview reads the right file. The
	// move-the-file path uses Plan.AbsoluteFile() instead.
	dest := item.Source
	if !item.ExternallyManaged {
		dest = item.Plan.AbsoluteFile()
	}
	info, err := os.Stat(dest)
	if err != nil {
		e.Logger.Warn().Err(err).Str("path", dest).Msg("failed to stat ingested file")
		return
	}

	// Use the original source filename for codec/quality heuristics: the
	// canonical target name is template-driven and rarely carries the
	// release tags we're sniffing for.
	sourceName := filepath.Base(item.Source)
	container := strings.ToLower(strings.TrimPrefix(filepath.Ext(dest), "."))
	quality := ParseQuality(sourceName)
	videoCodec := ParseVideoCodec(sourceName)
	audioCodec := ParseAudioCodec(sourceName)
	size := info.Size()

	version := storage.RecordingVersion{
		RecordingID:   item.EncoraID,
		FilePath:      dest,
		FileSizeBytes: size,
		Container:     container,
		Quality:       quality,
		VideoCodec:    videoCodec,
		AudioCodec:    audioCodec,
		FormatLabel:   DefaultFormatLabel(container, quality, videoCodec, size),
		MediaInfoJSON: encodeMediaInfo(item.MediaInfo, e.Logger),
		SourceFolder:  item.SourceFolder,
		PartIndex:     item.PartIndex,
	}
	if upsertErr := storage.UpsertVersion(ctx, e.DB, version); upsertErr != nil {
		e.Logger.Warn().Err(upsertErr).Msg("failed to record version")
	}
}

// writeEncoraIDSidecar drops a .encora-id sidecar in dir. Mirrors
// rename.Plan.EnsureSidecar but works against an arbitrary directory
// (the source folder for externally-managed imports) rather than the
// canonical library folder. Idempotent: an existing sidecar is
// overwritten with the same id.
func writeEncoraIDSidecar(dir string, id int64) error {
	path := filepath.Join(dir, rename.SidecarFilename)
	content := fmt.Sprintf("%d\n", id)
	if err := os.WriteFile(path, []byte(content), externallyManagedSidecarPerm); err != nil {
		return fmt.Errorf("write encora-id sidecar %s: %w", path, err)
	}
	return nil
}

// writeExternallyManagedSentinel drops the empty sentinel file in
// dir. The scanner reads this on later passes to recognize the folder
// as already-imported externally-managed even when the external tool
// (Radarr/Plex) renamed the parent folder out from under us.
func writeExternallyManagedSentinel(dir string) error {
	path := filepath.Join(dir, ExternallyManagedSentinel)
	if err := os.WriteFile(path, nil, externallyManagedSidecarPerm); err != nil {
		return fmt.Errorf("write externally-managed sentinel %s: %w", path, err)
	}
	return nil
}

// externallyManagedSidecarPerm matches rename.libraryFilePerm
// (world-readable) so downstream consumers running as different uids
// can still read the sidecar / sentinel files. 0o644 mirrors the
// rename engine's choice rather than introducing a new convention.
const externallyManagedSidecarPerm = 0o644

// encodeMediaInfo JSON-encodes the probe.MediaInfo blob for
// persistence on the recording_versions row. A marshal failure is
// treated as a soft error: the version row still persists with an
// empty media_info_json so the file move isn't blocked. The detail
// page hides the media-info card when the JSON is empty / unparseable.
func encodeMediaInfo(info probe.MediaInfo, logger zerolog.Logger) string {
	// Empty MediaInfo is the sentinel for "probe didn't run" or the
	// caller skipped it; persist empty string so the GraphQL resolver
	// returns null mediaInfo.
	if info.VideoCodec == "" && info.Width == 0 && info.Height == 0 &&
		len(info.AudioStreams) == 0 && len(info.SubtitleStreams) == 0 &&
		info.DurationSeconds == 0 {
		return ""
	}
	b, err := json.Marshal(info)
	if err != nil {
		logger.Warn().Err(err).Msg("failed to encode media info; persisting empty blob")
		return ""
	}
	return string(b)
}

// recordIngestEvent persists a single history row summarizing the
// outcome of an ingestOne pipeline run. Best-effort: a DB write failure
// is logged at warn level but does not fail the ingest, since history
// is observability — not the source of truth.
//
// Skips recording entirely for:
//   - Dry-run invocations (no real action took place).
//   - "no encora id" skips, which are noisy false starts during a
//     directory walk (a scan of a tree typically encounters a lot of
//     un-ID'd files; persisting one row per such file would drown the
//     audit trail).
func (e *Engine) recordIngestEvent(ctx context.Context, opts Options, item *ItemResult) {
	if opts.DryRun {
		return
	}
	if item.Action == ActionSkipped && item.SkippedReason == "no encora id" {
		return
	}

	event := storage.HistoryEvent{
		Kind:    storage.HistoryKindIngest,
		Summary: ingestEventSummary(item),
		Details: ingestEventDetails(item),
	}
	if item.EncoraID > 0 {
		id := item.EncoraID
		event.RecordingID = &id
	}

	if _, err := storage.RecordEvent(ctx, e.DB, event); err != nil {
		e.Logger.Warn().Err(err).
			Int64("encora_id", item.EncoraID).
			Str("action", item.Action).
			Msg("failed to record history event")
	}
}

// ingestEventSummary builds the one-line human-readable description of
// what happened to the item.
func ingestEventSummary(item *ItemResult) string {
	switch item.Action {
	case ActionMoved:
		if item.AutoAdded && item.Recording != nil {
			return fmt.Sprintf("Auto-fetched and orphaned %s", item.Recording.Show)
		}
		if item.Plan != nil {
			return fmt.Sprintf("Moved %s -> %s", item.Source, item.Plan.AbsoluteFile())
		}
		return fmt.Sprintf("Moved %s", item.Source)
	case ActionSkipped:
		if item.SkippedReason != "" {
			return fmt.Sprintf("Skipped %s: %s", item.Source, item.SkippedReason)
		}
		return fmt.Sprintf("Skipped %s", item.Source)
	default:
		return fmt.Sprintf("%s %s", item.Action, item.Source)
	}
}

// ingestEventDetails builds the structured details map persisted as
// JSON alongside the history event. Keys are stable so future readers
// can rely on them.
func ingestEventDetails(item *ItemResult) map[string]any {
	details := map[string]any{
		"action":         item.Action,
		"source":         item.Source,
		"encora_id":      item.EncoraID,
		"subtitle_count": len(item.SubtitlePaths),
	}
	if item.Plan != nil {
		details["dest"] = item.Plan.AbsoluteFile()
	}
	if item.NFOPath != "" {
		details["nfo_path"] = item.NFOPath
	}
	if item.Err != nil {
		details["error"] = item.Err.Error()
	}
	if item.AutoAdded {
		details["auto_added"] = true
	}
	if item.ResolvedFrom != "" {
		details["resolved_from"] = string(item.ResolvedFrom)
	}
	return details
}

// lookupOrAdd reads from the local DB; if missing it auto-fetches the
// recording via /recording/{id} from Encora and persists it as an orphan
// (no collection / wants membership). If opts.AddToCollection is set, it
// then POSTs /collection/{id}/collect so the upstream collection picks it up
// — that flag now ONLY governs the optional Encora write, not whether the
// auto-fetch runs.
//
// The returned bool is true when the recording was freshly auto-fetched
// from Encora (i.e. was not previously cached locally). Returns a clear
// error when Encora itself doesn't know the id (404).
func (e *Engine) lookupOrAdd(
	ctx context.Context,
	id int64,
	opts Options,
) (*encora.Recording, bool, error) {
	loaded, err := storage.LoadRecording(ctx, e.DB, id)
	if err == nil {
		return &loaded.Recording, false, nil
	}
	if !errors.Is(err, storage.ErrRecordingNotFound) {
		return nil, false, fmt.Errorf("load recording: %w", err)
	}

	recording, _, fetchErr := e.Client.Recording(ctx, id)
	if errors.Is(fetchErr, encora.ErrNotFound) {
		return nil, false, fmt.Errorf("recording %d doesn't exist in Encora", id)
	}
	if fetchErr != nil {
		return nil, false, fmt.Errorf("fetch recording %d: %w", id, fetchErr)
	}

	if perr := syncpkg.PersistRecording(ctx, e.DB, e.SQLDB, recording, time.Now); perr != nil {
		return nil, false, fmt.Errorf("persist recording %d: %w", id, perr)
	}

	if opts.AddToCollection {
		if _, addErr := e.Client.AddToCollection(ctx, id); addErr != nil {
			return nil, false, fmt.Errorf("add to collection: %w", addErr)
		}
	}
	return &recording, true, nil
}

// plannedSubtitlePaths is what *would* be written without actually
// downloading. Used by --dry-run for human inspection.
func (e *Engine) plannedSubtitlePaths(plan *rename.Plan) []string {
	return []string{
		filepath.Join(plan.AbsoluteFolder(), plan.TargetFile+".eng.srt"),
	}
}

// promptForID asks stdin for an encora id when --interactive is on and
// nothing else resolved.
func (e *Engine) promptForID(src string) (int64, rename.ResolveSource, error) {
	if e.InteractiveReader == nil {
		return 0, "", errors.New("no interactive reader configured")
	}
	if _, err := fmt.Fprintf(os.Stderr, "encora id for %s? (blank to skip): ", src); err != nil {
		return 0, "", err
	}
	var input string
	if _, err := fmt.Fscanln(e.InteractiveReader, &input); err != nil {
		return 0, "", fmt.Errorf("read input: %w", err)
	}
	input = strings.TrimSpace(input)
	if input == "" {
		return 0, "", errors.New("empty input")
	}
	id, err := storage.ParseRecordingID(input)
	if err != nil {
		return 0, "", err
	}
	return id, rename.ResolveSource("interactive"), nil
}
