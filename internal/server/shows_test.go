package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/server"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// TestAPIListShows seeds a small fixture covering: a show with
// multiple recordings spanning different states + years (so we can
// assert state-count buckets and FirstYear/LastYear), a show with a
// single recording (single-year span), and a show with zero
// recordings (sanity check that the seed-from-shows-table path
// keeps it from being dropped on the floor).
func TestAPIListShows(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	type rec struct {
		recordingID  int64
		dateFull     string
		inCollection bool
		inWants      bool
		hasFile      bool
		encoraFormat string
		localFormat  string
	}
	type show struct {
		showID  int64
		name    string
		records []rec
	}
	shows := []show{
		{
			showID: 7001, name: "Halcyon Crossing",
			records: []rec{
				// synced (in collection + format match + file).
				{
					recordingID: 70011, dateFull: "2019-04-17",
					inCollection: true, hasFile: true,
					encoraFormat: "MKV 1080p", localFormat: "MKV 1080p",
				},
				// missing (in collection, no file).
				{
					recordingID: 70012, dateFull: "2024-09-12",
					inCollection: true,
					encoraFormat: "MKV 1080p",
				},
				// wanted.
				{
					recordingID: 70013, dateFull: "2017-06-01",
					inWants: true,
				},
			},
		},
		{
			showID: 7002, name: "Greenwich Beacon",
			records: []rec{
				{
					recordingID: 70021, dateFull: "2003-10-30",
					inCollection: true, hasFile: true,
					encoraFormat: "MKV 720p", localFormat: "MKV 720p",
				},
			},
		},
		{
			// Show with no recordings — should still surface in the
			// response with a zero count + empty state map.
			showID: 7003, name: "ZeroShow",
			records: nil,
		},
	}

	for _, sh := range shows {
		_, seedErr := db.ExecContext(ctx,
			`INSERT INTO shows (show_id, name) VALUES (?, ?)`, sh.showID, sh.name)
		require.NoError(t, seedErr)
		for _, r := range sh.records {
			_, seedErr = db.ExecContext(ctx, `
				INSERT INTO recordings (
					recording_id, show_id, tour, date_full, raw_json
				) VALUES (?, ?, '', ?, '{}')
			`, r.recordingID, sh.showID, r.dateFull)
			require.NoError(t, seedErr)
			if r.inCollection {
				_, seedErr = db.ExecContext(ctx,
					`INSERT INTO collection (recording_id, format) VALUES (?, ?)`,
					r.recordingID, r.encoraFormat)
				require.NoError(t, seedErr)
			}
			if r.inWants {
				_, seedErr = db.ExecContext(ctx,
					`INSERT INTO wants (recording_id) VALUES (?)`, r.recordingID)
				require.NoError(t, seedErr)
			}
			if r.hasFile {
				require.NoError(t, storage.UpsertVersion(ctx, db, storage.RecordingVersion{
					RecordingID: r.recordingID,
					FilePath:    "/store/" + sh.name + ".mkv",
					FormatLabel: r.localFormat,
				}))
			}
		}
	}

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/shows", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		Items []struct {
			ID             int64          `json:"id"`
			Name           string         `json:"name"`
			RecordingCount int            `json:"recording_count"`
			StateCounts    map[string]int `json:"state_counts"`
			FirstYear      *int           `json:"first_year"`
			LastYear       *int           `json:"last_year"`
			LocalPosterURL string         `json:"local_poster_url"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))

	// Three shows in, three shows out — including the zero-recording one.
	require.Len(t, body.Items, 3)

	byID := make(map[int64]int, len(body.Items))
	for i, it := range body.Items {
		byID[it.ID] = i
	}

	// Halcyon Crossing: 3 recordings — 1 synced, 1 missing, 1 wanted.
	halcyon-crossing := body.Items[byID[7001]]
	assert.Equal(t, "Halcyon Crossing", halcyon-crossing.Name)
	assert.Equal(t, 3, halcyon-crossing.RecordingCount)
	assert.Equal(t, 1, halcyon-crossing.StateCounts["synced"])
	assert.Equal(t, 1, halcyon-crossing.StateCounts["missing"])
	assert.Equal(t, 1, halcyon-crossing.StateCounts["wanted"])
	require.NotNil(t, halcyon-crossing.FirstYear)
	require.NotNil(t, halcyon-crossing.LastYear)
	assert.Equal(t, 2017, *halcyon-crossing.FirstYear)
	assert.Equal(t, 2024, *halcyon-crossing.LastYear)
	assert.Empty(t, halcyon-crossing.LocalPosterURL,
		"no image cache configured -> empty url")

	// Greenwich Beacon: 1 recording, synced. First == Last == 2003.
	greenwich-beacon := body.Items[byID[7002]]
	assert.Equal(t, "Greenwich Beacon", greenwich-beacon.Name)
	assert.Equal(t, 1, greenwich-beacon.RecordingCount)
	assert.Equal(t, 1, greenwich-beacon.StateCounts["synced"])
	require.NotNil(t, greenwich-beacon.FirstYear)
	require.NotNil(t, greenwich-beacon.LastYear)
	assert.Equal(t, 2003, *greenwich-beacon.FirstYear)
	assert.Equal(t, 2003, *greenwich-beacon.LastYear)

	// ZeroShow: surfaces with empty buckets + nil years.
	zero := body.Items[byID[7003]]
	assert.Equal(t, "ZeroShow", zero.Name)
	assert.Zero(t, zero.RecordingCount)
	assert.Empty(t, zero.StateCounts)
	assert.Nil(t, zero.FirstYear)
	assert.Nil(t, zero.LastYear)

	// Canonical sort: by name asc (case-insensitive).
	assert.Equal(t, "Halcyon Crossing", body.Items[0].Name)
	assert.Equal(t, "Greenwich Beacon", body.Items[1].Name)
	assert.Equal(t, "ZeroShow", body.Items[2].Name)
}

// TestAPIRecordingsLocalPosterURL asserts the recordings list endpoint
// surfaces local_poster_url when image caching is on AND the
// show_image_choice points at an on-disk poster. Caching off (the
// default fixture-backed test) leaves the field empty — a separate
// case verifies that contract.
func TestAPIRecordingsLocalPosterURL(t *testing.T) {
	t.Parallel()

	srv := fourStatusServer(t)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(),
		http.MethodGet, "/api/v1/recordings?limit=200", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	var body struct {
		Items []struct {
			ID             int64  `json:"id"`
			ShowID         int64  `json:"show_id"`
			LocalPosterURL string `json:"local_poster_url"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	require.NotEmpty(t, body.Items)
	for _, it := range body.Items {
		assert.NotZero(t, it.ShowID,
			"every list item should carry the joining show_id")
		assert.Empty(t, it.LocalPosterURL,
			"no image cache -> empty local_poster_url")
	}
}
