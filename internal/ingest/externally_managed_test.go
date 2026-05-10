package ingest_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/ingest"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// TestIngest_ExternallyManagedSingleFile pins the catalog-only
// single-file ingest: the source file MUST stay where it is, no
// movie.nfo is written, the .encora-id sidecar + sentinel are dropped
// next to the source, and the recording row's externally_managed flag
// is flipped to true. The recording_versions row's file_path mirrors
// the source path verbatim.
func TestIngest_ExternallyManagedSingleFile(t *testing.T) {
	t.Parallel()

	dbPath := seededDBPath(t)
	sqlDB, db, err := storage.OpenEnt(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "Marigold [encora-90100222].mkv")
	srcBytes := []byte("video bytes — DO NOT MOVE")
	require.NoError(t, os.WriteFile(src, srcBytes, 0o644))

	libRoot := filepath.Join(t.TempDir(), "library")
	fetcher := &stubFetcher{}
	engine := &ingest.Engine{
		DB:              db,
		Client:          &stubClient{},
		LibraryRoot:     libRoot,
		FolderTemplate:  "{Show} - {Tour} - {Date} [encora-{EncoraID}]",
		FileTemplate:    "{Show} - {Tour} - {Date} [{Master}]",
		SubtitleFetcher: fetcher,
		Prober:          defaultStubProber(),
	}

	res, err := engine.Ingest(t.Context(), src,
		ingest.Options{ExternallyManaged: true})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	item := res.Items[0]
	require.NoError(t, item.Err)
	assert.Equal(t, ingest.ActionMoved, item.Action)
	assert.True(t, item.ExternallyManaged,
		"item must carry the externally-managed flag for downstream UI")

	// Source file must still exist with its original bytes — the
	// catalog-only flow never touches the file.
	got, err := os.ReadFile(src)
	require.NoError(t, err, "source file must still exist after ingest")
	assert.Equal(t, srcBytes, got, "source bytes must be untouched")

	// Library root must remain empty — no canonical folder, no NFO.
	if entries, statErr := os.ReadDir(libRoot); statErr == nil {
		assert.Empty(t, entries,
			"library root must remain empty for externally-managed imports")
	}

	// Sidecar + sentinel landed next to the source.
	sentinel := filepath.Join(srcDir, ingest.ExternallyManagedSentinel)
	_, err = os.Stat(sentinel)
	require.NoError(t, err, "sentinel file must be present in source folder")

	encoraID := filepath.Join(srcDir, ".encora-id")
	idBytes, err := os.ReadFile(encoraID)
	require.NoError(t, err, ".encora-id must be present in source folder")
	assert.Contains(t, string(idBytes), "90100222",
		".encora-id sidecar carries the recording id")

	// The version row points at the source path verbatim.
	versions, err := storage.ListVersions(t.Context(), db, item.EncoraID)
	require.NoError(t, err)
	require.Len(t, versions, 1)
	assert.Equal(t, src, versions[0].FilePath,
		"version row's file_path must equal the source path verbatim")

	// Recording row's externally_managed flag is true.
	loaded, err := storage.LoadRecording(t.Context(), db, item.EncoraID)
	require.NoError(t, err)
	assert.True(t, loaded.ExternallyManaged,
		"recording row's externally_managed flag must flip to true")

	// Subtitles fetcher is never called: the catalog-only path skips
	// every Encora-write workflow even when has_subtitles=true.
	assert.False(t, fetcher.called,
		"subtitle fetcher must not run for externally-managed imports")

	// NFO path is empty — the catalog-only path never writes
	// movie.nfo.
	assert.Empty(t, item.NFOPath,
		"NFOPath must be empty for externally-managed imports")
}

// TestIngest_ExternallyManagedMultipart pins the catalog-only
// multipart flow: each part stays at its source path, every part gets
// its own version row, the sentinel goes in the folder once, and the
// recording row's externally_managed flag flips to true.
func TestIngest_ExternallyManagedMultipart(t *testing.T) {
	t.Parallel()

	dbPath := seededDBPath(t)
	sqlDB, db, err := storage.OpenEnt(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	srcDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, ".encora-id"),
		[]byte("90100222\n"), 0o644))
	part1 := filepath.Join(srcDir, "act-1.mkv")
	part2 := filepath.Join(srcDir, "act-2.mkv")
	require.NoError(t, os.WriteFile(part1, []byte("act 1 bytes"), 0o644))
	require.NoError(t, os.WriteFile(part2, []byte("act 2 bytes"), 0o644))

	libRoot := filepath.Join(t.TempDir(), "library")
	engine := &ingest.Engine{
		DB:             db,
		Client:         &stubClient{},
		LibraryRoot:    libRoot,
		FolderTemplate: "{Show} [encora-{EncoraID}]",
		FileTemplate:   "{Show}[ - part-{Part}]",
		Prober:         defaultStubProber(),
	}

	res, err := engine.Ingest(t.Context(), srcDir, ingest.Options{
		ExternallyManaged: true,
		FileAssignments: []ingest.FileAssignment{
			{SourcePath: part1, Kind: "part-1"},
			{SourcePath: part2, Kind: "part-2"},
		},
	})
	require.NoError(t, err)
	require.Len(t, res.Items, 2, "multipart should produce two ItemResults")
	for i, item := range res.Items {
		require.NoError(t, item.Err, "part %d had error", i+1)
		assert.Equal(t, ingest.ActionMoved, item.Action, "part %d action", i+1)
		assert.True(t, item.ExternallyManaged,
			"part %d must carry the externally-managed flag", i+1)
	}

	// Both source files must still exist verbatim.
	for _, p := range []string{part1, part2} {
		_, err = os.Stat(p)
		require.NoError(t, err, "%s must still exist after ingest", p)
	}

	// Sentinel is present in the source folder.
	sentinel := filepath.Join(srcDir, ingest.ExternallyManagedSentinel)
	_, err = os.Stat(sentinel)
	require.NoError(t, err)

	// Two version rows, both pointing at the source paths verbatim,
	// with the right part_index values.
	versions, err := storage.ListVersions(t.Context(), db, 90100222)
	require.NoError(t, err)
	require.Len(t, versions, 2)
	byPart := make(map[int]storage.RecordingVersion)
	for _, v := range versions {
		byPart[v.PartIndex] = v
	}
	require.Contains(t, byPart, 1)
	require.Contains(t, byPart, 2)
	assert.Equal(t, part1, byPart[1].FilePath,
		"part 1 version row points at the source path verbatim")
	assert.Equal(t, part2, byPart[2].FilePath,
		"part 2 version row points at the source path verbatim")

	// Library root stays empty — no canonical folder.
	if entries, statErr := os.ReadDir(libRoot); statErr == nil {
		assert.Empty(t, entries,
			"library root must remain empty for externally-managed multipart")
	}

	// Recording row's externally_managed flag is true.
	loaded, err := storage.LoadRecording(t.Context(), db, 90100222)
	require.NoError(t, err)
	assert.True(t, loaded.ExternallyManaged)
}

// TestSetRecordingExternallyManagedRoundTrip verifies the storage-
// level setter flips the flag and LoadRecording reads it back. Pins
// the contract the new GraphQL toggle mutation depends on.
func TestSetRecordingExternallyManagedRoundTrip(t *testing.T) {
	t.Parallel()

	dbPath := seededDBPath(t)
	sqlDB, db, err := storage.OpenEnt(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	// Default (post-sync) is false.
	loaded, err := storage.LoadRecording(t.Context(), db, 90100222)
	require.NoError(t, err)
	assert.False(t, loaded.ExternallyManaged)

	// Flip on, read back true.
	require.NoError(t,
		storage.SetRecordingExternallyManaged(t.Context(), db, 90100222, true))
	loaded, err = storage.LoadRecording(t.Context(), db, 90100222)
	require.NoError(t, err)
	assert.True(t, loaded.ExternallyManaged)

	// Flip off, read back false.
	require.NoError(t,
		storage.SetRecordingExternallyManaged(t.Context(), db, 90100222, false))
	loaded, err = storage.LoadRecording(t.Context(), db, 90100222)
	require.NoError(t, err)
	assert.False(t, loaded.ExternallyManaged)
}
