package cmd

import (
	"github.com/spf13/cobra"
)

//nolint:gochecknoglobals // cobra CLI flags require package-level variables
var nfoDryRun bool

//nolint:gochecknoglobals // cobra requires package-level command variable
var nfoCmd = &cobra.Command{
	Use:   "nfo [path]",
	Short: "Generate Jellyfin-compatible movie.nfo files",
	Long: `Walks a tree of canonically-named recordings, extracts the Encora ID from
each folder name, looks up the recording in the local cache, and writes a
movie.nfo file alongside the video so Jellyfin reads metadata locally.`,
	Args: cobra.ExactArgs(1),
	RunE: runNFO,
}

//nolint:gochecknoinits // cobra requires init for command registration
func init() {
	nfoCmd.Flags().BoolVar(&nfoDryRun, "dry-run", false, "show what would change without writing NFO files")
	rootCmd.AddCommand(nfoCmd)
}

func runNFO(_ *cobra.Command, _ []string) error {
	return errNotImplemented
}
