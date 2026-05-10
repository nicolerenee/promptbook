package builtin

import (
	"context"
	"errors"
	"fmt"

	"github.com/rs/zerolog"

	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/imagerender"
	"github.com/nicolerenee/promptbook/internal/jobs"
	"github.com/nicolerenee/promptbook/internal/nforefresh"
	"github.com/nicolerenee/promptbook/internal/storage"
	pbsync "github.com/nicolerenee/promptbook/internal/sync"
)

// RefreshRecordingImagesJob fetches the wide fanart from Encora's
// /screenshots and writes recordings/<id>/fanart.jpg, then fetches the
// vertical poster source from StageMedia and writes
// recordings/<id>/poster-src.jpg. Finally it kicks the renderer to
// produce poster.jpg with the burned-in overlay.
//
// StageMedia gives the show-level POSTER (vertical) which we use as
// the poster source. Encora /screenshots gives wide screen-grabs which
// we use as fanart. Both upstream calls are gated on the destination
// not already existing (or force=true).
type RefreshRecordingImagesJob struct {
	DB       *DBConn
	Cache    *imagecache.Cache
	Encora   pbsync.EncoraScreenshotClient
	SM       pbsync.StagemediaImageClient
	Renderer *imagerender.Renderer
	Logger   zerolog.Logger
	// NFORefresh, when non-nil, gets RewriteForRecording(recID) called
	// after a successful image fetch + render so the recording's
	// movie.nfo picks up the new mtime in its `?v=` cache-buster.
	// nil leaves the NFO untouched — fine when the recording hasn't
	// been imported yet, since there's no NFO on disk to refresh.
	NFORefresh *nforefresh.Service
}

// jobNameRefreshRecordingImages is the registry key.
const jobNameRefreshRecordingImages = "refresh-recording-images"

// Name returns the registry key.
func (j *RefreshRecordingImagesJob) Name() string { return jobNameRefreshRecordingImages }

// Run fetches the recording's images. Args:
//
//	recording_id: int64 (required, must be > 0)
//	force:        bool  (optional; overwrite existing fanart + poster)
func (j *RefreshRecordingImagesJob) Run(ctx context.Context, args jobs.JobArgs) error {
	recID := args.GetInt64("recording_id")
	if recID <= 0 {
		return errors.New("refresh-recording-images: missing or invalid recording_id arg")
	}
	force := args.GetBool("force")

	if j.Cache == nil || j.Cache.Disabled() {
		return errors.New("refresh-recording-images: image cache not configured")
	}

	// Resolve show_id + has_screenshots from the cached recording row.
	// A missing recording isn't fatal — the job just no-ops, since the
	// fan-out scan reads from the same table moments earlier.
	loaded, err := storage.LoadRecording(ctx, j.DB, recID)
	if errors.Is(err, storage.ErrRecordingNotFound) {
		j.Logger.Debug().
			Int64("recording_id", recID).
			Msg("refresh-recording-images: recording not in catalog; skipping")
		return nil
	}
	if err != nil {
		return fmt.Errorf("refresh-recording-images: load recording %d: %w", recID, err)
	}

	// Fanart from Encora screen-grabs. has_screenshots=false means the
	// upstream guarantees an empty list; we skip the call entirely so a
	// rate-limit budget isn't spent on guaranteed empties.
	if loaded.Recording.Metadata.HasScreenshots {
		if fetchErr := j.persistFanart(ctx, recID, force); fetchErr != nil {
			j.Logger.Debug().
				Err(fetchErr).
				Int64("recording_id", recID).
				Msg("refresh-recording-images: fanart fetch failed")
		}
	}

	// Poster source from StageMedia, then render.
	if posterErr := j.persistPosterAndRender(ctx, recID,
		loaded.Recording.Metadata.ShowID, force); posterErr != nil {
		j.Logger.Debug().
			Err(posterErr).
			Int64("recording_id", recID).
			Msg("refresh-recording-images: poster fetch/render failed")
	}

	// Refresh the recording's movie.nfo so the writer's
	// `?v={mtime}` cache-buster picks up whichever images we just
	// wrote. Best-effort: a failure here doesn't roll back the image
	// writes. RewriteForRecording is itself a no-op when the
	// recording hasn't been imported yet (no version row on disk).
	if j.NFORefresh != nil {
		if rerr := j.NFORefresh.RewriteForRecording(ctx, recID); rerr != nil {
			j.Logger.Warn().
				Err(rerr).
				Int64("recording_id", recID).
				Msg("refresh-recording-images: nfo rewrite failed")
		}
	}

	return nil
}

// persistFanart fetches the first screen-grab URL from Encora and
// writes it to fanart.jpg. With force=true the existing fanart is
// removed before the fetch so the new bytes land in place.
func (j *RefreshRecordingImagesJob) persistFanart(
	ctx context.Context, recID int64, force bool,
) error {
	if j.Encora == nil {
		return errors.New("encora client not configured")
	}
	if !force && j.Cache.HasRecordingFanart(recID) {
		return nil
	}
	urls, _, err := j.Encora.Screenshots(ctx, recID)
	if err != nil {
		return fmt.Errorf("encora screenshots %d: %w", recID, err)
	}
	url := firstNonEmpty(urls)
	if url == "" {
		return nil
	}
	if force {
		if path := j.Cache.RecordingFanartPath(recID); path != "" {
			_ = removeIfExists(path)
		}
	}
	dest, err := j.Cache.FetchRecordingFanart(ctx, recID, url)
	if err != nil {
		return fmt.Errorf("fetch fanart: %w", err)
	}
	j.Logger.Info().
		Int64("recording_id", recID).
		Bool("force", force).
		Str("dest", dest).
		Msg("refresh-recording-images: fanart written")
	return nil
}

// persistPosterAndRender fetches the poster source from StageMedia
// (keyed on show_id since posters are show-level), writes it to
// poster-src.jpg, then asks the renderer to composite poster.jpg from
// it. force=true clobbers both poster.jpg and poster-src.jpg before
// the fetch.
func (j *RefreshRecordingImagesJob) persistPosterAndRender(
	ctx context.Context, recID, showID int64, force bool,
) error {
	if showID <= 0 {
		return errors.New("recording has no show_id")
	}
	if j.SM == nil {
		return errors.New("stagemedia client not configured")
	}
	if !force && j.Cache.HasRecordingPoster(recID) {
		return nil
	}

	imgs, err := j.SM.Images(ctx, showID, []int64{1})
	if err != nil {
		return fmt.Errorf("stagemedia images %d: %w", showID, err)
	}
	url := firstNonEmpty(imgs.Posters)
	if url == "" {
		return nil
	}
	if force {
		if path := j.Cache.RecordingPosterSrcPath(recID); path != "" {
			_ = removeIfExists(path)
		}
		if path := j.Cache.RecordingPosterPath(recID); path != "" {
			_ = removeIfExists(path)
		}
	}
	dest, err := j.Cache.FetchRecordingPosterSrc(ctx, recID, url)
	if err != nil {
		if errors.Is(err, imagecache.ErrDisabled) {
			return err
		}
		return fmt.Errorf("fetch poster-src: %w", err)
	}
	j.Logger.Info().
		Int64("recording_id", recID).
		Bool("force", force).
		Str("dest", dest).
		Msg("refresh-recording-images: poster-src written")

	// Render unless explicitly disabled. The renderer is nil-safe.
	if regenErr := j.Renderer.Regenerate(ctx, recID); regenErr != nil {
		j.Logger.Warn().
			Err(regenErr).
			Int64("recording_id", recID).
			Msg("refresh-recording-images: poster render failed; poster-src is on disk")
	}
	return nil
}
