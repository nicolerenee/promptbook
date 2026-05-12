package builtin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/jobs"
	"github.com/nicolerenee/promptbook/internal/jobs/builtin"
	"github.com/nicolerenee/promptbook/internal/makemkv"
	"github.com/nicolerenee/promptbook/internal/nforefresh"
	"github.com/nicolerenee/promptbook/internal/probe"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// fakeMakeMKVCmd plays a pre-canned makemkvcon stdout transcript
// and, more importantly, drops a fake .mkv file into outDir so the
// post-makemkv steps (probe + rename + move) have something to
// operate on. Mirrors the makemkv package's own test fake but
// adds the side-effect.
type fakeMakeMKVCmd struct {
	stdout io.ReadCloser
	// produce, when non-empty, is a path under outDir the fake will
	// touch on Wait() so the post-call directory listing picks it up
	// as a "newly produced .mkv". The remux job uses a before/after
	// diff of *.mkv files to attribute outputs to titles.
	produce string
}

func (f *fakeMakeMKVCmd) StdoutPipe() (io.ReadCloser, error) { return f.stdout, nil }
func (f *fakeMakeMKVCmd) Start() error                       { return nil }
func (f *fakeMakeMKVCmd) Wait() error {
	if f.produce != "" {
		// makemkvcon would write the real DVD content here; the
		// remux job only cares that a .mkv file with a stable byte
		// signature lands in outDir.
		return os.WriteFile(f.produce, []byte("fake mkv"), 0o644)
	}
	return nil
}

// noopCloser wraps a strings.Reader as the io.ReadCloser the Cmd
// surface expects. We don't read stdout in the remux job (it
// doesn't parse the transcript) but the Runner contract still
// requires returning a working pipe.
type noopCloser struct{ *strings.Reader }

func (noopCloser) Close() error { return nil }

// stubMakeMKVProber returns a fixed MediaInfo for the produced .mkv
// path so the rename engine has stable inputs.
type stubMakeMKVProber struct {
	info probe.MediaInfo
}

func (s *stubMakeMKVProber) Probe(_ context.Context, _ string) (probe.MediaInfo, error) {
	return s.info, nil
}

// remuxFixture bundles everything the test setup hand-rolls: an
// on-disk recording folder with a VIDEO_TS/ child, a fake DB row
// with one version pointing into VIDEO_TS, plus the stubs the job
// reads through.
type remuxFixture struct {
	t              *testing.T
	ctx            context.Context //nolint:containedctx // test fixture; cleanup runs through t.Cleanup.
	recID          int64
	showID         int64
	libraryRoot    string
	recFolder      string
	videoTSPath    string
	tempForMakeMKV string

	// titleProducedFiles is the per-title-index basename the fake
	// makemkvcon will create under outDir. titles are 0-indexed
	// to match makemkvcon's contract.
	titleProducedFiles map[int]string

	db         *ent.Client
	prober     *stubMakeMKVProber
	makemkvCli *makemkv.Client
	cache      *imagecache.Cache
	nfoSvc     *nforefresh.Service
}

// newRemuxFixture seeds a recording + a VIDEO_TS/ folder layout and
// wires a fake makemkvcon Runner that produces .mkv files in the
// caller-specified outDir on each call.
func newRemuxFixture(t *testing.T, titleOutputs map[int]string) *remuxFixture {
	t.Helper()
	ctx := t.Context()

	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "promptbook.db")
	sqlDB, db, err := storage.OpenEnt(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	libraryRoot := filepath.Join(tmp, "library")
	require.NoError(t, os.MkdirAll(libraryRoot, 0o755))

	recID := int64(42424)
	showID := int64(7777)
	recFolder := filepath.Join(libraryRoot, fmt.Sprintf("DVD Show (%d) [encora-%d]", showID, recID))
	videoTSPath := filepath.Join(recFolder, "VIDEO_TS")
	require.NoError(t, os.MkdirAll(videoTSPath, 0o755))
	// Drop a single fake VOB so the directory has content.
	vobPath := filepath.Join(videoTSPath, "VTS_01_1.VOB")
	require.NoError(t, os.WriteFile(vobPath, []byte("fake vob"), 0o644))

	// Seed show + recording rows.
	require.NoError(t, db.Show.Create().
		SetID(showID).
		SetName("DVD Show").
		Exec(ctx))
	seedRec := encora.Recording{
		ID:   recID,
		Show: "DVD Show",
		Tour: "Anniversary Tour",
		Date: encora.Date{FullDate: "2020-06-15", MonthKnown: true, DayKnown: true},
	}
	seedRec.Metadata.ShowID = showID
	rawJSON, err := json.Marshal(seedRec)
	require.NoError(t, err)
	require.NoError(t, db.Recording.Create().
		SetID(recID).
		SetShowID(showID).
		SetRawJSON(string(rawJSON)).
		Exec(ctx))

	// Seed a version row pointing INTO VIDEO_TS so
	// findRecordingDVDFolder can resolve the parent.
	require.NoError(t, storage.UpsertVersion(ctx, db, storage.RecordingVersion{
		RecordingID: recID,
		FilePath:    vobPath,
		FormatLabel: "DVD",
	}))

	// Image cache + NFO refresh.
	cacheRoot := filepath.Join(tmp, "image-cache")
	require.NoError(t, os.MkdirAll(cacheRoot, 0o755))
	cache := imagecache.New(cacheRoot, nil, zerolog.Nop())
	nfoSvc := nforefresh.New(db, nil, cache, "https://promptbook.example.com", zerolog.Nop())

	// Fake makemkvcon runner: produces a fixed-name .mkv per title
	// index under whatever outDir the job passes (last arg to Mkv).
	cli := &makemkv.Client{
		Binary: "/bin/true",
		Runner: func(_ context.Context, _ string, args ...string) makemkv.Cmd {
			// args layout: --robot --noscan mkv file:{folder} {title} {outDir}.
			require.GreaterOrEqual(t, len(args), 6)
			titleStr := args[len(args)-2]
			outDir := args[len(args)-1]
			titleIdx := 0
			_, _ = fmt.Sscanf(titleStr, "%d", &titleIdx)
			basename, ok := titleOutputs[titleIdx]
			if !ok {
				basename = fmt.Sprintf("title%02d.mkv", titleIdx)
			}
			return &fakeMakeMKVCmd{
				stdout:  noopCloser{strings.NewReader("")},
				produce: filepath.Join(outDir, basename),
			}
		},
	}

	return &remuxFixture{
		t:                  t,
		ctx:                ctx,
		recID:              recID,
		showID:             showID,
		libraryRoot:        libraryRoot,
		recFolder:          recFolder,
		videoTSPath:        videoTSPath,
		tempForMakeMKV:     tmp,
		titleProducedFiles: titleOutputs,
		db:                 db,
		prober: &stubMakeMKVProber{info: probe.MediaInfo{
			Container:       "MKV",
			VideoCodec:      "mpeg2video",
			Width:           720,
			Height:          480,
			DurationSeconds: 7464,
			AudioStreams: []probe.AudioStreamInfo{
				{Codec: "ac3", Language: "eng", ChannelLayout: "stereo"},
			},
		}},
		makemkvCli: cli,
		cache:      cache,
		nfoSvc:     nfoSvc,
	}
}

// TestRemuxDVDJob_SingleTitle exercises the happy path with one
// selected title. Asserts:
//
//   - VIDEO_TS/ got moved into original/VIDEO_TS/ (idempotent
//     preservation of bit-for-bit DVD originals).
//   - the produced .mkv landed in the recording folder under the
//     canonical filename derived from the rename templates.
//   - the old VOB-pointing recording_versions row is gone; a new
//     row points at the .mkv with the probed MediaInfo persisted.
//   - CONVERSION_NOTES.txt landed in original/ and carries the
//     verbatim makemkvcon command.
func TestRemuxDVDJob_SingleTitle(t *testing.T) {
	t.Parallel()
	f := newRemuxFixture(t, map[int]string{0: "title00.mkv"})

	fixedNow := time.Date(2026, 5, 10, 12, 30, 0, 0, time.UTC)
	job := &builtin.RemuxDVDJob{
		DB:             f.db,
		MakeMKV:        f.makemkvCli,
		Prober:         f.prober,
		NFORefresh:     f.nfoSvc,
		LibraryRoot:    f.libraryRoot,
		FolderTemplate: "{Show} ({DateWithVariant}) [encora-{EncoraID}]",
		FileTemplate:   "{Show} ({DateWithVariant}) [encora-{EncoraID}]{? - part-{Part}}",
		Now:            func() time.Time { return fixedNow },
		Logger:         zerolog.Nop(),
	}

	require.NoError(t, job.Run(f.ctx, jobs.JobArgs{
		"recording_id":  f.recID,
		"title_indexes": []int{0},
	}))

	// VIDEO_TS moved into original/.
	preservedVTS := filepath.Join(f.recFolder, "original", "VIDEO_TS")
	_, err := os.Stat(preservedVTS)
	require.NoError(t, err, "VIDEO_TS originals must be preserved under original/")
	_, err = os.Stat(filepath.Join(preservedVTS, "VTS_01_1.VOB"))
	require.NoError(t, err, "original .VOB content must survive the move")

	// Original location must be gone.
	_, statErr := os.Stat(f.videoTSPath)
	assert.True(t, os.IsNotExist(statErr),
		"VIDEO_TS at the top level must have been moved away")

	// The .mkv landed at the recording folder under the canonical
	// filename. The rename template renders against the seeded
	// encora.Recording (show + date) so the output basename is
	// predictable.
	entries, err := os.ReadDir(f.recFolder)
	require.NoError(t, err)
	var mkvNames []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".mkv") {
			mkvNames = append(mkvNames, e.Name())
		}
	}
	require.Len(t, mkvNames, 1, "exactly one .mkv must land in the recording folder")
	assert.Contains(t, mkvNames[0], "DVD Show",
		"output filename must use the show name from the recording row")
	assert.True(t, strings.HasSuffix(mkvNames[0], ".mkv"),
		"output must keep the .mkv extension")

	// CONVERSION_NOTES landed.
	notesPath := filepath.Join(f.recFolder, "original", "CONVERSION_NOTES.txt")
	notesBody, err := os.ReadFile(notesPath)
	require.NoError(t, err, "CONVERSION_NOTES.txt must be written")
	notes := string(notesBody)
	assert.Contains(t, notes, "bit-for-bit lossless",
		"notes must say what the conversion did")
	assert.Contains(t, notes, "makemkvcon --robot --noscan mkv",
		"notes must carry the exact command shape")
	assert.Contains(t, notes, fmt.Sprintf("Recording ID:  %d", f.recID),
		"notes must carry the recording id for audit trail")
	assert.Contains(t, notes, "2026-05-10T12:30:00Z",
		"notes must carry the fixed-clock timestamp from the injected Now")

	// recording_versions rows: old VOB row gone, fresh .mkv row in.
	versions, err := storage.ListVersions(f.ctx, f.db, f.recID)
	require.NoError(t, err)
	require.Len(t, versions, 1,
		"recording_versions should have one row pointing at the new .mkv")
	assert.Equal(t, filepath.Join(f.recFolder, mkvNames[0]), versions[0].FilePath,
		"version row's file_path must point at the canonical .mkv")
	assert.NotContains(t, versions[0].FilePath, "VIDEO_TS",
		"version row must no longer reference the VIDEO_TS layout")
	assert.NotEmpty(t, versions[0].MediaInfoJSON,
		"media_info_json must be populated from the probe")
	assert.Equal(t, "lossless DVD remux", versions[0].FormatLabel,
		"format label must mark this as a lossless remux")
	assert.Equal(t, 0, versions[0].PartIndex,
		"single-title remux must leave part_index at 0")
}

// TestRemuxDVDJob_MultiTitle covers the multi-title case. Asserts:
//
//   - one .mkv per title lands in the recording folder.
//   - recording_versions rows get part_index 1..N so the existing
//     multipart format-string machinery groups them.
//   - CONVERSION_NOTES.txt lists every command + every output file.
func TestRemuxDVDJob_MultiTitle(t *testing.T) {
	t.Parallel()
	f := newRemuxFixture(t, map[int]string{
		0: "title00.mkv",
		3: "title03.mkv",
	})

	job := &builtin.RemuxDVDJob{
		DB:             f.db,
		MakeMKV:        f.makemkvCli,
		Prober:         f.prober,
		NFORefresh:     f.nfoSvc,
		LibraryRoot:    f.libraryRoot,
		FolderTemplate: "{Show} ({DateWithVariant}) [encora-{EncoraID}]",
		FileTemplate:   "{Show} ({DateWithVariant}) [encora-{EncoraID}]{? - part-{Part}}",
		Logger:         zerolog.Nop(),
	}

	require.NoError(t, job.Run(f.ctx, jobs.JobArgs{
		"recording_id":  f.recID,
		"title_indexes": []int{0, 3},
	}))

	versions, err := storage.ListVersions(f.ctx, f.db, f.recID)
	require.NoError(t, err)
	require.Len(t, versions, 2, "multi-title remux must yield two version rows")
	// Sort by PartIndex to make assertion deterministic.
	partSeen := map[int]bool{}
	for _, v := range versions {
		partSeen[v.PartIndex] = true
		assert.True(t, strings.HasSuffix(v.FilePath, ".mkv"))
		assert.Contains(t, v.FilePath, f.recFolder,
			"every produced .mkv must land in the recording folder")
	}
	assert.True(t, partSeen[1], "row with PartIndex=1 must exist")
	assert.True(t, partSeen[2], "row with PartIndex=2 must exist")

	notesPath := filepath.Join(f.recFolder, "original", "CONVERSION_NOTES.txt")
	notesBody, err := os.ReadFile(notesPath)
	require.NoError(t, err)
	notes := string(notesBody)
	// Both title indexes show up in the recorded commands.
	assert.Contains(t, notes, " 0 ", "command for title 0 must be logged")
	assert.Contains(t, notes, " 3 ", "command for title 3 must be logged")
}

// TestRemuxDVDJob_IdempotentVTSMove covers the partial-prior-run
// recovery case: if original/VIDEO_TS already exists (a previous
// attempt got that far before crashing), the job reuses it instead
// of failing or double-moving.
func TestRemuxDVDJob_IdempotentVTSMove(t *testing.T) {
	t.Parallel()
	f := newRemuxFixture(t, map[int]string{0: "title00.mkv"})

	// Pre-stage: move VIDEO_TS into original/ ourselves so the job
	// finds it already preserved. Also drop the top-level VIDEO_TS
	// so the "both exist" guard doesn't trip — that's the
	// "interrupted move" anti-pattern we explicitly bail on.
	originalDir := filepath.Join(f.recFolder, "original")
	require.NoError(t, os.MkdirAll(originalDir, 0o755))
	require.NoError(t, os.Rename(f.videoTSPath, filepath.Join(originalDir, "VIDEO_TS")))

	job := &builtin.RemuxDVDJob{
		DB:             f.db,
		MakeMKV:        f.makemkvCli,
		Prober:         f.prober,
		NFORefresh:     f.nfoSvc,
		LibraryRoot:    f.libraryRoot,
		FolderTemplate: "{Show} ({DateWithVariant}) [encora-{EncoraID}]",
		FileTemplate:   "{Show} ({DateWithVariant}) [encora-{EncoraID}]{? - part-{Part}}",
		Logger:         zerolog.Nop(),
	}

	require.NoError(t, job.Run(f.ctx, jobs.JobArgs{
		"recording_id":  f.recID,
		"title_indexes": []int{0},
	}))

	// The pre-staged originals must still be intact.
	_, err := os.Stat(filepath.Join(originalDir, "VIDEO_TS", "VTS_01_1.VOB"))
	require.NoError(t, err, "pre-staged originals must survive the job's idempotent move")
}

// TestRemuxDVDJob_BadArgs covers the validation failure modes.
func TestRemuxDVDJob_BadArgs(t *testing.T) {
	t.Parallel()
	job := &builtin.RemuxDVDJob{
		DB:             nil, // never reached — args fail first.
		MakeMKV:        &makemkv.Client{},
		LibraryRoot:    "/lib",
		FolderTemplate: "{Show}",
		FileTemplate:   "{Show}",
		Logger:         zerolog.Nop(),
	}

	tests := []struct {
		name string
		args jobs.JobArgs
		want string
	}{
		{
			name: "missing recording_id",
			args: jobs.JobArgs{"title_indexes": []int{0}},
			want: "recording_id",
		},
		{
			name: "zero recording_id",
			args: jobs.JobArgs{"recording_id": int64(0), "title_indexes": []int{0}},
			want: "recording_id",
		},
		{
			name: "missing title_indexes",
			args: jobs.JobArgs{"recording_id": int64(1)},
			want: "title_indexes",
		},
		{
			name: "empty title_indexes",
			args: jobs.JobArgs{"recording_id": int64(1), "title_indexes": []int{}},
			want: "title_indexes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := job.Run(context.Background(), tt.args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

// TestRemuxDVDJob_NoMakeMKVClient asserts the typed error when the
// job runs without a configured makemkv client — the registration
// path skips registration in that case, but defense-in-depth on the
// Run method keeps the failure mode visible.
func TestRemuxDVDJob_NoMakeMKVClient(t *testing.T) {
	t.Parallel()
	job := &builtin.RemuxDVDJob{
		DB:             nil,
		LibraryRoot:    "/lib",
		FolderTemplate: "{Show}",
		FileTemplate:   "{Show}",
		Logger:         zerolog.Nop(),
	}
	err := job.Run(context.Background(), jobs.JobArgs{
		"recording_id":  int64(1),
		"title_indexes": []int{0},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "makemkv client not configured")
}
