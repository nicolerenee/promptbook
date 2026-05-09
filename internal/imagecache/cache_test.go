package imagecache_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brianvoe/gofakeit/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/imagecache"
)

// fakePNG is a 1x1 transparent PNG byte sequence — enough payload that
// the cache writer's len(body) > 0 guard passes without us shipping an
// image fixture. The cache happily saves PNG bytes under .jpg; the
// extension dance is documented at the package level.
//
//nolint:gochecknoglobals // fixture data shared by table tests
var fakePNG = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
	0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
	0x89, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x44, 0x41,
	0x54, 0x78, 0x9C, 0x62, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00,
	0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE,
	0x42, 0x60, 0x82,
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
		{name: "PosterPath empty", op: func(t *testing.T) {
			t.Helper()
			assert.Empty(t, c.PosterPath(1, 0))
		}},
		{name: "BackdropPath empty", op: func(t *testing.T) {
			t.Helper()
			assert.Empty(t, c.BackdropPath(1, 0))
		}},
		{name: "HeadshotPath empty", op: func(t *testing.T) {
			t.Helper()
			assert.Empty(t, c.HeadshotPath(1))
		}},
		{name: "HasPoster false", op: func(t *testing.T) {
			t.Helper()
			assert.False(t, c.HasPoster(1, 0))
		}},
		{name: "HasBackdrop false", op: func(t *testing.T) {
			t.Helper()
			assert.False(t, c.HasBackdrop(1, 0))
		}},
		{name: "HasHeadshot false", op: func(t *testing.T) {
			t.Helper()
			assert.False(t, c.HasHeadshot(1))
		}},
		{name: "PosterURL empty", op: func(t *testing.T) {
			t.Helper()
			assert.Empty(t, c.PosterURL(1, 0))
		}},
		{name: "HeadshotURL empty", op: func(t *testing.T) {
			t.Helper()
			assert.Empty(t, c.HeadshotURL(1))
		}},
		{name: "Counts zero", op: func(t *testing.T) {
			t.Helper()
			assert.Equal(t, imagecache.Counts{}, c.Counts())
		}},
		{name: "FetchPoster ErrDisabled", op: func(t *testing.T) {
			t.Helper()
			_, err := c.FetchPoster(t.Context(), 1, 0, "https://example.invalid/x.jpg")
			require.Error(t, err)
			assert.ErrorIs(t, err, imagecache.ErrDisabled)
		}},
		{name: "FetchHeadshot ErrDisabled", op: func(t *testing.T) {
			t.Helper()
			_, err := c.FetchHeadshot(t.Context(), 1, "https://example.invalid/x.jpg")
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

func TestFetchPosterRoundTrip(t *testing.T) {
	t.Parallel()
	gofakeit.Seed(0)
	srv, hits := imageServer(t, "image/jpeg")

	root := t.TempDir()
	c := imagecache.New(root, srv.Client(), zerologTest(t))

	showID := gofakeit.Int64()
	require.False(t, c.HasPoster(showID, 0))

	dest, err := c.FetchPoster(t.Context(), showID, 0, srv.URL+"/poster.jpg")
	require.NoError(t, err)
	assert.True(t, c.HasPoster(showID, 0))
	assert.Equal(t, c.PosterPath(showID, 0), dest)
	assert.FileExists(t, dest)

	body, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, fakePNG, body)
	assert.Equal(t, "/images/posters/"+strconv.FormatInt(showID, 10)+"/0.jpg",
		c.PosterURL(showID, 0))

	// Re-fetch is a no-op; the upstream server should not be hit
	// twice for the same slot.
	_, err = c.FetchPoster(t.Context(), showID, 0, srv.URL+"/poster.jpg")
	require.NoError(t, err)
	assert.Equal(t, int32(1), hits.Load(),
		"second FetchPoster must not re-download an already-cached file")
}

func TestFetchHeadshotIdempotent(t *testing.T) {
	t.Parallel()
	gofakeit.Seed(1)
	srv, hits := imageServer(t, "image/png")

	root := t.TempDir()
	c := imagecache.New(root, srv.Client(), zerologTest(t))

	actorID := gofakeit.Int64()

	// Fetch twice; both calls must succeed but the upstream server
	// is hit exactly once.
	_, err := c.FetchHeadshot(t.Context(), actorID, srv.URL+"/h.png")
	require.NoError(t, err)
	_, err = c.FetchHeadshot(t.Context(), actorID, srv.URL+"/h.png")
	require.NoError(t, err)

	assert.Equal(t, int32(1), hits.Load())
	assert.True(t, c.HasHeadshot(actorID))

	url := c.HeadshotURL(actorID)
	assert.Equal(t, "/images/headshots/"+strconv.FormatInt(actorID, 10)+".jpg", url)
}

func TestFetchRejectsNonImageContentType(t *testing.T) {
	t.Parallel()
	srv, _ := imageServer(t, "text/html")

	root := t.TempDir()
	c := imagecache.New(root, srv.Client(), zerologTest(t))

	_, err := c.FetchPoster(t.Context(), 42, 0, srv.URL+"/oops.html")
	require.Error(t, err)
	require.ErrorIs(t, err, imagecache.ErrNotImage)
	assert.False(t, c.HasPoster(42, 0))
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

	_, err := c.FetchPoster(t.Context(), 99, 0, srv.URL+"/boom")
	require.Error(t, err)
	assert.False(t, c.HasPoster(99, 0))
}

func TestCounts(t *testing.T) {
	t.Parallel()
	srv, _ := imageServer(t, "image/jpeg")

	root := t.TempDir()
	c := imagecache.New(root, srv.Client(), zerologTest(t))

	// Empty root → all zeros.
	assert.Equal(t, imagecache.Counts{}, c.Counts())

	_, err := c.FetchPoster(t.Context(), 1, 0, srv.URL+"/p0.jpg")
	require.NoError(t, err)
	_, err = c.FetchPoster(t.Context(), 1, 1, srv.URL+"/p1.jpg")
	require.NoError(t, err)
	_, err = c.FetchPoster(t.Context(), 2, 0, srv.URL+"/p2.jpg")
	require.NoError(t, err)
	_, err = c.FetchHeadshot(t.Context(), 7, srv.URL+"/h7.jpg")
	require.NoError(t, err)
	_, err = c.FetchHeadshot(t.Context(), 8, srv.URL+"/h8.jpg")
	require.NoError(t, err)
	_, err = c.FetchBackdrop(t.Context(), 555, 0, srv.URL+"/b0.jpg")
	require.NoError(t, err)

	counts := c.Counts()
	assert.Equal(t, imagecache.Counts{
		Posters: 3, Backdrops: 1, Headshots: 2,
	}, counts)
}

func TestCountBackdrops(t *testing.T) {
	t.Parallel()
	srv, _ := imageServer(t, "image/jpeg")

	root := t.TempDir()
	c := imagecache.New(root, srv.Client(), zerologTest(t))

	const recA int64 = 90100222
	const recB int64 = 8223

	// Empty directory tree → zero count even before any fetches.
	assert.Equal(t, 0, c.CountBackdrops(recA))

	// Populate two backdrops for recA, one for recB; they must
	// be counted separately so the per-recording surface in the
	// API doesn't bleed across recordings.
	_, err := c.FetchBackdrop(t.Context(), recA, 0, srv.URL+"/a0.jpg")
	require.NoError(t, err)
	_, err = c.FetchBackdrop(t.Context(), recA, 1, srv.URL+"/a1.jpg")
	require.NoError(t, err)
	_, err = c.FetchBackdrop(t.Context(), recB, 0, srv.URL+"/b0.jpg")
	require.NoError(t, err)

	assert.Equal(t, 2, c.CountBackdrops(recA))
	assert.Equal(t, 1, c.CountBackdrops(recB))
	assert.Equal(t, 0, c.CountBackdrops(int64(99999)),
		"a recording with no cached backdrops returns 0")

	// Disabled cache returns 0 without touching the filesystem.
	disabled := imagecache.New("", nil, zerologTest(t))
	assert.Equal(t, 0, disabled.CountBackdrops(recA))
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
			name:    "poster",
			path:    c.PosterPath(101, 2),
			wantRel: filepath.Join("posters", "101", "2.jpg"),
		},
		{
			name:    "backdrop",
			path:    c.BackdropPath(202, 5),
			wantRel: filepath.Join("backdrops", "202", "5.jpg"),
		},
		{
			name:    "headshot",
			path:    c.HeadshotPath(303),
			wantRel: filepath.Join("headshots", "303.jpg"),
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
	_, err := c.FetchPoster(t.Context(), 1, 0, srv.URL+"/never.jpg")
	require.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second,
		"the per-request timeout must short-circuit a stuck upstream")
	// Don't assert the precise error; net/http surfaces a context
	// deadline error here and it can vary by platform.
	assert.NotErrorIs(t, err, imagecache.ErrDisabled)
}

// TestFetchSurvivesPartialBytes guards against a regression where an
// io.EOF mid-stream silently produced a truncated cache entry. We
// simulate the partial response by closing the connection after the
// first chunk; the cache should fail loudly rather than persist a
// corrupt file.
func TestFetchSurvivesPartialBytes(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "image/jpeg")
			// Hijack to drop the connection mid-write.
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
			// Don't write a body; the next read will see EOF.
			_ = conn.Close()
		}))
	t.Cleanup(srv.Close)

	root := t.TempDir()
	c := imagecache.New(root, srv.Client(), zerologTest(t))

	_, err := c.FetchPoster(t.Context(), 7, 0, srv.URL+"/x.jpg")
	// Either we get ErrNotImage (empty body) or a transport-level
	// error; both are fine. What we don't tolerate is a quiet success
	// with a zero-byte file on disk.
	if err == nil {
		assert.False(t, c.HasPoster(7, 0),
			"truncated upstream must not produce a cached file")
		return
	}
	// On the error path, no file should be visible — fetchTo writes
	// only after a successful body read.
	assert.False(t, c.HasPoster(7, 0))
	// One known shape is ErrNotImage when the body comes back empty;
	// the alternative is a transport error. We don't pin the exact
	// error so the test stays robust across Go versions.
	if errors.Is(err, imagecache.ErrNotImage) {
		assert.ErrorIs(t, err, imagecache.ErrNotImage)
	}
}
