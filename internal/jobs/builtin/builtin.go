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
	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/jobs"
	"github.com/nicolerenee/promptbook/internal/nforefresh"
	"github.com/nicolerenee/promptbook/internal/scanner"
	"github.com/nicolerenee/promptbook/internal/storage"
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
	DB *ent.Client
	// SQLDB shares DB's connection pool. Plumbed onto pbsync.Options
	// so the sync's per-page commit also stamps Encora ids into the
	// external_ids table (keeps the table uniform across legacy
	// back-fill + new sync inserts). Optional: nil is tolerated and
	// the sync's external_ids upsert is skipped.
	SQLDB        *sql.DB
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
		SQLDB:        j.SQLDB,
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
	ids, err := j.DB.Show.Query().IDs(ctx)
	if err != nil {
		return fmt.Errorf("query show ids: %w", err)
	}
	for _, showID := range ids {
		if showID <= 0 || j.Cache.HasShowBanner(showID) {
			continue
		}
		if _, e := j.Enqueuer.EnqueueFromJob(ctx, jobNameRefreshShowImages,
			jobs.JobArgs{"show_id": showID}); e != nil && *firstErr == nil {
			*firstErr = fmt.Errorf("enqueue refresh-show-images %d: %w", showID, e)
		}
		counts.shows++
	}
	return nil
}

// fanOutRecordings enqueues refresh-recording-images for every
// recording missing fanart.jpg OR poster.jpg.
func (j *RefreshEncoraJob) fanOutRecordings(
	ctx context.Context, counts *fanOutCounts, firstErr *error,
) error {
	ids, err := j.DB.Recording.Query().IDs(ctx)
	if err != nil {
		return fmt.Errorf("query recording ids: %w", err)
	}
	for _, recID := range ids {
		if recID <= 0 || (j.Cache.HasRecordingFanart(recID) && j.Cache.HasRecordingPoster(recID)) {
			continue
		}
		if _, e := j.Enqueuer.EnqueueFromJob(ctx, jobNameRefreshRecordingImages,
			jobs.JobArgs{argKeyRecordingID: recID}); e != nil && *firstErr == nil {
			*firstErr = fmt.Errorf("enqueue refresh-recording-images %d: %w", recID, e)
		}
		counts.recordings++
	}
	return nil
}

// fanOutActors enqueues refresh-actor-headshot for every credited
// performer missing a headshot.
func (j *RefreshEncoraJob) fanOutActors(
	ctx context.Context, counts *fanOutCounts, firstErr *error,
) error {
	ids, err := j.DB.Performer.Query().IDs(ctx)
	if err != nil {
		return fmt.Errorf("query performer ids: %w", err)
	}
	for _, actorID := range ids {
		if actorID <= 0 || j.Cache.HasHeadshot(actorID) {
			continue
		}
		if _, e := j.Enqueuer.EnqueueFromJob(ctx, jobNameRefreshActorHeadshot,
			jobs.JobArgs{"actor_id": actorID}); e != nil && *firstErr == nil {
			*firstErr = fmt.Errorf("enqueue refresh-actor-headshot %d: %w", actorID, e)
		}
		counts.actors++
	}
	return nil
}

// ScanIncomingJob wraps a single scanner.Engine.Scan pass. The
// scheduled cadence replaces what `library watch` used to do
// continuously — the scheduler runs Scan at WatchInterval and
// surfaces the run row in the UI.
type ScanIncomingJob struct {
	DB           *ent.Client
	SQLDB        *sql.DB
	Encora       scanner.EncoraRecordingClient
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
		SQLDB:     j.SQLDB,
		Encora:    j.Encora,
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

// ScanLibraryRootJob walks library.root once and queues anything that
// doesn't already have a recording_versions row. Use case: backfill
// an existing Jellyfin tree that pre-dates promptbook into the queue
// + import flow so each recording gets renamed under the new default
// templates and gets a tracking row written.
//
// Manual-only by design. The library-root tree is large and stable;
// re-walking it on a schedule would be a lot of stat traffic for
// almost no win, and the user only needs to trigger it occasionally
// (after a fresh import drop, or once during initial migration). The
// runner registration leaves Interval as the zero value so the
// ticker never auto-fires it.
type ScanLibraryRootJob struct {
	DB     *ent.Client
	SQLDB  *sql.DB
	Encora scanner.EncoraRecordingClient
	Root   string
	Logger zerolog.Logger
}

// Name is the registry key for this job.
func (j *ScanLibraryRootJob) Name() string { return "scan-library-root" }

// Run executes one scanner pass over Root. Files already represented
// in recording_versions are silently skipped via the IsTracked
// callback; everything else lands on the manual_import_queue with the
// matcher's best-confidence suggestion (most library-root folders
// carry an `[encora-N]` token so the matcher resolves them at high
// confidence). Ignores args — the job is unparameterized.
func (j *ScanLibraryRootJob) Run(ctx context.Context, _ jobs.JobArgs) error {
	if j.Root == "" {
		return errors.New("scan-library-root: library.root is not configured")
	}
	engine := &scanner.Engine{
		DB:        j.DB,
		SQLDB:     j.SQLDB,
		Encora:    j.Encora,
		WatchDirs: []string{j.Root},
		Logger:    j.Logger,
		IsTracked: func(path string) bool {
			exists, err := storage.VersionExistsByPath(ctx, j.DB, path)
			if err != nil {
				// On lookup error, treat as untracked so the file
				// still surfaces in the queue rather than being
				// silently dropped. The scanner's per-file error
				// channel will record the underlying issue.
				j.Logger.Warn().Err(err).Str("path", path).
					Msg("scan-library-root: tracked-lookup failed; treating as untracked")
				return false
			}
			return exists
		},
	}
	res, err := engine.Scan(ctx)
	if err != nil {
		return fmt.Errorf("scan-library-root: %w", err)
	}
	j.Logger.Info().
		Int("enqueued", res.Enqueued).
		Int("removed", res.Removed).
		Int("skipped", res.Skipped).
		Int("errors", len(res.Errors)).
		Msg("scan-library-root: pass complete")
	return nil
}

// RefreshAllRecordingsJob walks every recording with at least one
// recording_versions row and runs the refresh-recording-full pipeline
// for each. Use case: a nightly auto-refresh that picks up external-
// tool changes (Radarr replacing a file in place + renaming it) so
// the user wakes up to a catalog that mirrors disk reality without
// having to click Refresh on every recording.
//
// The aggregate-per-recording pipeline (encora re-pull, reconcile
// file_path, re-stat + re-probe, NFO rewrite, image refresh) is
// shared with the detail page's manual Refresh button — this job is
// just the fan-out driver. Per-recording failures get a warn log and
// the next id is attempted; only a list-recordings failure aborts.
//
// ctx cancellation is checked between recordings so a long pass can
// be stopped cleanly via the runner's cancel handle (or shutdown).
type RefreshAllRecordingsJob struct {
	DB        *DBConn
	PerRecord *RefreshRecordingFullJob
	Logger    zerolog.Logger
}

// jobNameRefreshAllRecordings is the registry key. Stable string —
// surfaced in logs, the job_runs.job_name column, and the UI run-now
// route.
const jobNameRefreshAllRecordings = "refresh-all-recordings"

// argKeyRecordingID is the JobArgs key carrying the int64 recording
// id every per-recording job + fan-out consumes. Pulled out as a
// named constant so the goconst lint stays satisfied across the
// half-dozen call sites.
const argKeyRecordingID = "recording_id"

// Name returns the registry key.
func (j *RefreshAllRecordingsJob) Name() string { return jobNameRefreshAllRecordings }

// Run executes one fan-out pass. Ignores args — the job is
// unparameterized.
func (j *RefreshAllRecordingsJob) Run(ctx context.Context, _ jobs.JobArgs) error {
	if j.DB == nil {
		return errors.New("refresh-all-recordings: db not configured")
	}
	if j.PerRecord == nil {
		return errors.New("refresh-all-recordings: per-recording job not configured")
	}
	ids, err := storage.ListRecordingIDsWithVersions(ctx, j.DB)
	if err != nil {
		return fmt.Errorf("refresh-all-recordings: list recordings: %w", err)
	}
	j.Logger.Info().
		Int("recordings", len(ids)).
		Msg("refresh-all-recordings: pass starting")
	var (
		refreshed int
		failed    int
	)
	for _, id := range ids {
		if ctxErr := ctx.Err(); ctxErr != nil {
			j.Logger.Warn().Err(ctxErr).
				Int("refreshed", refreshed).
				Int("failed", failed).
				Msg("refresh-all-recordings: cancelled mid-pass")
			return fmt.Errorf("refresh-all-recordings: cancelled: %w", ctxErr)
		}
		args := jobs.JobArgs{argKeyRecordingID: id}
		if runErr := j.PerRecord.Run(ctx, args); runErr != nil {
			failed++
			j.Logger.Warn().Err(runErr).
				Int64("recording_id", id).
				Msg("refresh-all-recordings: per-recording refresh failed; continuing")
			continue
		}
		refreshed++
	}
	j.Logger.Info().
		Int("refreshed", refreshed).
		Int("failed", failed).
		Msg("refresh-all-recordings: pass complete")
	return nil
}

// RegenerateAllNFOJob walks every recording with at least one
// recording_versions row and rewrites its movie.nfo using
// nforefresh.Service. Use cases:
//
//   - The user changed `server.publicURL` and old NFOs reference the
//     old base URL. Re-running this rewrites every NFO with the new
//     URL so media servers re-fetch images on next scan.
//   - A schema change adds new fields to the NFO writer (e.g.
//     <uniqueid> per external provider) and existing on-disk NFOs
//     need to gain those fields without re-importing.
//   - An image cache rebuild bumped every poster/fanart mtime; the
//     `?v={mtime}` cache-buster URLs in old NFOs are stale.
//
// Manual-only by design. Rewriting hundreds of NFOs is cheap but
// not so cheap that you want it on a recurring schedule. The runner
// registration leaves Interval as the zero value so the ticker
// never auto-fires it.
//
// Per-recording failures are logged but don't abort the batch — one
// missing folder shouldn't block the rest of the catalog. The summary
// log line records totals so the user can audit.
type RegenerateAllNFOJob struct {
	DB      *ent.Client
	Service *nforefresh.Service
	Logger  zerolog.Logger
}

// Name is the registry key for this job.
func (j *RegenerateAllNFOJob) Name() string { return "regenerate-all-nfo" }

// Run executes one pass over every recording with versions. Ignores
// args — the job is unparameterized.
func (j *RegenerateAllNFOJob) Run(ctx context.Context, _ jobs.JobArgs) error {
	if j.DB == nil {
		return errors.New("regenerate-all-nfo: db not configured")
	}
	if j.Service == nil {
		return errors.New("regenerate-all-nfo: nforefresh service not configured")
	}
	ids, err := storage.ListRecordingIDsWithVersions(ctx, j.DB)
	if err != nil {
		return fmt.Errorf("regenerate-all-nfo: list recordings: %w", err)
	}
	j.Logger.Info().
		Int("recordings", len(ids)).
		Msg("regenerate-all-nfo: pass starting")
	var (
		rewritten int
		failed    int
	)
	for _, id := range ids {
		// Honor cancellation between recordings so the user can
		// abort a long pass cleanly via the runner.
		if ctxErr := ctx.Err(); ctxErr != nil {
			j.Logger.Warn().Err(ctxErr).
				Int("rewritten", rewritten).
				Int("failed", failed).
				Msg("regenerate-all-nfo: cancelled mid-pass")
			return fmt.Errorf("regenerate-all-nfo: cancelled: %w", ctxErr)
		}
		if rewriteErr := j.Service.RewriteForRecording(ctx, id); rewriteErr != nil {
			failed++
			j.Logger.Warn().Err(rewriteErr).
				Int64("recording_id", id).
				Msg("regenerate-all-nfo: per-recording rewrite failed; continuing")
			continue
		}
		rewritten++
	}
	j.Logger.Info().
		Int("rewritten", rewritten).
		Int("failed", failed).
		Msg("regenerate-all-nfo: pass complete")
	return nil
}
