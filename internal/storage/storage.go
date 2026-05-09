// Package storage provides the local SQLite cache and migrations.
//
// Migrations are embedded in the binary and applied via goose on Open.
package storage

import (
	"database/sql"
	"embed"
	"fmt"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // sqlite driver
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Open opens the SQLite database at path and runs pending migrations.
func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite serializes writes; one writer is fine.

	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set goose dialect: %w", err)
	}
	if err := goose.Up(db, "migrations"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}
	return db, nil
}
