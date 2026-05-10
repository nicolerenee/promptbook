package storage_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/storage"
	"github.com/nicolerenee/promptbook/internal/sync"
)

func TestParseRecordingID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    int64
		wantErr bool
	}{
		{name: "ok", input: "90100222", want: 90100222},
		{name: "negative", input: "-1", wantErr: true},
		{name: "zero", input: "0", wantErr: true},
		{name: "non-numeric", input: "marigold", wantErr: true},
		{name: "empty", input: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := storage.ParseRecordingID(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestLoadRecording(t *testing.T) {
	t.Parallel()

	srv := newFixtureServer(t)
	t.Cleanup(srv.Close)

	dbPath := filepath.Join(t.TempDir(), "promptbook.db")
	sqlDB, db, err := storage.OpenEnt(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	c, err := encora.New(encora.Options{BaseURL: srv.URL, APIKey: "test"})
	require.NoError(t, err)

	_, err = sync.Sync(context.Background(), c, db, sync.Options{BurstReserve: 2})
	require.NoError(t, err)

	t.Run("found_in_collection", func(t *testing.T) {
		t.Parallel()
		loaded, lerr := storage.LoadRecording(t.Context(), db, 90100222)
		require.NoError(t, lerr)
		assert.Equal(t, "Marigold Junction", loaded.Recording.Show)
		assert.Equal(t, "Broadway", loaded.Recording.Tour)
		assert.True(t, loaded.InCollection, "marigold is owned in fixtures")
		assert.False(t, loaded.InWants, "marigold is owned, not wanted")
		assert.NotEmpty(t, loaded.Format)
	})

	t.Run("found_in_wants", func(t *testing.T) {
		t.Parallel()
		// Chasing Polaris 90001143 is in wants.json fixture.
		loaded, lerr := storage.LoadRecording(t.Context(), db, 90001143)
		require.NoError(t, lerr)
		assert.Equal(t, "Chasing Polaris", loaded.Recording.Show)
		assert.False(t, loaded.InCollection)
		assert.True(t, loaded.InWants)
	})

	t.Run("not_found", func(t *testing.T) {
		t.Parallel()
		_, lerr := storage.LoadRecording(t.Context(), db, 99999999)
		require.Error(t, lerr)
		assert.ErrorIs(t, lerr, storage.ErrRecordingNotFound)
	})
}

func TestLoadRecordingPopulatesVersions(t *testing.T) {
	t.Parallel()

	ctx, db := openTestDB(t)
	const (
		showID      int64 = 90001
		recordingID int64 = 90002
	)
	seedShow(ctx, t, db, showID, "Greenwich Beacon")
	seedRecordingWithRawJSON(ctx, t, db, recordingID, showID, encora.Recording{ID: recordingID})

	// Seed real MediaInfoJSON so the new release-format compose path
	// has codec / quality / size data to render. Both versions are
	// MKV with HEVC video; the 2160p row reports 1920x1080 — wait,
	// 2160p — let the test data pin both heights so the multi-version
	// bracketed render is deterministic.
	mi2160 := `{"container":"MKV","videoCodec":"hevc","width":3840,"height":2160,` +
		`"audioStreams":[{"codec":"aac"}]}`
	mi1080 := `{"container":"MKV","videoCodec":"h264","width":1920,"height":1080,` +
		`"audioStreams":[{"codec":"aac"}]}`
	require.NoError(t, storage.UpsertVersion(ctx, db, storage.RecordingVersion{
		RecordingID:   recordingID,
		FilePath:      "/store/greenwich-beacon/2160p.mkv",
		FileSizeBytes: 40 * 1024 * 1024 * 1024,
		FormatLabel:   "MKV 2160p hevc",
		MediaInfoJSON: mi2160,
	}))
	require.NoError(t, storage.UpsertVersion(ctx, db, storage.RecordingVersion{
		RecordingID:   recordingID,
		FilePath:      "/store/greenwich-beacon/1080p.mkv",
		FileSizeBytes: 5 * 1024 * 1024 * 1024,
		FormatLabel:   "MKV 1080p h264",
		MediaInfoJSON: mi1080,
	}))

	loaded, err := storage.LoadRecording(ctx, db, recordingID)
	require.NoError(t, err)
	require.NotNil(t, loaded.Versions)
	assert.Len(t, loaded.Versions, 2)
	assert.Equal(t, storage.ComputeFormatString(loaded.Versions), loaded.LocalFormatString)
	// Multi-version render: each version bracketed, sorted by height
	// descending (2160p first), single-space separator.
	assert.Equal(t,
		"[MKV - x265 / AAC - 2160p - 40.00 GB] [MKV - x264 / AAC - 1080p - 5.00 GB]",
		loaded.LocalFormatString)
}

func TestLoadRecordingPopulatesCast(t *testing.T) {
	t.Parallel()

	ctx, db := openTestDB(t)
	const (
		showID        int64 = 90101
		recordingID   int64 = 90102
		performerID   int64 = 90103
		characterID   int64 = 90104
		characterRank int   = 3
	)
	seedShow(ctx, t, db, showID, "Halcyon Crossing")

	status := &encora.CastStatus{Label: "Understudy", Abbreviation: "u/s"}
	rec := encora.Recording{
		ID: recordingID,
		Cast: []encora.CastEntry{{
			Performer: encora.Performer{ID: performerID, Name: "Casper Bly"},
			Character: encora.Character{ID: characterID, Name: "Old Mariner", Order: characterRank},
			Status:    status,
		}},
	}
	seedRecordingWithRawJSON(ctx, t, db, recordingID, showID, rec)

	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, storage.UpsertPerformer(ctx, db, storage.Performer{
		PerformerID: performerID,
		Name:        "Casper Bly",
		Slug:        "casper-bly",
		URL:         "https://encora.example/p/casper-bly",
		LastSeenAt:  now,
	}))
	require.NoError(t, storage.UpsertCharacter(ctx, db, storage.Character{
		CharacterID: characterID,
		Name:        "Old Mariner",
		Slug:        "old-mariner",
		URL:         "https://encora.example/c/old-mariner",
		LastSeenAt:  now,
	}))

	loaded, err := storage.LoadRecording(ctx, db, recordingID)
	require.NoError(t, err)
	require.Len(t, loaded.Cast, 1)

	got := loaded.Cast[0]
	assert.Equal(t, performerID, got.Performer.PerformerID)
	assert.Equal(t, "Casper Bly", got.Performer.Name)
	assert.Equal(t, "casper-bly", got.Performer.Slug)
	assert.Equal(t, characterID, got.Character.CharacterID)
	assert.Equal(t, "Old Mariner", got.Character.Name)
	assert.Equal(t, characterRank, got.Order)
	require.NotNil(t, got.Status)
	assert.Equal(t, "u/s", got.Status.Abbreviation)
}

func TestLoadRecordingMissingPerformerSilent(t *testing.T) {
	t.Parallel()

	ctx, db := openTestDB(t)
	const (
		showID      int64 = 90201
		recordingID int64 = 90202
		performerID int64 = 90203
		characterID int64 = 90204
	)
	seedShow(ctx, t, db, showID, "Six")

	rec := encora.Recording{
		ID: recordingID,
		Cast: []encora.CastEntry{{
			Performer: encora.Performer{ID: performerID, Name: "Legacy Actor"},
			Character: encora.Character{ID: characterID, Name: "The Heiress"},
		}},
	}
	seedRecordingWithRawJSON(ctx, t, db, recordingID, showID, rec)

	// Deliberately do not seed performers/characters — simulating legacy
	// rows that haven't been promoted to the people tables yet.

	loaded, err := storage.LoadRecording(ctx, db, recordingID)
	require.NoError(t, err)
	require.Len(t, loaded.Cast, 1)

	got := loaded.Cast[0]
	assert.Equal(t, storage.Performer{}, got.Performer, "missing performer leaves zero value")
	assert.Equal(t, storage.Character{}, got.Character, "missing character leaves zero value")
}

// seedRecordingWithRawJSON inserts a recording row whose raw_json column
// contains the marshaled encora.Recording. Use this when a test needs
// LoadRecording to decode a non-trivial Recording (e.g. with cast
// entries) — seedRecording writes '{}' and won't satisfy that.
func seedRecordingWithRawJSON(
	ctx context.Context,
	t *testing.T,
	db *ent.Client,
	recordingID, showID int64,
	rec encora.Recording,
) {
	t.Helper()
	raw, err := json.Marshal(rec)
	require.NoError(t, err)
	require.NoError(t, db.Recording.Create().
		SetID(recordingID).
		SetShowID(showID).
		SetRawJSON(string(raw)).
		Exec(ctx))
}

// newFixtureServer is a duplicate of the sync test's fixtureServer but
// scoped to this package so tests don't have a cross-package import cycle.
func newFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	routes := map[string]string{
		"/api/collection": "../encora/testdata/collection.json",
		"/api/wants":      "../encora/testdata/wants.json",
	}

	mux := http.NewServeMux()
	for path, file := range routes {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			b, err := os.ReadFile(file)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-RateLimit-Limit", "30")
			w.Header().Set("X-RateLimit-Remaining", "25")
			_, _ = w.Write(b)
		})
	}
	return httptest.NewServer(mux)
}
