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

//nolint:gochecknoglobals // cobra requires package-level command variable
var collectionSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Pull collection and wants from Encora into the local cache",
	Long: `Fetches the user's collection and wants list from Encora and stores them
in the local SQLite cache. Honors Encora's 30-req/min rate limit and preserves
last-good data on transient failures.

Image caching has moved to per-entity refresh-show-images,
refresh-recording-images, and refresh-actor-headshot jobs that the
serve-side scheduler enqueues after each sync. The CLI sync no
longer fetches images itself.`,
	RunE: runCollectionSync,
}

//nolint:gochecknoinits // cobra requires init for command registration
func init() {
	collectionCmd.AddCommand(collectionSyncCmd)
}

func runCollectionSync(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()

	if appConfig.Encora.APIKey == "" {
		return errors.New("encora.apiKey is not configured (set PROMPTBOOK_ENCORA_APIKEY)")
	}

	client, err := encora.New(encora.Options{
		BaseURL: appConfig.Encora.BaseURL,
		APIKey:  appConfig.Encora.APIKey,
	})
	if err != nil {
		return fmt.Errorf("build encora client: %w", err)
	}

	sqlDB, db, err := storage.OpenEnt(ctx, appConfig.Storage.DatabasePath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()

	res, err := sync.Sync(ctx, client, db, sync.Options{
		BurstReserve: appConfig.Encora.RateLimit.BurstReserve,
		Logger:       log.Logger,
		SQLDB:        sqlDB,
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
