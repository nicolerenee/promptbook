// Package sync mirrors the user's Encora collection and wants list into
// the local SQLite cache.
//
// One entry point — Sync — paginates /collection and /wants to exhaustion,
// upserting through shows → recordings → cast_entries → collection|wants
// inside a transaction per page. Rate limit headers from each response
// drive a pause-or-bail policy so promptbook never lights up Encora's
// 30-req/min ceiling.
package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/ent/castentry"
	"github.com/nicolerenee/promptbook/internal/ent/collectionentry"
	"github.com/nicolerenee/promptbook/internal/ent/recording"
	"github.com/nicolerenee/promptbook/internal/ent/show"
	"github.com/nicolerenee/promptbook/internal/ent/wantsentry"
	"github.com/nicolerenee/promptbook/internal/stagemedia"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// SyncKind names a sync run for the sync_runs.kind column.
//
// SyncKindCollection and SyncKindWants are reserved for future kind-specific
// sync entry points (e.g. `promptbook collection sync --only-wants`); the
// current Sync runs both phases together and always logs SyncKindAll.
const (
	SyncKindAll        = "all"
	SyncKindCollection = "collection"
	SyncKindWants      = "wants"
)

// Retry-after policy applied when a paginated fetch returns
// encora.ErrRateLimited mid-batch. The retry happens at most once per call
// site; a second 429 propagates the error.
const (
	// MaxRetryAfter caps the sleep duration honored from a Retry-After
	// header — protects against pathological values that would stall the
	// sync for minutes.
	MaxRetryAfter = 90 * time.Second
	// DefaultRetryAfter is used when the 429 response does not carry a
	// usable Retry-After header. One full Encora rate-limit window is a
	// safe default.
	DefaultRetryAfter = 30 * time.Second
)

// Result summarizes one Sync invocation.
type Result struct {
	RunID                int64
	CollectionCount      int
	WantsCount           int
	Errors               int
	RateLimitRemaining   int
	RateLimitedBailedOut bool
}

// Client is the subset of *encora.Client that Sync needs. Defined as an
// interface so tests can pass a mock without standing up an httptest server.
type Client interface {
	Profile(ctx context.Context) (encora.Profile, encora.RateLimitInfo, error)
	Collection(
		ctx context.Context, page int,
	) (encora.Page[encora.CollectionEntry], encora.RateLimitInfo, error)
	CollectionURL(
		ctx context.Context, fullURL string,
	) (encora.Page[encora.CollectionEntry], encora.RateLimitInfo, error)
	Wants(
		ctx context.Context, page int,
	) (encora.Page[encora.WantEntry], encora.RateLimitInfo, error)
	WantsURL(
		ctx context.Context, fullURL string,
	) (encora.Page[encora.WantEntry], encora.RateLimitInfo, error)
}

// StagemediaImageClient is the slice of *stagemedia.Client per-entity
// image-refresh jobs need. Kept here (rather than promoted into a
// shared package) so consumers without a StageMedia key can pass nil
// without forcing an import of the concrete client. The real
// *stagemedia.Client satisfies this via its Images method.
//
// Sync itself no longer fetches images — that work moved to the
// per-entity refresh-show / refresh-recording / refresh-actor jobs.
// The interface is re-exposed here to keep the existing import paths
// stable for builtin/refresh_*.go.
type StagemediaImageClient interface {
	Images(ctx context.Context, showID int64, performerIDs []int64) (stagemedia.Images, error)
}

// EncoraScreenshotClient is the slice of *encora.Client used by the
// per-recording image refresh job to pull screen-grab URLs. The real
// *encora.Client satisfies this via its Screenshots method.
type EncoraScreenshotClient interface {
	Screenshots(ctx context.Context, id int64) ([]string, encora.RateLimitInfo, error)
}

// Options tunes Sync. Zero values are sane defaults.
//
// Image-cache fields are deliberately absent: under the v2 layout the
// sync writes only DB rows and the per-entity refresh-* jobs handle
// image downloads. RefreshEncoraJob enqueues those follow-ups after
// Sync returns.
type Options struct {
	BurstReserve      int
	PauseBetweenPages time.Duration
	Now               func() time.Time
	Sleep             func(time.Duration)
	Logger            zerolog.Logger
}

// Sync runs a full collection + wants sync into client. The sync_runs row
// is always written (even on failure) so the caller can correlate errors
// to the run.
func Sync(ctx context.Context, c Client, client *ent.Client, opts Options) (*Result, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Sleep == nil {
		opts.Sleep = time.Sleep
	}

	res := &Result{}
	startedAt := opts.Now().UTC()

	runID, runErr := insertSyncRun(ctx, client, SyncKindAll, startedAt)
	if runErr != nil {
		return res, fmt.Errorf("insert sync_run: %w", runErr)
	}
	res.RunID = runID

	// Profile is best-effort: a failure here logs a warning but does not
	// block collection/wants. The cached counts surfaced in the UI may be
	// stale until the next sync, but recordings remain importable.
	if profErr := syncProfile(ctx, c, client, opts, res); profErr != nil {
		opts.Logger.Warn().Err(profErr).Msg("profile sync failed; continuing")
	}

	syncErr := syncCollection(ctx, c, client, opts, res)
	if syncErr == nil && !res.RateLimitedBailedOut {
		syncErr = syncWants(ctx, c, client, opts, res)
	} else if res.RateLimitedBailedOut {
		opts.Logger.Warn().Int("remaining", res.RateLimitRemaining).
			Msg("bailing before /wants — rate-limit floor reached")
	}

	finished := opts.Now().UTC()
	errText := ""
	if syncErr != nil {
		errText = syncErr.Error()
	}
	if upErr := updateSyncRun(
		ctx, client, runID, finished,
		res.CollectionCount+res.WantsCount,
		res.Errors,
		res.RateLimitRemaining,
		errText,
	); upErr != nil {
		opts.Logger.Error().Err(upErr).Int64("run_id", runID).
			Msg("failed to update sync_run row")
	}
	return res, syncErr
}

// syncProfile fetches the authenticated user's profile and writes it to the
// single-row profile table. The caller treats the error as advisory — see
// the call site in Sync for the rationale.
func syncProfile(
	ctx context.Context,
	c Client,
	client *ent.Client,
	opts Options,
	res *Result,
) error {
	p, rl, err := c.Profile(ctx)
	if err != nil {
		return fmt.Errorf("fetch profile: %w", err)
	}
	res.RateLimitRemaining = rl.Remaining

	row := storage.Profile{
		EncoraID:          p.ID,
		Name:              p.Name,
		Slug:              p.Slug,
		Username:          p.Username,
		Status:            p.Status,
		RecordingsCount:   p.RecordingsCount,
		WantsCount:        p.WantsCount,
		LastSeenAt:        p.LastSeenAt,
		ProfileVisibility: p.ProfileVisibility,
		ColVisibility:     p.ColVisibility,
		LastSyncedAt:      opts.Now().UTC(),
	}
	if upErr := storage.UpsertProfile(ctx, client, row); upErr != nil {
		return fmt.Errorf("persist profile: %w", upErr)
	}
	return nil
}

// retryOnRateLimit runs op once. If op returns encora.ErrRateLimited, it
// sleeps for the Retry-After duration carried on the returned RateLimitInfo
// (capped at MaxRetryAfter, defaulting to DefaultRetryAfter when zero) and
// invokes op exactly one more time. Any other error or a successful call
// returns immediately. A cancelled context during the sleep returns
// ctx.Err() rather than continuing into the retry.
func retryOnRateLimit[T any](
	ctx context.Context,
	opts Options,
	op func() (T, encora.RateLimitInfo, error),
) (T, encora.RateLimitInfo, error) {
	val, rl, err := op()
	if !errors.Is(err, encora.ErrRateLimited) {
		return val, rl, err
	}

	wait := rl.RetryAfter
	if wait <= 0 {
		wait = DefaultRetryAfter
	}
	if wait > MaxRetryAfter {
		wait = MaxRetryAfter
	}

	opts.Logger.Warn().
		Dur("retry_after", wait).
		Int("remaining", rl.Remaining).
		Msg("encora 429: sleeping then retrying once")

	// Honor context cancellation by racing the sleep against ctx.Done().
	// opts.Sleep is the injected sleeper; tests record the duration even
	// when the context fires first by invoking the sleeper in a goroutine.
	done := make(chan struct{})
	go func() {
		opts.Sleep(wait)
		close(done)
	}()
	select {
	case <-ctx.Done():
		var zero T
		return zero, rl, ctx.Err()
	case <-done:
	}

	return op()
}

// syncCollection mirrors the collection endpoint. The structural overlap
// with syncWants is intentional — the two pagination loops carry different
// generic types and feed different writers, so a shared helper would have
// to round-trip through reflection or pull both writers under one type.
//
//nolint:dupl // see comment above; symmetry-by-design
func syncCollection(
	ctx context.Context,
	c Client,
	client *ent.Client,
	opts Options,
	res *Result,
) error {
	page, rl, err := retryOnRateLimit(ctx, opts,
		func() (encora.Page[encora.CollectionEntry], encora.RateLimitInfo, error) {
			return c.Collection(ctx, 1)
		},
	)
	if err != nil {
		return fmt.Errorf("fetch collection page 1: %w", err)
	}
	res.RateLimitRemaining = rl.Remaining

	for {
		writeErr := writeCollectionPage(ctx, client, page.Data, opts.Now)
		if writeErr != nil {
			return fmt.Errorf("write collection page %d: %w", page.CurrentPage, writeErr)
		}
		res.CollectionCount += len(page.Data)

		if rl.Remaining <= opts.BurstReserve {
			res.RateLimitedBailedOut = true
			return nil
		}
		if page.NextPageURL == nil {
			return nil
		}
		if opts.PauseBetweenPages > 0 {
			opts.Sleep(opts.PauseBetweenPages)
		}
		nextURL := *page.NextPageURL
		nextPage, nextRL, fetchErr := retryOnRateLimit(ctx, opts,
			func() (encora.Page[encora.CollectionEntry], encora.RateLimitInfo, error) {
				return c.CollectionURL(ctx, nextURL)
			},
		)
		if fetchErr != nil {
			return fmt.Errorf("fetch collection page %d: %w", page.CurrentPage+1, fetchErr)
		}
		page = nextPage
		rl = nextRL
		res.RateLimitRemaining = rl.Remaining
	}
}

//nolint:dupl // mirror of syncCollection; see that function's comment
func syncWants(
	ctx context.Context,
	c Client,
	client *ent.Client,
	opts Options,
	res *Result,
) error {
	page, rl, err := retryOnRateLimit(ctx, opts,
		func() (encora.Page[encora.WantEntry], encora.RateLimitInfo, error) {
			return c.Wants(ctx, 1)
		},
	)
	if err != nil {
		return fmt.Errorf("fetch wants page 1: %w", err)
	}
	res.RateLimitRemaining = rl.Remaining

	for {
		writeErr := writeWantsPage(ctx, client, page.Data, opts.Now)
		if writeErr != nil {
			return fmt.Errorf("write wants page %d: %w", page.CurrentPage, writeErr)
		}
		res.WantsCount += len(page.Data)

		if rl.Remaining <= opts.BurstReserve {
			res.RateLimitedBailedOut = true
			return nil
		}
		if page.NextPageURL == nil {
			return nil
		}
		if opts.PauseBetweenPages > 0 {
			opts.Sleep(opts.PauseBetweenPages)
		}
		nextURL := *page.NextPageURL
		nextPage, nextRL, fetchErr := retryOnRateLimit(ctx, opts,
			func() (encora.Page[encora.WantEntry], encora.RateLimitInfo, error) {
				return c.WantsURL(ctx, nextURL)
			},
		)
		if fetchErr != nil {
			return fmt.Errorf("fetch wants page %d: %w", page.CurrentPage+1, fetchErr)
		}
		page = nextPage
		rl = nextRL
		res.RateLimitRemaining = rl.Remaining
	}
}

func writeCollectionPage(
	ctx context.Context,
	client *ent.Client,
	entries []encora.CollectionEntry,
	now func() time.Time,
) error {
	tx, err := client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, entry := range entries {
		if rerr := upsertRecording(ctx, tx, entry.Recording, now); rerr != nil {
			return fmt.Errorf("upsert recording %d: %w", entry.Recording.ID, rerr)
		}
		if cerr := upsertCollection(ctx, tx, entry, now); cerr != nil {
			return fmt.Errorf("upsert collection %d: %w", entry.Recording.ID, cerr)
		}
	}
	if cerr := tx.Commit(); cerr != nil {
		return fmt.Errorf("commit tx: %w", cerr)
	}
	return nil
}

func writeWantsPage(
	ctx context.Context,
	client *ent.Client,
	entries []encora.WantEntry,
	now func() time.Time,
) error {
	tx, err := client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, entry := range entries {
		if rerr := upsertRecording(ctx, tx, entry.Recording, now); rerr != nil {
			return fmt.Errorf("upsert recording %d: %w", entry.Recording.ID, rerr)
		}
		if werr := upsertWant(ctx, tx, entry.Recording.ID, now); werr != nil {
			return fmt.Errorf("upsert want %d: %w", entry.Recording.ID, werr)
		}
	}
	if cerr := tx.Commit(); cerr != nil {
		return fmt.Errorf("commit tx: %w", cerr)
	}
	return nil
}

// upsertRecording writes the recording, its show, and refreshes its cast.
// Cast entries are deleted+reinserted so renames in the upstream Encora
// data don't leave stale rows.
func upsertRecording(
	ctx context.Context,
	tx *ent.Tx,
	r encora.Recording,
	now func() time.Time,
) error {
	rawJSON, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal raw json: %w", err)
	}
	nowTS := now().UTC()

	if serr := upsertShow(ctx, tx, r, nowTS); serr != nil {
		return serr
	}
	if rerr := upsertRecordingRow(ctx, tx, r, string(rawJSON), nowTS); rerr != nil {
		return rerr
	}
	return refreshCastEntries(ctx, tx, r, now)
}

func upsertShow(ctx context.Context, tx *ent.Tx, r encora.Recording, ts time.Time) error {
	err := tx.Show.Create().
		SetID(r.Metadata.ShowID).
		SetName(r.Show).
		SetDescriptionHTML(r.Metadata.ShowDescription).
		SetLastSeenAt(ts).
		OnConflictColumns(show.FieldID).
		Update(func(u *ent.ShowUpsert) {
			u.UpdateName()
			u.UpdateDescriptionHTML()
			u.UpdateLastSeenAt()
		}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("upsert show: %w", err)
	}
	return nil
}

func upsertRecordingRow(
	ctx context.Context,
	tx *ent.Tx,
	r encora.Recording,
	rawJSON string,
	ts time.Time,
) error {
	create := newRecordingCreate(tx, r, rawJSON, ts)
	err := create.
		OnConflictColumns(recording.FieldID).
		Update(recordingUpsertFn(r)).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("upsert recording row: %w", err)
	}
	return nil
}

// newRecordingCreate constructs the per-recording Create builder used
// by both the bare insert path and the upsert path. Pulled out of
// upsertRecordingRow so the funlen lint stays under cap with the
// fully-typed ent SetX() chain.
func newRecordingCreate(
	tx *ent.Tx, r encora.Recording, rawJSON string, ts time.Time,
) *ent.RecordingCreate {
	create := tx.Recording.Create().
		SetID(r.ID).
		SetShowID(r.Metadata.ShowID).
		SetTour(r.Tour).
		SetDateFull(r.Date.FullDate).
		SetDateMonthKnown(r.Date.MonthKnown).
		SetDateDayKnown(r.Date.DayKnown).
		SetDateTime(r.Date.Time).
		SetMaster(r.Master).
		SetNftForever(r.NFT.NFTForever).
		SetNotes(r.Notes).
		SetVenue(r.Metadata.Venue).
		SetCity(r.Metadata.City).
		SetMediaType(r.Metadata.MediaType).
		SetRecordingType(r.Metadata.RecordingType).
		SetAmountRecorded(r.Metadata.AmountRecorded).
		SetGiftingStatus(r.Metadata.GiftingStatus).
		SetLimitedStatus(r.Metadata.LimitedStatus).
		SetIsOpening(r.Metadata.IsOpening).
		SetIsClosing(r.Metadata.IsClosing).
		SetIsPreview(r.Metadata.IsPreview).
		SetIsConcert(r.Metadata.IsConcert).
		SetIsNfs(r.Metadata.IsNFS).
		SetIsFavourite(r.Metadata.IsFavourite).
		SetHasScreenshots(r.Metadata.HasScreenshots).
		SetHasSubtitles(r.Metadata.HasSubtitles).
		SetBootCampRecommended(r.Metadata.BootCampRecommended).
		SetOwnersCount(r.Metadata.OwnersCount).
		SetWantersCount(r.Metadata.WantersCount).
		SetLastUpdated(r.Metadata.LastUpdated).
		SetRawJSON(rawJSON).
		SetLastSeenAt(ts)
	if r.Date.DateVariant != nil {
		create = create.SetDateVariant(*r.Date.DateVariant)
	}
	if r.NFT.NFTDate != nil {
		create = create.SetNftDate(*r.NFT.NFTDate)
	}
	if r.MasterNotes != "" {
		create = create.SetMasterNotes(r.MasterNotes)
	}
	if r.ReleaseFormat != nil {
		create = create.SetReleaseFormat(*r.ReleaseFormat)
	}
	return create
}

// recordingUpsertFn returns the closure that the OnConflict branch
// runs to refresh every mutable column on an existing recording. The
// nullable columns (date_variant, nft_date, master_notes,
// release_format) are explicit Set/Clear so a recording that loses a
// previously-populated value gets the column nulled rather than
// velvet-antlers.
func recordingUpsertFn(r encora.Recording) func(u *ent.RecordingUpsert) {
	return func(u *ent.RecordingUpsert) {
		u.UpdateShowID()
		u.UpdateTour()
		u.UpdateDateFull()
		u.UpdateDateMonthKnown()
		u.UpdateDateDayKnown()
		if r.Date.DateVariant != nil {
			u.SetDateVariant(*r.Date.DateVariant)
		} else {
			u.ClearDateVariant()
		}
		u.UpdateDateTime()
		u.UpdateMaster()
		if r.NFT.NFTDate != nil {
			u.SetNftDate(*r.NFT.NFTDate)
		} else {
			u.ClearNftDate()
		}
		u.UpdateNftForever()
		u.UpdateNotes()
		if r.MasterNotes != "" {
			u.SetMasterNotes(r.MasterNotes)
		} else {
			u.ClearMasterNotes()
		}
		if r.ReleaseFormat != nil {
			u.SetReleaseFormat(*r.ReleaseFormat)
		} else {
			u.ClearReleaseFormat()
		}
		u.UpdateVenue()
		u.UpdateCity()
		u.UpdateMediaType()
		u.UpdateRecordingType()
		u.UpdateAmountRecorded()
		u.UpdateGiftingStatus()
		u.UpdateLimitedStatus()
		u.UpdateIsOpening()
		u.UpdateIsClosing()
		u.UpdateIsPreview()
		u.UpdateIsConcert()
		u.UpdateIsNfs()
		u.UpdateIsFavourite()
		u.UpdateHasScreenshots()
		u.UpdateHasSubtitles()
		u.UpdateBootCampRecommended()
		u.UpdateOwnersCount()
		u.UpdateWantersCount()
		u.UpdateLastUpdated()
		u.UpdateRawJSON()
		u.UpdateLastSeenAt()
	}
}

// refreshCastEntries wipes and rewrites cast_entries for the recording while
// keeping the first-class performers and characters tables in sync. The legacy
// denormalized columns on cast_entries are still populated (downstream
// consumers haven't migrated off them yet); the upserts into performers and
// characters layer the canonical people rows on top so foreign-key style
// joins (e.g. ListRecordingsForPerformer + LoadPerformer) work post-sync.
func refreshCastEntries(
	ctx context.Context,
	tx *ent.Tx,
	r encora.Recording,
	now func() time.Time,
) error {
	if _, err := tx.CastEntry.Delete().
		Where(castentry.RecordingID(r.ID)).
		Exec(ctx); err != nil {
		return fmt.Errorf("clear cast_entries: %w", err)
	}
	nowTS := now().UTC()
	for _, cast := range r.Cast {
		if perr := storage.UpsertPerformerTx(ctx, tx, storage.Performer{
			PerformerID: cast.Performer.ID,
			Name:        cast.Performer.Name,
			Slug:        cast.Performer.Slug,
			URL:         cast.Performer.URL,
			LastSeenAt:  nowTS,
		}); perr != nil {
			return fmt.Errorf("upsert performer for cast_entry: %w", perr)
		}
		if cerr := storage.UpsertCharacterTx(ctx, tx, storage.Character{
			CharacterID: cast.Character.ID,
			Name:        cast.Character.Name,
			Slug:        cast.Character.Slug,
			URL:         cast.Character.URL,
			LastSeenAt:  nowTS,
		}); cerr != nil {
			return fmt.Errorf("upsert character for cast_entry: %w", cerr)
		}

		create := tx.CastEntry.Create().
			SetRecordingID(r.ID).
			SetPerformerID(cast.Performer.ID).
			SetPerformerName(cast.Performer.Name).
			SetPerformerSlug(cast.Performer.Slug).
			SetPerformerURL(cast.Performer.URL).
			SetCharacterID(cast.Character.ID).
			SetCharacterName(cast.Character.Name).
			SetCharacterSlug(cast.Character.Slug).
			SetCharacterURL(cast.Character.URL).
			SetCharacterOrder(cast.Character.Order)
		if cast.Status != nil {
			create = create.
				SetStatusLabel(cast.Status.Label).
				SetStatusAbbrev(cast.Status.Abbreviation)
		}
		if _, err := create.Save(ctx); err != nil {
			return fmt.Errorf("insert cast_entry: %w", err)
		}
	}
	return nil
}

func upsertCollection(
	ctx context.Context,
	tx *ent.Tx,
	entry encora.CollectionEntry,
	now func() time.Time,
) error {
	nowTS := now().UTC()
	collectedAt, hasCollected := parseCollectionTimestamp(entry.CollectedAt)
	updatedAt, hasUpdated := parseCollectionTimestamp(entry.UpdatedAt)

	create := tx.CollectionEntry.Create().
		SetID(entry.Recording.ID).
		SetFormat(entry.Format).
		SetUserWatched(entry.UserWatched != 0).
		SetLastSyncedAt(nowTS)
	if entry.Notes != nil {
		create = create.SetUserNotes(*entry.Notes)
	}
	if hasCollected {
		create = create.SetCollectedAt(collectedAt)
	}
	if hasUpdated {
		create = create.SetUpdatedAt(updatedAt)
	}
	err := create.
		OnConflictColumns(collectionentry.FieldID).
		Update(func(u *ent.CollectionEntryUpsert) {
			u.UpdateFormat()
			if entry.Notes != nil {
				u.SetUserNotes(*entry.Notes)
			} else {
				u.ClearUserNotes()
			}
			u.UpdateUserWatched()
			if hasCollected {
				u.SetCollectedAt(collectedAt)
			} else {
				u.ClearCollectedAt()
			}
			if hasUpdated {
				u.SetUpdatedAt(updatedAt)
			} else {
				u.ClearUpdatedAt()
			}
			u.SetLastSyncedAt(nowTS)
		}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("upsert collection row: %w", err)
	}
	return nil
}

// parseCollectionTimestamp parses Encora's RFC3339-ish collection
// timestamp (e.g. "2024-10-19T12:10:29.000000Z"). Returns the parsed
// value + true on success; an empty input or parse failure returns
// the zero time + false so the caller can treat it as "absent".
//
// Supported formats: RFC3339 / RFC3339Nano (the only shapes observed
// in production fixtures). Any other input is silently treated as
// missing — Encora's audit trail is best-effort and we'd rather drop
// a malformed timestamp than fail the entire sync page.
func parseCollectionTimestamp(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

func upsertWant(
	ctx context.Context,
	tx *ent.Tx,
	recordingID int64,
	now func() time.Time,
) error {
	nowTS := now().UTC()
	err := tx.WantsEntry.Create().
		SetID(recordingID).
		SetLastSyncedAt(nowTS).
		OnConflictColumns(wantsentry.FieldID).
		Update(func(u *ent.WantsEntryUpsert) {
			u.SetLastSyncedAt(nowTS)
		}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("upsert wants row: %w", err)
	}
	return nil
}

func insertSyncRun(
	ctx context.Context,
	client *ent.Client,
	kind string,
	startedAt time.Time,
) (int64, error) {
	row, err := client.SyncRun.Create().
		SetKind(kind).
		SetStartedAt(startedAt).
		Save(ctx)
	if err != nil {
		return 0, err
	}
	return int64(row.ID), nil
}

func updateSyncRun(
	ctx context.Context,
	client *ent.Client,
	runID int64,
	finishedAt time.Time,
	okCount, errorCount, rateLimitRemaining int,
	errorText string,
) error {
	_, err := client.SyncRun.UpdateOneID(int(runID)).
		SetFinishedAt(finishedAt).
		SetOkCount(okCount).
		SetErrorCount(errorCount).
		SetRateLimitRemaining(rateLimitRemaining).
		SetErrorText(errorText).
		Save(ctx)
	if err != nil {
		return err
	}
	return nil
}
