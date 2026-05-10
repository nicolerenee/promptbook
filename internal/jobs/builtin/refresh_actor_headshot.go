package builtin

import (
	"context"
	"errors"
	"fmt"

	"github.com/rs/zerolog"

	"github.com/nicolerenee/promptbook/internal/ent/castentry"
	"github.com/nicolerenee/promptbook/internal/ent/recording"
	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/jobs"
	pbsync "github.com/nicolerenee/promptbook/internal/sync"
)

// RefreshActorHeadshotJob fetches a single actor's headshot from
// StageMedia. Useful when a show fan-out missed an actor (e.g. an
// orphan performer not credited on any cached show). With force=true,
// the existing headshot is overwritten; without force, the job is a
// no-op when one's already on disk.
//
// StageMedia's /api/images requires a show id and an actor list; we
// look up one of the actor's recorded shows and use that as the show
// id. When the actor isn't credited anywhere we'd need to call
// /api/images, the job logs a debug-level skip and returns nil.
type RefreshActorHeadshotJob struct {
	DB     *DBConn
	Cache  *imagecache.Cache
	SM     pbsync.StagemediaImageClient
	Logger zerolog.Logger
}

// jobNameRefreshActorHeadshot is the registry key.
const jobNameRefreshActorHeadshot = "refresh-actor-headshot"

// Name returns the registry key.
func (j *RefreshActorHeadshotJob) Name() string { return jobNameRefreshActorHeadshot }

// Run fetches an actor's headshot. Args:
//
//	actor_id: int64 (required, must be > 0)
//	force:    bool  (optional; overwrite an existing headshot)
func (j *RefreshActorHeadshotJob) Run(ctx context.Context, args jobs.JobArgs) error {
	actorID := args.GetInt64("actor_id")
	if actorID <= 0 {
		return errors.New("refresh-actor-headshot: missing or invalid actor_id arg")
	}
	force := args.GetBool("force")

	if err := j.checkPrereqs(); err != nil {
		return err
	}

	if !force && j.Cache.HasHeadshot(actorID) {
		j.Logger.Debug().
			Int64("actor_id", actorID).
			Msg("refresh-actor-headshot: already on disk; skipping")
		return nil
	}

	showID, err := lookupShowForActor(ctx, j.DB, actorID)
	if err != nil {
		return fmt.Errorf("refresh-actor-headshot: lookup show for actor %d: %w", actorID, err)
	}
	if showID == 0 {
		j.Logger.Debug().
			Int64("actor_id", actorID).
			Msg("refresh-actor-headshot: no recorded show for actor; skipping")
		return nil
	}

	return j.fetchAndPersist(ctx, actorID, showID, force)
}

// checkPrereqs verifies the job has the dependencies it needs to make
// progress. Pulled out so Run reads cleanly under the gocognit cap.
func (j *RefreshActorHeadshotJob) checkPrereqs() error {
	if j.SM == nil {
		return errors.New("refresh-actor-headshot: stagemedia client not configured")
	}
	if j.Cache == nil || j.Cache.Disabled() {
		return errors.New("refresh-actor-headshot: image cache not configured")
	}
	return nil
}

// fetchAndPersist handles the StageMedia call + slot write once
// prereq + lookup checks have passed.
func (j *RefreshActorHeadshotJob) fetchAndPersist(
	ctx context.Context, actorID, showID int64, force bool,
) error {
	imgs, err := j.SM.Images(ctx, showID, []int64{actorID})
	if err != nil {
		return fmt.Errorf("refresh-actor-headshot: stagemedia fetch: %w", err)
	}
	for _, p := range imgs.Performers {
		if p.ID != actorID || p.URL == "" {
			continue
		}
		return j.writeHeadshot(ctx, actorID, p.URL, force)
	}

	// StageMedia silently drops actor ids it has no data for. Not an
	// error — log debug so the operator can spot it if they're chasing
	// a missing headshot.
	j.Logger.Debug().
		Int64("actor_id", actorID).
		Int64("show_id", showID).
		Msg("refresh-actor-headshot: stagemedia returned no headshot")
	return nil
}

// writeHeadshot lands the upstream URL in the headshot slot, clearing
// an existing file first when force=true.
func (j *RefreshActorHeadshotJob) writeHeadshot(
	ctx context.Context, actorID int64, url string, force bool,
) error {
	if force {
		if path := j.Cache.HeadshotPath(actorID); path != "" {
			_ = removeIfExists(path)
		}
	}
	dest, err := j.Cache.FetchHeadshot(ctx, actorID, url)
	if err != nil {
		if errors.Is(err, imagecache.ErrDisabled) {
			return err
		}
		return fmt.Errorf("refresh-actor-headshot: fetch: %w", err)
	}
	j.Logger.Info().
		Int64("actor_id", actorID).
		Bool("force", force).
		Str("dest", dest).
		Msg("refresh-actor-headshot: headshot written")
	return nil
}

// lookupShowForActor returns one show id the actor is credited on, or
// 0 when the actor has no recorded credits in the local catalog.
// Picks the smallest show id deterministically so retries hit the
// same upstream cache key. Errors propagate.
func lookupShowForActor(ctx context.Context, db *DBConn, actorID int64) (int64, error) {
	if db == nil {
		return 0, errors.New("db is nil")
	}
	// Cast entries credited to the actor → recording ids → minimum
	// non-zero show id. The legacy SQL composed this in one MIN()
	// query; the ent variant pulls cast entries scoped to the actor
	// and the matching recordings, then picks the min show id in Go.
	recIDs, err := db.CastEntry.Query().
		Where(castentry.PerformerID(actorID)).
		Select(castentry.FieldRecordingID).
		Strings(ctx)
	if err != nil || len(recIDs) == 0 {
		if err != nil {
			return 0, fmt.Errorf("query recording ids for actor %d: %w", actorID, err)
		}
		return 0, nil
	}
	ids := make([]int64, 0, len(recIDs))
	for _, s := range recIDs {
		var id int64
		if _, ferr := fmt.Sscanf(s, "%d", &id); ferr == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	recs, err := db.Recording.Query().
		Where(recording.IDIn(ids...)).
		All(ctx)
	if err != nil {
		return 0, fmt.Errorf("query show ids for actor %d: %w", actorID, err)
	}
	var minShow int64
	for _, r := range recs {
		if r.ShowID <= 0 {
			continue
		}
		if minShow == 0 || r.ShowID < minShow {
			minShow = r.ShowID
		}
	}
	return minShow, nil
}
