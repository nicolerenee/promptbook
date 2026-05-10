package server_test

import (
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
// detail-handler test in this file.
const showFixtureName = "Halcyon Crossing"

// showDetailFixture seeds a minimal shows + recordings setup so the
// show detail handler has data to render.
func showDetailFixture(
	t *testing.T,
	showID int64,
	recordingIDs []int64,
) (*server.Server, *imagecache.Cache) {
	t.Helper()
	ctx := t.Context()

	sqlDB, db, err := storage.OpenEnt(ctx, filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	require.NoError(t, db.Show.Create().SetID(showID).SetName(showFixtureName).Exec(ctx))

	for i, rid := range recordingIDs {
		raw := `{"id":` + strconv.FormatInt(rid, 10) +
			`,"metadata":{"show_id":` + strconv.FormatInt(showID, 10) +
			`,"show_description":"<p>Best show.</p>"}}`
		date := []string{"2017-04-10", "2024-06-12"}[i%2]
		require.NoError(t, db.Recording.Create().
			SetID(rid).SetShowID(showID).SetDateFull(date).SetRawJSON(raw).Exec(ctx))
	}

	cacheRoot := t.TempDir()
	cache := imagecache.New(cacheRoot, nil, zerolog.Nop())

	srv, err := server.New(server.Options{DB: db, ImageCache: cache})
	require.NoError(t, err)
	return srv, cache
}

// TestAPIGetShow verifies the detail JSON includes the show name,
// recording list, year span, and a state_counts breakdown.
func TestAPIGetShow(t *testing.T) {
	t.Parallel()

	const showID int64 = 4711
	srv, _ := showDetailFixture(t, showID, []int64{90100222, 8223})

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

	sqlDB, db, err := storage.OpenEnt(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodGet, "/api/v1/shows/999", nil)
	srv.Handler().ServeHTTP(rr, req)
	assert.Equal(t, http.StatusNotFound, rr.Code, rr.Body.String())
}

// TestAPIGetShowReflectsBanner verifies that when a banner.jpg is on
// disk, the GET response surfaces it via local_banner_url.
func TestAPIGetShowReflectsBanner(t *testing.T) {
	t.Parallel()

	const showID int64 = 4713
	srv, cache := showDetailFixture(t, showID, []int64{9101})

	// Stage a banner.jpg directly — selection is implicit by file
	// existence under v2.
	stageImage(t, cache.ShowBannerPath(showID))

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodGet,
		"/api/v1/shows/"+strconv.FormatInt(showID, 10), nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var got map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	assert.Contains(t, got["local_banner_url"], "/images/shows/")
}
