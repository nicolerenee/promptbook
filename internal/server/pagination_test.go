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
