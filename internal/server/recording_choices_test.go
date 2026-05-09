package server_test

import (
	"bytes"
	"context"
	gosql "database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
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

// pickerTestServer wires a fresh DB + on-disk image cache + minimal
// recording row, then returns the server, db, cache root, and the
// recording id so tests can drive the picker endpoints without
// pulling in the full fixture sync. The cache root is left empty by
// default; tests stage poster/backdrop fixtures under it before
// posting.
func pickerTestServer(
	t *testing.T,
	recordingID, showID int64,
) (*server.Server, *gosql.DB, *imagecache.Cache) {
	t.Helper()
	ctx := t.Context()

	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	// Seed a minimal recording with a raw_json payload that carries the
	// show id — handleSetPoster reads it back via storage.LoadRecording
	// to bounds-check against CountPosters(showID).
	_, err = db.ExecContext(ctx,
		`INSERT INTO shows (show_id, name) VALUES (?, ?)`, showID, "PickerShow")
	require.NoError(t, err)
	rawJSON, err := json.Marshal(map[string]any{
		"id": recordingID, "show": "PickerShow",
		"metadata": map[string]any{"show_id": showID},
	})
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO recordings (recording_id, show_id, tour, date_full, raw_json)
		VALUES (?, ?, '', '', ?)
	`, recordingID, showID, string(rawJSON))
	require.NoError(t, err)

	cacheRoot := t.TempDir()
	cache := imagecache.New(cacheRoot, nil, zerolog.Nop())

	srv, err := server.New(server.Options{DB: db, ImageCache: cache})
	require.NoError(t, err)
	return srv, db, cache
}

// stageImage writes a placeholder file under the cache root at the
// canonical poster/backdrop layout so CountPosters / CountBackdrops
// see it. The bytes are arbitrary — the picker handlers don't open
// the files, they only count them.
func stageImage(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
}

func postChoiceJSON(
	t *testing.T,
	srv *server.Server,
	path string,
	body any,
) (int, string) {
	t.Helper()
	buf, err := json.Marshal(body)
	require.NoError(t, err)
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodPost, path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

// loadChoice is a tiny test-side helper around storage.GetImageChoice
// so tests can read back what they wrote without re-deriving the
// scan boilerplate.
func loadChoice(t *testing.T, db *gosql.DB, recordingID int64) storage.ImageChoice {
	t.Helper()
	c, err := storage.GetImageChoice(context.Background(), db, recordingID)
	require.NoError(t, err)
	return c
}

func TestAPISetPosterPersists(t *testing.T) {
	t.Parallel()

	const recordingID int64 = 90100222
	const showID int64 = 4711
	srv, db, cache := pickerTestServer(t, recordingID, showID)

	// Stage two posters so the bounds-check accepts index 0 and 1.
	stageImage(t, cache.PosterPath(showID, 0))
	stageImage(t, cache.PosterPath(showID, 1))

	status, body := postChoiceJSON(t, srv,
		"/api/v1/recordings/"+strconv.FormatInt(recordingID, 10)+"/poster",
		map[string]int{"index": 1})

	require.Equal(t, http.StatusOK, status, body)
	assert.Contains(t, body, `"ok":true`)

	choice := loadChoice(t, db, recordingID)
	require.NotNil(t, choice.PosterIndex,
		"poster choice should be persisted, not nil")
	assert.Equal(t, 1, *choice.PosterIndex)
}

func TestAPISetBackdropPersists(t *testing.T) {
	t.Parallel()

	const recordingID int64 = 8223
	const showID int64 = 4712
	srv, db, cache := pickerTestServer(t, recordingID, showID)

	// Stage three backdrops keyed on recording id (not show id).
	stageImage(t, cache.BackdropPath(recordingID, 0))
	stageImage(t, cache.BackdropPath(recordingID, 1))
	stageImage(t, cache.BackdropPath(recordingID, 2))

	status, body := postChoiceJSON(t, srv,
		"/api/v1/recordings/"+strconv.FormatInt(recordingID, 10)+"/backdrop",
		map[string]int{"index": 2})

	require.Equal(t, http.StatusOK, status, body)
	assert.Contains(t, body, `"ok":true`)

	choice := loadChoice(t, db, recordingID)
	require.NotNil(t, choice.BackdropIndex)
	assert.Equal(t, 2, *choice.BackdropIndex)
}

func TestAPISetOverlayTextOverrideAndClear(t *testing.T) {
	t.Parallel()

	const recordingID int64 = 8224
	const showID int64 = 4713
	srv, db, cache := pickerTestServer(t, recordingID, showID)

	// Backdrop has to exist for the cache not to be Disabled() —
	// stage one so the renderer call (stub) is exercised.
	stageImage(t, cache.BackdropPath(recordingID, 0))

	// Step 1: persist an explicit override.
	status, body := postChoiceJSON(t, srv,
		"/api/v1/recordings/"+strconv.FormatInt(recordingID, 10)+"/overlay",
		map[string]any{"text": "Cresthaven · OBC · 2016", "clear": false})
	require.Equal(t, http.StatusOK, status, body)
	choice := loadChoice(t, db, recordingID)
	require.NotNil(t, choice.OverlayTextOverride,
		"override should be persisted on save")
	assert.Equal(t, "Cresthaven · OBC · 2016", *choice.OverlayTextOverride)

	// Step 2: clearing nulls the override so the auto-derived label
	// takes over again.
	status, body = postChoiceJSON(t, srv,
		"/api/v1/recordings/"+strconv.FormatInt(recordingID, 10)+"/overlay",
		map[string]any{"text": "", "clear": true})
	require.Equal(t, http.StatusOK, status, body)
	choice = loadChoice(t, db, recordingID)
	assert.Nil(t, choice.OverlayTextOverride,
		"clear=true must null the override column")
}

func TestAPISetOverlayDisabledFlipsAndSurfacesInDetail(t *testing.T) {
	t.Parallel()

	const recordingID int64 = 8230
	const showID int64 = 4720
	srv, db, cache := pickerTestServer(t, recordingID, showID)

	// Stage one backdrop so the cache has something to render against.
	stageImage(t, cache.BackdropPath(recordingID, 0))

	// Step 1: flip the flag on.
	status, body := postChoiceJSON(t, srv,
		"/api/v1/recordings/"+strconv.FormatInt(recordingID, 10)+"/overlay-disabled",
		map[string]bool{"disabled": true})
	require.Equal(t, http.StatusOK, status, body)
	assert.Contains(t, body, `"ok":true`)

	choice := loadChoice(t, db, recordingID)
	assert.True(t, choice.OverlayDisabled,
		"overlay-disabled should be persisted true")

	// GET /recordings/:id should reflect the flag.
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(),
		http.MethodGet,
		"/api/v1/recordings/"+strconv.FormatInt(recordingID, 10), nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var detail map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &detail))
	assert.Equal(t, true, detail["overlay_disabled"],
		"recording detail GET should expose overlay_disabled=true")

	// Step 2: flip back off.
	status, body = postChoiceJSON(t, srv,
		"/api/v1/recordings/"+strconv.FormatInt(recordingID, 10)+"/overlay-disabled",
		map[string]bool{"disabled": false})
	require.Equal(t, http.StatusOK, status, body)
	choice = loadChoice(t, db, recordingID)
	assert.False(t, choice.OverlayDisabled,
		"overlay-disabled should flip back to false")
}

func TestAPISetPosterBoundsCheck(t *testing.T) {
	t.Parallel()

	const recordingID int64 = 8225
	const showID int64 = 4714
	srv, _, cache := pickerTestServer(t, recordingID, showID)

	// One poster on disk → valid range is [0, 1).
	stageImage(t, cache.PosterPath(showID, 0))

	tests := []struct {
		name  string
		index int
	}{
		{name: "negative_index", index: -1},
		{name: "out_of_range", index: 999},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			status, body := postChoiceJSON(t, srv,
				"/api/v1/recordings/"+strconv.FormatInt(recordingID, 10)+"/poster",
				map[string]int{"index": tt.index})
			assert.Equal(t, http.StatusBadRequest, status, body)
			assert.Contains(t, body, "out of range")
		})
	}
}

func TestAPISetBackdropBoundsCheck(t *testing.T) {
	t.Parallel()

	const recordingID int64 = 8226
	const showID int64 = 4715
	srv, _, cache := pickerTestServer(t, recordingID, showID)

	// Two backdrops on disk → valid range is [0, 2).
	stageImage(t, cache.BackdropPath(recordingID, 0))
	stageImage(t, cache.BackdropPath(recordingID, 1))

	status, body := postChoiceJSON(t, srv,
		"/api/v1/recordings/"+strconv.FormatInt(recordingID, 10)+"/backdrop",
		map[string]int{"index": 5})
	assert.Equal(t, http.StatusBadRequest, status, body)
	assert.Contains(t, body, "out of range")
}

func TestAPIPickerRequiresImageCache(t *testing.T) {
	t.Parallel()

	db, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	// No ImageCache on Options → 503 on every picker route.
	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	tests := []struct {
		name string
		path string
		body any
	}{
		{name: "poster", path: "/api/v1/recordings/1/poster", body: map[string]int{"index": 0}},
		{name: "backdrop", path: "/api/v1/recordings/1/backdrop", body: map[string]int{"index": 0}},
		{name: "overlay", path: "/api/v1/recordings/1/overlay", body: map[string]any{"clear": true}},
		{
			name: "overlay_disabled",
			path: "/api/v1/recordings/1/overlay-disabled",
			body: map[string]bool{"disabled": true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			status, body := postChoiceJSON(t, srv, tt.path, tt.body)
			assert.Equal(t, http.StatusServiceUnavailable, status, body)
		})
	}
}

func TestAPIPickerRecordingNotFound(t *testing.T) {
	t.Parallel()

	db, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	cache := imagecache.New(t.TempDir(), nil, zerolog.Nop())
	srv, err := server.New(server.Options{DB: db, ImageCache: cache})
	require.NoError(t, err)

	status, body := postChoiceJSON(t, srv,
		"/api/v1/recordings/99999/poster",
		map[string]int{"index": 0})
	assert.Equal(t, http.StatusNotFound, status, body)
}
