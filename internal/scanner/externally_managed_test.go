package scanner_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/ingest"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// TestScanDetectsExternallyManagedDrift covers the path-drift case:
// the user previously imported recording 90100222 in externally-managed
// mode at /watch/Old Folder/main.mkv (so a recording_versions row
// points there), then Radarr renamed the parent folder to /watch/
// New Folder/. The scanner finds the sentinel + .encora-id in the
// new folder, recognizes the recording, and updates the version
// row's file_path in place rather than re-enqueueing the file.
func TestScanDetectsExternallyManagedDrift(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	const recordingID = int64(90100222)
	seedRecording(t, f.db, recordingID)

	// Stale path the recording_versions row currently points at —
	// the file is NOT actually here on disk anymore (Radarr moved
	// it), but the row still references it.
	stalePath := filepath.Join(f.watchDir, "Old Folder", "main.mkv")
	seedVersion(t, f.db, recordingID, stalePath)

	// New path the file lives at after Radarr's rename. Sentinel +
	// .encora-id ride along so the scanner can recognize the folder
	// as externally-managed.
	newFolder := filepath.Join(f.watchDir, "New Folder")
	require.NoError(t, os.MkdirAll(newFolder, 0o755))
	newPath := filepath.Join(newFolder, "main.mkv")
	require.NoError(t, os.WriteFile(newPath, []byte("video bytes"), 0o644))
	require.NoError(t, os.WriteFile(
		filepath.Join(newFolder, ingest.ExternallyManagedSentinel),
		nil, 0o644))
	require.NoError(t, os.WriteFile(
		filepath.Join(newFolder, ".encora-id"),
		[]byte("90100222\n"), 0o644))

	res, err := f.engine.Scan(f.ctx)
	require.NoError(t, err)
	assert.Empty(t, res.Errors)
	assert.Zero(t, res.Enqueued,
		"externally-managed drift must NOT enqueue the folder")

	// Queue stays empty.
	queue, err := storage.ListQueue(f.ctx, f.db)
	require.NoError(t, err)
	assert.Empty(t, queue,
		"externally-managed drift must not create any queue rows")

	// Version row updated to the new path.
	versions, err := storage.ListVersions(f.ctx, f.db, recordingID)
	require.NoError(t, err)
	require.Len(t, versions, 1)
	assert.Equal(t, newPath, versions[0].FilePath,
		"recording_versions.file_path must follow the file to the new folder")
}

// TestScanIgnoresOrphanSentinel verifies that a sentinel without a
// matching local recording (no .encora-id sidecar — Resolve fails)
// falls through to the normal scan path. The folder gets enqueued
// like any other unrecognized folder.
func TestScanIgnoresOrphanSentinel(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	folder := filepath.Join(f.watchDir, "Mystery Folder")
	require.NoError(t, os.MkdirAll(folder, 0o755))
	mainPath := filepath.Join(folder, "main.mkv")
	require.NoError(t, os.WriteFile(mainPath, []byte("video bytes"), 0o644))
	// Sentinel present, but no .encora-id — orphan case.
	require.NoError(t, os.WriteFile(
		filepath.Join(folder, ingest.ExternallyManagedSentinel),
		nil, 0o644))

	res, err := f.engine.Scan(f.ctx)
	require.NoError(t, err)
	assert.Empty(t, res.Errors)
	assert.Equal(t, 1, res.Enqueued,
		"orphan sentinel falls through to normal scan + enqueue")

	queue, err := storage.ListQueue(f.ctx, f.db)
	require.NoError(t, err)
	require.Len(t, queue, 1)
	assert.Equal(t, mainPath, queue[0].FilePath)
}
