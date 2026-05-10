package cmd

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/ingest"
	"github.com/nicolerenee/promptbook/internal/probe"
	"github.com/nicolerenee/promptbook/internal/storage"
)

//nolint:gochecknoglobals // cobra CLI flags require package-level variables
var (
	ingestEncoraID        int
	ingestDryRun          bool
	ingestInteractive     bool
	ingestAddToCollection bool
)

// ingestSubtitleHTTPTimeout caps subtitle downloads so a stuck CDN
// connection can't wedge the whole ingest run.
const ingestSubtitleHTTPTimeout = 60 * time.Second

//nolint:gochecknoglobals // cobra requires package-level command variable
var libraryIngestCmd = &cobra.Command{
	Use:   "ingest SRC",
	Short: "Resolve, rename, subtitle, and NFO each video under SRC into the library",
	Long: `Walks SRC for video files (or single-video folders) and pulls each
through the full ingest pipeline: encora-id resolution, canonical rename
into ` + "`library.root`" + `, optional subtitle download, and ` + "`movie.nfo`" +
		` write. Use ` + "`--dry-run`" + ` to preview the plan without touching
the filesystem.`,
	Args: cobra.ExactArgs(1),
	RunE: runLibraryIngest,
}

//nolint:gochecknoinits // cobra requires init for command registration
func init() {
	libraryIngestCmd.Flags().IntVar(
		&ingestEncoraID,
		"encora-id",
		0,
		"explicit Encora recording ID (only valid when SRC is a single file)",
	)
	libraryIngestCmd.Flags().BoolVar(
		&ingestDryRun,
		"dry-run",
		false,
		"print the plan without modifying the filesystem",
	)
	libraryIngestCmd.Flags().BoolVar(
		&ingestInteractive,
		"interactive",
		false,
		"prompt on stdin for missing encora ids",
	)
	libraryIngestCmd.Flags().BoolVar(
		&ingestAddToCollection,
		"add-to-collection",
		false,
		"if a recording isn't in the local cache, post to /collection/{id}/collect and re-sync",
	)
	libraryCmd.AddCommand(libraryIngestCmd)
}

func runLibraryIngest(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	src := args[0]

	if appConfig.Library.Root == "" {
		return errors.New("library.root is not configured")
	}

	_, db, err := storage.OpenEnt(ctx, appConfig.Storage.DatabasePath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	client, err := buildEncoraClientForIngest()
	if err != nil {
		return err
	}

	// Optional image cache: when library.imageRoot is configured, the
	// NFO writer will emit <thumb> / <fanart> hints pointing at cached
	// posters/backdrops. Empty root yields a nil cache and the writer
	// quietly omits those elements.
	var imgCache *imagecache.Cache
	if appConfig.Library.ImageRoot != "" {
		imgCache = imagecache.New(appConfig.Library.ImageRoot, nil, log.Logger)
	}

	engine := &ingest.Engine{
		DB:             db,
		Client:         client,
		LibraryRoot:    appConfig.Library.Root,
		FolderTemplate: appConfig.Library.FolderTemplate,
		FileTemplate:   appConfig.Library.FileTemplate,
		SubtitleFetcher: &ingest.HTTPSubtitleFetcher{
			HTTP: &http.Client{Timeout: ingestSubtitleHTTPTimeout},
		},
		InteractiveReader: os.Stdin,
		Logger:            log.Logger,
		ImageCache:        imgCache,
		Prober:            probe.FFProbe{Path: appConfig.Library.FFProbePath},
	}

	res, err := engine.Ingest(ctx, src, ingest.Options{
		FlagEncoraID:    ingestEncoraID,
		DryRun:          ingestDryRun,
		Interactive:     ingestInteractive,
		AddToCollection: ingestAddToCollection,
	})
	if err != nil {
		return fmt.Errorf("ingest: %w", err)
	}

	for _, item := range res.Items {
		evt := log.Info().
			Str("source", item.Source).
			Str("action", item.Action).
			Int64("encora_id", item.EncoraID).
			Str("resolved_from", string(item.ResolvedFrom))
		if item.Plan != nil {
			evt = evt.Str("dest", item.Plan.AbsoluteFile())
		}
		if item.SkippedReason != "" {
			evt = evt.Str("reason", item.SkippedReason)
		}
		if item.Err != nil {
			evt = evt.AnErr("err", item.Err)
		}
		evt.Msg("ingest item")
	}
	return nil
}

// buildEncoraClientForIngest assembles the encora client. ingest only
// needs Subtitles and AddToCollection; both are off the same client.
func buildEncoraClientForIngest() (*encora.Client, error) {
	if appConfig.Encora.APIKey == "" {
		// Allow ingest without an API key as long as the recording is
		// already cached and has_subtitles=false. Dry-runs always work.
		return encora.New(encora.Options{
			BaseURL: appConfig.Encora.BaseURL, APIKey: "unset",
			UserAgent: appConfig.Encora.UserAgent,
		})
	}
	c, err := encora.New(encora.Options{
		BaseURL:   appConfig.Encora.BaseURL,
		APIKey:    appConfig.Encora.APIKey,
		UserAgent: appConfig.Encora.UserAgent,
	})
	if err != nil {
		return nil, fmt.Errorf("build encora client: %w", err)
	}
	return c, nil
}
