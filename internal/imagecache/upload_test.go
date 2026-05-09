package imagecache_test

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/imagecache"
)

// makeJPEG returns a tiny valid JPEG byte slice. Used as the synthetic
// upload payload in the SaveUploaded* tests so we don't need a fixture
// file on disk.
func makeJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for x := range 4 {
		for y := range 4 {
			img.Set(x, y, color.RGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}))
	return buf.Bytes()
}

// makePNG returns a tiny valid PNG byte slice. Exercises the
// re-encode path — uploads that arrive as PNG must be saved back as
// JPEG so the on-disk shape is uniform.
func makePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for x := range 4 {
		for y := range 4 {
			img.Set(x, y, color.RGBA{R: 10, G: 200, B: 30, A: 255})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

func TestNextUploadPosterIndex_Empty(t *testing.T) {
	t.Parallel()
	c := imagecache.New(t.TempDir(), nil, zerologTest(t))
	idx, err := c.NextUploadPosterIndex(42)
	require.NoError(t, err)
	assert.Equal(t, imagecache.UploadIndexFloor, idx,
		"first upload lands on the reserved floor")
}

func TestNextUploadPosterIndex_SkipsExisting(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	c := imagecache.New(root, nil, zerologTest(t))

	// Pre-stage two uploads at the floor so the next pick is +2.
	for _, i := range []int{
		imagecache.UploadIndexFloor,
		imagecache.UploadIndexFloor + 1,
	} {
		require.NoError(t, os.MkdirAll(
			c.PosterPath(7, i)[:len(c.PosterPath(7, i))-len("/100.jpg")], 0o750))
		require.NoError(t, os.WriteFile(c.PosterPath(7, i), []byte("x"), 0o600))
	}
	idx, err := c.NextUploadPosterIndex(7)
	require.NoError(t, err)
	assert.Equal(t, imagecache.UploadIndexFloor+2, idx)
}

func TestNextUploadPosterIndex_Disabled(t *testing.T) {
	t.Parallel()
	c := imagecache.New("", nil, zerologTest(t))
	_, err := c.NextUploadPosterIndex(1)
	assert.ErrorIs(t, err, imagecache.ErrDisabled)
}

func TestNextUploadBackdropIndex_Empty(t *testing.T) {
	t.Parallel()
	c := imagecache.New(t.TempDir(), nil, zerologTest(t))
	idx, err := c.NextUploadBackdropIndex(99)
	require.NoError(t, err)
	assert.Equal(t, imagecache.UploadIndexFloor, idx)
}

func TestSaveUploadedPoster_RoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body func(*testing.T) []byte
	}{
		{name: "jpeg_input", body: makeJPEG},
		{name: "png_input", body: makePNG},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := imagecache.New(t.TempDir(), nil, zerologTest(t))
			payload := tt.body(t)
			idx, err := c.SaveUploadedPoster(t.Context(), 4711, bytes.NewReader(payload))
			require.NoError(t, err)
			assert.Equal(t, imagecache.UploadIndexFloor, idx)

			// File on disk must be a valid JPEG regardless of input format.
			path := c.PosterPath(4711, idx)
			require.FileExists(t, path)
			f, err := os.Open(path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = f.Close() })
			_, err = jpeg.Decode(f)
			require.NoError(t, err, "stored upload must decode as JPEG")
		})
	}
}

func TestSaveUploadedPoster_AdvancesIndex(t *testing.T) {
	t.Parallel()
	c := imagecache.New(t.TempDir(), nil, zerologTest(t))
	payload := makeJPEG(t)

	first, err := c.SaveUploadedPoster(t.Context(), 1, bytes.NewReader(payload))
	require.NoError(t, err)
	second, err := c.SaveUploadedPoster(t.Context(), 1, bytes.NewReader(payload))
	require.NoError(t, err)
	assert.Equal(t, imagecache.UploadIndexFloor, first)
	assert.Equal(t, imagecache.UploadIndexFloor+1, second,
		"sequential uploads must occupy consecutive slots")

	// CountPosters walks the directory tree, so it should see both
	// uploaded files even though their indexes are far above the
	// upstream-fetched range.
	assert.Equal(t, 2, c.CountPosters(1))
}

func TestSaveUploadedBackdrop_RoundTrip(t *testing.T) {
	t.Parallel()
	c := imagecache.New(t.TempDir(), nil, zerologTest(t))
	payload := makePNG(t)
	idx, err := c.SaveUploadedBackdrop(t.Context(), 90100222, bytes.NewReader(payload))
	require.NoError(t, err)
	assert.Equal(t, imagecache.UploadIndexFloor, idx)
	assert.Equal(t, 1, c.CountBackdrops(90100222))

	// The stored file must round-trip through jpeg.Decode.
	f, err := os.Open(c.BackdropPath(90100222, idx))
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	_, err = jpeg.Decode(f)
	require.NoError(t, err)
}

func TestSaveUploadedPoster_Disabled(t *testing.T) {
	t.Parallel()
	c := imagecache.New("", nil, zerologTest(t))
	_, err := c.SaveUploadedPoster(t.Context(), 1, bytes.NewReader(makeJPEG(t)))
	assert.ErrorIs(t, err, imagecache.ErrDisabled)
}

func TestSaveUploadedPoster_RejectsUndecodable(t *testing.T) {
	t.Parallel()
	c := imagecache.New(t.TempDir(), nil, zerologTest(t))

	_, err := c.SaveUploadedPoster(
		t.Context(), 1, strings.NewReader("not an image"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode upload",
		"undecodable bodies must surface a decode error")

	// Failed decode must NOT leave a half-written file on disk.
	assert.False(t, c.HasPoster(1, imagecache.UploadIndexFloor))
}
