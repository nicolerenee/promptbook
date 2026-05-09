package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/storage"
)

// pageScanLimit caps how many recordings the home/wants pages will
// fetch in one go. Higher than the API default since pages render the
// whole list inline.
const pageScanLimit = 200

// homeStatuses lists the status filter tabs the home page renders, in
// display order. The empty status is "All" — the canonical "no filter"
// link in the nav.
//
//nolint:gochecknoglobals // immutable display-order lookup table.
var homeStatuses = []struct {
	Key   storage.Status
	Label string
}{
	{Key: "", Label: "All"},
	{Key: storage.StatusSynced, Label: "Synced"},
	{Key: storage.StatusFormatMismatch, Label: "Format mismatch"},
	{Key: storage.StatusMissing, Label: "Missing"},
	{Key: storage.StatusWanted, Label: "Wanted"},
	{Key: storage.StatusOrphan, Label: "Orphan"},
}

type homeStatusTab struct {
	Key    string
	Label  string
	Count  int
	Active bool
}

type homeData struct {
	Title        string
	Recordings   []RecordingListItem
	ActiveStatus string
	Counts       map[string]int
	Tabs         []homeStatusTab
}

func (s *Server) handleHomePage(c echo.Context) error {
	statusParam := c.QueryParam("status")
	statuses := parseStatusParam(statusParam)

	ctx := c.Request().Context()
	items, err := loadStatefulRecordings(ctx, s.db, statuses, "", pageScanLimit, 0)
	if err != nil {
		return err
	}

	counts, err := computeStatusCounts(ctx, s.db)
	if err != nil {
		return err
	}

	tabs := make([]homeStatusTab, 0, len(homeStatuses))
	totalAll := 0
	for _, c := range counts {
		totalAll += c
	}
	for _, hs := range homeStatuses {
		key := string(hs.Key)
		count := counts[key]
		if hs.Key == "" {
			count = totalAll
		}
		tabs = append(tabs, homeStatusTab{
			Key:    key,
			Label:  hs.Label,
			Count:  count,
			Active: key == statusParam,
		})
	}

	return c.Render(http.StatusOK, "home.html", homeData{
		Title:        "Promptbook",
		Recordings:   items,
		ActiveStatus: statusParam,
		Counts:       counts,
		Tabs:         tabs,
	})
}

// computeStatusCounts returns a map from status key to the count of
// recordings currently reporting that status. Uses ListStates so the
// numbers match what the table renders.
func computeStatusCounts(ctx context.Context, db *sql.DB) (map[string]int, error) {
	all, err := storage.ListStates(ctx, db, storage.ListStatesOptions{Limit: maxStateScan})
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int, len(homeStatuses))
	for _, st := range all {
		counts[string(st.Status)]++
	}
	return counts, nil
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

// wantsPageData is the view-model for /wants. Kept separate from
// homeData so the home-page status tabs don't bleed into the wants
// template.
type wantsPageData struct {
	Title      string
	Recordings []recordingsListItem
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
	return c.Render(http.StatusOK, "wants.html", wantsPageData{
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
