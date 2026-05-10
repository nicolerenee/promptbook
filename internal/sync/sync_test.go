package sync_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/ent/syncrun"
	"github.com/nicolerenee/promptbook/internal/storage"
	promptbookSync "github.com/nicolerenee/promptbook/internal/sync"
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

func newTestDB(t *testing.T) *ent.Client {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "promptbook.db")
	sqlDB, db, err := storage.OpenEnt(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
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
	res, err := promptbookSync.Sync(context.Background(), c, db, promptbookSync.Options{
		BurstReserve: 2,
		Now:          func() time.Time { return velvet-antlersNow },
	})
	require.NoError(t, err)
	require.NotNil(t, res)

	tests := []struct {
		name  string
		count func() (int, error)
		want  int
	}{
		// many distinct shows / recordings — assert positive only.
		{
			name:  "shows",
			count: func() (int, error) { return db.Show.Query().Count(t.Context()) },
			want:  -1,
		},
		{
			name:  "recordings",
			count: func() (int, error) { return db.Recording.Query().Count(t.Context()) },
			want:  -1,
		},
		{
			name:  "collection",
			count: func() (int, error) { return db.CollectionEntry.Query().Count(t.Context()) },
			want:  28,
		},
		{
			name:  "wants",
			count: func() (int, error) { return db.WantsEntry.Query().Count(t.Context()) },
			want:  14,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			n, cerr := tt.count()
			require.NoError(t, cerr)
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
		r, lerr := db.Recording.Get(t.Context(), 90100222)
		require.NoError(t, lerr)
		assert.Equal(t, "Broadway", r.Tour)
		assert.Equal(t, "pro-shot", r.Master)
		assert.Equal(t, "2009-12-01", r.DateFull)
		assert.True(t, r.DateMonthKnown)
		assert.False(t, r.DateDayKnown, "marigold date is December 2009, day unknown")
		assert.Equal(t, int64(90004089), r.ShowID)
	})

	t.Run("marigold_show_landed", func(t *testing.T) {
		t.Parallel()
		s, sErr := db.Show.Get(t.Context(), 90004089)
		require.NoError(t, sErr)
		assert.Equal(t, "Marigold Junction", s.Name)
	})

	t.Run("marigold_cast_landed", func(t *testing.T) {
		t.Parallel()
		// CastEntry doesn't expose a typed predicate package; use the
		// ent query builder's recording-edge filter via the parent
		// Recording's edge traversal.
		recRow, lerr := db.Recording.Get(t.Context(), 90100222)
		require.NoError(t, lerr)
		n, cErr := recRow.QueryCastEntries().Count(t.Context())
		require.NoError(t, cErr)
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
		row, lerr := db.SyncRun.Get(t.Context(), res.RunID)
		require.NoError(t, lerr)
		assert.Equal(t, promptbookSync.SyncKindAll, row.Kind)
		assert.False(t, row.StartedAt.IsZero())
		require.NotNil(t, row.FinishedAt)
		assert.Equal(t, 28+14, row.OkCount)
		assert.Equal(t, 0, row.ErrorCount)
	})
}

func TestSyncIsIdempotent(t *testing.T) {
	t.Parallel()

	srv := newSyncFixtureServer(t, 25)
	t.Cleanup(srv.Close)

	db := newTestDB(t)
	c := newTestClient(t, srv.URL)

	for i := range 2 {
		_, err := promptbookSync.Sync(context.Background(), c, db, promptbookSync.Options{BurstReserve: 2})
		require.NoError(t, err, "sync iteration %d", i)
	}

	tests := []struct {
		name  string
		count func() (int, error)
		want  int
	}{
		{
			name:  "collection",
			count: func() (int, error) { return db.CollectionEntry.Query().Count(t.Context()) },
			want:  28,
		},
		{
			name:  "wants",
			count: func() (int, error) { return db.WantsEntry.Query().Count(t.Context()) },
			want:  14,
		},
		{
			name: "cast_entries_for_8222",
			count: func() (int, error) {
				r, gerr := db.Recording.Get(t.Context(), 90100222)
				if gerr != nil {
					return 0, gerr
				}
				return r.QueryCastEntries().Count(t.Context())
			},
			want: -1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			n, cerr := tt.count()
			require.NoError(t, cerr)
			if tt.want >= 0 {
				assert.Equal(t, tt.want, n)
				return
			}
			assert.Positive(t, n)
		})
	}

	t.Run("two_sync_runs_logged", func(t *testing.T) {
		t.Parallel()
		n, err := db.SyncRun.Query().Count(t.Context())
		require.NoError(t, err)
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

			res, err := promptbookSync.Sync(context.Background(), c, db, promptbookSync.Options{BurstReserve: 2})
			require.NoError(t, err)
			require.NotNil(t, res)

			assert.True(t, res.RateLimitedBailedOut)
			assert.Equal(t, 28, res.CollectionCount, "collection finishes its single page")
			assert.Equal(t, 0, res.WantsCount, "wants must be skipped under floor")

			wants, qErr := db.WantsEntry.Query().Count(t.Context())
			require.NoError(t, qErr)
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
	_, err := promptbookSync.Sync(context.Background(), c, db, promptbookSync.Options{
		BurstReserve: 2,
		Now:          func() time.Time { return velvet-antlersNow },
	})
	require.NoError(t, err)

	t.Run("performers_populated", func(t *testing.T) {
		t.Parallel()
		n, qerr := db.Performer.Query().Count(t.Context())
		require.NoError(t, qerr)
		assert.Positive(t, n, "performers table should have rows")
	})

	t.Run("characters_populated", func(t *testing.T) {
		t.Parallel()
		n, qerr := db.Character.Query().Count(t.Context())
		require.NoError(t, qerr)
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

// rateLimitFixtureServer serves the fixture JSON for /api/profile and
// /api/wants but lets the caller control /api/collection: the first N
// requests return 429 with the given Retry-After header; subsequent
// requests serve the collection fixture. The hit counter is observable
// through the returned *atomic.Int32 so tests can assert call counts.
func rateLimitFixtureServer(
	t *testing.T,
	failFirstN int32,
	retryAfterSeconds int,
) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	collectionHits := &atomic.Int32{}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/profile", func(w http.ResponseWriter, _ *http.Request) {
		b, err := os.ReadFile(filepath.Join(fixturesDir, "profile.json"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Limit", "30")
		w.Header().Set("X-RateLimit-Remaining", "25")
		_, _ = w.Write(b)
	})
	mux.HandleFunc("/api/wants", func(w http.ResponseWriter, _ *http.Request) {
		b, err := os.ReadFile(filepath.Join(fixturesDir, "wants.json"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Limit", "30")
		w.Header().Set("X-RateLimit-Remaining", "25")
		_, _ = w.Write(b)
	})
	mux.HandleFunc("/api/collection", func(w http.ResponseWriter, _ *http.Request) {
		hit := collectionHits.Add(1)
		if hit <= failFirstN {
			w.Header().Set("X-RateLimit-Limit", "30")
			w.Header().Set("X-RateLimit-Remaining", "0")
			if retryAfterSeconds > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
			}
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		b, err := os.ReadFile(filepath.Join(fixturesDir, "collection.json"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Limit", "30")
		w.Header().Set("X-RateLimit-Remaining", "25")
		_, _ = w.Write(b)
	})
	return httptest.NewServer(mux), collectionHits
}

// TestSyncSleepsOnRateLimitRetryAfter verifies that a 429 with Retry-After
// triggers a single sleep-then-retry, and the eventual success populates
// both collection and wants tables.
func TestSyncSleepsOnRateLimitRetryAfter(t *testing.T) {
	t.Parallel()

	srv, collectionHits := rateLimitFixtureServer(t, 1, 5)
	t.Cleanup(srv.Close)

	db := newTestDB(t)
	c := newTestClient(t, srv.URL)

	var sleeps []time.Duration
	var sleepMu sync.Mutex
	recordSleep := func(d time.Duration) {
		sleepMu.Lock()
		defer sleepMu.Unlock()
		sleeps = append(sleeps, d)
	}

	res, err := promptbookSync.Sync(context.Background(), c, db, promptbookSync.Options{
		BurstReserve: 2,
		Sleep:        recordSleep,
	})
	require.NoError(t, err)
	require.NotNil(t, res)

	assert.Equal(t, int32(2), collectionHits.Load(),
		"collection should be hit twice: first 429, second 200")

	sleepMu.Lock()
	defer sleepMu.Unlock()
	require.Len(t, sleeps, 1, "exactly one sleep call (the retry-after wait)")
	assert.Equal(t, 5*time.Second, sleeps[0])

	assert.Equal(t, 28, res.CollectionCount)
	assert.Equal(t, 14, res.WantsCount)

	collectionRows, qErr := db.CollectionEntry.Query().Count(t.Context())
	require.NoError(t, qErr)
	wantsRows, qErr := db.WantsEntry.Query().Count(t.Context())
	require.NoError(t, qErr)
	assert.Equal(t, 28, collectionRows)
	assert.Equal(t, 14, wantsRows)
}

// TestSyncGivesUpAfterSecondRateLimit verifies that when the retry also
// returns 429 the sync surfaces the error. RateLimitedBailedOut stays
// false because that flag tracks the burst-reserve floor bail-out, which
// is distinct from a 429 mid-batch.
func TestSyncGivesUpAfterSecondRateLimit(t *testing.T) {
	t.Parallel()

	srv, collectionHits := rateLimitFixtureServer(t, 5, 5)
	t.Cleanup(srv.Close)

	db := newTestDB(t)
	c := newTestClient(t, srv.URL)

	var sleeps []time.Duration
	var sleepMu sync.Mutex
	recordSleep := func(d time.Duration) {
		sleepMu.Lock()
		defer sleepMu.Unlock()
		sleeps = append(sleeps, d)
	}

	res, err := promptbookSync.Sync(context.Background(), c, db, promptbookSync.Options{
		BurstReserve: 2,
		Sleep:        recordSleep,
	})
	require.Error(t, err)
	require.ErrorIs(t, err, encora.ErrRateLimited)
	require.NotNil(t, res)

	assert.Equal(t, int32(2), collectionHits.Load(),
		"exactly one retry: first 429, second 429, then give up")

	sleepMu.Lock()
	defer sleepMu.Unlock()
	require.Len(t, sleeps, 1, "exactly one sleep call before giving up")
	assert.Equal(t, 5*time.Second, sleeps[0])

	assert.False(t, res.RateLimitedBailedOut,
		"the burst-reserve bail flag is unrelated to mid-batch 429s")
}

// TestSyncRespectsContextDuringRetrySleep verifies that a cancelled
// context short-circuits the retry-after wait. The server returns 429
// with a 60-second Retry-After; with a pre-cancelled context the function
// must return ctx.Err() promptly rather than blocking on the full sleep.
func TestSyncRespectsContextDuringRetrySleep(t *testing.T) {
	t.Parallel()

	srv, _ := rateLimitFixtureServer(t, 5, 60)
	t.Cleanup(srv.Close)

	db := newTestDB(t)
	c := newTestClient(t, srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel; the retry-after sleep must observe this.

	// Sleep stub: spin until ctx is done so the helper's select sees the
	// context cancellation and returns ctx.Err() before this returns.
	// In a real run with time.Sleep the leaked goroutine would still
	// finish; the test only cares that Sync returns promptly.
	sleepDone := make(chan time.Duration, 4)
	stubSleep := func(d time.Duration) {
		<-ctx.Done()
		sleepDone <- d
	}

	start := time.Now()
	_, err := promptbookSync.Sync(ctx, c, db, promptbookSync.Options{
		BurstReserve: 2,
		Sleep:        stubSleep,
	})
	elapsed := time.Since(start)

	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	assert.Less(t, elapsed, 5*time.Second,
		"sync must abandon the retry sleep when ctx is cancelled")
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

	_, err := promptbookSync.Sync(context.Background(), c, db, promptbookSync.Options{})
	require.Error(t, err)

	rows, qErr := db.SyncRun.Query().
		Order(syncrun.ByID(entsql.OrderDesc())).
		Limit(1).
		All(t.Context())
	require.NoError(t, qErr)
	require.Len(t, rows, 1)
	assert.Contains(t, rows[0].ErrorText, "fetch collection page 1")
}
