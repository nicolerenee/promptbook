package server

// picker_options_tmdb.go — TMDB poster + fanart picker source for
// recordings tagged with a TMDB or IMDB external id.
//
// Wiring rules:
//
//   - When the recording has a TMDB external id, that id drives the
//     /movie/{tmdb_id}/images call directly.
//   - When the recording has only an IMDB id (the Velvet Antlers Revival-style
//     {imdb-ttNN} folder name shape), the helper resolves it to a
//     TMDB id via /find/{imdb_id}?external_source=imdb_id before
//     calling /images.
//   - When the recording carries neither, the helper returns 0
//     options + nil error — the picker source is silently absent.
//   - When the TMDB client itself isn't configured (no API key),
//     the helper returns 0 options + nil error so the other sources
//     keep working.
//
// All TMDB calls are wrapped in pickerUpstreamTimeout so a slow API
// can't pin the picker modal.

import (
	"context"
	"errors"

	"github.com/nicolerenee/promptbook/internal/externalids"
	"github.com/nicolerenee/promptbook/internal/tmdb"
)

// pickerSourceTMDB is the Source tag the SPA picker UI groups by.
// Distinct from "encora" / "stagemedia" / "frames" so the SPA can
// label / colour the chip cluster appropriately.
const pickerSourceTMDB = "tmdb"

// fetchTMDBPosterOptions resolves the recording's TMDB id (via TMDB
// or IMDB external id), calls /movie/{id}/images, and maps the
// poster array onto pickerOptions. Returns ([], nil) on every
// "fall through cleanly" signal (no TMDB key, recording has no
// TMDB/IMDB id, TMDB has no record for the id) so the caller can
// safely concatenate the result onto its existing options without a
// presence check.
func (s *Server) fetchTMDBPosterOptions(
	ctx context.Context, recordingID int64,
) []pickerOption {
	imgs, ok := s.resolveTMDBImages(ctx, recordingID)
	if !ok {
		return []pickerOption{}
	}
	out := make([]pickerOption, 0, len(imgs.Posters))
	for _, p := range imgs.Posters {
		if p.URL == "" {
			continue
		}
		out = append(out, pickerOption{URL: p.URL, Source: pickerSourceTMDB})
	}
	return out
}

// fetchTMDBFanartOptions is the backdrops mirror of
// fetchTMDBPosterOptions. Same fall-through semantics.
func (s *Server) fetchTMDBFanartOptions(
	ctx context.Context, recordingID int64,
) []pickerOption {
	imgs, ok := s.resolveTMDBImages(ctx, recordingID)
	if !ok {
		return []pickerOption{}
	}
	out := make([]pickerOption, 0, len(imgs.Backdrops))
	for _, b := range imgs.Backdrops {
		if b.URL == "" {
			continue
		}
		out = append(out, pickerOption{URL: b.URL, Source: pickerSourceTMDB})
	}
	return out
}

// resolveTMDBImages is the shared driver for the poster + fanart
// helpers above: looks up the recording's external_ids, resolves a
// TMDB id (directly or via IMDB → TMDB), calls /movie/{id}/images,
// and returns the result. The bool return is false when any step
// short-circuits cleanly (no TMDB client, no eligible id, no upstream
// match); errors are logged at warn level and swallowed since the
// picker fans out across multiple sources and one source failing
// shouldn't tank the whole response.
func (s *Server) resolveTMDBImages(
	ctx context.Context, recordingID int64,
) (tmdb.Images, bool) {
	if s.tmdb == nil || s.sqlDB == nil {
		return tmdb.Images{}, false
	}
	tmdbID, ok := s.resolveTMDBID(ctx, recordingID)
	if !ok {
		return tmdb.Images{}, false
	}
	upstreamCtx, cancel := context.WithTimeout(ctx, pickerUpstreamTimeout)
	defer cancel()
	imgs, err := s.tmdb.Images(upstreamCtx, tmdbID)
	if err != nil {
		if errors.Is(err, tmdb.ErrNotFound) {
			// TMDB doesn't know the id — surface as "no options"
			// rather than a hard error so the picker stays usable.
			return tmdb.Images{}, false
		}
		s.logger.Warn().Err(err).
			Int64("recording_id", recordingID).
			Int64("tmdb_id", tmdbID).
			Msg("picker: tmdb images fetch failed")
		return tmdb.Images{}, false
	}
	return imgs, true
}

// resolveTMDBID picks the TMDB id for the recording. Direct TMDB
// external id wins; falls back to IMDB → TMDB resolution via
// /find/{imdb_id} when no TMDB id is on file but an IMDB id is.
// Returns (id, true) on success, (0, false) on every "no id
// available" signal (no rows, no eligible provider, IMDB lookup
// missed). Errors are logged at warn level and treated as a miss.
func (s *Server) resolveTMDBID(
	ctx context.Context, recordingID int64,
) (int64, bool) {
	rows, err := externalids.ListForRecording(ctx, s.sqlDB, recordingID)
	if err != nil {
		s.logger.Warn().Err(err).
			Int64("recording_id", recordingID).
			Msg("picker: list external_ids failed for tmdb resolution")
		return 0, false
	}
	var imdbID string
	for _, row := range rows {
		switch row.Provider {
		case externalids.ProviderTMDB:
			tmdbID, parseOK := parseInt64(row.ExternalID)
			if !parseOK {
				continue
			}
			return tmdbID, true
		case externalids.ProviderIMDB:
			imdbID = row.ExternalID
		case externalids.ProviderEncora:
			// Encora doesn't resolve to a TMDB id (it's a different
			// catalog). Explicit case here so the exhaustive linter
			// is happy; the picker just skips Encora rows.
		}
	}
	if imdbID == "" {
		return 0, false
	}
	upstreamCtx, cancel := context.WithTimeout(ctx, pickerUpstreamTimeout)
	defer cancel()
	tmdbID, ok, err := s.tmdb.FindByIMDBID(upstreamCtx, imdbID)
	if err != nil {
		s.logger.Warn().Err(err).
			Int64("recording_id", recordingID).
			Str("imdb_id", imdbID).
			Msg("picker: tmdb FindByIMDBID failed")
		return 0, false
	}
	if !ok {
		return 0, false
	}
	return tmdbID, true
}

// parseInt64 returns (val, true) on success, (0, false) on parse
// failure. Inline helper to avoid importing strconv at the package
// level just for one branch; the TMDB id column is always numeric
// digits, so a malformed value is the caller's bug, not a transient
// upstream condition. Lives here (not in helpers.go) to keep all the
// TMDB-only logic colocated.
func parseInt64(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		const decimalBase = 10
		n = n*decimalBase + int64(r-'0')
	}
	return n, true
}
