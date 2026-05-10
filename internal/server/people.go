package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/ent/castentry"
	"github.com/nicolerenee/promptbook/internal/ent/performer"
	"github.com/nicolerenee/promptbook/internal/ent/recording"
	"github.com/nicolerenee/promptbook/internal/ent/show"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// peopleListLimit is the default limit applied to /api/v1/people when
// the caller doesn't pass ?limit=. The list is short enough that the
// home-grown 50/page cadence used elsewhere is plenty.
const peopleListLimit = 50

// PersonListItem is one row in the JSON list of people that appear in
// the user's library. RecordingCount counts distinct recordings the
// performer is credited on, scoped to recordings the user owns OR
// wants. StateCounts breaks that count down by reconciled status (e.g.
// "synced", "missing", "wanted", ...) so the list page can surface
// per-performer aggregates without N+1 detail fetches.
type PersonListItem struct {
	PerformerID    int64          `json:"performer_id"`
	Name           string         `json:"name"`
	Slug           string         `json:"slug"`
	RecordingCount int            `json:"recording_count"`
	StateCounts    map[string]int `json:"state_counts"`
}

// PersonRecording is a recording credit on the people-detail JSON.
type PersonRecording struct {
	ID             int64  `json:"id"`
	Show           string `json:"show"`
	Tour           string `json:"tour"`
	DateFull       string `json:"date_full"`
	DateMonthKnown bool   `json:"date_month_known"`
	DateDayKnown   bool   `json:"date_day_known"`
	ShowID         int64  `json:"show_id"`
	State          string `json:"state"`
}

// PersonDetail is the JSON payload for /api/v1/people/{id}.
type PersonDetail struct {
	PerformerID      int64             `json:"performer_id"`
	Name             string            `json:"name"`
	Slug             string            `json:"slug"`
	URL              string            `json:"url"`
	Recordings       []PersonRecording `json:"recordings"`
	LocalHeadshotURL string            `json:"local_headshot_url"`
}

func (s *Server) handleListPeople(c echo.Context) error {
	limit := paramInt(c, "limit", peopleListLimit)
	offset := paramInt(c, "offset", 0)
	sortKey := c.QueryParam("sort")
	dir := parseSortDir(c)

	ctx := c.Request().Context()
	all, err := loadPeopleList(ctx, s.db)
	if err != nil {
		return err
	}
	sortPeople(all, sortKey, dir)
	total := len(all)
	if offset >= total {
		return c.JSON(http.StatusOK, pageEnvelope([]PersonListItem{}, total, limit, offset))
	}
	end := min(offset+limit, total)
	page := all[offset:end]
	return c.JSON(http.StatusOK, pageEnvelope(page, total, limit, offset))
}

// ownedOrWantedIDs returns the union of recording ids that appear in
// either the collection or wants table — the "in scope for the people
// list" set used by every people endpoint.
func ownedOrWantedIDs(ctx context.Context, client *ent.Client) ([]int64, error) {
	colIDs, err := client.CollectionEntry.Query().IDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("query collection ids: %w", err)
	}
	wantsIDs, err := client.WantsEntry.Query().IDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("query wants ids: %w", err)
	}
	set := make(map[int64]struct{}, len(colIDs)+len(wantsIDs))
	for _, id := range colIDs {
		set[id] = struct{}{}
	}
	for _, id := range wantsIDs {
		set[id] = struct{}{}
	}
	out := make([]int64, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	return out, nil
}

func (s *Server) handleGetPerson(c echo.Context) error {
	id, err := parsePerformerID(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	ctx := c.Request().Context()

	detail, err := loadPersonDetail(ctx, s.db, id)
	if err != nil {
		if isStorageNotFound(err) {
			return echo.NewHTTPError(http.StatusNotFound, err.Error())
		}
		return err
	}

	if cache := s.ImageCache(); cache != nil && !cache.Disabled() {
		detail.LocalHeadshotURL = cache.HeadshotURL(detail.PerformerID)
	}

	return c.JSON(http.StatusOK, detail)
}

// isStorageNotFound returns true when err is one of the package-level
// "X not found" sentinels storage exposes for the people-related
// loaders. Used by the people-detail handler to map the error to a
// 404 without dragging the storage error vocabulary into the import
// graph at every call site.
func isStorageNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, storage.ErrPerformerNotFound) {
		return true
	}
	if errors.Is(err, storage.ErrCharacterNotFound) {
		return true
	}
	return errors.Is(err, storage.ErrRecordingNotFound)
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

// loadPeopleList returns every performer with at least one cast credit
// against an in-scope recording (collection ∪ wants), with their
// recording count and per-recording state tallies. The legacy SQL did
// this with a single GROUP BY + N+1 LoadState calls; the ent variant
// keeps the same shape.
func loadPeopleList(
	ctx context.Context,
	client *ent.Client,
) ([]PersonListItem, error) {
	scopeIDs, err := ownedOrWantedIDs(ctx, client)
	if err != nil {
		return nil, err
	}
	if len(scopeIDs) == 0 {
		return []PersonListItem{}, nil
	}

	// Cast entries scoped to in-scope recordings: maps performer_id ->
	// set of recording_ids.
	casts, err := client.CastEntry.Query().
		Where(castentry.RecordingIDIn(scopeIDs...)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query cast entries: %w", err)
	}
	credits := make(map[int64]map[int64]struct{})
	for _, ce := range casts {
		s := credits[ce.PerformerID]
		if s == nil {
			s = make(map[int64]struct{})
			credits[ce.PerformerID] = s
		}
		s[ce.RecordingID] = struct{}{}
	}
	if len(credits) == 0 {
		return []PersonListItem{}, nil
	}

	performerIDs := make([]int64, 0, len(credits))
	for id := range credits {
		performerIDs = append(performerIDs, id)
	}
	performers, err := client.Performer.Query().
		Where(performer.IDIn(performerIDs...)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query performers: %w", err)
	}

	out := make([]PersonListItem, 0, len(performers))
	for _, p := range performers {
		recIDs := credits[p.ID]
		counts := make(map[string]int, len(recIDs))
		for recID := range recIDs {
			st, stErr := storage.LoadState(ctx, client, recID)
			if stErr != nil {
				return nil, fmt.Errorf("load state for recording %d: %w", recID, stErr)
			}
			counts[string(st.Status)]++
		}
		out = append(out, PersonListItem{
			PerformerID:    p.ID,
			Name:           p.Name,
			Slug:           p.Slug,
			RecordingCount: len(recIDs),
			StateCounts:    counts,
		})
	}
	return out, nil
}

// sortPeople orders the list according to the SPA's sort keys.
// Stable, ascending by default; "count" sorts by RecordingCount.
func sortPeople(items []PersonListItem, sortKey string, dir sortDir) {
	desc := dir == sortDesc
	cmp := func(a, b PersonListItem) int {
		switch sortKey {
		case "count":
			return a.RecordingCount - b.RecordingCount
		default:
			return cmpString(a.Name, b.Name)
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		c := cmp(items[i], items[j])
		if desc {
			c = -c
		}
		if c != 0 {
			return c < 0
		}
		return items[i].PerformerID < items[j].PerformerID
	})
}

// loadPersonDetail loads the performer + the recordings they appear in.
// Returns ErrPerformerNotFound when no performers row matches.
func loadPersonDetail(
	ctx context.Context,
	client *ent.Client,
	id int64,
) (*PersonDetail, error) {
	p, err := storage.LoadPerformer(ctx, client, id)
	if err != nil {
		return nil, err
	}

	recIDs, err := storage.ListRecordingsForPerformer(ctx, client, id)
	if err != nil {
		return nil, fmt.Errorf("list recordings for performer %d: %w", id, err)
	}

	recs, err := loadPersonRecordings(ctx, client, recIDs)
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
// by show name, tour, date so the table reads alphabetically. Each
// row is decorated with the reconciled storage.Status — the N+1 cost
// is acceptable here because N is the number of performances a single
// performer is in (typically under 10).
func loadPersonRecordings(
	ctx context.Context,
	client *ent.Client,
	ids []int64,
) ([]PersonRecording, error) {
	if len(ids) == 0 {
		return []PersonRecording{}, nil
	}
	recs, err := client.Recording.Query().
		Where(recording.IDIn(ids...)).
		Order(recording.ByID()).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query person recordings: %w", err)
	}
	showIDSet := make(map[int64]struct{}, len(recs))
	for _, r := range recs {
		showIDSet[r.ShowID] = struct{}{}
	}
	showIDs := make([]int64, 0, len(showIDSet))
	for sID := range showIDSet {
		showIDs = append(showIDs, sID)
	}
	showNames := map[int64]string{}
	if len(showIDs) > 0 {
		shows, sErr := client.Show.Query().
			Where(show.IDIn(showIDs...)).All(ctx)
		if sErr != nil {
			return nil, fmt.Errorf("query shows: %w", sErr)
		}
		for _, sh := range shows {
			showNames[sh.ID] = sh.Name
		}
	}

	out := make([]PersonRecording, 0, len(recs))
	for _, r := range recs {
		out = append(out, PersonRecording{
			ID:             r.ID,
			Show:           showNames[r.ShowID],
			Tour:           r.Tour,
			DateFull:       r.DateFull,
			DateMonthKnown: r.DateMonthKnown,
			DateDayKnown:   r.DateDayKnown,
			ShowID:         r.ShowID,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if c := cmpString(out[i].Show, out[j].Show); c != 0 {
			return c < 0
		}
		if c := cmpString(out[i].Tour, out[j].Tour); c != 0 {
			return c < 0
		}
		return out[i].DateFull < out[j].DateFull
	})
	for i := range out {
		st, stErr := storage.LoadState(ctx, client, out[i].ID)
		if stErr != nil {
			return nil, fmt.Errorf("load state for recording %d: %w", out[i].ID, stErr)
		}
		out[i].State = string(st.Status)
	}
	return out, nil
}
