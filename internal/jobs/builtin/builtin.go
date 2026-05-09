// Package builtin holds the concrete Job implementations the server
// registers at boot. Each adapter wraps an existing engine (sync,
// scanner) so the scheduler doesn't need to know about Encora rate
// limits or NFS mounts — it just calls Run(ctx) and records the
// outcome.
package builtin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/rs/zerolog"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/scanner"
	pbsync "github.com/nicolerenee/promptbook/internal/sync"
)

// RefreshEncoraJob is the scheduled wrapper around sync.Sync. The
// burst-reserve floor and pause-between-pages are read from the same
// config the CLI uses, so the rate-limit policy is identical between
// `promptbook collection sync` and the in-process scheduler.
type RefreshEncoraJob struct {
	DB           *sql.DB
	Client       *encora.Client
	Logger       zerolog.Logger
	BurstReserve int
}

// Name is the registry key for this job. Stable string — surfaced in
// logs, the job_runs.job_name column, and the UI route.
func (j *RefreshEncoraJob) Name() string { return "refresh-encora" }

// Run invokes pbsync.Sync. Returns the wrapped error so the scheduler
// records it on the job_runs row.
func (j *RefreshEncoraJob) Run(ctx context.Context) error {
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
	return nil
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
// (db unreachable, root unwalkable). A noisy NFS mount with
// permission errors should not paint the row red.
func (j *ScanIncomingJob) Run(ctx context.Context) error {
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
