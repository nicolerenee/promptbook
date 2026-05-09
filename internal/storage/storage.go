// Package storage provides the local SQLite cache and migrations.
//
// Migrations are embedded in the binary and applied via goose on Open.
package storage

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"sync"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // sqlite driver
)

// gooseMu serializes calls into goose's package-level globals (SetBaseFS,
// SetDialect). Without this, parallel test cases racing through Open
// trigger the race detector.
//
//nolint:gochecknoglobals // mutex protecting goose's own globals
var gooseMu sync.Mutex

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Connection-pool sizing. WAL mode allows concurrent readers, so a
// modest pool keeps the HTTP server responsive while a scheduled job
// fans out writes. Writes still serialize at the SQLite layer; the
// busy_timeout PRAGMA absorbs brief contention without surfacing
// SQLITE_BUSY to callers.
const (
	maxOpenConns = 8
	maxIdleConns = 4
)

// Open opens the SQLite database at path and runs pending migrations.
//
// PRAGMA setup per-connection (the sqlite driver invokes the
// connection-init each time the pool spins up a new conn):
//
//   - journal_mode = WAL — concurrent readers + one writer without the
//     blanket-serialization rollback journal forces. The schema-aware
//     `_pragma=` query string runs the PRAGMA on every connection
//     created from the pool, not just the first.
//   - busy_timeout = 5000ms — when a writer is mid-transaction and a
//     second writer arrives, the second waits up to 5s for the first
//     to commit before returning SQLITE_BUSY. Avoids spurious failures
//     under brief contention.
//   - foreign_keys = ON — sqlite defaults to off; we depend on FK
//     cascades for image_choices cleanup.
//
// Pool sizing: 8 open connections + 4 idle is comfortable for our
// scale (single-user app, dozens of concurrent reads at peak). With
// WAL the readers run in parallel; writes serialize at the SQLite
// layer regardless of pool size.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxIdleConns)

	if _, err = db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}

	gooseMu.Lock()
	defer gooseMu.Unlock()

	goose.SetBaseFS(migrationsFS)
	if err = goose.SetDialect("sqlite3"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set goose dialect: %w", err)
	}
	if err = goose.UpContext(ctx, db, "migrations"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}
	return db, nil
}
