package server_test

import (
	"context"
	"database/sql"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/imagerender"
	"github.com/nicolerenee/promptbook/internal/server"
	"github.com/nicolerenee/promptbook/internal/storage"
)

const (
	harnessImgWidth  = 640
	harnessImgHeight = 360
)

// rendererHarness builds a real on-disk SQLite + a real imagecache + a
// real renderer. Returns the tuple so each table-test row can drive
// the server with whichever combination it cares about.
func rendererHarness(t *testing.T) (context.Context, *sql.DB, *imagecache.Cache, *imagerender.Renderer) {
	t.Helper()
	ctx := t.Context()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	cache := imagecache.New(t.TempDir(), nil, zerolog.New(io.Discard))
	r := imagerender.New(db, cache, zerolog.New(io.Discard))
	require.NotNil(t, r)
	return ctx, db, cache, r
}

// rawGreenwich BeaconJSON is the raw_json blob for the seeded test recording.
// Pulled out as a const so the long inline string doesn't trip the
// 120-char line lint.
const rawGreenwich BeaconJSON = `{"id":90004242,"show":"Greenwich Beacon","tour":"Broadway",` +
	`"date":{"full_date":"2017-04-21","month_known":true,"day_known":true,"time":"evening"},` +
	`"master":"X","metadata":{"show_id":7}}`

func seedRecordingForRender(ctx context.Context, t *testing.T, db *sql.DB, rid int64) {
	t.Helper()
	_, err := db.ExecContext(ctx,
		`INSERT INTO shows (show_id, name) VALUES (?, ?)`, 7, "Greenwich Beacon")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO recordings (recording_id, show_id, tour, date_full, raw_json)
		VALUES (?, ?, ?, ?, ?)`, rid, 7, "Broadway", "2017-04-21", rawGreenwich BeaconJSON)
	require.NoError(t, err)
}

func writeRenderHarnessBackdrop(t *testing.T, cache *imagecache.Cache, rid int64, idx int) {
	t.Helper()
	dest := cache.BackdropPath(rid, idx)
	require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o750))
	img := image.NewRGBA(image.Rect(0, 0, harnessImgWidth, harnessImgHeight))
	for y := range harnessImgHeight {
		for x := range harnessImgWidth {
			img.Set(x, y, color.RGBA{R: 0x40, G: 0x80, B: 0xC0, A: 0xFF})
		}
	}
	f, err := os.Create(dest)
	require.NoError(t, err)
	require.NoError(t, jpeg.Encode(f, img, &jpeg.Options{Quality: 85}))
	require.NoError(t, f.Close())
}

func TestRegenerateBackdropEndpoint(t *testing.T) {
	t.Parallel()

	t.Run("503_when_renderer_nil", func(t *testing.T) {
		t.Parallel()
		db, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })

		srv, err := server.New(server.Options{DB: db})
		require.NoError(t, err)

		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(
			t.Context(), http.MethodPost, "/api/v1/recordings/1/regenerate-backdrop", nil)
		srv.Handler().ServeHTTP(rr, req)
		assert.Equal(t, http.StatusServiceUnavailable, rr.Code, rr.Body.String())
	})

	t.Run("404_when_recording_missing", func(t *testing.T) {
		t.Parallel()
		_, db, _, renderer := rendererHarness(t)
		srv, err := server.New(server.Options{DB: db, ImageRenderer: renderer})
		require.NoError(t, err)

		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(
			t.Context(), http.MethodPost, "/api/v1/recordings/9999/regenerate-backdrop", nil)
		srv.Handler().ServeHTTP(rr, req)
		assert.Equal(t, http.StatusNotFound, rr.Code, rr.Body.String())
	})

	t.Run("200_renders_when_backdrop_present", func(t *testing.T) {
		t.Parallel()
		ctx, db, cache, renderer := rendererHarness(t)
		const rid int64 = 90004242
		seedRecordingForRender(ctx, t, db, rid)
		writeRenderHarnessBackdrop(t, cache, rid, 0)

		srv, err := server.New(server.Options{DB: db, ImageRenderer: renderer, ImageCache: cache})
		require.NoError(t, err)

		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(
			t.Context(), http.MethodPost, "/api/v1/recordings/90004242/regenerate-backdrop", nil)
		srv.Handler().ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

		rendered := filepath.Join(filepath.Dir(cache.BackdropPath(rid, 0)), "rendered.jpg")
		assert.FileExists(t, rendered)
	})

	t.Run("400_when_id_invalid", func(t *testing.T) {
		t.Parallel()
		_, db, _, renderer := rendererHarness(t)
		srv, err := server.New(server.Options{DB: db, ImageRenderer: renderer})
		require.NoError(t, err)

		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(
			t.Context(), http.MethodPost, "/api/v1/recordings/notanumber/regenerate-backdrop", nil)
		srv.Handler().ServeHTTP(rr, req)
		assert.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
	})
}
