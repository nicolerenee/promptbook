package server_test

import (
	"bytes"
	"context"
	gosql "database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/server"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// showFixtureName is the canonical synthetic show name used by every
// detail-handler test in this file. Hard-coded because every test
// only needs a single show and the value isn't load-bearing on any
// assertion (assertions key off the JSON shape, not the literal name).
const showFixtureName = "Halcyon Crossing"

// showDetailFixture seeds a minimal shows + recordings setup so the
// show detail handler has data to render. Returns the wired server,
// the DB handle (for direct assertions), and the cache handle (so
// tests can stage poster files).
func showDetailFixture(
	t *testing.T,
	showID int64,
	recordingIDs []int64,
) (*server.Server, *gosql.DB, *imagecache.Cache) {
	t.Helper()
	ctx := t.Context()

	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.ExecContext(ctx,
		`INSERT INTO shows (show_id, name) VALUES (?, ?)`, showID, showFixtureName)
	require.NoError(t, err)

	for i, rid := range recordingIDs {
		// Pick increasing date_full so yearSpan has something to chew
		// on; the first row carries a non-empty show_description so
		// the handler's description query has a hit.
		raw := `{"id":` + strconv.FormatInt(rid, 10) +
			`,"metadata":{"show_id":` + strconv.FormatInt(showID, 10) +
			`,"show_description":"<p>Best show.</p>"}}`
		date := []string{"2017-04-10", "2024-06-12"}[i%2]
		_, err = db.ExecContext(ctx, `
			INSERT INTO recordings (recording_id, show_id, tour, date_full, raw_json)
			VALUES (?, ?, '', ?, ?)
		`, rid, showID, date, raw)
		require.NoError(t, err)
	}

	cacheRoot := t.TempDir()
	cache := imagecache.New(cacheRoot, nil, zerolog.Nop())

	srv, err := server.New(server.Options{DB: db, ImageCache: cache})
	require.NoError(t, err)
	return srv, db, cache
}

// TestAPIGetShow verifies the detail JSON includes the show name,
// recording list, year span, and a state_counts breakdown.
func TestAPIGetShow(t *testing.T) {
	t.Parallel()

	const showID int64 = 4711
	srv, _, _ := showDetailFixture(t, showID, []int64{90100222, 8223})

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodGet,
		"/api/v1/shows/"+strconv.FormatInt(showID, 10), nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var got map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	assert.Equal(t, "Halcyon Crossing", got["name"])
	assert.InEpsilon(t, float64(2), got["recording_count"], 0.0001)
	assert.InEpsilon(t, float64(2017), got["first_year"], 0.0001)
	assert.InEpsilon(t, float64(2024), got["last_year"], 0.0001)
	assert.Equal(t, "Best show.", got["description"])

	recs, _ := got["recordings"].([]any)
	assert.Len(t, recs, 2)

	stateCounts, _ := got["state_counts"].(map[string]any)
	assert.NotNil(t, stateCounts)
}

// TestAPIGetShowNotFound returns 404 when no shows row matches.
func TestAPIGetShowNotFound(t *testing.T) {
	t.Parallel()

	db, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodGet, "/api/v1/shows/999", nil)
	srv.Handler().ServeHTTP(rr, req)
	assert.Equal(t, http.StatusNotFound, rr.Code, rr.Body.String())
}

// TestAPISetShowPosterPersists exercises the picker POST: a new
// show_image_choices row should land with the requested index.
func TestAPISetShowPosterPersists(t *testing.T) {
	t.Parallel()

	const showID int64 = 4712
	srv, db, cache := showDetailFixture(t, showID, []int64{9001})

	stageImage(t, cache.PosterPath(showID, 0))
	stageImage(t, cache.PosterPath(showID, 1))

	body, err := json.Marshal(map[string]int{"index": 1})
	require.NoError(t, err)
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodPost,
		"/api/v1/shows/"+strconv.FormatInt(showID, 10)+"/poster",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	choice, err := storage.GetShowImageChoice(context.Background(), db, showID)
	require.NoError(t, err)
	require.NotNil(t, choice.PosterIndex)
	assert.Equal(t, 1, *choice.PosterIndex)
}

// TestAPIGetShowReflectsSelectedPoster ensures the selected_poster_index
// surfaced by GET /shows/:id round-trips after a POST persists a pick.
func TestAPIGetShowReflectsSelectedPoster(t *testing.T) {
	t.Parallel()

	const showID int64 = 4713
	srv, _, cache := showDetailFixture(t, showID, []int64{9101})
	stageImage(t, cache.PosterPath(showID, 0))
	stageImage(t, cache.PosterPath(showID, 1))

	postBody, err := json.Marshal(map[string]int{"index": 1})
	require.NoError(t, err)
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodPost,
		"/api/v1/shows/"+strconv.FormatInt(showID, 10)+"/poster",
		bytes.NewReader(postBody))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	rr = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(
		t.Context(), http.MethodGet,
		"/api/v1/shows/"+strconv.FormatInt(showID, 10), nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var got map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	assert.InEpsilon(t, float64(1), got["selected_poster_index"], 0.0001)

	urls, _ := got["local_poster_urls"].([]any)
	assert.Len(t, urls, 2, "both staged posters should round-trip")
}

// TestAPISetShowPosterBoundsCheck rejects out-of-range indexes.
func TestAPISetShowPosterBoundsCheck(t *testing.T) {
	t.Parallel()

	const showID int64 = 4714
	srv, _, cache := showDetailFixture(t, showID, []int64{9201})
	stageImage(t, cache.PosterPath(showID, 0))

	tests := []struct {
		name  string
		index int
	}{
		{name: "negative", index: -1},
		{name: "too_large", index: 99},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body, err := json.Marshal(map[string]int{"index": tt.index})
			require.NoError(t, err)
			rr := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(
				t.Context(), http.MethodPost,
				"/api/v1/shows/"+strconv.FormatInt(showID, 10)+"/poster",
				bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			srv.Handler().ServeHTTP(rr, req)
			assert.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
			assert.Contains(t, rr.Body.String(), "out of range")
		})
	}
}

// TestAPISetShowPosterRequiresImageCache returns 503 when the cache
// isn't configured.
func TestAPISetShowPosterRequiresImageCache(t *testing.T) {
	t.Parallel()

	db, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.ExecContext(t.Context(),
		`INSERT INTO shows (show_id, name) VALUES (?, ?)`, int64(7), "X")
	require.NoError(t, err)

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	body, err := json.Marshal(map[string]int{"index": 0})
	require.NoError(t, err)
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodPost,
		"/api/v1/shows/7/poster", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req)
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
}
