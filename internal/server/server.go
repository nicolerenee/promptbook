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

	"github.com/nicolerenee/promptbook/internal/stagemedia"
	"github.com/nicolerenee/promptbook/internal/web"
)

// HTTP server timing. ReadHeaderTimeout defends against slowloris-style
// stalls without disrupting normal browser usage. shutdownTimeout caps
// how long graceful shutdown waits for in-flight requests.
const (
	readHeaderTimeout = 10 * time.Second
	shutdownTimeout   = 10 * time.Second
)

// Server is the HTTP entry point.
type Server struct {
	echo       *echo.Echo
	db         *sql.DB
	logger     zerolog.Logger
	stagemedia *stagemedia.Client
}

// Options configures a new server.
type Options struct {
	DB     *sql.DB
	Logger zerolog.Logger
	// Stagemedia is optional. When nil, poster + headshot fetching is
	// disabled; handlers that depend on it must nil-check.
	Stagemedia *stagemedia.Client
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

	srv := &Server{
		echo:       e,
		db:         opts.DB,
		logger:     opts.Logger,
		stagemedia: opts.Stagemedia,
	}
	srv.routes()
	srv.echo.GET("/static/*", echo.WrapHandler(http.StripPrefix("/static/", web.StaticHandler())))
	return srv, nil
}

// Handler exposes the underlying http.Handler so tests can drive the
// server without binding a real socket.
func (s *Server) Handler() http.Handler { return s.echo }

// Stagemedia returns the configured StageMedia client, or nil when
// stagemedia is disabled. Handlers must nil-check before use.
func (s *Server) Stagemedia() *stagemedia.Client { return s.stagemedia }

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
// Each page name maps to its own parsed tree so per-page body blocks
// don't collide.
type templateRenderer struct {
	pages web.PageSet
}

// Render implements echo.Renderer.
func (r *templateRenderer) Render(w io.Writer, name string, data any, _ echo.Context) error {
	t, ok := r.pages[name]
	if !ok {
		return fmt.Errorf("server: no template registered for %q", name)
	}
	return t.ExecuteTemplate(w, "_layout.html", data)
}
