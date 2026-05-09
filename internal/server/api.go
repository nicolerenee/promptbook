package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/storage"
)

// API tunables.
const (
	defaultListLimit = 50
	wantsScanLimit   = 100
	itemsKey         = "items"
)

// recordingsListItem is the trimmed shape returned by /api/v1/recordings.
// Detail callers should hit /api/v1/recordings/{id} for the full payload.
type recordingsListItem struct {
	ID             int64  `json:"id"`
	Show           string `json:"show"`
	Tour           string `json:"tour"`
	DateFull       string `json:"date_full"`
	DateMonthKnown bool   `json:"date_month_known"`
	DateDayKnown   bool   `json:"date_day_known"`
	Master         string `json:"master"`
	InCollection   bool   `json:"in_collection"`
	InWants        bool   `json:"in_wants"`
}

func (s *Server) routes() {
	api := s.echo.Group("/api/v1")
	api.GET("/health", s.handleHealth)
	api.GET("/profile", s.handleProfile)
	api.GET("/recordings", s.handleListRecordings)
	api.GET("/recordings/:id", s.handleGetRecording)
	api.GET("/wants", s.handleListWants)
	api.GET("/sync/runs", s.handleSyncRuns)

	s.echo.GET("/", s.handleHomePage)
	s.echo.GET("/recordings/:id", s.handleRecordingPage)
	s.echo.GET("/wants", s.handleWantsPage)
	s.echo.GET("/sync", s.handleSyncPage)
}

func (s *Server) handleHealth(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}

// handleProfile surfaces the cached /api/profile buttonshot that sync writes.
// Responds 404 if no sync has populated the row yet so the UI can prompt
// the user to run `promptbook collection sync`.
func (s *Server) handleProfile(c echo.Context) error {
	p, err := storage.LoadProfile(c.Request().Context(), s.db)
	if errors.Is(err, storage.ErrProfileNotSynced) {
		return echo.NewHTTPError(http.StatusNotFound, "profile not synced yet")
	}
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, p)
}

func (s *Server) handleListRecordings(c echo.Context) error {
	limit := paramInt(c, "limit", defaultListLimit)
	offset := paramInt(c, "offset", 0)
	owned := c.QueryParam("owned")

	items, err := loadRecordingsList(c.Request().Context(), s.db, limit, offset, owned)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{
		itemsKey: items,
		"limit":  limit,
		"offset": offset,
	})
}

func (s *Server) handleGetRecording(c echo.Context) error {
	id, err := storage.ParseRecordingID(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	loaded, err := storage.LoadRecording(c.Request().Context(), s.db, id)
	if errors.Is(err, storage.ErrRecordingNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, loaded)
}

func (s *Server) handleListWants(c echo.Context) error {
	items, err := loadRecordingsList(c.Request().Context(), s.db, wantsScanLimit, 0, "false")
	if err != nil {
		return err
	}
	wants := make([]recordingsListItem, 0, len(items))
	for _, item := range items {
		if item.InWants {
			wants = append(wants, item)
		}
	}
	return c.JSON(http.StatusOK, map[string]any{itemsKey: wants})
}

func (s *Server) handleSyncRuns(c echo.Context) error {
	rows, err := s.db.QueryContext(c.Request().Context(), `
		SELECT id, kind, started_at, finished_at, ok_count, error_count,
		       rate_limit_remaining, error_text
		FROM sync_runs
		ORDER BY id DESC
		LIMIT 30
	`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	type runRow struct {
		ID                 int64   `json:"id"`
		Kind               string  `json:"kind"`
		StartedAt          string  `json:"started_at"`
		FinishedAt         *string `json:"finished_at"`
		OkCount            int     `json:"ok_count"`
		ErrorCount         int     `json:"error_count"`
		RateLimitRemaining int     `json:"rate_limit_remaining"`
		ErrorText          string  `json:"error_text"`
	}

	var out []runRow
	for rows.Next() {
		var row runRow
		var finishedAt sql.NullString
		if scanErr := rows.Scan(&row.ID, &row.Kind, &row.StartedAt, &finishedAt,
			&row.OkCount, &row.ErrorCount, &row.RateLimitRemaining, &row.ErrorText); scanErr != nil {
			return scanErr
		}
		if finishedAt.Valid {
			s := finishedAt.String
			row.FinishedAt = &s
		}
		out = append(out, row)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return rowsErr
	}
	return c.JSON(http.StatusOK, map[string]any{itemsKey: out})
}

// loadRecordingsList queries recordings + collection/wants membership in
// one go. owned filter values: "" / "any" / "true" / "false".
func loadRecordingsList(
	ctx context.Context,
	db *sql.DB,
	limit, offset int,
	owned string,
) ([]recordingsListItem, error) {
	q := `
		SELECT
			r.recording_id, s.name, r.tour, r.date_full,
			r.date_month_known, r.date_day_known, r.master,
			c.recording_id IS NOT NULL AS in_collection,
			w.recording_id IS NOT NULL AS in_wants
		FROM recordings r
		LEFT JOIN shows s ON s.show_id = r.show_id
		LEFT JOIN collection c ON c.recording_id = r.recording_id
		LEFT JOIN wants w ON w.recording_id = r.recording_id
	`
	switch owned {
	case "true":
		q += `WHERE c.recording_id IS NOT NULL `
	case "false":
		q += `WHERE c.recording_id IS NULL `
	}
	q += `ORDER BY s.name, r.tour, r.date_full LIMIT ? OFFSET ?`

	rows, err := db.QueryContext(ctx, q, limit, offset)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]recordingsListItem, 0)
	for rows.Next() {
		var item recordingsListItem
		var monthKnown, dayKnown int
		var inColl, inWants int
		if scanErr := rows.Scan(&item.ID, &item.Show, &item.Tour, &item.DateFull,
			&monthKnown, &dayKnown, &item.Master, &inColl, &inWants); scanErr != nil {
			return nil, scanErr
		}
		item.DateMonthKnown = monthKnown == 1
		item.DateDayKnown = dayKnown == 1
		item.InCollection = inColl == 1
		item.InWants = inWants == 1
		out = append(out, item)
	}
	return out, rows.Err()
}

func paramInt(c echo.Context, name string, fallback int) int {
	v := c.QueryParam(name)
	if v == "" {
		return fallback
	}
	var n int
	if err := json.Unmarshal([]byte(v), &n); err != nil {
		return fallback
	}
	if n < 0 {
		return fallback
	}
	return n
}
