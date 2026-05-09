package storage_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/encora"
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
	db, err := storage.Open(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

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
