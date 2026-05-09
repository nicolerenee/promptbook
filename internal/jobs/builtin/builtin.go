// Package builtin holds the concrete Job implementations the server
// registers at boot. Each adapter wraps an existing engine (sync,
// scanner, image cache) so the scheduler doesn't need to know about
// Encora rate limits or NFS mounts — it just calls Run(ctx) and
// records the outcome.
//
// Image-refresh fan-out: refresh-encora reads paginated /collection +
// /wants and upserts DB rows. After the sync settles it walks the
// catalog and enqueues per-entity refresh-show-images,
// refresh-recording-images, and refresh-actor-headshot jobs for
// anything missing a slot file. The actual image downloads happen on
// those follow-up jobs, not inside the sync transaction — that keeps
// the transactional surface tight and lets the scheduler interleave
// image work with other passes (scan-incoming, etc.).
package builtin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/rs/zerolog"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/jobs"
	"github.com/nicolerenee/promptbook/internal/scanner"
	pbsync "github.com/nicolerenee/promptbook/internal/sync"
)

// RefreshEncoraJob is the scheduled wrapper around sync.Sync. The
// burst-reserve floor and pause-between-pages are read from the same
// config the CLI uses, so the rate-limit policy is identical between
// `promptbook collection sync` and the in-process scheduler.
//
// Image fetching has moved to per-entity refresh jobs: this job
// upserts rows and then fans out refresh-show-images,
// refresh-recording-images, and refresh-actor-headshot for entities
// missing a slot file on disk.
type RefreshEncoraJob struct {
	DB           *sql.DB
	Client       *encora.Client
	Logger       zerolog.Logger
	BurstReserve int
	// Cache + Enqueuer drive the fan-out. Both must be non-nil for the
	// fan-out to fire; either nil leaves the sync intact and skips the
	// follow-up enqueues.
	Cache    *imagecache.Cache
	Enqueuer jobs.Enqueuer
}

// Name is the registry key for this job.
func (j *RefreshEncoraJob) Name() string { return "refresh-encora" }

// Run invokes pbsync.Sync, then (when an Enqueuer + cache are wired)
// enqueues per-entity refresh-* jobs for anything missing a slot file.
// Ignores args — refresh-encora is a periodic full-collection sync.
func (j *RefreshEncoraJob) Run(ctx context.Context, _ jobs.JobArgs) error {
	if j.Client == nil {
		return errors.New("refresh-encora: encora client not configured")
	}
	_, err := pbsync.Sync(ctx, j.Client, j.DB, pbsync.Options{
		BurstReserve: j.BurstReserve,
		Logger:       j.Logger,
	})
	if err != nil {
		return fmt.Errorf("refresh-encora sync: %w", err)
	}

	// Fan out per-entity image jobs for entities missing a slot file.
	// A nil Enqueuer or disabled cache skips this step cleanly — the
	// sync itself already succeeded so the run row is green.
	if j.Enqueuer != nil && j.Cache != nil && !j.Cache.Disabled() {
		if fanErr := j.fanOutImageJobs(ctx); fanErr != nil {
			j.Logger.Warn().Err(fanErr).Msg("refresh-encora: fan-out partial failure")
		}
	}
	return nil
}

// fanOutCounts is a tiny tally surfaced after fan-out completes.
type fanOutCounts struct {
	shows, recordings, actors int
}

// fanOutImageJobs walks the catalog and enqueues a refresh-* job for
// every entity missing its slot file. Per-entity scanning is done
// in-Go (cache.Has* is a cheap stat) rather than building a disjoint
// SQL set; the catalog ceiling is well within the budget for that.
func (j *RefreshEncoraJob) fanOutImageJobs(ctx context.Context) error {
	var counts fanOutCounts
	var firstErr error

	if err := j.fanOutShows(ctx, &counts, &firstErr); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := j.fanOutRecordings(ctx, &counts, &firstErr); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := j.fanOutActors(ctx, &counts, &firstErr); err != nil && firstErr == nil {
		firstErr = err
	}

	j.Logger.Info().
		Int("shows", counts.shows).
		Int("recordings", counts.recordings).
		Int("actors", counts.actors).
		Msg("refresh-encora: fan-out enqueued")

	return firstErr
}

// fanOutShows enqueues refresh-show-images for every show missing
// banner.jpg.
func (j *RefreshEncoraJob) fanOutShows(
	ctx context.Context, counts *fanOutCounts, firstErr *error,
) error {
	return iterateRows(ctx, j.DB,
		`SELECT show_id FROM shows WHERE show_id > 0`,
		func(showID int64) {
			if j.Cache.HasShowBanner(showID) {
				return
			}
			if _, e := j.Enqueuer.EnqueueFromJob(ctx, jobNameRefreshShowImages,
				jobs.JobArgs{"show_id": showID}); e != nil && *firstErr == nil {
				*firstErr = fmt.Errorf("enqueue refresh-show-images %d: %w", showID, e)
			}
			counts.shows++
		})
}

// fanOutRecordings enqueues refresh-recording-images for every
// recording missing fanart.jpg OR poster.jpg.
func (j *RefreshEncoraJob) fanOutRecordings(
	ctx context.Context, counts *fanOutCounts, firstErr *error,
) error {
	return iterateRows(ctx, j.DB,
		`SELECT recording_id FROM recordings WHERE recording_id > 0`,
		func(recID int64) {
			if j.Cache.HasRecordingFanart(recID) && j.Cache.HasRecordingPoster(recID) {
				return
			}
			if _, e := j.Enqueuer.EnqueueFromJob(ctx, jobNameRefreshRecordingImages,
				jobs.JobArgs{"recording_id": recID}); e != nil && *firstErr == nil {
				*firstErr = fmt.Errorf("enqueue refresh-recording-images %d: %w", recID, e)
			}
			counts.recordings++
		})
}

// fanOutActors enqueues refresh-actor-headshot for every credited
// performer missing a headshot.
func (j *RefreshEncoraJob) fanOutActors(
	ctx context.Context, counts *fanOutCounts, firstErr *error,
) error {
	return iterateRows(ctx, j.DB,
		`SELECT performer_id FROM performers WHERE performer_id > 0`,
		func(actorID int64) {
			if j.Cache.HasHeadshot(actorID) {
				return
			}
			if _, e := j.Enqueuer.EnqueueFromJob(ctx, jobNameRefreshActorHeadshot,
				jobs.JobArgs{"actor_id": actorID}); e != nil && *firstErr == nil {
				*firstErr = fmt.Errorf("enqueue refresh-actor-headshot %d: %w", actorID, e)
			}
			counts.actors++
		})
}

// iterateRows runs query (which must select a single int64 column)
// and invokes onID for each row. Errors propagate; an empty result
// set is fine.
//
// The IDs are drained into a slice and the cursor is closed BEFORE
// onID is called. The single-connection pool (storage.Open caps at 1)
// means a live cursor pins the only connection; if onID issues any DB
// write — like Enqueuer.EnqueueFromJob inserting a job_runs row — the
// write would block waiting for the connection forever, deadlocking
// the entire server. Draining first costs one slice allocation but
// keeps the connection free.
func iterateRows(ctx context.Context, db *sql.DB, query string, onID func(int64)) error {
	ids, err := scanIDs(ctx, db, query)
	if err != nil {
		return err
	}
	for _, id := range ids {
		onID(id)
	}
	return nil
}

// scanIDs runs query and returns every row's first column as int64.
func scanIDs(ctx context.Context, db *sql.DB, query string) ([]int64, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if scanErr := rows.Scan(&id); scanErr != nil {
			return nil, fmt.Errorf("scan: %w", scanErr)
		}
		ids = append(ids, id)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate: %w", rerr)
	}
	return ids, nil
}

// ScanIncomingJob wraps a single scanner.Engine.Scan pass. The
// scheduled cadence replaces what `library watch` used to do
// continuously — the scheduler runs Scan at WatchInterval and
// surfaces the run row in the UI.
type ScanIncomingJob struct {
	DB           *sql.DB
	IncomingDirs []string
	Logger       zerolog.Logger
}

// Name is the registry key for this job.
func (j *ScanIncomingJob) Name() string { return "scan-incoming" }

// Run executes one scanner pass. Per-file errors are tracked on the
// scanner Result but not surfaced as Run errors — the scheduled-jobs
// failure semantics are reserved for catastrophic scan failures
// (db unreachable, root unwalkable). Ignores args.
func (j *ScanIncomingJob) Run(ctx context.Context, _ jobs.JobArgs) error {
	if len(j.IncomingDirs) == 0 {
		return errors.New("scan-incoming: no incoming directories configured")
	}
	engine := &scanner.Engine{
		DB:        j.DB,
		WatchDirs: j.IncomingDirs,
		Logger:    j.Logger,
	}
	res, err := engine.Scan(ctx)
	if err != nil {
		return fmt.Errorf("scan-incoming: %w", err)
	}
	j.Logger.Info().
		Int("enqueued", res.Enqueued).
		Int("removed", res.Removed).
		Int("skipped", res.Skipped).
		Int("errors", len(res.Errors)).
		Msg("scan-incoming: pass complete")
	return nil
}
