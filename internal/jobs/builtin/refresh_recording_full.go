package builtin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/rs/zerolog"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/jobs"
	"github.com/nicolerenee/promptbook/internal/nforefresh"
	"github.com/nicolerenee/promptbook/internal/probe"
	"github.com/nicolerenee/promptbook/internal/storage"
	pbsync "github.com/nicolerenee/promptbook/internal/sync"
)

// EncoraRecordingClient is the slice of *encora.Client this job needs:
// a per-recording detail fetch against /api/recording/{id}. The real
// *encora.Client satisfies this via its Recording method. Defined
// locally so callers without an Encora API key can leave the field
// nil and have the job skip the upstream re-pull cleanly.
type EncoraRecordingClient interface {
	Recording(ctx context.Context, id int64) (encora.Recording, encora.RateLimitInfo, error)
}

// RefreshRecordingFullJob is the aggregate "refresh everything for one
// recording" job fired by the recording detail page's Refresh button.
// Where RefreshRecordingImagesJob only touches on-disk image slots,
// this one drives the full per-recording refresh pipeline:
//
//  1. Re-fetch /api/recording/{id} from Encora and upsert the DB row
//     via the same path the regular sync uses (pbsync.PersistRecording).
//  2. Re-run ffprobe on every recording_versions row and persist the
//     fresh MediaInfo blob via storage.UpsertVersion.
//  3. Rewrite movie.nfo via nforefresh.Service.RewriteForRecording so
//     any field changes (cast turnover, banner refresh, format string)
//     surface to the media server.
//  4. Optionally enqueue the per-recording image refresh job so a fresh
//     fanart/poster pair land alongside the metadata refresh.
//
// Each step's failures get logged with structured fields and the job
// continues — only an unparseable recording_id arg aborts. Encora-less
// setups (no API key) leave the Encora field nil and skip step 1
// without error so probe + nforefresh + image refresh still run.
type RefreshRecordingFullJob struct {
	DB *DBConn
	// SQLDB shares DB's connection pool. Plumbed alongside so the
	// post-fetch persist hook can write the recording's Encora id into
	// the external_ids table via the externalids package (no ent type
	// for that table — see internal/externalids for the rationale).
	// Optional: a nil SQLDB skips the external_ids upsert and the
	// recording still lands via the ent path.
	SQLDB      *sql.DB
	Encora     EncoraRecordingClient
	Prober     probe.Prober
	NFORefresh *nforefresh.Service
	// ImageRefresh, when non-nil, is invoked directly with the
	// recording_id arg after the metadata refresh settles. Mirrors the
	// way refresh-encora's fan-out drives the per-recording image job;
	// running it inline (rather than going through Enqueuer) keeps the
	// aggregate job's failure surface visible in one run row.
	ImageRefresh *RefreshRecordingImagesJob
	Logger       zerolog.Logger
}

// jobNameRefreshRecordingFull is the registry key. Stable string —
// surfaced in logs, the job_runs.job_name column, and the UI run-now
// route the SPA's Refresh button POSTs.
const jobNameRefreshRecordingFull = "refresh-recording-full"

// Name returns the registry key.
func (j *RefreshRecordingFullJob) Name() string { return jobNameRefreshRecordingFull }

// Run executes the four-step refresh pipeline. Args:
//
//	recording_id: int64 (required, must be > 0)
//
// Per-step failures log + continue; only the args parse can return an
// error. The run row therefore goes "succeeded" as long as the args
// were valid — partial-success diagnostics live in the structured
// log fields.
func (j *RefreshRecordingFullJob) Run(ctx context.Context, args jobs.JobArgs) error {
	recID := args.GetInt64("recording_id")
	if recID <= 0 {
		return errors.New("refresh-recording-full: missing or invalid recording_id arg")
	}

	j.refreshFromEncora(ctx, recID)
	j.reprobeVersions(ctx, recID)
	j.rewriteNFO(ctx, recID)
	j.runImageRefresh(ctx, recID)

	return nil
}

// refreshFromEncora pulls the recording detail from Encora and upserts
// it into the DB via the regular sync's persistence path. A nil
// Encora client is the intended self-hosted-without-API-key
// configuration: the step is skipped with a debug log so the
// remaining stages still run against whatever local state exists.
func (j *RefreshRecordingFullJob) refreshFromEncora(ctx context.Context, recID int64) {
	if j.Encora == nil {
		j.Logger.Debug().
			Int64("recording_id", recID).
			Msg("refresh-recording-full: encora client not configured; skipping upstream re-pull")
		return
	}
	rec, _, err := j.Encora.Recording(ctx, recID)
	if err != nil {
		j.Logger.Warn().
			Err(err).
			Int64("recording_id", recID).
			Msg("refresh-recording-full: encora detail fetch failed; continuing with local state")
		return
	}
	if perr := pbsync.PersistRecording(ctx, j.DB, j.SQLDB, rec, time.Now); perr != nil {
		j.Logger.Warn().
			Err(perr).
			Int64("recording_id", recID).
			Msg("refresh-recording-full: persist recording failed; continuing")
		return
	}
	j.Logger.Info().
		Int64("recording_id", recID).
		Msg("refresh-recording-full: encora detail re-pulled + persisted")
}

// reprobeVersions runs ffprobe against every recording_versions row's
// FilePath and persists the fresh MediaInfo JSON blob. Per-version
// failures (missing file, unreadable container) get a warn log and the
// next version is attempted; the row is left untouched so the prior
// MediaInfo remains visible in the UI rather than going blank.
func (j *RefreshRecordingFullJob) reprobeVersions(ctx context.Context, recID int64) {
	if j.Prober == nil {
		j.Logger.Debug().
			Int64("recording_id", recID).
			Msg("refresh-recording-full: prober not configured; skipping reprobe")
		return
	}
	versions, err := storage.ListVersions(ctx, j.DB, recID)
	if err != nil {
		j.Logger.Warn().
			Err(err).
			Int64("recording_id", recID).
			Msg("refresh-recording-full: list versions failed; skipping reprobe")
		return
	}
	for _, v := range versions {
		j.reprobeVersion(ctx, recID, v)
	}
}

// reprobeVersion runs one ffprobe pass for a single version row and
// persists the fresh MediaInfo. Pulled out of the loop so the funlen
// lint stays under cap and the per-version failure path is symmetric
// with the rest of the job.
func (j *RefreshRecordingFullJob) reprobeVersion(
	ctx context.Context, recID int64, v storage.RecordingVersion,
) {
	if v.FilePath == "" {
		return
	}
	info, perr := j.Prober.Probe(ctx, v.FilePath)
	if perr != nil {
		j.Logger.Warn().
			Err(perr).
			Int64("recording_id", recID).
			Int64("version_id", v.ID).
			Str("path", v.FilePath).
			Msg("refresh-recording-full: probe failed; leaving prior media_info_json in place")
		return
	}
	blob, merr := json.Marshal(info)
	if merr != nil {
		j.Logger.Warn().
			Err(merr).
			Int64("recording_id", recID).
			Int64("version_id", v.ID).
			Msg("refresh-recording-full: marshal media_info failed; skipping upsert")
		return
	}
	v.MediaInfoJSON = string(blob)
	if uerr := storage.UpsertVersion(ctx, j.DB, v); uerr != nil {
		j.Logger.Warn().
			Err(uerr).
			Int64("recording_id", recID).
			Int64("version_id", v.ID).
			Msg("refresh-recording-full: upsert version failed; continuing")
		return
	}
	j.Logger.Debug().
		Int64("recording_id", recID).
		Int64("version_id", v.ID).
		Str("path", v.FilePath).
		Msg("refresh-recording-full: version reprobed")
}

// rewriteNFO drives the NFO refresh. nforefresh.Service is itself a
// best-effort no-op when the recording hasn't been imported yet
// (no version row → no on-disk folder), so this is safe to call
// unconditionally for any recording id.
func (j *RefreshRecordingFullJob) rewriteNFO(ctx context.Context, recID int64) {
	if j.NFORefresh == nil {
		j.Logger.Debug().
			Int64("recording_id", recID).
			Msg("refresh-recording-full: nforefresh not configured; skipping nfo rewrite")
		return
	}
	if err := j.NFORefresh.RewriteForRecording(ctx, recID); err != nil {
		j.Logger.Warn().
			Err(err).
			Int64("recording_id", recID).
			Msg("refresh-recording-full: nfo rewrite failed; continuing")
		return
	}
	j.Logger.Debug().
		Int64("recording_id", recID).
		Msg("refresh-recording-full: nfo rewritten")
}

// runImageRefresh fires the per-recording image refresh inline. We
// invoke RefreshRecordingImagesJob.Run directly rather than enqueueing
// through the runner so the entire aggregate-job's outcome shows up
// on a single job_runs row — clicking Refresh once produces one log
// line in the UI's run history, not a parent + a child fan-out row.
func (j *RefreshRecordingFullJob) runImageRefresh(ctx context.Context, recID int64) {
	if j.ImageRefresh == nil {
		j.Logger.Debug().
			Int64("recording_id", recID).
			Msg("refresh-recording-full: image refresh job not wired; skipping")
		return
	}
	args := jobs.JobArgs{"recording_id": recID}
	if err := j.ImageRefresh.Run(ctx, args); err != nil {
		j.Logger.Warn().
			Err(err).
			Int64("recording_id", recID).
			Msg("refresh-recording-full: image refresh failed; continuing")
		return
	}
	j.Logger.Debug().
		Int64("recording_id", recID).
		Msg("refresh-recording-full: image refresh completed")
}

// Compile-time guard: *encora.Client must satisfy
// EncoraRecordingClient. Catches drift in the client's Recording
// signature at build time rather than at first run.
var _ EncoraRecordingClient = (*encora.Client)(nil)
