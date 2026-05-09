package cmd

import (
	"github.com/spf13/cobra"
)

//nolint:gochecknoglobals // cobra requires package-level command variable
var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the promptbook HTTP server",
	Long: `Starts the echo HTTP server that exposes the human-facing catalog pages and
the JWT-authenticated /api/v1/* endpoints. Includes a background goroutine that
periodically syncs from Encora.`,
	RunE: runServe,
}

//nolint:gochecknoinits // cobra requires init for command registration
func init() {
	rootCmd.AddCommand(serveCmd)
}

func runServe(_ *cobra.Command, _ []string) error {
	return errNotImplemented
}
