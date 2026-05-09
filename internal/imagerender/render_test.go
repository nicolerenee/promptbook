package imagerender_test

import (
	"bytes"
	"context"
	"database/sql"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/imagerender"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// imgWidth / imgHeight are the synthetic backdrop dimensions used by
// every test that writes a fake JPEG. 1280x720 mirrors the canonical
// Encora screen-grab aspect.
const (
	imgWidth  = 1280
	imgHeight = 720
)

// setup builds a fresh on-disk SQLite + a tmp imagecache + a real
// renderer, returns the trio, and seeds a show + recording row so
// LoadRecording succeeds. Callers that don't want the recording row
// can pass seed=false.
func setup(t *testing.T, seed bool) (
	context.Context, *sql.DB, *imagecache.Cache, *imagerender.Renderer, int64,
) {
	t.Helper()
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "promptbook.db")
	db, err := storage.Open(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	cacheRoot := t.TempDir()
	cache := imagecache.New(cacheRoot, nil, zerolog.New(io.Discard))
	r := imagerender.New(db, cache, zerolog.New(io.Discard))
	require.NotNil(t, r, "renderer should be non-nil when cache root is set")

	const recordingID int64 = 9001
	if seed {
		_, execErr := db.ExecContext(ctx,
			`INSERT INTO shows (show_id, name) VALUES (?, ?)`, 7, "Greenwich Beacon")
		require.NoError(t, execErr)
		// raw_json carries the encora.Recording fields the renderer reads
		// for the auto-derived overlay text.
		const raw = `{
			"id": 9001, "show": "Greenwich Beacon", "tour": "Broadway",
			"date": {"full_date": "2017-04-21", "month_known": true, "day_known": true, "time": "evening"},
			"master": "SampleMaster", "metadata": {"show_id": 7}
		}`
		_, execErr = db.ExecContext(ctx, `
			INSERT INTO recordings (recording_id, show_id, tour, date_full, raw_json)
			VALUES (?, ?, ?, ?, ?)`,
			recordingID, 7, "Broadway", "2017-04-21", raw)
		require.NoError(t, execErr)
	}
	return ctx, db, cache, r, recordingID
}

// writeSyntheticBackdrop lays a solid-color JPEG into the cache at the
// canonical path for the recording. Always writes to backdrop index 0
// — every test only needs the canonical default slot.
func writeSyntheticBackdrop(t *testing.T, cache *imagecache.Cache, recordingID int64) {
	t.Helper()
	dest := cache.BackdropPath(recordingID, 0)
	require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o750))
	img := image.NewRGBA(image.Rect(0, 0, imgWidth, imgHeight))
	// Fill with a recognizable mid-blue so a human eyeballing the file
	// sees it's the synthetic, not real footage.
	for y := range imgHeight {
		for x := range imgWidth {
			img.Set(x, y, color.RGBA{R: 0x33, G: 0x66, B: 0x99, A: 0xFF})
		}
	}
	f, err := os.Create(dest)
	require.NoError(t, err)
	require.NoError(t, jpeg.Encode(f, img, &jpeg.Options{Quality: 85}))
	require.NoError(t, f.Close())
}

func TestRegenerate_NoBackdrop(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		seed bool
	}{
		{name: "no_recording_no_backdrop", seed: false},
		{name: "recording_seeded_but_no_backdrop", seed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, _, cache, r, rid := setup(t, tt.seed)
			err := r.Regenerate(ctx, rid)
			require.NoError(t, err, "no-backdrop case should be a soft no-op")
			// No rendered.jpg should appear when there was no input.
			rendered := filepath.Join(filepath.Dir(cache.BackdropPath(rid, 0)), "rendered.jpg")
			_, statErr := os.Stat(rendered)
			assert.ErrorIs(t, statErr, os.ErrNotExist, "rendered.jpg should not exist")
		})
	}
}

func TestRegenerate_NilRendererIsNoop(t *testing.T) {
	t.Parallel()
	var r *imagerender.Renderer
	require.NoError(t, r.Regenerate(t.Context(), 42))
}

func TestRegenerate_DisabledCacheReturnsNilRenderer(t *testing.T) {
	t.Parallel()
	c := imagecache.New("", nil, zerolog.New(io.Discard))
	r := imagerender.New(nil, c, zerolog.New(io.Discard))
	assert.Nil(t, r, "disabled cache should yield a nil Renderer")
}

func TestRegenerate_ProducesFile(t *testing.T) {
	t.Parallel()
	ctx, db, cache, r, rid := setup(t, true)
	writeSyntheticBackdrop(t, cache, rid)
	require.NoError(t, storage.SetBackdropIndex(ctx, db, rid, 0))
	require.NoError(t, storage.SetOverlayTextOverride(ctx, db, rid,
		"Greenwich Beacon\nBroadway · 2017-04-21 · SampleMaster"))

	require.NoError(t, r.Regenerate(ctx, rid))

	rendered := filepath.Join(filepath.Dir(cache.BackdropPath(rid, 0)), "rendered.jpg")
	assertJPEGDimensions(t, rendered)
}

func TestRegenerate_AutoDerivedText(t *testing.T) {
	t.Parallel()
	ctx, _, cache, r, rid := setup(t, true)
	writeSyntheticBackdrop(t, cache, rid)
	// Don't set overrides — the renderer should derive text from the
	// recording row.
	require.NoError(t, r.Regenerate(ctx, rid))

	rendered := filepath.Join(filepath.Dir(cache.BackdropPath(rid, 0)), "rendered.jpg")
	assertJPEGDimensions(t, rendered)
}

func TestRegenerate_BadStyleJSON(t *testing.T) {
	t.Parallel()
	ctx, db, cache, r, rid := setup(t, true)
	writeSyntheticBackdrop(t, cache, rid)
	require.NoError(t, storage.SetOverlayStyle(ctx, db, rid, "{not valid json"))
	require.NoError(t, storage.SetOverlayTextOverride(ctx, db, rid, "TITLE · SUBTITLE"))

	// Bad style JSON is logged at warn but never fails the render — the
	// default style takes over.
	require.NoError(t, r.Regenerate(ctx, rid))

	rendered := filepath.Join(filepath.Dir(cache.BackdropPath(rid, 0)), "rendered.jpg")
	assertJPEGDimensions(t, rendered)
}

func TestRegenerate_GoodStyleJSON(t *testing.T) {
	t.Parallel()
	ctx, db, cache, r, rid := setup(t, true)
	writeSyntheticBackdrop(t, cache, rid)
	require.NoError(t, storage.SetOverlayStyle(ctx, db, rid,
		`{"band_color":"#000000","text_color":"#ff0000","band_height_fraction":0.2}`))
	require.NoError(t, storage.SetOverlayTextOverride(ctx, db, rid, "RED ON BLACK"))

	require.NoError(t, r.Regenerate(ctx, rid))

	rendered := filepath.Join(filepath.Dir(cache.BackdropPath(rid, 0)), "rendered.jpg")
	assertJPEGDimensions(t, rendered)
}

func TestRegenerate_AtomicWrite(t *testing.T) {
	t.Parallel()
	ctx, _, cache, r, rid := setup(t, true)
	writeSyntheticBackdrop(t, cache, rid)

	require.NoError(t, r.Regenerate(ctx, rid))

	dir := filepath.Dir(cache.BackdropPath(rid, 0))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".tmp", "no .tmp leftover should remain")
	}
	// rendered.jpg exists; the raw 0.jpg also still sits next to it.
	assert.FileExists(t, filepath.Join(dir, "rendered.jpg"))
	assert.FileExists(t, filepath.Join(dir, "0"+".jpg"))
}

func TestRegenerate_LongShowNameFits(t *testing.T) {
	t.Parallel()
	ctx, db, cache, r, rid := setup(t, true)
	writeSyntheticBackdrop(t, cache, rid)
	// A pathologically long show name should shrink to fit; the test
	// just asserts the render completes + produces a valid JPEG.
	require.NoError(t, storage.SetOverlayTextOverride(ctx, db, rid,
		"A VERY VERY VERY VERY VERY LONG BROADWAY MUSICAL TITLE THAT WOULD NORMALLY OVERFLOW"))

	require.NoError(t, r.Regenerate(ctx, rid))
	rendered := filepath.Join(filepath.Dir(cache.BackdropPath(rid, 0)), "rendered.jpg")
	assertJPEGDimensions(t, rendered)
}

// assertJPEGDimensions decodes the file and asserts the rendered
// image matches the synthetic source's imgWidth × imgHeight. Pixel-
// level golden comparisons are deliberately skipped — they're too
// brittle for compositing code that may legitimately tweak its
// glyph rendering between font versions.
func assertJPEGDimensions(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	img, err := jpeg.Decode(bytes.NewReader(b))
	require.NoError(t, err, "rendered.jpg must decode as JPEG")
	bounds := img.Bounds()
	assert.Equal(t, imgWidth, bounds.Dx(), "width should match source")
	assert.Equal(t, imgHeight, bounds.Dy(), "height should match source")
}
