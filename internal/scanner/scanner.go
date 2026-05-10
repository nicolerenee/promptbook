// Package scanner implements a polling watched-folder scanner. It walks
// each configured directory on a fixed interval, classifies every video
// file it sees, and pushes anything that isn't already an ingested
// recording onto the manual_import_queue with a confidence label. The
// poll-instead-of-fsnotify shape is deliberate: the user's Performances
// library lives on an NFS mount where inotify/kqueue are unreliable.
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

// walkDir descends dir and processes each video file. Per-file errors
// are tracked on res; a directory walk error (permissions, missing
// root) gets recorded as a single error and the walk aborts for that
// directory.
func (e *Engine) walkDir(ctx context.Context, dir string, res *Result) {
	walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			recordError(res, fmt.Errorf("walk %q: %w", path, walkErr))
			// Returning nil on a per-entry error keeps the walk going for
			// the remaining files; only a fatal walk failure (the root
			// itself unreadable) reaches the outer return below.
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if _, ok := ingest.VideoExtensions[strings.ToLower(filepath.Ext(path))]; !ok {
			res.Skipped++
			return nil
		}
		e.processFile(ctx, path, res)
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, context.Canceled) {
		recordError(res, fmt.Errorf("walk root %q: %w", dir, walkErr))
	}
}

// processFile classifies one video and dispatches to the right
// queue-mutation. It treats every "couldn't make sense of this file"
// case as an error tracked on res rather than a hard failure — one
// bad permission shouldn't abort the rest of the walk.
func (e *Engine) processFile(ctx context.Context, path string, res *Result) {
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
		// No id anywhere — leave both fields zero so the UI knows the
		// human has to pick from scratch.
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
