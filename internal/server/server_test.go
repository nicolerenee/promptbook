package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/server"
	"github.com/nicolerenee/promptbook/internal/storage"
	syncpkg "github.com/nicolerenee/promptbook/internal/sync"
)

const fixturesDir = "../encora/testdata"

// fixtureBackedServer returns a fully-wired *server.Server with a
// fixture-seeded SQLite cache behind it. Used for the API + page tests
// so each one starts from a known shape.
func fixtureBackedServer(t *testing.T) *server.Server {
	t.Helper()

	mux := http.NewServeMux()
	for path, file := range map[string]string{
		"/api/collection": "collection.json",
		"/api/wants":      "wants.json",
	} {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			b, err := os.ReadFile(filepath.Join(fixturesDir, file))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("X-RateLimit-Remaining", "25")
			_, _ = w.Write(b)
		})
	}
	upstream := httptest.NewServer(mux)
	t.Cleanup(upstream.Close)

	db, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	c, err := encora.New(encora.Options{BaseURL: upstream.URL, APIKey: "test"})
	require.NoError(t, err)
	_, err = syncpkg.Sync(t.Context(), c, db, syncpkg.Options{BurstReserve: 2})
	require.NoError(t, err)

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)
	return srv
}

func TestAPIHealth(t *testing.T) {
	t.Parallel()

	srv := fixtureBackedServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/health", nil)
	srv.Handler().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var got map[string]string
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	assert.Equal(t, "ok", got["status"])
}

func TestAPIRecordings(t *testing.T) {
	t.Parallel()

	srv := fixtureBackedServer(t)

	tests := []struct {
		name      string
		query     string
		minItems  int
		maxItems  int
		assertOne func(t *testing.T, items []map[string]any)
	}{
		{
			name:     "default_returns_records",
			query:    "?limit=200",
			minItems: 1,
			maxItems: 1000,
		},
		{
			name:     "owned_only_matches_collection_count",
			query:    "?owned=true&limit=200",
			minItems: 28,
			maxItems: 28,
		},
		{
			name:     "wants_only",
			query:    "?owned=false&limit=200",
			minItems: 14,
			maxItems: 14,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rr := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(
				t.Context(), http.MethodGet, "/api/v1/recordings"+tt.query, nil,
			)
			srv.Handler().ServeHTTP(rr, req)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

			var body struct {
				Items []map[string]any `json:"items"`
			}
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
			assert.GreaterOrEqual(t, len(body.Items), tt.minItems)
			assert.LessOrEqual(t, len(body.Items), tt.maxItems)
		})
	}
}

func TestAPIRecordingByID(t *testing.T) {
	t.Parallel()

	srv := fixtureBackedServer(t)

	t.Run("found", func(t *testing.T) {
		t.Parallel()
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/recordings/90100222", nil)
		srv.Handler().ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code)
		assert.Contains(t, rr.Body.String(), `"show":"Marigold Junction"`)
	})

	t.Run("not_found", func(t *testing.T) {
		t.Parallel()
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/recordings/99999999", nil)
		srv.Handler().ServeHTTP(rr, req)
		assert.Equal(t, http.StatusNotFound, rr.Code)
	})

	t.Run("bad_id", func(t *testing.T) {
		t.Parallel()
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/recordings/marigold", nil)
		srv.Handler().ServeHTTP(rr, req)
		assert.Equal(t, http.StatusBadRequest, rr.Code)
	})
}

func TestAPISyncRuns(t *testing.T) {
	t.Parallel()

	srv := fixtureBackedServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/sync/runs", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), `"kind":"all"`)
}

func TestPagesSmoke(t *testing.T) {
	t.Parallel()

	srv := fixtureBackedServer(t)

	tests := []struct {
		name   string
		path   string
		expect string
	}{
		{name: "home", path: "/", expect: "Promptbook"},
		{name: "wants", path: "/wants", expect: "wishlist"},
		{name: "sync", path: "/sync", expect: "sync runs"},
		{name: "recording_detail", path: "/recordings/90100222", expect: "Marigold"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rr := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.path, nil)
			srv.Handler().ServeHTTP(rr, req)
			require.Equal(t, http.StatusOK, rr.Code, "page %s body=%s", tt.path, rr.Body.String())
			assert.Contains(t, rr.Body.String(), tt.expect)
		})
	}
}

func TestServeStartCancels(t *testing.T) {
	t.Parallel()

	srv := fixtureBackedServer(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel before Start so it returns immediately
	require.NoError(t, srv.Start(ctx, "127.0.0.1:0"))
}
