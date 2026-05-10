package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/ent/recordingimagechoice"
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
func GetImageChoice(
	ctx context.Context, client *ent.Client, recordingID int64,
) (ImageChoice, error) {
	choice := ImageChoice{RecordingID: recordingID}
	row, err := client.RecordingImageChoice.Get(ctx, recordingID)
	if ent.IsNotFound(err) {
		return choice, nil
	}
	if err != nil {
		return choice, fmt.Errorf("get image choice %d: %w", recordingID, err)
	}
	if row.OverlayTextOverride != nil {
		v := *row.OverlayTextOverride
		choice.OverlayTextOverride = &v
	}
	if row.OverlayStyleJSON != nil {
		v := *row.OverlayStyleJSON
		choice.OverlayStyleJSON = &v
	}
	choice.OverlayDisabled = row.OverlayDisabled
	return choice, nil
}

// SetOverlayDisabled toggles the burn-in opt-out flag for a recording.
// When true the renderer copies the raw poster source through unchanged.
func SetOverlayDisabled(
	ctx context.Context, client *ent.Client, recordingID int64, disabled bool,
) error {
	now := time.Now().UTC()
	err := client.RecordingImageChoice.Create().
		SetID(recordingID).
		SetOverlayDisabled(disabled).
		SetUpdatedAt(now).
		OnConflictColumns(recordingimagechoice.FieldID).
		Update(func(u *ent.RecordingImageChoiceUpsert) {
			u.UpdateOverlayDisabled()
			u.SetUpdatedAt(now)
		}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("upsert image choice overlay_disabled for recording %d: %w", recordingID, err)
	}
	return nil
}

// SetOverlayTextOverride upserts the overlay text override. Pass an
// empty string with override=true to clear an existing override
// without going through SetOverlayTextDefault — useful when the user
// types nothing into the editor and saves.
func SetOverlayTextOverride(
	ctx context.Context, client *ent.Client, recordingID int64, text string,
) error {
	return upsertOverlayText(ctx, client, recordingID, &text)
}

// ClearOverlayTextOverride nulls the override so the auto-derived
// text takes over again.
func ClearOverlayTextOverride(
	ctx context.Context, client *ent.Client, recordingID int64,
) error {
	return upsertOverlayText(ctx, client, recordingID, nil)
}

func upsertOverlayText(
	ctx context.Context, client *ent.Client, recordingID int64, value *string,
) error {
	now := time.Now().UTC()
	create := client.RecordingImageChoice.Create().
		SetID(recordingID).
		SetUpdatedAt(now)
	if value != nil {
		create = create.SetOverlayTextOverride(*value)
	}
	err := create.
		OnConflictColumns(recordingimagechoice.FieldID).
		Update(func(u *ent.RecordingImageChoiceUpsert) {
			if value != nil {
				u.SetOverlayTextOverride(*value)
			} else {
				u.ClearOverlayTextOverride()
			}
			u.SetUpdatedAt(now)
		}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("upsert image choice overlay_text_override for recording %d: %w", recordingID, err)
	}
	return nil
}

// SetOverlayStyle upserts the overlay style JSON blob. Pass an empty
// string to clear (the renderer falls back to the global default).
func SetOverlayStyle(
	ctx context.Context, client *ent.Client, recordingID int64, styleJSON string,
) error {
	now := time.Now().UTC()
	create := client.RecordingImageChoice.Create().
		SetID(recordingID).
		SetUpdatedAt(now)
	if styleJSON != "" {
		create = create.SetOverlayStyleJSON(styleJSON)
	}
	err := create.
		OnConflictColumns(recordingimagechoice.FieldID).
		Update(func(u *ent.RecordingImageChoiceUpsert) {
			if styleJSON != "" {
				u.SetOverlayStyleJSON(styleJSON)
			} else {
				u.ClearOverlayStyleJSON()
			}
			u.SetUpdatedAt(now)
		}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("upsert image choice overlay_style_json for recording %d: %w", recordingID, err)
	}
	return nil
}
