package storage_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// seedCollection inserts a row into the collection table for the given
// recording with the supplied format string. Used by state tests to put
// a recording "in collection" with a specific Encora-side format.
func seedCollection(
	ctx context.Context,
	t *testing.T,
	db *ent.Client,
	recordingID int64,
	format string,
) {
	t.Helper()
	require.NoError(t, db.CollectionEntry.Create().
		SetID(recordingID).
		SetFormat(format).
		Exec(ctx))
}

// seedWants inserts a row into the wants table for the given recording.
func seedWants(ctx context.Context, t *testing.T, db *ent.Client, recordingID int64) {
	t.Helper()
	require.NoError(t, db.WantsEntry.Create().SetID(recordingID).Exec(ctx))
}

// seedVersion inserts a recording_version row with the given format
// label so state tests can simulate "file present".
func seedVersion(
	ctx context.Context,
	t *testing.T,
	db *ent.Client,
	recordingID int64,
	filePath, formatLabel string,
) {
	t.Helper()
	require.NoError(t, storage.UpsertVersion(ctx, db, storage.RecordingVersion{
		RecordingID: recordingID,
		FilePath:    filePath,
		FormatLabel: formatLabel,
	}))
}

func TestComputeStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state storage.RecordingState
		want  storage.Status
	}{
		{
			name: "synced: file + collection + matching format",
			state: storage.RecordingState{
				FileCount:    1,
				InCollection: true,
				EncoraFormat: "MKV 1080p",
				LocalFormat:  "MKV 1080p",
			},
			want: storage.StatusSynced,
		},
		{
			name: "format_mismatch: file + collection + differing format",
			state: storage.RecordingState{
				FileCount:    1,
				InCollection: true,
				EncoraFormat: "MKV 1080p",
				LocalFormat:  "MKV 720p",
			},
			want: storage.StatusFormatMismatch,
		},
		{
			name: "orphan: file + not in collection + not in wants",
			state: storage.RecordingState{
				FileCount: 1,
			},
			want: storage.StatusOrphan,
		},
		{
			name: "synced: file + not in collection + in wants",
			state: storage.RecordingState{
				FileCount: 1,
				InWants:   true,
			},
			want: storage.StatusSynced,
		},
		{
			name: "missing: no file + in collection",
			state: storage.RecordingState{
				InCollection: true,
				EncoraFormat: "MKV 1080p",
			},
			want: storage.StatusMissing,
		},
		{
			name: "wanted: no file + in wants",
			state: storage.RecordingState{
				InWants: true,
			},
			want: storage.StatusWanted,
		},
		{
			name:  "fallback: no file + not in collection + not in wants",
			state: storage.RecordingState{},
			want:  storage.StatusOrphan,
		},
		{
			name: "collection wins over wants when both set + file + match",
			state: storage.RecordingState{
				FileCount:    1,
				InCollection: true,
				InWants:      true,
				EncoraFormat: "MKV 1080p",
				LocalFormat:  "MKV 1080p",
			},
			want: storage.StatusSynced,
		},
		{
			name: "collection wins over wants when both set + file + mismatch",
			state: storage.RecordingState{
				FileCount:    1,
				InCollection: true,
				InWants:      true,
				EncoraFormat: "MKV 1080p",
				LocalFormat:  "MKV 720p",
			},
			want: storage.StatusFormatMismatch,
		},
		{
			name: "collection wins over wants when both set + no file",
			state: storage.RecordingState{
				InCollection: true,
				InWants:      true,
			},
			want: storage.StatusMissing,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := storage.ComputeStatus(tt.state)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestLoadStateSynced(t *testing.T) {
	t.Parallel()

	ctx, db := openTestDB(t)
	const showID, recordingID int64 = 100, 1000
	// Encora format is the legacy-fallback compose output, matching
	// what ComputeFormatString produces for a version with no
	// MediaInfoJSON and no FileSizeBytes (the seedVersion helper's
	// shape). Equality of the two produces Synced; the actual string
	// content isn't load-bearing here.
	const fallback = "MKV - ? / ? - ? - 0 B"
	seedShow(ctx, t, db, showID, "Marigold")
	seedRecording(ctx, t, db, recordingID, showID)
	seedCollection(ctx, t, db, recordingID, fallback)
	seedVersion(ctx, t, db, recordingID, "/store/marigold.mkv", "MKV 1080p")

	got, err := storage.LoadState(ctx, db, recordingID)
	require.NoError(t, err)
	assert.Equal(t, storage.StatusSynced, got.Status)
	assert.True(t, got.InCollection)
	assert.False(t, got.InWants)
	assert.Equal(t, 1, got.FileCount)
	assert.Equal(t, fallback, got.EncoraFormat)
	assert.Equal(t, fallback, got.LocalFormat)
}

func TestLoadStateFormatMismatch(t *testing.T) {
	t.Parallel()

	ctx, db := openTestDB(t)
	const showID, recordingID int64 = 101, 1001
	seedShow(ctx, t, db, showID, "Greenwich Beacon")
	seedRecording(ctx, t, db, recordingID, showID)
	seedCollection(ctx, t, db, recordingID, "MKV 1080p")
	seedVersion(ctx, t, db, recordingID, "/store/greenwich-beacon.mkv", "MKV 720p")

	got, err := storage.LoadState(ctx, db, recordingID)
	require.NoError(t, err)
	assert.Equal(t, storage.StatusFormatMismatch, got.Status)
	assert.True(t, got.InCollection)
	assert.Equal(t, "MKV 1080p", got.EncoraFormat)
	// LocalFormat is whatever the new compose path produces for the
	// seeded version — we only need it to differ from EncoraFormat
	// to exercise the FormatMismatch branch.
	assert.Equal(t, "MKV - ? / ? - ? - 0 B", got.LocalFormat)
	assert.NotEqual(t, got.EncoraFormat, got.LocalFormat)
}

func TestLoadStateMissing(t *testing.T) {
	t.Parallel()

	ctx, db := openTestDB(t)
	const showID, recordingID int64 = 102, 1002
	seedShow(ctx, t, db, showID, "Halcyon Crossing")
	seedRecording(ctx, t, db, recordingID, showID)
	seedCollection(ctx, t, db, recordingID, "MKV 2160p")

	got, err := storage.LoadState(ctx, db, recordingID)
	require.NoError(t, err)
	assert.Equal(t, storage.StatusMissing, got.Status)
	assert.True(t, got.InCollection)
	assert.False(t, got.InWants)
	assert.Equal(t, 0, got.FileCount)
	assert.Equal(t, "MKV 2160p", got.EncoraFormat)
	assert.Empty(t, got.LocalFormat)
}

func TestLoadStateWanted(t *testing.T) {
	t.Parallel()

	ctx, db := openTestDB(t)
	const showID, recordingID int64 = 103, 1003
	seedShow(ctx, t, db, showID, "Cresthaven")
	seedRecording(ctx, t, db, recordingID, showID)
	seedWants(ctx, t, db, recordingID)

	got, err := storage.LoadState(ctx, db, recordingID)
	require.NoError(t, err)
	assert.Equal(t, storage.StatusWanted, got.Status)
	assert.False(t, got.InCollection)
	assert.True(t, got.InWants)
	assert.Equal(t, 0, got.FileCount)
	assert.Empty(t, got.EncoraFormat)
}

func TestLoadStateOrphan(t *testing.T) {
	t.Parallel()

	ctx, db := openTestDB(t)
	const showID, recordingID int64 = 104, 1004
	seedShow(ctx, t, db, showID, "Ember Choir")
	seedRecording(ctx, t, db, recordingID, showID)
	seedVersion(ctx, t, db, recordingID, "/store/cats.mkv", "MKV 1080p")

	got, err := storage.LoadState(ctx, db, recordingID)
	require.NoError(t, err)
	assert.Equal(t, storage.StatusOrphan, got.Status)
	assert.False(t, got.InCollection)
	assert.False(t, got.InWants)
	assert.Equal(t, 1, got.FileCount)
	assert.Empty(t, got.EncoraFormat)
	// Legacy-fallback compose output for a seedVersion with no
	// MediaInfoJSON / size — exercises the orphan path's local-format
	// surfacing without asserting on the format string's content.
	assert.Equal(t, "MKV - ? / ? - ? - 0 B", got.LocalFormat)
}

func TestListStatesFilterByStatus(t *testing.T) {
	t.Parallel()

	ctx, db := openTestDB(t)

	// Synced: collection row + file with matching format. Both sides
	// share the legacy-fallback compose output ("MKV - ? / ? - ? - 0 B")
	// so the equality check in ComputeStatus passes — exercising the
	// status-machine alone, not the format-string content.
	const fallbackMKV = "MKV - ? / ? - ? - 0 B"
	seedShow(ctx, t, db, 200, "Synced Show")
	seedRecording(ctx, t, db, 2000, 200)
	seedCollection(ctx, t, db, 2000, fallbackMKV)
	seedVersion(ctx, t, db, 2000, "/store/synced.mkv", "MKV 1080p")

	// FormatMismatch: collection row + file with different format.
	// Encora carries the new compose output but the legacy-fallback
	// version reports a mismatching string, so ComputeStatus reports
	// FormatMismatch.
	seedShow(ctx, t, db, 201, "Mismatch Show")
	seedRecording(ctx, t, db, 2001, 201)
	seedCollection(ctx, t, db, 2001, "MP4 - x264 / AAC - 720p - 5.00 GB")
	seedVersion(ctx, t, db, 2001, "/store/mismatch.mkv", "MKV 720p")

	// Missing: collection row, no file.
	seedShow(ctx, t, db, 202, "Missing Show")
	seedRecording(ctx, t, db, 2002, 202)
	seedCollection(ctx, t, db, 2002, "MKV 1080p")

	// Wanted: wants row, no file.
	seedShow(ctx, t, db, 203, "Wanted Show")
	seedRecording(ctx, t, db, 2003, 203)
	seedWants(ctx, t, db, 2003)

	// Orphan: file present, neither in collection nor wants.
	seedShow(ctx, t, db, 204, "Orphan Show")
	seedRecording(ctx, t, db, 2004, 204)
	seedVersion(ctx, t, db, 2004, "/store/orphan.mkv", "MKV 1080p")

	t.Run("no filter returns all five", func(t *testing.T) {
		t.Parallel()
		got, err := storage.ListStates(ctx, db, storage.ListStatesOptions{})
		require.NoError(t, err)
		assert.Len(t, got, 5)
	})

	t.Run("filter by Synced and Wanted", func(t *testing.T) {
		t.Parallel()
		got, err := storage.ListStates(ctx, db, storage.ListStatesOptions{
			Status: []storage.Status{storage.StatusSynced, storage.StatusWanted},
		})
		require.NoError(t, err)
		require.Len(t, got, 2)

		gotStatuses := map[storage.Status]int64{}
		for _, st := range got {
			gotStatuses[st.Status] = st.RecordingID
		}
		assert.Equal(t, int64(2000), gotStatuses[storage.StatusSynced])
		assert.Equal(t, int64(2003), gotStatuses[storage.StatusWanted])
	})
}
