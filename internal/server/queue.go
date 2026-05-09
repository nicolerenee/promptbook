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

// handleQueuePage renders the queue shell. Data comes from
// /api/v1/queue, fetched client-side by /static/queue.js.
func (s *Server) handleQueuePage(c echo.Context) error {
	return c.Render(http.StatusOK, "queue.html", shellData{
		Title:     "Queue",
		ActiveNav: "queue",
		Version:   s.version,
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
