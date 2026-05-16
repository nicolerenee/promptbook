package ingest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/ent/show"
	"github.com/nicolerenee/promptbook/internal/ingest"
	"github.com/nicolerenee/promptbook/internal/scanner"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// TestDVDIntegration_9to5Closing_FullPipeline exercises the full
// scanner-then-ingest DVD pipeline against the user's canonical
// example folder shape: "9 to 5 The Musical 2009.09.06 (Closing
// Night) (juniper47's video)" with VIDEO_TS scaffolding + five
// content VOBs + an info .txt the ripper left behind.
//
// The test:
//
//  1. Builds a fixture-seeded DB (the same Marigold / Greenwich Beacon / Halcyon Crossing
//     collection the rest of the ingest tests reuse).
//  2. Writes a flat-layout DVD source folder under a watched dir.
//  3. Runs the scanner once so the queue picks up exactly one row
//     pointing at VTS_01_1.VOB with classification.discFormat=="dvd"
//     and the 6 scaffolding paths on discScaffolding.
//  4. Translates the classification into ingest.FileAssignments +
//     ingest.Options.DiscScaffolding (mirrors what the GraphQL
//     importQueueEntry resolver does for a real import) and runs
//     the engine.
//  5. Asserts the destination layout matches the spec: five content
//     VOBs + six scaffolding files inside VIDEO_TS/, the ripper
//     notes at other/, no .IFO / .BUP at the parent root, five
//     recording_versions rows with PartIndex 1..5, movie.nfo at
//     the parent folder.
func TestDVDIntegration_9to5Closing_FullPipeline(t *testing.T) {
	t.Parallel()

	dbPath := seededDBPath(t)
	sqlDB, db, err := storage.OpenEnt(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	// Seed a recording row for the 9 to 5 DVD. The fixture-backed
	// collection doesn't include it, so we insert a minimal show +
	// recording row by hand. The recording ID and the show name
	// drive the recording-folder template render.
	const recordingID = int64(372)
	seedDVDRecording(t.Context(), t, db, recordingID)

	tmp := t.TempDir()
	watchDir := filepath.Join(tmp, "watch")
	require.NoError(t, os.MkdirAll(watchDir, 0o755))

	rip := filepath.Join(watchDir,
		"9 to 5 The Musical 2009.09.06 (Closing Night) (juniper47's video)")
	require.NoError(t, os.MkdirAll(rip, 0o755))

	// .encora-id sidecar pointing at the DVD recording row.
	require.NoError(t, os.WriteFile(filepath.Join(rip, ".encora-id"),
		fmt.Appendf(nil, "%d\n", recordingID), 0o644))

	// Flat-layout scaffolding at the rip root. Source paths aren't
	// passed to the ingest engine directly — the scanner picks
	// them up + stamps them into classification.discScaffolding,
	// and the assertion below reads that stamped list.
	scaffNames := []string{
		"VIDEO_TS.IFO", "VIDEO_TS.BUP", "VIDEO_TS.VOB",
		"VTS_01_0.IFO", "VTS_01_0.BUP", "VTS_01_0.VOB",
	}
	for _, name := range scaffNames {
		p := filepath.Join(rip, name)
		require.NoError(t, os.WriteFile(p, []byte("scaffold"), 0o644))
	}

	contentPaths := make([]string, 0, 5)
	for m := 1; m <= 5; m++ {
		p := filepath.Join(rip, fmt.Sprintf("VTS_01_%d.VOB", m))
		require.NoError(t, os.WriteFile(p, []byte("vob content"), 0o644))
		contentPaths = append(contentPaths, p)
	}

	// Non-DVD extra: the ripper's notes.
	rippersNotes := filepath.Join(rip, "info .txt")
	require.NoError(t, os.WriteFile(rippersNotes, []byte("notes"), 0o644))

	// Run the scanner pass. We expect exactly one queue row pointing
	// at VTS_01_1.VOB with the DVD classification stamped onto it.
	engine := &scanner.Engine{
		DB:        db,
		WatchDirs: []string{watchDir},
		Logger:    zerolog.Nop(),
	}
	scanRes, err := engine.Scan(t.Context())
	require.NoError(t, err)
	require.Empty(t, scanRes.Errors)
	require.Equal(t, 1, scanRes.Enqueued,
		"flat DVD layout should produce exactly one queue row")

	queue, err := storage.ListQueue(t.Context(), db)
	require.NoError(t, err)
	require.Len(t, queue, 1)
	entry := queue[0]
	assert.Equal(t, contentPaths[0], entry.FilePath,
		"queue row FilePath = first content VOB")

	var cls struct {
		Parts []struct {
			Path          string `json:"path"`
			SuggestedKind string `json:"suggestedKind"`
			PartIndex     int    `json:"partIndex"`
		} `json:"parts"`
		Extras []struct {
			Path          string `json:"path"`
			SuggestedKind string `json:"suggestedKind"`
		} `json:"extras"`
		DiscFormat      string   `json:"discFormat"`
		DiscScaffolding []string `json:"discScaffolding"`
	}
	require.NoError(t, json.Unmarshal([]byte(entry.ClassificationJSON), &cls))
	assert.Equal(t, "dvd", cls.DiscFormat)
	require.Len(t, cls.Parts, 5)
	for i, p := range cls.Parts {
		assert.Equal(t, contentPaths[i], p.Path,
			"part %d source path", i+1)
		assert.Equal(t, i+1, p.PartIndex)
	}
	require.Len(t, cls.DiscScaffolding, 6,
		"three IFOs + three BUPs / menu VOBs are scaffolding")
	require.Len(t, cls.Extras, 1)
	assert.Equal(t, rippersNotes, cls.Extras[0].Path)

	// Translate the classification into ingest options the same way
	// the GraphQL importQueueEntry resolver does for a real import.
	assignments := make([]ingest.FileAssignment, 0, len(cls.Parts)+len(cls.Extras))
	for _, p := range cls.Parts {
		assignments = append(assignments, ingest.FileAssignment{
			SourcePath: p.Path,
			Kind:       p.SuggestedKind,
		})
	}
	for _, ex := range cls.Extras {
		assignments = append(assignments, ingest.FileAssignment{
			SourcePath: ex.Path,
			Kind:       ex.SuggestedKind,
		})
	}

	libRoot := filepath.Join(tmp, "library")
	srv := newFixtureServer(t)
	t.Cleanup(srv.Close)
	encoraClient, err := encora.New(encora.Options{BaseURL: srv.URL, APIKey: "test"})
	require.NoError(t, err)

	ingestEngine := &ingest.Engine{
		DB:             db,
		SQLDB:          sqlDB,
		Client:         encoraClient,
		LibraryRoot:    libRoot,
		FolderTemplate: "{Show} ({Date}) [encora-{EncoraID}]",
		FileTemplate:   "{Show}",
		Prober:         defaultStubProber(),
	}

	res, err := ingestEngine.Ingest(t.Context(), entry.FilePath, ingest.Options{
		FlagEncoraID:    int(recordingID),
		FileAssignments: assignments,
		DiscFormat:      ingest.DiscFormatDVD,
		DiscScaffolding: cls.DiscScaffolding,
		SourceFolder:    rip,
	})
	require.NoError(t, err)
	require.Len(t, res.Items, 5, "five parts produce five ItemResults")
	for i, item := range res.Items {
		require.NoError(t, item.Err, "part %d had error: %v", i+1, item.Err)
		assert.Equal(t, ingest.ActionMoved, item.Action)
	}

	canonicalFolder := res.Items[0].Plan.AbsoluteFolder()
	videoTSDest := filepath.Join(canonicalFolder, "VIDEO_TS")

	// Content VOBs landed verbatim inside VIDEO_TS/.
	for m := 1; m <= 5; m++ {
		assertFileExists(t,
			filepath.Join(videoTSDest, fmt.Sprintf("VTS_01_%d.VOB", m)))
	}
	// Scaffolding followed alongside.
	for _, name := range scaffNames {
		assertFileExists(t, filepath.Join(videoTSDest, name))
	}
	// Ripper notes landed at other/, NOT inside VIDEO_TS/.
	assertFileExists(t, filepath.Join(canonicalFolder, "other", "info .txt"))
	_, notesInVideoTS := os.Stat(filepath.Join(videoTSDest, "info .txt"))
	assert.True(t, os.IsNotExist(notesInVideoTS),
		"info .txt must NOT leak into VIDEO_TS/")
	// No IFO/BUP/menu-VOB at the parent root — all six scaffolding
	// files must land inside VIDEO_TS/.
	for _, name := range scaffNames {
		_, statErr := os.Stat(filepath.Join(canonicalFolder, name))
		assert.True(t, os.IsNotExist(statErr),
			"scaffolding %s must NOT land at parent root", name)
	}

	// Five recording_versions rows with PartIndex 1..5, each
	// pointing at the canonical VIDEO_TS/ destination.
	versions, err := storage.ListVersions(t.Context(), db, recordingID)
	require.NoError(t, err)
	require.Len(t, versions, 5)
	byPart := map[int]storage.RecordingVersion{}
	for _, v := range versions {
		byPart[v.PartIndex] = v
	}
	for m := 1; m <= 5; m++ {
		require.Contains(t, byPart, m, "PartIndex=%d missing", m)
		expectedPath := filepath.Join(videoTSDest,
			fmt.Sprintf("VTS_01_%d.VOB", m))
		assert.Equal(t, expectedPath, byPart[m].FilePath)
		assert.Equal(t, "vob", byPart[m].Container,
			"VOB container token persists on the version row")
	}

	// movie.nfo at the parent folder root, not inside VIDEO_TS/.
	assertFileExists(t, filepath.Join(canonicalFolder, "movie.nfo"))
	_, nfoInVideoTS := os.Stat(filepath.Join(videoTSDest, "movie.nfo"))
	assert.True(t, os.IsNotExist(nfoInVideoTS),
		"movie.nfo must live at the parent root, not VIDEO_TS/")

	// The source rip folder should be empty after the move + the
	// post-apply cleanup — no leftover scaffolding or stray files.
	// (The .encora-id sidecar was at the rip root; after Apply moved
	// the first VOB the per-part cleanup leaves the sidecar in place
	// because the folder still holds other files. After every move
	// completes, the parent's cleanup may run.)
	remaining, _ := os.ReadDir(rip)
	for _, e := range remaining {
		assert.NotEqual(t, ".vob",
			strings.ToLower(filepath.Ext(e.Name())),
			"no content VOBs left at source after import")
	}
}

// seedDVDRecording inserts a minimal recording + show row for the
// integration test's DVD. Keeps the test self-contained without
// having to add a fixture to internal/encora/testdata. The
// recordingID lines up with the .encora-id sidecar value.
//
// raw_json carries an Encora-shaped recording document populated
// with the bare minimum the rename engine needs to render the
// folder + file templates: show name + tour + ISO date. Without
// the document the templates render empty and the engine errors
// the part out before any move runs.
func seedDVDRecording(
	ctx context.Context, t *testing.T,
	db *ent.Client, recordingID int64,
) {
	t.Helper()
	const showID = int64(8472)
	exists, err := db.Show.Query().Where(show.IDEQ(showID)).Exist(ctx)
	require.NoError(t, err)
	if !exists {
		require.NoError(t, db.Show.Create().
			SetID(showID).SetName("9 to 5 The Musical").Exec(ctx))
	}
	rec := encora.Recording{
		ID:     recordingID,
		Show:   "9 to 5 The Musical",
		Tour:   "Broadway",
		Master: "juniper47",
		Date: encora.Date{
			FullDate:   "2009-09-06",
			MonthKnown: true,
			DayKnown:   true,
			Time:       "evening",
		},
		Metadata: encora.RecordingMeta{
			ShowID:    showID,
			IsClosing: true,
		},
	}
	raw, err := json.Marshal(rec)
	require.NoError(t, err)
	require.NoError(t, db.Recording.Create().
		SetID(recordingID).
		SetShowID(showID).
		SetRawJSON(string(raw)).
		Exec(ctx))
}
