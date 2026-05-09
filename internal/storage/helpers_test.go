package storage_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/storage"
)

// openTestDB creates a fresh on-disk SQLite database under t.TempDir and
// runs every embedded migration. Returns the test context and the open
// handle; cleanup is registered. Multiple agents authoring tests in parallel
// converged on three different signatures for this — the canonical one
// returns both ctx and db so callers can choose how much of each to use.
func openTestDB(t *testing.T) (context.Context, *sql.DB) {
	t.Helper()
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "promptbook.db")
	db, err := storage.Open(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return ctx, db
}

// seedShow inserts a row into the shows table directly, bypassing the
// sync package. Useful for narrow-scope tests of child-table helpers.
func seedShow(ctx context.Context, t *testing.T, db *sql.DB, id int64, name string) {
	t.Helper()
	_, err := db.ExecContext(ctx,
		`INSERT INTO shows (show_id, name) VALUES (?, ?)`, id, name)
	require.NoError(t, err)
}

// seedRecording inserts a minimal recording row referencing the given
// show. The recording's tour, date, and raw_json are placeholders.
func seedRecording(ctx context.Context, t *testing.T, db *sql.DB, id, showID int64) {
	t.Helper()
	_, err := db.ExecContext(ctx, `
		INSERT INTO recordings (
			recording_id, show_id, tour, date_full, raw_json
		) VALUES (?, ?, '', '', '{}')
	`, id, showID)
	require.NoError(t, err)
}

// recordingPtr returns a pointer to v for compactly setting optional ID
// fields like HistoryEvent.RecordingID.
//
//nolint:modernize // newexpr: explicit helper reads better at table-driven call sites.
func recordingPtr(v int64) *int64 { return &v }
