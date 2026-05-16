package storage_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/storage"
)

// TestOpenEntRoundTrip is the Phase 1 smoke test for the ent client:
// open a fresh DB, insert a Show through ent, read it back through ent,
// and confirm the underlying *sql.DB sees the same row. The point is
// to prove the two clients share a connection pool and a schema.
func TestOpenEntRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	dbPath := filepath.Join(t.TempDir(), "ent.db")
	db, client, err := storage.OpenEnt(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	const (
		showID   = 42
		showName = "Halcyon Crossing"
	)

	created, err := client.Show.Create().
		SetID(showID).
		SetName(showName).
		Save(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(showID), created.ID)
	assert.Equal(t, showName, created.Name)

	got, err := client.Show.Get(ctx, showID)
	require.NoError(t, err)
	assert.Equal(t, showName, got.Name)

	var (
		gotID   int64
		gotName string
	)
	err = db.QueryRowContext(ctx,
		`SELECT show_id, name FROM shows WHERE show_id = ?`, showID,
	).Scan(&gotID, &gotName)
	require.NoError(t, err)
	assert.Equal(t, int64(showID), gotID)
	assert.Equal(t, showName, gotName)
}

// TestOpenEntSharedConnectionPool ensures the ent client uses the same
// connection pool as the *sql.DB — closing the *sql.DB invalidates
// subsequent ent operations rather than leaving them on a parallel
// pool.
func TestOpenEntSharedConnectionPool(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	dbPath := filepath.Join(t.TempDir(), "ent.db")
	db, client, err := storage.OpenEnt(ctx, dbPath)
	require.NoError(t, err)

	require.NoError(t, db.Close())

	_, err = client.Show.Query().Count(ctx)
	require.Error(t, err, "ent query after closing the underlying *sql.DB must surface an error")
}
