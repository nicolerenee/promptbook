package server

// upstream_proxy.go — GET /api/v1/upstream-image?url=<raw>
//
// Streams a thumbnail image from a vetted upstream host (StageMedia,
// Encora) back through the SPA's same-origin so the browser doesn't
// have to make the cross-origin fetch itself.
//
// Why proxy at all: Safari on macOS aborts cross-origin <img> loads
// from a localhost page to stagemedia.me with "network connection
// lost" — even with referrerpolicy=no-referrer. Loading the same URL
// in a top-level tab works. The picker modal needs the strip to
// render reliably, so we relay the bytes server-side. The "Set from
// URL" handlers still consume the raw upstream URL — only the
// thumbnail render path goes through this proxy.
//
// Hardening:
//   - Allowlist of host suffixes; anything else is 400.
//   - Only http/https schemes.
//   - GET only.
//   - 8 s upstream timeout, 10 MiB body cap (matches upload cap).
//   - Content-Type forwarded from upstream when it's image/*; coerced
//     to application/octet-stream otherwise so a misconfigured upstream
//     can't trick the browser into rendering HTML.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/version"
)

const (
	upstreamProxyTimeout      = 8 * time.Second
	upstreamProxyMaxBodyBytes = 10 * 1024 * 1024
)

// upstreamHostAllowlist enumerates the host suffixes the proxy will
// fetch from. Suffix match (host == s || strings.HasSuffix(host,
// "."+s)) so subdomains of the listed hosts are allowed but a
// look-alike like "stagemedia.me.evil.com" is not.
var upstreamHostAllowlist = []string{ //nolint:gochecknoglobals // immutable allowlist
	"stagemedia.me",
	"encora.it",
}

// handleUpstreamImageProxy streams a vetted upstream image back to the
// caller. URL comes in via ?url=<encoded raw URL>; an empty / invalid
// / off-allowlist URL is a 400. Upstream non-2xx is 502; upstream
// timeout is 504.
func (s *Server) handleUpstreamImageProxy(c echo.Context) error {
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

	ctx, cancel := context.WithTimeout(c.Request().Context(), upstreamProxyTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return fmt.Errorf("upstream proxy: build request: %w", err)
	}
	req.Header.Set("User-Agent", version.UserAgent())
	req.Header.Set("Accept", "image/*,*/*;q=0.5")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return echo.NewHTTPError(http.StatusGatewayTimeout, "upstream timeout")
		}
		s.logger.Warn().Err(err).Str("url", raw).Msg("upstream image proxy fetch failed")
		return echo.NewHTTPError(http.StatusBadGateway, "upstream fetch failed")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		s.logger.Warn().
			Int("status", resp.StatusCode).
			Str("url", raw).
			Msg("upstream image proxy non-2xx")
		return echo.NewHTTPError(http.StatusBadGateway,
			fmt.Sprintf("upstream returned %d", resp.StatusCode))
	}

	contentType := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "image/") {
		// Don't let upstream sneak HTML / JS through us.
		contentType = "application/octet-stream"
	}
	c.Response().Header().Set("Content-Type", contentType)
	c.Response().Header().Set("X-Content-Type-Options", "nosniff")
	c.Response().WriteHeader(http.StatusOK)

	limited := io.LimitReader(resp.Body, upstreamProxyMaxBodyBytes)
	if _, copyErr := io.Copy(c.Response().Writer, limited); copyErr != nil {
		// Headers are already flushed — just log.
		s.logger.Warn().Err(copyErr).Str("url", raw).Msg("upstream image proxy stream failed")
	}
	return nil
}

// hostAllowed reports whether host is one of the allowlisted upstream
// hostnames or a subdomain thereof. Matching is exact + suffix; a
// leading-dot check guards against "stagemedia.me.evil.com" sneaking
// through a naive HasSuffix.
func hostAllowed(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	for _, allowed := range upstreamHostAllowlist {
		if host == allowed || strings.HasSuffix(host, "."+allowed) {
			return true
		}
	}
	return false
}
