package server_test

// picker_options_tmdb_test.go — tests for the TMDB picker source.
// Covers the three signal paths the helper must respect:
//
//   - Recording has a TMDB external id → /movie/{id}/images
//   - Recording has only an IMDB id → /find/{imdb_id} → /movie/{id}/images
//   - Recording has neither → no TMDB options (silent)
//
// All TMDB calls go through a fake satisfying server.TMDBClient — no
// real network.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/externalids"
	"github.com/nicolerenee/promptbook/internal/server"
	"github.com/nicolerenee/promptbook/internal/tmdb"
)

// fakeTMDBClient stubs server.TMDBClient. It records every Images +
// FindByIMDBID call so tests can assert which path the picker took.
type fakeTMDBClient struct {
	mu sync.Mutex
	// imagesByID is the response map keyed on tmdb id.
	imagesByID map[int64]tmdb.Images
	// findByIMDB is the resolved tmdb id keyed on imdb id (e.g.
	// "tt99999999" → 90181637). Unknown imdb ids return ok=false.
	findByIMDB map[string]int64
	// callsImages / callsFind record every call's argument so tests
	// can assert "this branch was taken" rather than "this output
	// matches" — picker tests care about the resolution flow.
	callsImages []int64
	callsFind   []string
	// imagesErr / findErr short-circuit before recording when set.
	imagesErr error
	findErr   error
}

func (f *fakeTMDBClient) Images(_ context.Context, tmdbID int64) (tmdb.Images, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.imagesErr != nil {
		return tmdb.Images{}, f.imagesErr
	}
	f.callsImages = append(f.callsImages, tmdbID)
	imgs, ok := f.imagesByID[tmdbID]
	if !ok {
		return tmdb.Images{}, tmdb.ErrNotFound
	}
	return imgs, nil
}

func (f *fakeTMDBClient) FindByIMDBID(_ context.Context, imdbID string) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.findErr != nil {
		return 0, false, f.findErr
	}
	f.callsFind = append(f.callsFind, imdbID)
	id, ok := f.findByIMDB[imdbID]
	if !ok {
		return 0, false, nil
	}
	return id, true, nil
}

// seedExternalID writes one external_ids row for the test recording
// (marigold, id 90100222 in the fixture). Pulled out of each test so the
// tests stay focused on the picker behaviour.
func seedTMDBExternalID(t *testing.T, srv *server.Server, recID int64, prov externalids.Provider, id string) {
	t.Helper()
	err := externalids.UpsertMany(t.Context(), srv.SQLDB(), []externalids.ExternalID{
		{RecordingID: recID, Provider: prov, ExternalID: id},
	})
	require.NoError(t, err)
}

// TestPickerRecordingFanartOptionsTMDBAppended covers the canonical
// path: recording has a TMDB external id, the picker's fanart
// response carries Encora screenshots first AND the TMDB backdrops
// appended afterwards.
func TestPickerRecordingFanartOptionsTMDBAppended(t *testing.T) {
	t.Parallel()

	const recID int64 = 90100222
	encFake := &fakeScreenshotClient{
		urls: []string{"https://encora.example/marigold-1.jpg"},
	}
	tmFake := &fakeTMDBClient{
		imagesByID: map[int64]tmdb.Images{
			90181637: {
				Backdrops: []tmdb.Image{
					{URL: "https://image.tmdb.org/back-A.jpg"},
					{URL: "https://image.tmdb.org/back-B.jpg"},
				},
			},
		},
	}
	srv := fixtureBackedServerWithAllClients(t, nil, encFake, tmFake)
	seedTMDBExternalID(t, srv, recID, externalids.ProviderTMDB, "90181637")

	body := requestFanartOptions(t, srv, recID, "")
	require.Len(t, body.Options, 3)
	assert.Equal(t, "encora", body.Options[0].Source)
	assert.Equal(t, "tmdb", body.Options[1].Source)
	assert.Equal(t, "tmdb", body.Options[2].Source)
	assert.Equal(t, "https://image.tmdb.org/back-A.jpg", body.Options[1].URL)
}

// TestPickerRecordingFanartOptionsTMDBIMDBFallback covers the IMDB →
// TMDB resolution path: recording has only an IMDB external id; the
// picker calls FindByIMDBID first, then /movie/{tmdbID}/images.
func TestPickerRecordingFanartOptionsTMDBIMDBFallback(t *testing.T) {
	t.Parallel()

	const recID int64 = 90100222
	tmFake := &fakeTMDBClient{
		findByIMDB: map[string]int64{"tt99999999": 90181637},
		imagesByID: map[int64]tmdb.Images{
			90181637: {
				Backdrops: []tmdb.Image{
					{URL: "https://image.tmdb.org/imdb-resolved.jpg"},
				},
			},
		},
	}
	srv := fixtureBackedServerWithAllClients(t, nil, nil, tmFake)
	seedTMDBExternalID(t, srv, recID, externalids.ProviderIMDB, "tt99999999")

	body := requestFanartOptions(t, srv, recID, "")
	// Encora has no client + no IMDB recording → no encora options.
	// TMDB-only path is fine.
	require.Len(t, body.Options, 1)
	assert.Equal(t, "tmdb", body.Options[0].Source)
	assert.Equal(t, "https://image.tmdb.org/imdb-resolved.jpg", body.Options[0].URL)
	assert.Equal(t, []string{"tt99999999"}, tmFake.callsFind)
	assert.Equal(t, []int64{90181637}, tmFake.callsImages)
}

// TestPickerRecordingFanartOptionsNoExternalIDsSkipsTMDB confirms a
// recording with only its back-fill Encora row gets NO TMDB options
// — the picker source is silent rather than rendering "no TMDB id"
// errors.
func TestPickerRecordingFanartOptionsNoExternalIDsSkipsTMDB(t *testing.T) {
	t.Parallel()

	const recID int64 = 90100222
	tmFake := &fakeTMDBClient{}
	srv := fixtureBackedServerWithAllClients(t, nil, nil, tmFake)
	// No TMDB / IMDB external id — only the Encora back-fill row.

	body := requestFanartOptions(t, srv, recID, "")
	// Encora client is nil + no curated screenshots → falls into the
	// frame-extract fallback path, which returns [] because no cache
	// is wired in this fixture.
	assert.Empty(t, body.Options)
	assert.Empty(t, tmFake.callsImages, "TMDB Images shouldn't be called when no TMDB/IMDB id")
	assert.Empty(t, tmFake.callsFind, "TMDB FindByIMDBID shouldn't be called either")
}

// TestPickerRecordingPosterOptionsTMDB covers the poster handler's
// TMDB path: the recording has a TMDB id, stagemedia returns nothing
// (or isn't configured), TMDB carries the poster list.
func TestPickerRecordingPosterOptionsTMDB(t *testing.T) {
	t.Parallel()

	const recID int64 = 90100222
	tmFake := &fakeTMDBClient{
		imagesByID: map[int64]tmdb.Images{
			90181637: {
				Posters: []tmdb.Image{
					{URL: "https://image.tmdb.org/poster-A.jpg"},
				},
			},
		},
	}
	srv := fixtureBackedServerWithAllClients(t, nil, nil, tmFake)
	seedTMDBExternalID(t, srv, recID, externalids.ProviderTMDB, "90181637")

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
	assert.Equal(t, "tmdb", body.Options[0].Source)
	assert.Equal(t, "https://image.tmdb.org/poster-A.jpg", body.Options[0].URL)
}

// requestFanartOptions issues the fanart-options request and decodes
// the response body. Refresh, when non-empty, is appended as a query
// param.
func requestFanartOptions(t *testing.T, srv *server.Server, recID int64, refresh string) struct {
	Options []struct {
		URL    string `json:"url"`
		Source string `json:"source"`
	} `json:"options"`
} {
	t.Helper()
	url := "/api/v1/recordings/" + strconv.FormatInt(recID, 10) + "/fanart-options"
	if refresh != "" {
		url += "?refresh=" + refresh
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		Options []struct {
			URL    string `json:"url"`
			Source string `json:"source"`
		} `json:"options"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	return body
}
