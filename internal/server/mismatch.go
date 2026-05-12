package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// MismatchType labels a single divergence between the local cache and
// what Encora knows. Each non-Synced storage.Status maps to exactly one
// MismatchType so the review surface lists every recording exactly
// once.
type MismatchType string

// The four mismatch types the review surface enumerates. The string
// values match the ?type= query tokens used by the JSON + HTML
// handlers.
const (
	// MismatchTypeAddToCollection is raised when the user has a file on
	// disk for a recording that isn't in their Encora collection — the
	// suggested push is POST /collection/{id}/collect.
	MismatchTypeAddToCollection MismatchType = "add_to_collection"
	// MismatchTypeOutOfSync is raised when the locally-computed
	// format string differs from the collection.format value — the
	// suggested push is POST /collection/{id}/format/{...}.
	MismatchTypeOutOfSync MismatchType = "out_of_sync"
	// MismatchTypeMissingFile is raised when the recording is in the
	// collection but no version row backs it locally — there's nothing
	// to push, but the user may want to download the file.
	MismatchTypeMissingFile MismatchType = "missing_file"
	// MismatchTypeWantedFile is raised when the recording is on the
	// wants list with no local file — also nothing to push, just a
	// reminder to download.
	MismatchTypeWantedFile MismatchType = "wanted_file"
)

// mismatchTypeLabels lists the MismatchTypes the UI exposes as filter
// tabs, in display order. The empty Key entry is the "All" tab —
// equivalent to no filter. Ordering matches the natural workflow:
// pushable changes first, download reminders last.
//
//nolint:gochecknoglobals // immutable display-order lookup table.
var mismatchTypeLabels = []struct {
	Key   MismatchType
	Label string
}{
	{Key: "", Label: allTabLabel},
	{Key: MismatchTypeAddToCollection, Label: "Add to collection"},
	{Key: MismatchTypeOutOfSync, Label: "Out of sync"},
	{Key: MismatchTypeMissingFile, Label: "Missing file"},
	{Key: MismatchTypeWantedFile, Label: "Wanted file"},
}

// validMismatchTypes is the set of type tokens parseMismatchTypeParam
// accepts. Built from mismatchTypeLabels so the nav tabs and the
// validation list cannot drift apart.
//
//nolint:gochecknoglobals // immutable lookup set.
var validMismatchTypes = func() map[MismatchType]struct{} {
	out := make(map[MismatchType]struct{}, len(mismatchTypeLabels))
	for _, t := range mismatchTypeLabels {
		if t.Key != "" {
			out[t.Key] = struct{}{}
		}
	}
	return out
}()

// mismatchScanLimit caps how many states ListMismatches materializes
// before filtering. Set well above the user's expected library size so
// a single page surfaces every divergence.
const mismatchScanLimit = 4096

// MismatchItem is one row in the mismatches list. Type identifies the
// kind of divergence; Description is the pre-baked one-line summary
// the UI renders. LocalFormat / EncoraFormat are populated for
// OutOfSync rows and otherwise zero.
type MismatchItem struct {
	Type         MismatchType   `json:"type"`
	RecordingID  int64          `json:"recording_id"`
	Show         string         `json:"show"`
	Tour         string         `json:"tour"`
	DateFull     string         `json:"date_full"`
	Status       storage.Status `json:"status"`
	LocalFormat  string         `json:"local_format"`
	EncoraFormat string         `json:"encora_format"`
	Description  string         `json:"description"`
}

// loadMismatches walks every non-Synced recording state, translates it
// into a MismatchItem, and returns the slice sorted by recording_id.
// When types is non-empty the result is filtered to those types
// post-aggregation. Show/tour/date are looked up in a single batch
// query keyed on the surviving recording ids.
func loadMismatches(
	ctx context.Context,
	client *ent.Client,
	types []MismatchType,
) ([]MismatchItem, error) {
	states, err := storage.ListStates(ctx, client, storage.ListStatesOptions{Limit: mismatchScanLimit})
	if err != nil {
		return nil, fmt.Errorf("list states: %w", err)
	}

	wantTypes := mismatchTypeSet(types)

	prelim := make([]MismatchItem, 0, len(states))
	for _, st := range states {
		item, ok := stateToMismatch(st)
		if !ok {
			continue
		}
		if wantTypes != nil {
			if _, found := wantTypes[item.Type]; !found {
				continue
			}
		}
		prelim = append(prelim, item)
	}

	if len(prelim) == 0 {
		return []MismatchItem{}, nil
	}

	// Promote storage.RecordingState into recordingMeta for the
	// surviving rows in one query so show/tour/date display matches
	// the home page.
	pageStates := make([]storage.RecordingState, 0, len(prelim))
	for _, item := range prelim {
		pageStates = append(pageStates, storage.RecordingState{RecordingID: item.RecordingID})
	}
	meta, err := loadRecordingMeta(ctx, client, pageStates)
	if err != nil {
		return nil, err
	}
	for i := range prelim {
		m := meta[prelim[i].RecordingID]
		prelim[i].Show = m.show
		prelim[i].Tour = m.tour
		prelim[i].DateFull = m.dateFull
	}

	sort.SliceStable(prelim, func(i, j int) bool {
		return prelim[i].RecordingID < prelim[j].RecordingID
	})
	return prelim, nil
}

// stateToMismatch maps a single RecordingState onto a MismatchItem.
// Returns ok=false for Synced states and any unexpected status value
// so callers can skip them silently.
func stateToMismatch(st storage.RecordingState) (MismatchItem, bool) {
	item := MismatchItem{
		RecordingID:  st.RecordingID,
		Status:       st.Status,
		LocalFormat:  st.LocalFormat,
		EncoraFormat: st.EncoraFormat,
	}
	switch st.Status {
	case storage.StatusOutOfSync:
		// OutOfSync covers three subcases (see storage.ComputeStatus):
		// in-collection-with-format-drift, in-wants-with-file, and
		// have-file-but-no-encora-link. The first dispatches to a
		// format push; the latter two need an add-to-collection
		// upstream write, so we split the MismatchType here.
		if st.InCollection {
			item.Type = MismatchTypeOutOfSync
			item.Description = fmt.Sprintf(
				"Local %q differs from Encora %q — push to update",
				st.LocalFormat, st.EncoraFormat,
			)
		} else {
			item.Type = MismatchTypeAddToCollection
			item.Description = "Has file; not in collection — push to add"
		}
	case storage.StatusOrphan:
		// Metadata-only ghost (no file, no collection, no wants).
		// Nothing to reconcile upstream — the user has to either
		// add to collection/wants manually or remove the row.
		return MismatchItem{}, false
	case storage.StatusMissing:
		item.Type = MismatchTypeMissingFile
		item.Description = "In collection but no local file — download to reconcile"
	case storage.StatusWanted:
		item.Type = MismatchTypeWantedFile
		item.Description = "On wants list but no local file — download to reconcile"
	case storage.StatusSynced:
		return MismatchItem{}, false
	default:
		return MismatchItem{}, false
	}
	return item, true
}

// parseMismatchTypeParam splits a comma-separated type CSV into the
// recognized MismatchType set. Unknown tokens are dropped silently so
// stale links don't 400 the JSON endpoint or the HTML page.
func parseMismatchTypeParam(raw string) []MismatchType {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]MismatchType, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		mt := MismatchType(p)
		if _, ok := validMismatchTypes[mt]; !ok {
			continue
		}
		out = append(out, mt)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mismatchTypeSet returns nil for an empty slice (meaning "no filter")
// and a set-shaped map otherwise. Mirrors statusSet over in storage.
func mismatchTypeSet(types []MismatchType) map[MismatchType]struct{} {
	if len(types) == 0 {
		return nil
	}
	m := make(map[MismatchType]struct{}, len(types))
	for _, t := range types {
		m[t] = struct{}{}
	}
	return m
}

// handleListMismatches serves the JSON mismatch list. Empty result
// still returns {items: []} so the client can range over it safely.
func (s *Server) handleListMismatches(c echo.Context) error {
	types := parseMismatchTypeParam(c.QueryParam("type"))
	items, err := loadMismatches(c.Request().Context(), s.db, types)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{itemsKey: items})
}
