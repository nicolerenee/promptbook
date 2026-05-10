package server

// shows.go — JSON API surface for show-level views.
//
// Two endpoints registered here:
//   - GET  /api/v1/shows              (list — by-show aggregate)
//   - GET  /api/v1/shows/:id          (show detail page payload)
//
// The picker endpoints (POST /shows/:id/poster) from the v1 cache
// layout are gone — show banner selection is implicit by file
// existence at shows/<show_id>/banner.jpg under v2. Uploads + "set
// from URL" handlers live in upload.go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/ent/recording"
	"github.com/nicolerenee/promptbook/internal/ent/show"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// ShowListItem is one row in the JSON list returned by
// GET /api/v1/shows. Counts and state buckets are computed from the
// recordings + collection + wants tables via the same reconciler the
// recordings list uses, so the by-show view agrees with the per-row
// status taxonomy.
//
// FirstYear / LastYear are nil when the show has no dated recordings.
// LocalPosterURL is the cached banner URL or empty when the cache is
// disabled / the banner isn't on disk.
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

	shows, err := loadAllShows(ctx, s.db)
	if err != nil {
		return err
	}

	items := aggregateShows(states, meta, shows)
	s.decorateShowPosters(items)
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
func loadAllShows(ctx context.Context, client *ent.Client) (map[int64]string, error) {
	rows, err := client.Show.Query().All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query shows: %w", err)
	}
	out := make(map[int64]string, len(rows))
	for _, sh := range rows {
		out[sh.ID] = sh.Name
	}
	return out, nil
}

// aggregateShows groups the per-recording states + meta by show id and
// returns one ShowListItem per show.
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
const (
	minYear = 1000
	maxYear = 9999
)

// parseYear extracts a 4-digit year from the leading characters of
// date_full.
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

// decorateShowPosters sets LocalPosterURL on every item with a banner
// on disk. Selection is implicit by file existence under v2 — no SQL
// fan-out required.
func (s *Server) decorateShowPosters(items []ShowListItem) {
	cache := s.ImageCache()
	if cache == nil || cache.Disabled() || len(items) == 0 {
		return
	}
	for i := range items {
		showID := items[i].ID
		if showID == 0 {
			continue
		}
		items[i].LocalPosterURL = cache.ShowBannerURL(showID)
	}
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
// recordings.show_description across the show's recordings.
//
// LocalBannerURL is the show's chosen banner under v2, or empty when
// no banner is on disk yet.
type ShowDetailResponse struct {
	ID             int64               `json:"id"`
	Name           string              `json:"name"`
	Description    string              `json:"description,omitempty"`
	RecordingCount int                 `json:"recording_count"`
	FirstYear      *int                `json:"first_year"`
	LastYear       *int                `json:"last_year"`
	StateCounts    map[string]int      `json:"state_counts"`
	Recordings     []RecordingListItem `json:"recordings"`
	LocalBannerURL string              `json:"local_banner_url"`
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

// errShowNotFound is the sentinel returned by loadShowDetail when no
// shows row matches the requested id.
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
func (s *Server) showExists(ctx context.Context, id int64) error {
	exists, err := s.db.Show.Query().Where(show.IDEQ(id)).Exist(ctx)
	if err != nil {
		return fmt.Errorf("query show %d: %w", id, err)
	}
	if !exists {
		return echo.NewHTTPError(http.StatusNotFound, "show not found")
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

	var localBanner string
	if cache := s.ImageCache(); cache != nil && !cache.Disabled() {
		localBanner = cache.ShowBannerURL(id)
	}

	return &ShowDetailResponse{
		ID:             id,
		Name:           name,
		Description:    description,
		RecordingCount: len(items),
		FirstYear:      firstYear,
		LastYear:       lastYear,
		StateCounts:    stateCounts,
		Recordings:     items,
		LocalBannerURL: localBanner,
	}, nil
}

// loadShowName returns the canonical show name from the shows table.
func loadShowName(ctx context.Context, client *ent.Client, id int64) (string, error) {
	row, err := client.Show.Query().Where(show.IDEQ(id)).Only(ctx)
	if ent.IsNotFound(err) {
		return "", errShowNotFound
	}
	if err != nil {
		return "", fmt.Errorf("query show name %d: %w", id, err)
	}
	return row.Name, nil
}

// loadRecordingIDsForShow returns every recording_id for the show.
func loadRecordingIDsForShow(
	ctx context.Context, client *ent.Client, showID int64,
) ([]int64, error) {
	rows, err := client.Recording.Query().
		Where(recording.ShowID(showID)).
		Order(
			recording.ByDateFull(),
			recording.ByID(),
		).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query show recordings: %w", err)
	}
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out, nil
}

// loadShowDescription returns the first row's description_html from the
// show's recordings — used for the SPA's show detail header.
//
// The legacy SQL pulled the first non-empty show_description out of the
// raw_json column via json_extract; the ent variant pulls the
// recordings rows in show_id order and inspects raw_json in Go to keep
// the result identical without leaning on dialect-specific JSON
// support.
func loadShowDescription(
	ctx context.Context, client *ent.Client, showID int64,
) string {
	rows, err := client.Recording.Query().
		Where(recording.ShowID(showID)).
		Order(recording.ByID()).
		Limit(showDescriptionScanLimit).
		All(ctx)
	if err != nil {
		return ""
	}
	for _, r := range rows {
		// json_extract on raw_json returned the metadata.show_description
		// string; we mirror that path by pulling it from the recording's
		// last_seen denormalized fields when ent has them, else falling
		// back to a JSON parse of raw_json. ent's Recording struct does
		// not store metadata.show_description directly — it lives only
		// in raw_json — so a small JSON peek is the right call here.
		desc := extractShowDescription(r.RawJSON)
		if desc != "" {
			return stripShowDescriptionHTML(desc)
		}
	}
	return ""
}

// showDescriptionScanLimit caps how many rows loadShowDescription
// scans before giving up. Most shows resolve on the first row; very
// few shows would need more than 8 to surface a non-empty description.
const showDescriptionScanLimit = 16

// extractShowDescription pulls metadata.show_description out of a
// Recording.raw_json blob. Returns "" on any parse failure or absent
// field. The legacy SQL used SQLite's json_extract; we do it in Go
// here so the helper stays portable across ent dialects.
func extractShowDescription(rawJSON string) string {
	if rawJSON == "" {
		return ""
	}
	var m struct {
		Metadata struct {
			ShowDescription string `json:"show_description"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(rawJSON), &m); err != nil {
		return ""
	}
	return m.Metadata.ShowDescription
}

// stripShowDescriptionHTML normalizes a raw HTML show description into
// plain text suitable for the SPA.
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

// isoYearPrefixLen is the number of leading characters we read off an
// ISO date to extract the year.
const isoYearPrefixLen = 4

// yearSpan walks RecordingListItems and returns the (min, max) year
// across them.
func yearSpan(items []RecordingListItem) (*int, *int) {
	var first, last *int
	for _, it := range items {
		if len(it.DateFull) < isoYearPrefixLen {
			continue
		}
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
