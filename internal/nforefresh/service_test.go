package nforefresh_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/nforefresh"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// openTestDB opens a fresh on-disk SQLite + ent client at t.TempDir
// and runs every embedded migration. The db file path lives under
// the test's tempdir so every test gets a clean slate.
func openTestDB(t *testing.T) (context.Context, *ent.Client) {
	t.Helper()
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "promptbook.db")
	sqlDB, client, err := storage.OpenEnt(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return ctx, client
}

// seedShowOnce inserts a shows row for showID iff one isn't already
// present. The shows table FK-constrains recordings.show_id, so any
// recording-seeding helper must guarantee its show exists. Tests
// that re-seed the same show across multiple recordings call this
// idempotently.
func seedShowOnce(ctx context.Context, t *testing.T, db *ent.Client, showID int64) {
	t.Helper()
	if exists, _ := db.Show.Query().Where().IDs(ctx); contains(exists, showID) {
		return
	}
	err := db.Show.Create().
		SetID(showID).
		SetName(fmt.Sprintf("Show %d", showID)).
		Exec(ctx)
	if err != nil {
		// Ignore PK conflicts so the helper is safe to call multiple
		// times for the same show across different recordings in a
		// single test.
		if !strings.Contains(err.Error(), "UNIQUE") {
			require.NoError(t, err)
		}
	}
}

func contains(haystack []int64, needle int64) bool {
	return slices.Contains(haystack, needle)
}

// seedRecording inserts a Recording row carrying the given showID
// and a real raw_json blob describing the upstream shape. The shape
// drives nfo.WriteRecordingFile output, so callers care about both
// the cast and the metadata.show_id values.
func seedRecording(
	ctx context.Context, t *testing.T, db *ent.Client,
	recID, showID int64, cast []encora.CastEntry,
) {
	t.Helper()
	seedShowOnce(ctx, t, db, showID)
	rec := encora.Recording{
		ID:    recID,
		Show:  fmt.Sprintf("Show %d", showID),
		Tour:  "Test Tour",
		Date:  encora.Date{FullDate: "2024-01-15", MonthKnown: true, DayKnown: true},
		Cast:  cast,
		Notes: "seed",
	}
	rec.Metadata.ShowID = showID
	raw, err := json.Marshal(rec)
	require.NoError(t, err)
	require.NoError(t, db.Recording.Create().
		SetID(recID).
		SetShowID(showID).
		SetRawJSON(string(raw)).
		Exec(ctx))
}

// seedVersion writes a recording_versions row for the given recording
// pointing at filePath. The file itself doesn't have to exist —
// nforefresh only reads the row to derive the destination folder.
func seedVersion(
	ctx context.Context, t *testing.T, db *ent.Client,
	recID int64, filePath string,
) {
	t.Helper()
	require.NoError(t, storage.UpsertVersion(ctx, db, storage.RecordingVersion{
		RecordingID: recID,
		FilePath:    filePath,
		FormatLabel: "test",
	}))
}

// seedCast inserts a cast_entries row + its performer for the given
// recording. The Service's RewriteForPerformer path queries the cast
// table to find affected recordings. The performer row is upserted
// idempotently so multiple recordings can share the same performer.
func seedCast(
	ctx context.Context, t *testing.T, db *ent.Client,
	recID, performerID int64,
) {
	t.Helper()
	if exists, _ := db.Performer.Query().IDs(ctx); !contains(exists, performerID) {
		require.NoError(t, db.Performer.Create().
			SetID(performerID).
			SetName(fmt.Sprintf("Performer %d", performerID)).
			SetSlug(fmt.Sprintf("performer-%d", performerID)).
			SetURL(fmt.Sprintf("https://example.com/performer/%d", performerID)).
			SetLastSeenAt(time.Now()).
			Exec(ctx))
	}
	require.NoError(t, db.CastEntry.Create().
		SetRecordingID(recID).
		SetPerformerID(performerID).
		SetPerformerName(fmt.Sprintf("Performer %d", performerID)).
		SetCharacterID(0).
		SetCharacterName("Lead").
		SetCharacterOrder(1).
		Exec(ctx))
}

// writeImageWithMtime drops a 1-byte placeholder at path and forces
// its mtime so the writer reads a deterministic value. Mirrors the
// helper in nfo/writer_test.go but lives here too so this package
// keeps no test-only cross-package dependency.
func writeImageWithMtime(t *testing.T, path string, mtime int64) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte{0xff}, 0o600))
	ts := time.Unix(mtime, 0)
	require.NoError(t, os.Chtimes(path, ts, ts))
}

// TestRewriteForRecording covers the per-recording rewrite: a
// movie.nfo on disk gets regenerated against the latest image
// mtimes, and the new file content reflects them.
func TestRewriteForRecording(t *testing.T) {
	t.Parallel()
	ctx, db := openTestDB(t)

	// Layout: <libraryRoot>/<recordingFolder>/movie.{mkv,nfo}
	libraryRoot := t.TempDir()
	recFolder := filepath.Join(libraryRoot, "Show-12345 [encora-67890]")
	require.NoError(t, os.MkdirAll(recFolder, 0o755))
	videoPath := filepath.Join(recFolder, "movie.mkv")
	require.NoError(t, os.WriteFile(videoPath, []byte("v"), 0o644))

	recID := int64(67890)
	showID := int64(12345)
	performerID := int64(90004242)
	seedRecording(ctx, t, db, recID, showID, []encora.CastEntry{{
		Performer: encora.Performer{ID: performerID, Name: "Jane Test"},
		Character: encora.Character{Name: "Lead", Order: 1},
	}})
	seedVersion(ctx, t, db, recID, videoPath)

	cacheRoot := t.TempDir()
	cache := imagecache.New(cacheRoot, nil, zerolog.Nop())
	posterMtime := int64(1_700_001_001)
	writeImageWithMtime(t, cache.RecordingPosterPath(recID), posterMtime)

	svc := nforefresh.New(db, nil, cache, "https://promptbook.example.com", zerolog.Nop())
	require.NoError(t, svc.RewriteForRecording(ctx, recID))

	nfoPath := filepath.Join(recFolder, "movie.nfo")
	body, err := os.ReadFile(nfoPath)
	require.NoError(t, err)
	got := string(body)
	assert.Contains(t, got, fmt.Sprintf(
		"poster.jpg?v=%d", posterMtime))
	assert.Contains(t, got, fmt.Sprintf("recordings/%d/", recID))
}

// TestRewriteForRecording_NoVersion is a no-op when the recording
// has no on-disk version row yet — typical for a freshly-created
// catalog row that hasn't been ingested. Must NOT error and must
// NOT create a movie.nfo anywhere.
func TestRewriteForRecording_NoVersion(t *testing.T) {
	t.Parallel()
	ctx, db := openTestDB(t)

	recID := int64(11111)
	seedRecording(ctx, t, db, recID, 12345, nil)

	cache := imagecache.New(t.TempDir(), nil, zerolog.Nop())
	svc := nforefresh.New(db, nil, cache, "https://promptbook.example.com", zerolog.Nop())

	// Should silently succeed — no version row → no folder → no work.
	require.NoError(t, svc.RewriteForRecording(ctx, recID))
}

// TestRewriteForRecording_NotFound is a no-op when the recording
// row itself is missing from the local catalog. ErrRecordingNotFound
// is swallowed at the Service boundary so a stale fan-out call
// doesn't pollute the upload-handler error log.
func TestRewriteForRecording_NotFound(t *testing.T) {
	t.Parallel()
	ctx, db := openTestDB(t)

	cache := imagecache.New(t.TempDir(), nil, zerolog.Nop())
	svc := nforefresh.New(db, nil, cache, "https://promptbook.example.com", zerolog.Nop())

	require.NoError(t, svc.RewriteForRecording(ctx, 99999))
}

// TestRewriteForShow walks every recording for the show id and
// regenerates each NFO. The fan-out targets the show banner mtime
// since that's what every recording's <set><thumb> references.
func TestRewriteForShow(t *testing.T) {
	t.Parallel()
	ctx, db := openTestDB(t)

	libraryRoot := t.TempDir()
	showID := int64(54321)
	recIDs := []int64{1001, 1002, 1003}
	folders := make(map[int64]string, len(recIDs))
	for _, recID := range recIDs {
		folder := filepath.Join(libraryRoot, fmt.Sprintf("rec-%d", recID))
		require.NoError(t, os.MkdirAll(folder, 0o755))
		videoPath := filepath.Join(folder, "movie.mkv")
		require.NoError(t, os.WriteFile(videoPath, []byte("v"), 0o644))
		seedRecording(ctx, t, db, recID, showID, nil)
		seedVersion(ctx, t, db, recID, videoPath)
		folders[recID] = folder
	}

	cacheRoot := t.TempDir()
	cache := imagecache.New(cacheRoot, nil, zerolog.Nop())
	bannerMtime := int64(1_700_002_002)
	writeImageWithMtime(t, cache.ShowBannerPath(showID), bannerMtime)

	svc := nforefresh.New(db, nil, cache, "https://promptbook.example.com", zerolog.Nop())
	require.NoError(t, svc.RewriteForShow(ctx, showID))

	// Every recording's NFO carries the banner URL with the new mtime.
	bannerSuffix := fmt.Sprintf("banner.jpg?v=%d", bannerMtime)
	for _, recID := range recIDs {
		nfoPath := filepath.Join(folders[recID], "movie.nfo")
		body, err := os.ReadFile(nfoPath)
		require.NoError(t, err, "recording %d", recID)
		assert.Contains(t, string(body), bannerSuffix,
			"recording %d should reference banner mtime", recID)
	}
}

// TestRewriteForPerformer fans out to every recording crediting the
// performer. Recordings the performer is NOT in must NOT be
// rewritten (we use mtime stability to detect that).
func TestRewriteForPerformer(t *testing.T) {
	t.Parallel()
	ctx, db := openTestDB(t)

	libraryRoot := t.TempDir()
	showID := int64(77777)
	performerID := int64(8888)

	// recA + recB credit the performer; recC does not.
	recIDs := []int64{2001, 2002, 2003}
	folders := make(map[int64]string, len(recIDs))
	for _, recID := range recIDs {
		folder := filepath.Join(libraryRoot, fmt.Sprintf("rec-%d", recID))
		require.NoError(t, os.MkdirAll(folder, 0o755))
		videoPath := filepath.Join(folder, "movie.mkv")
		require.NoError(t, os.WriteFile(videoPath, []byte("v"), 0o644))
		var cast []encora.CastEntry
		if recID != 2003 {
			cast = []encora.CastEntry{{
				Performer: encora.Performer{ID: performerID, Name: "Jane Test"},
				Character: encora.Character{Name: "Lead", Order: 1},
			}}
		}
		seedRecording(ctx, t, db, recID, showID, cast)
		seedVersion(ctx, t, db, recID, videoPath)
		if recID != 2003 {
			seedCast(ctx, t, db, recID, performerID)
		}
		folders[recID] = folder
	}

	cacheRoot := t.TempDir()
	cache := imagecache.New(cacheRoot, nil, zerolog.Nop())
	headshotMtime := int64(1_700_003_003)
	writeImageWithMtime(t, cache.HeadshotPath(performerID), headshotMtime)

	svc := nforefresh.New(db, nil, cache, "https://promptbook.example.com", zerolog.Nop())
	require.NoError(t, svc.RewriteForPerformer(ctx, performerID))

	// recA + recB carry an NFO referencing the new headshot mtime.
	headshotURLSuffix := fmt.Sprintf("actors/%d.jpg?v=%d", performerID, headshotMtime)
	for _, recID := range []int64{2001, 2002} {
		nfoPath := filepath.Join(folders[recID], "movie.nfo")
		body, err := os.ReadFile(nfoPath)
		require.NoError(t, err, "recording %d", recID)
		assert.Contains(t, string(body), headshotURLSuffix,
			"recording %d should reference headshot mtime", recID)
	}

	// recC has no NFO on disk because the rewrite never touched it
	// (no cast row → not in the fan-out set).
	uninvolved := filepath.Join(folders[2003], "movie.nfo")
	_, err := os.Stat(uninvolved)
	require.True(t, os.IsNotExist(err),
		"recording 2003 should NOT have been rewritten")
}

// TestRewriteForRecording_MtimeAdvancesContent asserts the rewritten
// NFO file content actually changes after a re-write with a new
// image mtime. Belt-and-braces guard against a regression where the
// writer bypasses the cache and emits a stale URL.
func TestRewriteForRecording_MtimeAdvancesContent(t *testing.T) {
	t.Parallel()
	ctx, db := openTestDB(t)

	libraryRoot := t.TempDir()
	recFolder := filepath.Join(libraryRoot, "rec")
	require.NoError(t, os.MkdirAll(recFolder, 0o755))
	videoPath := filepath.Join(recFolder, "movie.mkv")
	require.NoError(t, os.WriteFile(videoPath, []byte("v"), 0o644))

	recID := int64(33333)
	seedRecording(ctx, t, db, recID, 99, nil)
	seedVersion(ctx, t, db, recID, videoPath)

	cacheRoot := t.TempDir()
	cache := imagecache.New(cacheRoot, nil, zerolog.Nop())
	writeImageWithMtime(t, cache.RecordingFanartPath(recID), 1_700_000_100)

	svc := nforefresh.New(db, nil, cache, "https://promptbook.example.com", zerolog.Nop())
	require.NoError(t, svc.RewriteForRecording(ctx, recID))
	first, err := os.ReadFile(filepath.Join(recFolder, "movie.nfo"))
	require.NoError(t, err)

	// Bump fanart mtime, re-run.
	writeImageWithMtime(t, cache.RecordingFanartPath(recID), 1_700_000_200)
	require.NoError(t, svc.RewriteForRecording(ctx, recID))
	second, err := os.ReadFile(filepath.Join(recFolder, "movie.nfo"))
	require.NoError(t, err)

	assert.NotEqual(t, string(first), string(second),
		"NFO content must reflect the new fanart mtime")
	assert.Contains(t, string(first), "?v=1700000100",
		"first rewrite should reference initial mtime")
	assert.Contains(t, string(second), "?v=1700000200",
		"second rewrite should reference bumped mtime")
}
