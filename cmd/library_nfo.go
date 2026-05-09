package cmd

import (
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/nicolerenee/promptbook/internal/nfo"
	"github.com/nicolerenee/promptbook/internal/rename"
	"github.com/nicolerenee/promptbook/internal/storage"
)

//nolint:gochecknoglobals // cobra CLI flags require package-level variables
var libraryNFODryRun bool

//nolint:gochecknoglobals // cobra requires package-level command variable
var libraryNFOCmd = &cobra.Command{
	Use:   "nfo PATH",
	Short: "Regenerate Jellyfin movie.nfo files for canonically-named recordings",
	Long: `Walks a tree of canonically-named recording folders, extracts the
encora id from each folder, looks up the recording in the local cache, and
writes ` + "`movie.nfo`" + ` alongside the video. Use this after editing notes
in the local DB or whenever the NFO writer changes shape.`,
	Args: cobra.ExactArgs(1),
	RunE: runLibraryNFO,
}

//nolint:gochecknoinits // cobra requires init for command registration
func init() {
	libraryNFOCmd.Flags().BoolVar(
		&libraryNFODryRun,
		"dry-run",
		false,
		"show what would change without writing NFO files",
	)
	libraryCmd.AddCommand(libraryNFOCmd)
}

func runLibraryNFO(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	root := args[0]

	db, err := storage.Open(ctx, appConfig.Storage.DatabasePath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() {
			return nil
		}
		// Try to extract an encora id from the directory name. If
		// none, this isn't a canonical folder — skip silently.
		id, _, idErr := rename.Resolve(path, 0)
		if idErr != nil {
			return nil //nolint:nilerr // non-canonical dirs are expected
		}

		loaded, lErr := storage.LoadRecording(ctx, db, id)
		if lErr != nil {
			// Cache miss is expected on first ingest of a brand-new
			// recording; warn so the user can re-sync but don't fail
			// the whole walk.
			log.Warn().Err(lErr).Int64("encora_id", id).Str("dir", path).
				Msg("recording not in cache; skipping")
			return nil
		}

		dest := filepath.Join(path, "movie.nfo")
		if libraryNFODryRun {
			log.Info().Str("dir", path).Int64("encora_id", id).Str("would_write", dest).
				Msg("would regenerate nfo")
			return nil
		}

		written, writeErr := nfo.WriteFile(path, nfo.FromRecording(loaded.Recording))
		if writeErr != nil {
			return fmt.Errorf("write nfo for %s: %w", path, writeErr)
		}
		log.Info().Str("dir", path).Int64("encora_id", id).Str("wrote", written).
			Msg("nfo regenerated")
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("walk %s: %w", root, walkErr)
	}
	return nil
}
