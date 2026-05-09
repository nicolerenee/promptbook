// Package imagerender produces the burned-in backdrop image
// (rendered.jpg) the NFO writer points Jellyfin/Plex at as
// <fanart>. The raw Encora screen-grab is preserved on disk under
// backdrops/<recording_id>/<index>.jpg; rendered.jpg sits next to it
// with the user-curated text band composited at the bottom.
//
// This package is currently a wiring stub — Regenerate is a no-op
// returning nil. The actual playbill-style band renderer (color, font,
// layout) lands in a follow-up wave; the function signature is fixed
// up front so callers (the recording-detail POST handlers, the future
// regenerate endpoint) can wire in now without churning when the
// renderer fills in.
package imagerender

import (
	"context"
	"database/sql"

	"github.com/rs/zerolog"

	"github.com/nicolerenee/promptbook/internal/imagecache"
)

// Renderer composites raw backdrops with a user-curated text overlay
// and writes the result to rendered.jpg under the same recording's
// backdrop directory.
type Renderer struct {
	DB     *sql.DB
	Cache  *imagecache.Cache
	Logger zerolog.Logger
}

// New constructs a Renderer. Returns nil when cache is nil or
// disabled — the caller's nil-check skips regeneration in that mode.
func New(db *sql.DB, cache *imagecache.Cache, logger zerolog.Logger) *Renderer {
	if cache == nil || cache.Disabled() {
		return nil
	}
	return &Renderer{DB: db, Cache: cache, Logger: logger}
}

// Regenerate composes rendered.jpg for the recording from its
// currently-selected raw backdrop and the overlay text + style stored
// in recording_image_choices. Idempotent — calling it repeatedly for
// the same selection produces a byte-identical file. No-op when the
// recording has no cached backdrops.
//
// Stub: the current implementation logs at debug and returns nil so
// downstream callers can already wire it in. The full playbill-style
// band renderer lands in a follow-up wave.
func (r *Renderer) Regenerate(ctx context.Context, recordingID int64) error {
	if r == nil {
		return nil
	}
	r.Logger.Debug().Int64("recording_id", recordingID).Msg("imagerender.Regenerate: stub no-op")
	_ = ctx
	return nil
}
