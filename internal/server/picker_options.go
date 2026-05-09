package server

// picker_options.go — live "what's available upstream?" endpoints
// the image-picker modal hits when the user opens it.
//
// These are the only API surfaces that talk to StageMedia / Encora at
// request time. Everything else under /api/v1/* serves the local DB +
// cached images on disk; the picker is allowed to incur upstream
// latency because the user is actively waiting on the modal to fill.
//
// Each handler:
//   1. Validates + loads the entity from the DB (404 on miss).
//   2. Calls upstream live.
//   3. Maps the response into []pickerOption{URL, Source}.
//   4. 200 with {options: [...]}, possibly empty.
//
// 503 is reserved for the case where the upstream client is nil
// (StageMedia / Encora key not configured). Empty options + 200 is
// the answer to "upstream had nothing for this entity".

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/storage"
)

// pickerUpstreamTimeout caps how long any one /poster-options /
// /fanart-options / /headshot-options call waits on upstream before
// the handler bails. Five seconds is enough for a normal StageMedia /
// Encora round-trip but tight enough that a hung backend doesn't pin
// the picker modal indefinitely — the user can hit "Re-fetch" once
// upstream is healthy again.
const pickerUpstreamTimeout = 5 * time.Second

// pickerOption is one upstream URL the picker UI can render as a
// thumbnail. Source is "stagemedia" or "encora" so the SPA can group
// or label the strip if it ever cares to (today the modal renders all
// options in one row regardless of source).
type pickerOption struct {
	URL    string `json:"url"`
	Source string `json:"source"`
}

// pickerOptionsResponse is the JSON envelope for every options
// endpoint. options is always a non-nil slice so the SPA can iterate
// without a presence check.
type pickerOptionsResponse struct {
	Options []pickerOption `json:"options"`
}

// emptyOptionsResponse returns a 200 with an empty options array so
// the SPA renders "No upstream options found" rather than a 503 or a
// silent empty render.
func emptyOptionsResponse() pickerOptionsResponse {
	return pickerOptionsResponse{Options: []pickerOption{}}
}

// requireStagemedia returns a 503 echo error when no stagemedia client
// is configured. Callers nil-check via the returned error.
func (s *Server) requireStagemedia() error {
	if s.Stagemedia() == nil {
		return echo.NewHTTPError(
			http.StatusServiceUnavailable, "stagemedia client not configured")
	}
	return nil
}

// requireEncoraScreenshots returns a 503 echo error when no Encora
// screenshots client is configured. The picker uses this read-only
// surface; if it's nil the fanart picker can't surface options.
func (s *Server) requireEncoraScreenshots() error {
	if s.encoraScreenshots == nil {
		return echo.NewHTTPError(
			http.StatusServiceUnavailable, "encora client not configured")
	}
	return nil
}

// fetchShowPosterOptions calls stagemedia /api/images for a given
// show_id and returns its Posters array mapped to pickerOptions. The
// picker UI only cares about posters here; headshots come from a
// separate endpoint.
//
// StageMedia rejects /api/images calls without at least one
// actor_ids — its handler returns 400 "at least one performer id is
// required" on an empty list. Pass [1] as a sentinel (matching what
// the sync image fetcher does) so the call still returns the show's
// posters; the headshot half of the response is ignored here.
func (s *Server) fetchShowPosterOptions(
	ctx context.Context, showID int64,
) ([]pickerOption, error) {
	if showID <= 0 {
		return []pickerOption{}, nil
	}
	upstreamCtx, cancel := context.WithTimeout(ctx, pickerUpstreamTimeout)
	defer cancel()
	imgs, err := s.Stagemedia().Images(upstreamCtx, showID, []int64{1})
	if err != nil {
		return nil, err
	}
	out := make([]pickerOption, 0, len(imgs.Posters))
	for _, u := range imgs.Posters {
		if u == "" {
			continue
		}
		out = append(out, pickerOption{URL: u, Source: "stagemedia"})
	}
	return out, nil
}

// handleListShowPosterOptions handles
// GET /api/v1/shows/:id/poster-options. Returns the StageMedia poster
// URLs for the show.
func (s *Server) handleListShowPosterOptions(c echo.Context) error {
	id, err := parseShowIDParam(c)
	if err != nil {
		return err
	}
	if smErr := s.requireStagemedia(); smErr != nil {
		return smErr
	}
	if existsErr := s.showExists(c.Request().Context(), id); existsErr != nil {
		return existsErr
	}

	options, fetchErr := s.fetchShowPosterOptions(c.Request().Context(), id)
	if fetchErr != nil {
		s.logger.Warn().
			Err(fetchErr).
			Int64("show_id", id).
			Msg("picker: show poster options fetch failed")
		return echo.NewHTTPError(http.StatusBadGateway,
			"upstream poster fetch failed: "+fetchErr.Error())
	}
	return c.JSON(http.StatusOK, pickerOptionsResponse{Options: options})
}

// handleListRecordingPosterOptions handles
// GET /api/v1/recordings/:id/poster-options. Posters are keyed on the
// recording's show, not the recording itself — recordings inherit
// their poster from the show under the v2 layout.
func (s *Server) handleListRecordingPosterOptions(c echo.Context) error {
	id, err := parseRecordingIDParam(c)
	if err != nil {
		return err
	}
	if smErr := s.requireStagemedia(); smErr != nil {
		return smErr
	}
	loaded, loadErr := storage.LoadRecording(c.Request().Context(), s.db, id)
	if errors.Is(loadErr, storage.ErrRecordingNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, loadErr.Error())
	}
	if loadErr != nil {
		return loadErr
	}
	showID := loaded.Recording.Metadata.ShowID
	options, fetchErr := s.fetchShowPosterOptions(c.Request().Context(), showID)
	if fetchErr != nil {
		s.logger.Warn().
			Err(fetchErr).
			Int64("recording_id", id).
			Int64("show_id", showID).
			Msg("picker: recording poster options fetch failed")
		return echo.NewHTTPError(http.StatusBadGateway,
			"upstream poster fetch failed: "+fetchErr.Error())
	}
	return c.JSON(http.StatusOK, pickerOptionsResponse{Options: options})
}

// handleListRecordingFanartOptions handles
// GET /api/v1/recordings/:id/fanart-options. Returns the Encora
// /screenshots URLs for the recording. has_screenshots=false yields a
// 200 with an empty options array — saves an upstream round-trip on
// recordings the API guarantees are empty.
func (s *Server) handleListRecordingFanartOptions(c echo.Context) error {
	id, err := parseRecordingIDParam(c)
	if err != nil {
		return err
	}
	if encErr := s.requireEncoraScreenshots(); encErr != nil {
		return encErr
	}
	loaded, loadErr := storage.LoadRecording(c.Request().Context(), s.db, id)
	if errors.Is(loadErr, storage.ErrRecordingNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, loadErr.Error())
	}
	if loadErr != nil {
		return loadErr
	}
	if !loaded.Recording.Metadata.HasScreenshots {
		return c.JSON(http.StatusOK, emptyOptionsResponse())
	}

	upstreamCtx, cancel := context.WithTimeout(c.Request().Context(), pickerUpstreamTimeout)
	defer cancel()
	urls, _, fetchErr := s.encoraScreenshots.Screenshots(upstreamCtx, id)
	if fetchErr != nil {
		s.logger.Warn().
			Err(fetchErr).
			Int64("recording_id", id).
			Msg("picker: fanart options fetch failed")
		return echo.NewHTTPError(http.StatusBadGateway,
			"upstream screenshots fetch failed: "+fetchErr.Error())
	}
	options := make([]pickerOption, 0, len(urls))
	for _, u := range urls {
		if u == "" {
			continue
		}
		options = append(options, pickerOption{URL: u, Source: "encora"})
	}
	return c.JSON(http.StatusOK, pickerOptionsResponse{Options: options})
}

// handleListActorHeadshotOptions handles
// GET /api/v1/actors/:id/headshot-options. Returns the StageMedia
// headshot URL for the performer (at most one URL today; modeled as a
// list so the JSON shape stays consistent with the other options
// endpoints). Empty when stagemedia has no hit, the performer doesn't
// appear on any of the user's recordings, or stagemedia returns
// nothing for the (show, performer) tuple.
func (s *Server) handleListActorHeadshotOptions(c echo.Context) error {
	actorID, err := parseActorIDParam(c)
	if err != nil {
		return err
	}
	if smErr := s.requireStagemedia(); smErr != nil {
		return smErr
	}

	// Resolve a show_id to scope the /api/images call. StageMedia
	// keys headshots on (show_id, performer_id); we use the first
	// recording the performer appears in. No recordings -> empty.
	showID, lookupErr := firstShowIDForPerformer(c.Request().Context(), s.db, actorID)
	if lookupErr != nil {
		s.logger.Warn().
			Err(lookupErr).
			Int64("actor_id", actorID).
			Msg("picker: actor show-id lookup failed")
		return lookupErr
	}
	if showID == 0 {
		return c.JSON(http.StatusOK, emptyOptionsResponse())
	}

	upstreamCtx, cancel := context.WithTimeout(c.Request().Context(), pickerUpstreamTimeout)
	defer cancel()
	imgs, fetchErr := s.Stagemedia().Images(upstreamCtx, showID, []int64{actorID})
	if fetchErr != nil {
		s.logger.Warn().
			Err(fetchErr).
			Int64("actor_id", actorID).
			Int64("show_id", showID).
			Msg("picker: actor headshot options fetch failed")
		return echo.NewHTTPError(http.StatusBadGateway,
			"upstream headshot fetch failed: "+fetchErr.Error())
	}

	options := make([]pickerOption, 0, len(imgs.Performers))
	for _, p := range imgs.Performers {
		if p.ID != actorID || p.URL == "" {
			continue
		}
		options = append(options, pickerOption{URL: p.URL, Source: "stagemedia"})
	}
	return c.JSON(http.StatusOK, pickerOptionsResponse{Options: options})
}

// firstShowIDForPerformer returns the first show_id from the
// performer's credited recordings, or 0 when the performer has none.
// Used to scope the StageMedia /api/images call for actor headshots.
func firstShowIDForPerformer(
	ctx context.Context, db *sql.DB, performerID int64,
) (int64, error) {
	recIDs, err := storage.ListRecordingsForPerformer(ctx, db, performerID)
	if err != nil {
		return 0, err
	}
	for _, recID := range recIDs {
		loaded, loadErr := storage.LoadRecording(ctx, db, recID)
		if errors.Is(loadErr, storage.ErrRecordingNotFound) {
			continue
		}
		if loadErr != nil {
			return 0, loadErr
		}
		if showID := loaded.Recording.Metadata.ShowID; showID > 0 {
			return showID, nil
		}
	}
	return 0, nil
}
