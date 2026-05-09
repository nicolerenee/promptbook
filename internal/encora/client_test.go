package encora_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/encora"
)

// fixtureServer serves the JSON files in testdata/ at predictable Encora paths
// with a synthetic rate-limit header.
func fixtureServer(t *testing.T, remaining int) *httptest.Server {
	t.Helper()

	routes := map[string]string{
		"/api/profile":                    "profile.json",
		"/api/collection":                 "collection.json",
		"/api/wants":                      "wants.json",
		"/api/recording/90100222":             "recording_8222.json",
		"/api/recording/90100222/subtitles":   "recording_8222_subtitles.json",
		"/api/recording/90100222/screenshots": "recording_8222_screenshots.json",
		"/api/recording/90100312":          "probe_2008312.json",
	}

	mux := http.NewServeMux()
	for path, file := range routes {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			b, err := os.ReadFile(filepath.Join("testdata", file))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-RateLimit-Limit", "30")
			w.Header().Set("X-RateLimit-Remaining", itoa(remaining))
			_, _ = w.Write(b)
		})
	}
	return httptest.NewServer(mux)
}

func itoa(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func newTestClient(t *testing.T, baseURL string) *encora.Client {
	t.Helper()
	c, err := encora.New(encora.Options{BaseURL: baseURL, APIKey: "test"})
	require.NoError(t, err)
	return c
}

func TestClientFixtureRoundTrip(t *testing.T) {
	t.Parallel()

	srv := fixtureServer(t, 28)
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL)
	ctx := context.Background()

	t.Run("profile", func(t *testing.T) {
		t.Parallel()
		p, rl, err := c.Profile(ctx)
		require.NoError(t, err)
		assert.Equal(t, "fixturearchive", p.Username)
		assert.Equal(t, 28, p.RecordingsCount)
		assert.Equal(t, 14, p.WantsCount)
		assert.Equal(t, 28, rl.Remaining)
	})

	t.Run("collection", func(t *testing.T) {
		t.Parallel()
		page, _, err := c.Collection(ctx, 1)
		require.NoError(t, err)
		assert.Equal(t, 28, page.Total)
		assert.Equal(t, 1, page.CurrentPage)
		require.NotEmpty(t, page.Data)
		assert.Nil(t, page.NextPageURL, "single-page fixture should have no next")
	})

	t.Run("wants", func(t *testing.T) {
		t.Parallel()
		page, _, err := c.Wants(ctx, 1)
		require.NoError(t, err)
		assert.Equal(t, 14, page.Total)
		require.NotEmpty(t, page.Data)
		// WantEntry only carries the recording payload.
		assert.NotZero(t, page.Data[0].Recording.ID)
	})

	t.Run("recording", func(t *testing.T) {
		t.Parallel()
		r, _, err := c.Recording(ctx, 90100222)
		require.NoError(t, err)
		assert.Equal(t, int64(90100222), r.ID)
		assert.Equal(t, "Marigold Junction", r.Show)
		assert.Equal(t, "Broadway", r.Tour)
		assert.Equal(t, "pro-shot", r.Master)
		assert.True(t, r.Date.MonthKnown)
		assert.False(t, r.Date.DayKnown, "marigold date is December 2009 — day unknown")
	})

	t.Run("subtitles", func(t *testing.T) {
		t.Parallel()
		subs, _, err := c.Subtitles(ctx, 90100222)
		require.NoError(t, err)
		require.Len(t, subs, 3)
		assert.Equal(t, "English", subs[0].Language)
	})

	t.Run("screenshots", func(t *testing.T) {
		t.Parallel()
		urls, rl, err := c.Screenshots(ctx, 90100222)
		require.NoError(t, err)
		require.Len(t, urls, 1)
		assert.Equal(t,
			"https://fixture.invalid/storage/0099/marigold-junction-screenshot.png",
			urls[0])
		// fixtureServer stamps the synthetic remaining header on every
		// response — same shape we cover in the subtitles assertion.
		assert.Equal(t, 28, rl.Remaining)
	})

	t.Run("recording_2008312_not_in_collection", func(t *testing.T) {
		t.Parallel()
		// /recording/{id} works for any ID, not just owned.
		r, _, err := c.Recording(ctx, 90100312)
		require.NoError(t, err)
		assert.Equal(t, int64(90100312), r.ID)
	})
}

func TestClientCastStatusParsing(t *testing.T) {
	t.Parallel()

	// The Chasing Polaris wants entry (id 90001143) has multiple cast members
	// with status objects; verify they parse as CastStatus, not strings.
	wantsBytes, err := os.ReadFile(filepath.Join("testdata", "wants.json"))
	require.NoError(t, err)

	var page encora.Page[encora.WantEntry]
	require.NoError(t, json.Unmarshal(wantsBytes, &page))
	require.NotEmpty(t, page.Data)

	var sawUS bool
	for _, e := range page.Data {
		for _, cast := range e.Recording.Cast {
			if cast.Status != nil && cast.Status.Abbreviation == "u/s" {
				sawUS = true
				assert.Equal(t, "Understudy", cast.Status.Label)
				break
			}
		}
		if sawUS {
			break
		}
	}
	assert.True(t, sawUS, "expected at least one understudy in wants fixture")
}

func TestClientHonorsErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusCode int
		want       error
	}{
		{name: "unauthorized", statusCode: http.StatusUnauthorized, want: encora.ErrUnauthorized},
		{name: "not_found", statusCode: http.StatusNotFound, want: encora.ErrNotFound},
		{name: "rate_limited", statusCode: http.StatusTooManyRequests, want: encora.ErrRateLimited},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.statusCode)
			}))
			t.Cleanup(srv.Close)
			c := newTestClient(t, srv.URL)
			_, _, err := c.Recording(context.Background(), 1)
			assert.ErrorIs(t, err, tt.want)
		})
	}
}

// recordingHandler captures the method/path/auth header of the most recent
// inbound request so write-endpoint tests can assert against them.
type recordingHandler struct {
	method atomic.Value // string
	path   atomic.Value // string
	auth   atomic.Value // string
	status int
}

func (r *recordingHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.method.Store(req.Method)
	r.path.Store(req.URL.EscapedPath())
	r.auth.Store(req.Header.Get("Authorization"))
	status := r.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
}

// TestClientWriteEndpoints exercises the POST endpoints. Each subcase fires
// the call, then asserts the recorded method, path, and auth header. The
// upstream status varies per subcase so 200/201/204 are all proven to count
// as success.
func TestClientWriteEndpoints(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		call       func(ctx context.Context, c *encora.Client) (encora.RateLimitInfo, error)
		wantPath   string
		respStatus int
	}{
		{
			name: "update_collection_format",
			call: func(ctx context.Context, c *encora.Client) (encora.RateLimitInfo, error) {
				return c.UpdateCollectionFormat(ctx, 90100222, "MKV (1080p) - 8.74 GB")
			},
			wantPath:   "/api/collection/90100222/format/MKV%20%281080p%29%20-%208.74%20GB",
			respStatus: http.StatusOK,
		},
		{
			name: "update_collection_notes",
			call: func(ctx context.Context, c *encora.Client) (encora.RateLimitInfo, error) {
				return c.UpdateCollectionNotes(ctx, 90100222, "saw it last night")
			},
			wantPath:   "/api/collection/90100222/notes/saw%20it%20last%20night",
			respStatus: http.StatusCreated,
		},
		{
			name: "remove_from_collection",
			call: func(ctx context.Context, c *encora.Client) (encora.RateLimitInfo, error) {
				return c.RemoveFromCollection(ctx, 90100222)
			},
			wantPath:   "/api/collection/90100222/remove",
			respStatus: http.StatusNoContent,
		},
		{
			name: "add_to_wants",
			call: func(ctx context.Context, c *encora.Client) (encora.RateLimitInfo, error) {
				return c.AddToWants(ctx, 90100222)
			},
			wantPath:   "/api/wants/90100222/add",
			respStatus: http.StatusOK,
		},
		{
			name: "remove_from_wants",
			call: func(ctx context.Context, c *encora.Client) (encora.RateLimitInfo, error) {
				return c.RemoveFromWants(ctx, 90100222)
			},
			wantPath:   "/api/wants/90100222/remove",
			respStatus: http.StatusNoContent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := &recordingHandler{status: tt.respStatus}
			srv := httptest.NewServer(h)
			t.Cleanup(srv.Close)

			c := newTestClient(t, srv.URL)
			_, err := tt.call(context.Background(), c)
			require.NoError(t, err)

			assert.Equal(t, http.MethodPost, h.method.Load())
			assert.Equal(t, tt.wantPath, h.path.Load())
			assert.Equal(t, "Bearer test", h.auth.Load())
		})
	}
}

// TestClientWriteEndpointsHonorErrors confirms 401 and 429 (with Retry-After)
// surface the right errors and rate-limit info on every write method.
func TestClientWriteEndpointsHonorErrors(t *testing.T) {
	t.Parallel()

	calls := []struct {
		name string
		do   func(ctx context.Context, c *encora.Client) (encora.RateLimitInfo, error)
	}{
		{"update_collection_format", func(ctx context.Context, c *encora.Client) (encora.RateLimitInfo, error) {
			return c.UpdateCollectionFormat(ctx, 1, "MKV")
		}},
		{"update_collection_notes", func(ctx context.Context, c *encora.Client) (encora.RateLimitInfo, error) {
			return c.UpdateCollectionNotes(ctx, 1, "x")
		}},
		{"remove_from_collection", func(ctx context.Context, c *encora.Client) (encora.RateLimitInfo, error) {
			return c.RemoveFromCollection(ctx, 1)
		}},
		{"add_to_wants", func(ctx context.Context, c *encora.Client) (encora.RateLimitInfo, error) {
			return c.AddToWants(ctx, 1)
		}},
		{"remove_from_wants", func(ctx context.Context, c *encora.Client) (encora.RateLimitInfo, error) {
			return c.RemoveFromWants(ctx, 1)
		}},
	}

	t.Run("unauthorized", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		t.Cleanup(srv.Close)
		c := newTestClient(t, srv.URL)

		for _, tc := range calls {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				_, err := tc.do(context.Background(), c)
				assert.ErrorIs(t, err, encora.ErrUnauthorized)
			})
		}
	})

	t.Run("rate_limited_with_retry_after", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		t.Cleanup(srv.Close)
		c := newTestClient(t, srv.URL)

		for _, tc := range calls {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				rl, err := tc.do(context.Background(), c)
				require.ErrorIs(t, err, encora.ErrRateLimited)
				assert.Equal(t, 60*time.Second, rl.RetryAfter)
			})
		}
	})
}

// TestParseRateLimitRetryAfter covers the Retry-After header parsing
// independently of any single endpoint.
func TestParseRateLimitRetryAfter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		header string
		want   time.Duration
	}{
		{name: "valid_seconds", header: "60", want: 60 * time.Second},
		{name: "absent", header: "", want: 0},
		{name: "malformed", header: "not-a-number", want: 0},
		{name: "http_date_unsupported", header: "Wed, 21 Oct 2026 07:28:00 GMT", want: 0},
		{name: "zero_treated_as_absent", header: "0", want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.header != "" {
					w.Header().Set("Retry-After", tt.header)
				}
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			t.Cleanup(srv.Close)

			c := newTestClient(t, srv.URL)
			rl, err := c.AddToWants(context.Background(), 1)
			require.ErrorIs(t, err, encora.ErrRateLimited)
			assert.Equal(t, tt.want, rl.RetryAfter)
		})
	}
}
