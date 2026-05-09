package server

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/storage"
)

// imageChoiceResponse is the JSON envelope every picker endpoint
// returns. ok=true on success; error carries the human-readable
// failure reason otherwise. Mirrors encoraWriteResponse so the SPA can
// share an error-extraction helper across the two surfaces.
type imageChoiceResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// posterChoiceRequest is the JSON body for POST /recordings/:id/poster.
// Index is 0-based and must be in [0, CountPosters).
type posterChoiceRequest struct {
	Index int `json:"index"`
}

// backdropChoiceRequest is the JSON body for POST /recordings/:id/backdrop.
// Index is 0-based and must be in [0, CountBackdrops).
type backdropChoiceRequest struct {
	Index int `json:"index"`
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
// selected backdrop verbatim to rendered.jpg. Disabled=false (the
// default) restores the normal composite path on the next render.
type overlayDisabledRequest struct {
	Disabled bool `json:"disabled"`
}

// requireImageCache returns a 503 echo error when the image cache
// wasn't configured (or is in disabled mode). The picker endpoints
// can't bounds-check or trigger a re-render without it, so the call
// is rejected rather than partially mutating image_choices.
func (s *Server) requireImageCache() error {
	cache := s.ImageCache()
	if cache == nil || cache.Disabled() {
		return echo.NewHTTPError(
			http.StatusServiceUnavailable, "image cache not configured")
	}
	return nil
}

// recordingExists reports whether a recording row is present in the
// local cache. The picker endpoints reject unknown ids with 404 so the
// UI surfaces "no such recording" instead of silently writing an
// orphan image_choices row keyed on a non-existent recording.
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

// handleSetPoster handles POST /api/v1/recordings/:id/poster. Persists
// the user's poster pick after validating it's in [0, CountPosters)
// for the recording's show. Does NOT trigger a re-render — posters
// don't get burned-in text, so no rendered.jpg lifecycle is involved.
func (s *Server) handleSetPoster(c echo.Context) error {
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

	var req posterChoiceRequest
	if bindErr := c.Bind(&req); bindErr != nil {
		return echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("decode body: %s", bindErr.Error()))
	}

	// CountPosters keys on show_id (one poster set per show), not the
	// per-recording id. The recording's show_id is on the loaded
	// payload — fetch once for the bounds-check.
	loaded, loadErr := storage.LoadRecording(c.Request().Context(), s.db, id)
	if loadErr != nil {
		return loadErr
	}
	count := s.ImageCache().CountPosters(loaded.Recording.Metadata.ShowID)
	if req.Index < 0 || req.Index >= count {
		return c.JSON(http.StatusBadRequest, imageChoiceResponse{
			Error: fmt.Sprintf("poster index %d out of range [0, %d)", req.Index, count),
		})
	}

	if setErr := storage.SetPosterIndex(c.Request().Context(), s.db, id, req.Index); setErr != nil {
		return fmt.Errorf("set poster index: %w", setErr)
	}
	return c.JSON(http.StatusOK, imageChoiceResponse{OK: true})
}

// handleSetBackdrop handles POST /api/v1/recordings/:id/backdrop.
// Persists the user's backdrop pick after bounds-checking against
// CountBackdrops, then asks the renderer to refresh rendered.jpg
// against the new selection. The renderer is a stub today — the call
// stays so the wiring is in place when the real implementation lands.
func (s *Server) handleSetBackdrop(c echo.Context) error {
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

	var req backdropChoiceRequest
	if bindErr := c.Bind(&req); bindErr != nil {
		return echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("decode body: %s", bindErr.Error()))
	}

	count := s.ImageCache().CountBackdrops(id)
	if req.Index < 0 || req.Index >= count {
		return c.JSON(http.StatusBadRequest, imageChoiceResponse{
			Error: fmt.Sprintf("backdrop index %d out of range [0, %d)", req.Index, count),
		})
	}

	if setErr := storage.SetBackdropIndex(c.Request().Context(), s.db, id, req.Index); setErr != nil {
		return fmt.Errorf("set backdrop index: %w", setErr)
	}
	if r := s.ImageRenderer(); r != nil {
		if rerr := r.Regenerate(c.Request().Context(), id); rerr != nil {
			s.logger.Warn().
				Err(rerr).
				Int64("recording_id", id).
				Msg("backdrop regenerate failed; choice was persisted")
		}
	}
	return c.JSON(http.StatusOK, imageChoiceResponse{OK: true})
}

// handleSetOverlay handles POST /api/v1/recordings/:id/overlay.
// Either persists Text as the override (Clear=false) or nulls the
// column so the auto-derived label takes over (Clear=true). Empty
// Text with Clear=false is intentionally allowed — it pins "no label"
// against the renderer's auto-derived fallback. After the write the
// renderer is asked to refresh rendered.jpg so the visible burn
// matches the saved override.
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
// flag and asks the renderer to refresh rendered.jpg so the on-disk
// file flips between burned-in composite and raw copy immediately on
// toggle. Standard 503/400/404 patterns mirror the sibling picker
// endpoints.
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
