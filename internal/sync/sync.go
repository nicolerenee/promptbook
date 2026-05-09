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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/imagecache"
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

// StagemediaImageClient is the slice of *stagemedia.Client the sync
// loop needs to opportunistically populate the image cache. Defined as
// an interface so tests can stub it without standing up an httptest
// server, and so callers without a stagemedia API key can pass nil
// without forcing the sync to import the concrete client. The real
// *stagemedia.Client satisfies this via its Images method.
type StagemediaImageClient interface {
	Images(ctx context.Context, showID int64, performerIDs []int64) (stagemedia.Images, error)
}

// EncoraScreenshotClient is the slice of *encora.Client the sync loop
// needs to fetch per-recording screen-grab URLs into the local backdrop
// cache. Surfaced as a separate interface (rather than rolled into the
// existing Client interface above) so tests can opt in to backdrop
// fetching without overriding the broader collection/wants surface, and
// so a nil pointer disables backdrop caching cleanly.
//
// The real *encora.Client satisfies this via its Screenshots method.
type EncoraScreenshotClient interface {
	Screenshots(ctx context.Context, id int64) ([]string, encora.RateLimitInfo, error)
}

// Options tunes Sync. Zero values are sane defaults.
type Options struct {
	BurstReserve      int
	PauseBetweenPages time.Duration
	Now               func() time.Time
	Sleep             func(time.Duration)
	Logger            zerolog.Logger
	// ImageCache is optional. When nil or Disabled(), the sync loop
	// skips the StageMedia poster + headshot fetch entirely. When
	// configured, the loop dedups show IDs across the run to avoid
	// hammering /api/images for the same show twice.
	ImageCache *imagecache.Cache
	// Stagemedia is optional and only used in tandem with ImageCache.
	// When nil, no opportunistic image fetch happens regardless of
	// the ImageCache field. The real *stagemedia.Client satisfies
	// this; a nil interface is the disabled case.
	Stagemedia StagemediaImageClient
	// EncoraScreenshots is optional and only used in tandem with
	// ImageCache. When non-nil and the recording's metadata has
	// has_screenshots == true, the fetcher calls /recording/{id}/
	// screenshots and writes each returned URL into the backdrop
	// slot. nil disables backdrop caching independently of poster
	// caching. The real *encora.Client satisfies this.
	EncoraScreenshots EncoraScreenshotClient
}

// Sync runs a full collection + wants sync into db. The sync_runs row is
// always written (even on failure) so the caller can correlate errors to
// the run.
func Sync(ctx context.Context, c Client, db *sql.DB, opts Options) (*Result, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Sleep == nil {
		opts.Sleep = time.Sleep
	}

	res := &Result{}
	startedAt := opts.Now().UTC()

	runID, runErr := insertSyncRun(ctx, db, SyncKindAll, startedAt)
	if runErr != nil {
		return res, fmt.Errorf("insert sync_run: %w", runErr)
	}
	res.RunID = runID

	// Profile is best-effort: a failure here logs a warning but does not
	// block collection/wants. The cached counts surfaced in the UI may be
	// stale until the next sync, but recordings remain importable.
	if profErr := syncProfile(ctx, c, db, opts, res); profErr != nil {
		opts.Logger.Warn().Err(profErr).Msg("profile sync failed; continuing")
	}

	// imageFetcher lifecycle is per-Sync so the show-id dedup map
	// resets between runs. The fetcher is a no-op when ImageCache or
	// Stagemedia is missing — callers can leave both nil to keep
	// classic sync semantics untouched. The encora screenshot client
	// is independent: if it's set the fetcher will call /screenshots
	// for every recording with has_screenshots=true, otherwise
	// backdrop caching is skipped.
	images := newImageFetcher(opts.ImageCache, opts.Stagemedia, opts.EncoraScreenshots, opts.Logger)

	syncErr := syncCollection(ctx, c, db, opts, res, images)
	if syncErr == nil && !res.RateLimitedBailedOut {
		syncErr = syncWants(ctx, c, db, opts, res, images)
	} else if res.RateLimitedBailedOut {
		opts.Logger.Warn().Int("remaining", res.RateLimitRemaining).
			Msg("bailing before /wants — rate-limit floor reached")
	}

	// Surface the screenshot fetch tally before the sync_runs row
	// closes so operators can see how many recordings the run
	// touched in the backdrop cache. Zero is logged too — silence
	// would be ambiguous between "nothing eligible" and "feature
	// disabled".
	images.logScreenshotSummary()

	finished := opts.Now().UTC()
	errText := ""
	if syncErr != nil {
		errText = syncErr.Error()
	}
	if upErr := updateSyncRun(
		ctx, db, runID, finished,
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
	db *sql.DB,
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
	if upErr := storage.UpsertProfile(ctx, db, row); upErr != nil {
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
	db *sql.DB,
	opts Options,
	res *Result,
	images *imageFetcher,
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
		writeErr := writeCollectionPage(ctx, db, page.Data, opts.Now)
		if writeErr != nil {
			return fmt.Errorf("write collection page %d: %w", page.CurrentPage, writeErr)
		}
		res.CollectionCount += len(page.Data)
		// Image cache is updated post-commit so a network hiccup can
		// never roll back a recording write. The fetcher is a no-op
		// when caching is disabled.
		for _, entry := range page.Data {
			images.forRecording(ctx, entry.Recording)
		}

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
	db *sql.DB,
	opts Options,
	res *Result,
	images *imageFetcher,
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
		writeErr := writeWantsPage(ctx, db, page.Data, opts.Now)
		if writeErr != nil {
			return fmt.Errorf("write wants page %d: %w", page.CurrentPage, writeErr)
		}
		res.WantsCount += len(page.Data)
		// Wants entries also get their images opportunistically
		// cached — show posters are useful in the wants list and
		// dedup keeps the StageMedia call count bounded.
		for _, entry := range page.Data {
			images.forRecording(ctx, entry.Recording)
		}

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
	db *sql.DB,
	entries []encora.CollectionEntry,
	now func() time.Time,
) error {
	tx, err := db.BeginTx(ctx, nil)
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
	db *sql.DB,
	entries []encora.WantEntry,
	now func() time.Time,
) error {
	tx, err := db.BeginTx(ctx, nil)
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
	tx *sql.Tx,
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
	if rerr := upsertRecordingRow(ctx, tx, r, rawJSON, nowTS); rerr != nil {
		return rerr
	}
	return refreshCastEntries(ctx, tx, r, now)
}

func upsertShow(ctx context.Context, tx *sql.Tx, r encora.Recording, ts time.Time) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO shows (show_id, name, description_html, last_seen_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(show_id) DO UPDATE SET
			name             = excluded.name,
			description_html = excluded.description_html,
			last_seen_at     = excluded.last_seen_at
	`, r.Metadata.ShowID, r.Show, r.Metadata.ShowDescription, ts)
	if err != nil {
		return fmt.Errorf("upsert show: %w", err)
	}
	return nil
}

func upsertRecordingRow(
	ctx context.Context,
	tx *sql.Tx,
	r encora.Recording,
	rawJSON []byte,
	ts time.Time,
) error {
	const stmt = `
		INSERT INTO recordings (
			recording_id, show_id, tour,
			date_full, date_month_known, date_day_known, date_variant, date_time,
			master, nft_date, nft_forever, notes, master_notes, release_format,
			venue, city, media_type, recording_type, amount_recorded,
			gifting_status, limited_status,
			is_opening, is_closing, is_preview, is_concert, is_nfs, is_favourite,
			has_screenshots, has_subtitles, boot_camp_recommended,
			owners_count, wanters_count, last_updated, raw_json, last_seen_at
		) VALUES (
			?, ?, ?,
			?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?,
			?, ?,
			?, ?, ?, ?, ?, ?,
			?, ?, ?,
			?, ?, ?, ?, ?
		)
		ON CONFLICT(recording_id) DO UPDATE SET
			show_id               = excluded.show_id,
			tour                  = excluded.tour,
			date_full             = excluded.date_full,
			date_month_known      = excluded.date_month_known,
			date_day_known        = excluded.date_day_known,
			date_variant          = excluded.date_variant,
			date_time             = excluded.date_time,
			master                = excluded.master,
			nft_date              = excluded.nft_date,
			nft_forever           = excluded.nft_forever,
			notes                 = excluded.notes,
			master_notes          = excluded.master_notes,
			release_format        = excluded.release_format,
			venue                 = excluded.venue,
			city                  = excluded.city,
			media_type            = excluded.media_type,
			recording_type        = excluded.recording_type,
			amount_recorded       = excluded.amount_recorded,
			gifting_status        = excluded.gifting_status,
			limited_status        = excluded.limited_status,
			is_opening            = excluded.is_opening,
			is_closing            = excluded.is_closing,
			is_preview            = excluded.is_preview,
			is_concert            = excluded.is_concert,
			is_nfs                = excluded.is_nfs,
			is_favourite          = excluded.is_favourite,
			has_screenshots       = excluded.has_screenshots,
			has_subtitles         = excluded.has_subtitles,
			boot_camp_recommended = excluded.boot_camp_recommended,
			owners_count          = excluded.owners_count,
			wanters_count         = excluded.wanters_count,
			last_updated          = excluded.last_updated,
			raw_json              = excluded.raw_json,
			last_seen_at          = excluded.last_seen_at
	`
	_, err := tx.ExecContext(ctx, stmt,
		r.ID, r.Metadata.ShowID, r.Tour,
		r.Date.FullDate, boolToInt(r.Date.MonthKnown), boolToInt(r.Date.DayKnown),
		r.Date.DateVariant, r.Date.Time,
		r.Master, r.NFT.NFTDate, boolToInt(r.NFT.NFTForever), r.Notes,
		nullableString(r.MasterNotes), r.ReleaseFormat,
		r.Metadata.Venue, r.Metadata.City, r.Metadata.MediaType,
		r.Metadata.RecordingType, r.Metadata.AmountRecorded,
		r.Metadata.GiftingStatus, r.Metadata.LimitedStatus,
		boolToInt(r.Metadata.IsOpening), boolToInt(r.Metadata.IsClosing),
		boolToInt(r.Metadata.IsPreview), boolToInt(r.Metadata.IsConcert),
		boolToInt(r.Metadata.IsNFS), boolToInt(r.Metadata.IsFavourite),
		boolToInt(r.Metadata.HasScreenshots), boolToInt(r.Metadata.HasSubtitles),
		boolToInt(r.Metadata.BootCampRecommended),
		r.Metadata.OwnersCount, r.Metadata.WantersCount,
		r.Metadata.LastUpdated, string(rawJSON), ts,
	)
	if err != nil {
		return fmt.Errorf("upsert recording row: %w", err)
	}
	return nil
}

// refreshCastEntries wipes and rewrites cast_entries for the recording while
// keeping the first-class performers and characters tables in sync. The legacy
// denormalized columns on cast_entries are still populated (downstream
// consumers haven't migrated off them yet); the upserts into performers and
// characters layer the canonical people rows on top so foreign-key style
// joins (e.g. ListRecordingsForPerformer + LoadPerformer) work post-sync.
func refreshCastEntries(
	ctx context.Context,
	tx *sql.Tx,
	r encora.Recording,
	now func() time.Time,
) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM cast_entries WHERE recording_id = ?`, r.ID,
	); err != nil {
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

		var label, abbrev *string
		if cast.Status != nil {
			label = &cast.Status.Label
			abbrev = &cast.Status.Abbreviation
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO cast_entries (
				recording_id,
				performer_id, performer_name, performer_slug, performer_url,
				character_id, character_name, character_slug, character_url, character_order,
				status_label, status_abbrev
			) VALUES (
				?,
				?, ?, ?, ?,
				?, ?, ?, ?, ?,
				?, ?
			)
		`,
			r.ID,
			cast.Performer.ID, cast.Performer.Name, cast.Performer.Slug, cast.Performer.URL,
			cast.Character.ID, cast.Character.Name, cast.Character.Slug,
			cast.Character.URL, cast.Character.Order,
			label, abbrev,
		)
		if err != nil {
			return fmt.Errorf("insert cast_entry: %w", err)
		}
	}
	return nil
}

func upsertCollection(
	ctx context.Context,
	tx *sql.Tx,
	entry encora.CollectionEntry,
	now func() time.Time,
) error {
	nowTS := now().UTC()
	_, err := tx.ExecContext(ctx, `
		INSERT INTO collection (
			recording_id, format, user_notes, user_watched,
			collected_at, updated_at, last_synced_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(recording_id) DO UPDATE SET
			format         = excluded.format,
			user_notes     = excluded.user_notes,
			user_watched   = excluded.user_watched,
			collected_at   = excluded.collected_at,
			updated_at     = excluded.updated_at,
			last_synced_at = excluded.last_synced_at
	`, entry.Recording.ID, entry.Format, entry.Notes, entry.UserWatched,
		entry.CollectedAt, entry.UpdatedAt, nowTS)
	if err != nil {
		return fmt.Errorf("upsert collection row: %w", err)
	}
	return nil
}

func upsertWant(
	ctx context.Context,
	tx *sql.Tx,
	recordingID int64,
	now func() time.Time,
) error {
	nowTS := now().UTC()
	_, err := tx.ExecContext(ctx, `
		INSERT INTO wants (recording_id, last_synced_at)
		VALUES (?, ?)
		ON CONFLICT(recording_id) DO UPDATE SET last_synced_at = excluded.last_synced_at
	`, recordingID, nowTS)
	if err != nil {
		return fmt.Errorf("upsert wants row: %w", err)
	}
	return nil
}

func insertSyncRun(
	ctx context.Context,
	db *sql.DB,
	kind string,
	startedAt time.Time,
) (int64, error) {
	res, err := db.ExecContext(ctx, `
		INSERT INTO sync_runs (kind, started_at) VALUES (?, ?)
	`, kind, startedAt)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("last insert id: %w", err)
	}
	return id, nil
}

func updateSyncRun(
	ctx context.Context,
	db *sql.DB,
	runID int64,
	finishedAt time.Time,
	okCount, errorCount, rateLimitRemaining int,
	errorText string,
) error {
	_, err := db.ExecContext(ctx, `
		UPDATE sync_runs SET
			finished_at          = ?,
			ok_count             = ?,
			error_count          = ?,
			rate_limit_remaining = ?,
			error_text           = ?
		WHERE id = ?
	`, finishedAt, okCount, errorCount, rateLimitRemaining, errorText, runID)
	if err != nil {
		return err
	}
	return nil
}

// boolToInt is the SQLite-friendly cast for go bools.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nullableString turns "" into NULL so the DB distinguishes empty from
// absent. Encora's payloads use both; preserve the round-trip via raw_json.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
