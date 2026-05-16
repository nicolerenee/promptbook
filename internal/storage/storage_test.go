package storage_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/storage"
)

// TestOpenAppliesMigrations is a smoke test: opening a fresh database file
// runs all migrations to completion. Schema-shape assertions live in
// migration-specific tests once the real schema lands in phase 2.
func TestOpenAppliesMigrations(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "promptbook.db")

	db, err := storage.Open(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	// Foreign keys should be on per-connection.
	var fk int
	require.NoError(t, db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk))
	assert.Equal(t, 1, fk, "foreign_keys pragma must be on")
}
