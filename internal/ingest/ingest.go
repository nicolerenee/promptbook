// Package ingest orchestrates the full library workflow: encora-id
// resolution, recording lookup, canonical rename, optional subtitle
// download, and NFO write — for one file or every video under a tree.
package ingest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/nfo"
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
	DB                *sql.DB
	Client            Client
	LibraryRoot       string
	FolderTemplate    string
	FileTemplate      string
	SubtitleFetcher   SubtitleFetcher
	InteractiveReader io.Reader
	Logger            zerolog.Logger
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
}

// Action constants for ItemResult.Action.
const (
	ActionMoved     = "moved"
	ActionWouldMove = "would-move"
	ActionSkipped   = "skipped"
)

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
}

// Result aggregates per-item outcomes.
type Result struct {
	Items []ItemResult
}

// Ingest walks src and runs each video through the pipeline. src may be
// a file or a directory. Errors on individual items are recorded on the
// item but don't abort the rest of the walk.
func (e *Engine) Ingest(ctx context.Context, src string, opts Options) (*Result, error) {
	src = filepath.Clean(src)
	info, err := os.Stat(src)
	if err != nil {
		return nil, fmt.Errorf("stat src: %w", err)
	}

	if !info.IsDir() {
		item := e.ingestOne(ctx, src, opts)
		return &Result{Items: []ItemResult{item}}, nil
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
	return res, nil
}

// ingestOne runs the full pipeline for a single video path.
func (e *Engine) ingestOne(ctx context.Context, src string, opts Options) ItemResult {
	item := ItemResult{Source: src}

	if !e.resolveID(src, opts, &item) {
		e.recordIngestEvent(ctx, opts, &item)
		return item
	}
	if !e.lookupRecording(ctx, opts, &item) {
		e.recordIngestEvent(ctx, opts, &item)
		return item
	}
	if !e.buildPlan(src, &item) {
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

func (e *Engine) buildPlan(src string, item *ItemResult) bool {
	plan, err := rename.BuildPlan(rename.PlanInputs{
		Recording:      *item.Recording,
		Source:         src,
		LibraryRoot:    e.LibraryRoot,
		FolderTemplate: e.FolderTemplate,
		FileTemplate:   e.FileTemplate,
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
	item.NFOPath = nfo.MovieNFOPathForPlan(*item.Plan)
	if item.Recording.Metadata.HasSubtitles {
		item.SubtitlePaths = e.plannedSubtitlePaths(item.Plan)
	}
}

func (e *Engine) applyPlan(ctx context.Context, item *ItemResult) {
	if _, applyErr := item.Plan.Apply(); applyErr != nil {
		item.Err = applyErr
		item.Action = ActionSkipped
		return
	}
	if sidecarErr := item.Plan.EnsureSidecar(item.EncoraID); sidecarErr != nil {
		e.Logger.Warn().Err(sidecarErr).Msg("failed to write sidecar")
	}
	item.Action = ActionMoved

	e.recordVersion(ctx, item)

	if item.Recording.Metadata.HasSubtitles && e.SubtitleFetcher != nil {
		paths, subErr := e.SubtitleFetcher.Fetch(ctx, e.Client, *item.Recording, *item.Plan)
		if subErr != nil {
			e.Logger.Warn().Err(subErr).Msg("subtitle fetch failed")
		}
		item.SubtitlePaths = paths
	}

	nfoPath, nfoErr := nfo.WriteFile(item.Plan.AbsoluteFolder(), nfo.FromRecording(*item.Recording))
	if nfoErr != nil {
		item.Err = nfoErr
		return
	}
	item.NFOPath = nfoPath
}

// recordVersion writes a recording_versions row for the file we just
// moved into the canonical library. Best-effort: a failure here does not
// roll back the move (the file is already in place and other consumers
// can recover it via a future scan), so we log a warning and move on.
func (e *Engine) recordVersion(ctx context.Context, item *ItemResult) {
	dest := item.Plan.AbsoluteFile()
	info, err := os.Stat(dest)
	if err != nil {
		e.Logger.Warn().Err(err).Str("path", dest).Msg("failed to stat moved file")
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
	}
	if upsertErr := storage.UpsertVersion(ctx, e.DB, version); upsertErr != nil {
		e.Logger.Warn().Err(upsertErr).Msg("failed to record version")
	}
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

	if perr := syncpkg.PersistRecording(ctx, e.DB, recording, time.Now); perr != nil {
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
