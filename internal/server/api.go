package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/storage"
)

// API tunables.
const (
	defaultListLimit = 50
	wantsScanLimit   = 100
	itemsKey         = "items"
	limitKey         = "limit"
	offsetKey        = "offset"
)

// RecordingListItem is the shape returned by /api/v1/recordings and
// embedded in the home page view-model. Status and the per-recording
// format strings come from the storage.RecordingState reconciler so the
// JSON and HTML pages render the same five-status taxonomy.
type RecordingListItem struct {
	ID             int64  `json:"id"`
	Show           string `json:"show"`
	Tour           string `json:"tour"`
	DateFull       string `json:"date_full"`
	DateMonthKnown bool   `json:"date_month_known"`
	DateDayKnown   bool   `json:"date_day_known"`
	Master         string `json:"master"`
	Status         string `json:"status"`
	InCollection   bool   `json:"in_collection"`
	InWants        bool   `json:"in_wants"`
	FileCount      int    `json:"file_count"`
	EncoraFormat   string `json:"encora_format"`
	LocalFormat    string `json:"local_format"`
}

// recordingsListItem is the legacy trimmed shape kept for the deprecated
// loadRecordingsList helper. New code paths should use RecordingListItem.
//
// Deprecated: use RecordingListItem and loadStatefulRecordings instead.
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
	api.GET("/queue", s.handleListQueue)
	api.GET("/people", s.handleListPeople)
	api.GET("/people/:id", s.handleGetPerson)
	api.GET("/history", s.handleListHistory)

	s.echo.GET("/", s.handleHomePage)
	s.echo.GET("/recordings/:id", s.handleRecordingPage)
	s.echo.GET("/wants", s.handleWantsPage)
	s.echo.GET("/sync", s.handleSyncPage)
	s.echo.GET("/queue", s.handleQueuePage)
	s.echo.GET("/people", s.handlePeoplePage)
	s.echo.GET("/people/:id", s.handlePersonPage)
	s.echo.GET("/history", s.handleHistoryPage)
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
	statusParam := c.QueryParam("status")
	owned := c.QueryParam("owned")

	statuses := parseStatusParam(statusParam)

	// status takes precedence over the legacy owned filter when both are
	// supplied; owned is honored only when status is absent.
	ownedFilter := owned
	if statusParam != "" {
		ownedFilter = ""
	}

	items, err := loadStatefulRecordings(
		c.Request().Context(), s.db, statuses, ownedFilter, limit, offset,
	)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{
		itemsKey:  items,
		limitKey:  limit,
		offsetKey: offset,
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

// parseStatusParam splits a comma-separated status query value into a
// slice of storage.Status values. Empty / whitespace tokens are skipped.
// Returns nil for the empty string so callers can distinguish "no filter"
// from "filter to nothing".
func parseStatusParam(raw string) []storage.Status {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]storage.Status, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, storage.Status(p))
	}
	return out
}

// loadStatefulRecordings walks ListStates and decorates each row with the
// recording metadata (show, tour, date, master) needed for the list view.
// When statuses is empty and ownedFilter is set, the result is post-
// filtered by collection membership to preserve the legacy ?owned= API.
func loadStatefulRecordings(
	ctx context.Context,
	db *sql.DB,
	statuses []storage.Status,
	ownedFilter string,
	limit, offset int,
) ([]RecordingListItem, error) {
	// Pull all matching states (ListStates does its own pagination, but
	// we want to apply the legacy owned filter before paginating, so ask
	// for a wide window and slice it ourselves).
	listOpts := storage.ListStatesOptions{Status: statuses, Limit: maxStateScan}
	states, err := storage.ListStates(ctx, db, listOpts)
	if err != nil {
		return nil, fmt.Errorf("list states: %w", err)
	}

	if ownedFilter != "" {
		states = applyOwnedFilter(states, ownedFilter)
	}

	// Page-slice after filtering so callers asking for "page 2 of owned"
	// see the right window.
	total := len(states)
	if offset >= total {
		return []RecordingListItem{}, nil
	}
	end := min(offset+limit, total)
	page := states[offset:end]

	if len(page) == 0 {
		return []RecordingListItem{}, nil
	}

	meta, err := loadRecordingMeta(ctx, db, page)
	if err != nil {
		return nil, err
	}

	out := make([]RecordingListItem, 0, len(page))
	for _, st := range page {
		m := meta[st.RecordingID]
		out = append(out, RecordingListItem{
			ID:             st.RecordingID,
			Show:           m.show,
			Tour:           m.tour,
			DateFull:       m.dateFull,
			DateMonthKnown: m.monthKnown,
			DateDayKnown:   m.dayKnown,
			Master:         m.master,
			Status:         string(st.Status),
			InCollection:   st.InCollection,
			InWants:        st.InWants,
			FileCount:      st.FileCount,
			EncoraFormat:   st.EncoraFormat,
			LocalFormat:    st.LocalFormat,
		})
	}
	return out, nil
}

// maxStateScan caps how many states we materialize before paginating.
// 1024 is well past the ~50-row pages the UI shows but small enough that
// the N+1 LocalFormat lookup inside ListStates stays cheap.
const maxStateScan = 1024

// applyOwnedFilter re-applies the legacy ?owned= filter to a state slice.
// "true" keeps in-collection rows; "false" drops them.
func applyOwnedFilter(states []storage.RecordingState, owned string) []storage.RecordingState {
	switch owned {
	case "true":
		out := make([]storage.RecordingState, 0, len(states))
		for _, st := range states {
			if st.InCollection {
				out = append(out, st)
			}
		}
		return out
	case "false":
		out := make([]storage.RecordingState, 0, len(states))
		for _, st := range states {
			if !st.InCollection {
				out = append(out, st)
			}
		}
		return out
	default:
		return states
	}
}

// recordingMeta is the per-row recording display metadata loaded
// alongside a RecordingState.
type recordingMeta struct {
	show       string
	tour       string
	dateFull   string
	monthKnown bool
	dayKnown   bool
	master     string
}

// loadRecordingMeta resolves show/tour/date/master for a slice of
// states in a single SQL query keyed on recording_id. Recordings that
// don't exist in the recordings table (orphaned versions sans payload)
// surface as the zero value.
func loadRecordingMeta(
	ctx context.Context,
	db *sql.DB,
	states []storage.RecordingState,
) (map[int64]recordingMeta, error) {
	if len(states) == 0 {
		return map[int64]recordingMeta{}, nil
	}
	placeholders := make([]string, len(states))
	args := make([]any, len(states))
	for i, st := range states {
		placeholders[i] = "?"
		args[i] = st.RecordingID
	}
	// Concatenated placeholders are bind-parameter markers (no
	// user-controlled SQL); the recording_id args are passed via
	// QueryContext.
	//nolint:gosec // G202: placeholders are "?" markers, not user input.
	q := `
		SELECT r.recording_id, COALESCE(s.name, ''), r.tour, r.date_full,
		       r.date_month_known, r.date_day_known, r.master
		FROM recordings r
		LEFT JOIN shows s ON s.show_id = r.show_id
		WHERE r.recording_id IN (` + strings.Join(placeholders, ",") + `)
	`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query recording meta: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[int64]recordingMeta, len(states))
	for rows.Next() {
		var (
			id                   int64
			m                    recordingMeta
			monthKnown, dayKnown int
		)
		if scanErr := rows.Scan(
			&id, &m.show, &m.tour, &m.dateFull,
			&monthKnown, &dayKnown, &m.master,
		); scanErr != nil {
			return nil, fmt.Errorf("scan recording meta: %w", scanErr)
		}
		m.monthKnown = monthKnown == 1
		m.dayKnown = dayKnown == 1
		out[id] = m
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate recording meta: %w", rerr)
	}
	return out, nil
}

// loadRecordingsList queries recordings + collection/wants membership in
// one go. owned filter values: "" / "any" / "true" / "false".
//
// Deprecated: this is the legacy shape used by /api/v1/wants. New code
// should use loadStatefulRecordings, which exposes the RecordingState
// reconciler output.
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
