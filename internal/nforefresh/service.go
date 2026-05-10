// Package nforefresh rewrites movie.nfo files when a recording's
// poster/fanart, its show's banner, or one of its performers' headshots
// changes on disk. Without this rewrite, the NFO carries the same
// /images/* URL it had at first import and a media server's cache
// never picks up the newly-uploaded image — even though the writer
// appends a `?v={mtime}` cache-buster, the buster only matters once
// the new value reaches the file the media server scans.
//
// The Service exposes three fan-out methods:
//
//	RewriteForRecording(id)  → rewrite the one recording's NFO.
//	RewriteForShow(id)       → rewrite every recording's NFO under
//	                            the show (set-thumb / set-fanart
//	                            reference the show banner).
//	RewriteForPerformer(id)  → rewrite every recording the performer
//	                            is credited on (actor-thumb URL).
//
// Triggers fire AFTER the image file is on disk (upload handler,
// set-from-URL, refresh job). Rewrites are best-effort and run as a
// background goroutine off the trigger path so the upload response
// returns immediately. Per-recording errors are logged but never
// abort a batch — a single bad row shouldn't block the rest of the
// fan-out.
package nforefresh

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/rs/zerolog"

	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/ent/castentry"
	"github.com/nicolerenee/promptbook/internal/ent/recording"
	"github.com/nicolerenee/promptbook/internal/externalids"
	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/nfo"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// Service rewrites movie.nfo files in response to image-change events.
// All fields are required — construct via New so a misconfigured
// Service never silently degrades to a no-op.
type Service struct {
	db *ent.Client
	// sqlDB shares db's connection pool. Read by the rewrite path so
	// the regenerated NFO carries one <uniqueid> per external_ids row
	// (encora + tmdb + imdb + …). Optional: nil falls back to the
	// writer's legacy single-Encora shape.
	sqlDB     *sql.DB
	cache     *imagecache.Cache
	publicURL string
	logger    zerolog.Logger
}

// New constructs a Service. publicURL may be empty — the rewrite still
// runs but the resulting NFO falls back to local-sibling-path mode,
// which is the right behaviour for an installation that doesn't expose
// an external URL. db is required; nil aborts the call. sqlDB is
// optional — when nil the rewritten NFO carries only the legacy
// single-Encora <uniqueid>; when set it emits one entry per row in
// external_ids.
func New(
	db *ent.Client,
	sqlDB *sql.DB,
	cache *imagecache.Cache,
	publicURL string,
	logger zerolog.Logger,
) *Service {
	return &Service{
		db:        db,
		sqlDB:     sqlDB,
		cache:     cache,
		publicURL: publicURL,
		logger:    logger,
	}
}

// loadExternalIDs returns the recording's external_ids rows for the
// rewrite path's <uniqueid> emission. Nil sqlDB skips the call so
// callers without the handle wired (legacy tests) keep working —
// the writer's encora fallback covers that mode.
func (s *Service) loadExternalIDs(
	ctx context.Context, recordingID int64,
) ([]externalids.ExternalID, error) {
	if s.sqlDB == nil {
		return nil, nil
	}
	rows, err := externalids.ListForRecording(ctx, s.sqlDB, recordingID)
	if err != nil {
		return nil, fmt.Errorf("list external_ids: %w", err)
	}
	return rows, nil
}

// RewriteForRecording loads the recording's destination folder from
// recording_versions, regenerates the movie.nfo using the current
// recording shape + image mtimes, and writes it in place. No-op when
// no version row exists (recording hasn't been imported yet) or when
// the recording is missing from the local catalog.
func (s *Service) RewriteForRecording(ctx context.Context, recordingID int64) error {
	if s == nil || s.db == nil {
		return errors.New("nforefresh: service not configured")
	}
	loaded, err := storage.LoadRecording(ctx, s.db, recordingID)
	if errors.Is(err, storage.ErrRecordingNotFound) {
		s.logger.Debug().
			Int64("recording_id", recordingID).
			Msg("nforefresh: recording not in catalog; skipping")
		return nil
	}
	if err != nil {
		return fmt.Errorf("nforefresh: load recording %d: %w", recordingID, err)
	}

	folder, err := s.destFolderForRecording(ctx, recordingID)
	if err != nil {
		return err
	}
	if folder == "" {
		// No version row yet — recording exists in the catalog but
		// hasn't been ingested. The NFO will be written by the
		// IngestRunner on first import; nothing to refresh here.
		s.logger.Debug().
			Int64("recording_id", recordingID).
			Msg("nforefresh: recording has no on-disk version; skipping")
		return nil
	}

	extIDs, extIDsErr := s.loadExternalIDs(ctx, recordingID)
	if extIDsErr != nil {
		s.logger.Warn().Err(extIDsErr).Int64("recording_id", recordingID).
			Msg("nforefresh: failed to load external_ids; falling back to legacy shape")
	}
	written, err := nfo.WriteRecordingFile(
		ctx,
		folder,
		loaded.Recording,
		nfo.WriteOptions{
			DB:          s.db,
			Cache:       s.cache,
			PublicURL:   s.publicURL,
			ExternalIDs: extIDs,
		},
	)
	if err != nil {
		return fmt.Errorf("nforefresh: write nfo for recording %d: %w", recordingID, err)
	}
	s.logger.Debug().
		Int64("recording_id", recordingID).
		Str("path", written).
		Msg("nforefresh: nfo rewritten")
	return nil
}

// RewriteForShow walks every recording sharing the show id and
// rewrites each one's NFO. The show banner URL lives inside each
// recording's <set><thumb> + <set><fanart> elements, so a banner
// upload has to fan out to every recording for the change to land
// in the disk files media servers scan.
//
// Per-recording failures are logged but don't abort the batch — one
// bad row shouldn't block the rest of a 50-recording show.
func (s *Service) RewriteForShow(ctx context.Context, showID int64) error {
	if s == nil || s.db == nil {
		return errors.New("nforefresh: service not configured")
	}
	if showID <= 0 {
		return fmt.Errorf("nforefresh: invalid show_id %d", showID)
	}
	recIDs, err := s.db.Recording.Query().
		Where(recording.ShowID(showID)).
		IDs(ctx)
	if err != nil {
		return fmt.Errorf("nforefresh: query recordings for show %d: %w", showID, err)
	}
	s.rewriteBatch(ctx, recIDs, "show", showID)
	return nil
}

// RewriteForPerformer walks every recording where the performer is
// credited and rewrites each NFO. Largest fan-out of the three —
// popular performers can be in 50+ recordings — but the per-recording
// work is bounded (a few stat calls + a small XML write), so even a
// performer in 200 recordings finishes in well under a second.
//
// Per-recording failures are logged but don't abort the batch.
func (s *Service) RewriteForPerformer(ctx context.Context, performerID int64) error {
	if s == nil || s.db == nil {
		return errors.New("nforefresh: service not configured")
	}
	if performerID <= 0 {
		return fmt.Errorf("nforefresh: invalid performer_id %d", performerID)
	}
	// Distinct recording ids credited to the performer. ent's GroupBy
	// on a single column yields a distinct slice; mirrors the helper in
	// internal/storage/people.go but inlined here to keep the rewrite
	// service free of additional storage API surface.
	var recIDs []int64
	if err := s.db.CastEntry.Query().
		Where(castentry.PerformerID(performerID)).
		GroupBy(castentry.FieldRecordingID).
		Scan(ctx, &recIDs); err != nil {
		return fmt.Errorf("nforefresh: query recordings for performer %d: %w",
			performerID, err)
	}
	s.rewriteBatch(ctx, recIDs, "performer", performerID)
	return nil
}

// rewriteBatch dispatches RewriteForRecording across recIDs, swallowing
// per-row errors after a debug log. Callers pass a fan-out kind +
// trigger id purely for the error log; nothing else uses them.
func (s *Service) rewriteBatch(
	ctx context.Context, recIDs []int64, fanoutKind string, fanoutID int64,
) {
	for _, id := range recIDs {
		if err := s.RewriteForRecording(ctx, id); err != nil {
			s.logger.Warn().
				Err(err).
				Int64("recording_id", id).
				Str("fanout_kind", fanoutKind).
				Int64("fanout_id", fanoutID).
				Msg("nforefresh: rewrite failed; continuing batch")
		}
	}
}

// destFolderForRecording returns the parent directory of the
// recording's first version row's file path, or empty when no version
// row exists. Multiple versions of the same recording always live in
// the same canonical folder (template-derived from the recording
// shape), so picking any version's parent yields the right answer.
func (s *Service) destFolderForRecording(
	ctx context.Context, recordingID int64,
) (string, error) {
	versions, err := storage.ListVersions(ctx, s.db, recordingID)
	if err != nil {
		return "", fmt.Errorf("nforefresh: list versions for %d: %w", recordingID, err)
	}
	for _, v := range versions {
		if v.FilePath == "" {
			continue
		}
		return filepath.Dir(v.FilePath), nil
	}
	return "", nil
}
