package cmd

import "github.com/spf13/cobra"

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

func runLibraryScan(_ *cobra.Command, _ []string) error {
	return errNotImplemented
}
