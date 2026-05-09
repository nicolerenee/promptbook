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
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// defaultHTTPTimeout is the timeout applied when callers don't pass an
// HTTPClient.
const defaultHTTPTimeout = 30 * time.Second

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
		hc = &http.Client{Timeout: defaultHTTPTimeout}
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
	// RetryAfter is parsed from the Retry-After header on 429 responses.
	// Encora documents this as integer seconds; the HTTP-date form is not
	// honored. Zero when the header is absent or malformed.
	RetryAfter time.Duration
}

// Profile returns the authenticated user's Encora profile.
func (c *Client) Profile(ctx context.Context) (Profile, RateLimitInfo, error) {
	var p Profile
	rl, err := c.do(ctx, http.MethodGet, "profile", &p)
	return p, rl, err
}

// Collection returns one page of the user's owned recordings. Page is the
// 1-indexed page number; pass 0 or 1 for the first page. The response includes
// NextPageURL for paginating; callers should follow it via CollectionURL.
func (c *Client) Collection(ctx context.Context, page int) (Page[CollectionEntry], RateLimitInfo, error) {
	path := "collection"
	if page > 1 {
		path = fmt.Sprintf("collection?page=%d", page)
	}
	var p Page[CollectionEntry]
	rl, err := c.do(ctx, http.MethodGet, path, &p)
	return p, rl, err
}

// CollectionURL fetches a fully-qualified collection URL (used to follow
// Page.NextPageURL across pages).
func (c *Client) CollectionURL(ctx context.Context, fullURL string) (Page[CollectionEntry], RateLimitInfo, error) {
	var p Page[CollectionEntry]
	rl, err := c.doAbsolute(ctx, http.MethodGet, fullURL, &p)
	return p, rl, err
}

// Wants returns one page of the user's wants list.
func (c *Client) Wants(ctx context.Context, page int) (Page[WantEntry], RateLimitInfo, error) {
	path := "wants"
	if page > 1 {
		path = fmt.Sprintf("wants?page=%d", page)
	}
	var p Page[WantEntry]
	rl, err := c.do(ctx, http.MethodGet, path, &p)
	return p, rl, err
}

// WantsURL fetches a fully-qualified wants URL (Page.NextPageURL).
func (c *Client) WantsURL(ctx context.Context, fullURL string) (Page[WantEntry], RateLimitInfo, error) {
	var p Page[WantEntry]
	rl, err := c.doAbsolute(ctx, http.MethodGet, fullURL, &p)
	return p, rl, err
}

// Recording fetches detail for a single recording by Encora ID. Works for any
// ID, not just IDs in the user's collection.
func (c *Client) Recording(ctx context.Context, id int64) (Recording, RateLimitInfo, error) {
	var r Recording
	rl, err := c.do(ctx, http.MethodGet, fmt.Sprintf("recording/%d", id), &r)
	return r, rl, err
}

// Subtitles fetches the subtitle list for a recording. Empty array if none.
func (c *Client) Subtitles(ctx context.Context, id int64) ([]Subtitle, RateLimitInfo, error) {
	var subs []Subtitle
	rl, err := c.do(ctx, http.MethodGet, fmt.Sprintf("recording/%d/subtitles", id), &subs)
	return subs, rl, err
}

// Screenshots fetches the screen-grab URLs for a recording. Empty array
// if none. Only call when RecordingMetadata.HasScreenshots is true to
// avoid burning rate-limit budget on guaranteed-empty responses.
func (c *Client) Screenshots(ctx context.Context, id int64) ([]string, RateLimitInfo, error) {
	var urls []string
	rl, err := c.do(ctx, http.MethodGet, fmt.Sprintf("recording/%d/screenshots", id), &urls)
	return urls, rl, err
}

// AddToCollection POSTs to /collection/{id}/collect. The plan run defers
// real exercise of this endpoint; it exists so library ingest can wire
// --add-to-collection against a mock client.
func (c *Client) AddToCollection(ctx context.Context, id int64) (RateLimitInfo, error) {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("collection/%d/collect", id), nil)
}

// UpdateCollectionFormat POSTs to /collection/{id}/format/{format}. The
// format string is URL-PathEscape'd because it lives in the path, not in
// the query string. The pre-escaped path is fed through doAbsolute so the
// already-encoded percent triplets survive into the wire request.
func (c *Client) UpdateCollectionFormat(
	ctx context.Context, id int64, format string,
) (RateLimitInfo, error) {
	u := *c.baseURL
	u.Path = fmt.Sprintf("/api/collection/%d/format/", id)
	return c.doAbsolute(ctx, http.MethodPost, u.String()+url.PathEscape(format), nil)
}

// UpdateCollectionNotes POSTs to /collection/{id}/notes/{notes}. The notes
// string is URL-PathEscape'd because it lives in the path.
func (c *Client) UpdateCollectionNotes(
	ctx context.Context, id int64, notes string,
) (RateLimitInfo, error) {
	u := *c.baseURL
	u.Path = fmt.Sprintf("/api/collection/%d/notes/", id)
	return c.doAbsolute(ctx, http.MethodPost, u.String()+url.PathEscape(notes), nil)
}

// RemoveFromCollection POSTs to /collection/{id}/remove.
func (c *Client) RemoveFromCollection(ctx context.Context, id int64) (RateLimitInfo, error) {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("collection/%d/remove", id), nil)
}

// AddToWants POSTs to /wants/{id}/add.
func (c *Client) AddToWants(ctx context.Context, id int64) (RateLimitInfo, error) {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("wants/%d/add", id), nil)
}

// RemoveFromWants POSTs to /wants/{id}/remove.
func (c *Client) RemoveFromWants(ctx context.Context, id int64) (RateLimitInfo, error) {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("wants/%d/remove", id), nil)
}

// do executes a request and decodes a JSON body. The path is relative to
// /api/. Pass out=nil to discard the response body.
func (c *Client) do(ctx context.Context, method, path string, out any) (RateLimitInfo, error) {
	u := *c.baseURL
	// path may already contain a query string ("collection?page=2"), so
	// split it back out so url.URL.RawQuery is set correctly.
	if base, query, ok := strings.Cut(path, "?"); ok {
		u.Path = "/api/" + base
		u.RawQuery = query
	} else {
		u.Path = "/api/" + path
	}

	return c.doRequest(ctx, method, u.String(), out)
}

// doAbsolute is like do but takes a fully-qualified URL — used to follow
// Laravel-style next_page_url values which are absolute.
func (c *Client) doAbsolute(ctx context.Context, method, fullURL string, out any) (RateLimitInfo, error) {
	return c.doRequest(ctx, method, fullURL, out)
}

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
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		// 200 covers the read endpoints; POST writes (collect, format,
		// notes, remove, wants/add, wants/remove) are observed returning
		// 201 or 204 in production, depending on Encora's mood.
	case http.StatusUnauthorized:
		return rl, ErrUnauthorized
	case http.StatusNotFound:
		return rl, ErrNotFound
	case http.StatusTooManyRequests:
		return rl, ErrRateLimited
	default:
		return rl, fmt.Errorf("encora: unexpected status %d", resp.StatusCode)
	}

	// 204 explicitly carries no body; skip the decoder so callers passing a
	// non-nil out don't get an "unexpected EOF".
	if out != nil && resp.StatusCode != http.StatusNoContent {
		if decErr := json.NewDecoder(resp.Body).Decode(out); decErr != nil {
			return rl, fmt.Errorf("decode body: %w", decErr)
		}
	}
	return rl, nil
}

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
		// RFC 7231 allows HTTP-date or delta-seconds; Encora docs only
		// emit integer seconds, so handle the integer form. Malformed or
		// HTTP-date values leave RetryAfter at zero.
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			rl.RetryAfter = time.Duration(n) * time.Second
		}
	}
	return rl
}
