// Package stagemedia is a thin client for the StageMedia.me image API.
//
// Auth is Bearer token. The API exposes a single endpoint, /api/images, that
// returns curated posters for a show plus headshots for a batch of performer
// ids. Posters and headshots come back as fully-qualified hot-link URLs to
// stagemedia's CDN — promptbook downloads + caches them locally so detail
// pages survive upstream deletions.
//
// No documented rate limit, but the client surfaces RateLimitInfo for
// symmetry with the Encora client and so future server-side throttling can
// be honored without an API change.
package stagemedia

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/nicolerenee/promptbook/internal/version"
)

// defaultHTTPTimeout is the timeout applied when callers don't pass an
// HTTPClient.
const defaultHTTPTimeout = 30 * time.Second

// posterSentinelActorID is the magic actor_ids value used when the caller
// only wants posters back. The API hard-rejects /api/images calls without
// any actor_ids value, so the Jellyfin plugin (and now this client) sends
// actor_ids=1 as a filler when posters-only is the goal.
const posterSentinelActorID = "1"

// Errors returned by the client.
var (
	// ErrUnauthorized is returned on 401.
	ErrUnauthorized = errors.New("stagemedia: unauthorized")
	// ErrNotFound is returned on 404. Not currently observed in the wild —
	// the API prefers 400 with an error envelope — but kept for symmetry
	// with the Encora client and so a future 404 doesn't masquerade as
	// ErrUnexpectedStatus.
	ErrNotFound = errors.New("stagemedia: not found")
	// ErrBadRequest is returned on 400. The upstream error string from the
	// response envelope is wrapped into the returned error.
	ErrBadRequest = errors.New("stagemedia: bad request")
	// ErrUnexpectedStatus is returned on any non-200/400/401/404 status.
	ErrUnexpectedStatus = errors.New("stagemedia: unexpected status")
)

// Client talks to the StageMedia API.
type Client struct {
	baseURL    *url.URL
	apiKey     string
	userAgent  string
	httpClient *http.Client
	logger     zerolog.Logger
}

// Options configures a new Client.
type Options struct {
	BaseURL string
	APIKey  string
	// HTTPClient is optional; if nil a default with sane timeouts is used.
	HTTPClient *http.Client
	Logger     zerolog.Logger
}

// New constructs a Client.
func New(opts Options) (*Client, error) {
	if opts.APIKey == "" {
		return nil, errors.New("stagemedia: api key is required")
	}
	if opts.BaseURL == "" {
		opts.BaseURL = "https://stagemedia.me"
	}
	u, err := url.Parse(opts.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base url: %w", err)
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &Client{
		baseURL:    u,
		apiKey:     opts.APIKey,
		userAgent:  version.UserAgent(),
		httpClient: hc,
		logger:     opts.Logger,
	}, nil
}

// RateLimitInfo carries the rate-limit headers from the most recent response.
//
// StageMedia doesn't currently document a rate limit, so all fields will be
// zero in practice — but the type matches the Encora client's shape so
// callers can treat third-party clients uniformly.
type RateLimitInfo struct {
	Limit     int
	Remaining int
	Reset     time.Time
	// RetryAfter is parsed from the Retry-After header. Zero when absent.
	RetryAfter time.Duration
}

// Posters fetches just the curated poster URLs for a show. It calls
// /api/images with actor_ids=1 as a sentinel since the API rejects the call
// without an actor_ids value entirely. Returns a non-nil empty slice when
// the show has no curated posters.
func (c *Client) Posters(ctx context.Context, showID int64) ([]string, error) {
	imgs, err := c.fetchImages(ctx, showID, posterSentinelActorID)
	if err != nil {
		return nil, err
	}
	if imgs.Posters == nil {
		return []string{}, nil
	}
	return imgs.Posters, nil
}

// Images fetches posters and headshots for a show + batch of performer ids.
// performerIDs must be non-empty; an empty slice short-circuits with
// ErrBadRequest without making an HTTP call (the API would reject it
// upstream anyway).
//
// Note that performer ids stagemedia doesn't have are silently dropped from
// the returned Performers slice — callers that need per-id presence
// information should diff the requested-vs-returned id sets.
func (c *Client) Images(
	ctx context.Context, showID int64, performerIDs []int64,
) (Images, error) {
	if len(performerIDs) == 0 {
		return Images{}, fmt.Errorf("%w: at least one performer id is required", ErrBadRequest)
	}
	parts := make([]string, len(performerIDs))
	for i, id := range performerIDs {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return c.fetchImages(ctx, showID, strings.Join(parts, ","))
}

// fetchImages issues the /api/images call with the given pre-formatted
// actor_ids string. RateLimitInfo from doRequest is currently discarded —
// stagemedia doesn't emit rate-limit headers — but the parser is in place
// so future server-side throttling can be plumbed through without an API
// change.
func (c *Client) fetchImages(
	ctx context.Context, showID int64, actorIDs string,
) (Images, error) {
	u := *c.baseURL
	u.Path = "/api/images"
	q := url.Values{}
	q.Set("show_id", strconv.FormatInt(showID, 10))
	q.Set("actor_ids", actorIDs)
	u.RawQuery = q.Encode()

	var imgs Images
	if _, err := c.doRequest(ctx, http.MethodGet, u.String(), &imgs); err != nil {
		return Images{}, err
	}
	return imgs, nil
}

// doRequest executes a single HTTP request, parses rate-limit headers, and
// decodes the body into out (if non-nil). 400 responses are decoded into a
// throwaway envelope so the upstream error string can be wrapped into
// ErrBadRequest.
func (c *Client) doRequest(
	ctx context.Context,
	method, fullURL string,
	out any,
) (RateLimitInfo, error) {
	req, err := http.NewRequestWithContext(ctx, method, fullURL, nil)
	if err != nil {
		return RateLimitInfo{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return RateLimitInfo{}, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	rl := parseRateLimit(resp.Header)

	switch resp.StatusCode {
	case http.StatusOK:
		// fall through
	case http.StatusUnauthorized:
		return rl, ErrUnauthorized
	case http.StatusNotFound:
		return rl, ErrNotFound
	case http.StatusBadRequest:
		// The 400 body is the same Images envelope with a populated
		// "error" field; decode it to surface the upstream message.
		var env Images
		if decErr := json.NewDecoder(resp.Body).Decode(&env); decErr == nil &&
			env.Error != nil && *env.Error != "" {
			return rl, fmt.Errorf("%w: %s", ErrBadRequest, *env.Error)
		}
		return rl, ErrBadRequest
	default:
		return rl, fmt.Errorf("%w: %d", ErrUnexpectedStatus, resp.StatusCode)
	}

	if out != nil {
		if decErr := json.NewDecoder(resp.Body).Decode(out); decErr != nil {
			return rl, fmt.Errorf("decode body: %w", decErr)
		}
	}
	return rl, nil
}

// parseRateLimit pulls X-RateLimit-* / Retry-After out of response headers.
// StageMedia doesn't currently emit any of these, but the client surfaces
// them for parity with Encora and to be ready for future server-side
// throttling.
func parseRateLimit(h http.Header) RateLimitInfo {
	var rl RateLimitInfo
	if v := h.Get("X-Ratelimit-Limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			rl.Limit = n
		}
	}
	if v := h.Get("X-Ratelimit-Remaining"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			rl.Remaining = n
		}
	}
	if v := h.Get("X-Ratelimit-Reset"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			rl.Reset = time.Unix(n, 0)
		}
	}
	if v := h.Get("Retry-After"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			rl.RetryAfter = time.Duration(n) * time.Second
		}
	}
	return rl
}
