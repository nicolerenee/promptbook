package encora_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/encora"
)

// fixtureServer serves the JSON files in testdata/ at predictable Encora paths
// with a synthetic rate-limit header.
func fixtureServer(t *testing.T, remaining int) *httptest.Server {
	t.Helper()

	routes := map[string]string{
		"/api/profile":                  "profile.json",
		"/api/collection":               "collection.json",
		"/api/wants":                    "wants.json",
		"/api/recording/90100222":           "recording_8222.json",
		"/api/recording/90100222/subtitles": "recording_8222_subtitles.json",
		"/api/recording/90100312":        "probe_2008312.json",
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
