package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/jobs"
	"github.com/nicolerenee/promptbook/internal/jobs/builtin"
	"github.com/nicolerenee/promptbook/internal/nforefresh"
	"github.com/nicolerenee/promptbook/internal/probe"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// stubEncoraRecordingClient returns a fixed encora.Recording from a
// recorded shape, recording call counts so tests can assert the upstream
// re-pull happened. err, when non-nil, is returned in place of the
// recording so the job's failure path can be exercised.
type stubEncoraRecordingClient struct {
	rec   encora.Recording
	err   error
	calls atomic.Int32
}

func (s *stubEncoraRecordingClient) Recording(
	_ context.Context, _ int64,
) (encora.Recording, encora.RateLimitInfo, error) {
	s.calls.Add(1)
	if s.err != nil {
		return encora.Recording{}, encora.RateLimitInfo{}, s.err
	}
	return s.rec, encora.RateLimitInfo{}, nil
}

// stubProber returns a fixed MediaInfo and tracks the paths it was
// asked to probe. Tests assert against the call count + path slice
// to prove every recording_versions row got reprobed.
type stubProber struct {
	info  probe.MediaInfo
	err   error
	mu    sync.Mutex
	paths []string
}

func (s *stubProber) Probe(_ context.Context, path string) (probe.MediaInfo, error) {
	s.mu.Lock()
	s.paths = append(s.paths, path)
	s.mu.Unlock()
	if s.err != nil {
		return probe.MediaInfo{}, s.err
	}
	return s.info, nil
}

// fullJobFixture bundles the moving parts a refresh-recording-full
// test typically needs: ctx + ent client, an on-disk library folder
// with a movie.{mkv,nfo}, an imagecache + nforefresh.Service, and the
// stub deps the job calls into.
type fullJobFixture struct {
	ctx        context.Context
	db         *ent.Client
	recID      int64
	showID     int64
	libraryDir string
	recFolder  string
	videoPath  string
	cache      *imagecache.Cache
	nfoSvc     *nforefresh.Service
	prober     *stubProber
	encora     *stubEncoraRecordingClient
}

// newFullJobFixture seeds a recording + a single version row pointing
// at an on-disk video file, plus a cache root and an nforefresh service.
// The Encora stub is wired with a recording shape that adds a Notes
// field different from the seeded one so the upsert is observable.
func newFullJobFixture(t *testing.T) *fullJobFixture {
	t.Helper()
	ctx := t.Context()

	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "promptbook.db")
	sqlDB, db, err := storage.OpenEnt(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	libraryDir := filepath.Join(tmp, "library")
	require.NoError(t, os.MkdirAll(libraryDir, 0o755))
	recID := int64(67890)
	showID := int64(12345)
	recFolder := filepath.Join(libraryDir, fmt.Sprintf("Show-%d [encora-%d]", showID, recID))
	require.NoError(t, os.MkdirAll(recFolder, 0o755))
	videoPath := filepath.Join(recFolder, "movie.mkv")
	require.NoError(t, os.WriteFile(videoPath, []byte("mkv"), 0o644))

	require.NoError(t, db.Show.Create().
		SetID(showID).
		SetName(fmt.Sprintf("Show %d", showID)).
		Exec(ctx))
	seedRec := encora.Recording{
		ID:    recID,
		Show:  fmt.Sprintf("Show %d", showID),
		Tour:  "Initial Tour",
		Date:  encora.Date{FullDate: "2024-01-15", MonthKnown: true, DayKnown: true},
		Notes: "before-refresh",
	}
	seedRec.Metadata.ShowID = showID
	rawJSON, err := json.Marshal(seedRec)
	require.NoError(t, err)
	require.NoError(t, db.Recording.Create().
		SetID(recID).
		SetShowID(showID).
		SetRawJSON(string(rawJSON)).
		Exec(ctx))
	require.NoError(t, storage.UpsertVersion(ctx, db, storage.RecordingVersion{
		RecordingID: recID,
		FilePath:    videoPath,
		FormatLabel: "test",
	}))

	cacheRoot := filepath.Join(tmp, "image-cache")
	require.NoError(t, os.MkdirAll(cacheRoot, 0o755))
	cache := imagecache.New(cacheRoot, nil, zerolog.Nop())
	nfoSvc := nforefresh.New(db, nil, cache, "https://promptbook.example.com", zerolog.Nop())

	// The Encora "fresh" payload differs in Notes + Tour so the upsert
	// is observable via storage.LoadRecording after the job runs.
	freshRec := seedRec
	freshRec.Notes = "after-refresh"
	freshRec.Tour = "Refreshed Tour"

	prober := &stubProber{info: probe.MediaInfo{
		Container:       "MKV",
		VideoCodec:      "h264",
		Width:           1920,
		Height:          1080,
		DurationSeconds: 9000,
	}}
	encStub := &stubEncoraRecordingClient{rec: freshRec}

	return &fullJobFixture{
		ctx:        ctx,
		db:         db,
		recID:      recID,
		showID:     showID,
		libraryDir: libraryDir,
		recFolder:  recFolder,
		videoPath:  videoPath,
		cache:      cache,
		nfoSvc:     nfoSvc,
		prober:     prober,
		encora:     encStub,
	}
}

// TestRefreshRecordingFullJob_FullPath drives every step of the
// aggregate job. Asserts:
//
//   - the encora stub was called (step 1: upstream re-pull),
//   - the recording row's Notes column reflects the fresh payload
//     (step 1 actually committed the upsert through PersistRecording),
//   - the version row's media_info_json carries the stub probe blob
//     (step 2: ffprobe + upsert ran),
//   - movie.nfo lands on disk in the recording folder
//     (step 3: NFORefresh.RewriteForRecording ran).
func TestRefreshRecordingFullJob_FullPath(t *testing.T) {
	t.Parallel()
	f := newFullJobFixture(t)

	job := &builtin.RefreshRecordingFullJob{
		DB:         f.db,
		Encora:     f.encora,
		Prober:     f.prober,
		NFORefresh: f.nfoSvc,
		Logger:     zerolog.Nop(),
	}

	require.NoError(t, job.Run(f.ctx, jobs.JobArgs{"recording_id": f.recID}))

	// Step 1: Encora detail fetched + upserted.
	assert.Equal(t, int32(1), f.encora.calls.Load(),
		"encora detail fetch must have been called once")
	loaded, err := storage.LoadRecording(f.ctx, f.db, f.recID)
	require.NoError(t, err)
	assert.Equal(t, "after-refresh", loaded.Recording.Notes,
		"recording row must carry the upserted notes from the encora re-pull")
	assert.Equal(t, "Refreshed Tour", loaded.Recording.Tour,
		"recording row must carry the upserted tour from the encora re-pull")

	// Step 2: every version row reprobed and persisted.
	require.Len(t, f.prober.paths, 1, "prober must be invoked once per version row")
	assert.Equal(t, f.videoPath, f.prober.paths[0])
	require.Len(t, loaded.Versions, 1)
	mediaInfoJSON := loaded.Versions[0].MediaInfoJSON
	require.NotEmpty(t, mediaInfoJSON, "media_info_json must be set after reprobe")
	var got probe.MediaInfo
	require.NoError(t, json.Unmarshal([]byte(mediaInfoJSON), &got))
	assert.Equal(t, "h264", got.VideoCodec)
	assert.Equal(t, 1080, got.Height)

	// Step 3: NFO landed on disk.
	nfoPath := filepath.Join(f.recFolder, "movie.nfo")
	body, err := os.ReadFile(nfoPath)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(body), "<?xml"),
		"movie.nfo must be a real XML payload after the rewrite")
}

// TestRefreshRecordingFullJob_NoEncoraClient covers the
// self-hosted-without-API-key configuration: with Encora left nil, the
// job must still run probe + nforefresh against the local state. The
// recording row's Notes column stays at the seeded value (no upsert
// happened) but the version row gets a fresh media_info_json and the
// NFO is written.
func TestRefreshRecordingFullJob_NoEncoraClient(t *testing.T) {
	t.Parallel()
	f := newFullJobFixture(t)

	job := &builtin.RefreshRecordingFullJob{
		DB:         f.db,
		Encora:     nil, // self-hosted setups without an Encora API key.
		Prober:     f.prober,
		NFORefresh: f.nfoSvc,
		Logger:     zerolog.Nop(),
	}

	require.NoError(t, job.Run(f.ctx, jobs.JobArgs{"recording_id": f.recID}))

	// Encora step skipped: the seeded notes stick around, no upsert ran.
	loaded, err := storage.LoadRecording(f.ctx, f.db, f.recID)
	require.NoError(t, err)
	assert.Equal(t, "before-refresh", loaded.Recording.Notes,
		"no encora client → no upsert; seeded notes must remain")

	// Probe step still ran.
	require.Len(t, f.prober.paths, 1, "prober must run even without encora client")
	require.Len(t, loaded.Versions, 1)
	assert.NotEmpty(t, loaded.Versions[0].MediaInfoJSON,
		"media_info_json must be populated by the local-only path")

	// NFO step still ran.
	nfoPath := filepath.Join(f.recFolder, "movie.nfo")
	_, err = os.Stat(nfoPath)
	assert.NoError(t, err, "movie.nfo must be written even without an encora client")
}

// TestRefreshRecordingFullJob_BadArgs covers the only failure path
// that aborts the run: a missing or non-positive recording_id arg.
// Every other failure inside the job logs + continues so the run row
// stays "succeeded"; this one short-circuits before any work is
// dispatched and surfaces an honest error.
func TestRefreshRecordingFullJob_BadArgs(t *testing.T) {
	t.Parallel()
	job := &builtin.RefreshRecordingFullJob{Logger: zerolog.Nop()}

	tests := []struct {
		name string
		args jobs.JobArgs
	}{
		{name: "missing arg", args: jobs.JobArgs{}},
		{name: "zero arg", args: jobs.JobArgs{"recording_id": int64(0)}},
		{name: "negative arg", args: jobs.JobArgs{"recording_id": int64(-1)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := job.Run(context.Background(), tt.args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "recording_id")
		})
	}
}

// TestRefreshRecordingFullJob_EncoraFailureContinues covers the
// resilience guarantee: an Encora detail-fetch failure is logged and
// the job continues with the remaining steps. The seeded recording
// row's Notes field is unchanged (no upsert because the fetch failed),
// but probe + NFO still run.
func TestRefreshRecordingFullJob_EncoraFailureContinues(t *testing.T) {
	t.Parallel()
	f := newFullJobFixture(t)
	f.encora.err = errors.New("simulated upstream timeout")

	job := &builtin.RefreshRecordingFullJob{
		DB:         f.db,
		Encora:     f.encora,
		Prober:     f.prober,
		NFORefresh: f.nfoSvc,
		Logger:     zerolog.Nop(),
	}

	require.NoError(t, job.Run(f.ctx, jobs.JobArgs{"recording_id": f.recID}))

	// Encora was attempted.
	assert.Equal(t, int32(1), f.encora.calls.Load())

	// Notes unchanged because the fetch errored before persist.
	loaded, err := storage.LoadRecording(f.ctx, f.db, f.recID)
	require.NoError(t, err)
	assert.Equal(t, "before-refresh", loaded.Recording.Notes)

	// Probe + NFO still ran.
	require.Len(t, f.prober.paths, 1)
	nfoPath := filepath.Join(f.recFolder, "movie.nfo")
	_, err = os.Stat(nfoPath)
	assert.NoError(t, err)
}
