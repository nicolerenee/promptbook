package cmd

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/imagerender"
	"github.com/nicolerenee/promptbook/internal/ingest"
	"github.com/nicolerenee/promptbook/internal/jobs"
	"github.com/nicolerenee/promptbook/internal/jobs/builtin"
	"github.com/nicolerenee/promptbook/internal/nforefresh"
	"github.com/nicolerenee/promptbook/internal/probe"
	"github.com/nicolerenee/promptbook/internal/server"
	"github.com/nicolerenee/promptbook/internal/stagemedia"
	"github.com/nicolerenee/promptbook/internal/storage"
	"github.com/nicolerenee/promptbook/internal/sync"
)

// imageFetchHTTPTimeout caps any single image download from the local
// cache's perspective. Generous — large posters on a slow link should
// still land — but tight enough that one stuck CDN can't wedge a
// long-running serve process.
const imageFetchHTTPTimeout = 60 * time.Second

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

	sqlDB, db, err := storage.OpenEnt(ctx, appConfig.Storage.DatabasePath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()

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
	// handlers' nil-checks fire correctly. All three fields point at
	// the same concrete client when configured — the surface split is
	// purely a compile-time guard against the apply pipeline calling
	// into the remove/add-wants methods or the picker accidentally
	// reaching a write surface.
	var (
		encOpt           server.EncoraWriteClient
		encDestrucOpt    server.EncoraDestructiveClient
		encScreenshotOpt server.EncoraScreenshotClient
	)
	if encClient != nil {
		encOpt = encClient
		encDestrucOpt = encClient
		encScreenshotOpt = encClient
	}
	// server.Options.Stagemedia is also an interface; same nil idiom
	// applies so handlers' nil-check sees a true-nil interface and
	// gracefully degrades when no API key is configured.
	var smOpt server.StagemediaImageClient
	if smClient != nil {
		smOpt = smClient
	}

	// Image cache is nil when library.imageRoot is empty so the
	// /images/* route stays unregistered and detail-page handlers
	// fall back to the upstream URL. The single shared *http.Client
	// with imageFetchHTTPTimeout is reused for every cache fetch.
	var imgCache *imagecache.Cache
	if appConfig.Library.ImageRoot != "" {
		imgCache = imagecache.New(
			appConfig.Library.ImageRoot,
			&http.Client{Timeout: imageFetchHTTPTimeout},
			log.Logger,
		)
	} else {
		log.Info().Msg("image cache disabled (library.imageRoot not configured)")
	}

	ingestOpt := buildIngestEngine(db, encClient, imgCache)

	// Renderer is nil when image caching is off so the picker
	// handlers' nil-check 503s rather than half-mutating state. When
	// caching is on, the same DB + cache instance the rest of the
	// server uses powers Regenerate.
	imgRenderer := imagerender.New(db, imgCache, log.Logger)

	// NFO-refresh service is shared with the per-entity image-refresh
	// jobs so the cascade fires both off the upload-handler triggers
	// (Server constructs its own copy) and off background job writes.
	nfoRefresh := buildNFORefresh(db, imgCache)

	runner := buildJobRunner(ctx, db, encClient, smClient, imgCache, imgRenderer, nfoRefresh)

	srv, err := server.New(server.Options{
		DB:                db,
		Logger:            log.Logger,
		Stagemedia:        smOpt,
		Encora:            encOpt,
		EncoraDestructive: encDestrucOpt,
		EncoraScreenshots: encScreenshotOpt,
		IngestEngine:      ingestOpt,
		ImageCache:        imgCache,
		ImageRenderer:     imgRenderer,
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

// buildNFORefresh returns the NFO-refresh service, or nil when image
// caching is off. Pulled out of runServe so the function stays under
// the funlen ceiling without disabling the lint outright.
func buildNFORefresh(db *ent.Client, imgCache *imagecache.Cache) *nforefresh.Service {
	if imgCache == nil || imgCache.Disabled() {
		return nil
	}
	return nforefresh.New(db, imgCache, appConfig.Server.PublicURL, log.Logger)
}

// buildIngestEngine returns the queue-import ingest.Engine — wired
// through to server.Options.IngestEngine — only when both an encora
// client and a library root are configured. Returns nil otherwise so
// the queue-import handler 503s instead of failing requests at run
// time.
func buildIngestEngine(
	db *ent.Client, encClient *encora.Client, imgCache *imagecache.Cache,
) server.IngestRunner {
	if encClient == nil || appConfig.Library.Root == "" {
		log.Info().Msg("queue import disabled (encora api key or library.root missing)")
		return nil
	}
	return &ingest.Engine{
		DB:             db,
		Client:         encClient,
		LibraryRoot:    appConfig.Library.Root,
		FolderTemplate: appConfig.Library.FolderTemplate,
		FileTemplate:   appConfig.Library.FileTemplate,
		SubtitleFetcher: &ingest.HTTPSubtitleFetcher{
			HTTP: &http.Client{Timeout: serveSubtitleHTTPTimeout},
		},
		Logger:     log.Logger,
		ImageCache: imgCache,
		Prober:     probe.FFProbe{Path: appConfig.Library.FFProbePath},
		PublicURL:  appConfig.Server.PublicURL,
	}
}

// buildJobRunner constructs the scheduled-jobs runner with whatever
// jobs the current configuration supports. Returns nil when no job has
// the prerequisites configured — the server's nil-check then 503s on
// /api/v1/jobs/* and the rest of the API stays usable.
//
// Image-refresh jobs (refresh-show-images, refresh-recording-images,
// refresh-actor-headshot) are registered as manual-only (Interval==0)
// so they appear in the Scheduled view but only fire when
// refresh-encora's fan-out enqueues them or a user clicks Run.
func buildJobRunner(
	_ context.Context,
	db *ent.Client,
	encClient *encora.Client,
	smClient *stagemedia.Client,
	imgCache *imagecache.Cache,
	imgRenderer *imagerender.Renderer,
	nfoRefresh *nforefresh.Service,
) *jobs.Runner {
	runner := jobs.New(jobs.Options{
		DB:      db,
		Logger:  log.Logger,
		Workers: jobRunnerWorkers,
	})

	registered := 0
	registered += registerRefreshEncora(runner, db, encClient, imgCache)
	registered += registerImageRefreshJobs(
		runner, db, encClient, smClient, imgCache, imgRenderer, nfoRefresh,
	)
	registered += registerScanIncoming(runner, db)
	registered += registerScanLibraryRoot(runner, db)

	if registered == 0 {
		log.Info().Msg("jobs runner has no registered jobs (encora + incomingDirs both unconfigured)")
		return nil
	}
	return runner
}

// registerRefreshEncora wires the refresh-encora job and its
// post-registration Enqueuer back-reference. Returns 1 on success, 0
// otherwise.
func registerRefreshEncora(
	runner *jobs.Runner, db *ent.Client, encClient *encora.Client, imgCache *imagecache.Cache,
) int {
	if encClient == nil {
		return 0
	}
	// Construct with a back-reference to the runner via the Enqueuer
	// interface. The runner is already fully initialized at this
	// point, so the assignment is safe. Solves the apparent circular
	// dependency (jobs depend on Runner, Runner registers jobs) by
	// treating the runner as a plain value injected into the job.
	refreshJob := &builtin.RefreshEncoraJob{
		DB:           db,
		Client:       encClient,
		Logger:       log.Logger,
		BurstReserve: appConfig.Encora.RateLimit.BurstReserve,
		Cache:        imgCache,
		Enqueuer:     runner,
	}
	if err := runner.Register(jobs.JobDef{
		Job:      refreshJob,
		Interval: refreshEncoraInterval,
	}); err != nil {
		log.Error().Err(err).Msg("register refresh-encora job")
		return 0
	}
	return 1
}

// registerImageRefreshJobs wires the three per-entity image refresh
// jobs as manual-only. Returns the number successfully registered.
func registerImageRefreshJobs(
	runner *jobs.Runner,
	db *ent.Client,
	encClient *encora.Client,
	smClient *stagemedia.Client,
	imgCache *imagecache.Cache,
	imgRenderer *imagerender.Renderer,
	nfoRefresh *nforefresh.Service,
) int {
	if smClient == nil || imgCache == nil || imgCache.Disabled() {
		return 0
	}
	var smSync sync.StagemediaImageClient = smClient
	var encScreenshots sync.EncoraScreenshotClient
	if encClient != nil {
		encScreenshots = encClient
	}

	count := 0
	jobsToRegister := []jobs.JobDef{
		{Job: &builtin.RefreshShowImagesJob{
			DB: db, Cache: imgCache, SM: smSync, Logger: log.Logger,
			NFORefresh: nfoRefresh,
		}},
		{Job: &builtin.RefreshRecordingImagesJob{
			DB: db, Cache: imgCache, Encora: encScreenshots, SM: smSync,
			Renderer: imgRenderer, Logger: log.Logger,
			NFORefresh: nfoRefresh,
		}},
		{Job: &builtin.RefreshActorHeadshotJob{
			DB: db, Cache: imgCache, SM: smSync, Logger: log.Logger,
			NFORefresh: nfoRefresh,
		}},
	}
	for _, def := range jobsToRegister {
		if err := runner.Register(def); err != nil {
			log.Error().Err(err).
				Str("job_name", def.Job.Name()).
				Msg("register image refresh job")
			continue
		}
		count++
	}
	return count
}

// registerScanIncoming wires the scan-incoming job when at least one
// incoming directory is configured. Returns 1 on success, 0 otherwise.
func registerScanIncoming(runner *jobs.Runner, db *ent.Client) int {
	if len(appConfig.Library.IncomingDirs) == 0 {
		return 0
	}
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
		return 0
	}
	return 1
}

// registerScanLibraryRoot wires the scan-library-root job when
// library.root is configured. Manual-only: Interval is left zero so
// the scheduler ticker never auto-fires it. The user invokes it via
// the queue page's "Scan library" button (POST
// /api/v1/jobs/scheduled/scan-library-root/run) when they want to
// backfill orphan recordings into the queue.
func registerScanLibraryRoot(runner *jobs.Runner, db *ent.Client) int {
	if appConfig.Library.Root == "" {
		return 0
	}
	err := runner.Register(jobs.JobDef{
		Job: &builtin.ScanLibraryRootJob{
			DB:     db,
			Root:   appConfig.Library.Root,
			Logger: log.Logger,
		},
	})
	if err != nil {
		log.Error().Err(err).Msg("register scan-library-root job")
		return 0
	}
	return 1
}
