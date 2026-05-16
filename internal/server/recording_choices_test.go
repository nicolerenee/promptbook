package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/server"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// pickerTestServer wires a fresh DB + on-disk image cache + minimal
// recording row, then returns the server + db so tests can drive the
// overlay endpoints and read back the persisted choice.
func pickerTestServer(
	t *testing.T,
	recordingID, showID int64,
) (*server.Server, *ent.Client) {
	t.Helper()
	ctx := t.Context()

	sqlDB, db, err := storage.OpenEnt(ctx, filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	require.NoError(t, db.Show.Create().SetID(showID).SetName("PickerShow").Exec(ctx))
	rawJSON, err := json.Marshal(map[string]any{
		"id": recordingID, "show": "PickerShow",
		"metadata": map[string]any{"show_id": showID},
	})
	require.NoError(t, err)
	require.NoError(t, db.Recording.Create().
		SetID(recordingID).SetShowID(showID).SetRawJSON(string(rawJSON)).Exec(ctx))

	cacheRoot := t.TempDir()
	cache := imagecache.New(cacheRoot, nil, zerolog.Nop())

	srv, err := server.New(server.Options{DB: db, ImageCache: cache})
	require.NoError(t, err)
	return srv, db
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
func loadChoice(t *testing.T, db *ent.Client, recordingID int64) storage.ImageChoice {
	t.Helper()
	c, err := storage.GetImageChoice(context.Background(), db, recordingID)
	require.NoError(t, err)
	return c
}

func TestAPISetOverlayTextOverrideAndClear(t *testing.T) {
	t.Parallel()

	const recordingID int64 = 8224
	const showID int64 = 4713
	srv, db := pickerTestServer(t, recordingID, showID)

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
	srv, db := pickerTestServer(t, recordingID, showID)

	// Step 1: flip the flag on.
	status, body := postChoiceJSON(t, srv,
		"/api/v1/recordings/"+strconv.FormatInt(recordingID, 10)+"/overlay-disabled",
		map[string]bool{"disabled": true})
	require.Equal(t, http.StatusOK, status, body)
	assert.Contains(t, body, `"ok":true`)

	choice := loadChoice(t, db, recordingID)
	assert.True(t, choice.OverlayDisabled,
		"overlay-disabled should be persisted true")

	// Step 2: flip back off.
	status, body = postChoiceJSON(t, srv,
		"/api/v1/recordings/"+strconv.FormatInt(recordingID, 10)+"/overlay-disabled",
		map[string]bool{"disabled": false})
	require.Equal(t, http.StatusOK, status, body)
	choice = loadChoice(t, db, recordingID)
	assert.False(t, choice.OverlayDisabled,
		"overlay-disabled should flip back to false")
}

func TestAPIOverlayRequiresImageCache(t *testing.T) {
	t.Parallel()

	sqlDB, db, err := storage.OpenEnt(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	// No ImageCache on Options → 503 on every overlay route.
	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	tests := []struct {
		name string
		path string
		body any
	}{
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

func TestAPIOverlayRecordingNotFound(t *testing.T) {
	t.Parallel()

	sqlDB, db, err := storage.OpenEnt(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	cache := imagecache.New(t.TempDir(), nil, zerolog.Nop())
	srv, err := server.New(server.Options{DB: db, ImageCache: cache})
	require.NoError(t, err)

	status, body := postChoiceJSON(t, srv,
		"/api/v1/recordings/99999/overlay",
		map[string]any{"clear": true})
	assert.Equal(t, http.StatusNotFound, status, body)
}
