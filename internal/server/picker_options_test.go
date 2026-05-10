package server_test

// picker_options_test.go — tests for the live picker-options
// endpoints. Each handler hits StageMedia / Encora at request time;
// the fakes here let us exercise the success / error / empty paths
// without spinning up an httptest upstream.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/probe"
	"github.com/nicolerenee/promptbook/internal/server"
	"github.com/nicolerenee/promptbook/internal/stagemedia"
	"github.com/nicolerenee/promptbook/internal/storage"
	syncpkg "github.com/nicolerenee/promptbook/internal/sync"
)

// fakeScreenshotClient stubs server.EncoraScreenshotClient. urls is
// returned verbatim; err short-circuits before recording.
type fakeScreenshotClient struct {
	mu    sync.Mutex
	calls []int64
	urls  []string
	err   error
}

func (f *fakeScreenshotClient) Screenshots(
	_ context.Context, id int64,
) ([]string, encora.RateLimitInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, encora.RateLimitInfo{}, f.err
	}
	f.calls = append(f.calls, id)
	out := make([]string, len(f.urls))
	copy(out, f.urls)
	return out, encora.RateLimitInfo{}, nil
}

func (f *fakeScreenshotClient) Calls() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int64, len(f.calls))
	copy(out, f.calls)
	return out
}

// fixtureBackedServerWithClients wires the fixture sync exactly like
// fixtureBackedServer and lets the caller hand the server stub
// stagemedia + encora-screenshots clients. Both are optional; nil
// arguments leave the corresponding /api surface 503.
func fixtureBackedServerWithClients(
	t *testing.T,
	sm server.StagemediaImageClient,
	enc server.EncoraScreenshotClient,
) *server.Server {
	t.Helper()
	return fixtureBackedServerWithAllClients(t, sm, enc, nil)
}

// fixtureBackedServerWithAllClients is fixtureBackedServerWithClients
// plus an optional TMDB client. Pulled out as a sibling helper so
// the existing signature stays the same for callers that don't care
// about the TMDB picker source.
func fixtureBackedServerWithAllClients(
	t *testing.T,
	sm server.StagemediaImageClient,
	enc server.EncoraScreenshotClient,
	tm server.TMDBClient,
) *server.Server {
	t.Helper()

	mux := http.NewServeMux()
	for path, file := range map[string]string{
		"/api/profile":    "profile.json",
		"/api/collection": "collection.json",
		"/api/wants":      "wants.json",
	} {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			b, err := readFixture(t, file)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("X-Ratelimit-Remaining", "25")
			_, _ = w.Write(b)
		})
	}
	upstream := httptest.NewServer(mux)
	t.Cleanup(upstream.Close)

	sqlDB, db, err := storage.OpenEnt(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	c, err := encora.New(encora.Options{BaseURL: upstream.URL, APIKey: "test"})
	require.NoError(t, err)
	_, err = syncpkg.Sync(t.Context(), c, db, syncpkg.Options{BurstReserve: 2, SQLDB: sqlDB})
	require.NoError(t, err)

	srv, err := server.New(server.Options{
		DB:                db,
		SQLDB:             sqlDB,
		Stagemedia:        sm,
		EncoraScreenshots: enc,
		TMDB:              tm,
	})
	require.NoError(t, err)
	return srv
}

// TestPickerShowPosterOptions covers the show-poster-options
// endpoint: a stub stagemedia client returns Posters URLs which the
// handler maps to {url, source: "stagemedia"} options.
func TestPickerShowPosterOptions(t *testing.T) {
	t.Parallel()

	const showID int64 = 90004089
	wantURLs := []string{
		"https://stagemedia.example/posters/90004089-a.jpg",
		"https://stagemedia.example/posters/90004089-b.jpg",
	}
	fake := &fakeStagemediaImageClient{
		postersByShow: map[int64][]string{showID: wantURLs},
	}
	srv := fixtureBackedServerWithClients(t, fake, nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/v1/shows/"+strconv.FormatInt(showID, 10)+"/poster-options", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		Options []struct {
			URL    string `json:"url"`
			Source string `json:"source"`
		} `json:"options"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	require.Len(t, body.Options, 2)
	assert.Equal(t, wantURLs[0], body.Options[0].URL)
	assert.Equal(t, "stagemedia", body.Options[0].Source)
}

// TestPickerShowPosterOptions503 confirms a 503 + helpful message
// when no stagemedia client is wired.
func TestPickerShowPosterOptions503(t *testing.T) {
	t.Parallel()

	srv := fixtureBackedServerWithClients(t, nil, nil)
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/v1/shows/90004089/poster-options", nil)
	srv.Handler().ServeHTTP(rr, req)
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
}

// TestPickerRecordingPosterOptions resolves the show id off the
// recording row before calling stagemedia. Recording 90100222's show_id
// is 90004089 in the fixture.
func TestPickerRecordingPosterOptions(t *testing.T) {
	t.Parallel()

	const recID int64 = 90100222
	const showID int64 = 90004089
	wantURL := "https://stagemedia.example/posters/90004089.jpg"
	fake := &fakeStagemediaImageClient{
		postersByShow: map[int64][]string{showID: {wantURL}},
	}
	srv := fixtureBackedServerWithClients(t, fake, nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/v1/recordings/"+strconv.FormatInt(recID, 10)+"/poster-options", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		Options []struct {
			URL    string `json:"url"`
			Source string `json:"source"`
		} `json:"options"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	require.Len(t, body.Options, 1)
	assert.Equal(t, wantURL, body.Options[0].URL)

	calls := fake.PosterCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, showID, calls[0])
}

// TestPickerRecordingFanartOptions calls Encora /screenshots when
// has_screenshots is true.
func TestPickerRecordingFanartOptions(t *testing.T) {
	t.Parallel()

	const recID int64 = 90100222 // has_screenshots = true in fixture.
	urls := []string{
		"https://encora.example/screenshots/90100222/01.jpg",
		"https://encora.example/screenshots/90100222/02.jpg",
	}
	enc := &fakeScreenshotClient{urls: urls}
	srv := fixtureBackedServerWithClients(t, nil, enc)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/v1/recordings/"+strconv.FormatInt(recID, 10)+"/fanart-options", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		Options []struct {
			URL    string `json:"url"`
			Source string `json:"source"`
		} `json:"options"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	require.Len(t, body.Options, 2)
	for _, opt := range body.Options {
		assert.Equal(t, "encora", opt.Source)
	}
	assert.Equal(t, []int64{recID}, enc.Calls())
}

// TestPickerRecordingFanartOptionsNoScreenshots short-circuits to a
// 200 + empty options when has_screenshots is false on the recording —
// no upstream call should fire.
func TestPickerRecordingFanartOptionsNoScreenshots(t *testing.T) {
	t.Parallel()

	const recID int64 = 8326 // has_screenshots = false in fixture.
	enc := &fakeScreenshotClient{
		err: errors.New("must not be called"),
	}
	srv := fixtureBackedServerWithClients(t, nil, enc)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/v1/recordings/"+strconv.FormatInt(recID, 10)+"/fanart-options", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		Options []struct {
			URL    string `json:"url"`
			Source string `json:"source"`
		} `json:"options"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Empty(t, body.Options)
	assert.Empty(t, enc.Calls(), "encora client must not be called when has_screenshots=false")
}

// TestPickerActorHeadshotOptions returns a single-element options
// list when stagemedia hits, scoped to a show id derived from the
// performer's first credit.
func TestPickerActorHeadshotOptions(t *testing.T) {
	t.Parallel()

	const performerID int64 = 90001001
	wantURL := "https://stagemedia.example/headshots/90001001.jpg"
	fake := &fakeStagemediaImageClient{
		performers: []stagemedia.Performer{{ID: performerID, URL: wantURL}},
	}
	srv := fixtureBackedServerWithClients(t, fake, nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/v1/actors/"+strconv.FormatInt(performerID, 10)+"/headshot-options", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		Options []struct {
			URL    string `json:"url"`
			Source string `json:"source"`
		} `json:"options"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	require.Len(t, body.Options, 1)
	assert.Equal(t, wantURL, body.Options[0].URL)
	assert.Equal(t, "stagemedia", body.Options[0].Source)
}

// fakeFrameExtractor stubs server.FrameExtractor by writing N
// touch-files (0.jpg .. (count-1).jpg) into outDir on Extract. The
// recorded slice lets tests assert how many times the extractor
// fired across a session — useful for proving the cache short-
// circuits a second open and the refresh=true branch re-extracts.
type fakeFrameExtractor struct {
	mu       sync.Mutex
	calls    []fakeFrameExtractCall
	count    int  // override for the number of frames to write; 0 -> requested count.
	failOnce bool // when true the first call returns an error and writes nothing.
}

type fakeFrameExtractCall struct {
	videoPath string
	outDir    string
	duration  float64
	count     int
}

func (f *fakeFrameExtractor) Extract(
	_ context.Context,
	videoPath, outDir string,
	durationSeconds float64,
	count int,
	_ probe.FrameRandSource,
) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeFrameExtractCall{
		videoPath: videoPath, outDir: outDir,
		duration: durationSeconds, count: count,
	})
	if f.failOnce {
		f.failOnce = false
		return nil, errors.New("synthetic extract failure")
	}
	if mkErr := os.MkdirAll(outDir, 0o750); mkErr != nil {
		return nil, mkErr
	}
	emit := f.count
	if emit == 0 {
		emit = count
	}
	out := make([]string, 0, emit)
	for i := range emit {
		p := filepath.Join(outDir, strconv.Itoa(i)+".jpg")
		if err := os.WriteFile(p, []byte{0xFF, 0xD8, 0xFF, 0xD9}, 0o600); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func (f *fakeFrameExtractor) Calls() []fakeFrameExtractCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeFrameExtractCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// frameFallbackRecordingID is the fixture recording every frame-
// fallback test exercises. Recording 90100222 carries has_screenshots=true
// in collection.json, so the new fanart handler still hits the
// (faked-empty) Encora screenshots client before falling through to
// the frame extractor — the right path for the fallback to exercise.
const frameFallbackRecordingID int64 = 90100222

// frameFallbackServer wires the fixture sync + an image cache + a
// fake frame extractor + (optionally) a fake encora screenshots
// client. Returns the server and the fake extractor so individual
// tests can assert on per-call state. mediaInfoJSON populates the
// recording version's media-info blob; the fanart handler decodes
// it directly to skip a live ffprobe pass.
func frameFallbackServer(
	t *testing.T,
	enc server.EncoraScreenshotClient,
	mediaInfoJSON string,
) (*server.Server, *fakeFrameExtractor) {
	t.Helper()
	rec := frameFallbackRecordingID

	mux := http.NewServeMux()
	for path, file := range map[string]string{
		"/api/profile":    "profile.json",
		"/api/collection": "collection.json",
		"/api/wants":      "wants.json",
	} {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			b, err := readFixture(t, file)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("X-Ratelimit-Remaining", "25")
			_, _ = w.Write(b)
		})
	}
	upstream := httptest.NewServer(mux)
	t.Cleanup(upstream.Close)

	sqlDB, db, err := storage.OpenEnt(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	c, err := encora.New(encora.Options{BaseURL: upstream.URL, APIKey: "test"})
	require.NoError(t, err)
	_, err = syncpkg.Sync(t.Context(), c, db, syncpkg.Options{BurstReserve: 2})
	require.NoError(t, err)

	// Lay down a real (but tiny) file at the recording's primary
	// version path so the fanart-source resolver's os.Stat passes.
	videoPath := filepath.Join(t.TempDir(), "recording.mkv")
	require.NoError(t, os.WriteFile(videoPath, []byte("fake video"), 0o600))
	require.NoError(t, storage.UpsertVersion(t.Context(), db, storage.RecordingVersion{
		RecordingID:   rec,
		FilePath:      videoPath,
		FileSizeBytes: 1024,
		Container:     "MKV",
		MediaInfoJSON: mediaInfoJSON,
	}))

	cache := imagecache.New(t.TempDir(), nil, zerolog.Nop())
	fake := &fakeFrameExtractor{}

	srv, err := server.New(server.Options{
		DB:                db,
		EncoraScreenshots: enc,
		ImageCache:        cache,
		FrameExtractor:    fake,
	})
	require.NoError(t, err)
	return srv, fake
}

// TestPickerRecordingFanartOptionsFrameFallback exercises the
// frame-extract fallback. Recording 90100222 has has_screenshots=true in
// the fixture but our fake screenshots client returns an empty list,
// so the handler should fall through to the local frame-extractor
// and emit 10 same-origin /images/frames/recordings/<id>/<idx>.jpg
// URLs tagged source="frames".
func TestPickerRecordingFanartOptionsFrameFallback(t *testing.T) {
	t.Parallel()

	recID := frameFallbackRecordingID
	enc := &fakeScreenshotClient{} // empty urls.
	mediaInfo := `{"durationSeconds": 7200, "videoCodec": "h264"}`
	srv, fake := frameFallbackServer(t, enc, mediaInfo)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/v1/recordings/"+strconv.FormatInt(recID, 10)+"/fanart-options", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		Options []struct {
			URL    string `json:"url"`
			Source string `json:"source"`
		} `json:"options"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	require.Len(t, body.Options, 10, "frame fallback should emit 10 options")
	for i, opt := range body.Options {
		assert.Equal(t, "frames", opt.Source)
		want := "/images/frames/recordings/" + strconv.FormatInt(recID, 10) +
			"/" + strconv.Itoa(i) + ".jpg"
		assert.Equal(t, want, opt.URL)
	}

	// Extractor was called exactly once with the configured count.
	calls := fake.Calls()
	require.Len(t, calls, 1)
	assert.Equal(t, 10, calls[0].count)
	assert.InDelta(t, 7200.0, calls[0].duration, 1e-9)
}

// TestPickerRecordingFanartOptionsFramesCacheHit confirms a second
// fanart-options call without ?refresh=true reuses the existing
// frame extracts on disk instead of re-running ffmpeg.
func TestPickerRecordingFanartOptionsFramesCacheHit(t *testing.T) {
	t.Parallel()

	recID := frameFallbackRecordingID
	enc := &fakeScreenshotClient{}
	mediaInfo := `{"durationSeconds": 3600}`
	srv, fake := frameFallbackServer(t, enc, mediaInfo)

	for range 2 {
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
			"/api/v1/recordings/"+strconv.FormatInt(recID, 10)+"/fanart-options", nil)
		srv.Handler().ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	}
	assert.Len(t, fake.Calls(), 1, "cached frames should short-circuit second call")
}

// TestPickerRecordingFanartOptionsRefresh confirms ?refresh=true
// scrubs the cache before re-extracting so the user gets a fresh
// random spread on Re-fetch.
func TestPickerRecordingFanartOptionsRefresh(t *testing.T) {
	t.Parallel()

	recID := frameFallbackRecordingID
	enc := &fakeScreenshotClient{}
	mediaInfo := `{"durationSeconds": 3600}`
	srv, fake := frameFallbackServer(t, enc, mediaInfo)

	// First call populates the cache.
	rr1 := httptest.NewRecorder()
	req1 := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/v1/recordings/"+strconv.FormatInt(recID, 10)+"/fanart-options", nil)
	srv.Handler().ServeHTTP(rr1, req1)
	require.Equal(t, http.StatusOK, rr1.Code)

	// Second call with refresh=true clears + re-extracts.
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/v1/recordings/"+strconv.FormatInt(recID, 10)+"/fanart-options?refresh=true", nil)
	srv.Handler().ServeHTTP(rr2, req2)
	require.Equal(t, http.StatusOK, rr2.Code)

	assert.Len(t, fake.Calls(), 2, "refresh=true should re-extract")
}

// TestPickerRecordingFanartOptionsEncoraWins covers the priority
// path: when Encora returns curated screenshots they win over the
// frame fallback (no extractor call fires).
func TestPickerRecordingFanartOptionsEncoraWins(t *testing.T) {
	t.Parallel()

	recID := frameFallbackRecordingID
	enc := &fakeScreenshotClient{
		urls: []string{"https://encora.example/sshot/01.jpg"},
	}
	mediaInfo := `{"durationSeconds": 3600}`
	srv, fake := frameFallbackServer(t, enc, mediaInfo)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/v1/recordings/"+strconv.FormatInt(recID, 10)+"/fanart-options", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		Options []struct {
			URL    string `json:"url"`
			Source string `json:"source"`
		} `json:"options"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	require.Len(t, body.Options, 1)
	assert.Equal(t, "encora", body.Options[0].Source)
	assert.Empty(t, fake.Calls(), "frame extractor must not run when encora has options")
}

// TestPickerOptionsRecordingNotFound asserts unknown recording ids
// trip a 404 before any upstream client is called.
func TestPickerOptionsRecordingNotFound(t *testing.T) {
	t.Parallel()

	enc := &fakeScreenshotClient{err: errors.New("must not be called")}
	fake := &fakeStagemediaImageClient{}
	srv := fixtureBackedServerWithClients(t, fake, enc)

	for _, path := range []string{
		"/api/v1/recordings/99999999/poster-options",
		"/api/v1/recordings/99999999/fanart-options",
	} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		srv.Handler().ServeHTTP(rr, req)
		assert.Equal(t, http.StatusNotFound, rr.Code, "GET %s -> %s", path, rr.Body.String())
	}
}
