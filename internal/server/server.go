// Package server runs promptbook's HTTP UI and JSON API.
//
// Phase 5 ships without auth — the listen default is 127.0.0.1:8080 so
// the surface is localhost-only by design. JWT/OIDC enforcement on
// /api/v1/* arrives in a later phase.
package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/rs/zerolog"

	"github.com/nicolerenee/promptbook/internal/config"
	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/imagerender"
	"github.com/nicolerenee/promptbook/internal/ingest"
	"github.com/nicolerenee/promptbook/internal/jobs"
	"github.com/nicolerenee/promptbook/internal/nforefresh"
	"github.com/nicolerenee/promptbook/internal/probe"
	"github.com/nicolerenee/promptbook/internal/server/graph"
	"github.com/nicolerenee/promptbook/internal/stagemedia"
	"github.com/nicolerenee/promptbook/internal/tmdb"
	"github.com/nicolerenee/promptbook/internal/web"
)

// IngestRunner is the slice of *ingest.Engine the server needs to run a
// single-file ingest from the manual import queue (driven through the
// importQueueEntry GraphQL mutation). Aliased to graph.IngestRunner so
// the schema resolver can hold the same interface without importing
// the parent server package — and the existing `server.IngestRunner`
// public name keeps production wiring (cmd/serve.go) untouched. The
// real *ingest.Engine satisfies the underlying interface.
type IngestRunner = graph.IngestRunner

// Compile-time guard that *ingest.Engine satisfies IngestRunner so
// production wiring (cmd/serve.go) can pass the real engine on
// Options.IngestEngine without an adapter.
var _ IngestRunner = (*ingest.Engine)(nil)

// HTTP server timing. ReadHeaderTimeout defends against slowloris-style
// stalls without disrupting normal browser usage. shutdownTimeout caps
// how long graceful shutdown waits for in-flight requests.
const (
	readHeaderTimeout = 10 * time.Second
	shutdownTimeout   = 10 * time.Second
)

// StagemediaImageClient is the read-only surface of stagemedia.Client
// the server depends on. The real *stagemedia.Client satisfies it via
// its Images method; tests can substitute a fake without spinning up an
// httptest server. Defined here (not in stagemedia) so the test seam
// lives next to its consumer and the stagemedia package stays a leaf.
type StagemediaImageClient interface {
	Images(ctx context.Context, showID int64, performerIDs []int64) (stagemedia.Images, error)
	Posters(ctx context.Context, showID int64) ([]string, error)
}

// EncoraScreenshotClient is the read-only Encora surface the picker
// uses to surface fanart options live. The real *encora.Client
// satisfies this via its Screenshots method; tests can substitute a
// fake without spinning up an httptest server. Kept distinct from
// EncoraWriteClient + EncoraDestructiveClient so the apply pipeline
// can't reach a read-only endpoint and the picker can't reach the
// write surfaces.
type EncoraScreenshotClient interface {
	Screenshots(ctx context.Context, id int64) ([]string, encora.RateLimitInfo, error)
}

// FrameExtractor is the ffmpeg shell-out surface the picker's fanart
// fallback consumes when Encora has no curated screenshots for a
// recording. The real probe.FrameExtractor satisfies this; tests can
// substitute a fake that lays down touch-files in outDir so the
// fallback path stays exercisable without ffmpeg on PATH.
//
// The interface keeps the rng argument concrete (probe.FrameRandSource
// is itself an interface) so handlers and tests share a single typed
// seam — no duck-typed any.
type FrameExtractor interface {
	Extract(
		ctx context.Context,
		videoPath, outDir string,
		durationSeconds float64,
		count int,
		rng probe.FrameRandSource,
	) ([]string, error)
}

// TMDBClient is the slice of *tmdb.Client the picker consumes for
// poster + fanart suggestions on recordings with a TMDB / IMDB
// external id. Two endpoints today: Images for the curated poster +
// backdrop arrays, FindByIMDBID for the IMDB → TMDB resolution path
// when only an IMDB id is present. Defined as an interface so tests
// can substitute a fake without spinning up an httptest server.
type TMDBClient interface {
	Images(ctx context.Context, tmdbID int64) (tmdb.Images, error)
	FindByIMDBID(ctx context.Context, imdbID string) (int64, bool, error)
}

// Server is the HTTP entry point.
type Server struct {
	echo *echo.Echo
	db   *ent.Client
	// sqlDB shares db's connection pool. Plumbed through so the
	// graphql resolver can read from the external_ids table (no ent
	// type for it — see internal/externalids). Optional; the
	// Recording.externalIDs resolver returns [] when sqlDB is nil.
	sqlDB             *sql.DB
	logger            zerolog.Logger
	stagemedia        StagemediaImageClient
	encora            EncoraWriteClient
	encoraDestructive EncoraDestructiveClient
	// encoraScreenshots is the read-only screenshots surface the picker
	// hits when the user opens the fanart tab. nil when no Encora API
	// key was configured; the picker-options handler 503s in that case
	// so other endpoints stay alive. Production wiring passes the same
	// *encora.Client the write/destructive surfaces use.
	encoraScreenshots EncoraScreenshotClient
	// tmdb is the TMDB poster + fanart picker source. nil when no
	// TMDB API key was configured — the picker silently skips the
	// TMDB group in that mode. Production wiring passes *tmdb.Client.
	tmdb TMDBClient
	// imageCache is the on-disk poster/headshot cache. nil when no
	// library.imageRoot was configured. Handlers nil-check before
	// calling into it; the cache itself also has a Disabled() guard
	// so a non-nil-but-empty cache is safe.
	imageCache *imagecache.Cache
	// imageRenderer composites the burned-in backdrop (rendered.jpg)
	// over the raw cached backdrop after a backdrop or overlay-text
	// change. nil when image caching is disabled (imagerender.New
	// returns nil in that mode); the picker + regenerate handlers
	// nil-check and 503 in that case.
	imageRenderer *imagerender.Renderer
	// ingestEngine drives `POST /api/v1/queue/{id}/import`. nil when the
	// server was constructed without one (tests or no-encora-key wiring);
	// the queue-import handler 503s in that case so other endpoints stay
	// usable.
	ingestEngine IngestRunner
	// jobRunner powers /api/v1/jobs/*. nil when the server was
	// constructed without one (tests or jobs disabled); each handler
	// returns 503 in that case so the rest of the API stays alive.
	jobRunner *jobs.Runner
	// nfoRefresh rewrites movie.nfo files on disk after an image
	// changes (upload, set-from-URL, refresh job) so the writer's
	// `?v={mtime}` cache-buster reaches the file media servers scan.
	// nil when image caching is disabled — there's no NFO to refresh
	// in that mode either way.
	nfoRefresh *nforefresh.Service
	// prober overrides the rename-Plan path's media probe. nil falls
	// back to probe.FFProbe at libraryPlan() time, which is the
	// production wiring; tests inject a stub.
	prober probe.Prober
	// frameExtractor is the ffmpeg shell-out the picker's fanart
	// fallback uses when Encora has no curated screenshots for a
	// recording. Defined as an interface so tests can substitute a
	// fake that drops touch-files into outDir without invoking real
	// ffmpeg. nil falls back to probe.FrameExtractor at handler time.
	frameExtractor FrameExtractor
	// sleeper is the function the apply batch driver uses to honor a
	// 429's Retry-After before issuing the next request. Defaults to
	// time.Sleep; tests inject a recorder to assert the call without
	// blocking real wall-clock time.
	sleeper func(time.Duration)
	// version is the build-time version string the sidebar footer
	// renders. "dev" when not configured. Plumbed via Options so the
	// server package doesn't need to import cmd (which would cycle).
	version string
	// config is a buttonshot of the loaded config.Config the read-only
	// /settings page surfaces via /api/v1/settings. Stored by value so
	// later mutation in the caller can't bleed into the JSON response.
	// Direct import of internal/config is safe — config has no inbound
	// internal/* imports, so no cycle.
	config config.Config
	// configSource is the file path viper used (or a sentinel like
	// "defaults + env") for display on the settings page.
	configSource string
}

// Options configures a new server.
type Options struct {
	DB *ent.Client
	// SQLDB shares DB's connection pool. Used by the graphql resolver
	// for external_ids reads. Optional in tests; production wiring
	// (cmd/serve.go) supplies the same *sql.DB returned by
	// storage.OpenEnt.
	SQLDB  *sql.DB
	Logger zerolog.Logger
	// Stagemedia is optional. When nil, poster + headshot fetching is
	// disabled; handlers that depend on it must nil-check. Typed as the
	// StagemediaImageClient interface so tests can stub the headshot path
	// without a real *stagemedia.Client.
	Stagemedia StagemediaImageClient
	// Encora is optional. When nil, the apply endpoints respond 503 so
	// read-only views still work without an API key configured. Tests
	// pass a stub satisfying EncoraWriteClient; production wiring passes
	// a real *encora.Client.
	Encora EncoraWriteClient
	// EncoraDestructive is optional. When nil, the destructive
	// /api/v1/encora/* endpoints respond 503. Production wiring passes
	// the same *encora.Client instance as Encora; the surface stays
	// split so the apply pipeline can never reach the remove/add-wants
	// methods by accident.
	EncoraDestructive EncoraDestructiveClient
	// EncoraScreenshots is optional. When nil, the picker's fanart-
	// options endpoint skips the upstream call and falls straight
	// through to the local frame-extract fallback. Production wiring
	// passes the same *encora.Client instance the apply pipeline
	// uses; the surface is split so picker reads can't accidentally
	// reach a write method.
	EncoraScreenshots EncoraScreenshotClient
	// TMDB is optional. When nil, the picker's poster + fanart
	// endpoints skip the TMDB source group cleanly. Production
	// wiring passes a real *tmdb.Client built from tmdb.apiKey;
	// tests substitute a fake satisfying TMDBClient.
	TMDB TMDBClient
	// IngestEngine is optional. When nil, POST /api/v1/queue/{id}/import
	// responds 503 so read-only queue views still work without ingest
	// wiring (e.g. when no library.root is configured). Tests pass a stub
	// satisfying IngestRunner; production wiring passes a real
	// *ingest.Engine.
	IngestEngine IngestRunner
	// JobRunner is optional. When nil, /api/v1/jobs/* responds 503.
	// Production wiring passes a *jobs.Runner the cmd layer started
	// alongside the HTTP server.
	JobRunner *jobs.Runner
	// Sleeper is optional. When nil, time.Sleep is used. Tests inject a
	// recorder that captures the requested duration without sleeping
	// for real, so the Retry-After honor logic stays exercisable under
	// `go test -race` without a wall-clock pause.
	Sleeper func(time.Duration)
	// Version is the build-time version string surfaced in the sidebar
	// footer. Empty falls back to "dev".
	Version string
	// Config is a buttonshot of the loaded application configuration the
	// read-only /settings page surfaces. Secrets (api keys) are redacted
	// at JSON-render time; the raw struct is held here so server-side
	// callers don't accidentally see the redacted shape.
	Config config.Config
	// ConfigSource describes where the config was loaded from (a file
	// path from viper.ConfigFileUsed, or a sentinel string like
	// "defaults + env"). Empty falls back to "<not exposed>".
	ConfigSource string
	// ImageCache is optional. When non-nil and not Disabled(), the
	// server registers a `/images/*` static handler that serves the
	// cached posters/backdrops/headshots. Disabled / nil leaves the
	// route unregistered, so requests to /images/... fall through to
	// the SPA shell (which renders 404s client-side).
	ImageCache *imagecache.Cache
	// ImageRenderer is optional. When non-nil, the recording-detail
	// picker handlers + the regenerate-backdrop endpoint call
	// Regenerate so rendered.jpg stays in sync with the user's
	// selection. nil when image caching is disabled — handlers 503
	// rather than partially mutate state. Production wiring
	// constructs it from the same imagecache.Cache; tests pass nil.
	ImageRenderer *imagerender.Renderer
	// Prober is an optional override for the rename Plan path's media
	// probe. nil falls back to probe.FFProbe with the configured
	// FFProbePath, which is the production wiring. Tests inject a
	// stub satisfying probe.Prober so the per-recording rename
	// preview / apply paths run without an ffprobe binary on PATH.
	Prober probe.Prober
	// FrameExtractor is an optional override for the picker's fanart-
	// fallback frame extractor. nil falls back to probe.FrameExtractor
	// with the configured FFmpegPath. Tests inject a fake so the
	// fallback path runs without a real ffmpeg binary.
	FrameExtractor FrameExtractor
	// NFORefresh is an optional override for the nfo refresh service
	// the regenerateRecordingNFO mutation + the apply-rename's
	// post-move rewrite drive. nil falls back to constructing one
	// from ImageCache + Config.Server.PublicURL when ImageCache is
	// configured; tests pass a hand-built service so the cascade can
	// be exercised without a full image cache.
	NFORefresh *nforefresh.Service
}

// New constructs a server with all routes registered and templates
// parsed. It does not bind a listener — call Start for that.
func New(opts Options) (*Server, error) {
	if opts.DB == nil {
		return nil, errors.New("server: db is required")
	}

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	pages, err := web.ParsePages()
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	e.Renderer = &templateRenderer{pages: pages}

	e.Use(middleware.Recover())
	e.Use(zerologMiddleware(opts.Logger))

	sleeper := opts.Sleeper
	if sleeper == nil {
		sleeper = time.Sleep
	}
	version := opts.Version
	if version == "" {
		version = "dev"
	}
	configSource := opts.ConfigSource
	if configSource == "" {
		configSource = "<not exposed>"
	}
	srv := &Server{
		echo:              e,
		db:                opts.DB,
		sqlDB:             opts.SQLDB,
		logger:            opts.Logger,
		stagemedia:        opts.Stagemedia,
		encora:            opts.Encora,
		encoraDestructive: opts.EncoraDestructive,
		encoraScreenshots: opts.EncoraScreenshots,
		tmdb:              opts.TMDB,
		ingestEngine:      opts.IngestEngine,
		imageCache:        opts.ImageCache,
		imageRenderer:     opts.ImageRenderer,
		jobRunner:         opts.JobRunner,
		prober:            opts.Prober,
		frameExtractor:    opts.FrameExtractor,
		sleeper:           sleeper,
		version:           version,
		config:            opts.Config,
		configSource:      configSource,
	}
	// Construct the NFO-refresh service when an image cache is
	// configured. Without a cache there are no image-change events to
	// react to, so the service stays nil and every fan-out trigger is
	// a no-op via its own nil-check. An explicit Options.NFORefresh
	// override (used by tests + future custom wiring) wins over the
	// auto-construction path.
	switch {
	case opts.NFORefresh != nil:
		srv.nfoRefresh = opts.NFORefresh
	case opts.ImageCache != nil && !opts.ImageCache.Disabled():
		srv.nfoRefresh = nforefresh.New(
			opts.DB, opts.SQLDB, opts.ImageCache, opts.Config.Server.PublicURL, opts.Logger,
		)
	}
	srv.routes()
	srv.echo.GET("/static/*", echo.WrapHandler(http.StripPrefix("/static/", web.StaticHandler())))
	// Serve cached images straight off disk when caching is on. The
	// /images/* route is registered alongside /static/* so the SPA
	// catch-all (registered in routes()) can't swallow it. On a cache
	// miss imagesHandler renders an SVG placeholder rather than 404 —
	// keeps the UI from showing broken-image icons when a refresh job
	// hasn't fired yet (or never will, for entities without an
	// upstream image).
	if opts.ImageCache != nil && !opts.ImageCache.Disabled() {
		srv.echo.GET("/images/*", srv.imagesHandler)
	}
	return srv, nil
}

// Handler exposes the underlying http.Handler so tests can drive the
// server without binding a real socket.
func (s *Server) Handler() http.Handler { return s.echo }

// Stagemedia returns the configured StageMedia image client, or nil
// when stagemedia is disabled. Handlers must nil-check before use.
func (s *Server) Stagemedia() StagemediaImageClient { return s.stagemedia }

// SQLDB exposes the underlying *sql.DB so tests can drive the
// external_ids surface (no ent type for it; the package-level
// helpers in internal/externalids work against the raw *sql.DB).
// Returns nil when no handle was wired.
func (s *Server) SQLDB() *sql.DB { return s.sqlDB }

// Encora returns the configured Encora write client, or nil when no
// API key was supplied. Apply handlers nil-check this and return 503
// rather than crashing the server.
func (s *Server) Encora() EncoraWriteClient { return s.encora }

// ImageCache returns the configured local image cache, or nil when no
// library.imageRoot was configured. Handlers nil-check before calling
// into it; the cache's own Disabled() guard handles a non-nil-but-empty
// cache safely.
func (s *Server) ImageCache() *imagecache.Cache { return s.imageCache }

// ImageRenderer returns the configured burned-in-backdrop renderer, or
// nil when image caching is disabled. Picker + regenerate handlers
// nil-check this and 503 rather than mutating image_choices without a
// renderer to honor the change.
func (s *Server) ImageRenderer() *imagerender.Renderer { return s.imageRenderer }

// Version returns the build-time version string the sidebar footer
// renders. Defaults to "dev" when no Options.Version was configured.
func (s *Server) Version() string { return s.version }

// proberOrFallback returns the configured prober override, or a fresh
// probe.FFProbe with the configured ffprobePath when no override is
// supplied. Pulled out so libraryPlan() stays a one-liner construction
// and tests can inject a stub via Options.Prober.
func (s *Server) proberOrFallback() probe.Prober {
	if s.prober != nil {
		return s.prober
	}
	return probe.FFProbe{Path: s.config.Library.FFProbePath}
}

// frameExtractorOrFallback returns the configured FrameExtractor
// override, or a fresh probe.FrameExtractor with the configured
// ffmpegPath when none was supplied. Picker handlers consume this so
// production wiring picks up the configured ffmpeg path automatically
// while tests inject a fake via Options.FrameExtractor.
func (s *Server) frameExtractorOrFallback() FrameExtractor {
	if s.frameExtractor != nil {
		return s.frameExtractor
	}
	return probe.FrameExtractor{Path: s.config.Library.FFmpegPath}
}

// libraryPlan projects the loaded library config into the
// graph.LibraryPlan shape the previewQueueImport resolver needs. The
// fallthrough zero value (root="" or empty templates) marks the plan
// as unconfigured; the resolver returns a typed error in that mode.
func (s *Server) libraryPlan() graph.LibraryPlan {
	if s.config.Library.Root == "" {
		return graph.LibraryPlan{}
	}
	return graph.LibraryPlan{
		Root:           s.config.Library.Root,
		FolderTemplate: s.config.Library.FolderTemplate,
		FileTemplate:   s.config.Library.FileTemplate,
		Prober:         s.proberOrFallback(),
	}
}

// Compile-time guard: the real *encora.Client must satisfy
// EncoraWriteClient so production wiring can pass it on Options.Encora
// without a wrapper. This isn't a runtime use; the underscore drops the
// reference once the compiler is happy.
var _ EncoraWriteClient = (*encora.Client)(nil)

// Compile-time guard: the real *encora.Client must satisfy
// EncoraScreenshotClient so production wiring can pass the same client
// on Options.EncoraScreenshots.
var _ EncoraScreenshotClient = (*encora.Client)(nil)

// Compile-time guard: probe.FrameExtractor must satisfy FrameExtractor
// so production wiring can pass it on Options.FrameExtractor without a
// wrapper.
var _ FrameExtractor = probe.FrameExtractor{}

// Compile-time guard: the real *stagemedia.Client must satisfy
// StagemediaImageClient so production wiring can pass it on
// Options.Stagemedia without a wrapper.
var _ StagemediaImageClient = (*stagemedia.Client)(nil)

// Start binds to addr and serves until the context is cancelled. Returns
// nil on graceful shutdown.
func (s *Server) Start(ctx context.Context, addr string) error {
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           s.echo,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		s.logger.Info().Str("addr", addr).Msg("server listening")
		if listenErr := httpSrv.ListenAndServe(); listenErr != nil &&
			!errors.Is(listenErr, http.ErrServerClosed) {
			errCh <- listenErr
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

// templateRenderer adapts html/template to echo's Renderer interface.
// Post-SPA migration the registry only carries `index.html`; the
// legacy `_layout.html`-composed pages no longer exist as live
// templates.
type templateRenderer struct {
	pages web.PageSet
}

// Render implements echo.Renderer. The named template is executed
// directly (no shared layout wrapper), since the SPA shell is a single
// self-contained file and the legacy per-page bodies have been retired.
func (r *templateRenderer) Render(w io.Writer, name string, data any, _ echo.Context) error {
	t, ok := r.pages[name]
	if !ok {
		return fmt.Errorf("server: no template registered for %q", name)
	}
	return t.Execute(w, data)
}
