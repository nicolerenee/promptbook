package cmd

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/stagemedia"
	"github.com/nicolerenee/promptbook/internal/storage"
	"github.com/nicolerenee/promptbook/internal/sync"
)

// syncImageFetchHTTPTimeout caps each opportunistic image download
// triggered during sync. Same default as the serve-side timeout — see
// imageFetchHTTPTimeout in serve.go for the rationale.
const syncImageFetchHTTPTimeout = 60 * time.Second

//nolint:gochecknoglobals // cobra requires package-level command variable
var collectionSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Pull collection and wants from Encora into the local cache",
	Long: `Fetches the user's collection and wants list from Encora and stores them
in the local SQLite cache. Honors Encora's 30-req/min rate limit and preserves
last-good data on transient failures.`,
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

	// Image cache is opt-in. When library.imageRoot is empty the
	// fetcher in internal/sync stays in no-op mode and the run
	// behaves exactly as it did before this wave.
	var imgCache *imagecache.Cache
	if appConfig.Library.ImageRoot != "" {
		imgCache = imagecache.New(
			appConfig.Library.ImageRoot,
			&http.Client{Timeout: syncImageFetchHTTPTimeout},
			log.Logger,
		)
	} else {
		log.Info().Msg("image cache disabled (library.imageRoot not configured)")
	}

	// StageMedia client is required for posters/headshots; sync's
	// fetcher tolerates a nil client gracefully so an unconfigured
	// API key just skips image caching during this run.
	var smClient *stagemedia.Client
	if appConfig.Stagemedia.APIKey != "" {
		smClient, err = stagemedia.New(stagemedia.Options{
			BaseURL:   appConfig.Stagemedia.BaseURL,
			APIKey:    appConfig.Stagemedia.APIKey,
			UserAgent: appConfig.Stagemedia.UserAgent,
			Logger:    log.Logger,
		})
		if err != nil {
			return fmt.Errorf("build stagemedia client: %w", err)
		}
	}
	// As elsewhere in the codebase, coerce a nil *stagemedia.Client
	// into a true nil sync.StagemediaImageClient interface so the
	// downstream nil-check fires correctly.
	var smOpt sync.StagemediaImageClient
	if smClient != nil {
		smOpt = smClient
	}

	// Pipe the same encora client into the image fetcher so it can
	// pull /recording/{id}/screenshots when has_screenshots == true.
	// Coerce to a true nil interface when image caching is off so the
	// downstream nil-check fires correctly.
	var encScreenshots sync.EncoraScreenshotClient
	if imgCache != nil && !imgCache.Disabled() {
		encScreenshots = client
	}

	res, err := sync.Sync(ctx, client, db, sync.Options{
		BurstReserve:      appConfig.Encora.RateLimit.BurstReserve,
		Logger:            log.Logger,
		ImageCache:        imgCache,
		Stagemedia:        smOpt,
		EncoraScreenshots: encScreenshots,
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
