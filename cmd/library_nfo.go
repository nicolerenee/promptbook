package cmd

import "github.com/spf13/cobra"

//nolint:gochecknoglobals // cobra CLI flags require package-level variables
var libraryNFODryRun bool

//nolint:gochecknoglobals // cobra requires package-level command variable
var libraryNFOCmd = &cobra.Command{
	Use:   "nfo PATH",
	Short: "Regenerate Jellyfin movie.nfo files for canonically-named recordings",
	Long: `Walks a tree of canonically-named recording folders, extracts the
encora id from each folder, looks up the recording in the local cache, and
writes ` + "`movie.nfo`" + ` alongside the video. Use this after editing notes
in the local DB or whenever the NFO writer changes shape.`,
	Args: cobra.ExactArgs(1),
	RunE: runLibraryNFO,
}

//nolint:gochecknoinits // cobra requires init for command registration
func init() {
	libraryNFOCmd.Flags().BoolVar(
		&libraryNFODryRun,
		"dry-run",
		false,
		"show what would change without writing NFO files",
	)
	libraryCmd.AddCommand(libraryNFOCmd)
}

func runLibraryNFO(_ *cobra.Command, _ []string) error {
	return errNotImplemented
}
