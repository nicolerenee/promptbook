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
// posters + headshots from StageMedia (and screen-grab backdrops from
// Encora) into the local image cache. It owns dedup sets scoped to the
// run so the sync loop only calls each upstream once per unique key —
// without this the worst case is one round-trip per recording, which
// would burn the upstream rate budgets on a fresh sync.
//
// Fetch failures never bubble up. Image caching is best-effort; an
// unreachable CDN must not abort an Encora sync.
type imageFetcher struct {
	cache   *imagecache.Cache
	client  StagemediaImageClient
	encora  EncoraScreenshotClient
	logger  zerolog.Logger
	visitMu sync.Mutex
	// visitedShow dedups StageMedia /api/images calls per run.
	visitedShow map[int64]bool
	// visitedRecording dedups Encora /screenshots calls per run.
	// Backdrops are recording-keyed so this set is independent of the
	// show-keyed posters dedup above.
	visitedRecording map[int64]bool
	// screenshotRecordings counts recordings for which a /screenshots
	// fetch was issued (a successful or failed call both count) so the
	// post-sync summary log can report progress visibility.
	screenshotRecordings int
}

// newImageFetcher returns a no-op fetcher (Disabled() == true) when
// either the cache is nil/disabled or the StageMedia client is nil.
// Callers can invoke forRecording on the returned value unconditionally.
func newImageFetcher(
	cache *imagecache.Cache,
	client StagemediaImageClient,
	enc EncoraScreenshotClient,
	logger zerolog.Logger,
) *imageFetcher {
	return &imageFetcher{
		cache:            cache,
		client:           client,
		encora:           enc,
		logger:           logger,
		visitedShow:      map[int64]bool{},
		visitedRecording: map[int64]bool{},
	}
}

// disabled reports whether the StageMedia poster/headshot path is in
// no-op mode. Backdrops are gated independently — see backdropDisabled.
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

// backdropDisabled reports whether the Encora screen-grab path is in
// no-op mode. Independent of disabled() so an unconfigured StageMedia
// key doesn't disable backdrops, and vice versa.
func (f *imageFetcher) backdropDisabled() bool {
	if f == nil {
		return true
	}
	if f.cache == nil || f.cache.Disabled() {
		return true
	}
	if f.encora == nil {
		return true
	}
	return false
}

// forRecording fetches posters for the recording's show + headshots
// for every credited performer + screen-grab backdrops for the
// recording itself, skipping anything already on disk and deduping
// keys across the whole sync run. Errors are logged and swallowed —
// image caching is never load-bearing for the sync.
func (f *imageFetcher) forRecording(ctx context.Context, r encora.Recording) {
	f.fetchPostersAndHeadshots(ctx, r)
	f.fetchBackdrops(ctx, r)
}

// fetchPostersAndHeadshots is the StageMedia half of forRecording.
// Split out so the backdrop path can run independently when StageMedia
// isn't configured.
func (f *imageFetcher) fetchPostersAndHeadshots(ctx context.Context, r encora.Recording) {
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
	f.visitMu.Lock()
	if f.visitedShow[showID] {
		f.visitMu.Unlock()
		return
	}
	f.visitedShow[showID] = true
	f.visitMu.Unlock()

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

// fetchBackdrops is the Encora-screenshots half of forRecording. Gated
// on RecordingMetadata.HasScreenshots so we don't spend rate-limit
// budget on calls guaranteed to return [].
func (f *imageFetcher) fetchBackdrops(ctx context.Context, r encora.Recording) {
	if f.backdropDisabled() {
		return
	}
	if !r.Metadata.HasScreenshots || r.ID == 0 {
		return
	}

	// Per-recording dedup: a recording can show up in both /collection
	// and /wants pagination, and we'd fetch screenshots once per page.
	// One call per recording per run is enough.
	f.visitMu.Lock()
	if f.visitedRecording[r.ID] {
		f.visitMu.Unlock()
		return
	}
	f.visitedRecording[r.ID] = true
	f.screenshotRecordings++
	f.visitMu.Unlock()

	urls, _, err := f.encora.Screenshots(ctx, r.ID)
	if err != nil {
		f.logger.Debug().
			Err(err).
			Int64("recording_id", r.ID).
			Msg("imagecache: encora screenshots fetch failed")
		return
	}
	f.persistBackdrops(ctx, r.ID, urls)
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

// persistBackdrops writes each screen-grab URL to the cache under the
// recording's backdrop slot. Already-cached slots short-circuit inside
// FetchBackdrop without a network call.
func (f *imageFetcher) persistBackdrops(ctx context.Context, recordingID int64, urls []string) {
	for i, url := range urls {
		if url == "" {
			continue
		}
		if f.cache.HasBackdrop(recordingID, i) {
			continue
		}
		if _, err := f.cache.FetchBackdrop(ctx, recordingID, i, url); err != nil {
			if errors.Is(err, imagecache.ErrDisabled) {
				return
			}
			f.logger.Debug().
				Err(err).
				Int64("recording_id", recordingID).
				Int("index", i).
				Str("url", url).
				Msg("imagecache: backdrop fetch failed")
		}
	}
}

// logScreenshotSummary emits a single info-level line counting how
// many recordings had a screenshots fetch issued during this sync.
// Always logged (zero included) so the operator can distinguish "no
// recordings eligible" from "feature wired but silent".
func (f *imageFetcher) logScreenshotSummary() {
	if f == nil {
		return
	}
	if f.backdropDisabled() {
		// Don't spam an info line every sync when the feature is
		// off. Debug is enough to confirm the path was reached.
		f.logger.Debug().Msg("imagecache: backdrop fetch disabled")
		return
	}
	f.visitMu.Lock()
	count := f.screenshotRecordings
	f.visitMu.Unlock()
	f.logger.Info().
		Int("recordings", count).
		Msg("imagecache: encora screenshots fetch complete")
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
