package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ImageChoice is the per-recording user-curated selection of which
// cached poster + backdrop should be the "chosen" one for NFO output,
// Jellyfin/Plex display, and the in-app UI. All fields are optional —
// a missing row, or null columns inside an existing row, mean "no
// choice was made yet, fall back to index 0".
//
// OverlayTextOverride is the user-supplied label burned into the
// rendered backdrop in place of the auto-derived "show · tour · date"
// string. Empty pointer means "use the auto-derived value".
//
// OverlayStyleJSON is a free-form JSON blob carrying optional style
// overrides (color, position, font weight). Stored as a string so the
// schema doesn't need to change as new style knobs land. Empty pointer
// means "use the global default style".
type ImageChoice struct {
	RecordingID         int64
	PosterIndex         *int
	BackdropIndex       *int
	OverlayTextOverride *string
	OverlayStyleJSON    *string
	// OverlayDisabled, when true, tells the renderer to skip the
	// playbill-style band entirely. The raw selected backdrop is
	// copied through unchanged (or rendered.jpg is omitted and the
	// NFO writer falls back to the raw file). The user picks this
	// when their chosen backdrop already has visible label text or
	// they prefer the unadorned art for Jellyfin/Plex.
	OverlayDisabled bool
}

// ResolvePoster returns the poster index to use for the recording.
// Returns 0 when no choice was made — every cached poster set has at
// least posters/<show_id>/0.jpg as the canonical default.
func (c ImageChoice) ResolvePoster() int {
	if c.PosterIndex != nil {
		return *c.PosterIndex
	}
	return 0
}

// ResolveBackdrop returns the backdrop index to use for the
// recording. Same fallback rules as ResolvePoster.
func (c ImageChoice) ResolveBackdrop() int {
	if c.BackdropIndex != nil {
		return *c.BackdropIndex
	}
	return 0
}

// ResolveOverlayText returns the user override when set, or the
// fallback computed from recording metadata. Caller supplies the
// fallback so this package doesn't need to import recording shape.
func (c ImageChoice) ResolveOverlayText(fallback string) string {
	if c.OverlayTextOverride != nil {
		return *c.OverlayTextOverride
	}
	return fallback
}

// GetImageChoice loads the recording's current image choice. Returns
// a zero-valued ImageChoice (with the recording id set) and a nil
// error when the row doesn't exist — the resolve helpers handle the
// fallback. Other errors propagate.
func GetImageChoice(ctx context.Context, db *sql.DB, recordingID int64) (ImageChoice, error) {
	choice := ImageChoice{RecordingID: recordingID}
	var (
		posterIdx       sql.NullInt64
		backdropIdx     sql.NullInt64
		overlayText     sql.NullString
		overlayJSON     sql.NullString
		overlayDisabled int
	)
	err := db.QueryRowContext(ctx, `
		SELECT poster_index, backdrop_index, overlay_text_override, overlay_style_json, overlay_disabled
		FROM recording_image_choices
		WHERE recording_id = ?
	`, recordingID).Scan(&posterIdx, &backdropIdx, &overlayText, &overlayJSON, &overlayDisabled)
	if errors.Is(err, sql.ErrNoRows) {
		return choice, nil
	}
	if err != nil {
		return choice, fmt.Errorf("get image choice %d: %w", recordingID, err)
	}
	if posterIdx.Valid {
		v := int(posterIdx.Int64)
		choice.PosterIndex = &v
	}
	if backdropIdx.Valid {
		v := int(backdropIdx.Int64)
		choice.BackdropIndex = &v
	}
	if overlayText.Valid {
		v := overlayText.String
		choice.OverlayTextOverride = &v
	}
	if overlayJSON.Valid {
		v := overlayJSON.String
		choice.OverlayStyleJSON = &v
	}
	choice.OverlayDisabled = overlayDisabled != 0
	return choice, nil
}

// SetOverlayDisabled toggles the burn-in opt-out flag for a recording.
// When true the renderer copies the raw backdrop through unchanged.
func SetOverlayDisabled(ctx context.Context, db *sql.DB, recordingID int64, disabled bool) error {
	v := 0
	if disabled {
		v = 1
	}
	return upsertImageChoice(ctx, db, recordingID, "overlay_disabled", sql.NullInt64{Int64: int64(v), Valid: true})
}

// SetPosterIndex upserts the poster choice for a recording. The
// upsert preserves any existing backdrop/overlay columns.
func SetPosterIndex(ctx context.Context, db *sql.DB, recordingID int64, index int) error {
	return upsertImageChoice(ctx, db, recordingID, "poster_index", sql.NullInt64{Int64: int64(index), Valid: true})
}

// SetBackdropIndex upserts the backdrop choice.
func SetBackdropIndex(ctx context.Context, db *sql.DB, recordingID int64, index int) error {
	return upsertImageChoice(ctx, db, recordingID, "backdrop_index", sql.NullInt64{Int64: int64(index), Valid: true})
}

// SetOverlayTextOverride upserts the overlay text override. Pass an
// empty string with override=true to clear an existing override
// without going through SetOverlayTextDefault — useful when the user
// types nothing into the editor and saves.
func SetOverlayTextOverride(ctx context.Context, db *sql.DB, recordingID int64, text string) error {
	return upsertImageChoice(ctx, db, recordingID, "overlay_text_override", sql.NullString{String: text, Valid: true})
}

// ClearOverlayTextOverride nulls the override so the auto-derived
// text takes over again.
func ClearOverlayTextOverride(ctx context.Context, db *sql.DB, recordingID int64) error {
	return upsertImageChoice(ctx, db, recordingID, "overlay_text_override", sql.NullString{Valid: false})
}

// SetOverlayStyle upserts the overlay style JSON blob. Pass an empty
// string to clear (the renderer falls back to the global default).
func SetOverlayStyle(ctx context.Context, db *sql.DB, recordingID int64, styleJSON string) error {
	if styleJSON == "" {
		return upsertImageChoice(ctx, db, recordingID, "overlay_style_json", sql.NullString{Valid: false})
	}
	return upsertImageChoice(ctx, db, recordingID, "overlay_style_json", sql.NullString{String: styleJSON, Valid: true})
}

// upsertImageChoice writes a single column on the
// recording_image_choices row, creating the row if it doesn't exist.
// The whitelisted columnNames keep this safe from SQL injection —
// the column name is interpolated, not parameterized.
//
//nolint:gosec // column is whitelisted; not user input.
func upsertImageChoice(ctx context.Context, db *sql.DB, recordingID int64, column string, value any) error {
	switch column {
	case "poster_index", "backdrop_index", "overlay_text_override", "overlay_style_json", "overlay_disabled":
	default:
		return fmt.Errorf("upsertImageChoice: unsupported column %q", column)
	}
	q := fmt.Sprintf(`
		INSERT INTO recording_image_choices (recording_id, %s, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(recording_id) DO UPDATE SET
			%s = excluded.%s,
			updated_at = CURRENT_TIMESTAMP
	`, column, column, column)
	if _, err := db.ExecContext(ctx, q, recordingID, value); err != nil {
		return fmt.Errorf("upsert image choice %s for recording %d: %w", column, recordingID, err)
	}
	return nil
}
