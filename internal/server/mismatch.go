package server

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/labstack/echo/v4"

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
	// MismatchTypeFormatMismatch is raised when the locally-computed
	// format string differs from the collection.format value — the
	// suggested push is POST /collection/{id}/format/{...}.
	MismatchTypeFormatMismatch MismatchType = "format_mismatch"
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
	{Key: MismatchTypeFormatMismatch, Label: "Format mismatch"},
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
// FormatMismatch rows and otherwise zero.
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
	db *sql.DB,
	types []MismatchType,
) ([]MismatchItem, error) {
	states, err := storage.ListStates(ctx, db, storage.ListStatesOptions{Limit: mismatchScanLimit})
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
	meta, err := loadRecordingMeta(ctx, db, pageStates)
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
	case storage.StatusOrphan:
		item.Type = MismatchTypeAddToCollection
		item.Description = "Has file; not in collection — push to add"
	case storage.StatusFormatMismatch:
		item.Type = MismatchTypeFormatMismatch
		item.Description = fmt.Sprintf(
			"Local %q differs from Encora %q — push to update",
			st.LocalFormat, st.EncoraFormat,
		)
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

// mismatchTypeTab is one tab entry rendered at the top of /mismatches.
// Active is true for the tab matching the current ?type= filter; Count
// is the per-type count over the unfiltered set so users can see how
// many of each kind exist before clicking through.
type mismatchTypeTab struct {
	Key    string
	Label  string
	Count  int
	Active bool
}

// mismatchPageData is the view-model for /mismatches.
type mismatchPageData struct {
	Title      string
	ActiveType string
	Tabs       []mismatchTypeTab
	Items      []MismatchItem
}

// handleMismatchesPage renders the mismatches review surface. The
// template counts each type tab from the unfiltered list so users can
// see at a glance how much work each filter scope represents.
func (s *Server) handleMismatchesPage(c echo.Context) error {
	rawType := c.QueryParam("type")
	types := parseMismatchTypeParam(rawType)

	ctx := c.Request().Context()

	// Compute counts off the unfiltered set so the tabs always show
	// totals; the displayed list applies the user's filter.
	all, err := loadMismatches(ctx, s.db, nil)
	if err != nil {
		return err
	}
	counts := make(map[MismatchType]int, len(mismatchTypeLabels))
	for _, item := range all {
		counts[item.Type]++
	}

	items := all
	if len(types) > 0 {
		items, err = loadMismatches(ctx, s.db, types)
		if err != nil {
			return err
		}
	}

	// "All" tab is the active one when no recognizable type tokens
	// were supplied; otherwise the first valid token wins. The
	// single-tab UI can't represent a multi-type filter, so a
	// multi-token CSV falls back to "All" highlighting.
	activeType := MismatchType("")
	if len(types) == 1 {
		activeType = types[0]
	}

	tabs := make([]mismatchTypeTab, 0, len(mismatchTypeLabels))
	for _, t := range mismatchTypeLabels {
		count := counts[t.Key]
		if t.Key == "" {
			count = len(all)
		}
		tabs = append(tabs, mismatchTypeTab{
			Key:    string(t.Key),
			Label:  t.Label,
			Count:  count,
			Active: t.Key == activeType,
		})
	}

	return c.Render(http.StatusOK, "mismatches.html", mismatchPageData{
		Title:      "Mismatches",
		ActiveType: string(activeType),
		Tabs:       tabs,
		Items:      items,
	})
}
