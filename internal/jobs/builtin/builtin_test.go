package builtin_test

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
	"github.com/nicolerenee/promptbook/internal/jobs"
	"github.com/nicolerenee/promptbook/internal/jobs/builtin"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// rootFixture bundles the moving parts a scan-library-root job test
// needs: a fresh DB with migrations applied, a clean library root on
// disk, and the configured job pointed at both. Per-test instances
// keep cases parallel-safe.
type rootFixture struct {
	ctx context.Context
	db  *ent.Client
	dir string
	job *builtin.ScanLibraryRootJob
}

// newRootFixture spins up a sqlite database under t.TempDir, runs
// every embedded migration, and returns a ScanLibraryRootJob ready
// to walk a sibling "lib" directory. The job's logger is silenced so
// the test output stays focused on assertions.
func newRootFixture(t *testing.T) *rootFixture {
	t.Helper()
	ctx := t.Context()
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "promptbook.db")
	sqlDB, db, err := storage.OpenEnt(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	libDir := filepath.Join(tmp, "lib")
	require.NoError(t, os.MkdirAll(libDir, 0o755))

	return &rootFixture{
		ctx: ctx,
		db:  db,
		dir: libDir,
		job: &builtin.ScanLibraryRootJob{
			DB:     db,
			Root:   libDir,
			Logger: zerolog.Nop(),
		},
	}
}

// writeFile drops a file at the supplied absolute path with size-byte
// content. Parent directories are created on demand.
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
// the helper in the scanner package's own tests but kept local so the
// builtin package doesn't depend on scanner_test internals.
func seedRecording(t *testing.T, db *ent.Client, recordingID int64) {
	t.Helper()
	ctx := t.Context()
	const showID = int64(42)
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

// seedVersion inserts a recording_versions row pointing at path.
func seedVersion(t *testing.T, db *ent.Client, recordingID int64, path string) {
	t.Helper()
	require.NoError(t, storage.UpsertVersion(t.Context(), db, storage.RecordingVersion{
		RecordingID:   recordingID,
		FilePath:      path,
		FileSizeBytes: 123,
		FormatLabel:   "MKV",
	}))
}

// TestScanLibraryRootJob_SkipsTracked is the headline contract: when a
// recording_versions row already points at one of the folders' main
// files, the job's IsTracked closure must skip it and enqueue only
// the un-tracked folder. Mirrors the orphan-backfill use case.
func TestScanLibraryRootJob_SkipsTracked(t *testing.T) {
	t.Parallel()
	f := newRootFixture(t)

	// Tracked folder: has a recording row + a recording_versions row
	// pointing at its main file. The job must skip it.
	const trackedRecordingID = int64(1111)
	seedRecording(t, f.db, trackedRecordingID)
	trackedFolder := filepath.Join(f.dir, "Already-Tracked [encora-1111]")
	trackedMain := filepath.Join(trackedFolder, "main.mkv")
	writeFile(t, trackedMain, 1024*1024)
	seedVersion(t, f.db, trackedRecordingID, trackedMain)

	// Orphan folder: no version row yet. Should land on the queue.
	orphanFolder := filepath.Join(f.dir, "Orphan [encora-2222]")
	orphanMain := filepath.Join(orphanFolder, "main.mkv")
	writeFile(t, orphanMain, 1024*1024)

	require.NoError(t, f.job.Run(f.ctx, jobs.JobArgs{}))

	got, err := storage.ListQueue(f.ctx, f.db)
	require.NoError(t, err)
	require.Len(t, got, 1, "tracked folder must be skipped; only orphan queued")
	assert.Equal(t, orphanMain, got[0].FilePath,
		"queue row must point at the orphan's main file")
}

// TestScanLibraryRootJob_NoRoot covers the configuration guard: an
// unset library.root surfaces a clear error rather than silently
// no-oping. The error wires up through the scheduler's failure
// channel so the run row carries an honest reason.
func TestScanLibraryRootJob_NoRoot(t *testing.T) {
	t.Parallel()
	job := &builtin.ScanLibraryRootJob{Logger: zerolog.Nop()}
	err := job.Run(context.Background(), jobs.JobArgs{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "library.root")
}
