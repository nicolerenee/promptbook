// Package dbm holds the embedded goose-flavored SQL migrations for
// promptbook's local SQLite database. The migrations are produced by
// `go run -mod=mod db/create_migration.go <name>` (see
// db/create_migration.go) which diffs the ent schema against the
// previous schema state and writes a goose-compatible file into
// internal/dbm/migrations/.
//
// The split between db/create_migration.go (codegen) and
// internal/dbm/migrations/ (embedded artifacts) lets the cmd/ and
// internal/storage/ packages embed the migrations without dragging
// the codegen-time atlas dependencies into the final binary.
package dbm

import "embed"

// Migrations is the embedded migrations filesystem. Both
// internal/storage (Open) and cmd/migrate consume it.
//
//go:embed migrations/*.sql
var Migrations embed.FS
