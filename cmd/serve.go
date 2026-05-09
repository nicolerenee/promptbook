package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/ingest"
	"github.com/nicolerenee/promptbook/internal/jobs"
	"github.com/nicolerenee/promptbook/internal/jobs/builtin"
	"github.com/nicolerenee/promptbook/internal/server"
	"github.com/nicolerenee/promptbook/internal/stagemedia"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// serveSubtitleHTTPTimeout caps subtitle downloads from the queue-import
// endpoint so a stuck CDN can't wedge the HTTP server. Mirrors the value
// `library ingest` uses on the CLI side.
const serveSubtitleHTTPTimeout = 60 * time.Second

// refreshEncoraInterval is the cadence the scheduled jobs framework
// uses to mirror the user's Encora collection. 15 minutes plays nicely
// with Encora's 30 req/min ceiling — even a large collection finishes
// well inside the window with room left over for ad-hoc CLI calls.
const refreshEncoraInterval = 15 * time.Minute

// jobRunnerWorkers is the worker-pool size the scheduled-jobs runner
// uses. Two matches the Radarr default and is enough to overlap the
// long Encora sync (15 min interval) with the short scan-incoming
// pass (1 min interval) without contending for the single sqlite
// writer.
const jobRunnerWorkers = 2

//nolint:gochecknoglobals // cobra requires package-level command variable
var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the promptbook HTTP server",
	Long: `Starts the echo HTTP server that serves the catalog pages and the
/api/v1/* JSON endpoints from the local SQLite cache. JWT/OIDC auth is
deferred to a later phase. The default listen address is [::]:8080
(all interfaces); set server.listen or PROMPTBOOK_SERVER_LISTEN to
127.0.0.1:8080 to restrict to loopback.`,
	RunE: runServe,
}

//nolint:gochecknoinits // cobra requires init for command registration
func init() {
	rootCmd.AddCommand(serveCmd)
}

func runServe(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()

	db, err := storage.Open(ctx, appConfig.Storage.DatabasePath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	var smClient *stagemedia.Client
	if appConfig.Stagemedia.APIKey != "" {
		smClient, err = stagemedia.New(stagemedia.Options{
			BaseURL:   appConfig.Stagemedia.BaseURL,
			APIKey:    appConfig.Stagemedia.APIKey,
			UserAgent: appConfig.Stagemedia.UserAgent,
			Logger:    log.Logger,
		})
		if err != nil {
			return fmt.Errorf("build stagemedia client: %w", err)
		}
	} else {
		log.Info().Msg("stagemedia disabled (no api key configured)")
	}

	var encClient *encora.Client
	if appConfig.Encora.APIKey != "" {
		encClient, err = encora.New(encora.Options{
			BaseURL:   appConfig.Encora.BaseURL,
			APIKey:    appConfig.Encora.APIKey,
			UserAgent: appConfig.Encora.UserAgent,
			Logger:    log.Logger,
		})
		if err != nil {
			return fmt.Errorf("build encora client: %w", err)
		}
	} else {
		log.Info().Msg("encora disabled (no api key configured)")
	}

	// server.Options.Encora and EncoraDestructive are interfaces; a nil
	// *encora.Client must arrive as a true nil interface so the
	// handlers' nil-checks fire correctly. Both fields point at the
	// same concrete client when configured — the surface split is
	// purely a compile-time guard against the apply pipeline calling
	// into the remove/add-wants methods.
	var (
		encOpt        server.EncoraWriteClient
		encDestrucOpt server.EncoraDestructiveClient
	)
	if encClient != nil {
		encOpt = encClient
		encDestrucOpt = encClient
	}
	// server.Options.Stagemedia is also an interface; same nil idiom
	// applies so handlers' nil-check sees a true-nil interface and
	// gracefully degrades when no API key is configured.
	var smOpt server.StagemediaImageClient
	if smClient != nil {
		smOpt = smClient
	}

	// Build the queue-import ingest engine only when both the encora
	// client and a library root are configured — otherwise the engine
	// has no useful work it can do, and the queue-import handler 503s
	// rather than failing requests at run time.
	var ingestOpt server.IngestRunner
	if encClient != nil && appConfig.Library.Root != "" {
		ingestOpt = &ingest.Engine{
			DB:             db,
			Client:         encClient,
			LibraryRoot:    appConfig.Library.Root,
			FolderTemplate: appConfig.Library.FolderTemplate,
			FileTemplate:   appConfig.Library.FileTemplate,
			SubtitleFetcher: &ingest.HTTPSubtitleFetcher{
				HTTP: &http.Client{Timeout: serveSubtitleHTTPTimeout},
			},
			Logger: log.Logger,
		}
	} else {
		log.Info().Msg("queue import disabled (encora api key or library.root missing)")
	}

	runner := buildJobRunner(ctx, db, encClient)

	srv, err := server.New(server.Options{
		DB:                db,
		Logger:            log.Logger,
		Stagemedia:        smOpt,
		Encora:            encOpt,
		EncoraDestructive: encDestrucOpt,
		IngestEngine:      ingestOpt,
		JobRunner:         runner,
		Version:           Version,
		Config:            appConfig,
		// ConfigSource intentionally left empty — viper.ConfigFileUsed
		// would require plumbing the *viper.Viper out of config.Load to
		// retrieve. The settings page falls back to "<not exposed>"
		// rather than block on the detail. Plumb through internal/config
		// when the source path becomes worth surfacing.
		ConfigSource: "",
	})
	if err != nil {
		return fmt.Errorf("build server: %w", err)
	}
	addr := appConfig.Server.Listen
	if addr == "" {
		addr = "[::]:8080"
	}

	// Launch the runner alongside the HTTP server. Both share ctx so a
	// single cancellation unwinds them together. Start blocks; we
	// fire-and-forget on a goroutine and let srv.Start drive shutdown.
	if runner != nil {
		go func() { _ = runner.Start(ctx) }()
	}

	return srv.Start(ctx, addr)
}

// buildJobRunner constructs the scheduled-jobs runner with whatever
// jobs the current configuration supports. Returns nil when neither
// job has the prerequisites configured — the server's nil-check then
// 503s on /api/v1/jobs/* and the rest of the API stays usable.
func buildJobRunner(_ context.Context, db *sql.DB, encClient *encora.Client) *jobs.Runner {
	runner := jobs.New(jobs.Options{
		DB:      db,
		Logger:  log.Logger,
		Workers: jobRunnerWorkers,
	})

	registered := 0

	if encClient != nil {
		err := runner.Register(jobs.JobDef{
			Job: &builtin.RefreshEncoraJob{
				DB:           db,
				Client:       encClient,
				Logger:       log.Logger,
				BurstReserve: appConfig.Encora.RateLimit.BurstReserve,
			},
			Interval:  refreshEncoraInterval,
			OnStartup: true,
		})
		if err != nil {
			log.Error().Err(err).Msg("register refresh-encora job")
		} else {
			registered++
		}
	}

	if len(appConfig.Library.IncomingDirs) > 0 {
		err := runner.Register(jobs.JobDef{
			Job: &builtin.ScanIncomingJob{
				DB:           db,
				IncomingDirs: appConfig.Library.IncomingDirs,
				Logger:       log.Logger,
			},
			Interval: appConfig.Library.WatchInterval,
		})
		if err != nil {
			log.Error().Err(err).Msg("register scan-incoming job")
		} else {
			registered++
		}
	}

	if registered == 0 {
		log.Info().Msg("jobs runner has no registered jobs (encora + incomingDirs both unconfigured)")
		return nil
	}
	return runner
}
