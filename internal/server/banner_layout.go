package server

// banner_layout.go — helper that merges per-recording banner layout
// choices (position + image-region) into the recording's
// OverlayStyleJSON so the renderer picks them up the same way it
// picks up font / color overrides.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nicolerenee/promptbook/internal/storage"
)

// persistBannerLayout merges position + imageRegion into the
// recording's OverlayStyleJSON via storage.SetOverlayStyle, leaving
// other fields untouched. Empty values clear the corresponding key
// (so the renderer falls back to the global default). When BOTH are
// empty AND the existing JSON has no banner-layout fields, this is
// a no-op so we don't dirty the row.
func (s *Server) persistBannerLayout(
	ctx context.Context, recordingID int64, position, imageRegion string,
) error {
	choice, err := storage.GetImageChoice(ctx, s.db, recordingID)
	if err != nil {
		return fmt.Errorf("load image choice: %w", err)
	}
	current := map[string]any{}
	if choice.OverlayStyleJSON != nil && *choice.OverlayStyleJSON != "" {
		if unmarshalErr := json.Unmarshal(
			[]byte(*choice.OverlayStyleJSON), &current,
		); unmarshalErr != nil {
			// Bad JSON on disk — overwrite rather than fail. The
			// previous value was already malformed and the renderer
			// would have ignored it.
			current = map[string]any{}
		}
	}
	changed := false
	switch position {
	case "":
		if _, ok := current["position"]; ok {
			delete(current, "position")
			changed = true
		}
	default:
		if current["position"] != position {
			current["position"] = position
			changed = true
		}
	}
	switch imageRegion {
	case "":
		if _, ok := current["image_region"]; ok {
			delete(current, "image_region")
			changed = true
		}
	default:
		if current["image_region"] != imageRegion {
			current["image_region"] = imageRegion
			changed = true
		}
	}
	if !changed {
		return nil
	}
	var encoded string
	if len(current) > 0 {
		buf, marshalErr := json.Marshal(current)
		if marshalErr != nil {
			return fmt.Errorf("marshal overlay style json: %w", marshalErr)
		}
		encoded = string(buf)
	}
	return storage.SetOverlayStyle(ctx, s.db, recordingID, encoded)
}
