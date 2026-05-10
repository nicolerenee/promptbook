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
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/encora"
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

	srv, err := server.New(server.Options{
		DB:                db,
		Stagemedia:        sm,
		EncoraScreenshots: enc,
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
