package storage_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/brianvoe/gofakeit/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/storage"
)

// seedRandomRecording wraps the canonical seedRecording with random IDs
// so versions tests don't have to invent unique IDs at every call site.
// Returns the recording_id.
func seedRandomRecording(ctx context.Context, t *testing.T, db *sql.DB) int64 {
	t.Helper()
	gofakeit.Seed(0)
	showID := int64(gofakeit.Number(1, 1_000_000))
	recordingID := int64(gofakeit.Number(1_000_001, 2_000_000))
	seedShow(ctx, t, db, showID, gofakeit.MovieName())
	seedRecording(ctx, t, db, recordingID, showID)
	return recordingID
}

func TestUpsertAndListVersions(t *testing.T) {
	t.Parallel()

	ctx, db := openTestDB(t)
	recordingID := seedRandomRecording(ctx, t, db)

	small := storage.RecordingVersion{
		RecordingID:   recordingID,
		FilePath:      "/store/marigold/1080p.mkv",
		FileSizeBytes: 5 * 1024 * 1024 * 1024,
		Container:     "mkv",
		Quality:       "1080p",
		VideoCodec:    "h264",
		AudioCodec:    "aac",
		FormatLabel:   "MKV 1080p h264",
	}
	large := storage.RecordingVersion{
		RecordingID:   recordingID,
		FilePath:      "/store/marigold/2160p.mkv",
		FileSizeBytes: 40 * 1024 * 1024 * 1024,
		Container:     "mkv",
		Quality:       "2160p",
		VideoCodec:    "hevc",
		AudioCodec:    "flac",
		FormatLabel:   "MKV 2160p hevc",
	}

	require.NoError(t, storage.UpsertVersion(ctx, db, small))
	require.NoError(t, storage.UpsertVersion(ctx, db, large))

	got, err := storage.ListVersions(ctx, db, recordingID)
	require.NoError(t, err)
	require.Len(t, got, 2)

	assert.Equal(t, large.FilePath, got[0].FilePath, "largest file first")
	assert.Equal(t, small.FilePath, got[1].FilePath, "smaller file second")
	assert.Equal(t, large.FileSizeBytes, got[0].FileSizeBytes)
	assert.Equal(t, large.Quality, got[0].Quality)
	assert.NotZero(t, got[0].ID)
	assert.NotZero(t, got[0].AddedAt, "added_at populated by default")
	assert.NotZero(t, got[0].LastSeenAt, "last_seen_at populated by upsert")
}

func TestUpsertVersionIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx, db := openTestDB(t)
	recordingID := seedRandomRecording(ctx, t, db)

	first := storage.RecordingVersion{
		RecordingID:   recordingID,
		FilePath:      "/store/marigold/master.mkv",
		FileSizeBytes: 1 * 1024 * 1024 * 1024,
		Container:     "mkv",
		Quality:       "720p",
		FormatLabel:   "MKV 720p",
		Notes:         "initial scan",
	}
	require.NoError(t, storage.UpsertVersion(ctx, db, first))

	// Capture the original added_at so we can confirm it survives the
	// conflict path.
	versions, err := storage.ListVersions(ctx, db, recordingID)
	require.NoError(t, err)
	require.Len(t, versions, 1)
	originalAddedAt := versions[0].AddedAt
	originalID := versions[0].ID

	second := first
	second.FileSizeBytes = 12 * 1024 * 1024 * 1024
	second.Quality = "1080p"
	second.FormatLabel = "MKV 1080p"
	second.Notes = "rescan"
	require.NoError(t, storage.UpsertVersion(ctx, db, second))

	versions, err = storage.ListVersions(ctx, db, recordingID)
	require.NoError(t, err)
	require.Len(t, versions, 1, "row count must stay at 1 after upsert")

	got := versions[0]
	assert.Equal(t, originalID, got.ID, "primary key preserved across upsert")
	assert.Equal(t, second.FileSizeBytes, got.FileSizeBytes)
	assert.Equal(t, second.Quality, got.Quality)
	assert.Equal(t, second.FormatLabel, got.FormatLabel)
	assert.Equal(t, second.Notes, got.Notes)
	assert.Equal(t, originalAddedAt, got.AddedAt, "added_at must not change on upsert")
}

func TestCascadeDeleteOnRecording(t *testing.T) {
	t.Parallel()

	ctx, db := openTestDB(t)
	recordingID := seedRandomRecording(ctx, t, db)

	require.NoError(t, storage.UpsertVersion(ctx, db, storage.RecordingVersion{
		RecordingID:   recordingID,
		FilePath:      "/store/marigold/v1.mkv",
		FileSizeBytes: 1024,
		FormatLabel:   "MKV 720p",
	}))

	versions, err := storage.ListVersions(ctx, db, recordingID)
	require.NoError(t, err)
	require.Len(t, versions, 1)

	_, err = db.ExecContext(ctx, `DELETE FROM recordings WHERE recording_id = ?`, recordingID)
	require.NoError(t, err)

	var count int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM recording_versions WHERE recording_id = ?`,
		recordingID,
	).Scan(&count))
	assert.Equal(t, 0, count, "deleting recording must cascade-delete its versions")
}

func TestDeleteVersion(t *testing.T) {
	t.Parallel()

	ctx, db := openTestDB(t)
	recordingID := seedRandomRecording(ctx, t, db)

	require.NoError(t, storage.UpsertVersion(ctx, db, storage.RecordingVersion{
		RecordingID: recordingID,
		FilePath:    "/store/marigold/keep.mkv",
		FormatLabel: "MKV 1080p",
	}))
	require.NoError(t, storage.UpsertVersion(ctx, db, storage.RecordingVersion{
		RecordingID: recordingID,
		FilePath:    "/store/marigold/drop.mkv",
		FormatLabel: "MKV 720p",
	}))

	versions, err := storage.ListVersions(ctx, db, recordingID)
	require.NoError(t, err)
	require.Len(t, versions, 2)

	var dropID int64
	for _, v := range versions {
		if v.FilePath == "/store/marigold/drop.mkv" {
			dropID = v.ID
		}
	}
	require.NotZero(t, dropID)

	require.NoError(t, storage.DeleteVersion(ctx, db, dropID))

	remaining, err := storage.ListVersions(ctx, db, recordingID)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	assert.Equal(t, "/store/marigold/keep.mkv", remaining[0].FilePath)
}

func TestDeleteVersionsForRecording(t *testing.T) {
	t.Parallel()

	ctx, db := openTestDB(t)
	recordingID := seedRandomRecording(ctx, t, db)

	for _, p := range []string{"/a.mkv", "/b.mkv", "/c.mkv"} {
		require.NoError(t, storage.UpsertVersion(ctx, db, storage.RecordingVersion{
			RecordingID: recordingID,
			FilePath:    p,
			FormatLabel: "MKV",
		}))
	}

	require.NoError(t, storage.DeleteVersionsForRecording(ctx, db, recordingID))

	got, err := storage.ListVersions(ctx, db, recordingID)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestComputeFormatString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		versions []storage.RecordingVersion
		want     string
	}{
		{
			name:     "empty slice",
			versions: nil,
			want:     "",
		},
		{
			name: "single version",
			versions: []storage.RecordingVersion{
				{FormatLabel: "MKV 1080p h264"},
			},
			want: "MKV 1080p h264",
		},
		{
			name: "three versions joined",
			versions: []storage.RecordingVersion{
				{FormatLabel: "MKV 2160p hevc"},
				{FormatLabel: "MKV 1080p h264"},
				{FormatLabel: "MP4 720p h264"},
			},
			want: "MKV 2160p hevc | MKV 1080p h264 | MP4 720p h264",
		},
		{
			name: "empty labels filtered out",
			versions: []storage.RecordingVersion{
				{FormatLabel: "MKV 2160p"},
				{FormatLabel: ""},
				{FormatLabel: "MP4 720p"},
				{FormatLabel: ""},
			},
			want: "MKV 2160p | MP4 720p",
		},
		{
			name: "all empty labels yields empty string",
			versions: []storage.RecordingVersion{
				{FormatLabel: ""},
				{FormatLabel: ""},
			},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := storage.ComputeFormatString(tt.versions)
			assert.Equal(t, tt.want, got)
		})
	}
}
