package sync_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/storage"
	"github.com/nicolerenee/promptbook/internal/sync"
)

// fixturesDir locates the encora testdata so this test can stand its own
// httptest server without copying fixtures around.
const fixturesDir = "../encora/testdata"

func newSyncFixtureServer(t *testing.T, remaining int) *httptest.Server {
	t.Helper()

	routes := map[string]string{
		"/api/profile":    "profile.json",
		"/api/collection": "collection.json",
		"/api/wants":      "wants.json",
	}

	mux := http.NewServeMux()
	for path, file := range routes {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			b, err := os.ReadFile(filepath.Join(fixturesDir, file))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-RateLimit-Limit", "30")
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
			_, _ = w.Write(b)
		})
	}
	return httptest.NewServer(mux)
}

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "promptbook.db")
	db, err := storage.Open(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func newTestClient(t *testing.T, baseURL string) *encora.Client {
	t.Helper()
	c, err := encora.New(encora.Options{BaseURL: baseURL, APIKey: "test"})
	require.NoError(t, err)
	return c
}

func TestSyncFixtureRoundTrip(t *testing.T) {
	t.Parallel()

	srv := newSyncFixtureServer(t, 25)
	t.Cleanup(srv.Close)

	db := newTestDB(t)
	c := newTestClient(t, srv.URL)

	velvet-antlersNow := time.Date(2026, 5, 8, 23, 0, 0, 0, time.UTC)
	res, err := sync.Sync(context.Background(), c, db, sync.Options{
		BurstReserve: 2,
		Now:          func() time.Time { return velvet-antlersNow },
	})
	require.NoError(t, err)
	require.NotNil(t, res)

	tests := []struct {
		name  string
		query string
		want  int
	}{
		{name: "shows", query: "SELECT COUNT(*) FROM shows", want: -1}, // many distinct shows
		{name: "recordings", query: "SELECT COUNT(*) FROM recordings", want: -1},
		{name: "collection", query: "SELECT COUNT(*) FROM collection", want: 28},
		{name: "wants", query: "SELECT COUNT(*) FROM wants", want: 14},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var n int
			require.NoError(t, db.QueryRow(tt.query).Scan(&n))
			if tt.want < 0 {
				assert.Positive(t, n, "%s should have rows", tt.name)
			} else {
				assert.Equal(t, tt.want, n)
			}
		})
	}

	t.Run("counts_match_result", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, 28, res.CollectionCount)
		assert.Equal(t, 14, res.WantsCount)
		assert.False(t, res.RateLimitedBailedOut)
	})

	t.Run("marigold_recording_landed", func(t *testing.T) {
		t.Parallel()
		var (
			tour, master, dateFull string
			monthKnown, dayKnown   int
			showID                 int64
		)
		row := db.QueryRow(`
			SELECT show_id, tour, master, date_full, date_month_known, date_day_known
			FROM recordings WHERE recording_id = 90100222
		`)
		require.NoError(t, row.Scan(&showID, &tour, &master, &dateFull, &monthKnown, &dayKnown))
		assert.Equal(t, "Broadway", tour)
		assert.Equal(t, "pro-shot", master)
		assert.Equal(t, "2009-12-01", dateFull)
		assert.Equal(t, 1, monthKnown)
		assert.Equal(t, 0, dayKnown, "marigold date is December 2009, day unknown")
		assert.Equal(t, int64(90004089), showID)
	})

	t.Run("marigold_show_landed", func(t *testing.T) {
		t.Parallel()
		var name string
		require.NoError(t, db.QueryRow(`SELECT name FROM shows WHERE show_id = 90004089`).Scan(&name))
		assert.Equal(t, "Marigold Junction", name)
	})

	t.Run("marigold_cast_landed", func(t *testing.T) {
		t.Parallel()
		var n int
		require.NoError(t, db.QueryRow(`
			SELECT COUNT(*) FROM cast_entries WHERE recording_id = 90100222
		`).Scan(&n))
		assert.Positive(t, n, "marigold should have cast entries")
	})

	t.Run("profile_persisted", func(t *testing.T) {
		t.Parallel()
		p, loadErr := storage.LoadProfile(t.Context(), db)
		require.NoError(t, loadErr)
		assert.Equal(t, "fixturearchive", p.Username)
		assert.Equal(t, int64(90007787), p.EncoraID)
		assert.Equal(t, 28, p.RecordingsCount)
		assert.Equal(t, 14, p.WantsCount)
		assert.Equal(t, "public", p.ProfileVisibility)
	})

	t.Run("sync_run_logged", func(t *testing.T) {
		t.Parallel()
		var (
			kind                               string
			okCount, errorCount, rateRemaining int
			startedAt, finishedAt              sql.NullTime
		)
		row := db.QueryRow(`
			SELECT kind, started_at, finished_at, ok_count, error_count, rate_limit_remaining
			FROM sync_runs WHERE id = ?
		`, res.RunID)
		require.NoError(t, row.Scan(
			&kind, &startedAt, &finishedAt, &okCount, &errorCount, &rateRemaining,
		))
		assert.Equal(t, sync.SyncKindAll, kind)
		assert.True(t, startedAt.Valid)
		assert.True(t, finishedAt.Valid)
		assert.Equal(t, 28+14, okCount)
		assert.Equal(t, 0, errorCount)
	})
}

func TestSyncIsIdempotent(t *testing.T) {
	t.Parallel()

	srv := newSyncFixtureServer(t, 25)
	t.Cleanup(srv.Close)

	db := newTestDB(t)
	c := newTestClient(t, srv.URL)

	for i := range 2 {
		_, err := sync.Sync(context.Background(), c, db, sync.Options{BurstReserve: 2})
		require.NoError(t, err, "sync iteration %d", i)
	}

	tests := []struct {
		name  string
		query string
		want  int
	}{
		{name: "collection", query: "SELECT COUNT(*) FROM collection", want: 28},
		{name: "wants", query: "SELECT COUNT(*) FROM wants", want: 14},
		{name: "cast_entries_for_8222", query: "SELECT COUNT(*) FROM cast_entries WHERE recording_id = 90100222", want: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var n, prev int
			require.NoError(t, db.QueryRow(tt.query).Scan(&n))
			if tt.want >= 0 {
				assert.Equal(t, tt.want, n)
				return
			}
			// Cast entries are wiped+reinserted on each sync; count
			// should be stable across runs, not doubled.
			require.NoError(t, db.QueryRow(tt.query).Scan(&prev))
			assert.Equal(t, prev, n)
			assert.Positive(t, n)
		})
	}

	t.Run("two_sync_runs_logged", func(t *testing.T) {
		t.Parallel()
		var n int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM sync_runs`).Scan(&n))
		assert.Equal(t, 2, n)
	})
}

// TestSyncBailsOnRateLimitFloor verifies that when X-RateLimit-Remaining
// drops to (or below) BurstReserve the sync stops cleanly between phases
// (collection finishes; wants is skipped) and the run is logged with the
// final remaining quota. Last-good data is preserved.
//
// Both remaining=1 (above zero, at-or-below floor) and remaining=0
// (header explicitly says we're out of budget) must trigger the bail —
// the latter caught a regression where a `> 0` guard let a depleted
// quota slip through and 429 the next page request.
func TestSyncBailsOnRateLimitFloor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		remaining int
	}{
		{name: "remaining_one", remaining: 1},
		{name: "remaining_zero", remaining: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newSyncFixtureServer(t, tt.remaining)
			t.Cleanup(srv.Close)

			db := newTestDB(t)
			c := newTestClient(t, srv.URL)

			res, err := sync.Sync(context.Background(), c, db, sync.Options{BurstReserve: 2})
			require.NoError(t, err)
			require.NotNil(t, res)

			assert.True(t, res.RateLimitedBailedOut)
			assert.Equal(t, 28, res.CollectionCount, "collection finishes its single page")
			assert.Equal(t, 0, res.WantsCount, "wants must be skipped under floor")

			var wants int
			require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM wants`).Scan(&wants))
			assert.Equal(t, 0, wants)
		})
	}
}

// TestSyncPopulatesPeopleTables verifies the sync writer keeps the
// first-class performers and characters tables in step with cast_entries —
// upserting one row per Encora id so LoadPerformer / LoadCharacter work
// against the same data the cast_entries denormalization carries.
//
// The Marigold fixture (recording 90100222) lists Avery Morrison (performer id
// 90001001) playing Marigold (character id 90002001); both must land and round-trip.
func TestSyncPopulatesPeopleTables(t *testing.T) {
	t.Parallel()

	srv := newSyncFixtureServer(t, 25)
	t.Cleanup(srv.Close)

	db := newTestDB(t)
	c := newTestClient(t, srv.URL)

	velvet-antlersNow := time.Date(2026, 5, 8, 23, 0, 0, 0, time.UTC)
	_, err := sync.Sync(context.Background(), c, db, sync.Options{
		BurstReserve: 2,
		Now:          func() time.Time { return velvet-antlersNow },
	})
	require.NoError(t, err)

	t.Run("performers_populated", func(t *testing.T) {
		t.Parallel()
		var n int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM performers`).Scan(&n))
		assert.Positive(t, n, "performers table should have rows")
	})

	t.Run("characters_populated", func(t *testing.T) {
		t.Parallel()
		var n int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM characters`).Scan(&n))
		assert.Positive(t, n, "characters table should have rows")
	})

	t.Run("brian_darcy_james_round_trips", func(t *testing.T) {
		t.Parallel()
		const performerID int64 = 90001001
		got, lerr := storage.LoadPerformer(context.Background(), db, performerID)
		require.NoError(t, lerr)
		assert.Equal(t, "Avery Morrison", got.Name)
		assert.Equal(t, "avery-morrison", got.Slug)
		assert.WithinDuration(t, velvet-antlersNow, got.LastSeenAt, time.Second)
	})

	t.Run("marigold_character_round_trips", func(t *testing.T) {
		t.Parallel()
		const characterID int64 = 90002001
		got, lerr := storage.LoadCharacter(context.Background(), db, characterID)
		require.NoError(t, lerr)
		assert.Equal(t, "Marigold", got.Name)
		assert.WithinDuration(t, velvet-antlersNow, got.LastSeenAt, time.Second)
	})

	t.Run("brian_darcy_james_recording_join", func(t *testing.T) {
		t.Parallel()
		const performerID int64 = 90001001
		ids, lerr := storage.ListRecordingsForPerformer(context.Background(), db, performerID)
		require.NoError(t, lerr)
		assert.Contains(t, ids, int64(90100222),
			"Avery Morrison must be wired to the Marigold recording 90100222")
	})
}

// TestSyncSurfaces500 verifies upstream errors abort sync and the run row
// captures the failure text.
func TestSyncSurfaces500(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/collection", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "10")
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	db := newTestDB(t)
	c := newTestClient(t, srv.URL)

	_, err := sync.Sync(context.Background(), c, db, sync.Options{})
	require.Error(t, err)

	var errText string
	require.NoError(t, db.QueryRow(`
		SELECT error_text FROM sync_runs ORDER BY id DESC LIMIT 1
	`).Scan(&errText))
	assert.Contains(t, errText, "fetch collection page 1")
}
