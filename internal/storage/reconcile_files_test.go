package storage_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/ent/show"
	"github.com/nicolerenee/promptbook/internal/storage"
)

func seedRecordingForReconcile(t *testing.T, db *ent.Client, recordingID int64) {
	t.Helper()
	ctx := t.Context()
	const showID = int64(42)
	exists, err := db.Show.Query().Where(show.IDEQ(showID)).Exist(ctx)
	require.NoError(t, err)
	if !exists {
		require.NoError(t, db.Show.Create().SetID(showID).SetName("Show").Exec(ctx))
	}
	require.NoError(t, db.Recording.Create().
		SetID(recordingID).
		SetShowID(showID).
		SetRawJSON("{}").
		Exec(ctx))
}

func newReconcileDB(t *testing.T) *ent.Client {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "promptbook.db")
	sqlDB, db, err := storage.OpenEnt(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// TestReconcileRecordingFiles_SwapsRenamedFile is the headline
// contract: a single-version recording whose file got renamed in
// place (Radarr upgraded the encode and bumped the basename) should
// have its recording_versions row's file_path repointed at the new
// file.
func TestReconcileRecordingFiles_SwapsRenamedFile(t *testing.T) {
	t.Parallel()
	db := newReconcileDB(t)
	ctx := t.Context()

	const recID = int64(1111)
	seedRecordingForReconcile(t, db, recID)

	folder := t.TempDir()
	oldPath := filepath.Join(folder, "Waitress.mkv")
	newPath := filepath.Join(folder, "Waitress.2160p.mkv")
	// Don't write the OLD file — it's gone, replaced by the new one.
	require.NoError(t, os.WriteFile(newPath, []byte("4k bytes"), 0o644))

	require.NoError(t, storage.UpsertVersion(ctx, db, storage.RecordingVersion{
		RecordingID:   recID,
		FilePath:      oldPath,
		FileSizeBytes: 4 * 1024 * 1024 * 1024,
	}))

	changes, err := storage.ReconcileRecordingFiles(ctx, db, recID)
	require.NoError(t, err)
	require.Len(t, changes, 1, "the single stale row should pair with the single new file")
	assert.Equal(t, oldPath, changes[0].OldPath)
	assert.Equal(t, newPath, changes[0].NewPath)

	got, err := storage.ListVersions(ctx, db, recID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, newPath, got[0].FilePath,
		"version row's file_path must point at the surviving on-disk file")
}

// TestReconcileRecordingFiles_NoChangeWhenAllFresh pins the no-op
// case: when every version's file_path still exists on disk, the
// reconciler returns zero changes and leaves rows alone.
func TestReconcileRecordingFiles_NoChangeWhenAllFresh(t *testing.T) {
	t.Parallel()
	db := newReconcileDB(t)
	ctx := t.Context()

	const recID = int64(2222)
	seedRecordingForReconcile(t, db, recID)

	folder := t.TempDir()
	main := filepath.Join(folder, "Show.mkv")
	require.NoError(t, os.WriteFile(main, []byte("bytes"), 0o644))

	require.NoError(t, storage.UpsertVersion(ctx, db, storage.RecordingVersion{
		RecordingID:   recID,
		FilePath:      main,
		FileSizeBytes: 5,
	}))

	changes, err := storage.ReconcileRecordingFiles(ctx, db, recID)
	require.NoError(t, err)
	assert.Empty(t, changes,
		"every version's file_path still exists; reconciler must be a no-op")
}

// TestReconcileRecordingFiles_AmbiguousLeavesAlone pins the safety
// guard: when the counts don't match (one stale row, two unmatched
// files — or zero unmatched), the reconciler refuses to guess and
// leaves the rows alone. The user resolves via the queue.
func TestReconcileRecordingFiles_AmbiguousLeavesAlone(t *testing.T) {
	t.Parallel()
	db := newReconcileDB(t)
	ctx := t.Context()

	const recID = int64(3333)
	seedRecordingForReconcile(t, db, recID)

	folder := t.TempDir()
	oldPath := filepath.Join(folder, "Original.mkv")
	// Two new files in the folder, no original — the reconciler can't
	// tell which one the version row should pair with.
	require.NoError(t, os.WriteFile(filepath.Join(folder, "Candidate.A.mkv"), []byte("a"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(folder, "Candidate.B.mkv"), []byte("b"), 0o644))

	require.NoError(t, storage.UpsertVersion(ctx, db, storage.RecordingVersion{
		RecordingID:   recID,
		FilePath:      oldPath,
		FileSizeBytes: 1,
	}))

	changes, err := storage.ReconcileRecordingFiles(ctx, db, recID)
	require.NoError(t, err)
	assert.Empty(t, changes,
		"ambiguous count mismatch must not auto-swap; leave for the user")

	got, err := storage.ListVersions(ctx, db, recID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, oldPath, got[0].FilePath,
		"stale path must remain so the recording surfaces as Missing for review")
}

// TestReconcileRecordingFiles_MultipartUpgrade pins the multipart
// case: two stale rows, two unmatched files in the same folder, both
// rows get reassigned by basename order so part-1 stays part-1.
func TestReconcileRecordingFiles_MultipartUpgrade(t *testing.T) {
	t.Parallel()
	db := newReconcileDB(t)
	ctx := t.Context()

	const recID = int64(4444)
	seedRecordingForReconcile(t, db, recID)

	folder := t.TempDir()
	oldA := filepath.Join(folder, "Show.act-1.mkv")
	oldB := filepath.Join(folder, "Show.act-2.mkv")
	newA := filepath.Join(folder, "Show.2160p.act-1.mkv")
	newB := filepath.Join(folder, "Show.2160p.act-2.mkv")
	require.NoError(t, os.WriteFile(newA, []byte("a"), 0o644))
	require.NoError(t, os.WriteFile(newB, []byte("b"), 0o644))

	require.NoError(t, storage.UpsertVersion(ctx, db, storage.RecordingVersion{
		RecordingID: recID, FilePath: oldA, FileSizeBytes: 1, PartIndex: 1,
	}))
	require.NoError(t, storage.UpsertVersion(ctx, db, storage.RecordingVersion{
		RecordingID: recID, FilePath: oldB, FileSizeBytes: 1, PartIndex: 2,
	}))

	changes, err := storage.ReconcileRecordingFiles(ctx, db, recID)
	require.NoError(t, err)
	require.Len(t, changes, 2)

	got, err := storage.ListVersions(ctx, db, recID)
	require.NoError(t, err)
	require.Len(t, got, 2)
	paths := []string{got[0].FilePath, got[1].FilePath}
	assert.Contains(t, paths, newA)
	assert.Contains(t, paths, newB)
}
