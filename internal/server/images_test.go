package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/server"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// imagesRouteServer wires a fresh DB + on-disk imagecache + a minimal
// performer / show / recording row so the image route tests can hit
// real-looking IDs without seeding the full Encora fixture set.
//
//nolint:unparam // db return is symmetric with sibling helpers.
func imagesRouteServer(t *testing.T) (*server.Server, *ent.Client, *imagecache.Cache) {
	t.Helper()
	ctx := t.Context()

	sqlDB, db, err := storage.OpenEnt(ctx, filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	// Performer 90001001 — Avery Morrison.
	require.NoError(t, storage.UpsertPerformer(ctx, db, storage.Performer{
		PerformerID: 90001001,
		Name:        "Avery Morrison",
	}))
	// Show 90004089 — Cresthaven.
	require.NoError(t, db.Show.Create().SetID(90004089).SetName("Cresthaven").Exec(ctx))
	// Recording 90100222 — minimal raw_json so LoadRecording succeeds.
	rawJSON, err := json.Marshal(map[string]any{
		"id":   90100222,
		"show": "Marigold",
		"tour": "OBC",
		"date": map[string]any{
			"full_date":   "2009-12-13",
			"month_known": true,
			"day_known":   true,
		},
	})
	require.NoError(t, err)
	require.NoError(t, db.Recording.Create().
		SetID(90100222).SetShowID(90004089).SetTour("OBC").
		SetDateFull("2009-12-13").SetRawJSON(string(rawJSON)).Exec(ctx))

	cache := imagecache.New(t.TempDir(), nil, zerolog.Nop())
	srv, err := server.New(server.Options{DB: db, ImageCache: cache})
	require.NoError(t, err)
	return srv, db, cache
}

// writeJPEG drops a tiny synthetic JPEG at path, MkdirAll'ing the
// parent directory. Used by the disk-hit test so /images/* finds a
// real file to serve instead of falling through to a placeholder.
func writeJPEG(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := range 4 {
		for x := range 4 {
			img.Set(x, y, color.RGBA{R: 0xff, A: 0xff})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, img, nil))
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600))
}

func TestImagesRoute_DiskHit(t *testing.T) {
	t.Parallel()

	srv, _, cache := imagesRouteServer(t)
	writeJPEG(t, cache.HeadshotPath(90001001))

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(),
		http.MethodGet, "/images/actors/90001001.jpg", nil)
	srv.Handler().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	ct := rr.Header().Get("Content-Type")
	assert.True(t, strings.HasPrefix(ct, "image/jpeg"),
		"on a cache hit we should serve the on-disk JPEG (got %q)", ct)
}

func TestImagesRoute_PlaceholderOnMiss_Headshot(t *testing.T) {
	t.Parallel()

	srv, _, _ := imagesRouteServer(t)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(),
		http.MethodGet, "/images/actors/90001001.jpg", nil)
	srv.Handler().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, "image/svg+xml; charset=utf-8", rr.Header().Get("Content-Type"))
	body := rr.Body.String()
	assert.True(t, strings.HasPrefix(body, "<svg"), "placeholder must be SVG")
	// Initials BD = "Brian" + "d'Arcy" — matches the performer row seeded above.
	assert.Contains(t, body, ">BD<")
}

func TestImagesRoute_PlaceholderOnMiss_ShowBanner(t *testing.T) {
	t.Parallel()

	srv, _, _ := imagesRouteServer(t)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(),
		http.MethodGet, "/images/shows/90004089/banner.jpg", nil)
	srv.Handler().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, "image/svg+xml; charset=utf-8", rr.Header().Get("Content-Type"))
	assert.Contains(t, rr.Body.String(), ">Cresthaven<")
}

// TestImagesRoute_FanartMissReturns404 confirms that fanart is the one
// slot that does NOT fall through to a generated SVG placeholder. The
// picker UI keys off the empty body / 404 to render
// "No image on disk yet" instead of pretending fanart exists. Posters
// + headshots + show banners keep the placeholder fallback.
func TestImagesRoute_FanartMissReturns404(t *testing.T) {
	t.Parallel()

	srv, _, _ := imagesRouteServer(t)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(),
		http.MethodGet, "/images/recordings/90100222/fanart.jpg", nil)
	srv.Handler().ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code, rr.Body.String())
}

func TestImagesRoute_UnknownEntityStillRenders(t *testing.T) {
	t.Parallel()

	srv, _, _ := imagesRouteServer(t)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(),
		http.MethodGet, "/images/actors/99999.jpg", nil)
	srv.Handler().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	body := rr.Body.String()
	assert.True(t, strings.HasPrefix(body, "<svg"))
	// Unknown performer falls back to "Unknown #99999" → initials "U"
	// (then a digit-led second word, which initialsFor skips).
	assert.Contains(t, body, "Unknown #99999")
}

func TestImagesRoute_UnknownPath_404(t *testing.T) {
	t.Parallel()

	srv, _, _ := imagesRouteServer(t)

	tests := []struct {
		name string
		path string
	}{
		{name: "garbage", path: "/images/garbage"},
		{name: "wrong_subdir", path: "/images/foo/123.jpg"},
		{name: "non_numeric_id", path: "/images/actors/abc.jpg"},
		{name: "wrong_filename", path: "/images/shows/90004089/wrong.jpg"},
		{name: "traversal", path: "/images/../etc/passwd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rr := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(context.Background(),
				http.MethodGet, tt.path, nil)
			srv.Handler().ServeHTTP(rr, req)
			assert.Equal(t, http.StatusNotFound, rr.Code,
				"unknown path %s should 404, body=%s", tt.path, rr.Body.String())
		})
	}
}

func TestImagesRoute_DisabledCache_404(t *testing.T) {
	t.Parallel()

	// No ImageCache configured at all → /images/* isn't even
	// registered, so the SPA fallback handles the path. We just need
	// to verify it does NOT return a placeholder image.
	sqlDB, db, err := storage.OpenEnt(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(),
		http.MethodGet, "/images/actors/1.jpg", nil)
	srv.Handler().ServeHTTP(rr, req)

	// Without an ImageCache the route is unregistered, so the SPA
	// fallback (or echo's default 404) handles it. Either way, the
	// response should NOT be image/svg+xml.
	assert.NotEqual(t, "image/svg+xml; charset=utf-8",
		rr.Header().Get("Content-Type"),
		"no placeholder should be served when imagecache is disabled")
}
