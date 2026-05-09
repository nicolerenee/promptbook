package sync

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/brianvoe/gofakeit/v7"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/imagecache"
)

// stubScreenshotClient is a deterministic EncoraScreenshotClient that
// records every call so tests can assert dedup behavior without
// standing up an httptest server.
type stubScreenshotClient struct {
	mu    sync.Mutex
	urls  map[int64][]string
	calls []int64
	err   error
}

func (s *stubScreenshotClient) Screenshots(
	_ context.Context, id int64,
) ([]string, encora.RateLimitInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, id)
	if s.err != nil {
		return nil, encora.RateLimitInfo{}, s.err
	}
	return s.urls[id], encora.RateLimitInfo{}, nil
}

func (s *stubScreenshotClient) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// fakeJPEG is a 1x1 transparent PNG byte sequence — same shape the
// imagecache_test.go uses. The cache happily saves PNG bytes under
// .jpg; the extension dance is documented at the imagecache package
// level.
//
//nolint:gochecknoglobals // fixture data shared by table tests
var fakeJPEG = []byte{
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

// jpegServer returns an httptest server that serves fakeJPEG with
// Content-Type image/jpeg. hits counts requests so tests can verify
// per-URL idempotency.
func jpegServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	hits := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(fakeJPEG)
		}))
	t.Cleanup(srv.Close)
	return srv, hits
}

// newFakeRecording builds a synthetic encora.Recording with the given
// id and HasScreenshots flag. Cast is empty so the StageMedia path is
// trivially skipped (see disabled() guard). Other fields stay at the
// gofakeit defaults so the structure is plausible end-to-end.
func newFakeRecording(t *testing.T, id int64, hasScreenshots bool) encora.Recording {
	t.Helper()
	return encora.Recording{
		ID:    id,
		Show:  gofakeit.MovieName(),
		Tour:  "Broadway",
		Notes: gofakeit.Sentence(5),
		Metadata: encora.RecordingMeta{
			ShowID:         gofakeit.Int64(),
			HasScreenshots: hasScreenshots,
		},
	}
}

// TestImageFetcherFetchesBackdrops covers the happy path: a recording
// with HasScreenshots=true causes the encora stub to be called, the
// returned URLs are downloaded, and the cache surfaces them.
func TestImageFetcherFetchesBackdrops(t *testing.T) {
	t.Parallel()
	gofakeit.Seed(0)

	srv, hits := jpegServer(t)
	root := t.TempDir()
	cache := imagecache.New(root, srv.Client(), zerolog.New(io.Discard))

	const recordingID int64 = 90100222
	stub := &stubScreenshotClient{
		urls: map[int64][]string{
			recordingID: {srv.URL + "/0.png", srv.URL + "/1.png"},
		},
	}

	f := newImageFetcher(cache, nil, stub, zerolog.New(io.Discard))
	r := newFakeRecording(t, recordingID, true)

	f.forRecording(t.Context(), r)

	assert.Equal(t, 1, stub.callCount(),
		"backdrop fetch must call /screenshots exactly once per recording")
	assert.True(t, cache.HasBackdrop(recordingID, 0))
	assert.True(t, cache.HasBackdrop(recordingID, 1))
	assert.Equal(t, int32(2), hits.Load(),
		"each distinct backdrop URL gets one upstream request")
	assert.Equal(t, 2, cache.CountBackdrops(recordingID))

	// Second call to forRecording must dedup — no extra screenshots
	// call, no additional upstream image hits.
	f.forRecording(t.Context(), r)
	assert.Equal(t, 1, stub.callCount(),
		"second forRecording on the same id must not re-issue /screenshots")
	assert.Equal(t, int32(2), hits.Load())
}

// TestImageFetcherSkipsWhenHasScreenshotsFalse confirms the
// has_screenshots gate prevents a wasted /screenshots call.
func TestImageFetcherSkipsWhenHasScreenshotsFalse(t *testing.T) {
	t.Parallel()
	srv, _ := jpegServer(t)
	root := t.TempDir()
	cache := imagecache.New(root, srv.Client(), zerolog.New(io.Discard))

	stub := &stubScreenshotClient{urls: map[int64][]string{}}
	f := newImageFetcher(cache, nil, stub, zerolog.New(io.Discard))

	r := newFakeRecording(t, 9999, false)
	f.forRecording(t.Context(), r)

	assert.Equal(t, 0, stub.callCount(),
		"has_screenshots=false must short-circuit before the encora call")
	assert.False(t, cache.HasBackdrop(9999, 0))
}

// TestImageFetcherSwallowsScreenshotErrors checks that an upstream
// error from the encora stub doesn't propagate. Image caching is
// best-effort and one bad recording must never abort a sync.
func TestImageFetcherSwallowsScreenshotErrors(t *testing.T) {
	t.Parallel()
	srv, _ := jpegServer(t)
	root := t.TempDir()
	cache := imagecache.New(root, srv.Client(), zerolog.New(io.Discard))

	stub := &stubScreenshotClient{err: errors.New("encora boom")}
	f := newImageFetcher(cache, nil, stub, zerolog.New(io.Discard))

	r := newFakeRecording(t, 90004242, true)
	// forRecording must return cleanly — no panic, no propagated error.
	f.forRecording(t.Context(), r)

	assert.Equal(t, 1, stub.callCount(),
		"the encora call is attempted exactly once before the error is logged")
	assert.False(t, cache.HasBackdrop(90004242, 0))
}

// TestImageFetcherBackdropDisabledWhenCacheOff confirms that a
// disabled cache short-circuits before the encora call. Belt-and-
// suspenders: the cache is the source of truth on caching state,
// not the sync wiring.
func TestImageFetcherBackdropDisabledWhenCacheOff(t *testing.T) {
	t.Parallel()
	stub := &stubScreenshotClient{urls: map[int64][]string{
		1: {"https://example.invalid/x.png"},
	}}
	disabled := imagecache.New("", nil, zerolog.New(io.Discard))
	f := newImageFetcher(disabled, nil, stub, zerolog.New(io.Discard))

	r := newFakeRecording(t, 1, true)
	f.forRecording(t.Context(), r)

	assert.Equal(t, 0, stub.callCount(),
		"disabled cache must skip the encora call entirely")
}
