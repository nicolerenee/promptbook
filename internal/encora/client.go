// Package encora is a thin client for the Encora REST API.
//
// Auth is Bearer token. The API publishes a 30-req/min rate limit; this
// client surfaces the X-RateLimit-Remaining header on every response so
// callers can pace themselves, and returns ErrRateLimited on 429.
package encora

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/rs/zerolog"
)

// Errors returned by the client.
var (
	// ErrUnauthorized is returned on 401.
	ErrUnauthorized = errors.New("encora: unauthorized")
	// ErrNotFound is returned on 404 from a recording lookup.
	ErrNotFound = errors.New("encora: not found")
	// ErrRateLimited is returned on 429.
	ErrRateLimited = errors.New("encora: rate limited")
)

// Client talks to the Encora API.
type Client struct {
	baseURL    *url.URL
	apiKey     string
	userAgent  string
	httpClient *http.Client
	logger     zerolog.Logger
}

// Options configures a new Client.
type Options struct {
	BaseURL   string
	APIKey    string
	UserAgent string
	// HTTPClient is optional; if nil a default with sane timeouts is used.
	HTTPClient *http.Client
	Logger     zerolog.Logger
}

// New constructs a Client.
func New(opts Options) (*Client, error) {
	if opts.APIKey == "" {
		return nil, errors.New("encora: api key is required")
	}
	if opts.BaseURL == "" {
		opts.BaseURL = "https://encora.it"
	}
	u, err := url.Parse(opts.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base url: %w", err)
	}
	if opts.UserAgent == "" {
		opts.UserAgent = "promptbook/0.0.1"
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{
		baseURL:    u,
		apiKey:     opts.APIKey,
		userAgent:  opts.UserAgent,
		httpClient: hc,
		logger:     opts.Logger,
	}, nil
}

// RateLimitInfo carries the rate-limit headers from the most recent response.
type RateLimitInfo struct {
	Limit     int
	Remaining int
	Reset     time.Time
}

// do executes a request and decodes a JSON body. The caller is responsible for
// constructing the path (relative to /api/).
func (c *Client) do(ctx context.Context, method, path string, out any) (RateLimitInfo, error) {
	u := *c.baseURL
	u.Path = "/api/" + path

	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
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
	case http.StatusTooManyRequests:
		return rl, ErrRateLimited
	default:
		return rl, fmt.Errorf("encora: unexpected status %d", resp.StatusCode)
	}

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return rl, fmt.Errorf("decode body: %w", err)
		}
	}
	return rl, nil
}

func parseRateLimit(h http.Header) RateLimitInfo {
	var rl RateLimitInfo
	if v := h.Get("X-RateLimit-Limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			rl.Limit = n
		}
	}
	if v := h.Get("X-RateLimit-Remaining"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			rl.Remaining = n
		}
	}
	if v := h.Get("X-RateLimit-Reset"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			rl.Reset = time.Unix(n, 0)
		}
	}
	return rl
}
