package imagecache_test

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brianvoe/gofakeit/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/imagecache"
)

// fakePNG is a tiny but real 1x1 PNG built at package-init time. The
// hand-rolled byte literal that used to live here was malformed (the
// IDAT chunk's deflate stream was corrupt), which the old tests never
// noticed because fetchTo streamed bytes through unchanged. Now that
// fetchTo decodes + re-encodes to JPEG, the upstream payload has to
// be a real image — so we build one with png.Encode.
//
//nolint:gochecknoglobals // fixture data shared by table tests
var fakePNG = func() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 200, G: 100, B: 50, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic("imagecache test fixture: png.Encode: " + err.Error())
	}
	return buf.Bytes()
}()

// assertJPEGAt verifies the file at path decodes as a JPEG and (when
// w/h are non-zero) matches the expected dimensions. Replaces the
// older byte-identity check that no longer works now that fetchTo
// re-encodes upstream bytes to JPEG.
func assertJPEGAt(t *testing.T, path string, w, h int) {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	img, err := jpeg.Decode(f)
	require.NoError(t, err, "expected a valid JPEG at %s", path)
	if w > 0 {
		assert.Equal(t, w, img.Bounds().Dx())
	}
	if h > 0 {
		assert.Equal(t, h, img.Bounds().Dy())
	}
}

// imageServer returns an httptest server that serves fakePNG with a
// configurable Content-Type and 200 status. hits is a counter the
// caller can assert against to verify idempotency.
func imageServer(t *testing.T, contentType string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	hits := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			if contentType != "" {
				w.Header().Set("Content-Type", contentType)
			}
			_, _ = w.Write(fakePNG)
		}))
	t.Cleanup(srv.Close)
	return srv, hits
}

// makeJPEG returns a tiny valid JPEG byte slice. Used as the synthetic
// upload payload in the SaveUploaded* tests.
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

func TestCacheDisabled(t *testing.T) {
	t.Parallel()
	c := imagecache.New("", nil, zerologTest(t))

	tests := []struct {
		name string
		op   func(*testing.T)
	}{
		{name: "Disabled true", op: func(t *testing.T) {
			t.Helper()
			assert.True(t, c.Disabled())
		}},
		{name: "HeadshotPath empty", op: func(t *testing.T) {
			t.Helper()
			assert.Empty(t, c.HeadshotPath(1))
		}},
		{name: "ShowBannerPath empty", op: func(t *testing.T) {
			t.Helper()
			assert.Empty(t, c.ShowBannerPath(1))
		}},
		{name: "RecordingFanartPath empty", op: func(t *testing.T) {
			t.Helper()
			assert.Empty(t, c.RecordingFanartPath(1))
		}},
		{name: "RecordingPosterPath empty", op: func(t *testing.T) {
			t.Helper()
			assert.Empty(t, c.RecordingPosterPath(1))
		}},
		{name: "HasHeadshot false", op: func(t *testing.T) {
			t.Helper()
			assert.False(t, c.HasHeadshot(1))
		}},
		{name: "HasShowBanner false", op: func(t *testing.T) {
			t.Helper()
			assert.False(t, c.HasShowBanner(1))
		}},
		{name: "HasRecordingFanart false", op: func(t *testing.T) {
			t.Helper()
			assert.False(t, c.HasRecordingFanart(1))
		}},
		{name: "HasRecordingPoster false", op: func(t *testing.T) {
			t.Helper()
			assert.False(t, c.HasRecordingPoster(1))
		}},
		{name: "HeadshotURL empty", op: func(t *testing.T) {
			t.Helper()
			assert.Empty(t, c.HeadshotURL(1))
		}},
		{name: "ShowBannerURL empty", op: func(t *testing.T) {
			t.Helper()
			assert.Empty(t, c.ShowBannerURL(1))
		}},
		{name: "Counts zero", op: func(t *testing.T) {
			t.Helper()
			assert.Equal(t, imagecache.Counts{}, c.Counts())
		}},
		{name: "FetchHeadshot ErrDisabled", op: func(t *testing.T) {
			t.Helper()
			_, err := c.FetchHeadshot(t.Context(), 1, "https://example.invalid/x.jpg")
			require.Error(t, err)
			assert.ErrorIs(t, err, imagecache.ErrDisabled)
		}},
		{name: "FetchShowBanner ErrDisabled", op: func(t *testing.T) {
			t.Helper()
			_, err := c.FetchShowBanner(t.Context(), 1, "https://example.invalid/x.jpg")
			require.Error(t, err)
			assert.ErrorIs(t, err, imagecache.ErrDisabled)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.op(t)
		})
	}
}

func TestNilCacheDisabled(t *testing.T) {
	t.Parallel()
	// A nil *Cache is the zero-value-safe variant of the disabled
	// path — handlers that gate on `if cache != nil && !cache.Disabled()`
	// would be a footgun if Disabled() panicked on nil receivers.
	var c *imagecache.Cache
	assert.True(t, c.Disabled())
}

func TestFetchHeadshotRoundTrip(t *testing.T) {
	t.Parallel()
	gofakeit.Seed(0)
	srv, hits := imageServer(t, "image/jpeg")

	root := t.TempDir()
	c := imagecache.New(root, srv.Client(), zerologTest(t))

	actorID := gofakeit.Int64()
	require.False(t, c.HasHeadshot(actorID))

	dest, err := c.FetchHeadshot(t.Context(), actorID, srv.URL+"/h.jpg")
	require.NoError(t, err)
	assert.True(t, c.HasHeadshot(actorID))
	assert.Equal(t, c.HeadshotPath(actorID), dest)
	assert.FileExists(t, dest)

	// The on-disk file is a JPEG (re-encoded from the PNG body), so
	// byte-identity with fakePNG no longer holds. Verify the decode
	// + dimensions instead.
	assertJPEGAt(t, dest, 1, 1)
	assert.Equal(t, "/images/actors/"+strconv.FormatInt(actorID, 10)+".jpg",
		c.HeadshotURL(actorID))

	// Re-fetch is a no-op; the upstream server should not be hit
	// twice for the same slot.
	_, err = c.FetchHeadshot(t.Context(), actorID, srv.URL+"/h.jpg")
	require.NoError(t, err)
	assert.Equal(t, int32(1), hits.Load(),
		"second FetchHeadshot must not re-download an already-cached file")
}

func TestFetchShowBannerIdempotent(t *testing.T) {
	t.Parallel()
	gofakeit.Seed(1)
	srv, hits := imageServer(t, "image/png")

	root := t.TempDir()
	c := imagecache.New(root, srv.Client(), zerologTest(t))

	showID := gofakeit.Int64()

	// Fetch twice; both calls must succeed but the upstream server
	// is hit exactly once.
	_, err := c.FetchShowBanner(t.Context(), showID, srv.URL+"/banner.png")
	require.NoError(t, err)
	_, err = c.FetchShowBanner(t.Context(), showID, srv.URL+"/banner.png")
	require.NoError(t, err)

	assert.Equal(t, int32(1), hits.Load())
	assert.True(t, c.HasShowBanner(showID))

	url := c.ShowBannerURL(showID)
	assert.Equal(t, "/images/shows/"+strconv.FormatInt(showID, 10)+"/banner.jpg", url)
}

func TestFetchRecordingFanartAndPosterSrc(t *testing.T) {
	t.Parallel()
	srv, _ := imageServer(t, "image/jpeg")

	root := t.TempDir()
	c := imagecache.New(root, srv.Client(), zerologTest(t))

	const recID int64 = 90004242
	_, err := c.FetchRecordingFanart(t.Context(), recID, srv.URL+"/fanart.jpg")
	require.NoError(t, err)
	_, err = c.FetchRecordingPosterSrc(t.Context(), recID, srv.URL+"/poster-src.jpg")
	require.NoError(t, err)

	assert.True(t, c.HasRecordingFanart(recID))
	assert.True(t, c.HasRecordingPosterSrc(recID))
	// poster.jpg is not auto-produced — it lands only after the renderer
	// composites poster-src.jpg + overlay.
	assert.False(t, c.HasRecordingPoster(recID))

	assert.Equal(t,
		"/images/recordings/"+strconv.FormatInt(recID, 10)+"/fanart.jpg",
		c.RecordingFanartURL(recID))
}

func TestFetchRejectsNonImageContentType(t *testing.T) {
	t.Parallel()
	srv, _ := imageServer(t, "text/html")

	root := t.TempDir()
	c := imagecache.New(root, srv.Client(), zerologTest(t))

	_, err := c.FetchHeadshot(t.Context(), 42, srv.URL+"/oops.html")
	require.Error(t, err)
	require.ErrorIs(t, err, imagecache.ErrNotImage)
	assert.False(t, c.HasHeadshot(42))
}

func TestFetchPropagatesUpstream500(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
	t.Cleanup(srv.Close)

	root := t.TempDir()
	c := imagecache.New(root, srv.Client(), zerologTest(t))

	_, err := c.FetchShowBanner(t.Context(), 99, srv.URL+"/boom")
	require.Error(t, err)
	assert.False(t, c.HasShowBanner(99))
}

func TestCounts(t *testing.T) {
	t.Parallel()
	srv, _ := imageServer(t, "image/jpeg")

	root := t.TempDir()
	c := imagecache.New(root, srv.Client(), zerologTest(t))

	// Empty root → all zeros.
	assert.Equal(t, imagecache.Counts{}, c.Counts())

	_, err := c.FetchShowBanner(t.Context(), 1, srv.URL+"/p0.jpg")
	require.NoError(t, err)
	_, err = c.FetchShowBanner(t.Context(), 2, srv.URL+"/p2.jpg")
	require.NoError(t, err)
	_, err = c.FetchHeadshot(t.Context(), 7, srv.URL+"/h7.jpg")
	require.NoError(t, err)
	_, err = c.FetchHeadshot(t.Context(), 8, srv.URL+"/h8.jpg")
	require.NoError(t, err)
	_, err = c.FetchRecordingFanart(t.Context(), 555, srv.URL+"/b0.jpg")
	require.NoError(t, err)
	// poster-src.jpg + a synthetic poster.jpg so the counter sees the
	// burned-in slot.
	_, err = c.FetchRecordingPosterSrc(t.Context(), 555, srv.URL+"/ps.jpg")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(c.RecordingPosterPath(555),
		[]byte("not really a jpeg"), 0o600))

	counts := c.Counts()
	assert.Equal(t, imagecache.Counts{
		Headshots:        2,
		ShowBanners:      2,
		RecordingFanarts: 1,
		RecordingPosters: 1,
	}, counts)
}

func TestPathLayoutMatchesURL(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	c := imagecache.New(root, nil, zerologTest(t))

	tests := []struct {
		name    string
		path    string
		wantRel string
	}{
		{
			name:    "headshot",
			path:    c.HeadshotPath(303),
			wantRel: filepath.Join("actors", "303.jpg"),
		},
		{
			name:    "show_banner",
			path:    c.ShowBannerPath(101),
			wantRel: filepath.Join("shows", "101", "banner.jpg"),
		},
		{
			name:    "recording_fanart",
			path:    c.RecordingFanartPath(202),
			wantRel: filepath.Join("recordings", "202", "fanart.jpg"),
		},
		{
			name:    "recording_poster",
			path:    c.RecordingPosterPath(202),
			wantRel: filepath.Join("recordings", "202", "poster.jpg"),
		},
		{
			name:    "recording_poster_src",
			path:    c.RecordingPosterSrcPath(202),
			wantRel: filepath.Join("recordings", "202", "poster-src.jpg"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rel, err := filepath.Rel(root, tt.path)
			require.NoError(t, err)
			assert.Equal(t, tt.wantRel, rel)
		})
	}
}

func TestSaveUploadedRoundTrip(t *testing.T) {
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

			require.NoError(t, c.SaveUploadedHeadshot(
				t.Context(), 4711, bytes.NewReader(payload)))
			require.NoError(t, c.SaveUploadedShowBanner(
				t.Context(), 5811, bytes.NewReader(payload)))
			require.NoError(t, c.SaveUploadedRecordingFanart(
				t.Context(), 6911, bytes.NewReader(payload)))
			require.NoError(t, c.SaveUploadedRecordingPosterSrc(
				t.Context(), 7011, bytes.NewReader(payload)))

			// Every saved file must round-trip through jpeg.Decode.
			for _, p := range []string{
				c.HeadshotPath(4711),
				c.ShowBannerPath(5811),
				c.RecordingFanartPath(6911),
				c.RecordingPosterSrcPath(7011),
			} {
				require.FileExists(t, p)
				f, err := os.Open(p)
				require.NoError(t, err)
				_, err = jpeg.Decode(f)
				require.NoError(t, err, "stored upload must decode as JPEG")
				_ = f.Close()
			}
		})
	}
}

func TestSaveUploadedRejectsUndecodable(t *testing.T) {
	t.Parallel()
	c := imagecache.New(t.TempDir(), nil, zerologTest(t))

	err := c.SaveUploadedHeadshot(
		t.Context(), 1, strings.NewReader("not an image"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode")
	assert.False(t, c.HasHeadshot(1),
		"failed decode must not leave a half-written file on disk")
}

func TestSaveUploadedDisabled(t *testing.T) {
	t.Parallel()
	c := imagecache.New("", nil, zerologTest(t))
	err := c.SaveUploadedHeadshot(t.Context(), 1, bytes.NewReader(makeJPEG(t)))
	assert.ErrorIs(t, err, imagecache.ErrDisabled)
}

func TestFetchTimesOutOnStuckUpstream(t *testing.T) {
	t.Parallel()
	// httptest server that hangs forever — the cache's per-request
	// context deadline should fire before the test's overall timeout.
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
			w.WriteHeader(http.StatusGatewayTimeout)
		}))
	t.Cleanup(srv.Close)

	hc := &http.Client{Timeout: 50 * time.Millisecond}
	root := t.TempDir()
	c := imagecache.New(root, hc, zerologTest(t))

	start := time.Now()
	_, err := c.FetchHeadshot(t.Context(), 1, srv.URL+"/never.jpg")
	require.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second,
		"the per-request timeout must short-circuit a stuck upstream")
	// Don't assert the precise error; net/http surfaces a context
	// deadline error here and it can vary by platform.
	assert.NotErrorIs(t, err, imagecache.ErrDisabled)
}

// TestFetchSurvivesPartialBytes guards against a regression where an
// io.EOF mid-stream silently produced a truncated cache entry.
func TestFetchSurvivesPartialBytes(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "image/jpeg")
			hj, ok := w.(http.Hijacker)
			if !ok {
				_, _ = w.Write(fakePNG[:5])
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte(
				"HTTP/1.1 200 OK\r\nContent-Type: image/jpeg\r\n\r\n"))
			_ = conn.Close()
		}))
	t.Cleanup(srv.Close)

	root := t.TempDir()
	c := imagecache.New(root, srv.Client(), zerologTest(t))

	_, err := c.FetchHeadshot(t.Context(), 7, srv.URL+"/x.jpg")
	if err == nil {
		assert.False(t, c.HasHeadshot(7),
			"truncated upstream must not produce a cached file")
		return
	}
	assert.False(t, c.HasHeadshot(7))
	if errors.Is(err, imagecache.ErrNotImage) {
		assert.ErrorIs(t, err, imagecache.ErrNotImage)
	}
}
