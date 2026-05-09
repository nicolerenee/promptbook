package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// maxUploadBytes caps a single multipart upload. 10 MiB is comfortably
// above realistic poster + fanart sizes (a 4K JPEG at quality 95 is
// ~3-4 MB) but tight enough that an oversized client can't OOM the
// process. Enforced via http.MaxBytesReader BEFORE we read anything
// into a decoder, so the cap is the real ceiling — not just a
// post-hoc length check.
const maxUploadBytes = 10 * 1024 * 1024

// uploadFormField is the multipart field name every upload endpoint
// expects. Lifted into a constant so handlers + tests agree on the
// spelling.
const uploadFormField = "file"

// uploadResponse is the JSON envelope every upload endpoint returns.
// On success: {ok: true}. On failure: ok=false + a human-readable
// error.
type uploadResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// fromURLRequest is the JSON body for the "set from URL" endpoints.
// The picker UI POSTs the chosen upstream URL; the server downloads it
// into the canonical slot.
type fromURLRequest struct {
	URL string `json:"url"`
}

// readUploadBody pulls the "file" form-file out of a multipart request
// and returns its body reader. The caller must close the returned
// io.ReadCloser. The request body is wrapped in http.MaxBytesReader
// before parsing so an oversized payload trips a 400 instead of
// streaming through the decoder.
func readUploadBody(c echo.Context) (io.ReadCloser, error) {
	req := c.Request()
	req.Body = http.MaxBytesReader(c.Response().Writer, req.Body, maxUploadBytes)

	fh, err := c.FormFile(uploadFormField)
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("read upload: %s", err.Error()))
	}
	f, err := fh.Open()
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("open upload: %s", err.Error()))
	}
	return f, nil
}

// mapUploadError turns a SaveUploaded* / Fetch* error into a JSON
// envelope + HTTP status. Decode failures are the user's fault (400);
// everything else is a server fault (500).
func mapUploadError(err error) (int, uploadResponse) {
	if errors.Is(err, imagecache.ErrDisabled) {
		return http.StatusServiceUnavailable, uploadResponse{
			Error: "image cache not configured",
		}
	}
	// SaveUploaded* wraps decode errors with "decode upload:".
	if err != nil && strings.Contains(err.Error(), "decode upload") {
		return http.StatusBadRequest, uploadResponse{
			Error: "decode upload: not a recognised image (PNG/JPEG/GIF only)",
		}
	}
	return http.StatusInternalServerError, uploadResponse{
		Error: err.Error(),
	}
}

// handleUploadRecordingFanart handles POST
// /api/v1/recordings/:id/fanart-upload. Multipart "file" field is the
// upload payload (10 MiB cap). The bytes are decoded + re-encoded as
// JPEG and written to recordings/<id>/fanart.jpg.
func (s *Server) handleUploadRecordingFanart(c echo.Context) error {
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

	body, err := readUploadBody(c)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()

	if saveErr := s.ImageCache().SaveUploadedRecordingFanart(
		c.Request().Context(), id, body); saveErr != nil {
		status, resp := mapUploadError(saveErr)
		return c.JSON(status, resp)
	}
	return c.JSON(http.StatusOK, uploadResponse{OK: true})
}

// handleUploadRecordingPoster handles POST
// /api/v1/recordings/:id/poster-upload. The bytes are written to
// poster-src.jpg (the renderer's input) and an immediate Regenerate
// produces poster.jpg with the burned-in overlay.
func (s *Server) handleUploadRecordingPoster(c echo.Context) error {
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

	body, err := readUploadBody(c)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()

	if saveErr := s.ImageCache().SaveUploadedRecordingPosterSrc(
		c.Request().Context(), id, body); saveErr != nil {
		status, resp := mapUploadError(saveErr)
		return c.JSON(status, resp)
	}
	if r := s.ImageRenderer(); r != nil {
		if rerr := r.Regenerate(c.Request().Context(), id); rerr != nil {
			s.logger.Warn().
				Err(rerr).
				Int64("recording_id", id).
				Msg("poster-upload: regenerate failed; poster-src is on disk")
		}
	}
	return c.JSON(http.StatusOK, uploadResponse{OK: true})
}

// handleUploadShowBanner handles POST /api/v1/shows/:id/banner-upload.
// Writes the upload to shows/<id>/banner.jpg.
func (s *Server) handleUploadShowBanner(c echo.Context) error {
	showID, err := parseShowIDParam(c)
	if err != nil {
		return err
	}
	if cacheErr := s.requireImageCache(); cacheErr != nil {
		return cacheErr
	}

	body, err := readUploadBody(c)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()

	if saveErr := s.ImageCache().SaveUploadedShowBanner(
		c.Request().Context(), showID, body); saveErr != nil {
		status, resp := mapUploadError(saveErr)
		return c.JSON(status, resp)
	}
	return c.JSON(http.StatusOK, uploadResponse{OK: true})
}

// handleUploadActorHeadshot handles POST
// /api/v1/actors/:id/headshot-upload. New under v2 — the previous
// cache layout had no actor-upload affordance.
func (s *Server) handleUploadActorHeadshot(c echo.Context) error {
	actorID, err := parseActorIDParam(c)
	if err != nil {
		return err
	}
	if cacheErr := s.requireImageCache(); cacheErr != nil {
		return cacheErr
	}

	body, err := readUploadBody(c)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()

	if saveErr := s.ImageCache().SaveUploadedHeadshot(
		c.Request().Context(), actorID, body); saveErr != nil {
		status, resp := mapUploadError(saveErr)
		return c.JSON(status, resp)
	}
	return c.JSON(http.StatusOK, uploadResponse{OK: true})
}

// handleSetRecordingFanartFromURL handles POST
// /api/v1/recordings/:id/fanart-from-url. Body: {url: "..."}. The
// server downloads the URL into recordings/<id>/fanart.jpg (atomic).
// The existing slot is removed first so the picker's "set this URL"
// always lands fresh bytes (FetchRecordingFanart is idempotent on
// existing files and would otherwise short-circuit the download).
func (s *Server) handleSetRecordingFanartFromURL(c echo.Context) error {
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
	url, parseErr := parseFromURL(c)
	if parseErr != nil {
		return parseErr
	}
	_ = removeIfExists(s.ImageCache().RecordingFanartPath(id))
	if _, fetchErr := s.ImageCache().FetchRecordingFanart(
		c.Request().Context(), id, url); fetchErr != nil {
		status, resp := mapUploadError(fetchErr)
		return c.JSON(status, resp)
	}
	return c.JSON(http.StatusOK, uploadResponse{OK: true})
}

// handleSetRecordingPosterFromURL downloads the URL into poster-src.jpg
// and triggers a render so poster.jpg refreshes against the new source.
func (s *Server) handleSetRecordingPosterFromURL(c echo.Context) error {
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
	url, parseErr := parseFromURL(c)
	if parseErr != nil {
		return parseErr
	}
	_ = removeIfExists(s.ImageCache().RecordingPosterSrcPath(id))
	_ = removeIfExists(s.ImageCache().RecordingPosterPath(id))
	if _, fetchErr := s.ImageCache().FetchRecordingPosterSrc(
		c.Request().Context(), id, url); fetchErr != nil {
		status, resp := mapUploadError(fetchErr)
		return c.JSON(status, resp)
	}
	if r := s.ImageRenderer(); r != nil {
		if rerr := r.Regenerate(c.Request().Context(), id); rerr != nil {
			s.logger.Warn().
				Err(rerr).
				Int64("recording_id", id).
				Msg("poster-from-url: regenerate failed")
		}
	}
	return c.JSON(http.StatusOK, uploadResponse{OK: true})
}

// handleSetShowBannerFromURL downloads the URL into the show's banner
// slot.
func (s *Server) handleSetShowBannerFromURL(c echo.Context) error {
	showID, err := parseShowIDParam(c)
	if err != nil {
		return err
	}
	if cacheErr := s.requireImageCache(); cacheErr != nil {
		return cacheErr
	}
	if existsErr := s.showExists(c.Request().Context(), showID); existsErr != nil {
		return existsErr
	}
	url, parseErr := parseFromURL(c)
	if parseErr != nil {
		return parseErr
	}
	_ = removeIfExists(s.ImageCache().ShowBannerPath(showID))
	if _, fetchErr := s.ImageCache().FetchShowBanner(
		c.Request().Context(), showID, url); fetchErr != nil {
		status, resp := mapUploadError(fetchErr)
		return c.JSON(status, resp)
	}
	return c.JSON(http.StatusOK, uploadResponse{OK: true})
}

// handleSetActorHeadshotFromURL downloads the URL into the actor's
// headshot slot.
func (s *Server) handleSetActorHeadshotFromURL(c echo.Context) error {
	actorID, err := parseActorIDParam(c)
	if err != nil {
		return err
	}
	if cacheErr := s.requireImageCache(); cacheErr != nil {
		return cacheErr
	}
	url, parseErr := parseFromURL(c)
	if parseErr != nil {
		return parseErr
	}
	_ = removeIfExists(s.ImageCache().HeadshotPath(actorID))
	if _, fetchErr := s.ImageCache().FetchHeadshot(
		c.Request().Context(), actorID, url); fetchErr != nil {
		status, resp := mapUploadError(fetchErr)
		return c.JSON(status, resp)
	}
	return c.JSON(http.StatusOK, uploadResponse{OK: true})
}

// parseFromURL pulls the URL field off the JSON body and rejects empty
// values with a 400.
func parseFromURL(c echo.Context) (string, error) {
	var req fromURLRequest
	if err := c.Bind(&req); err != nil {
		return "", echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("decode body: %s", err.Error()))
	}
	url := strings.TrimSpace(req.URL)
	if url == "" {
		return "", echo.NewHTTPError(http.StatusBadRequest, "url is required")
	}
	return url, nil
}

// parseShowIDParam extracts and validates the show_id path parameter.
func parseShowIDParam(c echo.Context) (int64, error) {
	id, err := storage.ParseRecordingID(c.Param("id"))
	if err != nil {
		return 0, echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	return id, nil
}

// parseActorIDParam extracts and validates the actor_id path parameter.
// Rejects non-positive ids with a 400.
func parseActorIDParam(c echo.Context) (int64, error) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, echo.NewHTTPError(http.StatusBadRequest,
			"invalid actor id")
	}
	return id, nil
}

// removeIfExists removes path, ignoring os.ErrNotExist. Other errors
// propagate. Used by the from-url endpoints to clear an existing slot
// so the next FetchX call lands the new bytes (FetchX is idempotent
// on existing files).
func removeIfExists(path string) error {
	if path == "" {
		return nil
	}
	err := os.Remove(path)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
