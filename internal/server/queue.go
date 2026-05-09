package server

import (
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/storage"
)

// queueItem is the JSON shape returned by /api/v1/queue. Mirrors
// storage.QueueEntry but encodes timestamps as RFC3339 strings and
// surfaces the optional suggested recording id as a nullable int.
type queueItem struct {
	ID                   int64  `json:"id"`
	FilePath             string `json:"file_path"`
	FileSizeBytes        int64  `json:"file_size_bytes"`
	DiscoveredAt         string `json:"discovered_at"`
	LastSeenAt           string `json:"last_seen_at"`
	SuggestedRecordingID *int64 `json:"suggested_recording_id"`
	SuggestedConfidence  string `json:"suggested_confidence"`
	Notes                string `json:"notes"`
}

// handleListQueue returns every row in the manual_import_queue ordered
// oldest-first. Empty queue still returns {items: []} so the client can
// range over it safely.
func (s *Server) handleListQueue(c echo.Context) error {
	entries, err := storage.ListQueue(c.Request().Context(), s.db)
	if err != nil {
		return err
	}
	items := make([]queueItem, 0, len(entries))
	for _, e := range entries {
		items = append(items, toQueueItem(e))
	}
	return c.JSON(http.StatusOK, map[string]any{itemsKey: items})
}

// queuePageRow is the per-row view-model the queue.html template
// renders. DiscoveredAt is pre-formatted to keep the template logic-free.
type queuePageRow struct {
	ID                   int64
	FilePath             string
	FileSizeBytes        int64
	DiscoveredAt         string
	LastSeenAt           string
	SuggestedRecordingID *int64
	SuggestedConfidence  string
	Notes                string
}

type queuePageData struct {
	Title   string
	Entries []queuePageRow
}

// handleQueuePage renders the manual import queue as an HTML table.
// Read-only for now — the resolve-and-import POST flow lands in a later
// scope.
func (s *Server) handleQueuePage(c echo.Context) error {
	entries, err := storage.ListQueue(c.Request().Context(), s.db)
	if err != nil {
		return err
	}
	rows := make([]queuePageRow, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, queuePageRow{
			ID:                   e.ID,
			FilePath:             e.FilePath,
			FileSizeBytes:        e.FileSizeBytes,
			DiscoveredAt:         e.DiscoveredAt.Format(time.RFC3339),
			LastSeenAt:           e.LastSeenAt.Format(time.RFC3339),
			SuggestedRecordingID: e.SuggestedRecordingID,
			SuggestedConfidence:  e.SuggestedConfidence,
			Notes:                e.Notes,
		})
	}
	return c.Render(http.StatusOK, "queue.html", queuePageData{
		Title:   "Manual import queue",
		Entries: rows,
	})
}

// toQueueItem projects a storage.QueueEntry onto the JSON shape used by
// the /api/v1/queue endpoint.
func toQueueItem(e storage.QueueEntry) queueItem {
	return queueItem{
		ID:                   e.ID,
		FilePath:             e.FilePath,
		FileSizeBytes:        e.FileSizeBytes,
		DiscoveredAt:         e.DiscoveredAt.Format(time.RFC3339),
		LastSeenAt:           e.LastSeenAt.Format(time.RFC3339),
		SuggestedRecordingID: e.SuggestedRecordingID,
		SuggestedConfidence:  e.SuggestedConfidence,
		Notes:                e.Notes,
	}
}
