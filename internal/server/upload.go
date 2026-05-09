package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// maxUploadBytes caps a single multipart upload. 10 MiB is comfortably
// above realistic poster + backdrop sizes (a 4K JPEG at quality 95 is
// ~3-4 MB) but tight enough that an oversized client can't OOM the
// process. Enforced via http.MaxBytesReader BEFORE we read anything
// into a decoder, so the cap is the real ceiling — not just a
// post-hoc length check.
const maxUploadBytes = 10 * 1024 * 1024

// uploadFormField is the multipart field name every upload endpoint
// expects. Lifted into a constant so the three handlers + tests agree
// on the spelling.
const uploadFormField = "file"

// uploadResponse is the JSON envelope every upload endpoint returns.
// On success: {ok: true, index: N} so the client can immediately POST
// the picker selection endpoint with the new index. On failure: ok=false
// + a human-readable error.
type uploadResponse struct {
	OK    bool   `json:"ok"`
	Index int    `json:"index,omitempty"`
	Error string `json:"error,omitempty"`
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

// mapUploadError turns a SaveUploaded* error into a JSON envelope +
// HTTP status. Decode failures are the user's fault (400); everything
// else is a server fault (500).
func mapUploadError(err error) (int, uploadResponse) {
	if errors.Is(err, imagecache.ErrDisabled) {
		return http.StatusServiceUnavailable, uploadResponse{
			Error: "image cache not configured",
		}
	}
	// SaveUploadedPoster wraps decode errors with "decode upload:".
	if err != nil && strings.Contains(err.Error(), "decode upload") {
		return http.StatusBadRequest, uploadResponse{
			Error: "decode upload: not a recognised image (PNG/JPEG/GIF only)",
		}
	}
	return http.StatusInternalServerError, uploadResponse{
		Error: err.Error(),
	}
}

// handleUploadBackdrop handles POST /api/v1/recordings/:id/backdrop-upload.
// Multipart "file" field is the upload payload (10 MiB cap). On success,
// returns the new index >= UploadIndexFloor; the client immediately
// follows up with POST /recordings/:id/backdrop {index: N} to make the
// upload the active selection.
func (s *Server) handleUploadBackdrop(c echo.Context) error {
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

	idx, saveErr := s.ImageCache().SaveUploadedBackdrop(c.Request().Context(), id, body)
	if saveErr != nil {
		status, resp := mapUploadError(saveErr)
		return c.JSON(status, resp)
	}
	return c.JSON(http.StatusOK, uploadResponse{OK: true, Index: idx})
}

// handleUploadPoster handles POST /api/v1/shows/:id/poster-upload. The
// path param is a show id, NOT a recording id — show posters are
// shared across every recording of the same show. Mirrors
// handleUploadBackdrop in shape.
func (s *Server) handleUploadPoster(c echo.Context) error {
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

	idx, saveErr := s.ImageCache().SaveUploadedPoster(c.Request().Context(), showID, body)
	if saveErr != nil {
		status, resp := mapUploadError(saveErr)
		return c.JSON(status, resp)
	}
	return c.JSON(http.StatusOK, uploadResponse{OK: true, Index: idx})
}

// handleUploadRecordingPoster handles POST
// /api/v1/recordings/:id/poster-upload. Posters are show-scoped, but
// the recording detail page is the natural place to upload one — so
// this wrapper resolves the recording's show_id and forwards the
// upload there. The returned index lives in the show's poster
// directory, NOT keyed on the recording id.
func (s *Server) handleUploadRecordingPoster(c echo.Context) error {
	id, err := parseRecordingIDParam(c)
	if err != nil {
		return err
	}
	if cacheErr := s.requireImageCache(); cacheErr != nil {
		return cacheErr
	}

	loaded, loadErr := storage.LoadRecording(c.Request().Context(), s.db, id)
	if errors.Is(loadErr, storage.ErrRecordingNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, loadErr.Error())
	}
	if loadErr != nil {
		return fmt.Errorf("load recording: %w", loadErr)
	}
	showID := loaded.Recording.Metadata.ShowID
	if showID == 0 {
		return c.JSON(http.StatusBadRequest, uploadResponse{
			Error: "recording has no show_id; cannot upload show poster",
		})
	}

	body, err := readUploadBody(c)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()

	idx, saveErr := s.ImageCache().SaveUploadedPoster(c.Request().Context(), showID, body)
	if saveErr != nil {
		status, resp := mapUploadError(saveErr)
		return c.JSON(status, resp)
	}
	return c.JSON(http.StatusOK, uploadResponse{OK: true, Index: idx})
}

// parseShowIDParam extracts and validates the show_id path parameter.
// Returns a 400 echo error when the value isn't a positive int64.
// Mirrors parseRecordingIDParam's contract; lives here because the
// show-poster-upload endpoint is the only consumer until the
// show-detail agent's work lands.
func parseShowIDParam(c echo.Context) (int64, error) {
	id, err := storage.ParseRecordingID(c.Param("id"))
	if err != nil {
		return 0, echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	return id, nil
}
