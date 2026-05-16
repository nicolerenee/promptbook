package cmd

import "github.com/spf13/cobra"

// collectionCmd is the parent of all encora-collection-mirror commands.
//
//nolint:gochecknoglobals // cobra requires package-level command variable
var collectionCmd = &cobra.Command{
	Use:   "collection",
	Short: "Mirror and inspect the local Encora collection cache",
	Long: `Commands that operate against the local SQLite cache of the user's
Encora collection and wants list. Use ` + "`promptbook collection sync`" + `
to refresh from Encora and ` + "`promptbook collection show ID`" + ` to inspect
a single recording without making any network calls.`,
}

//nolint:gochecknoinits // cobra requires init for command registration
func init() {
	rootCmd.AddCommand(collectionCmd)
}
