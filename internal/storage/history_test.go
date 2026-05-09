package storage_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/brianvoe/gofakeit/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/storage"
)

// openTestDB creates a fresh promptbook DB in t.TempDir and registers cleanup.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "promptbook.db")
	db, err := storage.Open(dbPath)
	require.NoError(t, err, "open test db")
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// recordingPtr returns a pointer to v for compactly setting RecordingID.
//
//nolint:modernize // newexpr: explicit helper reads better at table-driven call sites.
func recordingPtr(v int64) *int64 { return &v }

func TestRecordAndListHistory(t *testing.T) {
	t.Parallel()
	gofakeit.Seed(0)

	ctx := context.Background()
	db := openTestDB(t)

	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	events := []storage.HistoryEvent{
		{
			OccurredAt: now.Add(-2 * time.Hour),
			Kind:       storage.HistoryKindIngest,
			Summary:    gofakeit.Sentence(6),
		},
		{
			OccurredAt:  now.Add(-1 * time.Hour),
			Kind:        storage.HistoryKindRename,
			RecordingID: recordingPtr(42),
			Summary:     gofakeit.Sentence(6),
		},
		{
			OccurredAt: now,
			Kind:       storage.HistoryKindNFOWrite,
			Summary:    gofakeit.Sentence(6),
		},
	}
	for i := range events {
		id, err := storage.RecordEvent(ctx, db, events[i])
		require.NoError(t, err)
		assert.NotZero(t, id)
	}

	got, err := storage.ListHistory(ctx, db, storage.ListHistoryOptions{})
	require.NoError(t, err)
	require.Len(t, got, 3)

	// DESC by occurred_at: newest first.
	assert.Equal(t, storage.HistoryKindNFOWrite, got[0].Kind)
	assert.Equal(t, storage.HistoryKindRename, got[1].Kind)
	assert.Equal(t, storage.HistoryKindIngest, got[2].Kind)

	require.NotNil(t, got[1].RecordingID)
	assert.Equal(t, int64(42), *got[1].RecordingID)
	assert.Nil(t, got[0].RecordingID)
}

func TestListHistoryFilterByKind(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestDB(t)

	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	seed := []storage.HistoryEvent{
		{OccurredAt: base.Add(1 * time.Hour), Kind: storage.HistoryKindIngest},
		{OccurredAt: base.Add(2 * time.Hour), Kind: storage.HistoryKindRename},
		{OccurredAt: base.Add(3 * time.Hour), Kind: storage.HistoryKindNFOWrite},
		{OccurredAt: base.Add(4 * time.Hour), Kind: storage.HistoryKindEncoraPush},
		{OccurredAt: base.Add(5 * time.Hour), Kind: storage.HistoryKindSync},
	}
	for _, e := range seed {
		_, err := storage.RecordEvent(ctx, db, e)
		require.NoError(t, err)
	}

	tests := []struct {
		name      string
		kinds     []string
		wantKinds []string
	}{
		{
			name:  "no filter returns all",
			kinds: nil,
			wantKinds: []string{
				storage.HistoryKindSync,
				storage.HistoryKindEncoraPush,
				storage.HistoryKindNFOWrite,
				storage.HistoryKindRename,
				storage.HistoryKindIngest,
			},
		},
		{
			name:      "single kind",
			kinds:     []string{storage.HistoryKindRename},
			wantKinds: []string{storage.HistoryKindRename},
		},
		{
			name:      "multi-kind any-of",
			kinds:     []string{storage.HistoryKindIngest, storage.HistoryKindSync},
			wantKinds: []string{storage.HistoryKindSync, storage.HistoryKindIngest},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := storage.ListHistory(ctx, db, storage.ListHistoryOptions{Kinds: tt.kinds})
			require.NoError(t, err)
			gotKinds := make([]string, len(got))
			for i, e := range got {
				gotKinds[i] = e.Kind
			}
			assert.Equal(t, tt.wantKinds, gotKinds)
		})
	}
}

func TestListHistoryFilterByRecordingID(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestDB(t)

	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for _, e := range []storage.HistoryEvent{
		{OccurredAt: base.Add(1 * time.Hour), Kind: storage.HistoryKindIngest, RecordingID: recordingPtr(1)},
		{OccurredAt: base.Add(2 * time.Hour), Kind: storage.HistoryKindRename, RecordingID: recordingPtr(1)},
		{OccurredAt: base.Add(3 * time.Hour), Kind: storage.HistoryKindIngest, RecordingID: recordingPtr(2)},
		{OccurredAt: base.Add(4 * time.Hour), Kind: storage.HistoryKindSync},
	} {
		_, err := storage.RecordEvent(ctx, db, e)
		require.NoError(t, err)
	}

	got, err := storage.ListHistory(ctx, db, storage.ListHistoryOptions{RecordingID: recordingPtr(1)})
	require.NoError(t, err)
	require.Len(t, got, 2)
	for _, ev := range got {
		require.NotNil(t, ev.RecordingID)
		assert.Equal(t, int64(1), *ev.RecordingID)
	}
}

func TestListHistoryTimeRange(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestDB(t)

	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for i := range 5 {
		_, err := storage.RecordEvent(ctx, db, storage.HistoryEvent{
			OccurredAt: base.Add(time.Duration(i) * 24 * time.Hour),
			Kind:       storage.HistoryKindIngest,
			Summary:    "day " + time.Duration(i).String(),
		})
		require.NoError(t, err)
	}

	tests := []struct {
		name    string
		since   time.Time
		until   time.Time
		wantLen int
	}{
		{
			name:    "unbounded returns all",
			since:   time.Time{},
			until:   time.Time{},
			wantLen: 5,
		},
		{
			name:    "since only",
			since:   base.Add(2 * 24 * time.Hour),
			until:   time.Time{},
			wantLen: 3,
		},
		{
			name:    "until only",
			since:   time.Time{},
			until:   base.Add(2 * 24 * time.Hour),
			wantLen: 2,
		},
		{
			name:    "since and until window",
			since:   base.Add(1 * 24 * time.Hour),
			until:   base.Add(4 * 24 * time.Hour),
			wantLen: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := storage.ListHistory(ctx, db, storage.ListHistoryOptions{Since: tt.since, Until: tt.until})
			require.NoError(t, err)
			assert.Len(t, got, tt.wantLen)
		})
	}
}

func TestListHistoryLimit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestDB(t)

	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for i := range 5 {
		_, err := storage.RecordEvent(ctx, db, storage.HistoryEvent{
			OccurredAt: base.Add(time.Duration(i) * time.Hour),
			Kind:       storage.HistoryKindIngest,
		})
		require.NoError(t, err)
	}

	first, err := storage.ListHistory(ctx, db, storage.ListHistoryOptions{Limit: 2})
	require.NoError(t, err)
	require.Len(t, first, 2)

	next, err := storage.ListHistory(ctx, db, storage.ListHistoryOptions{Limit: 2, Offset: 2})
	require.NoError(t, err)
	require.Len(t, next, 2)

	// No overlap between page 1 and page 2.
	for _, a := range first {
		for _, b := range next {
			assert.NotEqual(t, a.ID, b.ID)
		}
	}
}

func TestPruneHistoryOlderThan(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestDB(t)

	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for i := range 5 {
		_, err := storage.RecordEvent(ctx, db, storage.HistoryEvent{
			OccurredAt: base.Add(time.Duration(i) * 24 * time.Hour),
			Kind:       storage.HistoryKindIngest,
		})
		require.NoError(t, err)
	}

	// Cutoff at base + 2 days: events at days 0 and 1 are older (count 2),
	// events at days 2, 3, 4 remain.
	cutoff := base.Add(2 * 24 * time.Hour)
	deleted, err := storage.PruneHistoryOlderThan(ctx, db, cutoff)
	require.NoError(t, err)
	assert.Equal(t, int64(2), deleted)

	remaining, err := storage.ListHistory(ctx, db, storage.ListHistoryOptions{})
	require.NoError(t, err)
	assert.Len(t, remaining, 3)
	for _, ev := range remaining {
		assert.False(t, ev.OccurredAt.Before(cutoff), "row at %s should not be older than cutoff", ev.OccurredAt)
	}
}

func TestRecordEventDetailsRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestDB(t)

	details := map[string]any{
		"src":         "/store01/incoming/foo.mkv",
		"dst":         "/store01/Performances/Halcyon Crossing (2019) [encora-12345]/Halcyon Crossing.mkv",
		"bytes":       float64(1234567890),
		"dry_run":     false,
		"matched_via": "filename-pattern",
		"tags":        []any{"master", "av1"},
	}

	id, err := storage.RecordEvent(ctx, db, storage.HistoryEvent{
		Kind:        storage.HistoryKindRename,
		RecordingID: recordingPtr(12345),
		Summary:     "rename foo.mkv into canonical layout",
		Details:     details,
	})
	require.NoError(t, err)
	assert.NotZero(t, id)

	got, err := storage.ListHistory(ctx, db, storage.ListHistoryOptions{Kinds: []string{storage.HistoryKindRename}})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, details, got[0].Details)
}
