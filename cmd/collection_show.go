package cmd

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nicolerenee/promptbook/internal/storage"
)

//nolint:gochecknoglobals // cobra requires package-level command variable
var collectionShowCmd = &cobra.Command{
	Use:   "show ID",
	Short: "Print recording detail from the local cache",
	Long: `Reads a single recording from the local SQLite cache and prints its
metadata, cast list, and (if owned) collection state. No network I/O.`,
	Args: cobra.ExactArgs(1),
	RunE: runCollectionShow,
}

//nolint:gochecknoinits // cobra requires init for command registration
func init() {
	collectionCmd.AddCommand(collectionShowCmd)
}

func runCollectionShow(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	id, err := storage.ParseRecordingID(args[0])
	if err != nil {
		return err
	}

	_, db, err := storage.OpenEnt(ctx, appConfig.Storage.DatabasePath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	loaded, err := storage.LoadRecording(ctx, db, id)
	if err != nil {
		if errors.Is(err, storage.ErrRecordingNotFound) {
			return fmt.Errorf("%w — run `promptbook collection sync` first", err)
		}
		return err
	}

	return printRecording(cmd.OutOrStdout(), loaded)
}

// printRecording renders a LoadedRecording to w. Kept side-by-side with
// the cobra wiring so the formatting can evolve without touching tests.
func printRecording(w io.Writer, loaded *storage.LoadedRecording) error {
	r := loaded.Recording
	var b strings.Builder

	fmt.Fprintf(&b, "%s — %s\n", r.Show, r.Tour)
	fmt.Fprintf(&b, "  encora id: %d\n", r.ID)
	fmt.Fprintf(&b, "  date:      %s (month_known=%t day_known=%t)\n",
		r.Date.FullDate, r.Date.MonthKnown, r.Date.DayKnown)
	if r.Date.Time != "" {
		fmt.Fprintf(&b, "  time:      %s\n", r.Date.Time)
	}
	fmt.Fprintf(&b, "  master:    %s\n", emptyDash(r.Master))
	fmt.Fprintf(&b, "  type:      %s / %s\n",
		emptyDash(r.Metadata.RecordingType), emptyDash(r.Metadata.MediaType))
	fmt.Fprintf(&b, "  venue:     %s, %s\n",
		emptyDash(r.Metadata.Venue), emptyDash(r.Metadata.City))

	if loaded.InCollection {
		fmt.Fprintf(&b, "\n  in collection: yes\n")
		fmt.Fprintf(&b, "  format:        %s\n", emptyDash(loaded.Format))
		fmt.Fprintf(&b, "  watched:       %t\n", loaded.UserWatched)
		if loaded.UserNotes != nil {
			fmt.Fprintf(&b, "  user notes:    %s\n", *loaded.UserNotes)
		}
	}
	if loaded.InWants {
		fmt.Fprintf(&b, "\n  in wants: yes\n")
	}

	if len(r.Cast) > 0 {
		fmt.Fprintf(&b, "\n  cast (%d):\n", len(r.Cast))
		for _, c := range r.Cast {
			status := "principal"
			if c.Status != nil {
				status = c.Status.Label
			}
			fmt.Fprintf(&b, "    %-30s %-30s %s\n",
				c.Performer.Name, c.Character.Name, status)
		}
	}

	if r.Notes != "" {
		fmt.Fprintf(&b, "\n  notes:\n    %s\n", strings.ReplaceAll(r.Notes, "\n", "\n    "))
	}

	_, err := io.WriteString(w, b.String())
	return err
}

func emptyDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
