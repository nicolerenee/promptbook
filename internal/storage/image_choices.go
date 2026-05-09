package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ImageChoice is the per-recording user-curated overlay configuration
// for the burned-in poster the renderer emits at poster.jpg. Under the
// single-file-per-slot v2 cache there are no index fields — the chosen
// images are implicit by file existence on disk. Only the overlay
// knobs survive on this row:
//
//   - OverlayTextOverride is the user-supplied label burned into the
//     rendered poster in place of the auto-derived "show · tour · date"
//     string. nil means "use the auto-derived value".
//   - OverlayStyleJSON is a free-form JSON blob carrying optional style
//     overrides (color, position, font weight). nil means "use the
//     global default style".
//   - OverlayDisabled, when true, tells the renderer to skip the
//     playbill-style band entirely. The raw poster source is copied
//     through unchanged.
type ImageChoice struct {
	RecordingID         int64
	OverlayTextOverride *string
	OverlayStyleJSON    *string
	OverlayDisabled     bool
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

// GetImageChoice loads the recording's current overlay configuration.
// Returns a zero-valued ImageChoice (with the recording id set) and a
// nil error when the row doesn't exist — callers handle the fallback.
// Other errors propagate.
func GetImageChoice(ctx context.Context, db *sql.DB, recordingID int64) (ImageChoice, error) {
	choice := ImageChoice{RecordingID: recordingID}
	var (
		overlayText     sql.NullString
		overlayJSON     sql.NullString
		overlayDisabled int
	)
	err := db.QueryRowContext(ctx, `
		SELECT overlay_text_override, overlay_style_json, overlay_disabled
		FROM recording_image_choices
		WHERE recording_id = ?
	`, recordingID).Scan(&overlayText, &overlayJSON, &overlayDisabled)
	if errors.Is(err, sql.ErrNoRows) {
		return choice, nil
	}
	if err != nil {
		return choice, fmt.Errorf("get image choice %d: %w", recordingID, err)
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
// When true the renderer copies the raw poster source through unchanged.
func SetOverlayDisabled(ctx context.Context, db *sql.DB, recordingID int64, disabled bool) error {
	v := 0
	if disabled {
		v = 1
	}
	return upsertImageChoice(ctx, db, recordingID, "overlay_disabled",
		sql.NullInt64{Int64: int64(v), Valid: true})
}

// SetOverlayTextOverride upserts the overlay text override. Pass an
// empty string with override=true to clear an existing override
// without going through SetOverlayTextDefault — useful when the user
// types nothing into the editor and saves.
func SetOverlayTextOverride(ctx context.Context, db *sql.DB, recordingID int64, text string) error {
	return upsertImageChoice(ctx, db, recordingID, "overlay_text_override",
		sql.NullString{String: text, Valid: true})
}

// ClearOverlayTextOverride nulls the override so the auto-derived
// text takes over again.
func ClearOverlayTextOverride(ctx context.Context, db *sql.DB, recordingID int64) error {
	return upsertImageChoice(ctx, db, recordingID, "overlay_text_override",
		sql.NullString{Valid: false})
}

// SetOverlayStyle upserts the overlay style JSON blob. Pass an empty
// string to clear (the renderer falls back to the global default).
func SetOverlayStyle(ctx context.Context, db *sql.DB, recordingID int64, styleJSON string) error {
	if styleJSON == "" {
		return upsertImageChoice(ctx, db, recordingID, "overlay_style_json",
			sql.NullString{Valid: false})
	}
	return upsertImageChoice(ctx, db, recordingID, "overlay_style_json",
		sql.NullString{String: styleJSON, Valid: true})
}

// upsertImageChoice writes a single column on the
// recording_image_choices row, creating the row if it doesn't exist.
// The whitelisted columnNames keep this safe from SQL injection —
// the column name is interpolated, not parameterized.
//
//nolint:gosec // column is whitelisted; not user input.
func upsertImageChoice(ctx context.Context, db *sql.DB, recordingID int64, column string, value any) error {
	switch column {
	case "overlay_text_override", "overlay_style_json", "overlay_disabled":
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
