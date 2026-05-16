// Package storage provides the local SQLite cache and migrations.
//
// Migrations are embedded in internal/dbm and applied via goose on Open.
// The data-access helpers in this package wrap *ent.Client; raw SQL via
// *sql.DB.QueryContext / ExecContext does not appear outside this file.
// OpenEnt is the canonical constructor — Open is retained only for the
// migrate CLI subcommand that needs goose's *sql.DB-typed surface.
package storage

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // sqlite driver

	"github.com/nicolerenee/promptbook/internal/dbm"
	"github.com/nicolerenee/promptbook/internal/ent"
)

// gooseMu serializes calls into goose's package-level globals (SetBaseFS,
// SetDialect). Without this, parallel test cases racing through Open
// trigger the race detector.
//
//nolint:gochecknoglobals // mutex protecting goose's own globals
var gooseMu sync.Mutex

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
// Open is the low-level constructor used by the `promptbook migrate`
// subcommand, which depends on goose's *sql.DB-typed surface. Server
// and CLI workflows should use OpenEnt instead — every other caller
// in the codebase consumes data through the *ent.Client.
//
// PRAGMA setup per-connection (the sqlite driver invokes the
// connection-init each time the pool spins up a new conn):
//
//   - journal_mode = WAL — concurrent readers + one writer without the
//     blanket-serialization rollback journal forces.
//   - busy_timeout = 5000ms — when a writer is mid-transaction and a
//     second writer arrives, the second waits up to 5s for the first
//     to commit before returning SQLITE_BUSY.
//   - foreign_keys = ON — sqlite defaults to off; we depend on FK
//     cascades for image_choices cleanup.
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

	goose.SetBaseFS(dbm.Migrations)
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

// OpenEnt opens the database at path (running migrations) and returns
// both the underlying *sql.DB and a new *ent.Client backed by the same
// connection pool. Callers hold onto both: the *sql.DB so Close
// tears down the pool exactly once, the *ent.Client for every read
// and write. Sharing the pool avoids double-pooling and double-
// lifecycle management — closing *sql.DB also tears down the ent
// client.
func OpenEnt(ctx context.Context, path string) (*sql.DB, *ent.Client, error) {
	db, err := Open(ctx, path)
	if err != nil {
		return nil, nil, err
	}
	drv := entsql.OpenDB(dialect.SQLite, db)
	return db, ent.NewClient(ent.Driver(drv)), nil
}
