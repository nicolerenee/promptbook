package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/nicolerenee/promptbook/internal/scanner"
	"github.com/nicolerenee/promptbook/internal/storage"
)

//nolint:gochecknoglobals // cobra requires package-level command variable
var libraryWatchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Poll incoming directories and enqueue files for manual import",
	Long: `Starts a polling watcher over ` + "`library.incomingDirs`" + ` on a
` + "`library.watchInterval`" + ` cadence. Each pass walks every configured
directory, enqueues unrecognized video files onto the manual_import_queue
for human review, and drops queue rows whose underlying file disappeared.
Blocks until SIGINT/SIGTERM. Inspect pending entries with ` +
		"`library queue`" + `.`,
	RunE: runLibraryWatch,
}

//nolint:gochecknoinits // cobra requires init for command registration
func init() {
	libraryCmd.AddCommand(libraryWatchCmd)
}

func runLibraryWatch(cmd *cobra.Command, _ []string) error {
	if len(appConfig.Library.IncomingDirs) == 0 {
		return errors.New(
			"library.incomingDirs is empty; configure at least one directory before running `library watch`",
		)
	}

	// Trap SIGINT/SIGTERM so a Ctrl-C unwinds Engine.Run cleanly via
	// ctx.Done() rather than killing the process mid-pass.
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := storage.Open(ctx, appConfig.Storage.DatabasePath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	engine := &scanner.Engine{
		DB:        db,
		WatchDirs: appConfig.Library.IncomingDirs,
		Interval:  appConfig.Library.WatchInterval,
		Logger:    log.Logger,
	}

	log.Info().
		Strs("incoming_dirs", appConfig.Library.IncomingDirs).
		Dur("interval", appConfig.Library.WatchInterval).
		Msg("starting watch loop")

	if err = engine.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("watch loop: %w", err)
	}
	return nil
}
