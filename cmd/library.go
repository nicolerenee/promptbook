package cmd

import "github.com/spf13/cobra"

// libraryCmd is the parent of all on-disk-library commands.
//
//nolint:gochecknoglobals // cobra requires package-level command variable
var libraryCmd = &cobra.Command{
	Use:   "library",
	Short: "Operate on the on-disk recording library",
	Long: `Commands that move, rename, and annotate video files in the canonical
library directory. ` + "`library ingest`" + ` is the typical entry point — it
pulls a tree of incoming videos through resolve → rename → subtitle/NFO
generation in one pass.`,
}

//nolint:gochecknoinits // cobra requires init for command registration
func init() {
	rootCmd.AddCommand(libraryCmd)
}
