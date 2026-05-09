//go:build ignore

// create_migration.go diffs the current ent schema against an
// at-replay-time clean SQLite database and writes a goose-flavored
// migration into internal/dbm/migrations/. Adapted from
// infratographer/location-api's pattern, but for sqlite + modernc
// instead of postgres + libpq.
//
// Usage:
//
//	ATLAS_DB_URI='sqlite://file:/tmp/atlas-replay.db?_fk=1' \
//	    go run -mod=mod db/create_migration.go <name>
package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"log"
	"os"
	"strings"

	_ "ariga.io/atlas/sql/sqlite" // register sqlite migrate driver.
	"ariga.io/atlas/sql/sqltool"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql/schema"
	sqlite "modernc.org/sqlite"

	"github.com/nicolerenee/promptbook/internal/ent/migrate"
)

// Atlas's sqlite client driver does sql.Open("sqlite3", ...). modernc
// registers itself as "sqlite" and uses different DSN parameter
// syntax (_pragma=...) than mattn/go-sqlite3 (_fk=1). Bridge the two
// here so we don't need CGO just for migration generation, and
// translate Atlas's _fk=1 into modernc's _pragma=foreign_keys(ON).
//
//nolint:gochecknoinits // single-file generator script.
func init() {
	sql.Register("sqlite3", proxyDriver{inner: &sqlite.Driver{}})
}

type proxyDriver struct {
	inner driver.Driver
}

func (p proxyDriver) Open(name string) (driver.Conn, error) {
	return p.inner.Open(translateDSN(name))
}

func translateDSN(name string) string {
	if !strings.Contains(name, "_fk=1") {
		return name
	}
	out := strings.ReplaceAll(name, "_fk=1", "_pragma=foreign_keys(ON)")
	return out
}

const migrationsDir = "internal/dbm/migrations"

func main() {
	if len(os.Args) != 2 {
		log.Fatalln("usage: go run -mod=mod db/create_migration.go <name>")
	}
	ctx := context.Background()

	dir, err := sqltool.NewGooseDir(migrationsDir)
	if err != nil {
		log.Fatalf("open goose dir %s: %v", migrationsDir, err)
	}

	dbURI, ok := os.LookupEnv("ATLAS_DB_URI")
	if !ok {
		log.Fatalln("ATLAS_DB_URI not set; e.g. sqlite://file:/tmp/atlas-replay.db?_fk=1")
	}

	opts := []schema.MigrateOption{
		schema.WithDir(dir),
		schema.WithMigrationMode(schema.ModeReplay),
		schema.WithDialect(dialect.SQLite),
		schema.WithFormatter(sqltool.GooseFormatter),
	}
	if err := migrate.NamedDiff(ctx, dbURI, os.Args[1], opts...); err != nil {
		log.Fatalf("named diff: %v", err)
	}
}
