package builtin

import (
	"context"
	"errors"
	"fmt"

	"github.com/rs/zerolog"

	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/jobs"
	"github.com/nicolerenee/promptbook/internal/nforefresh"
	pbsync "github.com/nicolerenee/promptbook/internal/sync"
)

// RefreshShowImagesJob fetches a show's banner from StageMedia and
// writes shows/<id>/banner.jpg. With force=true, overwrites an
// existing file; without force, the job is a no-op when the slot is
// already populated.
//
// Performer headshots ride along for free in StageMedia's /api/images
// response, so this job opportunistically writes them too — but only
// for actors that don't already have a headshot. (Re-fetching existing
// headshots without force would clobber any user-uploaded ones.)
type RefreshShowImagesJob struct {
	DB     *DBConn
	Cache  *imagecache.Cache
	SM     pbsync.StagemediaImageClient
	Logger zerolog.Logger
	// NFORefresh, when non-nil, gets RewriteForShow(showID) called
	// after a successful banner write so every recording for the
	// show picks up the new banner-mtime in its <set><thumb> cache-
	// buster. The headshot writes inside this job each get their own
	// RewriteForPerformer call. nil leaves the NFOs untouched.
	NFORefresh *nforefresh.Service
}

// jobNameRefreshShowImages is the registry key. Stable string —
// surfaced in logs, the job_runs.job_name column, and the UI route.
const jobNameRefreshShowImages = "refresh-show-images"

// Name returns the registry key.
func (j *RefreshShowImagesJob) Name() string { return jobNameRefreshShowImages }

// Run fetches the show's images. Args:
//
//	show_id: int64 (required, must be > 0)
//	force:   bool  (optional; overwrite an existing banner)
func (j *RefreshShowImagesJob) Run(ctx context.Context, args jobs.JobArgs) error {
	showID := args.GetInt64("show_id")
	if showID <= 0 {
		return errors.New("refresh-show-images: missing or invalid show_id arg")
	}
	force := args.GetBool("force")

	if j.SM == nil {
		return errors.New("refresh-show-images: stagemedia client not configured")
	}
	if j.Cache == nil || j.Cache.Disabled() {
		return errors.New("refresh-show-images: image cache not configured")
	}

	// Skip-if-present unless forced. The cache's Fetch* helper is itself
	// idempotent, but checking up-front lets us short-circuit before the
	// /api/images network call when nothing's needed.
	if !force && j.Cache.HasShowBanner(showID) {
		j.Logger.Debug().
			Int64("show_id", showID).
			Msg("refresh-show-images: banner already on disk; skipping")
		return nil
	}

	performerIDs := loadPerformerIDsForShow(ctx, j.DB, showID)
	if len(performerIDs) == 0 {
		// /api/images requires at least one actor_ids value. Use the
		// stagemedia sentinel so we still get poster URLs.
		performerIDs = []int64{1}
	}

	imgs, err := j.SM.Images(ctx, showID, performerIDs)
	if err != nil {
		return fmt.Errorf("refresh-show-images: stagemedia fetch %d: %w", showID, err)
	}

	if bannerErr := j.persistBanner(ctx, showID, imgs.Posters, force); bannerErr != nil {
		return bannerErr
	}
	// HasShowBanner is a cheap stat; check after persistBanner so the
	// post-write fan-out only fires when the banner is actually on
	// disk (StageMedia returning no posters is a successful no-op,
	// not a write).
	bannerWritten := j.Cache.HasShowBanner(showID)

	headshotIDs := j.persistHeadshots(ctx, imgs.Performers)

	// Banner change → rewrite every recording's NFO under the show.
	// Headshot changes → rewrite each affected performer's recordings.
	// Both fan-outs are best-effort and don't roll back on failure.
	if j.NFORefresh != nil {
		if bannerWritten {
			if rerr := j.NFORefresh.RewriteForShow(ctx, showID); rerr != nil {
				j.Logger.Warn().
					Err(rerr).
					Int64("show_id", showID).
					Msg("refresh-show-images: nfo show fan-out failed")
			}
		}
		for _, actorID := range headshotIDs {
			if rerr := j.NFORefresh.RewriteForPerformer(ctx, actorID); rerr != nil {
				j.Logger.Warn().
					Err(rerr).
					Int64("actor_id", actorID).
					Msg("refresh-show-images: nfo performer fan-out failed")
			}
		}
	}
	return nil
}

// persistBanner writes the first non-empty poster URL to the show
// banner slot. With force=true the existing banner is replaced via a
// fetch + atomic rename. Returns nil and logs at debug when no poster
// URLs were returned (StageMedia silently drops unknown shows).
func (j *RefreshShowImagesJob) persistBanner(
	ctx context.Context, showID int64, urls []string, force bool,
) error {
	url := firstNonEmpty(urls)
	if url == "" {
		j.Logger.Debug().
			Int64("show_id", showID).
			Msg("refresh-show-images: stagemedia returned no posters")
		return nil
	}
	if force {
		// FetchShowBanner is idempotent on existing files; remove the
		// existing banner up-front so the next call lands fresh bytes.
		if path := j.Cache.ShowBannerPath(showID); path != "" {
			_ = removeIfExists(path)
		}
	}
	dest, err := j.Cache.FetchShowBanner(ctx, showID, url)
	if err != nil {
		if errors.Is(err, imagecache.ErrDisabled) {
			return err
		}
		return fmt.Errorf("refresh-show-images: fetch banner: %w", err)
	}
	j.Logger.Info().
		Int64("show_id", showID).
		Bool("force", force).
		Str("dest", dest).
		Msg("refresh-show-images: banner written")
	return nil
}

// persistHeadshots fetches each performer's headshot when it's not
// already on disk. Failures are logged at debug and never bubble up —
// a bad CDN url shouldn't fail the whole show refresh. Returns the
// actor ids whose headshots were actually written so the caller can
// fan out an NFO refresh per affected performer.
func (j *RefreshShowImagesJob) persistHeadshots(
	ctx context.Context, performers []stagemediaPerformer,
) []int64 {
	var written []int64
	for _, p := range performers {
		if p.URL == "" || p.ID == 0 {
			continue
		}
		// Don't re-fetch existing headshots without an explicit force —
		// a manual user upload must not be clobbered by the next sync.
		if j.Cache.HasHeadshot(p.ID) {
			continue
		}
		if _, err := j.Cache.FetchHeadshot(ctx, p.ID, p.URL); err != nil {
			if errors.Is(err, imagecache.ErrDisabled) {
				return written
			}
			j.Logger.Debug().
				Err(err).
				Int64("actor_id", p.ID).
				Str("url", p.URL).
				Msg("refresh-show-images: headshot fetch failed")
			continue
		}
		written = append(written, p.ID)
	}
	return written
}
