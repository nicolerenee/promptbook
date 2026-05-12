package ingest_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/ingest"
	"github.com/nicolerenee/promptbook/internal/rename"
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

// TestIngest_DVDFlatLayout covers the canonical DVD import: a folder
// with VIDEO_TS.IFO + VTS_01_M.VOB chunks at the root (no nested
// VIDEO_TS/ subfolder), plus an info .txt the ripper left behind.
// After the ingest:
//   - Five content VOBs land in {recordingFolder}/VIDEO_TS/ with
//     their original DVD-spec names intact.
//   - Three IFO + three BUP + the menu VOB scaffolding follow into
//     the same VIDEO_TS/ subfolder verbatim.
//   - Five recording_versions rows exist with PartIndex 1..5, each
//     pointing at the canonical VIDEO_TS/VTS_01_M.VOB destination.
//   - The info .txt extra lands at extras/other (the regular extras
//     pipeline) at the parent folder, NOT inside VIDEO_TS/.
//   - movie.nfo + .encora-id live at the recording folder root.
func TestIngest_DVDFlatLayout(t *testing.T) {
	t.Parallel()

	dbPath := seededDBPath(t)
	sqlDB, db, err := storage.OpenEnt(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	srcDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, ".encora-id"),
		[]byte("90100222\n"), 0o644))
	// DVD scaffolding at the root (flat layout).
	scaffPaths := map[string]string{
		"VIDEO_TS.IFO": filepath.Join(srcDir, "VIDEO_TS.IFO"),
		"VIDEO_TS.BUP": filepath.Join(srcDir, "VIDEO_TS.BUP"),
		"VIDEO_TS.VOB": filepath.Join(srcDir, "VIDEO_TS.VOB"),
		"VTS_01_0.IFO": filepath.Join(srcDir, "VTS_01_0.IFO"),
		"VTS_01_0.BUP": filepath.Join(srcDir, "VTS_01_0.BUP"),
		"VTS_01_0.VOB": filepath.Join(srcDir, "VTS_01_0.VOB"),
	}
	for _, p := range scaffPaths {
		require.NoError(t, os.WriteFile(p, []byte("scaffold"), 0o644))
	}
	// Five content VOBs (M=1..5).
	contentPaths := []string{
		filepath.Join(srcDir, "VTS_01_1.VOB"),
		filepath.Join(srcDir, "VTS_01_2.VOB"),
		filepath.Join(srcDir, "VTS_01_3.VOB"),
		filepath.Join(srcDir, "VTS_01_4.VOB"),
		filepath.Join(srcDir, "VTS_01_5.VOB"),
	}
	for _, p := range contentPaths {
		require.NoError(t, os.WriteFile(p, []byte("vob content"), 0o644))
	}
	// Non-DVD extra: ripper notes.
	rippersNotes := filepath.Join(srcDir, "info .txt")
	require.NoError(t, os.WriteFile(rippersNotes, []byte("ripper notes"), 0o644))

	libRoot := filepath.Join(t.TempDir(), "library")
	engine := &ingest.Engine{
		DB:             db,
		Client:         &stubClient{},
		LibraryRoot:    libRoot,
		FolderTemplate: "{Show} [encora-{EncoraID}]",
		FileTemplate:   "{Show}",
		Prober:         defaultStubProber(),
	}

	assignments := []ingest.FileAssignment{
		{SourcePath: contentPaths[0], Kind: "part-1"},
		{SourcePath: contentPaths[1], Kind: "part-2"},
		{SourcePath: contentPaths[2], Kind: "part-3"},
		{SourcePath: contentPaths[3], Kind: "part-4"},
		{SourcePath: contentPaths[4], Kind: "part-5"},
		{SourcePath: rippersNotes, Kind: "extra-other"},
	}
	scaffolding := make([]string, 0, len(scaffPaths))
	for _, p := range scaffPaths {
		scaffolding = append(scaffolding, p)
	}

	res, err := engine.Ingest(t.Context(), srcDir, ingest.Options{
		FileAssignments: assignments,
		DiscFormat:      ingest.DiscFormatDVD,
		DiscScaffolding: scaffolding,
	})
	require.NoError(t, err)
	require.Len(t, res.Items, 5, "five parts produce five ItemResults")
	for i, item := range res.Items {
		require.NoError(t, item.Err, "part %d had error", i+1)
		assert.Equal(t, ingest.ActionMoved, item.Action,
			"part %d action", i+1)
	}

	canonicalFolder := res.Items[0].Plan.AbsoluteFolder()
	videoTSDir := filepath.Join(canonicalFolder, "VIDEO_TS")

	// Content VOBs land inside VIDEO_TS/ with their original names.
	assertFileExists(t, filepath.Join(videoTSDir, "VTS_01_1.VOB"))
	assertFileExists(t, filepath.Join(videoTSDir, "VTS_01_2.VOB"))
	assertFileExists(t, filepath.Join(videoTSDir, "VTS_01_3.VOB"))
	assertFileExists(t, filepath.Join(videoTSDir, "VTS_01_4.VOB"))
	assertFileExists(t, filepath.Join(videoTSDir, "VTS_01_5.VOB"))

	// Scaffolding follows alongside.
	assertFileExists(t, filepath.Join(videoTSDir, "VIDEO_TS.IFO"))
	assertFileExists(t, filepath.Join(videoTSDir, "VIDEO_TS.BUP"))
	assertFileExists(t, filepath.Join(videoTSDir, "VIDEO_TS.VOB"))
	assertFileExists(t, filepath.Join(videoTSDir, "VTS_01_0.IFO"))
	assertFileExists(t, filepath.Join(videoTSDir, "VTS_01_0.BUP"))
	assertFileExists(t, filepath.Join(videoTSDir, "VTS_01_0.VOB"))

	// The ripper's notes land at extras/other/, NOT inside VIDEO_TS/.
	assertFileExists(t, filepath.Join(canonicalFolder, "other", "info .txt"))
	_, statNotesErr := os.Stat(filepath.Join(videoTSDir, "info .txt"))
	assert.True(t, os.IsNotExist(statNotesErr),
		"info .txt must NOT leak into VIDEO_TS/")

	// No IFO/BUP at the recording folder root — they all live inside
	// VIDEO_TS/.
	for _, name := range []string{
		"VIDEO_TS.IFO", "VIDEO_TS.BUP", "VTS_01_0.IFO", "VTS_01_0.BUP",
	} {
		_, statRootErr := os.Stat(filepath.Join(canonicalFolder, name))
		assert.True(t, os.IsNotExist(statRootErr),
			"scaffolding %s must NOT land at parent folder root", name)
	}

	// Recording versions: five rows, PartIndex 1..5, each pointing
	// at the canonical VIDEO_TS/ destination.
	versions, err := storage.ListVersions(t.Context(), db, 90100222)
	require.NoError(t, err)
	require.Len(t, versions, 5,
		"one recording_versions row per content VOB")
	byPart := map[int]storage.RecordingVersion{}
	for _, v := range versions {
		byPart[v.PartIndex] = v
	}
	for m := 1; m <= 5; m++ {
		require.Contains(t, byPart, m, "PartIndex=%d row missing", m)
		expectedPath := filepath.Join(videoTSDir, fmt.Sprintf("VTS_01_%d.VOB", m))
		assert.Equal(t, expectedPath, byPart[m].FilePath,
			"part %d file_path", m)
	}

	// movie.nfo + .encora-id sidecar at the recording folder root.
	assertFileExists(t, filepath.Join(canonicalFolder, "movie.nfo"))
	assertFileExists(t, filepath.Join(canonicalFolder, rename.SidecarFilename))
}

// TestIngest_DVDNestedLayout covers the alternate source shape where
// the rip preserves the original VIDEO_TS/ subfolder. Destination
// layout is identical to the flat case — the mover flattens both.
func TestIngest_DVDNestedLayout(t *testing.T) {
	t.Parallel()

	dbPath := seededDBPath(t)
	sqlDB, db, err := storage.OpenEnt(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	srcDir := t.TempDir()
	videoTS := filepath.Join(srcDir, "VIDEO_TS")
	require.NoError(t, os.MkdirAll(videoTS, 0o755))
	scaffPaths := []string{
		filepath.Join(videoTS, "VIDEO_TS.IFO"),
		filepath.Join(videoTS, "VIDEO_TS.BUP"),
		filepath.Join(videoTS, "VTS_01_0.IFO"),
	}
	for _, p := range scaffPaths {
		require.NoError(t, os.WriteFile(p, []byte("s"), 0o644))
	}
	content1 := filepath.Join(videoTS, "VTS_01_1.VOB")
	content2 := filepath.Join(videoTS, "VTS_01_2.VOB")
	require.NoError(t, os.WriteFile(content1, []byte("c1"), 0o644))
	require.NoError(t, os.WriteFile(content2, []byte("c2"), 0o644))

	libRoot := filepath.Join(t.TempDir(), "library")
	engine := &ingest.Engine{
		DB:             db,
		Client:         &stubClient{},
		LibraryRoot:    libRoot,
		FolderTemplate: "{Show} [encora-{EncoraID}]",
		FileTemplate:   "{Show}",
		Prober:         defaultStubProber(),
	}

	// FlagEncoraID matches the production queue-import path: the
	// resolver supplies the recording id explicitly via input or the
	// queue row's suggestion. For the nested DVD layout the VOBs
	// live inside VIDEO_TS/ where there's no .encora-id sidecar (the
	// sidecar lives in the rip-folder root), so the explicit flag
	// is the only way through.
	res, err := engine.Ingest(t.Context(), srcDir, ingest.Options{
		FileAssignments: []ingest.FileAssignment{
			{SourcePath: content1, Kind: "part-1"},
			{SourcePath: content2, Kind: "part-2"},
		},
		FlagEncoraID:    90100222,
		DiscFormat:      ingest.DiscFormatDVD,
		DiscScaffolding: scaffPaths,
	})
	require.NoError(t, err)
	require.Len(t, res.Items, 2)
	for _, item := range res.Items {
		require.NoError(t, item.Err)
		assert.Equal(t, ingest.ActionMoved, item.Action)
	}

	canonicalFolder := res.Items[0].Plan.AbsoluteFolder()
	videoTSDest := filepath.Join(canonicalFolder, "VIDEO_TS")
	assertFileExists(t, filepath.Join(videoTSDest, "VTS_01_1.VOB"))
	assertFileExists(t, filepath.Join(videoTSDest, "VTS_01_2.VOB"))
	assertFileExists(t, filepath.Join(videoTSDest, "VIDEO_TS.IFO"))
	assertFileExists(t, filepath.Join(videoTSDest, "VIDEO_TS.BUP"))
	assertFileExists(t, filepath.Join(videoTSDest, "VTS_01_0.IFO"))

	versions, err := storage.ListVersions(t.Context(), db, 90100222)
	require.NoError(t, err)
	require.Len(t, versions, 2)
}

// TestIngest_DVDExternallyManaged covers the catalog-only DVD path:
// no source files move, the version rows point at the original
// source paths, and the .encora-id sidecar + sentinel + movie.nfo
// land in the source folder rather than the canonical library
// folder. The DVD + externally-managed combination composes via the
// per-item path overrides — scaffolding stays in place since
// applyDiscScaffolding short-circuits in externally-managed mode.
func TestIngest_DVDExternallyManaged(t *testing.T) {
	t.Parallel()

	dbPath := seededDBPath(t)
	sqlDB, db, err := storage.OpenEnt(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	srcDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, ".encora-id"),
		[]byte("90100222\n"), 0o644))
	scaff := []string{
		filepath.Join(srcDir, "VIDEO_TS.IFO"),
		filepath.Join(srcDir, "VTS_01_0.IFO"),
	}
	for _, p := range scaff {
		require.NoError(t, os.WriteFile(p, []byte("s"), 0o644))
	}
	content := filepath.Join(srcDir, "VTS_01_1.VOB")
	require.NoError(t, os.WriteFile(content, []byte("c"), 0o644))

	libRoot := filepath.Join(t.TempDir(), "library")
	engine := &ingest.Engine{
		DB:             db,
		SQLDB:          sqlDB,
		Client:         &stubClient{},
		LibraryRoot:    libRoot,
		FolderTemplate: "{Show} [encora-{EncoraID}]",
		FileTemplate:   "{Show}",
		Prober:         defaultStubProber(),
	}

	res, err := engine.Ingest(t.Context(), srcDir, ingest.Options{
		FileAssignments: []ingest.FileAssignment{
			{SourcePath: content, Kind: "main"},
		},
		DiscFormat:        ingest.DiscFormatDVD,
		DiscScaffolding:   scaff,
		ExternallyManaged: true,
	})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	require.NoError(t, res.Items[0].Err)
	assert.Equal(t, ingest.ActionMoved, res.Items[0].Action)

	// Source files must still exist where they started.
	assertFileExists(t, content)
	assertFileExists(t, scaff[0])
	assertFileExists(t, scaff[1])

	// Version row points at the source path verbatim.
	versions, err := storage.ListVersions(t.Context(), db, 90100222)
	require.NoError(t, err)
	require.Len(t, versions, 1)
	assert.Equal(t, content, versions[0].FilePath,
		"externally-managed DVD version row tracks the source path")

	// Sentinel + .encora-id sidecar land in the source folder.
	assertFileExists(t, filepath.Join(srcDir, ingest.ExternallyManagedSentinel))
}
