package cmd

import (
	"fmt"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/nicolerenee/promptbook/internal/storage"
)

// queueDiscoveredLayout truncates discovered_at to "YYYY-MM-DD HH:MM"
// for the queue listing so the column stays narrow without losing the
// useful bits — we don't need seconds or timezone for a human review
// table.
const queueDiscoveredLayout = "2006-01-02 15:04"

// queueColumnPadding is tabwriter's "padding" parameter — the spaces
// inserted between columns after computing the per-column max width.
// Two spaces is enough breathing room without producing visually noisy
// gaps for short rows.
const queueColumnPadding = 2

//nolint:gochecknoglobals // cobra requires package-level command variable
var libraryQueueCmd = &cobra.Command{
	Use:   "queue",
	Short: "List files waiting for manual import",
	Long: `Prints rows from the manual_import_queue — files the watcher saw but
couldn't auto-resolve to an Encora recording. Suggested ID is shown when
the scanner could resolve a candidate; confidence is "high" if the
recording is in the local cache, "low" if the id parsed but isn't known.
A "-" in either column means the scanner had no suggestion at all.`,
	RunE: runLibraryQueue,
}

//nolint:gochecknoinits // cobra requires init for command registration
func init() {
	libraryCmd.AddCommand(libraryQueueCmd)
}

func runLibraryQueue(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()

	_, db, err := storage.OpenEnt(ctx, appConfig.Storage.DatabasePath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	entries, err := storage.ListQueue(ctx, db)
	if err != nil {
		return fmt.Errorf("list queue: %w", err)
	}

	out := cmd.OutOrStdout()
	if len(entries) == 0 {
		_, _ = fmt.Fprintln(out, "(no entries)")
		return nil
	}

	tw := tabwriter.NewWriter(out, 0, 0, queueColumnPadding, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tDISCOVERED\tCONFIDENCE\tPATH\tSUGGESTED")
	for _, e := range entries {
		confidence := e.SuggestedConfidence
		if confidence == "" {
			confidence = "-"
		}
		suggested := "-"
		if e.SuggestedRecordingID != nil {
			suggested = strconv.FormatInt(*e.SuggestedRecordingID, 10)
		}
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n",
			e.ID,
			e.DiscoveredAt.Format(queueDiscoveredLayout),
			confidence,
			e.FilePath,
			suggested,
		)
	}
	if err = tw.Flush(); err != nil {
		return fmt.Errorf("flush queue table: %w", err)
	}
	return nil
}
