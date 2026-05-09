package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

// zerologger is a local alias so the migration helpers don't need to
// import zerolog directly. Keeps the helper signatures readable
// without spelling out the full zerolog.Logger type each time.
type zerologger = zerolog.Logger

// migrateImagesLegacyDir is the relocated home for the v1 layout
// after a successful migration. Renamed-not-deleted so the user can
// recover originals manually if they spot a bad copy.
const migrateImagesLegacyDir = ".legacy-pre-v2"

// Permissions used by the walker to create v2 destinations. Match the
// imagecache package's defaults.
const (
	migrateDirMode  os.FileMode = 0o750
	migrateFileMode os.FileMode = 0o600
)

//nolint:gochecknoglobals // cobra requires package-level command variable
var libraryMigrateImagesCmd = &cobra.Command{
	Use:   "migrate-images",
	Short: "Migrate the v1 indexed image cache to the v2 single-file-per-slot layout",
	Long: `Walks <library.imageRoot> and rewrites the legacy v1 layout
(posters/<show_id>/<n>.jpg, backdrops/<recording_id>/<n>.jpg,
headshots/<actor_id>.jpg) into the v2 single-file-per-slot layout
(shows/<show_id>/banner.jpg, recordings/<recording_id>/{fanart,poster}.jpg,
actors/<actor_id>.jpg).

For each show the lowest-indexed poster becomes the banner. For each
recording the lowest-indexed backdrop becomes the fanart, and the
old rendered.jpg (if present) is copied to poster.jpg as a best-
effort starting point — re-render via the UI to refresh it.

The legacy layout is preserved by renaming the old top-level
directories into <imageRoot>/.legacy-pre-v2/ rather than deleting
them. Re-running on an already-migrated tree is a clean no-op.`,
	RunE: runLibraryMigrateImages,
}

//nolint:gochecknoinits // cobra requires init for command registration
func init() {
	libraryCmd.AddCommand(libraryMigrateImagesCmd)
}

// migrateStats collects the migration tally for the summary log.
type migrateStats struct {
	banners, fanarts, posters, headshots, skipped int
}

func runLibraryMigrateImages(_ *cobra.Command, _ []string) error {
	root := strings.TrimSpace(appConfig.Library.ImageRoot)
	if root == "" {
		return errors.New("library.imageRoot is not configured")
	}
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("stat %s: %w", root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", root)
	}

	logger := log.With().Str("image_root", root).Logger()

	stats := migrateStats{}
	if mErr := migrateBanners(root, &stats, logger); mErr != nil {
		return mErr
	}
	if mErr := migrateBackdrops(root, &stats, logger); mErr != nil {
		return mErr
	}
	if mErr := migrateHeadshots(root, &stats, logger); mErr != nil {
		return mErr
	}
	if mErr := relocateLegacy(root, logger); mErr != nil {
		return mErr
	}

	logger.Info().
		Int("show_banners", stats.banners).
		Int("recording_fanarts", stats.fanarts).
		Int("recording_posters", stats.posters).
		Int("actor_headshots", stats.headshots).
		Int("skipped", stats.skipped).
		Msg("library migrate-images: complete")
	return nil
}

// migrateBanners walks posters/<show_id>/<n>.jpg and copies the
// lowest-indexed file (sorted by integer index) into
// shows/<show_id>/banner.jpg. Skips entire shows that already have a
// banner on disk.
func migrateBanners(root string, stats *migrateStats, logger zerologger) error {
	srcRoot := filepath.Join(root, "posters")
	if !dirExists(srcRoot) {
		return nil
	}
	entries, err := os.ReadDir(srcRoot)
	if err != nil {
		return fmt.Errorf("read %s: %w", srcRoot, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		showID, parseErr := strconv.ParseInt(e.Name(), 10, 64)
		if parseErr != nil || showID <= 0 {
			continue
		}
		dest := filepath.Join(root, "shows", strconv.FormatInt(showID, 10), "banner.jpg")
		if fileExists(dest) {
			stats.skipped++
			continue
		}
		src, srcErr := lowestIndexedFile(filepath.Join(srcRoot, e.Name()))
		if srcErr != nil || src == "" {
			continue
		}
		if copyErr := copyMigrationFile(src, dest); copyErr != nil {
			logger.Warn().Err(copyErr).Str("src", src).Str("dest", dest).
				Msg("migrate-images: copy banner failed")
			continue
		}
		stats.banners++
	}
	return nil
}

// migrateBackdrops walks backdrops/<recording_id>/ and copies the
// lowest-indexed file to fanart.jpg. If a rendered.jpg sibling
// exists, copies it to poster.jpg.
func migrateBackdrops(root string, stats *migrateStats, logger zerologger) error {
	srcRoot := filepath.Join(root, "backdrops")
	if !dirExists(srcRoot) {
		return nil
	}
	entries, err := os.ReadDir(srcRoot)
	if err != nil {
		return fmt.Errorf("read %s: %w", srcRoot, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		recID, parseErr := strconv.ParseInt(e.Name(), 10, 64)
		if parseErr != nil || recID <= 0 {
			continue
		}
		migrateOneBackdrop(root, recID, filepath.Join(srcRoot, e.Name()),
			stats, logger)
	}
	return nil
}

// migrateOneBackdrop handles the per-recording fanart + poster
// migration. Pulled out so migrateBackdrops stays under the gocognit
// cap.
func migrateOneBackdrop(
	root string, recID int64, srcDir string,
	stats *migrateStats, logger zerologger,
) {
	destDir := filepath.Join(root, "recordings", strconv.FormatInt(recID, 10))
	fanartDest := filepath.Join(destDir, "fanart.jpg")
	if fileExists(fanartDest) {
		stats.skipped++
	} else if src, srcErr := lowestIndexedFile(srcDir); srcErr == nil && src != "" {
		if copyErr := copyMigrationFile(src, fanartDest); copyErr != nil {
			logger.Warn().Err(copyErr).Str("src", src).Str("dest", fanartDest).
				Msg("migrate-images: copy fanart failed")
		} else {
			stats.fanarts++
		}
	}

	posterDest := filepath.Join(destDir, "poster.jpg")
	renderedSrc := filepath.Join(srcDir, "rendered.jpg")
	if fileExists(posterDest) || !fileExists(renderedSrc) {
		return
	}
	if copyErr := copyMigrationFile(renderedSrc, posterDest); copyErr != nil {
		logger.Warn().Err(copyErr).Str("src", renderedSrc).Str("dest", posterDest).
			Msg("migrate-images: copy poster failed")
		return
	}
	stats.posters++
}

// migrateHeadshots walks headshots/<actor_id>.jpg and renames each
// into actors/<actor_id>.jpg.
func migrateHeadshots(root string, stats *migrateStats, logger zerologger) error {
	srcRoot := filepath.Join(root, "headshots")
	if !dirExists(srcRoot) {
		return nil
	}
	entries, err := os.ReadDir(srcRoot)
	if err != nil {
		return fmt.Errorf("read %s: %w", srcRoot, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		// headshots/<actor_id>.jpg → actors/<actor_id>.jpg.
		base := e.Name()
		if !strings.HasSuffix(base, ".jpg") {
			continue
		}
		actorID, parseErr := strconv.ParseInt(strings.TrimSuffix(base, ".jpg"), 10, 64)
		if parseErr != nil || actorID <= 0 {
			continue
		}
		src := filepath.Join(srcRoot, base)
		dest := filepath.Join(root, "actors", base)
		if fileExists(dest) {
			stats.skipped++
			continue
		}
		if copyErr := copyMigrationFile(src, dest); copyErr != nil {
			logger.Warn().Err(copyErr).Str("src", src).Str("dest", dest).
				Msg("migrate-images: copy headshot failed")
			continue
		}
		stats.headshots++
	}
	return nil
}

// relocateLegacy renames the v1 top-level directories into the
// .legacy-pre-v2 sibling so the user has a recovery path. Idempotent:
// when the legacy dir already exists or the source is gone, the move
// is skipped silently.
func relocateLegacy(root string, logger zerologger) error {
	legacyDir := filepath.Join(root, migrateImagesLegacyDir)
	if err := os.MkdirAll(legacyDir, migrateDirMode); err != nil {
		return fmt.Errorf("mkdir %s: %w", legacyDir, err)
	}
	for _, name := range []string{"posters", "backdrops", "headshots"} {
		src := filepath.Join(root, name)
		if !dirExists(src) {
			continue
		}
		dest := filepath.Join(legacyDir, name)
		if dirExists(dest) {
			// A previous run already relocated; leave it alone.
			continue
		}
		if err := os.Rename(src, dest); err != nil {
			logger.Warn().Err(err).Str("src", src).Str("dest", dest).
				Msg("migrate-images: relocate failed; manual cleanup may be needed")
			continue
		}
		logger.Info().Str("dir", name).Msg("migrate-images: legacy dir relocated")
	}
	return nil
}

// lowestIndexedFile returns the path of the file in dir whose basename
// (without extension) parses as the smallest integer. Returns "" with
// no error when no such file exists. Used to pick the v1 default
// (lowest index) for the v2 single slot.
func lowestIndexedFile(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", dir, err)
	}
	bestIdx := -1
	best := ""
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".jpg") {
			continue
		}
		stem := strings.TrimSuffix(name, ".jpg")
		idx, parseErr := strconv.Atoi(stem)
		if parseErr != nil {
			// rendered.jpg lands here — skip.
			continue
		}
		if bestIdx == -1 || idx < bestIdx {
			bestIdx = idx
			best = filepath.Join(dir, name)
		}
	}
	return best, nil
}

// copyMigrationFile streams src to dest atomically (via .tmp +
// rename). Creates the parent directory on demand.
func copyMigrationFile(src, dest string) error {
	if mkErr := os.MkdirAll(filepath.Dir(dest), migrateDirMode); mkErr != nil {
		return fmt.Errorf("mkdir: %w", mkErr)
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open src: %w", err)
	}
	defer func() { _ = in.Close() }()

	tmp := dest + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, migrateFileMode)
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	if _, copyErr := io.Copy(out, in); copyErr != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("copy: %w", copyErr)
	}
	if closeErr := out.Close(); closeErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close tmp: %w", closeErr)
	}
	if renameErr := os.Rename(tmp, dest); renameErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename: %w", renameErr)
	}
	return nil
}

// dirExists reports whether path is an existing directory.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// fileExists reports whether path is an existing regular file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}
