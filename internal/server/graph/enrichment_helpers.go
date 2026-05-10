package graph

// enrichment_helpers.go — pure helpers shared by the enrichment
// resolvers. Lives outside *.resolvers.go so gqlgen's regenerator
// doesn't sweep them into a "may delete" comment block on every run.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/ent/castentry"
	"github.com/nicolerenee/promptbook/internal/ent/performer"
	"github.com/nicolerenee/promptbook/internal/ent/recording"
	"github.com/nicolerenee/promptbook/internal/ent/show"
	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/ingest"
	"github.com/nicolerenee/promptbook/internal/rename"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// showDescriptionScanLimit caps how many recording rows the show
// description scan inspects before giving up. Most shows resolve on
// the first row; 16 is overkill but cheap.
const showDescriptionScanLimit = 16

// readRecordingNFO is the shared helper behind nfoContent +
// nfoModifiedAt. Keeps the disk read in one place so both fields
// agree on which file's mtime they're reporting.
func readRecordingNFO(
	ctx context.Context, client *ent.Client, recordingID int64,
) (string, *time.Time, error) {
	versions, err := storage.ListVersions(ctx, client, recordingID)
	if err != nil {
		return "", nil, fmt.Errorf(
			"graphql: list versions for recording %d: %w", recordingID, err,
		)
	}
	if len(versions) == 0 {
		return "", nil, nil
	}
	nfoPath := filepath.Join(filepath.Dir(versions[0].FilePath), "movie.nfo")
	b, err := os.ReadFile(nfoPath)
	if err != nil {
		// fs.ErrNotExist is the dominant case (no rename has run yet).
		// Surface it as ("", nil, nil) so the GraphQL field renders as
		// an empty string (the SPA's "no nfo" branch).
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil, nil
		}
		return "", nil, fmt.Errorf("graphql: read nfo %s: %w", nfoPath, err)
	}
	info, err := os.Stat(nfoPath)
	if err != nil {
		return "", nil, fmt.Errorf("graphql: stat nfo %s: %w", nfoPath, err)
	}
	mod := info.ModTime()
	return string(b), &mod, nil
}

// bannerLayoutFromOverlayStyle parses the OverlayStyleJSON blob and
// returns the banner-layout fields. Mirrors the REST
// bannerLayoutFromChoice helper. Empty pair when nothing's persisted
// or the JSON is malformed.
func bannerLayoutFromOverlayStyle(jsonPtr *string) (string, string) {
	if jsonPtr == nil || *jsonPtr == "" {
		return "", ""
	}
	var raw struct {
		Position    string `json:"position"`
		ImageRegion string `json:"image_region"`
	}
	if err := json.Unmarshal([]byte(*jsonPtr), &raw); err != nil {
		return "", ""
	}
	return raw.Position, raw.ImageRegion
}

// extractShowDescriptionFromRaw pulls metadata.show_description out
// of a Recording.raw_json blob. Used by the Show.description resolver
// to source the show-detail header copy. Returns "" on parse failure
// or absent field.
func extractShowDescriptionFromRaw(rawJSON string) string {
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

// stripShowDescriptionHTML runs the minimal HTML→text rewriter
// declared in helpers.go (stringReplacer). Two callers: the show
// description resolver and any future hand-port of the show-detail
// header.
func stripShowDescriptionHTML(s string) string {
	if s == "" {
		return ""
	}
	return stringReplacer.Replace(s)
}

// showYearSpan walks every recording for the show and returns
// (firstYear, lastYear) — both pointers because a show with no
// dated recordings has a nil span.
func showYearSpan(
	ctx context.Context, client *ent.Client, showID int64,
) (*int, *int, error) {
	rows, err := client.Recording.Query().
		Where(recording.ShowID(showID)).
		All(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("graphql: list recordings for show %d: %w", showID, err)
	}
	var first, last *int
	for _, row := range rows {
		y, ok := parseYearPrefix(row.DateFull)
		if !ok {
			continue
		}
		if first == nil || y < *first {
			yy := y
			first = &yy
		}
		if last == nil || y > *last {
			yy := y
			last = &yy
		}
	}
	return first, last, nil
}

// listLimitDefault is the page size the legacy REST handler used and
// the SPA expects. Replicated here so the GraphQL list resolvers
// match the wire shape exactly.
const listLimitDefault = 50

// maxStateScan caps how many states the recordings list materializes
// before paginating in memory. Mirrors the REST constant of the same
// name — bounded so the in-memory sort stays cheap regardless of
// catalog size.
const maxStateScan = 1024

// sortDir / sortKey constants — pulled out so the comparator-switch
// + dir-normalization paths stop tripping the goconst lint with
// duplicated literals. The token strings are also what the REST
// handler used so the SPA migration sees no behavior change.
const (
	sortDirDesc        = "desc"
	sortDirDescUpper   = "DESC"
	sortKeyStatus      = "status"
	sortKeyDate        = "date"
	sortKeyMaster      = "master"
	sortKeyLocalFormat = "local_format"
	sortKeyShowName    = "name"
	sortKeyRecCount    = "recording_count"
	sortKeyFirstYear   = "first_year"
	sortKeyLastYear    = "last_year"
)

// recordingsList is the GraphQL resolver body for the
// recordingsList query. The status / sort / dir / limit / offset
// args mirror the legacy REST handler exactly so the SPA migration is
// a thin wire-format swap.
func (r *Resolver) recordingsList(
	ctx context.Context,
	statusArg, sortArg, dirArg *string,
	limitArg, offsetArg *int,
) (*RecordingsListPage, error) {
	limit, offset := defaultLimitOffset(limitArg, offsetArg)
	statusFilter := optString(statusArg)
	sortKey := optString(sortArg)
	dir := normalizeSortDir(optString(dirArg))

	statuses := parseStatusList(statusFilter)
	listOpts := storage.ListStatesOptions{Status: statuses, Limit: maxStateScan}
	states, err := storage.ListStates(ctx, r.client, listOpts)
	if err != nil {
		return nil, fmt.Errorf("graphql: list states: %w", err)
	}
	meta, err := loadRecordingMeta(ctx, r.client, states)
	if err != nil {
		return nil, err
	}
	full := make([]*RecordingsListItem, 0, len(states))
	for _, st := range states {
		m, found := meta[st.RecordingID]
		if !found {
			m = recordingMeta{}
		}
		item := &RecordingsListItem{
			ID:             st.RecordingID,
			ShowID:         m.showID,
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
		}
		if r.imageCache != nil && !r.imageCache.Disabled() {
			item.LocalPosterURL = r.imageCache.RecordingPosterURL(st.RecordingID)
		}
		full = append(full, item)
	}
	sortRecordingsList(full, sortKey, dir)

	total := len(full)
	if offset >= total {
		return &RecordingsListPage{
			Items: []*RecordingsListItem{}, Total: total,
			Limit: limit, Offset: offset,
		}, nil
	}
	end := min(offset+limit, total)
	return &RecordingsListPage{
		Items:  full[offset:end],
		Total:  total,
		Limit:  limit,
		Offset: offset,
	}, nil
}

// showsList is the GraphQL resolver body for the showsList query.
// Shape mirrors the legacy REST handler exactly so the SPA layer
// stays thin. Aggregation is split out into showsAccumulator and
// finalizeShowsItems so the parent function stays under the
// gocognit threshold.
func (r *Resolver) showsList(
	ctx context.Context,
	sortArg, dirArg *string,
	limitArg, offsetArg *int,
) (*ShowsListPage, error) {
	limit, offset := defaultLimitOffset(limitArg, offsetArg)
	sortKey := optString(sortArg)
	dir := normalizeSortDir(optString(dirArg))

	states, err := storage.ListStates(ctx, r.client, storage.ListStatesOptions{
		Limit: maxStateScan,
	})
	if err != nil {
		return nil, fmt.Errorf("graphql: list states: %w", err)
	}
	meta, err := loadRecordingMeta(ctx, r.client, states)
	if err != nil {
		return nil, err
	}
	shows, err := r.client.Show.Query().All(ctx)
	if err != nil {
		return nil, fmt.Errorf("graphql: list shows: %w", err)
	}
	groups := showsAccumulator(states, meta, shows)
	items := r.finalizeShowsItems(groups)
	sortShowsList(items, sortKey, dir)

	total := len(items)
	if offset >= total {
		return &ShowsListPage{
			Items: []*ShowsListItem{}, Total: total,
			Limit: limit, Offset: offset,
		}, nil
	}
	end := min(offset+limit, total)
	return &ShowsListPage{
		Items:  items[offset:end],
		Total:  total,
		Limit:  limit,
		Offset: offset,
	}, nil
}

// showAccumulator is the per-show running tally showsAccumulator
// builds. Values pin into the final ShowsListItem one row at a time.
type showAccumulator struct {
	name        string
	count       int
	stateCounts ShowStateCounts
	first, last *int
}

// showsAccumulator walks every recording state + its meta + the shows
// table and builds the per-show tally. Pulled out of showsList so the
// parent function stays readable.
func showsAccumulator(
	states []storage.RecordingState,
	meta map[int64]recordingMeta,
	shows []*ent.Show,
) map[int64]*showAccumulator {
	groups := make(map[int64]*showAccumulator, len(shows))
	for _, sh := range shows {
		groups[sh.ID] = &showAccumulator{name: sh.Name}
	}
	for _, st := range states {
		m, mFound := meta[st.RecordingID]
		if !mFound || m.showID == 0 {
			continue
		}
		g, gFound := groups[m.showID]
		if !gFound {
			g = &showAccumulator{name: m.show}
			groups[m.showID] = g
		}
		if g.name == "" {
			g.name = m.show
		}
		g.count++
		incrementStateCount(&g.stateCounts, st.Status)
		updateYearSpan(g, m.dateFull)
	}
	return groups
}

// incrementStateCount fans the storage.Status enum into the matching
// ShowStateCounts field. Lifted into a helper so showsAccumulator's
// hot loop reads as a single statement per status update.
func incrementStateCount(counts *ShowStateCounts, status storage.Status) {
	switch status {
	case storage.StatusSynced:
		counts.Synced++
	case storage.StatusFormatMismatch:
		counts.FormatMismatch++
	case storage.StatusMissing:
		counts.Missing++
	case storage.StatusWanted:
		counts.Wanted++
	case storage.StatusOrphan:
		counts.Orphan++
	}
}

// updateYearSpan mutates the showAccumulator's first/last year span
// to include dateFull's year if it parses cleanly.
func updateYearSpan(g *showAccumulator, dateFull string) {
	y, ok := parseYearPrefix(dateFull)
	if !ok {
		return
	}
	if g.first == nil || y < *g.first {
		yy := y
		g.first = &yy
	}
	if g.last == nil || y > *g.last {
		yy := y
		g.last = &yy
	}
}

// finalizeShowsItems flattens the per-show tally into the GraphQL
// model type. Image-cache resolution lives here so the resolver root
// is the only thing the helper needs from the outside.
func (r *Resolver) finalizeShowsItems(
	groups map[int64]*showAccumulator,
) []*ShowsListItem {
	items := make([]*ShowsListItem, 0, len(groups))
	for id, g := range groups {
		stateCopy := g.stateCounts
		item := &ShowsListItem{
			ID:             id,
			Name:           g.name,
			RecordingCount: g.count,
			StateCounts:    &stateCopy,
			FirstYear:      g.first,
			LastYear:       g.last,
		}
		if r.imageCache != nil && !r.imageCache.Disabled() {
			item.LocalPosterURL = r.imageCache.ShowBannerURL(id)
		}
		items = append(items, item)
	}
	return items
}

// recordingMeta + loadRecordingMeta mirror the REST helpers — used by
// both list resolvers to attach show name / tour / date / master to
// each row in a single follow-up query.
type recordingMeta struct {
	showID     int64
	show       string
	tour       string
	dateFull   string
	monthKnown bool
	dayKnown   bool
	master     string
}

func loadRecordingMeta(
	ctx context.Context, client *ent.Client, states []storage.RecordingState,
) (map[int64]recordingMeta, error) {
	if len(states) == 0 {
		return map[int64]recordingMeta{}, nil
	}
	ids := make([]int64, len(states))
	for i, st := range states {
		ids[i] = st.RecordingID
	}
	recs, err := client.Recording.Query().Where(recording.IDIn(ids...)).All(ctx)
	if err != nil {
		return nil, fmt.Errorf("graphql: query recording meta: %w", err)
	}
	showIDSet := map[int64]struct{}{}
	for _, rec := range recs {
		showIDSet[rec.ShowID] = struct{}{}
	}
	showIDs := make([]int64, 0, len(showIDSet))
	for id := range showIDSet {
		showIDs = append(showIDs, id)
	}
	showNames := map[int64]string{}
	if len(showIDs) > 0 {
		shows, sErr := client.Show.Query().Where(show.IDIn(showIDs...)).All(ctx)
		if sErr != nil {
			return nil, fmt.Errorf("graphql: query shows: %w", sErr)
		}
		for _, sh := range shows {
			showNames[sh.ID] = sh.Name
		}
	}
	out := make(map[int64]recordingMeta, len(recs))
	for _, rec := range recs {
		out[rec.ID] = recordingMeta{
			showID:     rec.ShowID,
			show:       showNames[rec.ShowID],
			tour:       rec.Tour,
			dateFull:   rec.DateFull,
			monthKnown: rec.DateMonthKnown,
			dayKnown:   rec.DateDayKnown,
			master:     rec.Master,
		}
	}
	return out, nil
}

// parseStatusList splits a comma-separated set of status tokens into
// a slice of storage.Status. Empty / whitespace tokens are dropped.
// Returns nil for the empty string so the storage helper treats it as
// "no filter".
func parseStatusList(raw string) []storage.Status {
	if raw == "" {
		return nil
	}
	parts := splitAndTrim(raw, ",")
	if len(parts) == 0 {
		return nil
	}
	out := make([]storage.Status, 0, len(parts))
	for _, p := range parts {
		out = append(out, storage.Status(p))
	}
	return out
}

// splitAndTrim is the comma-split helper parseStatusList uses; pulled
// out as a tiny helper so the resolver file stays focused.
func splitAndTrim(raw, sep string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// defaultLimitOffset normalizes nil-pointer args from optional
// GraphQL Int arguments. mirrors the legacy paramInt fallback.
func defaultLimitOffset(limitArg, offsetArg *int) (int, int) {
	limit := listLimitDefault
	if limitArg != nil && *limitArg > 0 {
		limit = *limitArg
	}
	offset := 0
	if offsetArg != nil && *offsetArg > 0 {
		offset = *offsetArg
	}
	return limit, offset
}

// optString dereferences a *string GraphQL arg into a string, treating
// nil as the empty string. Pulled out so the call sites read cleanly.
func optString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// normalizeSortDir maps the wire token to "asc" / "desc". Anything
// else maps to "asc" so a typo doesn't crash the sort.
func normalizeSortDir(d string) string {
	if d == sortDirDesc || d == sortDirDescUpper {
		return sortDirDesc
	}
	return "asc"
}

// sortRecordingsList orders items in place by (sortKey, dir). Mirrors
// the legacy REST sortRecordings but operates on the GraphQL model
// type. Status sort is supported here because the reconciler's status
// is already on each row by the time we sort.
func sortRecordingsList(items []*RecordingsListItem, sortKey, dir string) {
	desc := dir == sortDirDesc
	cmp := recordingsListCompareFn(sortKey)
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

func recordingsListCompareFn(key string) func(a, b *RecordingsListItem) int {
	switch key {
	case sortKeyStatus:
		return func(a, b *RecordingsListItem) int { return strCmp(a.Status, b.Status) }
	case sortKeyDate:
		return func(a, b *RecordingsListItem) int { return strCmp(a.DateFull, b.DateFull) }
	case sortKeyMaster:
		return func(a, b *RecordingsListItem) int { return strCmp(a.Master, b.Master) }
	case sortKeyLocalFormat:
		return func(a, b *RecordingsListItem) int { return strCmpEmptyLast(a.LocalFormat, b.LocalFormat) }
	default:
		return func(a, b *RecordingsListItem) int {
			if c := strCmp(a.Show, b.Show); c != 0 {
				return c
			}
			if c := strCmp(a.DateFull, b.DateFull); c != 0 {
				return c
			}
			return strCmp(a.Tour, b.Tour)
		}
	}
}

// sortShowsList orders items in place by (sortKey, dir). Mirrors the
// legacy REST sortShows.
func sortShowsList(items []*ShowsListItem, sortKey, dir string) {
	desc := dir == sortDirDesc
	cmp := showsListCompareFn(sortKey)
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

func showsListCompareFn(key string) func(a, b *ShowsListItem) int {
	switch key {
	case sortKeyRecCount:
		return func(a, b *ShowsListItem) int { return intCmp(a.RecordingCount, b.RecordingCount) }
	case sortKeyFirstYear:
		return func(a, b *ShowsListItem) int { return intCmp(intDeref(a.FirstYear), intDeref(b.FirstYear)) }
	case sortKeyLastYear:
		return func(a, b *ShowsListItem) int { return intCmp(intDeref(a.LastYear), intDeref(b.LastYear)) }
	default:
		return func(a, b *ShowsListItem) int { return strCmp(a.Name, b.Name) }
	}
}

func intDeref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// strCmp / strCmpEmptyLast / intCmp — tiny ternary comparators
// mirroring the REST helpers from internal/server/listing.go but
// bound to this package so the GraphQL resolver doesn't reach across
// packages for a 3-line function.
func strCmp(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

// strCmpEmptyLast pins empty strings to the bottom of an ascending
// sort so unmatched local_format rows don't dominate the top of the
// list.
func strCmpEmptyLast(a, b string) int {
	if a == "" && b == "" {
		return 0
	}
	if a == "" {
		return 1
	}
	if b == "" {
		return -1
	}
	return strCmp(a, b)
}

func intCmp(a, b int) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

// Compile-time assertion that the resolver package can reference
// imagecache (the resolver root holds a pointer to it). Forces a
// build error if the import is dropped accidentally.
var _ *imagecache.Cache = (*imagecache.Cache)(nil)

// peopleList is the GraphQL resolver body for the peopleList query.
// Mirrors the REST handleListPeople: load the union of collection +
// wants ids, group cast entries by performer, decorate each with the
// per-status histogram via the reconciler. Custom resolver because
// the in-scope filter (collection ∪ wants) and the per-row
// state-counts aren't expressible through entgql's RecordingWhereInput
// shape.
func (r *Resolver) peopleList(
	ctx context.Context,
	sortArg, dirArg *string,
	limitArg, offsetArg *int,
) (*PersonListPage, error) {
	limit, offset := defaultLimitOffset(limitArg, offsetArg)
	sortKey := optString(sortArg)
	dir := normalizeSortDir(optString(dirArg))

	scopeIDs, err := ownedOrWantedRecordingIDs(ctx, r.client)
	if err != nil {
		return nil, err
	}
	if len(scopeIDs) == 0 {
		return &PersonListPage{
			Items: []*PersonListItem{}, Total: 0,
			Limit: limit, Offset: offset,
		}, nil
	}

	credits, err := loadPerformerCredits(ctx, r.client, scopeIDs)
	if err != nil {
		return nil, err
	}
	if len(credits) == 0 {
		return &PersonListPage{
			Items: []*PersonListItem{}, Total: 0,
			Limit: limit, Offset: offset,
		}, nil
	}

	performerIDs := make([]int64, 0, len(credits))
	for id := range credits {
		performerIDs = append(performerIDs, id)
	}
	performers, err := r.client.Performer.Query().
		Where(performer.IDIn(performerIDs...)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("graphql: query performers: %w", err)
	}

	items := make([]*PersonListItem, 0, len(performers))
	for _, p := range performers {
		recIDs := credits[p.ID]
		counts := ShowStateCounts{}
		for recID := range recIDs {
			st, stErr := storage.LoadState(ctx, r.client, recID)
			if stErr != nil {
				return nil, fmt.Errorf(
					"graphql: load state for recording %d: %w", recID, stErr)
			}
			incrStateCount(&counts, st.Status)
		}
		items = append(items, &PersonListItem{
			PerformerID:    p.ID,
			Name:           p.Name,
			Slug:           p.Slug,
			RecordingCount: len(recIDs),
			StateCounts:    &counts,
		})
	}
	sortPersonList(items, sortKey, dir)

	total := len(items)
	if offset >= total {
		return &PersonListPage{
			Items: []*PersonListItem{}, Total: total,
			Limit: limit, Offset: offset,
		}, nil
	}
	end := min(offset+limit, total)
	return &PersonListPage{
		Items: items[offset:end], Total: total,
		Limit: limit, Offset: offset,
	}, nil
}

// person is the GraphQL resolver body for the person(id:) query.
// Returns nil when the performer id doesn't resolve so the field
// renders as null. Otherwise loads the performer + every recording
// they're credited on, with the reconciler-derived state on each
// credit row. localHeadshotURL falls through to "" when the image
// cache is disabled.
func (r *Resolver) person(ctx context.Context, id int64) (*PersonDetail, error) {
	p, err := storage.LoadPerformer(ctx, r.client, id)
	if err != nil {
		if errors.Is(err, storage.ErrPerformerNotFound) {
			return nil, nil //nolint:nilnil // null when not found.
		}
		return nil, fmt.Errorf("graphql: load performer %d: %w", id, err)
	}
	recIDs, err := storage.ListRecordingsForPerformer(ctx, r.client, id)
	if err != nil {
		return nil, fmt.Errorf(
			"graphql: list recordings for performer %d: %w", id, err)
	}
	recs, err := r.loadPersonRecordings(ctx, recIDs)
	if err != nil {
		return nil, err
	}
	out := &PersonDetail{
		PerformerID: p.PerformerID,
		Name:        p.Name,
		Slug:        p.Slug,
		URL:         p.URL,
		Recordings:  recs,
	}
	if r.imageCache != nil && !r.imageCache.Disabled() {
		out.LocalHeadshotURL = r.imageCache.HeadshotURL(p.PerformerID)
	}
	return out, nil
}

// loadPersonRecordings resolves a slice of recording ids into the
// PersonRecording shape the SPA's people-detail table renders.
// Sorted by show name, tour, date so the table reads alphabetically.
func (r *Resolver) loadPersonRecordings(
	ctx context.Context, ids []int64,
) ([]*PersonRecording, error) {
	if len(ids) == 0 {
		return []*PersonRecording{}, nil
	}
	recs, err := r.client.Recording.Query().
		Where(recording.IDIn(ids...)).Order(recording.ByID()).All(ctx)
	if err != nil {
		return nil, fmt.Errorf("graphql: query person recordings: %w", err)
	}
	showNames, err := loadShowNames(ctx, r.client, recs)
	if err != nil {
		return nil, err
	}
	out := make([]*PersonRecording, 0, len(recs))
	for _, rec := range recs {
		out = append(out, &PersonRecording{
			ID:             rec.ID,
			Show:           showNames[rec.ShowID],
			Tour:           rec.Tour,
			DateFull:       rec.DateFull,
			DateMonthKnown: rec.DateMonthKnown,
			DateDayKnown:   rec.DateDayKnown,
			ShowID:         rec.ShowID,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if c := strCmp(out[i].Show, out[j].Show); c != 0 {
			return c < 0
		}
		if c := strCmp(out[i].Tour, out[j].Tour); c != 0 {
			return c < 0
		}
		return out[i].DateFull < out[j].DateFull
	})
	for i := range out {
		st, stErr := storage.LoadState(ctx, r.client, out[i].ID)
		if stErr != nil {
			return nil, fmt.Errorf(
				"graphql: load state for recording %d: %w", out[i].ID, stErr)
		}
		out[i].State = string(st.Status)
	}
	return out, nil
}

// ownedOrWantedRecordingIDs returns the union of recording ids that
// appear in either the collection or wants table. Mirrors the
// server.ownedOrWantedIDs helper but lives here to avoid a graph
// → server import cycle.
func ownedOrWantedRecordingIDs(
	ctx context.Context, client *ent.Client,
) ([]int64, error) {
	colIDs, err := client.CollectionEntry.Query().IDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("graphql: query collection ids: %w", err)
	}
	wantsIDs, err := client.WantsEntry.Query().IDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("graphql: query wants ids: %w", err)
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

// loadPerformerCredits walks every cast entry whose recording is in
// scope and returns a map performer_id → set of recording_ids.
func loadPerformerCredits(
	ctx context.Context, client *ent.Client, recIDs []int64,
) (map[int64]map[int64]struct{}, error) {
	casts, err := client.CastEntry.Query().
		Where(castentry.RecordingIDIn(recIDs...)).All(ctx)
	if err != nil {
		return nil, fmt.Errorf("graphql: query cast entries: %w", err)
	}
	out := make(map[int64]map[int64]struct{})
	for _, ce := range casts {
		bucket := out[ce.PerformerID]
		if bucket == nil {
			bucket = make(map[int64]struct{})
			out[ce.PerformerID] = bucket
		}
		bucket[ce.RecordingID] = struct{}{}
	}
	return out, nil
}

// loadShowNames returns a map show_id → name covering every distinct
// show referenced by recs. One query rather than N+1.
func loadShowNames(
	ctx context.Context, client *ent.Client, recs []*ent.Recording,
) (map[int64]string, error) {
	if len(recs) == 0 {
		return map[int64]string{}, nil
	}
	set := make(map[int64]struct{}, len(recs))
	for _, r := range recs {
		set[r.ShowID] = struct{}{}
	}
	ids := make([]int64, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	rows, err := client.Show.Query().Where(show.IDIn(ids...)).All(ctx)
	if err != nil {
		return nil, fmt.Errorf("graphql: query shows for performer: %w", err)
	}
	out := make(map[int64]string, len(rows))
	for _, s := range rows {
		out[s.ID] = s.Name
	}
	return out, nil
}

// incrStateCount increments the per-status counter on dst by status.
// Pulled out so the peopleList / showsList accumulator loops don't
// repeat the switch statement.
func incrStateCount(dst *ShowStateCounts, status storage.Status) {
	switch status {
	case storage.StatusSynced:
		dst.Synced++
	case storage.StatusFormatMismatch:
		dst.FormatMismatch++
	case storage.StatusMissing:
		dst.Missing++
	case storage.StatusWanted:
		dst.Wanted++
	case storage.StatusOrphan:
		dst.Orphan++
	}
}

// sortPersonList orders items in place by (sortKey, dir). Supported
// keys: "name" (default) and "count" (RecordingCount). Mirrors the
// REST sortPeople comparator.
func sortPersonList(items []*PersonListItem, sortKey, dir string) {
	desc := dir == sortDirDesc
	cmp := func(a, b *PersonListItem) int {
		switch sortKey {
		case "count":
			return intCmp(a.RecordingCount, b.RecordingCount)
		default:
			return strCmp(a.Name, b.Name)
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

// errIngestNotConfigured is the resolver-side analog of the legacy
// REST 503: returned by importQueueEntry when the server was built
// without an ingest engine. gqlgen surfaces it in the response's
// `errors` array; the SPA inspects the message text to map it back
// onto the legacy "config hint" UX.
var errIngestNotConfigured = errors.New(
	"graphql: ingest not configured")

// queue is the resolver body for the queue() field. Loads every row
// in storage.QueueEntry order (oldest-first) and projects each onto
// the GraphQL QueueEntry type. Empty queue returns an empty
// (non-nil) slice so the schema's `[QueueEntry!]!` non-null promise
// holds. The suggestedRecording field is populated inline via a
// batched lookup so the SPA's queue page renders the rich match
// summary in a single round-trip.
func (r *Resolver) queue(ctx context.Context) ([]*QueueEntry, error) {
	entries, err := storage.ListQueue(ctx, r.client)
	if err != nil {
		return nil, fmt.Errorf("graphql: list queue: %w", err)
	}
	out := make([]*QueueEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, queueEntryToGraphQL(e))
	}
	if attachErr := r.attachSuggestedRecordings(ctx, out); attachErr != nil {
		return nil, attachErr
	}
	return out, nil
}

// attachSuggestedRecordings fans the per-row suggestedRecordingID
// across a single batched lookup, then decorates each entry with the
// rich RecordingsListItem the SPA's queue page renders. Stale ids
// (the suggestion points at a recording that's been deleted) leave
// the field nil. Pulled out so the queue resolver stays a thin
// projection.
func (r *Resolver) attachSuggestedRecordings(
	ctx context.Context, entries []*QueueEntry,
) error {
	idSet := map[int64]struct{}{}
	for _, e := range entries {
		if e.SuggestedRecordingID != nil {
			idSet[*e.SuggestedRecordingID] = struct{}{}
		}
	}
	if len(idSet) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	recs, err := r.client.Recording.Query().
		Where(recording.IDIn(ids...)).All(ctx)
	if err != nil {
		return fmt.Errorf("graphql: load suggested recordings: %w", err)
	}
	showNames, err := loadShowNames(ctx, r.client, recs)
	if err != nil {
		return err
	}
	byID := make(map[int64]*ent.Recording, len(recs))
	for _, rec := range recs {
		byID[rec.ID] = rec
	}
	for _, e := range entries {
		if e.SuggestedRecordingID == nil {
			continue
		}
		rec, ok := byID[*e.SuggestedRecordingID]
		if !ok {
			continue
		}
		item := &RecordingsListItem{
			ID:             rec.ID,
			ShowID:         rec.ShowID,
			Show:           showNames[rec.ShowID],
			Tour:           rec.Tour,
			DateFull:       rec.DateFull,
			DateMonthKnown: rec.DateMonthKnown,
			DateDayKnown:   rec.DateDayKnown,
			Master:         rec.Master,
		}
		if r.imageCache != nil && !r.imageCache.Disabled() {
			item.LocalPosterURL = r.imageCache.RecordingPosterURL(rec.ID)
		}
		e.SuggestedRecording = item
	}
	return nil
}

// queueEntryToGraphQL projects storage.QueueEntry onto the GraphQL
// type. Pointer fields stay pointers (the GraphQL field is nullable);
// the wire-format prefix is applied by MarshalPrefixedID at write
// time. file_size_bytes narrows from int64 to int because GraphQL's
// `Int` scalar maps to Go int — every catalog file fits comfortably
// inside int32 so the cast is safe.
func queueEntryToGraphQL(e storage.QueueEntry) *QueueEntry {
	out := &QueueEntry{
		ID:                  e.ID,
		FilePath:            e.FilePath,
		FileSizeBytes:       int(e.FileSizeBytes),
		DiscoveredAt:        e.DiscoveredAt,
		LastSeenAt:          e.LastSeenAt,
		SuggestedConfidence: e.SuggestedConfidence,
		Notes:               e.Notes,
	}
	if e.SuggestedRecordingID != nil {
		v := *e.SuggestedRecordingID
		out.SuggestedRecordingID = &v
	}
	return out
}

// importQueueEntry is the resolver body for the importQueueEntry
// mutation. Mirrors the legacy REST handleImportQueue: loads the
// queue row, resolves the target recording id (explicit override or
// fallback to the queue entry's suggestion), runs the ingest engine,
// removes the row + writes a manual_import history event on success.
//
// Error mapping (matches the REST surface's HTTP statuses):
//   - ingestEngine == nil → returned as an error so gqlgen surfaces
//     it in the `errors` array (legacy 503).
//   - storage.ErrQueueEntryNotFound → returned as an error (legacy
//     404). The SPA inspects the message to decide whether to drop
//     the row optimistically.
//   - explicit recordingID <= 0 / no fallback → returned as an error
//     (legacy 400).
//   - engine returned a result but item.Err != nil → returned as a
//     successful resolver call with ok=false + error=item.Err.Error()
//     (matches the legacy 200-with-error-body).
func (r *Resolver) importQueueEntry(
	ctx context.Context, input ImportQueueEntryInput,
) (*ImportQueueEntryPayload, error) {
	if r.ingestEngine == nil {
		return nil, errIngestNotConfigured
	}

	entry, err := storage.LoadQueueEntry(ctx, r.client, input.QueueID)
	if errors.Is(err, storage.ErrQueueEntryNotFound) {
		return nil, fmt.Errorf("graphql: %w", err)
	}
	if err != nil {
		return nil, fmt.Errorf("graphql: load queue entry %d: %w", input.QueueID, err)
	}

	recordingID, err := resolveImportRecordingID(input.RecordingID, entry)
	if err != nil {
		return nil, err
	}

	res, err := r.ingestEngine.Ingest(ctx, entry.FilePath, ingest.Options{
		FlagEncoraID: int(recordingID),
	})
	if err != nil {
		return nil, fmt.Errorf("graphql: ingest queue entry %d: %w", input.QueueID, err)
	}
	if res == nil || len(res.Items) == 0 {
		return nil, fmt.Errorf("graphql: ingest queue entry %d: no result", input.QueueID)
	}

	item := res.Items[0]
	out := &ImportQueueEntryPayload{Action: item.Action}
	if item.Plan != nil {
		out.Dest = item.Plan.AbsoluteFile()
	}
	if item.Err != nil {
		out.Error = item.Err.Error()
	}
	out.Ok = item.Action == ingest.ActionMoved && item.Err == nil

	if out.Ok {
		// Best-effort cleanup + audit trail. RemoveQueueEntry is
		// idempotent; RecordEvent failures are logged and dropped
		// because the file already moved successfully — losing the
		// audit row beats surfacing a hard error to the user for a
		// cosmetic write.
		if removeErr := storage.RemoveQueueEntry(ctx, r.client, input.QueueID); removeErr != nil {
			r.logger.Warn().Err(removeErr).Int64("queue_id", input.QueueID).
				Msg("failed to remove queue entry after import")
		}
		event := storage.HistoryEvent{
			Kind:    storage.HistoryKindManualImport,
			Summary: fmt.Sprintf("Imported queued file %s", entry.FilePath),
			Details: map[string]any{
				"queue_id":  input.QueueID,
				"source":    entry.FilePath,
				"encora_id": recordingID,
				//nolint:goconst // map keys for a single audit event payload; constants would obscure the schema.
				"dest": out.Dest,
				//nolint:goconst // see "dest".
				"action": item.Action,
			},
		}
		rid := recordingID
		event.RecordingID = &rid
		if _, recErr := storage.RecordEvent(ctx, r.client, event); recErr != nil {
			r.logger.Warn().Err(recErr).Int64("queue_id", input.QueueID).
				Msg("failed to record manual import history event")
		}
	}

	return out, nil
}

// resolveImportRecordingID picks the recording id the ingest pipeline
// will run with: explicit override on the input wins, otherwise the
// queue entry's suggested_recording_id (must be non-nil). Returns an
// error suitable for gqlgen's `errors` array when neither is
// available or the override is non-positive. Mirrors the legacy
// REST helper of the same name.
func resolveImportRecordingID(override *int64, entry *storage.QueueEntry) (int64, error) {
	if override != nil {
		if *override <= 0 {
			return 0, errors.New("graphql: recordingID must be positive")
		}
		return *override, nil
	}
	if entry.SuggestedRecordingID == nil {
		return 0, errors.New(
			"graphql: no recordingID supplied and queue entry has no suggestion")
	}
	return *entry.SuggestedRecordingID, nil
}

// errLibraryNotConfigured is the resolver-side analog of "no library
// root + templates configured". Returned by previewQueueImport when
// the server was started without a library.root / templates pair.
// Surfaced in the GraphQL `errors` array; the SPA renders an inline
// "could not compute preview" message and lets the user import
// anyway.
var errLibraryNotConfigured = errors.New(
	"graphql: library not configured")

// searchRecordingsLimitDefault is the default cap on the typeahead
// dropdown. The modal uses this dropdown to narrow down — not
// paginate — so a small cap keeps the in-memory scan cheap.
const searchRecordingsLimitDefault = 25

// searchRecordingsLimitMax bounds the user-supplied limit on the
// typeahead. A bigger limit dilutes the dropdown and pushes the
// scoring loop toward the wider ent.Recording table; 100 is overkill
// for any sane UX but keeps the door open for future bulk pickers
// without unbounded growth.
const searchRecordingsLimitMax = 100

// searchRecordingsMinQuery is the minimum trimmed query length that
// triggers a real lookup. Below this we return [] so the user doesn't
// see a flood of unrelated results from a one-character keystroke.
const searchRecordingsMinQuery = 2

// searchRecordings is the resolver body for the searchRecordings
// query — the typeahead source for the queue import modal. Walks
// every recording (the catalog stays small enough that the in-memory
// filter is fine) and returns the rows whose show name or tour
// contains the query as a case-insensitive substring. Ranking is a
// simple score: prefix match > non-prefix substring; tour matches are
// tie-broken below show matches because the user is overwhelmingly
// typing the show.
func (r *Resolver) searchRecordings(
	ctx context.Context, query string, limitArg *int,
) ([]*RecordingsListItem, error) {
	q := strings.TrimSpace(query)
	if len(q) < searchRecordingsMinQuery {
		return []*RecordingsListItem{}, nil
	}
	limit := searchRecordingsLimitDefault
	if limitArg != nil && *limitArg > 0 {
		limit = *limitArg
	}
	if limit > searchRecordingsLimitMax {
		limit = searchRecordingsLimitMax
	}
	needle := strings.ToLower(q)

	recs, err := r.client.Recording.Query().All(ctx)
	if err != nil {
		return nil, fmt.Errorf("graphql: query recordings for search: %w", err)
	}
	showNames, err := loadShowNames(ctx, r.client, recs)
	if err != nil {
		return nil, err
	}

	type scored struct {
		score int
		item  *RecordingsListItem
	}
	scoredItems := make([]scored, 0, len(recs))
	for _, rec := range recs {
		showName := showNames[rec.ShowID]
		s := searchScore(needle, showName, rec.Tour)
		if s == 0 {
			continue
		}
		item := &RecordingsListItem{
			ID:             rec.ID,
			ShowID:         rec.ShowID,
			Show:           showName,
			Tour:           rec.Tour,
			DateFull:       rec.DateFull,
			DateMonthKnown: rec.DateMonthKnown,
			DateDayKnown:   rec.DateDayKnown,
			Master:         rec.Master,
		}
		if r.imageCache != nil && !r.imageCache.Disabled() {
			item.LocalPosterURL = r.imageCache.RecordingPosterURL(rec.ID)
		}
		scoredItems = append(scoredItems, scored{score: s, item: item})
	}
	sort.SliceStable(scoredItems, func(i, j int) bool {
		if scoredItems[i].score != scoredItems[j].score {
			return scoredItems[i].score > scoredItems[j].score
		}
		if scoredItems[i].item.Show != scoredItems[j].item.Show {
			return scoredItems[i].item.Show < scoredItems[j].item.Show
		}
		// Prefer newer dates within the same show — the user is
		// likeliest typing the show name to narrow down to a recent
		// recording rather than an ancient one.
		return scoredItems[i].item.DateFull > scoredItems[j].item.DateFull
	})
	if len(scoredItems) > limit {
		scoredItems = scoredItems[:limit]
	}
	out := make([]*RecordingsListItem, 0, len(scoredItems))
	for _, s := range scoredItems {
		out = append(out, s.item)
	}
	return out, nil
}

// searchScore is the per-row scorer searchRecordings uses. Returns
// zero when the row doesn't match at all; higher scores rank better.
// Show prefix matches lead, then show substring, then tour prefix,
// then tour substring. Pulled out so the comparator stays tiny.
func searchScore(needle, show, tour string) int {
	if needle == "" {
		return 0
	}
	showLower := strings.ToLower(show)
	tourLower := strings.ToLower(tour)
	score := 0
	if strings.HasPrefix(showLower, needle) {
		score += 100
	} else if strings.Contains(showLower, needle) {
		score += 60
	}
	switch {
	case strings.HasPrefix(tourLower, needle):
		score += 30
	case strings.Contains(tourLower, needle):
		score += 15
	}
	return score
}

// previewQueueImport is the resolver body for the previewQueueImport
// query — runs the rename Plan path against the queue row + chosen
// recording and returns the planned destination paths. Does NOT move
// the file. Mirrors the Plan-build branch of importQueueEntry.
func (r *Resolver) previewQueueImport(
	ctx context.Context, input PreviewQueueImportInput,
) (*ImportPreview, error) {
	if !r.libraryPlan.Configured() {
		return nil, errLibraryNotConfigured
	}
	entry, err := storage.LoadQueueEntry(ctx, r.client, input.QueueID)
	if errors.Is(err, storage.ErrQueueEntryNotFound) {
		return nil, fmt.Errorf("graphql: %w", err)
	}
	if err != nil {
		return nil, fmt.Errorf(
			"graphql: load queue entry %d: %w", input.QueueID, err)
	}
	if input.RecordingID <= 0 {
		return nil, errors.New("graphql: recordingID must be positive")
	}
	loaded, err := storage.LoadRecording(ctx, r.client, input.RecordingID)
	if err != nil {
		return nil, fmt.Errorf(
			"graphql: load recording %d: %w", input.RecordingID, err)
	}
	plan, err := rename.BuildPlan(rename.PlanInputs{
		Recording:      loaded.Recording,
		Source:         entry.FilePath,
		LibraryRoot:    r.libraryPlan.Root,
		FolderTemplate: r.libraryPlan.FolderTemplate,
		FileTemplate:   r.libraryPlan.FileTemplate,
	})
	if err != nil {
		return nil, fmt.Errorf("graphql: build plan: %w", err)
	}
	return &ImportPreview{
		DestFolder:   plan.TargetFolder,
		DestFile:     plan.TargetFile + plan.Extension,
		DestAbsolute: plan.AbsoluteFile(),
	}, nil
}
