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
	"github.com/nicolerenee/promptbook/internal/ent/recording"
	"github.com/nicolerenee/promptbook/internal/ent/show"
	"github.com/nicolerenee/promptbook/internal/imagecache"
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
