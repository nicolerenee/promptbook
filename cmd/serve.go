package cmd

import (
	"fmt"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/nicolerenee/promptbook/internal/server"
	"github.com/nicolerenee/promptbook/internal/storage"
)

//nolint:gochecknoglobals // cobra requires package-level command variable
var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the promptbook HTTP server",
	Long: `Starts the echo HTTP server that serves the catalog pages and the
/api/v1/* JSON endpoints from the local SQLite cache. JWT/OIDC auth is
deferred to a later phase — the default listen address is loopback-only
so the surface stays private.`,
	RunE: runServe,
}

//nolint:gochecknoinits // cobra requires init for command registration
func init() {
	rootCmd.AddCommand(serveCmd)
}

func runServe(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()

	db, err := storage.Open(ctx, appConfig.Storage.DatabasePath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	srv, err := server.New(server.Options{DB: db, Logger: log.Logger})
	if err != nil {
		return fmt.Errorf("build server: %w", err)
	}
	addr := appConfig.Server.Listen
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	return srv.Start(ctx, addr)
}
