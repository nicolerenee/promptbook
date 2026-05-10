package server_test

// pagination_test.go — verifies every list endpoint returns the
// {items, total, limit, offset} envelope and that `total` reflects
// the unfiltered count for the active filter set (status / kind /
// recording_id). Sort + pagination behavior is exercised via slice
// length + first-row identity assertions.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/server"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// pageEnvelope is the shared response shape every paginated list
// endpoint emits. Tests decode into this struct so the assertions
// stay terse.
type pageEnvelopeResp struct {
	Items  []map[string]any `json:"items"`
	Total  int              `json:"total"`
	Limit  int              `json:"limit"`
	Offset int              `json:"offset"`
}

// decodePage decodes rr.Body as a page envelope. Helper keeps the
// per-test ceremony out of every block.
func decodePage(t *testing.T, rr *httptest.ResponseRecorder) pageEnvelopeResp {
	t.Helper()
	var got pageEnvelopeResp
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	return got
}

// TestPaginationEnvelopeRecordings asserts /api/v1/recordings returns
// total + limit + offset, and that total reflects the count of
// status-matching rows when ?status= is used.
func TestPaginationEnvelopeRecordings(t *testing.T) {
	t.Parallel()
	srv := fourStatusServer(t)

	t.Run("default_total_is_full_set", func(t *testing.T) {
		t.Parallel()
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(
			t.Context(), http.MethodGet, "/api/v1/recordings", nil)
		srv.Handler().ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

		got := decodePage(t, rr)
		assert.Equal(t, 4, got.Total, "fourStatusServer seeds four recordings")
		assert.Equal(t, 50, got.Limit, "default limit is 50")
		assert.Equal(t, 0, got.Offset)
		assert.Len(t, got.Items, 4)
	})

	t.Run("status_filter_total_matches_filter", func(t *testing.T) {
		t.Parallel()
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(
			t.Context(), http.MethodGet, "/api/v1/recordings?status=synced", nil)
		srv.Handler().ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

		got := decodePage(t, rr)
		assert.Equal(t, 1, got.Total,
			"only one synced row in the four-status seed")
		assert.Len(t, got.Items, 1)
	})

	t.Run("limit_offset_pages_correctly", func(t *testing.T) {
		t.Parallel()
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(
			t.Context(), http.MethodGet, "/api/v1/recordings?limit=2&offset=2", nil)
		srv.Handler().ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

		got := decodePage(t, rr)
		assert.Equal(t, 4, got.Total, "total ignores limit/offset")
		assert.Equal(t, 2, got.Limit)
		assert.Equal(t, 2, got.Offset)
		assert.Len(t, got.Items, 2)
	})
}

// TestPaginationSortRecordings asserts ?sort=date&dir=desc orders by
// recording date descending. Uses fourStatusServer's seed (the four
// recordings have empty date_full there, so we pick a key whose
// contents we know — show name — for a clearer assertion).
func TestPaginationSortRecordings(t *testing.T) {
	t.Parallel()
	srv := fourStatusServer(t)

	t.Run("sort_by_recording_asc", func(t *testing.T) {
		t.Parallel()
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(),
			http.MethodGet, "/api/v1/recordings?sort=recording&dir=asc", nil)
		srv.Handler().ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

		got := decodePage(t, rr)
		require.Len(t, got.Items, 4)
		// fourStatusServer seeds shows MismatchShow / MissingShow /
		// SyncedShow / WantedShow — ascending alpha = MismatchShow first.
		assert.Equal(t, "MismatchShow", got.Items[0]["show"])
	})

	t.Run("sort_by_recording_desc", func(t *testing.T) {
		t.Parallel()
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(),
			http.MethodGet, "/api/v1/recordings?sort=recording&dir=desc", nil)
		srv.Handler().ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

		got := decodePage(t, rr)
		require.Len(t, got.Items, 4)
		assert.Equal(t, "WantedShow", got.Items[0]["show"])
	})

	t.Run("unknown_sort_falls_back_to_default", func(t *testing.T) {
		t.Parallel()
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(),
			http.MethodGet, "/api/v1/recordings?sort=bogus", nil)
		srv.Handler().ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		got := decodePage(t, rr)
		assert.Len(t, got.Items, 4, "unknown sort still serves the page")
	})
}

// TestPaginationEnvelopePeople asserts /api/v1/people surfaces total
// and respects limit/offset.
func TestPaginationEnvelopePeople(t *testing.T) {
	t.Parallel()
	srv := fixtureBackedServer(t)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(),
		http.MethodGet, "/api/v1/people?limit=10&offset=0", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	got := decodePage(t, rr)
	assert.Equal(t, 10, got.Limit)
	assert.Equal(t, 0, got.Offset)
	assert.Positive(t, got.Total,
		"fixture-backed server seeds people from the cast entries")
	assert.LessOrEqual(t, len(got.Items), 10,
		"limit caps the page size")
	assert.LessOrEqual(t, len(got.Items), got.Total,
		"page never exceeds total")
}

// TestPaginationEnvelopeShows asserts /api/v1/shows returns the
// envelope and that total reflects every show row (including the
// zero-recording one).
func TestPaginationEnvelopeShows(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sqlDB, db, err := storage.OpenEnt(ctx, filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	for i, name := range []string{"Aida", "Tideline Manor", "Trillium Hall"} {
		showID := int64(8000 + i)
		require.NoError(t, db.Show.Create().SetID(showID).SetName(name).Exec(ctx))
	}

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet,
		"/api/v1/shows?limit=2&offset=0", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	got := decodePage(t, rr)
	assert.Equal(t, 3, got.Total)
	assert.Equal(t, 2, got.Limit)
	assert.Len(t, got.Items, 2, "first page is two of three")

	// Page 2 has the remaining show.
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequestWithContext(ctx, http.MethodGet,
		"/api/v1/shows?limit=2&offset=2", nil)
	srv.Handler().ServeHTTP(rr2, req2)
	require.Equal(t, http.StatusOK, rr2.Code, rr2.Body.String())
	got2 := decodePage(t, rr2)
	assert.Equal(t, 3, got2.Total)
	assert.Len(t, got2.Items, 1)
}

// TestPaginationEnvelopeWants asserts /api/v1/wants surfaces total +
// pages correctly.
func TestPaginationEnvelopeWants(t *testing.T) {
	t.Parallel()

	srv := fixtureBackedServer(t)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(),
		http.MethodGet, "/api/v1/wants?limit=5&offset=0", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	got := decodePage(t, rr)
	assert.Equal(t, 5, got.Limit)
	assert.Equal(t, 0, got.Offset)
	assert.Positive(t, got.Total,
		"fixture sync seeds at least one want")
	assert.LessOrEqual(t, len(got.Items), 5)
}

// TestPaginationEnvelopeHistory asserts /api/v1/history returns the
// envelope and that total reflects the kind filter.
//
//nolint:paralleltest // subtests mutate shared db state; sequential by design.
func TestPaginationEnvelopeHistory(t *testing.T) {
	ctx := t.Context()
	sqlDB, db, err := storage.OpenEnt(ctx, filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	// Seed five events: 3 ingests and 2 renames, so we can verify the
	// kind filter narrows total.
	seeds := []storage.HistoryEvent{
		{OccurredAt: base.Add(1 * time.Hour), Kind: storage.HistoryKindIngest, Summary: "ingest 1"},
		{OccurredAt: base.Add(2 * time.Hour), Kind: storage.HistoryKindIngest, Summary: "ingest 2"},
		{OccurredAt: base.Add(3 * time.Hour), Kind: storage.HistoryKindIngest, Summary: "ingest 3"},
		{OccurredAt: base.Add(4 * time.Hour), Kind: storage.HistoryKindRename, Summary: "rename 1"},
		{OccurredAt: base.Add(5 * time.Hour), Kind: storage.HistoryKindRename, Summary: "rename 2"},
	}
	for _, e := range seeds {
		_, recErr := storage.RecordEvent(ctx, db, e)
		require.NoError(t, recErr)
	}

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	// Subtests share the same db so they run sequentially (no
	// t.Parallel) — adding a row in one would otherwise race against
	// totals queried in another.
	t.Run("default_total_counts_all", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(ctx,
			http.MethodGet, "/api/v1/history", nil)
		srv.Handler().ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		got := decodePage(t, rr)
		assert.Equal(t, 5, got.Total)
		assert.Equal(t, 50, got.Limit, "history default limit is 50")
	})

	t.Run("kind_filter_narrows_total", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(ctx,
			http.MethodGet, "/api/v1/history?kind=ingest", nil)
		srv.Handler().ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		got := decodePage(t, rr)
		assert.Equal(t, 3, got.Total,
			"total reflects the kind filter, not the unfiltered table")
		assert.Len(t, got.Items, 3)
	})

	t.Run("limit_offset_pagination_works", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(ctx,
			http.MethodGet, "/api/v1/history?limit=2&offset=2", nil)
		srv.Handler().ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		got := decodePage(t, rr)
		assert.Equal(t, 5, got.Total)
		assert.Len(t, got.Items, 2)
		assert.Equal(t, 2, got.Limit)
		assert.Equal(t, 2, got.Offset)
	})

	t.Run("recording_id_filter_narrows_total", func(t *testing.T) {
		rec := int64(42)
		_, recErr := storage.RecordEvent(ctx, db, storage.HistoryEvent{
			OccurredAt:  base.Add(6 * time.Hour),
			Kind:        storage.HistoryKindIngest,
			RecordingID: &rec,
			Summary:     "ingest scoped to 42",
		})
		require.NoError(t, recErr)

		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(ctx,
			http.MethodGet, "/api/v1/history?recording_id="+strconv.FormatInt(rec, 10), nil)
		srv.Handler().ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		got := decodePage(t, rr)
		assert.Equal(t, 1, got.Total)
		assert.Len(t, got.Items, 1)
	})
}
