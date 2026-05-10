package storage_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/brianvoe/gofakeit/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// openQueueDB spins up a fresh sqlite database and runs migrations. Each
// test gets its own file to keep cases parallel-safe.
func openQueueDB(t *testing.T) *ent.Client {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "promptbook.db")
	sqlDB, db, err := storage.OpenEnt(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// makeEntry returns a QueueEntry with synthetic-but-deterministic fields.
// Caller can override FilePath when uniqueness matters.
func makeEntry(path string) storage.QueueEntry {
	id := int64(gofakeit.IntRange(1, 9999))
	return storage.QueueEntry{
		FilePath:             path,
		FileSizeBytes:        int64(gofakeit.IntRange(1, 1<<30)),
		SuggestedRecordingID: &id,
		SuggestedConfidence:  storage.ConfidenceMedium,
		Notes:                gofakeit.Sentence(5),
	}
}

func TestEnqueueAndListQueue(t *testing.T) {
	t.Parallel()
	gofakeit.Seed(0)
	db := openQueueDB(t)
	ctx := t.Context()

	paths := []string{
		"/store01/Performances/a.mkv",
		"/store01/Performances/b.mkv",
		"/store01/Performances/c.mkv",
	}
	wantIDs := make([]int64, 0, len(paths))
	for _, p := range paths {
		id, err := storage.EnqueueFile(ctx, db, makeEntry(p))
		require.NoError(t, err)
		require.Positive(t, id)
		wantIDs = append(wantIDs, id)
	}

	got, err := storage.ListQueue(ctx, db)
	require.NoError(t, err)
	require.Len(t, got, len(paths))

	// IDs are autoincrement, so insertion order == ascending id order ==
	// ascending discovered_at order (modulo same-tick ties broken by id).
	for i, entry := range got {
		assert.Equal(t, wantIDs[i], entry.ID)
		assert.Equal(t, paths[i], entry.FilePath)
		assert.False(t, entry.DiscoveredAt.IsZero(), "discovered_at must be populated")
		assert.False(t, entry.LastSeenAt.IsZero(), "last_seen_at must be populated")
	}
}

func TestEnqueueIsIdempotent(t *testing.T) {
	t.Parallel()
	gofakeit.Seed(0)
	db := openQueueDB(t)
	ctx := t.Context()

	const path = "/store01/Performances/dup.mkv"
	firstID, err := storage.EnqueueFile(ctx, db, storage.QueueEntry{
		FilePath:            path,
		FileSizeBytes:       100,
		SuggestedConfidence: storage.ConfidenceLow,
		Notes:               "first sighting",
	})
	require.NoError(t, err)

	suggested := int64(90100222)
	secondID, err := storage.EnqueueFile(ctx, db, storage.QueueEntry{
		FilePath:             path,
		FileSizeBytes:        2048,
		SuggestedRecordingID: &suggested,
		SuggestedConfidence:  storage.ConfidenceHigh,
		Notes:                "second sighting",
	})
	require.NoError(t, err)
	assert.Equal(t, firstID, secondID, "upsert must reuse the same row id")

	got, err := storage.ListQueue(ctx, db)
	require.NoError(t, err)
	require.Len(t, got, 1, "duplicate enqueue must not produce a second row")

	entry := got[0]
	assert.Equal(t, int64(2048), entry.FileSizeBytes)
	assert.Equal(t, storage.ConfidenceHigh, entry.SuggestedConfidence)
	assert.Equal(t, "second sighting", entry.Notes)
	require.NotNil(t, entry.SuggestedRecordingID)
	assert.Equal(t, suggested, *entry.SuggestedRecordingID)
}

func TestEnqueuePreservesDiscoveredAt(t *testing.T) {
	t.Parallel()
	gofakeit.Seed(0)
	db := openQueueDB(t)
	ctx := t.Context()

	const path = "/store01/Performances/preserve.mkv"
	id, err := storage.EnqueueFile(ctx, db, makeEntry(path))
	require.NoError(t, err)

	// Backdate discovered_at and last_seen_at so we can assert that the
	// next upsert touches one but not the other.
	backdated := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err = db.ManualImportQueue.UpdateOneID(int(id)).
		SetDiscoveredAt(backdated).
		SetLastSeenAt(backdated).
		Save(ctx)
	require.NoError(t, err)

	loaded, err := storage.LoadQueueEntry(ctx, db, id)
	require.NoError(t, err)
	originalDiscoveredAt := loaded.DiscoveredAt
	originalLastSeenAt := loaded.LastSeenAt

	// Re-enqueue.
	_, err = storage.EnqueueFile(ctx, db, makeEntry(path))
	require.NoError(t, err)

	after, err := storage.LoadQueueEntry(ctx, db, id)
	require.NoError(t, err)
	assert.Truef(
		t,
		after.DiscoveredAt.Equal(originalDiscoveredAt),
		"discovered_at must not change on re-enqueue (was %s, got %s)",
		originalDiscoveredAt, after.DiscoveredAt,
	)
	assert.Truef(
		t,
		after.LastSeenAt.After(originalLastSeenAt),
		"last_seen_at must move forward on re-enqueue (was %s, got %s)",
		originalLastSeenAt, after.LastSeenAt,
	)
}

func TestRemoveQueueEntry(t *testing.T) {
	t.Parallel()
	gofakeit.Seed(0)
	db := openQueueDB(t)
	ctx := t.Context()

	id, err := storage.EnqueueFile(ctx, db, makeEntry("/store01/Performances/remove-by-id.mkv"))
	require.NoError(t, err)

	require.NoError(t, storage.RemoveQueueEntry(ctx, db, id))

	_, err = storage.LoadQueueEntry(ctx, db, id)
	require.ErrorIs(t, err, storage.ErrQueueEntryNotFound)

	got, err := storage.ListQueue(ctx, db)
	require.NoError(t, err)
	assert.Empty(t, got)

	// Removing a non-existent id is a no-op, not an error.
	assert.NoError(t, storage.RemoveQueueEntry(ctx, db, id))
}

func TestRemoveQueueEntryByPath(t *testing.T) {
	t.Parallel()
	gofakeit.Seed(0)
	db := openQueueDB(t)
	ctx := t.Context()

	const keepPath = "/store01/Performances/keep.mkv"
	const dropPath = "/store01/Performances/drop.mkv"

	keepID, err := storage.EnqueueFile(ctx, db, makeEntry(keepPath))
	require.NoError(t, err)
	_, err = storage.EnqueueFile(ctx, db, makeEntry(dropPath))
	require.NoError(t, err)

	require.NoError(t, storage.RemoveQueueEntryByPath(ctx, db, dropPath))

	got, err := storage.ListQueue(ctx, db)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, keepID, got[0].ID)
	assert.Equal(t, keepPath, got[0].FilePath)

	// Removing a non-existent path is a no-op, not an error.
	assert.NoError(t, storage.RemoveQueueEntryByPath(ctx, db, dropPath))
}

func TestLoadQueueEntryNotFound(t *testing.T) {
	t.Parallel()
	db := openQueueDB(t)

	_, err := storage.LoadQueueEntry(context.Background(), db, 99999)
	require.Error(t, err)
	assert.ErrorIs(t, err, storage.ErrQueueEntryNotFound)
}
