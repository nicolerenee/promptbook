package server_test

import (
	"context"
	gosql "database/sql"
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

// fourStatusServer builds a server backed by a freshly-migrated SQLite DB
// seeded with exactly four recordings — one each in synced,
// format_mismatch, missing, and wanted state. Orphan is omitted because
// it requires a recording row with no FK from collection/wants, which is
// the most awkward to seed.
//
// Returns the server plus the show name strings so assertions can
// resolve recordings by their distinctive metadata.
func fourStatusServer(t *testing.T) (*server.Server, map[storage.Status]string) {
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

	names := make(map[storage.Status]string, len(seeds))
	for _, s := range seeds {
		names[s.status] = s.showName
	}
	return srv, names
}

func TestAPIRecordingsStatusFilter(t *testing.T) {
	t.Parallel()

	srv, _ := fourStatusServer(t)

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

func TestPagesQueueEmpty(t *testing.T) {
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
	assert.Contains(t, rr.Body.String(), "No files in the manual import queue")
}

func TestPagesQueueLists(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	_, err = storage.EnqueueFile(ctx, db, storage.QueueEntry{
		FilePath:            "/incoming/cresthaven.mkv",
		FileSizeBytes:       4096,
		SuggestedConfidence: storage.ConfidenceMedium,
	})
	require.NoError(t, err)
	_, err = storage.EnqueueFile(ctx, db, storage.QueueEntry{
		FilePath:            "/incoming/cats.mkv",
		FileSizeBytes:       8192,
		SuggestedConfidence: storage.ConfidenceLow,
	})
	require.NoError(t, err)

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/queue", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	body := rr.Body.String()
	assert.Contains(t, body, "/incoming/cresthaven.mkv")
	assert.Contains(t, body, "/incoming/cats.mkv")
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

func TestPagesPeople(t *testing.T) {
	t.Parallel()

	srv := fixtureBackedServer(t)

	t.Run("list", func(t *testing.T) {
		t.Parallel()
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/people", nil)
		srv.Handler().ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assert.Contains(t, rr.Body.String(), "Avery Morrison")
	})

	t.Run("detail", func(t *testing.T) {
		t.Parallel()
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/people/90001001", nil)
		srv.Handler().ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assert.Contains(t, rr.Body.String(), "Marigold")
	})
}

func TestPagesHomeStatusFilter(t *testing.T) {
	t.Parallel()

	srv, names := fourStatusServer(t)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/?status=missing", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	body := rr.Body.String()
	assert.Contains(t, body, names[storage.StatusMissing],
		"expected the missing recording's show name to render")
	assert.NotContains(t, body, names[storage.StatusSynced],
		"synced recording's show name should not render under ?status=missing")
	assert.NotContains(t, body, names[storage.StatusWanted],
		"wanted recording's show name should not render under ?status=missing")
}

// TestRecordingPageNFTWarning seeds a recording with NFT.NFTForever set
// and asserts the detail page renders the human-readable callout.
func TestRecordingPageNFTWarning(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	rec := encora.Recording{
		ID:    424242,
		Show:  "Greenwich BeaconShow",
		Tour:  "Broadway",
		Date:  encora.Date{FullDate: "2024-09-01", MonthKnown: true, DayKnown: true},
		NFT:   encora.NFT{NFTForever: true},
		Notes: "private collection only",
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
	assert.Contains(t, rr.Body.String(), "NFT forever",
		"expected NFT-forever callout to render in body")
}

// TestRecordingPagePosterEmpty asserts the detail page renders cleanly
// when the server has no stagemedia client configured. Exercises both
// the placeholder fallback and the nil-client short-circuit in
// handleRecordingPage.
func TestRecordingPagePosterEmpty(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	rec := encora.Recording{
		ID:   424243,
		Show: "PlaceholderShow",
		Tour: "Tour 1",
		Date: encora.Date{FullDate: "2025-01-15", MonthKnown: true, DayKnown: true},
		Metadata: encora.RecordingMeta{
			ShowID:        9101,
			RecordingType: "pro-shot",
		},
	}
	seedRecording(t, db, rec)

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)
	require.Nil(t, srv.Stagemedia(), "stagemedia client should be nil when not configured")

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/recordings/424243", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	body := rr.Body.String()
	assert.Contains(t, body, "no poster",
		"expected poster placeholder copy when stagemedia is disabled")
	assert.NotContains(t, body, "<img class=\"poster\"",
		"poster img tag should not render when no posters are loaded")
}
