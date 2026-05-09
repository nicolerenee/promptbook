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

	"github.com/rs/zerolog"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/nfo"
	"github.com/nicolerenee/promptbook/internal/rename"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// videoExtensions are recognized as ingestable. Lower-case, includes
// dot. Case-insensitive comparison via strings.EqualFold at call site.
//
//nolint:gochecknoglobals // immutable lookup table
var videoExtensions = map[string]struct{}{
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
	// AddToCollection: if a recording is missing from the local DB, POST
	// to /collection/{id}/collect (via the encora client) and re-sync
	// via a follow-up Recording fetch. Mock-only during the overnight run.
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
		if _, ok := videoExtensions[strings.ToLower(filepath.Ext(path))]; !ok {
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
		return item
	}
	if !e.lookupRecording(ctx, opts, &item) {
		return item
	}
	if !e.buildPlan(src, &item) {
		return item
	}
	if opts.DryRun {
		e.fillDryRunPaths(&item)
		return item
	}
	e.applyPlan(ctx, &item)
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
	recording, err := e.lookupOrAdd(ctx, item.EncoraID, opts)
	if err != nil {
		item.Err = err
		item.SkippedReason = err.Error()
		item.Action = ActionSkipped
		return false
	}
	item.Recording = recording
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

// lookupOrAdd reads from the local DB; if missing and AddToCollection is
// set, posts /collection/{id}/collect and refetches /recording/{id}.
// During the overnight run the AddToCollection path is exercised only by
// mock-backed tests — there's no real encora key in scope.
func (e *Engine) lookupOrAdd(
	ctx context.Context,
	id int64,
	opts Options,
) (*encora.Recording, error) {
	loaded, err := storage.LoadRecording(ctx, e.DB, id)
	if err == nil {
		return &loaded.Recording, nil
	}
	if !errors.Is(err, storage.ErrRecordingNotFound) {
		return nil, fmt.Errorf("load recording: %w", err)
	}
	if !opts.AddToCollection {
		return nil, fmt.Errorf("recording %d not in cache (use --add-to-collection)", id)
	}
	if _, addErr := e.Client.AddToCollection(ctx, id); addErr != nil {
		return nil, fmt.Errorf("add to collection: %w", addErr)
	}
	// Caller is expected to re-sync after this. For the overnight run
	// we skip the re-fetch and surface a clear error so the user can
	// run `collection sync` manually.
	return nil, fmt.Errorf("recording %d added to collection — run `promptbook collection sync` and re-ingest", id)
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
