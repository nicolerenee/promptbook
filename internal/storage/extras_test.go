package storage_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/storage"
)

// TestUpsertExtra round-trips a recording_extras row and then re-
// upserts the same (recording_id, file_path) with new kind / label
// / size values. The conflict path must update the row in place
// (rather than insert a duplicate) so re-ingesting the same file is
// idempotent.
func TestUpsertExtra(t *testing.T) {
	t.Parallel()

	ctx, db := openTestDB(t)
	recordingID := seedRandomRecording(ctx, t, db)

	id, err := storage.UpsertExtra(ctx, db, storage.RecordingExtra{
		RecordingID:   recordingID,
		FilePath:      "/library/greenwich-beacon/featurettes/bows.mp4",
		Kind:          "featurette",
		Label:         "Bows — original cast",
		FileSizeBytes: 100 * 1024 * 1024,
	})
	require.NoError(t, err)
	assert.NotZero(t, id, "upsert must return a non-zero id")

	rows, err := storage.ListExtras(ctx, db, recordingID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "featurette", rows[0].Kind)
	assert.Equal(t, "Bows — original cast", rows[0].Label)

	// Re-upsert with new metadata for the same path. The row count
	// must stay at 1 and the kind / label / size must reflect the
	// new values.
	updatedID, err := storage.UpsertExtra(ctx, db, storage.RecordingExtra{
		RecordingID:   recordingID,
		FilePath:      "/library/greenwich-beacon/featurettes/bows.mp4",
		Kind:          "scene",
		Label:         "renamed",
		FileSizeBytes: 200 * 1024 * 1024,
	})
	require.NoError(t, err)
	assert.Equal(t, id, updatedID,
		"upsert on (recording_id, file_path) conflict must reuse the row id")

	rows, err = storage.ListExtras(ctx, db, recordingID)
	require.NoError(t, err)
	require.Len(t, rows, 1, "row count must stay at 1 after upsert")
	assert.Equal(t, "scene", rows[0].Kind)
	assert.Equal(t, "renamed", rows[0].Label)
	assert.Equal(t, int64(200*1024*1024), rows[0].FileSizeBytes)
}

// TestListExtras_OrderedByPath pins the file_path lexicographic
// ordering. The recordingExtras GraphQL resolver relies on this
// order so the SPA renders extras in a deterministic sequence.
func TestListExtras_OrderedByPath(t *testing.T) {
	t.Parallel()

	ctx, db := openTestDB(t)
	recordingID := seedRandomRecording(ctx, t, db)

	// Insert in non-alphabetical order; ListExtras must still
	// return them sorted by file_path.
	for _, p := range []string{
		"/library/x/photos/03 — curtain call.jpg",
		"/library/x/audio/01 — overture.mp3",
		"/library/x/featurettes/02 — interview.mp4",
	} {
		_, err := storage.UpsertExtra(ctx, db, storage.RecordingExtra{
			RecordingID: recordingID,
			FilePath:    p,
			Kind:        "featurette",
		})
		require.NoError(t, err)
	}

	rows, err := storage.ListExtras(ctx, db, recordingID)
	require.NoError(t, err)
	require.Len(t, rows, 3)
	assert.Equal(t, "/library/x/audio/01 — overture.mp3", rows[0].FilePath)
	assert.Equal(t, "/library/x/featurettes/02 — interview.mp4", rows[1].FilePath)
	assert.Equal(t, "/library/x/photos/03 — curtain call.jpg", rows[2].FilePath)
}
