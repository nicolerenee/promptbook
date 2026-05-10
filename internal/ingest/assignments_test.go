package ingest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/ingest"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// TestIngest_DefaultEmptyAssignmentsActLikeBefore guards the
// backward-compatibility contract: when Options.FileAssignments is
// empty, ingest must follow the legacy single-file flow exactly —
// one version row, no extras rows. This keeps the existing test
// suite green and the queue modal's pre-phase-1 callers untouched.
func TestIngest_DefaultEmptyAssignmentsActLikeBefore(t *testing.T) {
	t.Parallel()

	dbPath := seededDBPath(t)
	sqlDB, db, err := storage.OpenEnt(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "Marigold [encora-90100222].mp4")
	require.NoError(t, os.WriteFile(src, []byte("v"), 0o644))

	libRoot := filepath.Join(t.TempDir(), "library")
	engine := &ingest.Engine{
		DB:             db,
		Client:         &stubClient{},
		LibraryRoot:    libRoot,
		FolderTemplate: "{Show} [encora-{EncoraID}]",
		FileTemplate:   "{Show}",
		Prober:         defaultStubProber(),
	}

	res, err := engine.Ingest(t.Context(), src, ingest.Options{})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	require.NoError(t, res.Items[0].Err)
	assert.Equal(t, ingest.ActionMoved, res.Items[0].Action)

	versions, err := storage.ListVersions(t.Context(), db, 90100222)
	require.NoError(t, err)
	require.Len(t, versions, 1)
	assert.Equal(t, 0, versions[0].PartIndex,
		"single-file legacy flow keeps part_index at 0")

	extras, err := storage.ListExtras(t.Context(), db, 90100222)
	require.NoError(t, err)
	assert.Empty(t, extras,
		"empty FileAssignments must not write any recording_extras rows")
}

// TestIngest_Multipart pins the multi-file path: two part
// assignments produce two version rows under one recording with
// consecutive part_index values + filenames containing the part
// suffix from the {Part} token. Verifies the rename engine + storage
// upsert agree on the part numbering.
func TestIngest_Multipart(t *testing.T) {
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
		// {Part} expands to "" when part==0 and to "<n>" when
		// part>=1. The optional segment grammar collapses the
		// trailing " - part-" when part==0; the multi-file path
		// always sets part>=1 so the suffix is present.
		FileTemplate: "{Show}[ - part-{Part}]",
		Prober:       defaultStubProber(),
	}

	res, err := engine.Ingest(t.Context(), srcDir, ingest.Options{
		FileAssignments: []ingest.FileAssignment{
			{SourcePath: part1, Kind: "part-1"},
			{SourcePath: part2, Kind: "part-2"},
		},
	})
	require.NoError(t, err)
	require.Len(t, res.Items, 2, "multipart should produce two ItemResults")
	for i, item := range res.Items {
		require.NoError(t, item.Err, "part %d had error", i+1)
		assert.Equal(t, ingest.ActionMoved, item.Action,
			"part %d action", i+1)
	}

	versions, err := storage.ListVersions(t.Context(), db, 90100222)
	require.NoError(t, err)
	require.Len(t, versions, 2, "two recording_versions rows for the two parts")

	parts := make(map[int]string)
	for _, v := range versions {
		parts[v.PartIndex] = v.FilePath
	}
	require.Contains(t, parts, 1, "must have a part_index=1 row")
	require.Contains(t, parts, 2, "must have a part_index=2 row")
	assert.Contains(t, parts[1], "part-1",
		"part 1 filename must carry the - part-1 suffix from {Part}")
	assert.Contains(t, parts[2], "part-2",
		"part 2 filename must carry the - part-2 suffix from {Part}")
	// Both parts share the canonical recording folder.
	assert.Equal(t, filepath.Dir(parts[1]), filepath.Dir(parts[2]),
		"multipart parts share one canonical folder")
}

// TestIngest_ExtrasIntoSubfolders pins the extras flow: three
// extras (featurette, audio, photo) land in featurettes/, audio/,
// and photos/ subfolders under the canonical recording folder, and
// matching recording_extras rows get written.
func TestIngest_ExtrasIntoSubfolders(t *testing.T) {
	t.Parallel()

	dbPath := seededDBPath(t)
	sqlDB, db, err := storage.OpenEnt(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	srcDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, ".encora-id"),
		[]byte("90100222\n"), 0o644))
	main := filepath.Join(srcDir, "main.mkv")
	require.NoError(t, os.WriteFile(main, []byte("main bytes"), 0o644))
	bows := filepath.Join(srcDir, "bows.mp4")
	require.NoError(t, os.WriteFile(bows, []byte("bows bytes"), 0o644))
	track := filepath.Join(srcDir, "track.mp3")
	require.NoError(t, os.WriteFile(track, []byte("audio bytes"), 0o644))
	photo := filepath.Join(srcDir, "photo.jpg")
	require.NoError(t, os.WriteFile(photo, []byte("photo bytes"), 0o644))

	libRoot := filepath.Join(t.TempDir(), "library")
	engine := &ingest.Engine{
		DB:             db,
		Client:         &stubClient{},
		LibraryRoot:    libRoot,
		FolderTemplate: "{Show} [encora-{EncoraID}]",
		FileTemplate:   "{Show}",
		Prober:         defaultStubProber(),
	}

	res, err := engine.Ingest(t.Context(), srcDir, ingest.Options{
		FileAssignments: []ingest.FileAssignment{
			{SourcePath: main, Kind: "main"},
			{SourcePath: bows, Kind: "extra-featurette", Label: "Bows"},
			{SourcePath: track, Kind: "extra-audio"},
			{SourcePath: photo, Kind: "extra-photo"},
		},
	})
	require.NoError(t, err)
	require.Len(t, res.Items, 1, "main is the only ItemResult; extras hang off it")
	require.NoError(t, res.Items[0].Err)
	require.Len(t, res.Items[0].AppliedExtras, 3,
		"three AppliedExtras returned to the caller")

	canonicalFolder := res.Items[0].Plan.AbsoluteFolder()

	// Each extra landed in its kind-specific subfolder.
	assertFileExists(t, filepath.Join(canonicalFolder, "featurettes", "bows.mp4"))
	assertFileExists(t, filepath.Join(canonicalFolder, "audio", "track.mp3"))
	assertFileExists(t, filepath.Join(canonicalFolder, "photos", "photo.jpg"))

	// recording_extras rows were written with the right kind.
	rows, err := storage.ListExtras(t.Context(), db, 90100222)
	require.NoError(t, err)
	require.Len(t, rows, 3)
	byKind := make(map[string]storage.RecordingExtra)
	for _, r := range rows {
		byKind[r.Kind] = r
	}
	require.Contains(t, byKind, "featurette")
	require.Contains(t, byKind, "audio")
	require.Contains(t, byKind, "photo")
	assert.Equal(t, "Bows", byKind["featurette"].Label,
		"label propagates from FileAssignment to the row")
	assert.True(t, strings.HasSuffix(byKind["featurette"].FilePath,
		filepath.Join("featurettes", "bows.mp4")))
	assert.True(t, strings.HasSuffix(byKind["audio"].FilePath,
		filepath.Join("audio", "track.mp3")))
	assert.True(t, strings.HasSuffix(byKind["photo"].FilePath,
		filepath.Join("photos", "photo.jpg")))
}

// assertFileExists fails the test when path doesn't resolve to a
// regular file. Helper for the multi-file ingest assertions where
// the per-extras destination is the unit under test.
func assertFileExists(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err, "expected %s on disk", path)
	require.False(t, info.IsDir(), "expected %s to be a file", path)
}
