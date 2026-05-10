package scanner_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/ent/show"
	"github.com/nicolerenee/promptbook/internal/scanner"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// fixture bundles the moving parts a scanner test needs: a fresh DB
// with migrations applied, a clean watch dir, and an Engine pointed at
// both. Per-test instances keep cases parallel-safe.
type fixture struct {
	ctx      context.Context
	db       *ent.Client
	watchDir string
	engine   *scanner.Engine
}

// newFixture spins up a sqlite database under t.TempDir, runs every
// embedded migration, and returns an Engine ready to scan a sibling
// "watch" directory. The Engine logger is silenced so the test output
// stays focused on assertions.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := t.Context()
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "promptbook.db")
	sqlDB, db, err := storage.OpenEnt(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	watchDir := filepath.Join(tmp, "watch")
	require.NoError(t, os.MkdirAll(watchDir, 0o755))

	return &fixture{
		ctx:      ctx,
		db:       db,
		watchDir: watchDir,
		engine: &scanner.Engine{
			DB:        db,
			WatchDirs: []string{watchDir},
			Logger:    zerolog.Nop(),
		},
	}
}

// writeVideo drops a minimal video file at watchDir/name. The contents
// are nonempty so file_size_bytes is recorded as a sanity check.
func (f *fixture) writeVideo(t *testing.T, name string) string {
	t.Helper()
	full := filepath.Join(f.watchDir, name)
	require.NoError(t, os.WriteFile(full, []byte("video bytes"), 0o644))
	return full
}

// writeFile drops a file at the supplied absolute path with size-byte
// content (so file size assertions can distinguish files that differ
// only in extras-bias). Parent directories are created on demand.
func writeFile(t *testing.T, path string, size int) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = byte('a' + (i % 26))
	}
	require.NoError(t, os.WriteFile(path, buf, 0o644))
}

// seedRecording inserts a recordings row plus its parent show. Mirrors
// the helpers in the storage package's own tests but kept local so the
// scanner package doesn't depend on storage_test internals.
func seedRecording(t *testing.T, db *ent.Client, recordingID int64) {
	t.Helper()
	ctx := t.Context()
	const showID = int64(42)
	// Show may already exist across test cases — ignore conflicts.
	exists, err := db.Show.Query().Where(show.IDEQ(showID)).Exist(ctx)
	require.NoError(t, err)
	if !exists {
		require.NoError(t, db.Show.Create().SetID(showID).SetName("Some Show").Exec(ctx))
	}
	require.NoError(t, db.Recording.Create().
		SetID(recordingID).
		SetShowID(showID).
		SetRawJSON("{}").
		Exec(ctx))
}

// seedVersion inserts a recording_versions row for the given recording
// at path. seedRecording must be called first.
func seedVersion(t *testing.T, db *ent.Client, recordingID int64, path string) {
	t.Helper()
	require.NoError(t, storage.UpsertVersion(t.Context(), db, storage.RecordingVersion{
		RecordingID:   recordingID,
		FilePath:      path,
		FileSizeBytes: 123,
		FormatLabel:   "MKV",
	}))
}

func TestScanEnqueuesUnresolvedFile(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	path := f.writeVideo(t, "Greenwich Beacon-2024-03-15.mkv") // no encora-id token.

	res, err := f.engine.Scan(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Enqueued)
	assert.Empty(t, res.Errors)

	got, err := storage.ListQueue(f.ctx, f.db)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, path, got[0].FilePath)
	assert.Empty(t, got[0].SuggestedConfidence, "no id resolved → empty confidence")
	assert.Nil(t, got[0].SuggestedRecordingID)
	assert.Positive(t, got[0].FileSizeBytes)
}

func TestScanEnqueuesResolvedKnownID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	const knownID = int64(90100222)
	seedRecording(t, f.db, knownID)
	path := f.writeVideo(t, "Show [encora-90100222].mp4")

	res, err := f.engine.Scan(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Enqueued)
	assert.Empty(t, res.Errors)

	got, err := storage.ListQueue(f.ctx, f.db)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, path, got[0].FilePath)
	assert.Equal(t, storage.ConfidenceHigh, got[0].SuggestedConfidence)
	require.NotNil(t, got[0].SuggestedRecordingID)
	assert.Equal(t, knownID, *got[0].SuggestedRecordingID)
}

func TestScanEnqueuesResolvedUnknownID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	const unknownID = int64(99999)
	path := f.writeVideo(t, "Mystery [encora-99999].mkv") // not seeded.

	res, err := f.engine.Scan(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Enqueued)
	assert.Empty(t, res.Errors)

	got, err := storage.ListQueue(f.ctx, f.db)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, path, got[0].FilePath)
	assert.Equal(t, storage.ConfidenceLow, got[0].SuggestedConfidence)
	require.NotNil(t, got[0].SuggestedRecordingID)
	assert.Equal(t, unknownID, *got[0].SuggestedRecordingID)
}

func TestScanSkipsAlreadyIngestedFile(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	const recordingID = int64(1234)
	seedRecording(t, f.db, recordingID)
	path := f.writeVideo(t, "Already [encora-1234].mkv")
	seedVersion(t, f.db, recordingID, path)

	res, err := f.engine.Scan(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Enqueued)
	assert.Empty(t, res.Errors)

	got, err := storage.ListQueue(f.ctx, f.db)
	require.NoError(t, err)
	assert.Empty(t, got, "ingested file must not appear in queue")
}

func TestScanRemovesGoneFiles(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// Pre-seed a queue entry for a path that doesn't exist on disk.
	const ghostPath = "/some/missing/path.mp4"
	_, err := storage.EnqueueFile(f.ctx, f.db, storage.QueueEntry{
		FilePath:      ghostPath,
		FileSizeBytes: 99,
	})
	require.NoError(t, err)

	res, err := f.engine.Scan(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Removed)
	assert.Equal(t, 0, res.Enqueued)
	assert.Empty(t, res.Errors)

	got, err := storage.ListQueue(f.ctx, f.db)
	require.NoError(t, err)
	assert.Empty(t, got, "missing file must be removed from queue")
}

func TestScanIsIdempotent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	const knownID = int64(7777)
	seedRecording(t, f.db, knownID)
	f.writeVideo(t, "Repeat [encora-7777].mkv")
	f.writeVideo(t, "Unresolved.mp4")

	for range 2 {
		_, err := f.engine.Scan(f.ctx)
		require.NoError(t, err)
	}

	got, err := storage.ListQueue(f.ctx, f.db)
	require.NoError(t, err)
	assert.Len(t, got, 2, "scanning twice must not double-up the queue")
}

func TestScanIgnoresNonVideo(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	require.NoError(t, os.WriteFile(
		filepath.Join(f.watchDir, "notes.txt"),
		[]byte("not a video"),
		0o644,
	))

	res, err := f.engine.Scan(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Enqueued)
	assert.Empty(t, res.Errors)

	got, err := storage.ListQueue(f.ctx, f.db)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestScanTreatsFolderAsUnit covers the canonical drop pattern: a
// folder with one main video + a sibling audio/ subdir of per-track
// rips + a photos/ subdir. The scanner must produce exactly one queue
// entry pointing at the main video, with extras_count covering the
// audio tracks. Photos / non-media are ignored.
func TestScanTreatsFolderAsUnit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	folder := filepath.Join(f.watchDir,
		"Halcyon Crossing - Broadway - September, 2024 [fixturetrader14]")
	main := filepath.Join(folder,
		"Halcyon Crossing - Broadway - September, 2024.mkv")
	track1 := filepath.Join(folder, "audio", "01 - Wedding Song.mp3")
	track2 := filepath.Join(folder, "audio", "02 - Way Down Halcyon Crossing.mp3")
	track3 := filepath.Join(folder, "audio", "03 - Anyway the Wind Blows.mp3")
	photo1 := filepath.Join(folder, "photos", "backdrop.jpg")
	photo2 := filepath.Join(folder, "photos", "cast.jpg")

	// Main video is by far the largest media file; extras are smaller
	// so the heuristic doesn't have to lean on any tie-breaker.
	writeFile(t, main, 1024*1024)
	writeFile(t, track1, 4*1024)
	writeFile(t, track2, 4*1024)
	writeFile(t, track3, 4*1024)
	writeFile(t, photo1, 2*1024)
	writeFile(t, photo2, 2*1024)

	res, err := f.engine.Scan(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Enqueued, "exactly one queue row per folder")
	assert.Empty(t, res.Errors)

	got, err := storage.ListQueue(f.ctx, f.db)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, main, got[0].FilePath, "FilePath = the main video")
	assert.Equal(t, 3, got[0].ExtrasCount,
		"three audio tracks counted as extras; photos ignored")
}

// TestScanFolderUnitPicksLargestVideo verifies the largest-file
// heuristic when two media files sit at the folder root. The bigger
// one wins regardless of name order on disk.
func TestScanFolderUnitPicksLargestVideo(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	folder := filepath.Join(f.watchDir, "Some Show 2024-03-15")
	smaller := filepath.Join(folder, "trailer.mkv")
	larger := filepath.Join(folder, "main.mkv")

	writeFile(t, smaller, 8*1024)
	writeFile(t, larger, 1024*1024)

	res, err := f.engine.Scan(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Enqueued)
	assert.Empty(t, res.Errors)

	got, err := storage.ListQueue(f.ctx, f.db)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, larger, got[0].FilePath,
		"largest media file wins as the main")
	assert.Equal(t, 1, got[0].ExtrasCount,
		"the other root-level video counts as one extra")
}

// TestScanFolderUnitPrefersRootOverNested verifies the close-band
// tie-breaker: when two media files are within ~10% of each other in
// size, the root-level one beats the nested one. Mirrors a folder
// where audio/track-01.mp3 happens to be similar in size to a tiny
// teaser.mp3 the user dropped at the root.
func TestScanFolderUnitPrefersRootOverNested(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	folder := filepath.Join(f.watchDir, "Tied Sizes")
	rootClip := filepath.Join(folder, "teaser.mp3")
	nested := filepath.Join(folder, "audio", "track-01.mp3")

	// Nested file is fractionally larger but well within the 10% band,
	// so the root-level tie-breaker should still pick teaser.mp3.
	writeFile(t, rootClip, 100*1024)
	writeFile(t, nested, 102*1024)

	res, err := f.engine.Scan(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Enqueued)
	assert.Empty(t, res.Errors)

	got, err := storage.ListQueue(f.ctx, f.db)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, rootClip, got[0].FilePath,
		"close-band tie-breaker prefers root-level file")
	assert.Equal(t, 1, got[0].ExtrasCount)
}

// TestScanSkipsEmptyFolder verifies that a folder with no media files
// (only photos / readmes / etc.) is silently dropped — no queue row,
// no error.
func TestScanSkipsEmptyFolder(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	folder := filepath.Join(f.watchDir, "Just Photos")
	writeFile(t, filepath.Join(folder, "poster.jpg"), 4*1024)
	writeFile(t, filepath.Join(folder, "notes.txt"), 32)

	res, err := f.engine.Scan(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Enqueued, "empty folder produces no row")
	assert.Empty(t, res.Errors)

	got, err := storage.ListQueue(f.ctx, f.db)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestScanLooseFileExtrasCountIsZero pins the contract that loose
// top-level files still produce a queue row with extras_count == 0
// (folder-as-unit only kicks in for top-level directories).
func TestScanLooseFileExtrasCountIsZero(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	path := f.writeVideo(t, "Greenwich Beacon-2024-03-15.mkv")

	res, err := f.engine.Scan(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Enqueued)
	assert.Empty(t, res.Errors)

	got, err := storage.ListQueue(f.ctx, f.db)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, path, got[0].FilePath)
	assert.Equal(t, 0, got[0].ExtrasCount,
		"loose-file rows always have extras_count = 0")
}
