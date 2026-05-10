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
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/probe"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// pickerUpstreamTimeout caps how long any one /poster-options /
// /fanart-options / /headshot-options call waits on upstream before
// the handler bails. Five seconds is enough for a normal StageMedia /
// Encora round-trip but tight enough that a hung backend doesn't pin
// the picker modal indefinitely — the user can hit "Re-fetch" once
// upstream is healthy again.
const pickerUpstreamTimeout = 5 * time.Second

// pickerFanartFrameCount is the number of still-frame extracts the
// fanart fallback emits. The user picked 10 — enough that the spread
// covers different scenes (two per "act" in a typical 2.5h Broadway
// recording) but small enough that 10 sequential ffmpeg calls finish
// in 3-5 seconds on local SSD. Hard-coded per spec; not configurable.
const pickerFanartFrameCount = 10

// pickerFrameExtractTimeout caps the whole frame-extract batch. Each
// individual ffmpeg call against a local file takes well under a
// second on modern hardware, so 30 seconds is comfortable headroom
// even for a slow disk or a very long recording. Larger than the
// upstream timeout because frame extraction is meaningfully slower
// than an HTTP round-trip.
const pickerFrameExtractTimeout = 30 * time.Second

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
// /screenshots URLs for the recording when available; falls back to
// extracting still frames from the local video file when Encora has
// nothing curated for this recording (the common case for Broadway —
// most recordings ship without screenshots).
//
// The has_screenshots metadata flag short-circuits the Encora call
// when we already know the API has nothing — saves a round-trip + a
// rate-limit budget tick. The frame fallback fires in both that
// branch and the "screenshots returned empty" branch so the user
// gets the same picker shape regardless of why upstream was empty.
//
// Refresh handling: ?refresh=true on this endpoint clears the cached
// frame extracts before re-extracting, so the picker's Re-fetch
// button rerolls the random offsets. Without ?refresh the cached
// extracts (if any) are returned directly — extraction is cheap but
// not free, and the user opens the modal much more often than they
// click Re-fetch.
//
// Failure modes (no version on disk, no duration, ffmpeg missing,
// extraction errors) all surface as a 200 with an empty options
// array + a logged reason. The picker's empty-state copy makes the
// "no upstream + no fallback" case legible to the user.
func (s *Server) handleListRecordingFanartOptions(c echo.Context) error {
	id, err := parseRecordingIDParam(c)
	if err != nil {
		return err
	}
	loaded, loadErr := storage.LoadRecording(c.Request().Context(), s.db, id)
	if errors.Is(loadErr, storage.ErrRecordingNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, loadErr.Error())
	}
	if loadErr != nil {
		return loadErr
	}

	refresh := c.QueryParam("refresh") == "true"

	encoraOptions, encoraErr := s.fetchEncoraFanartOptions(
		c.Request().Context(), id, loaded.Recording.Metadata.HasScreenshots,
	)
	if encoraErr != nil {
		// A real upstream failure (timeout, 5xx) gets surfaced as a
		// 502 — the user already has a Re-fetch button to retry. We
		// don't silently fall through to frames here because the user
		// might genuinely want the curated screenshots.
		s.logger.Warn().
			Err(encoraErr).
			Int64("recording_id", id).
			Msg("picker: fanart options fetch failed")
		return echo.NewHTTPError(http.StatusBadGateway,
			"upstream screenshots fetch failed: "+encoraErr.Error())
	}
	if len(encoraOptions) > 0 {
		return c.JSON(http.StatusOK, pickerOptionsResponse{Options: encoraOptions})
	}

	// Encora had nothing — try the frame fallback.
	frameOptions := s.fanartFrameOptions(c.Request().Context(), id, refresh)
	return c.JSON(http.StatusOK, pickerOptionsResponse{Options: frameOptions})
}

// fetchEncoraFanartOptions calls Encora's /screenshots endpoint when
// the screenshots client is configured AND the recording's metadata
// flag indicates the API has data. Returns an empty slice + nil error
// for the "no client" / "flag says empty" cases — both are
// "fall through to the frame fallback" signals, not server errors.
func (s *Server) fetchEncoraFanartOptions(
	ctx context.Context, recordingID int64, hasScreenshots bool,
) ([]pickerOption, error) {
	if s.encoraScreenshots == nil || !hasScreenshots {
		return nil, nil
	}
	upstreamCtx, cancel := context.WithTimeout(ctx, pickerUpstreamTimeout)
	defer cancel()
	urls, _, err := s.encoraScreenshots.Screenshots(upstreamCtx, recordingID)
	if err != nil {
		return nil, err
	}
	out := make([]pickerOption, 0, len(urls))
	for _, u := range urls {
		if u == "" {
			continue
		}
		out = append(out, pickerOption{URL: u, Source: "encora"})
	}
	return out, nil
}

// fanartFrameOptions runs the fanart-fallback frame extraction. When
// refresh is true the existing frames cache is cleared before
// re-extracting (re-rolls the random offsets so the user gets a
// different set of stills). Otherwise a populated cache is returned
// directly without re-running ffmpeg — extraction is cheap but the
// modal opens often enough that paying the cost on every visit would
// add up.
//
// The "frames" Source tag lets the SPA render the picker caption
// "Random frames from your local file" so the user knows they're
// seeing a fallback, not curated art.
//
// Every soft-failure path (no cache configured, no version on disk,
// no duration, ffmpeg failure, etc.) returns an empty slice with a
// logged reason at debug level — the picker handles "no options"
// gracefully and the user can investigate via logs if they want to.
func (s *Server) fanartFrameOptions(
	ctx context.Context, recordingID int64, refresh bool,
) []pickerOption {
	cache := s.imageCache
	if cache == nil || cache.Disabled() {
		s.logger.Debug().
			Int64("recording_id", recordingID).
			Msg("picker: frame fallback skipped (image cache disabled)")
		return []pickerOption{}
	}

	// On a refresh request, scrub the cache before checking so the
	// extractor always re-runs.
	if refresh {
		if clearErr := cache.ClearFrames(recordingID); clearErr != nil {
			s.logger.Warn().
				Err(clearErr).
				Int64("recording_id", recordingID).
				Msg("picker: clear frames cache failed")
		}
	}

	// Fast-path: cache already has frames, return them directly.
	if cached := s.listCachedFrameOptions(recordingID); len(cached) > 0 {
		return cached
	}

	videoPath, durationSeconds, ok := s.fanartVideoSource(ctx, recordingID)
	if !ok {
		return []pickerOption{}
	}

	extractCtx, cancel := context.WithTimeout(ctx, pickerFrameExtractTimeout)
	defer cancel()
	outDir := cache.FramesPath(recordingID)
	frames, extractErr := s.frameExtractorOrFallback().Extract(
		extractCtx,
		videoPath,
		outDir,
		durationSeconds,
		pickerFanartFrameCount,
		nil, // nil rng -> the extractor seeds its own.
	)
	if extractErr != nil {
		s.logger.Warn().
			Err(extractErr).
			Int64("recording_id", recordingID).
			Str("video_path", videoPath).
			Msg("picker: frame extraction failed")
		// Even on extraction error we may have partial output —
		// return what landed on disk.
	}
	if len(frames) == 0 {
		return []pickerOption{}
	}
	return s.listCachedFrameOptions(recordingID)
}

// listCachedFrameOptions enumerates the recording's existing frame
// extracts in numeric order and maps them to pickerOptions. Returns
// an empty slice when the directory is missing or empty. Walks
// pickerFanartFrameCount slots in order so the response shape stays
// predictable across runs (no os.ReadDir-driven ordering surprises).
func (s *Server) listCachedFrameOptions(recordingID int64) []pickerOption {
	cache := s.imageCache
	if cache == nil || cache.Disabled() {
		return []pickerOption{}
	}
	dir := cache.FramesPath(recordingID)
	if dir == "" {
		return []pickerOption{}
	}
	out := make([]pickerOption, 0, pickerFanartFrameCount)
	for i := range pickerFanartFrameCount {
		framePath := cache.FramePath(recordingID, i)
		if framePath == "" {
			continue
		}
		info, statErr := os.Stat(framePath)
		if statErr != nil || info.IsDir() {
			continue
		}
		out = append(out, pickerOption{
			URL:    cache.FrameURL(recordingID, i),
			Source: "frames",
		})
	}
	return out
}

// fanartVideoSource resolves the on-disk video path + duration the
// frame extractor needs. Pulls the recording's primary version from
// storage.ListVersions (largest-file-first ordering puts the master
// at index 0, which is what we want — best resolution gives the
// nicest fallback frames). Duration comes from the persisted
// MediaInfoJSON when available; otherwise we fall back to a fresh
// ffprobe so a legacy version without the blob still works.
//
// Returns ok=false (with a debug-level log line) on any path that
// can't yield a usable extraction input — missing version, missing
// file on disk, zero duration after probing. Caller surfaces those
// as an empty options array.
func (s *Server) fanartVideoSource(
	ctx context.Context, recordingID int64,
) (string, float64, bool) {
	versions, err := storage.ListVersions(ctx, s.db, recordingID)
	if err != nil {
		s.logger.Warn().
			Err(err).
			Int64("recording_id", recordingID).
			Msg("picker: frame fallback: list versions failed")
		return "", 0, false
	}
	if len(versions) == 0 {
		s.logger.Debug().
			Int64("recording_id", recordingID).
			Msg("picker: frame fallback skipped (no versions on disk)")
		return "", 0, false
	}
	primary := versions[0]
	if primary.FilePath == "" {
		return "", 0, false
	}
	if info, statErr := os.Stat(primary.FilePath); statErr != nil || info.IsDir() {
		s.logger.Debug().
			Int64("recording_id", recordingID).
			Str("video_path", primary.FilePath).
			Msg("picker: frame fallback skipped (file missing on disk)")
		return "", 0, false
	}

	duration := durationFromMediaInfoJSON(primary.MediaInfoJSON)
	if duration <= 0 {
		// Legacy version without a persisted blob — probe live so the
		// fallback still works. We accept the rare extra ffprobe call
		// here because the picker is interactive (user is waiting on
		// the modal anyway) and the alternative is a permanently empty
		// fanart tab for older imports.
		info, probeErr := s.proberOrFallback().Probe(ctx, primary.FilePath)
		if probeErr != nil {
			s.logger.Warn().
				Err(probeErr).
				Int64("recording_id", recordingID).
				Str("video_path", primary.FilePath).
				Msg("picker: frame fallback: live probe failed")
			return "", 0, false
		}
		duration = info.DurationSeconds
	}
	if duration <= 0 {
		s.logger.Debug().
			Int64("recording_id", recordingID).
			Str("video_path", primary.FilePath).
			Msg("picker: frame fallback skipped (zero duration)")
		return "", 0, false
	}
	return primary.FilePath, duration, true
}

// durationFromMediaInfoJSON pulls the durationSeconds field out of a
// persisted media-info blob without unmarshaling the entire shape.
// Returns 0 on any decode failure or empty input — caller falls back
// to a live probe in that case.
func durationFromMediaInfoJSON(blob string) float64 {
	if blob == "" {
		return 0
	}
	var partial struct {
		DurationSeconds float64 `json:"durationSeconds"`
	}
	if err := json.Unmarshal([]byte(blob), &partial); err != nil {
		return 0
	}
	return partial.DurationSeconds
}

// Compile-time guard: probe.FrameRandSource must satisfy what the
// extractor expects. The unused identifier ensures the import isn't
// optimized away when no other helper in this file references probe.
var _ probe.FrameRandSource = (*probeFrameRandSourceShim)(nil)

// probeFrameRandSourceShim only exists to anchor the compile-time
// guard above. It carries no behaviour and isn't constructed at
// runtime.
type probeFrameRandSourceShim struct{}

// Float64 satisfies probe.FrameRandSource for the compile-time guard.
func (probeFrameRandSourceShim) Float64() float64 { return 0 }

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
	ctx context.Context, client *ent.Client, performerID int64,
) (int64, error) {
	recIDs, err := storage.ListRecordingsForPerformer(ctx, client, performerID)
	if err != nil {
		return 0, err
	}
	for _, recID := range recIDs {
		loaded, loadErr := storage.LoadRecording(ctx, client, recID)
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
