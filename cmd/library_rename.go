package cmd

import "github.com/spf13/cobra"

//nolint:gochecknoglobals // cobra CLI flags require package-level variables
var (
	libraryRenameEncoraID int
	libraryRenameDryRun   bool
)

//nolint:gochecknoglobals // cobra requires package-level command variable
var libraryRenameCmd = &cobra.Command{
	Use:   "rename PATH",
	Short: "Rename a single recording in place to the canonical scheme",
	Long: `Renames a video file or folder-with-video to ` +
		"`library.folderTemplate`/`library.fileTemplate`" + `. No subtitle
download, no NFO. Useful for fixing a folder that's already in the library
under the wrong scheme. Use ` + "`library ingest`" + ` for the full workflow.`,
	Args: cobra.ExactArgs(1),
	RunE: runLibraryRename,
}

//nolint:gochecknoinits // cobra requires init for command registration
func init() {
	libraryRenameCmd.Flags().IntVar(
		&libraryRenameEncoraID,
		"encora-id",
		0,
		"explicit Encora recording ID (overrides sidecar/filename detection)",
	)
	libraryRenameCmd.Flags().BoolVar(
		&libraryRenameDryRun,
		"dry-run",
		false,
		"show what would change without touching the filesystem",
	)
	libraryCmd.AddCommand(libraryRenameCmd)
}

func runLibraryRename(_ *cobra.Command, _ []string) error {
	return errNotImplemented
}
