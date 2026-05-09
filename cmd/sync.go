package cmd

import (
	"errors"

	"github.com/spf13/cobra"
)

// errNotImplemented is returned by stub subcommands.
var errNotImplemented = errors.New("not implemented yet")

//nolint:gochecknoglobals // cobra requires package-level command variable
var syncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Pull collection and wants from Encora into the local cache",
	Long: `Fetches the user's collection and wants list from Encora and stores them
in the local SQLite cache. Honors Encora's 30-req/min rate limit and preserves
last-good data on transient failures.`,
	RunE: runSync,
}

//nolint:gochecknoinits // cobra requires init for command registration
func init() {
	rootCmd.AddCommand(syncCmd)
}

func runSync(_ *cobra.Command, _ []string) error {
	return errNotImplemented
}
