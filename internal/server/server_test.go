package server_test

import (
	"context"
	gosql "database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/server"
	"github.com/nicolerenee/promptbook/internal/stagemedia"
	"github.com/nicolerenee/promptbook/internal/storage"
	syncpkg "github.com/nicolerenee/promptbook/internal/sync"
)

// seedRecording inserts a minimal recordings row with the given encora
// payload as raw_json so LoadRecording round-trips it back. Used by the
// detail-page tests that need to control the NFT block / cast directly
// instead of going through the fixture sync path.
func seedRecording(t *testing.T, db *gosql.DB, r encora.Recording) {
	t.Helper()
	_, err := db.ExecContext(t.Context(),
		`INSERT OR IGNORE INTO shows (show_id, name) VALUES (?, ?)`,
		r.Metadata.ShowID, r.Show)
	require.NoError(t, err)
	rawJSON, err := json.Marshal(r)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `
		INSERT INTO recordings (
			recording_id, show_id, tour, date_full, raw_json
		) VALUES (?, ?, ?, ?, ?)
	`, r.ID, r.Metadata.ShowID, r.Tour, r.Date.FullDate, string(rawJSON))
	require.NoError(t, err)
}

const fixturesDir = "../encora/testdata"

// fixtureBackedServer returns a fully-wired *server.Server with a
// fixture-seeded SQLite cache behind it. Used for the API + page tests
// so each one starts from a known shape.
func fixtureBackedServer(t *testing.T) *server.Server {
	t.Helper()

	mux := http.NewServeMux()
	for path, file := range map[string]string{
		"/api/profile":    "profile.json",
		"/api/collection": "collection.json",
		"/api/wants":      "wants.json",
	} {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			b, err := os.ReadFile(filepath.Join(fixturesDir, file))
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

func TestAPIProfile(t *testing.T) {
	t.Parallel()

	t.Run("found", func(t *testing.T) {
		t.Parallel()
		srv := fixtureBackedServer(t)
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/profile", nil)
		srv.Handler().ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

		var got map[string]any
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
		assert.Equal(t, "fixturearchive", got["username"])
		assert.InEpsilon(t, float64(90007787), got["encora_id"], 0.0001)
		assert.InEpsilon(t, float64(28), got["recordings_count"], 0.0001)
		assert.InEpsilon(t, float64(14), got["wants_count"], 0.0001)
	})

	t.Run("not_synced", func(t *testing.T) {
		t.Parallel()
		// Bare DB without sync running — no profile row.
		db, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })

		srv, err := server.New(server.Options{DB: db})
		require.NoError(t, err)

		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/profile", nil)
		srv.Handler().ServeHTTP(rr, req)
		assert.Equal(t, http.StatusNotFound, rr.Code)
	})
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

// TestPagesSmoke asserts the empty-shell page templates render
// successfully. Inline data assertions are gone now that the pages are
// JS-driven — the shell is the only thing the server rendering layer is
// responsible for.
func TestPagesSmoke(t *testing.T) {
	t.Parallel()

	srv := fixtureBackedServer(t)

	tests := []struct {
		name      string
		path      string
		dataPage  string
		titleText string
	}{
		{name: "home", path: "/", dataPage: "library", titleText: "Library"},
		{name: "wants", path: "/wants", dataPage: "wants", titleText: "Wants"},
		{name: "sync", path: "/sync", dataPage: "sync", titleText: "Sync"},
		{
			name: "recording_detail", path: "/recordings/90100222",
			dataPage: "recording", titleText: "Recording",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rr := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.path, nil)
			srv.Handler().ServeHTTP(rr, req)
			require.Equal(t, http.StatusOK, rr.Code, "page %s body=%s", tt.path, rr.Body.String())
			body := rr.Body.String()
			assert.Contains(t, body, `data-page="`+tt.dataPage+`"`,
				"shell must stamp data-page attribute")
			assert.Contains(t, body, "pb-side", "sidebar must render")
			assert.Contains(t, body, tt.titleText, "H1 page title must render")
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

// fourStatusServer builds a server backed by a freshly-migrated SQLite DB
// seeded with exactly four recordings — one each in synced,
// format_mismatch, missing, and wanted state. Orphan is omitted because
// it requires a recording row with no FK from collection/wants, which is
// the most awkward to seed.
func fourStatusServer(t *testing.T) *server.Server {
	t.Helper()
	ctx := t.Context()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	type seed struct {
		showID, recordingID int64
		showName            string
		status              storage.Status
		inCollection        bool
		inWants             bool
		hasFile             bool
		encoraFormat        string
		localFormat         string
	}
	seeds := []seed{
		{
			showID: 9001, recordingID: 90001, showName: "SyncedShow",
			status: storage.StatusSynced, inCollection: true, hasFile: true,
			encoraFormat: "MKV 1080p", localFormat: "MKV 1080p",
		},
		{
			showID: 9002, recordingID: 90002, showName: "MismatchShow",
			status: storage.StatusFormatMismatch, inCollection: true, hasFile: true,
			encoraFormat: "MKV 1080p", localFormat: "MKV 720p",
		},
		{
			showID: 9003, recordingID: 90003, showName: "MissingShow",
			status: storage.StatusMissing, inCollection: true,
			encoraFormat: "MKV 1080p",
		},
		{
			showID: 9004, recordingID: 90004, showName: "WantedShow",
			status: storage.StatusWanted, inWants: true,
		},
	}

	for _, s := range seeds {
		_, seedErr := db.ExecContext(ctx,
			`INSERT INTO shows (show_id, name) VALUES (?, ?)`, s.showID, s.showName)
		require.NoError(t, seedErr)
		_, seedErr = db.ExecContext(ctx, `
			INSERT INTO recordings (
				recording_id, show_id, tour, date_full, raw_json
			) VALUES (?, ?, '', '', '{}')
		`, s.recordingID, s.showID)
		require.NoError(t, seedErr)
		if s.inCollection {
			_, seedErr = db.ExecContext(ctx,
				`INSERT INTO collection (recording_id, format) VALUES (?, ?)`,
				s.recordingID, s.encoraFormat)
			require.NoError(t, seedErr)
		}
		if s.inWants {
			_, seedErr = db.ExecContext(ctx,
				`INSERT INTO wants (recording_id) VALUES (?)`, s.recordingID)
			require.NoError(t, seedErr)
		}
		if s.hasFile {
			require.NoError(t, storage.UpsertVersion(ctx, db, storage.RecordingVersion{
				RecordingID: s.recordingID,
				FilePath:    "/store/" + s.showName + ".mkv",
				FormatLabel: s.localFormat,
			}))
		}
	}

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	return srv
}

func TestAPIRecordingsStatusFilter(t *testing.T) {
	t.Parallel()

	srv := fourStatusServer(t)

	tests := []struct {
		name      string
		query     string
		wantCount int
		wantStat  []string // statuses we expect to find in items.
	}{
		{
			name:      "no filter returns all four",
			query:     "?limit=200",
			wantCount: 4,
			wantStat:  []string{"synced", "format_mismatch", "missing", "wanted"},
		},
		{
			name:      "single status synced",
			query:     "?status=synced&limit=200",
			wantCount: 1,
			wantStat:  []string{"synced"},
		},
		{
			name:      "comma-separated missing,wanted",
			query:     "?status=missing,wanted&limit=200",
			wantCount: 2,
			wantStat:  []string{"missing", "wanted"},
		},
		{
			name:      "status overrides legacy owned",
			query:     "?status=wanted&owned=true&limit=200",
			wantCount: 1,
			wantStat:  []string{"wanted"},
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
			assert.Len(t, body.Items, tt.wantCount)

			gotStatuses := make(map[string]bool, len(body.Items))
			for _, item := range body.Items {
				if s, ok := item["status"].(string); ok {
					gotStatuses[s] = true
				}
			}
			for _, want := range tt.wantStat {
				assert.True(t, gotStatuses[want],
					"expected status %q in results, got %v", want, gotStatuses)
			}
		})
	}
}

func TestServerStagemediaAccessor(t *testing.T) {
	t.Parallel()

	db, openErr := storage.Open(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, openErr)
	t.Cleanup(func() { _ = db.Close() })

	t.Run("nil when not configured", func(t *testing.T) {
		t.Parallel()
		srv, err := server.New(server.Options{DB: db})
		require.NoError(t, err)
		assert.Nil(t, srv.Stagemedia())
	})

	t.Run("returns the configured client", func(t *testing.T) {
		t.Parallel()
		// Sentinel: empty Client value is enough since the test only
		// verifies pointer-equality plumbing — no methods are invoked.
		sentinel := &stagemedia.Client{}
		srv, err := server.New(server.Options{DB: db, Stagemedia: sentinel})
		require.NoError(t, err)
		assert.Same(t, sentinel, srv.Stagemedia())
	})
}

func TestAPIQueueEmpty(t *testing.T) {
	t.Parallel()

	db, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/queue", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Empty(t, body.Items)
}

func TestAPIQueueLists(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	suggested := int64(90100222)
	_, err = storage.EnqueueFile(ctx, db, storage.QueueEntry{
		FilePath:            "/incoming/marigold.mkv",
		FileSizeBytes:       1024,
		SuggestedConfidence: storage.ConfidenceLow,
		Notes:               "no match",
	})
	require.NoError(t, err)
	_, err = storage.EnqueueFile(ctx, db, storage.QueueEntry{
		FilePath:             "/incoming/greenwich-beacon.mkv",
		FileSizeBytes:        2048,
		SuggestedRecordingID: &suggested,
		SuggestedConfidence:  storage.ConfidenceHigh,
	})
	require.NoError(t, err)

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/queue", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Len(t, body.Items, 2)

	paths := make(map[string]bool, len(body.Items))
	for _, item := range body.Items {
		if p, ok := item["file_path"].(string); ok {
			paths[p] = true
		}
	}
	assert.True(t, paths["/incoming/marigold.mkv"])
	assert.True(t, paths["/incoming/greenwich-beacon.mkv"])
}

// TestPagesQueueShell asserts the queue page renders the empty shell
// regardless of queue contents — the table itself is now drawn by
// /static/queue.js, which fetches /api/v1/queue.
func TestPagesQueueShell(t *testing.T) {
	t.Parallel()

	db, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/queue", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	body := rr.Body.String()
	assert.Contains(t, body, `data-page="queue"`)
	assert.Contains(t, body, "Queue")
}

func TestAPIPeopleList(t *testing.T) {
	t.Parallel()

	srv := fixtureBackedServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/people?limit=200", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.NotEmpty(t, body.Items, "expected at least one performer in fixture-backed library")

	for _, item := range body.Items {
		assert.NotEmpty(t, item["name"], "every person row must have a name")
		assert.GreaterOrEqual(t, item["recording_count"], float64(1),
			"every person row must have at least one recording credit")
	}
}

func TestAPIPerson(t *testing.T) {
	t.Parallel()

	srv := fixtureBackedServer(t)

	t.Run("found", func(t *testing.T) {
		t.Parallel()
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/people/90001001", nil)
		srv.Handler().ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

		var body struct {
			PerformerID int64  `json:"performer_id"`
			Name        string `json:"name"`
			Recordings  []struct {
				ID   int64  `json:"id"`
				Show string `json:"show"`
			} `json:"recordings"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
		assert.Equal(t, int64(90001001), body.PerformerID)
		assert.Equal(t, "Avery Morrison", body.Name)
		assert.NotEmpty(t, body.Recordings, "Avery Morrison should have at least one recording")
	})

	t.Run("not_found", func(t *testing.T) {
		t.Parallel()
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/people/99999999", nil)
		srv.Handler().ServeHTTP(rr, req)
		assert.Equal(t, http.StatusNotFound, rr.Code)
	})

	t.Run("bad_id", func(t *testing.T) {
		t.Parallel()
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/people/brian", nil)
		srv.Handler().ServeHTTP(rr, req)
		assert.Equal(t, http.StatusBadRequest, rr.Code)
	})
}

// TestPagesPeople asserts the people-list and person-detail pages
// render the empty shell — the underlying performer rows are populated
// client-side by /static/people.js / person.js fetching the JSON API.
func TestPagesPeople(t *testing.T) {
	t.Parallel()

	srv := fixtureBackedServer(t)

	t.Run("list", func(t *testing.T) {
		t.Parallel()
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/people", nil)
		srv.Handler().ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assert.Contains(t, rr.Body.String(), `data-page="people"`)
	})

	t.Run("detail", func(t *testing.T) {
		t.Parallel()
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/people/90001001", nil)
		srv.Handler().ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		body := rr.Body.String()
		assert.Contains(t, body, `data-page="person"`)
		assert.Contains(t, body, `data-person-id="90001001"`,
			"person detail shell must stamp the performer id")
	})
}

func TestAPIHistoryEmpty(t *testing.T) {
	t.Parallel()

	db, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/history", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Empty(t, body.Items)
}

func TestAPIHistoryLists(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for _, e := range []storage.HistoryEvent{
		{OccurredAt: base.Add(1 * time.Hour), Kind: storage.HistoryKindIngest, Summary: "ingested foo"},
		{OccurredAt: base.Add(2 * time.Hour), Kind: storage.HistoryKindRename, Summary: "renamed foo"},
		{OccurredAt: base.Add(3 * time.Hour), Kind: storage.HistoryKindNFOWrite, Summary: "wrote nfo"},
	} {
		_, recErr := storage.RecordEvent(ctx, db, e)
		require.NoError(t, recErr)
	}

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/history", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		Items []struct {
			Kind    string `json:"kind"`
			Summary string `json:"summary"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	require.Len(t, body.Items, 3)
	// DESC by occurred_at: newest first.
	assert.Equal(t, storage.HistoryKindNFOWrite, body.Items[0].Kind)
	assert.Equal(t, storage.HistoryKindRename, body.Items[1].Kind)
	assert.Equal(t, storage.HistoryKindIngest, body.Items[2].Kind)
}

func TestAPIHistoryFilterByKind(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for _, e := range []storage.HistoryEvent{
		{OccurredAt: base.Add(1 * time.Hour), Kind: storage.HistoryKindIngest, Summary: "ingested foo"},
		{OccurredAt: base.Add(2 * time.Hour), Kind: storage.HistoryKindRename, Summary: "renamed foo"},
		{OccurredAt: base.Add(3 * time.Hour), Kind: storage.HistoryKindNFOWrite, Summary: "wrote nfo"},
	} {
		_, recErr := storage.RecordEvent(ctx, db, e)
		require.NoError(t, recErr)
	}

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/history?kind=ingest", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		Items []struct {
			Kind    string `json:"kind"`
			Summary string `json:"summary"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	require.Len(t, body.Items, 1)
	assert.Equal(t, storage.HistoryKindIngest, body.Items[0].Kind)
	assert.Equal(t, "ingested foo", body.Items[0].Summary)
}

// TestPagesHistory asserts the history page renders the empty shell.
// The actual events are populated client-side by /static/history.js.
func TestPagesHistory(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/history", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	body := rr.Body.String()
	assert.Contains(t, body, `data-page="history"`)
	assert.Contains(t, body, "History")
}

// TestPagesHomeShell asserts the library page renders the empty shell.
// Status filtering is now client-side via library.js parsing the
// ?status= URL param, so the server-side test only verifies the shell
// markup. The data contract (filtering against the JSON API) is still
// covered by TestAPIRecordingsStatusFilter.
func TestPagesHomeShell(t *testing.T) {
	t.Parallel()

	srv := fourStatusServer(t)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/?status=missing", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	body := rr.Body.String()
	assert.Contains(t, body, `data-page="library"`)
	assert.Contains(t, body, "pb-side")
}

// seedMismatchRecording inserts a minimal recordings row plus the
// optional collection/wants/version rows the four-cases mismatch
// fixture exercises. It mirrors fourStatusServer's local seed helper
// but is reusable from the mismatch test suite.
func seedMismatchRecording(
	t *testing.T,
	db *gosql.DB,
	showID, recordingID int64,
	showName string,
	inCollection, inWants, hasFile bool,
	encoraFormat, localFormat string,
) {
	t.Helper()
	ctx := t.Context()
	_, err := db.ExecContext(ctx,
		`INSERT INTO shows (show_id, name) VALUES (?, ?)`, showID, showName)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO recordings (
			recording_id, show_id, tour, date_full, raw_json
		) VALUES (?, ?, '', '', '{}')
	`, recordingID, showID)
	require.NoError(t, err)
	if inCollection {
		_, err = db.ExecContext(ctx,
			`INSERT INTO collection (recording_id, format) VALUES (?, ?)`,
			recordingID, encoraFormat)
		require.NoError(t, err)
	}
	if inWants {
		_, err = db.ExecContext(ctx,
			`INSERT INTO wants (recording_id) VALUES (?)`, recordingID)
		require.NoError(t, err)
	}
	if hasFile {
		require.NoError(t, storage.UpsertVersion(ctx, db, storage.RecordingVersion{
			RecordingID: recordingID,
			FilePath:    "/store/" + showName + ".mkv",
			FormatLabel: localFormat,
		}))
	}
}

func TestAPIMismatchesEmpty(t *testing.T) {
	t.Parallel()

	db, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/mismatches", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Empty(t, body.Items)
}

// TestAPIMismatchesEnumerates seeds four recordings — one synced, one
// orphan (file present, not in collection or wants), one in-collection
// without a backing file, and one wants-without-file — and asserts the
// endpoint returns exactly the three non-Synced rows.
func TestAPIMismatchesEnumerates(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	// synced: in collection + matching local format.
	seedMismatchRecording(t, db, 9101, 91001, "SyncedShow",
		true, false, true, "MKV 1080p", "MKV 1080p")
	// orphan: file present, not in collection or wants.
	seedMismatchRecording(t, db, 9102, 91002, "OrphanShow",
		false, false, true, "", "MKV 720p")
	// missing: in collection, no local file.
	seedMismatchRecording(t, db, 9103, 91003, "MissingShow",
		true, false, false, "MKV 1080p", "")
	// wanted: in wants, no local file.
	seedMismatchRecording(t, db, 9104, 91004, "WantedShow",
		false, true, false, "", "")

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/mismatches", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		Items []struct {
			Type        string `json:"type"`
			RecordingID int64  `json:"recording_id"`
			Show        string `json:"show"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	require.Len(t, body.Items, 3)

	byType := make(map[string]int64, len(body.Items))
	for _, item := range body.Items {
		byType[item.Type] = item.RecordingID
	}
	assert.Equal(t, int64(91002), byType["add_to_collection"])
	assert.Equal(t, int64(91003), byType["missing_file"])
	assert.Equal(t, int64(91004), byType["wanted_file"])
}

// TestAPIMismatchesFilterByType seeds the same four-case fixture and
// asserts ?type=add_to_collection narrows the response to just the
// orphan row.
func TestAPIMismatchesFilterByType(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	seedMismatchRecording(t, db, 9201, 92001, "SyncedShow",
		true, false, true, "MKV 1080p", "MKV 1080p")
	seedMismatchRecording(t, db, 9202, 92002, "OrphanShow",
		false, false, true, "", "MKV 720p")
	seedMismatchRecording(t, db, 9203, 92003, "MissingShow",
		true, false, false, "MKV 1080p", "")
	seedMismatchRecording(t, db, 9204, 92004, "WantedShow",
		false, true, false, "", "")

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet,
		"/api/v1/mismatches?type=add_to_collection", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		Items []struct {
			Type        string `json:"type"`
			RecordingID int64  `json:"recording_id"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	require.Len(t, body.Items, 1)
	assert.Equal(t, "add_to_collection", body.Items[0].Type)
	assert.Equal(t, int64(92002), body.Items[0].RecordingID)
}

// TestPagesMismatches asserts the mismatches page renders the empty
// shell. The mismatch rows + type tabs are populated client-side by
// /static/mismatches.js fetching /api/v1/mismatches; the data contract
// is still covered by TestAPIMismatchesEnumerates +
// TestAPIMismatchesFilterByType.
func TestPagesMismatches(t *testing.T) {
	t.Parallel()

	srv := fixtureBackedServer(t)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/mismatches", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	body := rr.Body.String()
	assert.Contains(t, body, `data-page="mismatches"`)
	assert.Contains(t, body, "Mismatches")
}

// TestRecordingPageShell asserts the recording detail page renders
// the empty shell with the recording id stamped into the page-root
// data attribute. NFT-callout copy + poster fallback rendering moved
// to the JS-driven page implementation; their behavior will be covered
// by JS-side tests once /static/recording.js is implemented.
func TestRecordingPageShell(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	rec := encora.Recording{
		ID:   424242,
		Show: "Greenwich BeaconShow",
		Tour: "Broadway",
		Date: encora.Date{FullDate: "2024-09-01", MonthKnown: true, DayKnown: true},
		Metadata: encora.RecordingMeta{
			ShowID:        9100,
			RecordingType: "pro-shot",
		},
	}
	seedRecording(t, db, rec)

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/recordings/424242", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	body := rr.Body.String()
	assert.Contains(t, body, `data-page="recording"`)
	assert.Contains(t, body, `data-recording-id="424242"`,
		"recording detail shell must stamp the recording id")
}

// stubEncoraClient is a minimal in-memory implementation of the
// EncoraWriteClient interface used by the apply tests. It records each
// call so assertions can verify the right endpoint fired with the right
// arguments, and lets each test slot a per-call response (success or
// sentinel error) into the queue.
type stubEncoraClient struct {
	mu                sync.Mutex
	addCalls          []int64
	formatCalls       []stubFormatCall
	addErr            error
	formatErr         error
	addRateLimit      encora.RateLimitInfo
	formatRateLimit   encora.RateLimitInfo
	updateCallCounter int
	// addResponses, when non-empty, supplies per-call (rate-limit,
	// error) responses in FIFO order; once drained, the stub falls back
	// to the addErr/addRateLimit defaults. Used by the retry-after test
	// to script a 429 followed by a success on the next call.
	addResponses []stubResponse
	// observedAddCtxCancelled captures whether the context handed to
	// AddToCollection was already cancelled at call time. Lets the
	// detached-context test verify pushCtx survives a request-context
	// cancel.
	observedAddCtxCancelled []bool
	// onAdd, when non-nil, is invoked at the start of every
	// AddToCollection call before the response is returned. Lets a
	// test cancel the request context mid-batch so the next iteration
	// of the apply loop can be observed receiving a detached context.
	onAdd func()
}

// stubResponse is one scripted (rate-limit, error) reply consumed in
// FIFO order from stubEncoraClient.addResponses.
type stubResponse struct {
	rl  encora.RateLimitInfo
	err error
}

type stubFormatCall struct {
	ID     int64
	Format string
}

func (s *stubEncoraClient) AddToCollection(ctx context.Context, id int64) (encora.RateLimitInfo, error) {
	s.mu.Lock()
	hook := s.onAdd
	s.addCalls = append(s.addCalls, id)
	s.observedAddCtxCancelled = append(s.observedAddCtxCancelled, ctx.Err() != nil)
	var resp stubResponse
	used := false
	if len(s.addResponses) > 0 {
		resp = s.addResponses[0]
		s.addResponses = s.addResponses[1:]
		used = true
	}
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	if used {
		return resp.rl, resp.err
	}
	return s.addRateLimit, s.addErr
}

func (s *stubEncoraClient) UpdateCollectionFormat(
	_ context.Context, id int64, format string,
) (encora.RateLimitInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.formatCalls = append(s.formatCalls, stubFormatCall{ID: id, Format: format})
	s.updateCallCounter++
	return s.formatRateLimit, s.formatErr
}

// applyTestServer wires a fresh DB + stub encora client into a server
// and returns both so tests can assert on history rows + recorded
// upstream calls without the fixture-backed sync.
func applyTestServer(t *testing.T) (*server.Server, *gosql.DB, *stubEncoraClient) {
	t.Helper()
	db, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	stub := &stubEncoraClient{}
	srv, err := server.New(server.Options{DB: db, Encora: stub})
	require.NoError(t, err)
	return srv, db, stub
}

func TestAPIApplyHandlesAddToCollection(t *testing.T) {
	t.Parallel()
	srv, db, stub := applyTestServer(t)

	// Seed an orphan: file present, not in collection or wants. The new
	// validation pass requires the action to match the live mismatch
	// oracle, so without this seed loadMismatches returns an empty set
	// and the action would be rejected before ever reaching the stub.
	seedMismatchRecording(t, db, 99001, 12345, "OrphanShow",
		false, false, true, "", "MKV 1080p")

	body, err := json.Marshal(map[string]any{
		"actions": []map[string]any{
			{"type": "add_to_collection", "recording_id": 12345},
		},
	})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/apply", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp struct {
		Results []struct {
			OK         bool   `json:"ok"`
			Error      string `json:"error"`
			HTTPStatus int    `json:"http_status"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Results, 1)
	assert.True(t, resp.Results[0].OK, "expected ok=true; got error %q", resp.Results[0].Error)
	assert.Equal(t, http.StatusOK, resp.Results[0].HTTPStatus)

	assert.Equal(t, []int64{12345}, stub.addCalls,
		"AddToCollection should be invoked exactly once with the supplied id")

	// History event must be written for successful pushes so the audit
	// log captures every change the server made upstream.
	events, err := storage.ListHistory(t.Context(), db, storage.ListHistoryOptions{
		Kinds: []string{storage.HistoryKindEncoraPush},
	})
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, storage.HistoryKindEncoraPush, events[0].Kind)
	require.NotNil(t, events[0].RecordingID)
	assert.Equal(t, int64(12345), *events[0].RecordingID)
	assert.Contains(t, events[0].Summary, "12345")
}

func TestAPIApplyHandlesFormatMismatch(t *testing.T) {
	t.Parallel()
	srv, db, stub := applyTestServer(t)

	// Seed a format mismatch: in collection with encoraFormat differing
	// from the local file's FormatLabel. The validation pass also
	// requires the submitted NewFormat to match the local oracle, so we
	// make local FormatLabel == "MKV 1080p" (the value the test pushes)
	// and Encora's recorded format something different.
	seedMismatchRecording(t, db, 99002, 90100222, "FormatShow",
		true, false, true, "MKV 720p", "MKV 1080p")

	body, err := json.Marshal(map[string]any{
		"actions": []map[string]any{
			{
				"type":         "format_mismatch",
				"recording_id": 90100222,
				"new_format":   "MKV 1080p",
			},
		},
	})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/apply", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	require.Len(t, stub.formatCalls, 1, "UpdateCollectionFormat should fire once")
	assert.Equal(t, int64(90100222), stub.formatCalls[0].ID)
	assert.Equal(t, "MKV 1080p", stub.formatCalls[0].Format,
		"format string must be forwarded as-is to the encora client")

	events, err := storage.ListHistory(t.Context(), db, storage.ListHistoryOptions{
		Kinds: []string{storage.HistoryKindEncoraPush},
	})
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Contains(t, events[0].Summary, "MKV 1080p")
}

func TestAPIApplyMissingFileShortCircuits(t *testing.T) {
	t.Parallel()
	srv, db, stub := applyTestServer(t)

	// Seed a missing-file mismatch: in collection but no local version
	// row. The validation pass needs the action to match the live state.
	seedMismatchRecording(t, db, 99003, 99, "MissingShow",
		true, false, false, "MKV 1080p", "")

	body, err := json.Marshal(map[string]any{
		"actions": []map[string]any{
			{"type": "missing_file", "recording_id": 99},
		},
	})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/apply", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp struct {
		Results []struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Results, 1)
	assert.False(t, resp.Results[0].OK)
	assert.Contains(t, resp.Results[0].Error, "requires a downloaded file")

	assert.Empty(t, stub.addCalls, "encora client must not be called for missing_file")
	assert.Empty(t, stub.formatCalls, "encora client must not be called for missing_file")

	// No history row should be written for a non-push.
	events, err := storage.ListHistory(t.Context(), db, storage.ListHistoryOptions{
		Kinds: []string{storage.HistoryKindEncoraPush},
	})
	require.NoError(t, err)
	assert.Empty(t, events)
}

func TestAPIApplyWithoutEncoraClient(t *testing.T) {
	t.Parallel()

	db, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	body, err := json.Marshal(map[string]any{
		"actions": []map[string]any{
			{"type": "add_to_collection", "recording_id": 1},
		},
	})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/apply", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req)

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "encora client not configured")
}

func TestAPIApplySurfacesEncoraError(t *testing.T) {
	t.Parallel()
	srv, db, stub := applyTestServer(t)
	stub.addErr = encora.ErrRateLimited
	stub.addRateLimit = encora.RateLimitInfo{RetryAfter: 60 * time.Second}

	// Seed an orphan so the validation pass lets the action through to
	// applyOne where the rate-limit error surfaces.
	seedMismatchRecording(t, db, 99004, 7, "ErrShow",
		false, false, true, "", "MKV 1080p")

	body, err := json.Marshal(map[string]any{
		"actions": []map[string]any{
			{"type": "add_to_collection", "recording_id": 7},
		},
	})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/apply", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp struct {
		Results []struct {
			OK         bool   `json:"ok"`
			Error      string `json:"error"`
			HTTPStatus int    `json:"http_status"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Results, 1)
	assert.False(t, resp.Results[0].OK)
	assert.Contains(t, strings.ToLower(resp.Results[0].Error), "rate limited")
	assert.Equal(t, http.StatusTooManyRequests, resp.Results[0].HTTPStatus)

	// Failure means no history row — the server only logs successful
	// pushes so the audit trail isn't polluted with non-events.
	events, err := storage.ListHistory(t.Context(), db, storage.ListHistoryOptions{
		Kinds: []string{storage.HistoryKindEncoraPush},
	})
	require.NoError(t, err)
	assert.Empty(t, events,
		"failed pushes must not produce HistoryKindEncoraPush events")
}

// applyTestServerWithSleeper is applyTestServer's sibling that lets a
// test inject a recording sleeper so the Retry-After honor logic stays
// asserrtable without spending wall-clock time. Returns the recorded
// durations slice (locked behind a mutex) the caller can read after
// the request returns.
func applyTestServerWithSleeper(
	t *testing.T,
) (*server.Server, *gosql.DB, *stubEncoraClient, *sleepRecorder) {
	t.Helper()
	db, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	stub := &stubEncoraClient{}
	rec := &sleepRecorder{}
	srv, err := server.New(server.Options{DB: db, Encora: stub, Sleeper: rec.Sleep})
	require.NoError(t, err)
	return srv, db, stub, rec
}

// sleepRecorder captures every duration handed to Sleep without
// blocking. Concurrency-safe so the apply batch driver can call into it
// from any goroutine the echo runtime spins up.
type sleepRecorder struct {
	mu        sync.Mutex
	durations []time.Duration
}

func (r *sleepRecorder) Sleep(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.durations = append(r.durations, d)
}

func (r *sleepRecorder) Calls() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]time.Duration, len(r.durations))
	copy(out, r.durations)
	return out
}

// TestAPIApplyRejectsStaleAction asserts the validation pass refuses to
// forward an action whose (type, recording_id) pair isn't in the live
// mismatch oracle — e.g. someone POSTs an add_to_collection for a
// recording that's already in the collection. The encora client must
// not be invoked, and no history row should be written.
func TestAPIApplyRejectsStaleAction(t *testing.T) {
	t.Parallel()
	srv, db, stub := applyTestServer(t)

	// Seed a recording in StatusSynced — collected, with a matching
	// local file, so loadMismatches returns zero rows for it.
	seedMismatchRecording(t, db, 99100, 90004242, "SyncedShow",
		true, false, true, "MKV 1080p", "MKV 1080p")

	body, err := json.Marshal(map[string]any{
		"actions": []map[string]any{
			{"type": "add_to_collection", "recording_id": 90004242},
		},
	})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/apply", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp struct {
		Results []struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Results, 1)
	assert.False(t, resp.Results[0].OK,
		"action against a synced recording must be rejected")
	assert.Contains(t, resp.Results[0].Error, "no longer applies",
		"rejection reason should mention the stale-state guard")

	assert.Empty(t, stub.addCalls,
		"encora client must not be invoked for a stale action")
	assert.Empty(t, stub.formatCalls,
		"encora client must not be invoked for a stale action")

	events, err := storage.ListHistory(t.Context(), db, storage.ListHistoryOptions{
		Kinds: []string{storage.HistoryKindEncoraPush},
	})
	require.NoError(t, err)
	assert.Empty(t, events,
		"a stale-action rejection must not produce an encora_push history row")
}

// TestAPIApplyRejectsTamperedFormat asserts that a FormatMismatch action
// whose NewFormat doesn't match the live LocalFormat oracle is refused
// before the encora client is touched. Without this guard a malicious
// or stale form could push arbitrary format strings.
func TestAPIApplyRejectsTamperedFormat(t *testing.T) {
	t.Parallel()
	srv, db, stub := applyTestServer(t)

	// Seed a format mismatch with local format MKV 1080p; the test
	// submits NewFormat=MKV 4K instead, simulating a tampered form.
	seedMismatchRecording(t, db, 99101, 5151, "TamperShow",
		true, false, true, "MKV 720p", "MKV 1080p")

	body, err := json.Marshal(map[string]any{
		"actions": []map[string]any{
			{
				"type":         "format_mismatch",
				"recording_id": 5151,
				"new_format":   "MKV 4K",
			},
		},
	})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/apply", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp struct {
		Results []struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Results, 1)
	assert.False(t, resp.Results[0].OK)
	assert.Contains(t, strings.ToLower(resp.Results[0].Error), "format")

	assert.Empty(t, stub.formatCalls,
		"a tampered format must not reach the encora client")
}

// TestAPIApplyHonorsRetryAfter scripts a 429 (RetryAfter=2s) on the
// first action and a success on the second, asserts both actions
// complete in a single response, and verifies the sleeper was called
// with the parsed RetryAfter before the second call fired. This is the
// regression fence against "one 429 cascades into N more 429s" because
// nothing slept between calls.
func TestAPIApplyHonorsRetryAfter(t *testing.T) {
	t.Parallel()
	srv, db, stub, sleepRec := applyTestServerWithSleeper(t)

	// Two orphan recordings so two add_to_collection actions can both
	// pass validation and go into the apply loop.
	seedMismatchRecording(t, db, 99200, 8001, "RetryShowA",
		false, false, true, "", "MKV 1080p")
	seedMismatchRecording(t, db, 99201, 8002, "RetryShowB",
		false, false, true, "", "MKV 1080p")

	stub.addResponses = []stubResponse{
		{rl: encora.RateLimitInfo{RetryAfter: 2 * time.Second}, err: encora.ErrRateLimited},
		{rl: encora.RateLimitInfo{}, err: nil},
	}

	body, err := json.Marshal(map[string]any{
		"actions": []map[string]any{
			{"type": "add_to_collection", "recording_id": 8001},
			{"type": "add_to_collection", "recording_id": 8002},
		},
	})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/apply", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp struct {
		Results []struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Results, 2)
	assert.False(t, resp.Results[0].OK,
		"first action should report the 429")
	assert.Contains(t, strings.ToLower(resp.Results[0].Error), "rate limited")
	assert.True(t, resp.Results[1].OK,
		"second action should still fire after the Retry-After sleep")

	assert.Equal(t, []int64{8001, 8002}, stub.addCalls,
		"both actions should reach the encora client")

	calls := sleepRec.Calls()
	require.Len(t, calls, 1,
		"sleeper should fire exactly once between the 429 and the second action")
	assert.Equal(t, 2*time.Second, calls[0],
		"sleeper should honor the parsed Retry-After")
}

// TestAPIApplyDetachedContext asserts the apply push loop runs against
// a context that survives a cancellation of the request context. We
// fire two actions and let the stub's onAdd hook cancel the request
// context during the first call. The second call's observed context
// state must still be non-cancelled — proving context.WithoutCancel
// was applied to the push loop. Validation runs first against the
// (still-live) request context, which is why we cancel from inside
// the stub instead of pre-cancelling: a pre-cancel would surface as a
// 500 from the local DB driver before the loop could even start.
func TestAPIApplyDetachedContext(t *testing.T) {
	t.Parallel()
	srv, db, stub := applyTestServer(t)

	seedMismatchRecording(t, db, 99300, 6001, "DetachShowA",
		false, false, true, "", "MKV 1080p")
	seedMismatchRecording(t, db, 99301, 6002, "DetachShowB",
		false, false, true, "", "MKV 1080p")

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	// Cancel the request context synchronously during the first
	// upstream call. The hook fires only on the first AddToCollection
	// because the test reuses cancel()'s idempotency.
	stub.onAdd = cancel

	body, err := json.Marshal(map[string]any{
		"actions": []map[string]any{
			{"type": "add_to_collection", "recording_id": 6001},
			{"type": "add_to_collection", "recording_id": 6002},
		},
	})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost,
		"/api/v1/apply", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req)

	// The handler ran to completion because pushCtx is detached from
	// the cancelled request context.
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	require.Equal(t, []int64{6001, 6002}, stub.addCalls,
		"both upstream calls should fire even after the request context was cancelled mid-batch")
	require.Len(t, stub.observedAddCtxCancelled, 2)
	for i, cancelled := range stub.observedAddCtxCancelled {
		assert.False(t, cancelled,
			"call %d: encora client should receive a non-cancelled context (detached)", i)
	}
}

// stagemediaPostersServer returns an httptest.Server that responds to
// /api/images?show_id=...&actor_ids=1 with the supplied poster URLs.
// The Posters() client method only consumes the "posters" field, so
// performers + error are stubbed empty/null.
func stagemediaPostersServer(t *testing.T, posters []string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/images", func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{
			"posters":    posters,
			"performers": []any{},
			"error":      nil,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
		_ = r // unused but kept for signature symmetry.
	})
	return httptest.NewServer(mux)
}

// TestAPIRecordingByIDIncludesPosters wires a real stagemedia.Client
// at an httptest server that returns two poster URLs and asserts the
// /api/v1/recordings/{id} response surfaces them under the "posters"
// JSON key.
func TestAPIRecordingByIDIncludesPosters(t *testing.T) {
	t.Parallel()

	upstream := stagemediaPostersServer(t, []string{"url-a", "url-b"})
	t.Cleanup(upstream.Close)

	smClient, err := stagemedia.New(stagemedia.Options{
		BaseURL: upstream.URL,
		APIKey:  "test",
	})
	require.NoError(t, err)

	srv := fixtureBackedServerWithStagemedia(t, smClient)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(),
		http.MethodGet, "/api/v1/recordings/90100222", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		Posters []string `json:"posters"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, []string{"url-a", "url-b"}, body.Posters)
}

// TestAPIRecordingByIDNoPostersWhenNilClient asserts the response
// returns an empty (but present) "posters" field when the server has
// no stagemedia client configured.
func TestAPIRecordingByIDNoPostersWhenNilClient(t *testing.T) {
	t.Parallel()

	srv := fixtureBackedServer(t)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(),
		http.MethodGet, "/api/v1/recordings/90100222", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		Posters []string `json:"posters"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Empty(t, body.Posters,
		"posters should be empty (or nil) when stagemedia is unconfigured")
}

// TestAPIRecordingByIDIncludesNFOContent seeds a recording_versions
// row pointing at a temp directory, writes a known XML string to
// {dir}/movie.nfo, and asserts the API returns the file content +
// non-nil mtime.
func TestAPIRecordingByIDIncludesNFOContent(t *testing.T) {
	t.Parallel()

	srv, db := fixtureBackedServerExposingDB(t)

	tmp := t.TempDir()
	versionPath := filepath.Join(tmp, "Marigold - 2009-12 (90100222).mkv")
	require.NoError(t, os.WriteFile(versionPath, []byte("not a real video"), 0o600))
	want := `<?xml version="1.0"?><movie><title>Marigold</title></movie>`
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "movie.nfo"), []byte(want), 0o600))

	require.NoError(t, storage.UpsertVersion(t.Context(), db, storage.RecordingVersion{
		RecordingID: 90100222,
		FilePath:    versionPath,
		FormatLabel: "MKV 1080p",
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(),
		http.MethodGet, "/api/v1/recordings/90100222", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		NFOContent    string     `json:"nfo_content"`
		NFOModifiedAt *time.Time `json:"nfo_modified_at"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, want, body.NFOContent)
	require.NotNil(t, body.NFOModifiedAt, "nfo_modified_at must be populated when the file exists")
	assert.WithinDuration(t, time.Now(), *body.NFOModifiedAt, 30*time.Second)
}

// TestAPIRecordingByIDNFOMissing covers the path where a version row
// exists but no movie.nfo sits next to it on disk. nfo_content is
// empty and nfo_modified_at is nil.
func TestAPIRecordingByIDNFOMissing(t *testing.T) {
	t.Parallel()

	srv, db := fixtureBackedServerExposingDB(t)

	tmp := t.TempDir()
	versionPath := filepath.Join(tmp, "Marigold - 2009-12 (90100222).mkv")
	require.NoError(t, os.WriteFile(versionPath, []byte("not a real video"), 0o600))
	// Deliberately do NOT write a movie.nfo.

	require.NoError(t, storage.UpsertVersion(t.Context(), db, storage.RecordingVersion{
		RecordingID: 90100222,
		FilePath:    versionPath,
		FormatLabel: "MKV 1080p",
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(),
		http.MethodGet, "/api/v1/recordings/90100222", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		NFOContent    string     `json:"nfo_content"`
		NFOModifiedAt *time.Time `json:"nfo_modified_at"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Empty(t, body.NFOContent)
	assert.Nil(t, body.NFOModifiedAt)
}

// fixtureBackedServerWithStagemedia mirrors fixtureBackedServer but
// wires the supplied stagemedia client through Options.Stagemedia so
// the recording-detail handler exercises its enrichment branch.
func fixtureBackedServerWithStagemedia(t *testing.T, sm *stagemedia.Client) *server.Server {
	t.Helper()

	mux := http.NewServeMux()
	for path, file := range map[string]string{
		"/api/profile":    "profile.json",
		"/api/collection": "collection.json",
		"/api/wants":      "wants.json",
	} {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			b, err := os.ReadFile(filepath.Join(fixturesDir, file))
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

	db, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	c, err := encora.New(encora.Options{BaseURL: upstream.URL, APIKey: "test"})
	require.NoError(t, err)
	_, err = syncpkg.Sync(t.Context(), c, db, syncpkg.Options{BurstReserve: 2})
	require.NoError(t, err)

	srv, err := server.New(server.Options{DB: db, Stagemedia: sm})
	require.NoError(t, err)
	return srv
}

// fixtureBackedServerExposingDB returns the same fixture-seeded server
// as fixtureBackedServer, plus the underlying *sql.DB so callers can
// seed extra rows (e.g. recording_versions pointing at a temp dir).
func fixtureBackedServerExposingDB(t *testing.T) (*server.Server, *gosql.DB) {
	t.Helper()

	mux := http.NewServeMux()
	for path, file := range map[string]string{
		"/api/profile":    "profile.json",
		"/api/collection": "collection.json",
		"/api/wants":      "wants.json",
	} {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			b, err := os.ReadFile(filepath.Join(fixturesDir, file))
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

	db, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	c, err := encora.New(encora.Options{BaseURL: upstream.URL, APIKey: "test"})
	require.NoError(t, err)
	_, err = syncpkg.Sync(t.Context(), c, db, syncpkg.Options{BurstReserve: 2})
	require.NoError(t, err)

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)
	return srv, db
}
