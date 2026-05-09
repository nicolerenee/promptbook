package sync

import (
	"context"
	"errors"
	"sync"

	"github.com/rs/zerolog"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/stagemedia"
)

// imageFetcher is the per-Sync helper that opportunistically downloads
// posters + headshots from StageMedia into the local image cache. It
// owns a dedup set of show IDs scoped to the run so the sync loop only
// calls /api/images once per unique show — without this the worst case
// is one StageMedia round-trip per recording, which would burn the
// upstream's (undocumented but real) rate budget on a fresh sync.
//
// Fetch failures never bubble up. Image caching is best-effort; an
// unreachable CDN must not abort an Encora sync.
type imageFetcher struct {
	cache  *imagecache.Cache
	client StagemediaImageClient
	logger zerolog.Logger

	mu          sync.Mutex
	visitedShow map[int64]bool
}

// newImageFetcher returns a no-op fetcher (Disabled() == true) when
// either the cache is nil/disabled or the StageMedia client is nil.
// Callers can invoke forRecording on the returned value unconditionally.
func newImageFetcher(
	cache *imagecache.Cache,
	client StagemediaImageClient,
	logger zerolog.Logger,
) *imageFetcher {
	return &imageFetcher{
		cache:       cache,
		client:      client,
		logger:      logger,
		visitedShow: map[int64]bool{},
	}
}

// disabled reports whether the fetcher is in no-op mode. The image
// cache being nil/disabled or the StageMedia client being nil both
// short-circuit the StageMedia call.
func (f *imageFetcher) disabled() bool {
	if f == nil {
		return true
	}
	if f.cache == nil || f.cache.Disabled() {
		return true
	}
	if f.client == nil {
		return true
	}
	return false
}

// forRecording fetches posters for the recording's show + headshots
// for every credited performer, skipping anything already on disk and
// deduping show IDs across the whole sync run. Errors are logged and
// swallowed — image caching is never load-bearing for the sync.
func (f *imageFetcher) forRecording(ctx context.Context, r encora.Recording) {
	if f.disabled() {
		return
	}
	showID := r.Metadata.ShowID
	if showID == 0 {
		return
	}

	// Dedup: if we've already issued an /api/images for this show in
	// the current sync, skip the StageMedia call entirely. Posters
	// are show-keyed, so a second call would return the same set;
	// headshots are performer-keyed, but the dedup set is intentionally
	// coarse — if a performer is credited on a show the user has
	// multiple recordings of, the first call already filled their slot.
	f.mu.Lock()
	if f.visitedShow[showID] {
		f.mu.Unlock()
		return
	}
	f.visitedShow[showID] = true
	f.mu.Unlock()

	performerIDs := uniquePerformerIDs(r.Cast)
	if len(performerIDs) == 0 {
		// /api/images requires at least one actor_ids value. Use 1 as
		// the sentinel (matches the stagemedia.Posters helper) so we
		// still get the curated poster URLs.
		performerIDs = []int64{1}
	}

	imgs, err := f.client.Images(ctx, showID, performerIDs)
	if err != nil {
		f.logger.Debug().
			Err(err).
			Int64("show_id", showID).
			Int64("recording_id", r.ID).
			Msg("imagecache: stagemedia images fetch failed")
		return
	}

	f.persistPosters(ctx, showID, imgs.Posters)
	f.persistHeadshots(ctx, imgs.Performers)
}

// persistPosters writes each poster URL to the cache under the show's
// poster slot. Already-cached slots short-circuit inside FetchPoster
// without a network call.
func (f *imageFetcher) persistPosters(ctx context.Context, showID int64, urls []string) {
	for i, url := range urls {
		if url == "" {
			continue
		}
		if f.cache.HasPoster(showID, i) {
			continue
		}
		if _, err := f.cache.FetchPoster(ctx, showID, i, url); err != nil {
			// ErrDisabled would have been caught by f.disabled() at
			// the top; any error here is a transient upstream issue
			// (404, timeout, non-image content-type) — log debug and
			// keep going so one bad image doesn't poison the run.
			if errors.Is(err, imagecache.ErrDisabled) {
				return
			}
			f.logger.Debug().
				Err(err).
				Int64("show_id", showID).
				Int("index", i).
				Str("url", url).
				Msg("imagecache: poster fetch failed")
		}
	}
}

// persistHeadshots writes each performer's headshot URL to the cache.
// Already-cached actors short-circuit without a network call.
func (f *imageFetcher) persistHeadshots(ctx context.Context, performers []stagemedia.Performer) {
	for _, p := range performers {
		if p.URL == "" || p.ID == 0 {
			continue
		}
		if f.cache.HasHeadshot(p.ID) {
			continue
		}
		if _, err := f.cache.FetchHeadshot(ctx, p.ID, p.URL); err != nil {
			if errors.Is(err, imagecache.ErrDisabled) {
				return
			}
			f.logger.Debug().
				Err(err).
				Int64("actor_id", p.ID).
				Str("url", p.URL).
				Msg("imagecache: headshot fetch failed")
		}
	}
}

// uniquePerformerIDs returns the sorted, deduped performer IDs from
// the cast slice. We send up to all of them in a single /api/images
// call — StageMedia silently drops IDs it doesn't have, so over-asking
// is fine and the dedup just trims the URL length.
func uniquePerformerIDs(cast []encora.CastEntry) []int64 {
	if len(cast) == 0 {
		return nil
	}
	seen := make(map[int64]bool, len(cast))
	out := make([]int64, 0, len(cast))
	for _, ce := range cast {
		id := ce.Performer.ID
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
