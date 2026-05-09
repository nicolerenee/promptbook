package server

// shows.go owns the by-show aggregate endpoints that power the
// Library "Shows" mode. The list endpoint (GET /api/v1/shows) lives
// here; a sibling agent will add the show detail route
// (GET /api/v1/shows/:id) to this same file in a follow-up commit.
// Keep the handler boundaries small and table-driven so the merge
// stays mechanical.

import (
	"context"
	"database/sql"
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
// a list (sorted by name asc) with state-count buckets and year span.
// Sorting beyond name happens client-side — the API stays a stable
// canonical ordering so cache layers can ETag the response cleanly.
func (s *Server) handleListShows(c echo.Context) error {
	ctx := c.Request().Context()

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

	sortShowsByName(items)

	return c.JSON(http.StatusOK, map[string]any{itemsKey: items})
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

// sortShowsByName sorts in place by case-insensitive name asc, ties
// broken by id asc so equal names read deterministically.
func sortShowsByName(items []ShowListItem) {
	sort.SliceStable(items, func(i, j int) bool {
		ni := strings.ToLower(items[i].Name)
		nj := strings.ToLower(items[j].Name)
		if ni != nj {
			return ni < nj
		}
		return items[i].ID < items[j].ID
	})
}
