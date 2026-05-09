package server

// shows.go — JSON API surface for show-level views.
//
// Three endpoints registered here:
//   - GET  /api/v1/shows              (list — by-show aggregate)
//   - GET  /api/v1/shows/:id          (show detail page payload)
//   - POST /api/v1/shows/:id/poster   (per-show poster picker)
//
// All three routes are registered together in api.go's routes(). Show
// poster choice persists in the show_image_choices table — no overlay
// text on shows (only recordings get burned-in labels).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/storage"
)

// ShowListItem is one row in the JSON list returned by
// GET /api/v1/shows. Counts and state buckets are computed from the
// recordings + collection + wants tables via the same reconciler the
// recordings list uses, so the by-show view agrees with the per-row
// status taxonomy.
//
// FirstYear / LastYear are nil when the show has no dated recordings
// (every recording's date_full is empty / unparseable). LocalPosterURL
// is empty when image caching is off or no poster is on disk for the
// show's curated index yet.
type ShowListItem struct {
	ID             int64          `json:"id"`
	Name           string         `json:"name"`
	RecordingCount int            `json:"recording_count"`
	StateCounts    map[string]int `json:"state_counts"`
	FirstYear      *int           `json:"first_year"`
	LastYear       *int           `json:"last_year"`
	LocalPosterURL string         `json:"local_poster_url"`
}

// handleListShows aggregates every recording into its show and returns
// a paginated, sorted slice with state-count buckets and year span.
// Sort happens server-side over the in-memory aggregate — the catalog
// is bounded by maxStateScan + the shows table size so this stays
// fast — and pagination then trims the slice to the requested page.
func (s *Server) handleListShows(c echo.Context) error {
	ctx := c.Request().Context()
	limit := paramInt(c, "limit", defaultListLimit)
	offset := paramInt(c, "offset", 0)
	sortKey := c.QueryParam("sort")
	dir := parseSortDir(c)

	// Pull every reconciled state in a single pass, then group by show.
	// maxStateScan caps the result so a runaway library doesn't blow
	// memory; the catalog ceiling is well below this in practice.
	states, err := storage.ListStates(ctx, s.db, storage.ListStatesOptions{
		Limit: maxStateScan,
	})
	if err != nil {
		return fmt.Errorf("list states: %w", err)
	}

	meta, err := loadRecordingMeta(ctx, s.db, states)
	if err != nil {
		return err
	}

	// Pre-load every show row so the response includes shows that have
	// zero recordings reconciled (rare — a show without any recording
	// shouldn't reach the catalog — but keeping the pre-load makes the
	// "no recordings yet" case render gracefully when it does).
	shows, err := loadAllShows(ctx, s.db)
	if err != nil {
		return err
	}

	items := aggregateShows(states, meta, shows)

	// Decorate posters once we have the full show set so the cache
	// query bulk-loads the same way the recordings handler does.
	if posterErr := s.decorateShowPosters(ctx, items); posterErr != nil {
		s.logger.Warn().Err(posterErr).Msg("decorate show poster urls failed; serving without")
	}

	sortShows(items, sortKey, dir)

	total := len(items)
	page := paginateShows(items, limit, offset)

	return c.JSON(http.StatusOK, pageEnvelope(page, total, limit, offset))
}

// paginateShows returns the requested window of items, or an empty
// slice when offset is past the end. Lifted out of handleListShows so
// the slicing math stays out of the handler.
func paginateShows(items []ShowListItem, limit, offset int) []ShowListItem {
	total := len(items)
	if offset >= total {
		return []ShowListItem{}
	}
	end := min(offset+limit, total)
	return items[offset:end]
}

// loadAllShows returns {show_id: name} for every row in the shows
// table. Used as the seed map so a show with no reconciled recordings
// still surfaces in the by-show list.
func loadAllShows(ctx context.Context, db *sql.DB) (map[int64]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT show_id, name FROM shows`)
	if err != nil {
		return nil, fmt.Errorf("query shows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[int64]string)
	for rows.Next() {
		var (
			id   int64
			name string
		)
		if scanErr := rows.Scan(&id, &name); scanErr != nil {
			return nil, fmt.Errorf("scan show row: %w", scanErr)
		}
		out[id] = name
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate show rows: %w", rerr)
	}
	return out, nil
}

// aggregateShows groups the per-recording states + meta by show id and
// returns one ShowListItem per show. State counts are bucketed on the
// lowercase storage.Status string so the JSON keys match the badge
// vocabulary the frontend already keys off. Years are pulled from the
// first 4 chars of date_full when present.
func aggregateShows(
	states []storage.RecordingState,
	meta map[int64]recordingMeta,
	shows map[int64]string,
) []ShowListItem {
	type acc struct {
		name   string
		count  int
		states map[string]int
		first  *int
		last   *int
	}
	groups := make(map[int64]*acc, len(shows))

	// Seed every known show so zero-recording shows render with empty
	// buckets rather than disappearing.
	for id, name := range shows {
		groups[id] = &acc{name: name, states: map[string]int{}}
	}

	for _, st := range states {
		m := meta[st.RecordingID]
		showID := m.showID
		if showID == 0 {
			continue
		}
		g, exists := groups[showID]
		if !exists {
			g = &acc{name: m.show, states: map[string]int{}}
			groups[showID] = g
		}
		if g.name == "" {
			g.name = m.show
		}
		g.count++
		g.states[string(st.Status)]++
		if y, parsed := parseYear(m.dateFull); parsed {
			if g.first == nil || y < *g.first {
				yy := y
				g.first = &yy
			}
			if g.last == nil || y > *g.last {
				yy := y
				g.last = &yy
			}
		}
	}

	items := make([]ShowListItem, 0, len(groups))
	for id, g := range groups {
		items = append(items, ShowListItem{
			ID:             id,
			Name:           g.name,
			RecordingCount: g.count,
			StateCounts:    g.states,
			FirstYear:      g.first,
			LastYear:       g.last,
		})
	}
	return items
}

// yearPrefixLen is the number of leading characters of date_full
// parseYear inspects — every Encora date_full starts with YYYY.
const yearPrefixLen = 4

// minYear / maxYear are the inclusive sanity bounds parseYear accepts.
// Anything outside this range is treated as an unparseable date so
// FirstYear/LastYear stay clean rather than echoing garbage.
const (
	minYear = 1000
	maxYear = 9999
)

// parseYear extracts a 4-digit year from the leading characters of
// date_full. Returns false on anything that doesn't parse cleanly so
// callers can leave FirstYear/LastYear nil for that recording.
func parseYear(dateFull string) (int, bool) {
	if len(dateFull) < yearPrefixLen {
		return 0, false
	}
	y, err := strconv.Atoi(dateFull[:yearPrefixLen])
	if err != nil || y < minYear || y > maxYear {
		return 0, false
	}
	return y, true
}

// decorateShowPosters bulk-loads show_image_choices for every show in
// items and sets LocalPosterURL on the rows that map to a cached
// poster. Quiet no-op when the cache is disabled.
func (s *Server) decorateShowPosters(ctx context.Context, items []ShowListItem) error {
	cache := s.ImageCache()
	if cache == nil || cache.Disabled() || len(items) == 0 {
		return nil
	}
	showIDs := make(map[int64]struct{}, len(items))
	for _, it := range items {
		if it.ID != 0 {
			showIDs[it.ID] = struct{}{}
		}
	}
	choices, err := loadShowPosterChoices(ctx, s.db, showIDs)
	if err != nil {
		return err
	}
	for i := range items {
		showID := items[i].ID
		if showID == 0 {
			continue
		}
		idx := choices[showID]
		items[i].LocalPosterURL = cache.PosterURL(showID, idx)
	}
	return nil
}

// sortShows orders items in place by the supplied (sortKey, dir).
// Unknown keys fall back to name asc — the canonical default that
// keeps the API ETag-friendly. Ties are broken by id asc so equal
// keys read deterministically across pages.
func sortShows(items []ShowListItem, sortKey string, dir sortDir) {
	cmp := showsCompareFn(sortKey)
	desc := dir == sortDesc
	sort.SliceStable(items, func(i, j int) bool {
		c := cmp(items[i], items[j])
		if desc {
			c = -c
		}
		if c != 0 {
			return c < 0
		}
		return items[i].ID < items[j].ID
	})
}

// showsCompareFn maps a sort key to the comparator that drives
// sortShows. Unknown keys fall back to name. Year columns coerce nil
// to zero so a show without dated recordings pins to the bottom of an
// asc sort — same trick the legacy client-side sort applied.
func showsCompareFn(key string) func(a, b ShowListItem) int {
	switch key {
	case "recording_count":
		return func(a, b ShowListItem) int { return cmpInt(a.RecordingCount, b.RecordingCount) }
	case "first_year":
		return func(a, b ShowListItem) int { return cmpInt(intOrZero(a.FirstYear), intOrZero(b.FirstYear)) }
	case "last_year":
		return func(a, b ShowListItem) int { return cmpInt(intOrZero(a.LastYear), intOrZero(b.LastYear)) }
	default: // "name" or unknown.
		return func(a, b ShowListItem) int { return cmpString(a.Name, b.Name) }
	}
}

// intOrZero dereferences a nullable int pointer for comparator use.
func intOrZero(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// ShowDetailResponse is the wire shape for GET /api/v1/shows/:id.
//
// Description is sourced from the first non-empty
// recordings.show_description across the show's recordings (every
// recording for a show carries the same value upstream). The field is
// omitted entirely when no recording ships a description.
//
// FirstYear / LastYear are derived from the recordings' parsed date
// years; nil when no recording in the set parses to a year.
//
// LocalPosterURLs enumerates every cached poster for the show in
// index order (0..CountPosters). Empty slice when the image cache is
// disabled or empty so the JSON renders [] rather than null.
//
// SelectedPosterIndex carries the user's curated pick from
// show_image_choices. nil when no pick is saved — the SPA falls back
// to highlighting index 0 to match ShowImageChoice.ResolvePoster.
type ShowDetailResponse struct {
	ID                  int64               `json:"id"`
	Name                string              `json:"name"`
	Description         string              `json:"description,omitempty"`
	RecordingCount      int                 `json:"recording_count"`
	FirstYear           *int                `json:"first_year"`
	LastYear            *int                `json:"last_year"`
	StateCounts         map[string]int      `json:"state_counts"`
	Recordings          []RecordingListItem `json:"recordings"`
	LocalPosterURLs     []string            `json:"local_poster_urls"`
	SelectedPosterIndex *int                `json:"selected_poster_index"`
}

// showPosterChoiceRequest is the JSON body for
// POST /api/v1/shows/:id/poster. Mirrors posterChoiceRequest in shape;
// kept as a separate type so a future divergence (e.g. allow null to
// clear) doesn't accidentally affect the per-recording picker.
type showPosterChoiceRequest struct {
	Index int `json:"index"`
}

// handleGetShow returns the show detail payload. 404 when no
// recordings reference the show id (or no shows row exists).
func (s *Server) handleGetShow(c echo.Context) error {
	id, err := parseShowID(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	ctx := c.Request().Context()

	resp, err := s.loadShowDetail(ctx, id)
	if errors.Is(err, errShowNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp)
}

// handleSetShowPoster persists the user's curated poster pick for the
// show after bounds-checking against CountPosters. 503 when image
// cache is nil/disabled; 400 on bad index; 404 on unknown show.
func (s *Server) handleSetShowPoster(c echo.Context) error {
	id, err := parseShowID(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if cacheErr := s.requireImageCache(); cacheErr != nil {
		return cacheErr
	}
	if existsErr := s.showExists(c.Request().Context(), id); existsErr != nil {
		return existsErr
	}

	var req showPosterChoiceRequest
	if bindErr := c.Bind(&req); bindErr != nil {
		return echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("decode body: %s", bindErr.Error()))
	}

	count := s.ImageCache().CountPosters(id)
	if req.Index < 0 || req.Index >= count {
		return c.JSON(http.StatusBadRequest, imageChoiceResponse{
			Error: fmt.Sprintf("poster index %d out of range [0, %d)", req.Index, count),
		})
	}

	if setErr := storage.SetShowPosterIndex(c.Request().Context(), s.db, id, req.Index); setErr != nil {
		return fmt.Errorf("set show poster index: %w", setErr)
	}
	return c.JSON(http.StatusOK, imageChoiceResponse{OK: true})
}

// errShowNotFound is the sentinel returned by loadShowDetail when no
// shows row matches the requested id. Distinct from
// storage.ErrRecordingNotFound so the handler can map it to 404
// without conflating the two entities.
var errShowNotFound = errors.New("show not found")

// parseShowID validates a positive int show id from the URL.
func parseShowID(s string) (int64, error) {
	id, err := storage.ParseRecordingID(s)
	if err != nil {
		return 0, fmt.Errorf("parse show id: %w", err)
	}
	return id, nil
}

// showExists returns 404 when no shows row matches id, nil otherwise.
// Used by the picker handler so an unknown id never silently writes a
// show_image_choices row keyed on a nonexistent show (the FK would
// cascade-block but we want to surface the failure cleanly).
func (s *Server) showExists(ctx context.Context, id int64) error {
	var marker int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM shows WHERE show_id = ?`, id).Scan(&marker)
	if errors.Is(err, sql.ErrNoRows) {
		return echo.NewHTTPError(http.StatusNotFound, "show not found")
	}
	if err != nil {
		return fmt.Errorf("query show %d: %w", id, err)
	}
	return nil
}

// loadShowDetail builds a ShowDetailResponse for the given show id.
// Returns errShowNotFound when no shows row exists for the id.
func (s *Server) loadShowDetail(ctx context.Context, id int64) (*ShowDetailResponse, error) {
	name, err := loadShowName(ctx, s.db, id)
	if err != nil {
		return nil, err
	}

	// Find recording ids for this show first, then resolve a state per
	// id via LoadState. The library list endpoint uses ListStates
	// (which unions collection/wants/recording_versions) — that
	// excludes recordings that exist in the recordings table without
	// any membership, which is exactly the case for a "show detail"
	// payload showing every recording catalogued. We loop and call
	// LoadState per id to get correct status (including "orphan" for
	// rows we know about upstream but neither own nor want).
	showRecIDs, err := loadRecordingIDsForShow(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	scoped := make([]storage.RecordingState, 0, len(showRecIDs))
	for _, rid := range showRecIDs {
		st, stErr := storage.LoadState(ctx, s.db, rid)
		if stErr != nil {
			return nil, fmt.Errorf("load state for recording %d: %w", rid, stErr)
		}
		scoped = append(scoped, *st)
	}

	meta, err := loadRecordingMeta(ctx, s.db, scoped)
	if err != nil {
		return nil, err
	}

	items := make([]RecordingListItem, 0, len(scoped))
	// Pre-size to the number of distinct storage.Status values so the
	// map doesn't grow as the loop tallies. The five status tokens are
	// enumerated in storage/state.go (synced / format_mismatch /
	// missing / wanted / orphan).
	const statusCardinality = 5
	stateCounts := make(map[string]int, statusCardinality)
	for _, st := range scoped {
		m := meta[st.RecordingID]
		items = append(items, RecordingListItem{
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
		stateCounts[string(st.Status)]++
	}

	firstYear, lastYear := yearSpan(items)
	description := loadShowDescription(ctx, s.db, id)

	return &ShowDetailResponse{
		ID:                  id,
		Name:                name,
		Description:         description,
		RecordingCount:      len(items),
		FirstYear:           firstYear,
		LastYear:            lastYear,
		StateCounts:         stateCounts,
		Recordings:          items,
		LocalPosterURLs:     s.showLocalPosterURLs(id),
		SelectedPosterIndex: s.loadShowPosterIndex(ctx, id),
	}, nil
}

// loadShowName returns the canonical show name from the shows table.
// errShowNotFound when no row matches.
func loadShowName(ctx context.Context, db *sql.DB, id int64) (string, error) {
	var name string
	err := db.QueryRowContext(ctx, `SELECT name FROM shows WHERE show_id = ?`, id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errShowNotFound
	}
	if err != nil {
		return "", fmt.Errorf("query show name %d: %w", id, err)
	}
	return name, nil
}

// loadRecordingIDsForShow returns every recording_id whose recordings
// row references the given show. Sorted by date ascending so callers
// that consume the slice in order get chronological results.
func loadRecordingIDsForShow(ctx context.Context, db *sql.DB, showID int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT recording_id FROM recordings
		WHERE show_id = ?
		ORDER BY date_full ASC, recording_id ASC
	`, showID)
	if err != nil {
		return nil, fmt.Errorf("query show recordings: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]int64, 0)
	for rows.Next() {
		var id int64
		if scanErr := rows.Scan(&id); scanErr != nil {
			return nil, fmt.Errorf("scan show recording id: %w", scanErr)
		}
		out = append(out, id)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate show recordings: %w", rerr)
	}
	return out, nil
}

// loadShowDescription walks the show's recordings looking for the
// first non-empty show_description on raw_json. Returns "" when no
// description is present — the SPA omits the field via the omitempty
// JSON tag in that case.
//
// The query asks SQLite to do the JSON probe via json_extract so we
// don't have to materialize the full payload Go-side. Failures are
// swallowed silently — the description is best-effort enrichment.
func loadShowDescription(ctx context.Context, db *sql.DB, showID int64) string {
	var description sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT json_extract(raw_json, '$.metadata.show_description')
		FROM recordings
		WHERE show_id = ?
		  AND json_extract(raw_json, '$.metadata.show_description') IS NOT NULL
		  AND json_extract(raw_json, '$.metadata.show_description') != ''
		LIMIT 1
	`, showID).Scan(&description)
	if err != nil {
		return ""
	}
	if !description.Valid {
		return ""
	}
	return stripShowDescriptionHTML(description.String)
}

// stripShowDescriptionHTML mirrors the nfo package's stripHTML helper
// (kept private there) so the API surface emits readable prose
// without `<p>` / `&#039;` artifacts. The SPA renders the value as
// plain text, so HTML tags would just leak into the page.
func stripShowDescriptionHTML(s string) string {
	if s == "" {
		return ""
	}
	r := strings.NewReplacer(
		"<p>", "",
		"</p>", "\n\n",
		"<br>", "\n",
		"<br/>", "\n",
		"<br />", "\n",
		"&#039;", "'",
		"&quot;", `"`,
		"&amp;", "&",
		"&lt;", "<",
		"&gt;", ">",
	)
	return strings.TrimSpace(r.Replace(s))
}

// isoYearPrefixLen is the number of leading characters we read off
// an ISO date to extract the year. Encora always emits ISO-8601, so
// the four-digit prefix is reliable.
const isoYearPrefixLen = 4

// yearSpan walks RecordingListItems and returns the (min, max) year
// across them. Returns (nil, nil) when no item has a parseable year.
func yearSpan(items []RecordingListItem) (*int, *int) {
	var first, last *int
	for _, it := range items {
		if len(it.DateFull) < isoYearPrefixLen {
			continue
		}
		// We don't need full ISO parsing — the year prefix is enough,
		// and Encora always emits ISO dates so the prefix is reliable.
		var y int
		_, err := fmt.Sscanf(it.DateFull[:isoYearPrefixLen], "%d", &y)
		if err != nil {
			continue
		}
		if first == nil || y < *first {
			v := y
			first = &v
		}
		if last == nil || y > *last {
			v := y
			last = &v
		}
	}
	return first, last
}

// showLocalPosterURLs walks 0..CountPosters and returns the cached
// /images/... URL for each present poster. Empty slice when the image
// cache is disabled.
func (s *Server) showLocalPosterURLs(showID int64) []string {
	cache := s.ImageCache()
	if cache == nil || cache.Disabled() {
		return []string{}
	}
	count := cache.CountPosters(showID)
	out := make([]string, 0, count)
	for i := range count {
		if u := cache.PosterURL(showID, i); u != "" {
			out = append(out, u)
		}
	}
	return out
}

// loadShowPosterIndex pulls the user's curated poster choice for the
// show. Returns nil on a missing row or a query error — the SPA falls
// back to highlighting index 0 in either case.
func (s *Server) loadShowPosterIndex(ctx context.Context, showID int64) *int {
	choice, err := storage.GetShowImageChoice(ctx, s.db, showID)
	if err != nil {
		s.logger.Warn().
			Err(err).
			Int64("show_id", showID).
			Msg("get show image choice failed; serving default")
		return nil
	}
	return choice.PosterIndex
}
