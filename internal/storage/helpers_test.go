package storage_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// openTestDB creates a fresh on-disk SQLite database under t.TempDir
// and runs every embedded migration. Returns the test context and the
// ent client; cleanup tears down the underlying *sql.DB.
func openTestDB(t *testing.T) (context.Context, *ent.Client) {
	t.Helper()
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "promptbook.db")
	sqlDB, client, err := storage.OpenEnt(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return ctx, client
}

// seedShow inserts a row into the shows table via the ent client.
// Useful for narrow-scope tests of child-table helpers.
func seedShow(ctx context.Context, t *testing.T, client *ent.Client, id int64, name string) {
	t.Helper()
	err := client.Show.Create().SetID(id).SetName(name).Exec(ctx)
	require.NoError(t, err)
}

// seedRecording inserts a minimal recording row referencing the given
// show. The recording's tour, date, and raw_json are placeholders.
func seedRecording(ctx context.Context, t *testing.T, client *ent.Client, id, showID int64) {
	t.Helper()
	err := client.Recording.Create().
		SetID(id).
		SetShowID(showID).
		SetRawJSON("{}").
		Exec(ctx)
	require.NoError(t, err)
}

// recordingPtr returns a pointer to v for compactly setting optional ID
// fields like HistoryEvent.RecordingID.
//
//nolint:modernize // newexpr: explicit helper reads better at table-driven call sites.
func recordingPtr(v int64) *int64 { return &v }
