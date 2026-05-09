package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/ingest"
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

// importQueueRequest is the JSON body for POST /api/v1/queue/{id}/import.
// recording_id is optional; when omitted the handler falls back to the
// queue entry's suggested_recording_id (which must be non-nil).
type importQueueRequest struct {
	RecordingID *int64 `json:"recording_id,omitempty"`
}

// importQueueResponse is the JSON body returned by the import endpoint.
// Mirrors a single ingest.ItemResult — `dest` is the moved-to path, or
// the planned path when the engine reports a would-move.
type importQueueResponse struct {
	OK     bool   `json:"ok"`
	Action string `json:"action"`
	Error  string `json:"error,omitempty"`
	Dest   string `json:"dest,omitempty"`
}

// handleImportQueue runs the ingest pipeline for one queued file. The
// queue entry is removed only on a successful move; skipped/would-move
// outcomes leave the row in place so the user can retry. A history event
// (kind=manual_import) is recorded on success so the audit trail
// distinguishes user-driven imports from cli/scanner-driven ones.
func (s *Server) handleImportQueue(c echo.Context) error {
	if s.ingestEngine == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "ingest not configured")
	}

	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid queue id")
	}

	ctx := c.Request().Context()
	entry, err := storage.LoadQueueEntry(ctx, s.db, id)
	if errors.Is(err, storage.ErrQueueEntryNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}
	if err != nil {
		return err
	}

	var body importQueueRequest
	// Empty body is allowed (suggested-id fallback), so ignore EOF; bind
	// failures on a non-empty malformed body are still surfaced as 400.
	if c.Request().ContentLength > 0 {
		if bindErr := c.Bind(&body); bindErr != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid request body")
		}
	}

	recordingID, err := resolveImportRecordingID(body, entry)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	res, err := s.ingestEngine.Ingest(ctx, entry.FilePath, ingest.Options{
		FlagEncoraID: int(recordingID),
	})
	if err != nil {
		return fmt.Errorf("ingest queue entry %d: %w", id, err)
	}
	if res == nil || len(res.Items) == 0 {
		return echo.NewHTTPError(http.StatusInternalServerError, "ingest returned no result")
	}

	item := res.Items[0]
	resp := importQueueResponse{Action: item.Action}
	if item.Plan != nil {
		resp.Dest = item.Plan.AbsoluteFile()
	}
	if item.Err != nil {
		resp.Error = item.Err.Error()
	}
	resp.OK = item.Action == ingest.ActionMoved && item.Err == nil

	if resp.OK {
		// Best-effort cleanup + audit trail. RemoveQueueEntry is
		// idempotent; RecordEvent failures are logged and dropped because
		// the file already moved successfully — losing the audit row is
		// preferable to surfacing a 500 to the user for a cosmetic write.
		if removeErr := storage.RemoveQueueEntry(ctx, s.db, id); removeErr != nil {
			s.logger.Warn().Err(removeErr).Int64("queue_id", id).
				Msg("failed to remove queue entry after import")
		}
		event := storage.HistoryEvent{
			Kind:    storage.HistoryKindManualImport,
			Summary: fmt.Sprintf("Imported queued file %s", entry.FilePath),
			Details: map[string]any{
				"queue_id":  id,
				"source":    entry.FilePath,
				"encora_id": recordingID,
				"dest":      resp.Dest,
				"action":    item.Action,
			},
		}
		rid := recordingID
		event.RecordingID = &rid
		if _, recErr := storage.RecordEvent(ctx, s.db, event); recErr != nil {
			s.logger.Warn().Err(recErr).Int64("queue_id", id).
				Msg("failed to record manual import history event")
		}
	}

	return c.JSON(http.StatusOK, resp)
}

// resolveImportRecordingID picks the recording id the ingest pipeline
// will run with: explicit override from the request body wins, otherwise
// the queue entry's suggested_recording_id (must be non-nil). Returns an
// error suitable for a 400 when neither is available or the override is
// non-positive.
func resolveImportRecordingID(req importQueueRequest, entry *storage.QueueEntry) (int64, error) {
	if req.RecordingID != nil {
		if *req.RecordingID <= 0 {
			return 0, errors.New("recording_id must be positive")
		}
		return *req.RecordingID, nil
	}
	if entry.SuggestedRecordingID == nil {
		return 0, errors.New("no recording_id supplied and queue entry has no suggestion")
	}
	return *entry.SuggestedRecordingID, nil
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
