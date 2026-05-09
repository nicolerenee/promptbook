package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/storage"
)

// stagemediaImageTimeout caps how long the people detail page waits on
// the StageMedia.me API before giving up and rendering without a
// headshot. The page must remain useful even when the image side-call
// is slow, errors, or returns nothing.
const stagemediaImageTimeout = 5 * time.Second

// peopleListLimit is the default limit applied to /api/v1/people when
// the caller doesn't pass ?limit=. The list is short enough that the
// home-grown 50/page cadence used elsewhere is plenty.
const peopleListLimit = 50

// PersonListItem is one row in the JSON list of people that appear in
// the user's library. RecordingCount counts distinct recordings the
// performer is credited on, scoped to recordings the user owns OR
// wants.
type PersonListItem struct {
	PerformerID    int64  `json:"performer_id"`
	Name           string `json:"name"`
	Slug           string `json:"slug"`
	RecordingCount int    `json:"recording_count"`
}

// PersonRecording is a recording credit on the people-detail JSON. The
// shape mirrors what the home/recordings list pages use so the same
// smartDate template helper formats it on the HTML side.
type PersonRecording struct {
	ID             int64  `json:"id"`
	Show           string `json:"show"`
	Tour           string `json:"tour"`
	DateFull       string `json:"date_full"`
	DateMonthKnown bool   `json:"date_month_known"`
	DateDayKnown   bool   `json:"date_day_known"`
	ShowID         int64  `json:"show_id"`
}

// PersonDetail is the JSON payload for /api/v1/people/{id}. Headshot
// is optional — populated only when a stagemedia client is configured
// AND the API call succeeds within the timeout.
type PersonDetail struct {
	PerformerID int64             `json:"performer_id"`
	Name        string            `json:"name"`
	Slug        string            `json:"slug"`
	URL         string            `json:"url"`
	Recordings  []PersonRecording `json:"recordings"`
	HeadshotURL string            `json:"headshot_url,omitempty"`
}

func (s *Server) handleListPeople(c echo.Context) error {
	limit := paramInt(c, "limit", peopleListLimit)
	offset := paramInt(c, "offset", 0)

	items, err := loadPeopleList(c.Request().Context(), s.db, limit, offset)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{
		itemsKey: items,
		"limit":  limit,
		"offset": offset,
	})
}

func (s *Server) handleGetPerson(c echo.Context) error {
	id, err := parsePerformerID(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	ctx := c.Request().Context()

	detail, err := loadPersonDetail(ctx, s.db, id)
	if errors.Is(err, storage.ErrPerformerNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}
	if err != nil {
		return err
	}

	if url := s.fetchHeadshot(ctx, detail); url != "" {
		detail.HeadshotURL = url
	}

	return c.JSON(http.StatusOK, detail)
}

// peoplePageData is the view-model for /people.
type peoplePageData struct {
	Title  string
	People []PersonListItem
}

func (s *Server) handlePeoplePage(c echo.Context) error {
	items, err := loadPeopleList(c.Request().Context(), s.db, pageScanLimit, 0)
	if err != nil {
		return err
	}
	return c.Render(http.StatusOK, "people.html", peoplePageData{
		Title:  "People",
		People: items,
	})
}

// personPageData is the view-model for /people/{id}.
type personPageData struct {
	Title       string
	PerformerID int64
	Name        string
	URL         string
	Recordings  []PersonRecording
	HeadshotURL string
}

func (s *Server) handlePersonPage(c echo.Context) error {
	id, err := parsePerformerID(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	ctx := c.Request().Context()

	detail, err := loadPersonDetail(ctx, s.db, id)
	if errors.Is(err, storage.ErrPerformerNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}
	if err != nil {
		return err
	}

	headshot := s.fetchHeadshot(ctx, detail)
	return c.Render(http.StatusOK, "person.html", personPageData{
		Title:       detail.Name,
		PerformerID: detail.PerformerID,
		Name:        detail.Name,
		URL:         detail.URL,
		Recordings:  detail.Recordings,
		HeadshotURL: headshot,
	})
}

// parsePerformerID validates a CLI-style positive int id from the path.
func parsePerformerID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse performer id %q: %w", s, err)
	}
	if id <= 0 {
		return 0, fmt.Errorf("invalid performer id %d", id)
	}
	return id, nil
}

// loadPeopleList returns performers that appear on at least one cast
// entry tied to a recording in either the collection or wants table.
// Sorted by name ascending so the page is alphabetical.
func loadPeopleList(
	ctx context.Context,
	db *sql.DB,
	limit, offset int,
) ([]PersonListItem, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT p.performer_id, p.name, p.slug, COUNT(DISTINCT ce.recording_id) AS rcount
		FROM performers p
		JOIN cast_entries ce ON ce.performer_id = p.performer_id
		WHERE ce.recording_id IN (
			SELECT recording_id FROM collection
			UNION
			SELECT recording_id FROM wants
		)
		GROUP BY p.performer_id, p.name, p.slug
		ORDER BY p.name ASC, p.performer_id ASC
		LIMIT ? OFFSET ?
	`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("query people list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]PersonListItem, 0)
	for rows.Next() {
		var item PersonListItem
		if scanErr := rows.Scan(&item.PerformerID, &item.Name, &item.Slug, &item.RecordingCount); scanErr != nil {
			return nil, fmt.Errorf("scan people row: %w", scanErr)
		}
		out = append(out, item)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate people rows: %w", rerr)
	}
	return out, nil
}

// loadPersonDetail loads the performer + the recordings they appear in.
// Returns ErrPerformerNotFound when no performers row matches.
func loadPersonDetail(
	ctx context.Context,
	db *sql.DB,
	id int64,
) (*PersonDetail, error) {
	p, err := storage.LoadPerformer(ctx, db, id)
	if err != nil {
		return nil, err
	}

	recIDs, err := storage.ListRecordingsForPerformer(ctx, db, id)
	if err != nil {
		return nil, fmt.Errorf("list recordings for performer %d: %w", id, err)
	}

	recs, err := loadPersonRecordings(ctx, db, recIDs)
	if err != nil {
		return nil, err
	}

	return &PersonDetail{
		PerformerID: p.PerformerID,
		Name:        p.Name,
		Slug:        p.Slug,
		URL:         p.URL,
		Recordings:  recs,
	}, nil
}

// loadPersonRecordings resolves a slice of recording ids into the
// PersonRecording shape used by the JSON + HTML detail views. Sorted
// by show name, tour, date so the table reads alphabetically.
func loadPersonRecordings(
	ctx context.Context,
	db *sql.DB,
	ids []int64,
) ([]PersonRecording, error) {
	if len(ids) == 0 {
		return []PersonRecording{}, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	// placeholders are bind markers, not user input.
	//nolint:gosec // G202: ? placeholders, recording ids are passed as args.
	q := `
		SELECT r.recording_id, COALESCE(s.name, ''), r.tour, r.date_full,
		       r.date_month_known, r.date_day_known, r.show_id
		FROM recordings r
		LEFT JOIN shows s ON s.show_id = r.show_id
		WHERE r.recording_id IN (` + strings.Join(placeholders, ",") + `)
		ORDER BY s.name, r.tour, r.date_full
	`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query person recordings: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]PersonRecording, 0, len(ids))
	for rows.Next() {
		var r PersonRecording
		var monthKnown, dayKnown int
		if scanErr := rows.Scan(
			&r.ID, &r.Show, &r.Tour, &r.DateFull,
			&monthKnown, &dayKnown, &r.ShowID,
		); scanErr != nil {
			return nil, fmt.Errorf("scan person recording: %w", scanErr)
		}
		r.DateMonthKnown = monthKnown == 1
		r.DateDayKnown = dayKnown == 1
		out = append(out, r)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate person recordings: %w", rerr)
	}
	return out, nil
}

// fetchHeadshot consults stagemedia for a single performer, scoped to
// the show id of the first recording in the detail. Returns "" when no
// stagemedia client is configured, when the call errors, when it times
// out, or when stagemedia has no headshot for the performer. The page
// must always render — image side-quests never block the response.
func (s *Server) fetchHeadshot(ctx context.Context, detail *PersonDetail) string {
	client := s.Stagemedia()
	if client == nil || detail == nil || len(detail.Recordings) == 0 {
		return ""
	}
	showID := detail.Recordings[0].ShowID
	if showID == 0 {
		return ""
	}

	imgCtx, cancel := context.WithTimeout(ctx, stagemediaImageTimeout)
	defer cancel()

	imgs, err := client.Images(imgCtx, showID, []int64{detail.PerformerID})
	if err != nil {
		s.logger.Debug().
			Err(err).
			Int64("performer_id", detail.PerformerID).
			Int64("show_id", showID).
			Msg("stagemedia headshot fetch failed")
		return ""
	}
	for _, p := range imgs.Performers {
		if p.ID == detail.PerformerID && p.URL != "" {
			return p.URL
		}
	}
	return ""
}
