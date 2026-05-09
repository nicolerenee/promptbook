package server

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/storage"
)

// handleRegeneratePoster forces a re-render of poster.jpg for the
// given recording. Useful as a manual "Re-render" affordance and as a
// cheap smoke test from curl. The overlay/upload paths also call
// imagerender.Regenerate directly when a choice changes, so this
// endpoint is purely additive — it doesn't alter any DB state.
//
// 503 when the renderer wasn't wired (no image cache configured), 404
// when the recording isn't in the local catalog, 200 with a tiny JSON
// body on success.
func (s *Server) handleRegeneratePoster(c echo.Context) error {
	if s.imageRenderer == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "image renderer not configured")
	}
	id, err := storage.ParseRecordingID(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	ctx := c.Request().Context()
	if _, loadErr := storage.LoadRecording(ctx, s.db, id); loadErr != nil {
		if errors.Is(loadErr, storage.ErrRecordingNotFound) {
			return echo.NewHTTPError(http.StatusNotFound, loadErr.Error())
		}
		return loadErr
	}
	if regenErr := s.imageRenderer.Regenerate(ctx, id); regenErr != nil {
		s.logger.Warn().
			Err(regenErr).
			Int64("recording_id", id).
			Msg("imagerender: regenerate failed")
		return echo.NewHTTPError(http.StatusInternalServerError, regenErr.Error())
	}
	return c.JSON(http.StatusOK, map[string]any{"ok": true, "recording_id": id})
}
