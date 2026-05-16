package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pressly/goose/v3"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	_ "modernc.org/sqlite" // sqlite driver

	"github.com/nicolerenee/promptbook/internal/dbm"
)

//nolint:gochecknoglobals // cobra requires package-level command variables
var (
	migrateDBPath string

	migrateCmd = &cobra.Command{
		Use:   "migrate",
		Short: "Database schema migrations (goose-flavored)",
		Long: `Manage the SQLite schema for promptbook's local cache.

Migrations live under internal/dbm/migrations and are produced by
running ` + "`go run -mod=mod db/create_migration.go <name>`" + ` against
the ent schemas in internal/ent/schema.`,
	}

	migrateUpCmd = &cobra.Command{
		Use:   "up",
		Short: "Apply all pending migrations",
		RunE:  runMigrateUp,
	}

	migrateDownCmd = &cobra.Command{
		Use:   "down",
		Short: "Roll back the most recent migration",
		RunE:  runMigrateDown,
	}

	migrateStatusCmd = &cobra.Command{
		Use:   "status",
		Short: "Show migration status",
		RunE:  runMigrateStatus,
	}

	migrateRedoCmd = &cobra.Command{
		Use:   "redo",
		Short: "Roll back the most recent migration and re-apply it",
		RunE:  runMigrateRedo,
	}

	migrateCreateCmd = &cobra.Command{
		Use:   "create <name>",
		Short: "Create an empty timestamped migration file",
		Args:  cobra.ExactArgs(1),
		RunE:  runMigrateCreate,
	}
)

//nolint:gochecknoinits // cobra requires init for command registration
func init() {
	migrateCmd.PersistentFlags().StringVar(
		&migrateDBPath,
		"database",
		"",
		"path to the SQLite database (defaults to config storage.dbpath)",
	)

	migrateCmd.AddCommand(migrateUpCmd)
	migrateCmd.AddCommand(migrateDownCmd)
	migrateCmd.AddCommand(migrateStatusCmd)
	migrateCmd.AddCommand(migrateRedoCmd)
	migrateCmd.AddCommand(migrateCreateCmd)

	rootCmd.AddCommand(migrateCmd)
}

// withGoose opens the SQLite db at the resolved path and configures
// goose to use the embedded migrations. Returns the *sql.DB and a
// closer the caller must defer.
func withGoose(ctx context.Context) (*sql.DB, func(), error) {
	path, err := resolveDBPath()
	if err != nil {
		return nil, nil, err
	}
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err = db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("enable foreign keys: %w", err)
	}
	goose.SetBaseFS(dbm.Migrations)
	if err = goose.SetDialect("sqlite3"); err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("set goose dialect: %w", err)
	}
	closer := func() { _ = db.Close() }
	return db, closer, nil
}

func resolveDBPath() (string, error) {
	if migrateDBPath != "" {
		return migrateDBPath, nil
	}
	if appConfig.Storage.DatabasePath != "" {
		return appConfig.Storage.DatabasePath, nil
	}
	return "", errors.New("no database path: pass --database or set storage.databasePath in config")
}

func runMigrateUp(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	db, closer, err := withGoose(ctx)
	if err != nil {
		return err
	}
	defer closer()

	if upErr := goose.UpContext(ctx, db, "migrations"); upErr != nil {
		return fmt.Errorf("goose up: %w", upErr)
	}
	log.Info().Msg("migrations applied")
	return nil
}

func runMigrateDown(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	db, closer, err := withGoose(ctx)
	if err != nil {
		return err
	}
	defer closer()

	if downErr := goose.DownContext(ctx, db, "migrations"); downErr != nil {
		return fmt.Errorf("goose down: %w", downErr)
	}
	log.Info().Msg("rolled back one migration")
	return nil
}

func runMigrateStatus(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	db, closer, err := withGoose(ctx)
	if err != nil {
		return err
	}
	defer closer()

	if statusErr := goose.StatusContext(ctx, db, "migrations"); statusErr != nil {
		return fmt.Errorf("goose status: %w", statusErr)
	}
	return nil
}

func runMigrateRedo(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	db, closer, err := withGoose(ctx)
	if err != nil {
		return err
	}
	defer closer()

	if redoErr := goose.RedoContext(ctx, db, "migrations"); redoErr != nil {
		return fmt.Errorf("goose redo: %w", redoErr)
	}
	log.Info().Msg("redo complete")
	return nil
}

// runMigrateCreate writes an empty timestamped goose migration into
// internal/dbm/migrations. For most schema changes you should run the
// ent diff (`go run -mod=mod db/create_migration.go <name>`); use
// `migrate create` for one-off data migrations that the diff can't
// produce.
//
//nolint:forbidigo // CLI output uses fmt.
func runMigrateCreate(_ *cobra.Command, args []string) error {
	name := args[0]
	stamp := time.Now().UTC().Format("20060102150405")
	filename := fmt.Sprintf("%s_%s.sql", stamp, name)
	target := filepath.Join("internal", "dbm", "migrations", filename)

	if _, err := os.Stat(target); err == nil {
		return fmt.Errorf("migration already exists: %s", target)
	}

	body := "-- +goose Up\n-- +goose StatementBegin\n\n-- +goose StatementEnd\n\n" +
		"-- +goose Down\n-- +goose StatementBegin\n\n-- +goose StatementEnd\n"
	if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
		return fmt.Errorf("write migration file: %w", err)
	}
	fmt.Println(target)
	return nil
}
