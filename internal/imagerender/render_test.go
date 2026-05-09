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

// imgWidth / imgHeight are the synthetic poster dimensions used by
// every test that writes a fake JPEG. 800x1200 is a 2:3 vertical
// poster aspect.
const (
	imgWidth  = 800
	imgHeight = 1200
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

// writeSyntheticPosterSrc lays a solid-color JPEG into the cache at the
// canonical poster-src path for the recording.
func writeSyntheticPosterSrc(t *testing.T, cache *imagecache.Cache, recordingID int64) {
	t.Helper()
	dest := cache.RecordingPosterSrcPath(recordingID)
	require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o750))
	img := image.NewRGBA(image.Rect(0, 0, imgWidth, imgHeight))
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

func TestRegenerate_NoPosterSrc(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		seed bool
	}{
		{name: "no_recording_no_poster", seed: false},
		{name: "recording_seeded_but_no_poster", seed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, _, cache, r, rid := setup(t, tt.seed)
			err := r.Regenerate(ctx, rid)
			require.NoError(t, err, "no-poster-src case should be a soft no-op")
			// No poster.jpg should appear when there was no input.
			assert.False(t, cache.HasRecordingPoster(rid),
				"poster.jpg should not exist with no source")
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
	writeSyntheticPosterSrc(t, cache, rid)
	require.NoError(t, storage.SetOverlayTextOverride(ctx, db, rid,
		"Greenwich Beacon\nBroadway · 2017-04-21 · SampleMaster"))

	require.NoError(t, r.Regenerate(ctx, rid))
	assertJPEGDimensions(t, cache.RecordingPosterPath(rid))
}

func TestRegenerate_AutoDerivedText(t *testing.T) {
	t.Parallel()
	ctx, _, cache, r, rid := setup(t, true)
	writeSyntheticPosterSrc(t, cache, rid)
	// Don't set overrides — the renderer should derive text from the
	// recording row.
	require.NoError(t, r.Regenerate(ctx, rid))
	assertJPEGDimensions(t, cache.RecordingPosterPath(rid))
}

func TestRegenerate_BadStyleJSON(t *testing.T) {
	t.Parallel()
	ctx, db, cache, r, rid := setup(t, true)
	writeSyntheticPosterSrc(t, cache, rid)
	require.NoError(t, storage.SetOverlayStyle(ctx, db, rid, "{not valid json"))
	require.NoError(t, storage.SetOverlayTextOverride(ctx, db, rid, "TITLE · SUBTITLE"))

	// Bad style JSON is logged at warn but never fails the render — the
	// default style takes over.
	require.NoError(t, r.Regenerate(ctx, rid))
	assertJPEGDimensions(t, cache.RecordingPosterPath(rid))
}

func TestRegenerate_GoodStyleJSON(t *testing.T) {
	t.Parallel()
	ctx, db, cache, r, rid := setup(t, true)
	writeSyntheticPosterSrc(t, cache, rid)
	require.NoError(t, storage.SetOverlayStyle(ctx, db, rid,
		`{"band_color":"#000000","text_color":"#ff0000","band_height_fraction":0.2}`))
	require.NoError(t, storage.SetOverlayTextOverride(ctx, db, rid, "RED ON BLACK"))

	require.NoError(t, r.Regenerate(ctx, rid))
	assertJPEGDimensions(t, cache.RecordingPosterPath(rid))
}

func TestRegenerate_AtomicWrite(t *testing.T) {
	t.Parallel()
	ctx, _, cache, r, rid := setup(t, true)
	writeSyntheticPosterSrc(t, cache, rid)

	require.NoError(t, r.Regenerate(ctx, rid))

	dir := filepath.Dir(cache.RecordingPosterPath(rid))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".tmp", "no .tmp leftover should remain")
	}
	// poster.jpg exists; poster-src.jpg also still sits next to it.
	assert.FileExists(t, cache.RecordingPosterPath(rid))
	assert.FileExists(t, cache.RecordingPosterSrcPath(rid))
}

func TestRegenerate_OverlayDisabled(t *testing.T) {
	t.Parallel()
	ctx, db, cache, r, rid := setup(t, true)
	writeSyntheticPosterSrc(t, cache, rid)
	require.NoError(t, storage.SetOverlayTextOverride(ctx, db, rid,
		"Greenwich Beacon\nBroadway · 2017-04-21 · SampleMaster"))
	require.NoError(t, storage.SetOverlayDisabled(ctx, db, rid, true))

	require.NoError(t, r.Regenerate(ctx, rid))

	srcBytes, err := os.ReadFile(cache.RecordingPosterSrcPath(rid))
	require.NoError(t, err)
	posterBytes, err := os.ReadFile(cache.RecordingPosterPath(rid))
	require.NoError(t, err)
	assert.Equal(t, srcBytes, posterBytes,
		"overlay-disabled poster.jpg must be a byte-identical copy of poster-src.jpg")

	// Flipping the flag back off rebuilds the burned-in composite.
	require.NoError(t, storage.SetOverlayDisabled(ctx, db, rid, false))
	require.NoError(t, r.Regenerate(ctx, rid))
	posterBytes, err = os.ReadFile(cache.RecordingPosterPath(rid))
	require.NoError(t, err)
	assert.NotEqual(t, srcBytes, posterBytes,
		"after re-enabling overlay, poster.jpg should be the composite again")
}

func TestRegenerate_LongShowNameFits(t *testing.T) {
	t.Parallel()
	ctx, db, cache, r, rid := setup(t, true)
	writeSyntheticPosterSrc(t, cache, rid)
	require.NoError(t, storage.SetOverlayTextOverride(ctx, db, rid,
		"A VERY VERY VERY VERY VERY LONG BROADWAY MUSICAL TITLE THAT WOULD NORMALLY OVERFLOW"))

	require.NoError(t, r.Regenerate(ctx, rid))
	assertJPEGDimensions(t, cache.RecordingPosterPath(rid))
}

// assertJPEGDimensions decodes the file and asserts the rendered image
// matches the synthetic source's imgWidth × imgHeight.
func assertJPEGDimensions(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	img, err := jpeg.Decode(bytes.NewReader(b))
	require.NoError(t, err, "poster.jpg must decode as JPEG")
	bounds := img.Bounds()
	assert.Equal(t, imgWidth, bounds.Dx(), "width should match source")
	assert.Equal(t, imgHeight, bounds.Dy(), "height should match source")
}
