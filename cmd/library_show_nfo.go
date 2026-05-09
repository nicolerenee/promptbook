package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/nfo"
	"github.com/nicolerenee/promptbook/internal/rename"
	"github.com/nicolerenee/promptbook/internal/storage"
)

//nolint:gochecknoglobals // cobra CLI flags require package-level variables
var libraryShowNFODryRun bool

//nolint:gochecknoglobals // cobra requires package-level command variable
var libraryShowNFOCmd = &cobra.Command{
	Use:   "show-nfo PATH",
	Short: "Write Plex/Jellyfin collection.nfo for every show with library files",
	Long: `Walks a tree of canonically-named recording folders, groups them by
show via the encora id baked into each folder name, and writes a
` + "`collection.nfo`" + ` at the parent show directory. The NFO surfaces the
show name, year span, and (when configured) a reference to the
user-curated show poster from the local image cache.

This command is intentionally separate from ` + "`library nfo`" + ` (which writes
per-recording movie.nfo). Run it after a fresh ` + "`collection sync`" + ` whenever
show-level metadata changes — the ingest pipeline avoids triggering it
automatically because parallel ingests of multiple recordings under
one show would otherwise race on the same collection.nfo file.`,
	Args: cobra.ExactArgs(1),
	RunE: runLibraryShowNFO,
}

//nolint:gochecknoinits // cobra requires init for command registration
func init() {
	libraryShowNFOCmd.Flags().BoolVar(
		&libraryShowNFODryRun,
		"dry-run",
		false,
		"show what would change without writing collection.nfo files",
	)
	libraryCmd.AddCommand(libraryShowNFOCmd)
}

// showFolderInfo accumulates the per-show metadata across the walk so
// we can emit one collection.nfo per show after the walk completes.
type showFolderInfo struct {
	showID     int64
	showName   string
	dir        string
	recordings []encora.Recording
}

func runLibraryShowNFO(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	root := args[0]

	db, err := storage.Open(ctx, appConfig.Storage.DatabasePath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	var imgCache *imagecache.Cache
	if appConfig.Library.ImageRoot != "" {
		imgCache = imagecache.New(appConfig.Library.ImageRoot, nil, log.Logger)
	}

	shows, err := collectShowFolders(ctx, db, root)
	if err != nil {
		return err
	}

	for _, info := range shows {
		writeShowNFO(ctx, db, imgCache, info)
	}
	return nil
}

// collectShowFolders walks root and groups canonical recording folders
// by show. The show's library directory is taken to be the parent of
// the recording folder — the canonical layout puts every recording
// under <library_root>/<show name>/<recording folder>, and the
// collection.nfo lives at the show level so Plex/Jellyfin can pick it
// up as a single collection scope.
func collectShowFolders(
	ctx context.Context, db *sql.DB, root string,
) (map[int64]*showFolderInfo, error) {
	shows := make(map[int64]*showFolderInfo)
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() {
			return nil
		}
		id, _, idErr := rename.Resolve(path, 0)
		if idErr != nil {
			return nil //nolint:nilerr // non-canonical dirs are expected
		}
		loaded, lErr := storage.LoadRecording(ctx, db, id)
		if lErr != nil {
			if !errors.Is(lErr, storage.ErrRecordingNotFound) {
				log.Warn().Err(lErr).Int64("encora_id", id).Str("dir", path).
					Msg("recording load failed; skipping")
			}
			return nil
		}
		showID := loaded.Recording.Metadata.ShowID
		if showID == 0 {
			return nil
		}
		showDir := filepath.Dir(path)
		entry, ok := shows[showID]
		if !ok {
			entry = &showFolderInfo{
				showID:   showID,
				showName: loaded.Recording.Show,
				dir:      showDir,
			}
			shows[showID] = entry
		}
		entry.recordings = append(entry.recordings, loaded.Recording)
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk %s: %w", root, walkErr)
	}
	return shows, nil
}

// writeShowNFO emits collection.nfo for one show, honoring the
// --dry-run flag. Errors log a warning rather than aborting the loop
// so a single bad show doesn't stop all the others from regenerating.
func writeShowNFO(
	ctx context.Context,
	db *sql.DB,
	cache *imagecache.Cache,
	info *showFolderInfo,
) {
	dest := filepath.Join(info.dir, "collection.nfo")
	if libraryShowNFODryRun {
		log.Info().
			Str("dir", info.dir).
			Int64("show_id", info.showID).
			Str("would_write", dest).
			Msg("would regenerate collection nfo")
		return
	}

	written, err := nfo.WriteShowCollectionFile(ctx, nfo.ShowWriteOptions{
		DB:         db,
		Cache:      cache,
		ShowID:     info.showID,
		ShowName:   info.showName,
		Dir:        info.dir,
		Recordings: info.recordings,
	})
	if err != nil {
		log.Warn().
			Err(err).
			Str("dir", info.dir).
			Int64("show_id", info.showID).
			Msg("write collection nfo failed; continuing")
		return
	}
	log.Info().
		Str("dir", info.dir).
		Int64("show_id", info.showID).
		Str("wrote", written).
		Int("recordings", len(info.recordings)).
		Msg("collection nfo regenerated")
}
