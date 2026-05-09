package cmd_test

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/cmd"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// TestLibraryQueueCommandEmpty verifies the empty-queue placeholder.
// Not Parallel — exercises global cobra/viper state.
func TestLibraryQueueCommandEmpty(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "promptbook.db")
	db, err := storage.Open(t.Context(), dbPath)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	t.Setenv("PROMPTBOOK_STORAGE_DATABASEPATH", dbPath)

	var buf bytes.Buffer
	require.NoError(t, cmd.RunForTest(t.Context(), []string{"library", "queue"}, &buf))

	assert.Contains(t, buf.String(), "(no entries)")
}

// TestLibraryQueueCommandLists pre-seeds two queue entries and verifies
// both file paths show up in the rendered table. Not Parallel —
// exercises global cobra/viper state.
func TestLibraryQueueCommandLists(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "promptbook.db")
	db, err := storage.Open(t.Context(), dbPath)
	require.NoError(t, err)

	suggested := int64(90100222)
	entries := []storage.QueueEntry{
		{
			FilePath:             "/incoming/Show.mkv",
			FileSizeBytes:        1024,
			SuggestedRecordingID: &suggested,
			SuggestedConfidence:  storage.ConfidenceHigh,
		},
		{
			FilePath:      "/incoming/random.mp4",
			FileSizeBytes: 2048,
		},
	}
	for _, e := range entries {
		_, eqErr := storage.EnqueueFile(t.Context(), db, e)
		require.NoError(t, eqErr)
	}
	require.NoError(t, db.Close())

	t.Setenv("PROMPTBOOK_STORAGE_DATABASEPATH", dbPath)

	var buf bytes.Buffer
	require.NoError(t, cmd.RunForTest(t.Context(), []string{"library", "queue"}, &buf))

	out := buf.String()
	assert.Contains(t, out, "/incoming/Show.mkv")
	assert.Contains(t, out, "/incoming/random.mp4")
	assert.Contains(t, out, "90100222")
	assert.Contains(t, out, "high")
	// Header row should be present.
	assert.Contains(t, out, "ID")
	assert.Contains(t, out, "DISCOVERED")
	assert.Contains(t, out, "CONFIDENCE")
	assert.Contains(t, out, "PATH")
	assert.Contains(t, out, "SUGGESTED")
}
