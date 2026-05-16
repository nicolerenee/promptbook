package scanner_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

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
// entry pointing at the main video, with extras_count covering EVERY
// non-main file (audio tracks + photos). The user explicitly asked
// for every file in a folder-as-unit drop to be tracked + preserved
// on import; the only filter is hidden / dot-prefixed OS files.
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
	assert.Equal(t, 5, got[0].ExtrasCount,
		"all five non-main files counted as extras (3 audio + 2 photos)")
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

// TestScanSkipsTrackedFiles covers the IsTracked-callback hook used
// by the scan-library-root job. The scanner asks the callback for
// each main file before enqueueing; a "true" answer means the file
// is already represented in recording_versions (or any equivalent
// signal) and should be skipped silently. Two folders are scanned;
// the callback returns true for one of them and the other should be
// the only row that lands on the queue.
func TestScanSkipsTrackedFiles(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// Two unit-sized folders. The scanner walks one level deep, so
	// each folder produces exactly one main-file candidate.
	trackedFolder := filepath.Join(f.watchDir, "Already-Tracked [encora-1111]")
	trackedMain := filepath.Join(trackedFolder, "main.mkv")
	orphanFolder := filepath.Join(f.watchDir, "Orphan [encora-2222]")
	orphanMain := filepath.Join(orphanFolder, "main.mkv")
	writeFile(t, trackedMain, 1024*1024)
	writeFile(t, orphanMain, 1024*1024)

	// IsTracked answers true only for the tracked folder's main file.
	// The scanner must therefore skip the tracked folder and enqueue
	// only the orphan.
	f.engine.IsTracked = func(path string) bool {
		return path == trackedMain
	}

	res, err := f.engine.Scan(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Enqueued, "tracked file must be skipped; orphan enqueued")
	assert.Empty(t, res.Errors)

	got, err := storage.ListQueue(f.ctx, f.db)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, orphanMain, got[0].FilePath,
		"only the orphan main file should land on the queue")
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

// TestScanSkipsInFlightLooseFile covers the rclone-partial bug. A
// loose `<name>.mp4.<chunk>.partial` file at the watched-dir root is
// rclone's in-flight marker; enqueuing it would strand a queue row
// keyed on a path that's about to be renamed when the transfer
// finishes. The scan must skip it, leaving the queue empty.
func TestScanSkipsInFlightLooseFile(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	f.writeVideo(t, "Sextet - Coastal Tour - November, 2024 [fixturetrader14].mp4.780b8258.partial")

	res, err := f.engine.Scan(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Enqueued, "in-flight file must not be enqueued")
	assert.Empty(t, res.Errors)

	got, err := storage.ListQueue(f.ctx, f.db)
	require.NoError(t, err)
	assert.Empty(t, got, "in-flight partial file must not leak into the queue")
}

// TestScanSkipsInFlightInsideFolderUnit covers the folder-as-unit
// branch: if a folder contains a settled main video AND a still-
// downloading sibling, the partial must be excluded from the
// classification so it neither shows up as an extra nor risks
// becoming the chosen main on a future scan once it grows past the
// real file.
func TestScanSkipsInFlightInsideFolderUnit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	folder := filepath.Join(f.watchDir, "Some Show - 2024-03-15")
	settled := filepath.Join(folder, "main.mkv")
	rclonePartial := filepath.Join(folder, "main.mkv.780b8258.partial")
	chromePartial := filepath.Join(folder, "trailer.mp4.crdownload")
	rcloneTmp := filepath.Join(folder, ".main.mkv.rclone-tmp123")

	writeFile(t, settled, 1024*1024)
	writeFile(t, rclonePartial, 4*1024*1024) // larger than settled on purpose.
	writeFile(t, chromePartial, 2*1024*1024)
	writeFile(t, rcloneTmp, 8*1024*1024)

	res, err := f.engine.Scan(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Enqueued, "exactly one queue row for the folder")
	assert.Empty(t, res.Errors)

	got, err := storage.ListQueue(f.ctx, f.db)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, settled, got[0].FilePath,
		"main must be the settled file, not the larger .partial")
	assert.Equal(t, 0, got[0].ExtrasCount,
		"in-flight files must not be counted as extras")
}

// TestScanEvictsStaleQueueRow covers the "rescan should be fresh"
// requirement. We pre-seed a queue row whose file_path lives under the
// watched dir but whose actual file no longer exists on disk (the
// canonical "rclone renamed `.partial` -> real name and the old row
// was never cleaned up" shape). A subsequent Scan must drop the stale
// row and enqueue the settled file, even when the queue already
// contained an entry that looked plausible.
func TestScanEvictsStaleQueueRow(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// Seed a stale row that points under the watched dir but at a path
	// that no longer exists (simulating an old rclone-partial enqueue
	// from a buggier version of the scanner).
	stalePath := filepath.Join(f.watchDir,
		"Show - 2024-03-15.mp4.780b8258.partial")
	_, err := storage.EnqueueFile(f.ctx, f.db, storage.QueueEntry{
		FilePath:      stalePath,
		FileSizeBytes: 1234,
	})
	require.NoError(t, err)

	// Now drop the settled file at the destination name and rescan.
	settled := f.writeVideo(t, "Show - 2024-03-15.mp4")

	res, err := f.engine.Scan(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Enqueued)
	// reconcileMissing handles the stat-not-found path; the explicit
	// "Removed" count covers either the reconcile-time delete or the
	// end-of-pass eviction, whichever caught it first.
	assert.GreaterOrEqual(t, res.Removed, 1, "stale row must be removed")
	assert.Empty(t, res.Errors)

	got, err := storage.ListQueue(f.ctx, f.db)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, settled, got[0].FilePath,
		"only the settled file should remain in the queue")
}

// TestScanEvictsStaleRowWithSurvivingFile pins the bit
// reconcileMissing alone can't fix: a queue row whose backing path
// still exists, but which the current walk no longer produces (e.g.
// the folder was reorganized and the previous "main" is now an extra
// of a different recording). The end-of-pass eviction must drop it
// because its last_seen_at predates the pass start.
func TestScanEvictsStaleRowWithSurvivingFile(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// Pre-seed a row under the watch dir whose file actually exists,
	// so reconcileMissing won't touch it. We then run a Scan with no
	// matching content on disk for that row (the walk doesn't visit
	// it because the file lives in a sibling location), so the only
	// thing that can evict it is the LastSeenAt-based sweep.
	orphan := filepath.Join(f.watchDir, "abandoned.mkv")
	writeFile(t, orphan, 1024)
	_, err := storage.EnqueueFile(f.ctx, f.db, storage.QueueEntry{
		FilePath:      orphan,
		FileSizeBytes: 1024,
	})
	require.NoError(t, err)

	// Engine.IsTracked returns true for the orphan path so the walk
	// finds the file but refuses to re-enqueue it. This is the closest
	// stand-in for "the walk no longer wants this row" without needing
	// a full ingest fixture.
	f.engine.IsTracked = func(path string) bool { return path == orphan }

	// Sleep a hair so EnqueueFile's last_seen_at < scanStartedAt for
	// the Scan call below. time.Now is sub-millisecond precise on
	// linux/darwin but ent-stored datetimes are second-resolution in
	// sqlite, so we pad past the second boundary.
	time.Sleep(1100 * time.Millisecond)

	res, err := f.engine.Scan(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Removed,
		"surviving file whose row went un-touched this pass must be evicted")
	assert.Empty(t, res.Errors)

	got, err := storage.ListQueue(f.ctx, f.db)
	require.NoError(t, err)
	assert.Empty(t, got, "stale row must be gone after the sweep")
}

// TestScanEvictionScopedToWatchDirs guarantees the end-of-pass sweep
// doesn't touch rows that live OUTSIDE any of Engine.WatchDirs. We
// stash a real file in a sibling tempdir, enqueue it, and verify the
// scanner leaves it alone: reconcileMissing can't drop it (the file
// stats fine) and the eviction sweep's WatchDirs prefix filter must
// exclude it.
func TestScanEvictionScopedToWatchDirs(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	sibling := filepath.Join(filepath.Dir(f.watchDir), "elsewhere")
	require.NoError(t, os.MkdirAll(sibling, 0o755))
	external := filepath.Join(sibling, "external.mkv")
	writeFile(t, external, 1024)
	_, err := storage.EnqueueFile(f.ctx, f.db, storage.QueueEntry{
		FilePath:      external,
		FileSizeBytes: 1024,
	})
	require.NoError(t, err)

	// Pad past the second boundary so the seeded row's last_seen_at
	// predates scanStartedAt — that's the exact precondition where a
	// missing prefix scope would otherwise let the sweep evict it.
	time.Sleep(1100 * time.Millisecond)

	res, err := f.engine.Scan(f.ctx)
	require.NoError(t, err)
	assert.Empty(t, res.Errors)
	assert.Equal(t, 0, res.Removed,
		"sweep must not touch rows outside Engine.WatchDirs")

	got, err := storage.ListQueue(f.ctx, f.db)
	require.NoError(t, err)
	require.Len(t, got, 1, "external row must survive the scan")
	assert.Equal(t, external, got[0].FilePath)
}
