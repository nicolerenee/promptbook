// Package tmdb is a thin client for the TMDB v3 API.
//
// Auth is api_key query parameter (TMDB's documented v3 auth shape).
// The picker consumes two endpoints: /movie/{tmdb_id}/images for
// posters + backdrops, and /find/{imdb_id}?external_source=imdb_id
// to resolve IMDB → TMDB when only an IMDB id is known. URLs in the
// response are relative paths; the client builds the absolute
// image.tmdb.org/t/p/original CDN URL before returning so callers
// can use the strings directly in <img src>.
package tmdb

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

// defaultHTTPTimeout is the timeout applied when callers don't pass
// an HTTPClient.
const defaultHTTPTimeout = 30 * time.Second

// DefaultBaseURL is the canonical TMDB v3 API root.
const DefaultBaseURL = "https://api.themoviedb.org/3"

// defaultImageBase is the CDN root where TMDB serves the actual
// poster / backdrop bytes. The /original size pulls the
// highest-resolution variant; the picker resizes client-side for
// thumbnails.
const defaultImageBase = "https://image.tmdb.org/t/p/original"

// Errors returned by the client.
var (
	// ErrUnauthorized is returned on 401.
	ErrUnauthorized = errors.New("tmdb: unauthorized")
	// ErrNotFound is returned on 404 — TMDB returns this for unknown
	// movie / external ids.
	ErrNotFound = errors.New("tmdb: not found")
	// ErrUnexpectedStatus is returned on any non-200/401/404 status.
	ErrUnexpectedStatus = errors.New("tmdb: unexpected status")
)

// Client talks to the TMDB API. Construct via New; all fields are
// private.
type Client struct {
	baseURL    *url.URL
	imageBase  string
	apiKey     string
	userAgent  string
	httpClient *http.Client
	logger     zerolog.Logger
}

// Options configures a new Client.
type Options struct {
	// BaseURL overrides the v3 root; empty falls back to
	// DefaultBaseURL. Useful for httptest fixtures.
	BaseURL string
	// ImageBase overrides the CDN root; empty falls back to
	// "https://image.tmdb.org/t/p/original". Tests can point this at
	// the httptest server so the returned URLs hit the same fixture
	// without a real network call.
	ImageBase string
	// APIKey is the v3 api_key. Required.
	APIKey string
	// UserAgent identifies this client in the request header.
	UserAgent string
	// HTTPClient is optional; if nil a default with sane timeouts is
	// used.
	HTTPClient *http.Client
	Logger     zerolog.Logger
}

// New constructs a Client. Returns an error when APIKey is empty —
// the caller is expected to nil-check the no-key case before calling.
func New(opts Options) (*Client, error) {
	if opts.APIKey == "" {
		return nil, errors.New("tmdb: api key is required")
	}
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultBaseURL
	}
	u, err := url.Parse(opts.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base url: %w", err)
	}
	if opts.UserAgent == "" {
		opts.UserAgent = "promptbook/0.0.1"
	}
	if opts.ImageBase == "" {
		opts.ImageBase = defaultImageBase
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &Client{
		baseURL:    u,
		imageBase:  strings.TrimSuffix(opts.ImageBase, "/"),
		apiKey:     opts.APIKey,
		userAgent:  opts.UserAgent,
		httpClient: hc,
		logger:     opts.Logger,
	}, nil
}

// Image describes one poster or backdrop. URL is the absolute CDN
// URL the picker renders as a thumbnail; the remaining fields mirror
// TMDB's API response shape so future filters (by language / aspect
// ratio / pixel density) can land without changing the type.
type Image struct {
	URL         string  `json:"url"`
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	AspectRatio float64 `json:"aspectRatio"`
	Language    string  `json:"language"`
}

// Images is the response body for /movie/{id}/images, with TMDB's
// "posters" + "backdrops" arrays each mapped to []Image.
type Images struct {
	Posters   []Image
	Backdrops []Image
}

// imagesResponse mirrors the wire shape of /movie/{id}/images. Kept
// private — callers use the public Images shape via Client.Images.
type imagesResponse struct {
	Posters   []imageResponse `json:"posters"`
	Backdrops []imageResponse `json:"backdrops"`
}

// imageResponse mirrors one entry in the posters / backdrops array.
// file_path is the relative URL the client prefixes with imageBase.
type imageResponse struct {
	FilePath    string  `json:"file_path"`
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	AspectRatio float64 `json:"aspect_ratio"`
	Language    string  `json:"iso_639_1"`
}

// Images returns the poster + backdrop sets TMDB has for the given
// movie id. Returns ErrNotFound when TMDB has no record of the id;
// the picker treats that as "fall through to the next source"
// rather than a hard error.
func (c *Client) Images(ctx context.Context, tmdbID int64) (Images, error) {
	if tmdbID <= 0 {
		return Images{}, errors.New("tmdb: invalid tmdb id")
	}
	path := "movie/" + strconv.FormatInt(tmdbID, 10) + "/images"
	var resp imagesResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return Images{}, err
	}
	return Images{
		Posters:   c.mapImages(resp.Posters),
		Backdrops: c.mapImages(resp.Backdrops),
	}, nil
}

// mapImages prefixes each relative file_path with the configured
// image base. Skips entries with an empty file_path (TMDB occasionally
// surfaces empties from incomplete metadata; emitting an absolute URL
// pointing at the bare CDN root would be a broken link).
func (c *Client) mapImages(in []imageResponse) []Image {
	out := make([]Image, 0, len(in))
	for _, img := range in {
		if img.FilePath == "" {
			continue
		}
		out = append(out, Image{
			URL:         c.imageBase + img.FilePath,
			Width:       img.Width,
			Height:      img.Height,
			AspectRatio: img.AspectRatio,
			Language:    img.Language,
		})
	}
	return out
}

// findResponse mirrors the wire shape of /find/{external_id}. We
// only care about the movie_results sub-array for the IMDB → TMDB
// resolution.
type findResponse struct {
	MovieResults []struct {
		ID int64 `json:"id"`
	} `json:"movie_results"`
}

// FindByIMDBID resolves an IMDB title id (e.g. "tt99999999") to its
// TMDB numeric movie id. Returns (id, true, nil) on a hit, (0, false,
// nil) when TMDB has no record of the IMDB id, and (0, false, err)
// on a real upstream error. Used by the picker's fallback flow when
// the recording only carries an IMDB id.
func (c *Client) FindByIMDBID(
	ctx context.Context, imdbID string,
) (int64, bool, error) {
	if imdbID == "" {
		return 0, false, errors.New("tmdb: invalid imdb id")
	}
	path := "find/" + imdbID
	var resp findResponse
	if err := c.do(ctx, http.MethodGet, path,
		url.Values{"external_source": []string{"imdb_id"}}, &resp); err != nil {
		return 0, false, err
	}
	if len(resp.MovieResults) == 0 {
		return 0, false, nil
	}
	return resp.MovieResults[0].ID, true, nil
}

// do issues a request and decodes the JSON body into out. extraQuery
// is merged with the auth + per-call params; nil is fine for plain
// GETs.
func (c *Client) do(
	ctx context.Context, method, path string,
	extraQuery url.Values, out any,
) error {
	u := *c.baseURL
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + strings.TrimPrefix(path, "/")
	q := u.Query()
	q.Set("api_key", c.apiKey)
	for k, vs := range extraQuery {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return fmt.Errorf("tmdb: build request: %w", err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("tmdb: http %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		// Fall through to decode.
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusNotFound:
		return ErrNotFound
	default:
		return fmt.Errorf("%w: %d", ErrUnexpectedStatus, resp.StatusCode)
	}

	if out == nil {
		return nil
	}
	if err = json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("tmdb: decode response: %w", err)
	}
	return nil
}
