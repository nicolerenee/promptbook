package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/storage"
)

// historyListLimit is the default page size for /api/v1/history and the
// /history HTML page when ?limit= is not supplied.
const historyListLimit = 50

// historyKindLabels lists the history-event kinds the UI exposes as filter
// tabs, in display order. The empty Key entry is the "All" tab — equivalent
// to no kind filter at all. The set must stay in sync with the
// HistoryKind* constants in internal/storage/history.go.
//
//nolint:gochecknoglobals // immutable display-order lookup table.
var historyKindLabels = []struct {
	Key   string
	Label string
}{
	{Key: "", Label: allTabLabel},
	{Key: storage.HistoryKindIngest, Label: "Ingest"},
	{Key: storage.HistoryKindRename, Label: "Rename"},
	{Key: storage.HistoryKindNFOWrite, Label: "NFO Write"},
	{Key: storage.HistoryKindEncoraPush, Label: "Encora Push"},
	{Key: storage.HistoryKindSync, Label: "Sync"},
	{Key: storage.HistoryKindManualImport, Label: "Manual Import"},
}

// validHistoryKinds is the set of kind tokens the JSON + HTML handlers
// accept on the ?kind= query string. Built from historyKindLabels so the
// nav tabs and the validation list cannot drift apart.
//
//nolint:gochecknoglobals // immutable lookup set.
var validHistoryKinds = func() map[string]struct{} {
	out := make(map[string]struct{}, len(historyKindLabels))
	for _, k := range historyKindLabels {
		if k.Key != "" {
			out[k.Key] = struct{}{}
		}
	}
	return out
}()

// historyItem is the JSON shape returned by /api/v1/history. Mirrors
// storage.HistoryEvent but encodes occurred_at as RFC3339 and surfaces
// the optional recording_id as a nullable int.
type historyItem struct {
	ID          int64          `json:"id"`
	OccurredAt  string         `json:"occurred_at"`
	Kind        string         `json:"kind"`
	RecordingID *int64         `json:"recording_id"`
	Summary     string         `json:"summary"`
	Details     map[string]any `json:"details"`
}

// handleListHistory serves the JSON history list. Honors the same
// ?kind=, ?limit=, ?offset= query params as the HTML page; unknown kinds
// in the CSV are silently dropped so a stale link doesn't 400 the UI.
func (s *Server) handleListHistory(c echo.Context) error {
	kinds := parseHistoryKindParam(c.QueryParam("kind"))
	limit := paramInt(c, "limit", historyListLimit)
	offset := paramInt(c, "offset", 0)

	events, err := storage.ListHistory(c.Request().Context(), s.db, storage.ListHistoryOptions{
		Kinds:  kinds,
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		return err
	}
	items := make([]historyItem, 0, len(events))
	for _, e := range events {
		items = append(items, toHistoryItem(e))
	}
	return c.JSON(http.StatusOK, map[string]any{
		itemsKey:  items,
		limitKey:  limit,
		offsetKey: offset,
	})
}

// historyKindTab is a single tab entry rendered at the top of /history.
// Active is true for the tab that matches the current ?kind= filter.
type historyKindTab struct {
	Key    string
	Label  string
	Active bool
}

// historyPageRow is the per-row view-model the history.html template
// renders. OccurredAt is pre-formatted and DetailsJSON is pre-rendered
// to keep the template logic-free.
type historyPageRow struct {
	ID          int64
	OccurredAt  string
	Kind        string
	RecordingID *int64
	Summary     string
	HasDetails  bool
	DetailsJSON string
}

type historyPageData struct {
	Title      string
	ActiveKind string
	Tabs       []historyKindTab
	Events     []historyPageRow
}

// handleHistoryPage renders the history log as an HTML table. Read-only;
// no mutation paths land in this scope.
func (s *Server) handleHistoryPage(c echo.Context) error {
	rawKind := c.QueryParam("kind")
	kinds := parseHistoryKindParam(rawKind)

	events, err := storage.ListHistory(c.Request().Context(), s.db, storage.ListHistoryOptions{
		Kinds: kinds,
		Limit: historyListLimit,
	})
	if err != nil {
		return err
	}

	rows := make([]historyPageRow, 0, len(events))
	for _, e := range events {
		rows = append(rows, toHistoryPageRow(e))
	}

	// "All" tab is the active one when no recognizable kind tokens were
	// supplied; otherwise the first valid token wins. The single-tab UI
	// can't represent a multi-kind filter, so a multi-token CSV falls
	// back to "All" highlighting.
	activeKind := ""
	if len(kinds) == 1 {
		activeKind = kinds[0]
	}

	tabs := make([]historyKindTab, 0, len(historyKindLabels))
	for _, k := range historyKindLabels {
		tabs = append(tabs, historyKindTab{
			Key:    k.Key,
			Label:  k.Label,
			Active: k.Key == activeKind,
		})
	}

	return c.Render(http.StatusOK, "history.html", historyPageData{
		Title:      "History",
		ActiveKind: activeKind,
		Tabs:       tabs,
		Events:     rows,
	})
}

// parseHistoryKindParam splits a comma-separated kind value into the
// slice of recognized HistoryKind* constants. Unknown tokens are
// dropped silently so the UI is forgiving of stale links and typos.
// Returns nil for "no filter" so callers can pass it straight to
// storage.ListHistoryOptions.Kinds.
func parseHistoryKindParam(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := validHistoryKinds[p]; !ok {
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// toHistoryItem projects a storage.HistoryEvent onto the JSON item shape
// used by /api/v1/history.
func toHistoryItem(e storage.HistoryEvent) historyItem {
	return historyItem{
		ID:          e.ID,
		OccurredAt:  e.OccurredAt.UTC().Format(time.RFC3339),
		Kind:        e.Kind,
		RecordingID: e.RecordingID,
		Summary:     e.Summary,
		Details:     e.Details,
	}
}

// toHistoryPageRow projects a storage.HistoryEvent onto the
// page-row view-model. Renders the details map as pretty JSON so the
// template can drop it straight into a <pre> without further fiddling.
func toHistoryPageRow(e storage.HistoryEvent) historyPageRow {
	row := historyPageRow{
		ID:          e.ID,
		OccurredAt:  e.OccurredAt.UTC().Format(time.RFC3339),
		Kind:        e.Kind,
		RecordingID: e.RecordingID,
		Summary:     e.Summary,
	}
	if len(e.Details) > 0 {
		b, err := json.MarshalIndent(e.Details, "", "  ")
		if err == nil {
			row.HasDetails = true
			row.DetailsJSON = string(b)
		}
	}
	return row
}
