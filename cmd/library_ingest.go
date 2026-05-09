package cmd

import "github.com/spf13/cobra"

//nolint:gochecknoglobals // cobra CLI flags require package-level variables
var (
	ingestEncoraID        int
	ingestDryRun          bool
	ingestInteractive     bool
	ingestAddToCollection bool
)

//nolint:gochecknoglobals // cobra requires package-level command variable
var libraryIngestCmd = &cobra.Command{
	Use:   "ingest SRC",
	Short: "Resolve, rename, subtitle, and NFO each video under SRC into the library",
	Long: `Walks SRC for video files (or single-video folders) and pulls each
through the full ingest pipeline: encora-id resolution, canonical rename
into ` + "`library.root`" + `, optional subtitle download, and ` + "`movie.nfo`" +
		` write. Use ` + "`--dry-run`" + ` to preview the plan without touching
the filesystem.`,
	Args: cobra.ExactArgs(1),
	RunE: runLibraryIngest,
}

//nolint:gochecknoinits // cobra requires init for command registration
func init() {
	libraryIngestCmd.Flags().IntVar(
		&ingestEncoraID,
		"encora-id",
		0,
		"explicit Encora recording ID (only valid when SRC is a single file)",
	)
	libraryIngestCmd.Flags().BoolVar(
		&ingestDryRun,
		"dry-run",
		false,
		"print the plan without modifying the filesystem",
	)
	libraryIngestCmd.Flags().BoolVar(
		&ingestInteractive,
		"interactive",
		false,
		"prompt on stdin for missing encora ids",
	)
	libraryIngestCmd.Flags().BoolVar(
		&ingestAddToCollection,
		"add-to-collection",
		false,
		"if a recording isn't in the local cache, post to /collection/{id}/collect and re-sync",
	)
	libraryCmd.AddCommand(libraryIngestCmd)
}

func runLibraryIngest(_ *cobra.Command, _ []string) error {
	return errNotImplemented
}
