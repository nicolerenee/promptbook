package server

import (
	"time"

	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog"
)

// zerologMiddleware logs each request at info level. Bodies are not
// logged — the API serves recording payloads which are large and not
// privacy-sensitive enough to warrant the noise.
func zerologMiddleware(logger zerolog.Logger) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			start := time.Now()
			err := next(c)
			latency := time.Since(start)

			req := c.Request()
			res := c.Response()
			evt := logger.Info().
				Str("method", req.Method).
				Str("uri", req.RequestURI).
				Int("status", res.Status).
				Int64("size", res.Size).
				Dur("latency", latency)
			if err != nil {
				evt = evt.AnErr("err", err)
			}
			evt.Msg("http")
			return err
		}
	}
}
