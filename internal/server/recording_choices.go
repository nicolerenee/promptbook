package server

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/storage"
)

// imageChoiceResponse is the JSON envelope every overlay endpoint
// returns. ok=true on success; error carries the human-readable
// failure reason otherwise. Mirrors encoraWriteResponse so the SPA can
// share an error-extraction helper across the two surfaces.
type imageChoiceResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// overlayChoiceRequest is the JSON body for POST /recordings/:id/overlay.
// Clear=true wipes the override (the auto-derived label takes over);
// otherwise Text is persisted verbatim — including the empty string,
// which means "render no label". The two paths are explicit so a UI
// can't accidentally clear by submitting an empty form.
type overlayChoiceRequest struct {
	Text  string `json:"text"`
	Clear bool   `json:"clear"`
}

// overlayDisabledRequest is the JSON body for
// POST /recordings/:id/overlay-disabled. Disabled=true asks the
// renderer to skip the playbill-style band and instead copy the raw
// poster source verbatim to poster.jpg.
type overlayDisabledRequest struct {
	Disabled bool `json:"disabled"`
}

// requireImageCache returns a 503 echo error when the image cache
// wasn't configured (or is in disabled mode). The endpoints can't
// trigger a re-render without it, so the call is rejected rather than
// partially mutating image_choices.
func (s *Server) requireImageCache() error {
	cache := s.ImageCache()
	if cache == nil || cache.Disabled() {
		return echo.NewHTTPError(
			http.StatusServiceUnavailable, "image cache not configured")
	}
	return nil
}

// recordingExists reports whether a recording row is present in the
// local cache. The endpoints reject unknown ids with 404 so the UI
// surfaces "no such recording" instead of silently writing an orphan
// image_choices row keyed on a non-existent recording.
func (s *Server) recordingExists(c echo.Context, id int64) error {
	_, err := storage.LoadRecording(c.Request().Context(), s.db, id)
	if errors.Is(err, storage.ErrRecordingNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}
	if err != nil {
		return err
	}
	return nil
}

// handleSetOverlay handles POST /api/v1/recordings/:id/overlay.
// Either persists Text as the override (Clear=false) or nulls the
// column so the auto-derived label takes over (Clear=true). Empty
// Text with Clear=false is intentionally allowed — it pins "no label"
// against the renderer's auto-derived fallback. After the write the
// renderer is asked to refresh poster.jpg so the visible burn matches
// the saved override.
func (s *Server) handleSetOverlay(c echo.Context) error {
	id, err := parseRecordingIDParam(c)
	if err != nil {
		return err
	}
	if cacheErr := s.requireImageCache(); cacheErr != nil {
		return cacheErr
	}
	if existsErr := s.recordingExists(c, id); existsErr != nil {
		return existsErr
	}

	var req overlayChoiceRequest
	if bindErr := c.Bind(&req); bindErr != nil {
		return echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("decode body: %s", bindErr.Error()))
	}

	if req.Clear {
		if clearErr := storage.ClearOverlayTextOverride(c.Request().Context(), s.db, id); clearErr != nil {
			return fmt.Errorf("clear overlay override: %w", clearErr)
		}
	} else {
		if setErr := storage.SetOverlayTextOverride(c.Request().Context(), s.db, id, req.Text); setErr != nil {
			return fmt.Errorf("set overlay override: %w", setErr)
		}
	}
	if r := s.ImageRenderer(); r != nil {
		if rerr := r.Regenerate(c.Request().Context(), id); rerr != nil {
			s.logger.Warn().
				Err(rerr).
				Int64("recording_id", id).
				Msg("overlay regenerate failed; choice was persisted")
		}
	}
	return c.JSON(http.StatusOK, imageChoiceResponse{OK: true})
}

// handleSetOverlayDisabled handles POST
// /api/v1/recordings/:id/overlay-disabled. Persists the burn-in opt-out
// flag and asks the renderer to refresh poster.jpg so the on-disk file
// flips between burned-in composite and raw copy immediately on toggle.
func (s *Server) handleSetOverlayDisabled(c echo.Context) error {
	id, err := parseRecordingIDParam(c)
	if err != nil {
		return err
	}
	if cacheErr := s.requireImageCache(); cacheErr != nil {
		return cacheErr
	}
	if existsErr := s.recordingExists(c, id); existsErr != nil {
		return existsErr
	}

	var req overlayDisabledRequest
	if bindErr := c.Bind(&req); bindErr != nil {
		return echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("decode body: %s", bindErr.Error()))
	}

	if setErr := storage.SetOverlayDisabled(c.Request().Context(), s.db, id, req.Disabled); setErr != nil {
		return fmt.Errorf("set overlay disabled: %w", setErr)
	}
	if r := s.ImageRenderer(); r != nil {
		if rerr := r.Regenerate(c.Request().Context(), id); rerr != nil {
			s.logger.Warn().
				Err(rerr).
				Int64("recording_id", id).
				Msg("overlay-disabled regenerate failed; choice was persisted")
		}
	}
	return c.JSON(http.StatusOK, imageChoiceResponse{OK: true})
}
