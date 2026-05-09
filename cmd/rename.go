package cmd

import (
	"github.com/spf13/cobra"
)

//nolint:gochecknoglobals // cobra CLI flags require package-level variables
var (
	renameEncoraID int
	renameDryRun   bool
)

//nolint:gochecknoglobals // cobra requires package-level command variable
var renameCmd = &cobra.Command{
	Use:   "rename [path]",
	Short: "Rename a recording on disk to the canonical scheme",
	Long: `Given a path to a video file or a folder containing one, rename it (and the
parent folder) according to the configured library.folderTemplate and
library.fileTemplate. The Encora ID can be supplied via --encora-id, an
.encora-id sidecar file in the folder, or parsed from the existing name in
formats like [encora-NNN], {e-NNN}, or [e-NNN].`,
	Args: cobra.ExactArgs(1),
	RunE: runRename,
}

//nolint:gochecknoinits // cobra requires init for command registration
func init() {
	renameCmd.Flags().IntVar(&renameEncoraID, "encora-id", 0, "explicit Encora recording ID (overrides sidecar/filename detection)")
	renameCmd.Flags().BoolVar(&renameDryRun, "dry-run", false, "show what would change without touching the filesystem")
	rootCmd.AddCommand(renameCmd)
}

func runRename(_ *cobra.Command, _ []string) error {
	return errNotImplemented
}
