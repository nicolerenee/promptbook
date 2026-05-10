package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/nicolerenee/promptbook/internal/ingest"
	"github.com/nicolerenee/promptbook/internal/probe"
	"github.com/nicolerenee/promptbook/internal/storage"
)

//nolint:gochecknoglobals // cobra requires package-level command variable
var libraryScanCmd = &cobra.Command{
	Use:   "scan PATH",
	Short: "Print an ingest dry-run report for PATH without writing anything",
	Long: `Equivalent to ` + "`library ingest --dry-run`" + ` — walks PATH, resolves
encora ids, and reports the proposed rename, subtitle, and NFO plan for
each entry. Useful for sanity-checking before a real ingest.`,
	Args: cobra.ExactArgs(1),
	RunE: runLibraryScan,
}

//nolint:gochecknoinits // cobra requires init for command registration
func init() {
	libraryCmd.AddCommand(libraryScanCmd)
}

func runLibraryScan(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	src := args[0]

	sqlDB, db, err := storage.OpenEnt(ctx, appConfig.Storage.DatabasePath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()

	client, err := buildEncoraClientForIngest()
	if err != nil {
		return err
	}

	engine := &ingest.Engine{
		DB:             db,
		SQLDB:          sqlDB,
		Client:         client,
		LibraryRoot:    appConfig.Library.Root,
		FolderTemplate: appConfig.Library.FolderTemplate,
		FileTemplate:   appConfig.Library.FileTemplate,
		Prober:         probe.FFProbe{Path: appConfig.Library.FFProbePath},
	}

	res, err := engine.Ingest(ctx, src, ingest.Options{DryRun: true})
	if err != nil {
		return fmt.Errorf("scan: %w", err)
	}

	out := cmd.OutOrStdout()
	for _, item := range res.Items {
		_, _ = fmt.Fprintf(out, "%s\n  encora id: %d (from %s)\n",
			item.Source, item.EncoraID, item.ResolvedFrom)
		if item.Plan != nil {
			_, _ = fmt.Fprintf(out, "  → %s\n", item.Plan.AbsoluteFile())
		}
		_, _ = fmt.Fprintf(out, "  action: %s", item.Action)
		if item.SkippedReason != "" {
			_, _ = fmt.Fprintf(out, " (%s)", item.SkippedReason)
		}
		_, _ = fmt.Fprintln(out)
	}
	return nil
}
