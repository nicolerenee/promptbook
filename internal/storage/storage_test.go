package storage

import (
	"path/filepath"
	"testing"
)

func TestOpenAppliesMigrations(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "promptbook.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var note string
	if err := db.QueryRow("SELECT note FROM schema_marker WHERE id = 1").Scan(&note); err != nil {
		t.Fatalf("query schema_marker: %v", err)
	}
	if note != "promptbook:init" {
		t.Errorf("schema_marker.note = %q, want %q", note, "promptbook:init")
	}
}
