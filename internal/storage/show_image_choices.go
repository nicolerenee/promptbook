package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ShowImageChoice is the per-show user-curated selection of which
// cached poster represents the "show" entity (i.e. the Plex/Jellyfin
// collection-level art). Shows don't get a burned-in overlay band —
// the playbill-style label is recording-specific — so the choice is
// just a poster index. Falls back to index 0 when unset.
type ShowImageChoice struct {
	ShowID      int64
	PosterIndex *int
}

// ResolvePoster returns the poster index for the show. Defaults to 0
// (the first cached poster) when the user hasn't picked one.
func (c ShowImageChoice) ResolvePoster() int {
	if c.PosterIndex != nil {
		return *c.PosterIndex
	}
	return 0
}

// GetShowImageChoice loads the show's poster choice. A missing row
// returns a zero-valued ShowImageChoice (with the show id set) and
// nil error — the resolve helper handles the fallback.
func GetShowImageChoice(ctx context.Context, db *sql.DB, showID int64) (ShowImageChoice, error) {
	choice := ShowImageChoice{ShowID: showID}
	var posterIdx sql.NullInt64
	err := db.QueryRowContext(ctx, `
		SELECT poster_index FROM show_image_choices WHERE show_id = ?
	`, showID).Scan(&posterIdx)
	if errors.Is(err, sql.ErrNoRows) {
		return choice, nil
	}
	if err != nil {
		return choice, fmt.Errorf("get show image choice %d: %w", showID, err)
	}
	if posterIdx.Valid {
		v := int(posterIdx.Int64)
		choice.PosterIndex = &v
	}
	return choice, nil
}

// SetShowPosterIndex upserts the show's poster choice. Index can be
// any non-negative int — bounds checking against actually-cached
// files is the caller's responsibility (the API handler does it).
func SetShowPosterIndex(ctx context.Context, db *sql.DB, showID int64, index int) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO show_image_choices (show_id, poster_index, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(show_id) DO UPDATE SET
			poster_index = excluded.poster_index,
			updated_at = CURRENT_TIMESTAMP
	`, showID, index)
	if err != nil {
		return fmt.Errorf("upsert show image choice %d: %w", showID, err)
	}
	return nil
}
