package tmdb_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/tmdb"
)

// newTestClient spins up an httptest server that serves the supplied
// JSON body for any request, builds a tmdb.Client pointed at it, and
// returns both. Cleanup is registered on t. No real-network calls.
func newTestClient(t *testing.T, handler http.Handler) *tmdb.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := tmdb.New(tmdb.Options{
		BaseURL:   srv.URL,
		ImageBase: srv.URL + "/img",
		APIKey:    "fake-key",
		Logger:    zerolog.Nop(),
	})
	require.NoError(t, err)
	return c
}

func TestNewRejectsBlankAPIKey(t *testing.T) {
	t.Parallel()
	_, err := tmdb.New(tmdb.Options{})
	require.Error(t, err)
}

func TestImages(t *testing.T) {
	t.Parallel()

	const tmdbID int64 = 90181637
	body := `{
	  "posters": [
	    {"file_path": "/posterA.jpg", "width": 1000, "height": 1500, "aspect_ratio": 0.667, "iso_639_1": "en"},
	    {"file_path": "/posterB.jpg", "width": 500, "height": 750}
	  ],
	  "backdrops": [
	    {"file_path": "/back1.jpg", "width": 1920, "height": 1080, "aspect_ratio": 1.78, "iso_639_1": null}
	  ]
	}`

	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/movie/90181637/images", r.URL.Path)
		assert.Equal(t, "fake-key", r.URL.Query().Get("api_key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))

	imgs, err := c.Images(t.Context(), tmdbID)
	require.NoError(t, err)
	require.Len(t, imgs.Posters, 2)
	require.Len(t, imgs.Backdrops, 1)
	// URL is the ImageBase + file_path. Tests verify both prefixes
	// and the per-image metadata round-tripped through the JSON
	// decoder.
	assert.True(t, strings.HasSuffix(imgs.Posters[0].URL, "/img/posterA.jpg"))
	assert.Equal(t, 1000, imgs.Posters[0].Width)
	assert.Equal(t, "en", imgs.Posters[0].Language)
	assert.True(t, strings.HasSuffix(imgs.Backdrops[0].URL, "/img/back1.jpg"))
	assert.InDelta(t, 1.78, imgs.Backdrops[0].AspectRatio, 1e-6)
}

func TestImagesSkipsEmptyFilePath(t *testing.T) {
	t.Parallel()

	body := `{
	  "posters": [
	    {"file_path": "", "width": 1, "height": 2},
	    {"file_path": "/keeper.jpg"}
	  ],
	  "backdrops": []
	}`
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	imgs, err := c.Images(t.Context(), 42)
	require.NoError(t, err)
	require.Len(t, imgs.Posters, 1)
	assert.True(t, strings.HasSuffix(imgs.Posters[0].URL, "/keeper.jpg"))
}

func TestImagesNotFound(t *testing.T) {
	t.Parallel()

	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	_, err := c.Images(t.Context(), 999999)
	require.ErrorIs(t, err, tmdb.ErrNotFound)
}

func TestImagesUnauthorized(t *testing.T) {
	t.Parallel()

	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	_, err := c.Images(t.Context(), 1)
	require.ErrorIs(t, err, tmdb.ErrUnauthorized)
}

func TestImagesUnexpectedStatus(t *testing.T) {
	t.Parallel()

	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	_, err := c.Images(t.Context(), 1)
	require.Error(t, err)
	assert.ErrorIs(t, err, tmdb.ErrUnexpectedStatus,
		"want ErrUnexpectedStatus, got %v", err)
}

func TestFindByIMDBID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       string
		wantID     int64
		wantOK     bool
		wantStatus int
	}{
		{
			name:       "hit",
			body:       `{"movie_results": [{"id": 90181637, "title": "Titanic"}]}`,
			wantID:     90181637,
			wantOK:     true,
			wantStatus: http.StatusOK,
		},
		{
			name:       "empty_results_is_miss",
			body:       `{"movie_results": []}`,
			wantID:     0,
			wantOK:     false,
			wantStatus: http.StatusOK,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/find/tt99999999", r.URL.Path)
				assert.Equal(t, "imdb_id", r.URL.Query().Get("external_source"))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.wantStatus)
				_, _ = w.Write([]byte(tt.body))
			}))
			gotID, gotOK, err := c.FindByIMDBID(t.Context(), "tt99999999")
			require.NoError(t, err)
			assert.Equal(t, tt.wantOK, gotOK)
			assert.Equal(t, tt.wantID, gotID)
		})
	}
}

func TestFindByIMDBIDRejectsBlank(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	_, _, err := c.FindByIMDBID(t.Context(), "")
	require.Error(t, err)
}

func TestImagesRejectsInvalidID(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	_, err := c.Images(t.Context(), 0)
	require.Error(t, err)
}
