package server

// poster_preview.go — GET /api/v1/recordings/:id/poster-preview?url=<raw>
//
// Renders the recording's burned-in band on top of an upstream URL the
// user has staged in the picker, returning the composited JPEG so the
// SPA can show a "what will this look like?" tile next to the
// "Current" image before the user commits the choice. Read-only — the
// recording's poster.jpg on disk is NOT touched until the user clicks
// Save and the from-URL handler runs.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/imagerender"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// posterPreviewMaxBytes caps the upstream body the preview endpoint
// will read. Same number the upstream proxy uses; a poster source
// over 10 MiB is almost certainly an error.
const posterPreviewMaxBytes = upstreamProxyMaxBodyBytes

// posterPreviewJPEGQuality is the JPEG quality used for the encoded
// preview. Higher than the cache's 90 because the preview is
// transient — the user looks at it once and moves on; an extra few
// KB doesn't matter.
const posterPreviewJPEGQuality = 92

// handleRecordingPosterPreview serves a transient JPEG of the
// recording's burn-in band composited over the upstream image at
// `url`. The endpoint shares the upstream-image proxy's allowlist
// + scheme guards so it can't be turned into an open image fetcher.
//
// Failure modes:
//   - 400: bad URL / off-allowlist / missing url param.
//   - 404: recording id not in storage.
//   - 502: upstream fetch failed.
//   - 503: server has no ImageRenderer wired (image cache disabled).
func (s *Server) handleRecordingPosterPreview(c echo.Context) error {
	id, err := storage.ParseRecordingID(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if s.imageRenderer == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable,
			"image renderer not configured")
	}

	raw := strings.TrimSpace(c.QueryParam("url"))
	if raw == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "missing url query param")
	}
	target, err := url.Parse(raw)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "url parse: "+err.Error())
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return echo.NewHTTPError(http.StatusBadRequest, "url scheme must be http or https")
	}
	if !hostAllowed(target.Hostname()) {
		return echo.NewHTTPError(http.StatusBadRequest,
			"upstream host not allowed: "+target.Hostname())
	}

	src, fetchErr := fetchAndDecode(c.Request().Context(), target.String(), s)
	if fetchErr != nil {
		return fetchErr
	}

	overrides := imagerender.PreviewOverrides{
		Position:    strings.TrimSpace(c.QueryParam("position")),
		ImageRegion: strings.TrimSpace(c.QueryParam("region")),
	}
	composed, err := s.imageRenderer.Preview(
		c.Request().Context(), id, src, overrides)
	if err != nil {
		s.logger.Warn().Err(err).Int64("recording_id", id).Msg("poster preview compose failed")
		return echo.NewHTTPError(http.StatusInternalServerError,
			"preview compose failed: "+err.Error())
	}

	var buf bytes.Buffer
	if encErr := jpeg.Encode(&buf, composed,
		&jpeg.Options{Quality: posterPreviewJPEGQuality}); encErr != nil {
		return fmt.Errorf("poster preview: encode jpeg: %w", encErr)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.Blob(http.StatusOK, "image/jpeg", buf.Bytes())
}

// fetchAndDecode fetches the upstream URL with the proxy's allowlist
// + timeouts, then decodes the body via image.Decode. Returns an
// echo.HTTPError on failure so the handler can return it directly.
func fetchAndDecode(
	ctx context.Context, rawURL string, s *Server,
) (image.Image, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, upstreamProxyTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "promptbook-poster-preview/1.0")
	req.Header.Set("Accept", "image/*,*/*;q=0.5")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, echo.NewHTTPError(http.StatusGatewayTimeout, "upstream timeout")
		}
		s.logger.Warn().Err(err).Str("url", rawURL).Msg("poster preview fetch failed")
		return nil, echo.NewHTTPError(http.StatusBadGateway, "upstream fetch failed")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		s.logger.Warn().
			Int("status", resp.StatusCode).
			Str("url", rawURL).
			Msg("poster preview upstream non-2xx")
		return nil, echo.NewHTTPError(http.StatusBadGateway,
			fmt.Sprintf("upstream returned %d", resp.StatusCode))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, posterPreviewMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read upstream body: %w", err)
	}
	if int64(len(body)) > posterPreviewMaxBytes {
		return nil, echo.NewHTTPError(http.StatusBadGateway, "upstream image too large")
	}

	img, _, err := image.Decode(bytes.NewReader(body))
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusBadGateway,
			"decode upstream image: "+err.Error())
	}
	return img, nil
}
