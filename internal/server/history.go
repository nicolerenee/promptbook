package server

import (
	"net/http"
	"strconv"
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
// Also accepts ?recording_id= to scope the list to a single recording's
// activity timeline; malformed values 400.
func (s *Server) handleListHistory(c echo.Context) error {
	kinds := parseHistoryKindParam(c.QueryParam("kind"))
	limit := paramInt(c, "limit", historyListLimit)
	offset := paramInt(c, "offset", 0)

	opts := storage.ListHistoryOptions{
		Kinds:  kinds,
		Limit:  limit,
		Offset: offset,
	}
	if raw := strings.TrimSpace(c.QueryParam("recording_id")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid recording_id")
		}
		opts.RecordingID = &id
	}

	events, err := storage.ListHistory(c.Request().Context(), s.db, opts)
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

// handleHistoryPage renders the history shell. Data comes from
// /api/v1/history, fetched client-side by /static/history.js.
func (s *Server) handleHistoryPage(c echo.Context) error {
	return c.Render(http.StatusOK, "history.html", shellData{
		Title:     "History",
		ActiveNav: "history",
		Version:   s.version,
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
