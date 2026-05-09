package server

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/storage"
)

// pageScanLimit caps how many recordings the home/wants pages will
// fetch in one go. Higher than the API default since pages render the
// whole list inline.
const pageScanLimit = 200

type homeData struct {
	Title       string
	Recordings  []recordingsListItem
	OwnedFilter string
}

func (s *Server) handleHomePage(c echo.Context) error {
	owned := c.QueryParam("owned")
	if owned == "" {
		owned = "true"
	}
	items, err := loadRecordingsList(c.Request().Context(), s.db, pageScanLimit, 0, owned)
	if err != nil {
		return err
	}
	return c.Render(http.StatusOK, "home.html", homeData{
		Title:       "Promptbook",
		Recordings:  items,
		OwnedFilter: owned,
	})
}

type recordingPageData struct {
	Title     string
	Recording *storage.LoadedRecording
}

func (s *Server) handleRecordingPage(c echo.Context) error {
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
	return c.Render(http.StatusOK, "recording.html", recordingPageData{
		Title:     loaded.Recording.Show + " — " + loaded.Recording.Tour,
		Recording: loaded,
	})
}

func (s *Server) handleWantsPage(c echo.Context) error {
	items, err := loadRecordingsList(c.Request().Context(), s.db, pageScanLimit, 0, "false")
	if err != nil {
		return err
	}
	wants := make([]recordingsListItem, 0, len(items))
	for _, item := range items {
		if item.InWants {
			wants = append(wants, item)
		}
	}
	return c.Render(http.StatusOK, "wants.html", homeData{
		Title:      "Wants",
		Recordings: wants,
	})
}

type syncPageData struct {
	Title string
	Runs  []syncRunSummary
}

type syncRunSummary struct {
	ID                 int64
	Kind               string
	StartedAt          string
	FinishedAt         string
	OkCount            int
	ErrorCount         int
	RateLimitRemaining int
	ErrorText          string
}

func (s *Server) handleSyncPage(c echo.Context) error {
	rows, err := s.db.QueryContext(c.Request().Context(), `
		SELECT id, kind, started_at, COALESCE(finished_at, ''), ok_count, error_count,
		       rate_limit_remaining, error_text
		FROM sync_runs
		ORDER BY id DESC
		LIMIT 30
	`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	var runs []syncRunSummary
	for rows.Next() {
		var r syncRunSummary
		if scanErr := rows.Scan(&r.ID, &r.Kind, &r.StartedAt, &r.FinishedAt,
			&r.OkCount, &r.ErrorCount, &r.RateLimitRemaining, &r.ErrorText); scanErr != nil {
			return scanErr
		}
		runs = append(runs, r)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return rowsErr
	}
	return c.Render(http.StatusOK, "sync.html", syncPageData{
		Title: "Sync runs",
		Runs:  runs,
	})
}
