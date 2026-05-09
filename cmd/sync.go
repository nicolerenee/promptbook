package cmd

import (
	"errors"
	"fmt"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/storage"
	"github.com/nicolerenee/promptbook/internal/sync"
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

func runSync(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()

	if appConfig.Encora.APIKey == "" {
		return errors.New("encora.apiKey is not configured (set PROMPTBOOK_ENCORA_APIKEY)")
	}

	client, err := encora.New(encora.Options{
		BaseURL:   appConfig.Encora.BaseURL,
		APIKey:    appConfig.Encora.APIKey,
		UserAgent: appConfig.Encora.UserAgent,
	})
	if err != nil {
		return fmt.Errorf("build encora client: %w", err)
	}

	db, err := storage.Open(ctx, appConfig.Storage.DatabasePath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	res, err := sync.Sync(ctx, client, db, sync.Options{
		BurstReserve: appConfig.Encora.RateLimit.BurstReserve,
		Logger:       log.Logger,
	})
	if err != nil {
		return fmt.Errorf("sync: %w", err)
	}

	log.Info().
		Int64("run_id", res.RunID).
		Int("collection", res.CollectionCount).
		Int("wants", res.WantsCount).
		Int("rate_limit_remaining", res.RateLimitRemaining).
		Bool("rate_limited_bailed_out", res.RateLimitedBailedOut).
		Msg("sync complete")
	return nil
}
