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

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/ent/castentry"
	"github.com/nicolerenee/promptbook/internal/ent/performer"
	"github.com/nicolerenee/promptbook/internal/ent/recording"
	"github.com/nicolerenee/promptbook/internal/ent/show"
	"github.com/nicolerenee/promptbook/internal/externalids"
	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/ingest"
	"github.com/nicolerenee/promptbook/internal/match"
	"github.com/nicolerenee/promptbook/internal/probe"
	"github.com/nicolerenee/promptbook/internal/rename"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// showDescriptionScanLimit caps how many recording rows the show
// description scan inspects before giving up. Most shows resolve on
// the first row; 16 is overkill but cheap.
const showDescriptionScanLimit = 16

// recordingExtras is the resolver body for Recording.extras. Reads
// the typed `recording_extras` table — phase 1 of multipart-and-extras
// swapped this resolver from the legacy "walk source_folder at read
// time" pattern to the persisted shape so kind/label metadata
// captured at ingest time survives.
//
// Recordings that pre-date the new table (legacy folder-as-unit
// imports whose source_folder was walked at read time) get an empty
// list; backfilling those rows is intentionally out of scope for
// phase 1.
//
// Each row's IsDir is always false — phase 1 routes per-file extras
// into Jellyfin-shaped subfolders at ingest time, so directories
// aren't first-class rows. The IsDir field stays on the GraphQL type
// for SPA backward compatibility with the legacy walking resolver.
func (r *Resolver) recordingExtras(
	ctx context.Context, recordingID int64,
) ([]*RecordingExtra, error) {
	rows, err := storage.ListExtras(ctx, r.client, recordingID)
	if err != nil {
		return nil, fmt.Errorf(
			"graphql: list extras for recording %d: %w", recordingID, err)
	}
	out := make([]*RecordingExtra, 0, len(rows))
	for _, row := range rows {
		out = append(out, &RecordingExtra{
			Path:      row.FilePath,
			Name:      filepath.Base(row.FilePath),
			SizeBytes: int(row.FileSizeBytes),
			IsDir:     false,
			Kind:      row.Kind,
			Label:     row.Label,
		})
	}
	return out, nil
}

// recordingExternalIDs is the resolver body for
// Recording.externalIDs. Reads every (provider, external_id) row
// from the external_ids table, ordered alphabetically by provider
// per externalids.ListForRecording's contract, and projects each
// row onto the GraphQL chip shape (provider / label / externalID /
// url).
//
// Returns an empty slice when the recording has no rows in
// external_ids (shouldn't happen for synced recordings — the
// migration's back-fill seeded encora rows for every existing
// recording, and sync.PersistRecording stamps new ones) or when the
// resolver's sqlDB handle is nil (test fixtures that didn't wire it).
func (r *Resolver) recordingExternalIDs(
	ctx context.Context, recordingID int64,
) ([]*RecordingExternalID, error) {
	if r.sqlDB == nil {
		return []*RecordingExternalID{}, nil
	}
	rows, err := externalids.ListForRecording(ctx, r.sqlDB, recordingID)
	if err != nil {
		return nil, fmt.Errorf(
			"graphql: list external_ids for recording %d: %w", recordingID, err)
	}
	out := make([]*RecordingExternalID, 0, len(rows))
	for _, row := range rows {
		entry := &RecordingExternalID{
			Provider:   string(row.Provider),
			Label:      externalids.LabelFor(row.Provider),
			ExternalID: row.ExternalID,
		}
		if url := externalids.URLFor(row.Provider, row.ExternalID); url != "" {
			urlCopy := url
			entry.URL = &urlCopy
		}
		out = append(out, entry)
	}
	return out, nil
}

// recordingMediaInfo is the resolver body for Recording.mediaInfo. It
// reads the latest recording_versions row (largest file first, the
// same order ListVersions returns) and decodes its media_info_json
// blob into the GraphQL MediaInfo shape. For multipart recordings
// the durationSeconds field is the SUM across every part — the user
// sees "the runtime of the show" rather than "the runtime of one
// act." Other fields read from the lead part since codec / quality
// agree across parts of one capture.
//
// Returns nil — i.e. the GraphQL field renders as null — when there
// is no version, the blob is empty (legacy import before the field
// existed), or the JSON is unparseable. The detail page hides the
// media-info card on null.
func (r *Resolver) recordingMediaInfo(
	ctx context.Context, recordingID int64,
) (*MediaInfo, error) {
	versions, err := storage.ListVersions(ctx, r.client, recordingID)
	if err != nil {
		return nil, fmt.Errorf(
			"graphql: list versions for recording %d: %w", recordingID, err)
	}
	if len(versions) == 0 {
		return nil, nil //nolint:nilnil // null when no version row exists.
	}
	blob := strings.TrimSpace(versions[0].MediaInfoJSON)
	if blob == "" || blob == "null" || blob == "{}" {
		return nil, nil //nolint:nilnil // null when no media info captured.
	}
	info, ok := decodeMediaInfoBlob(blob)
	if !ok {
		// Malformed blob is not fatal — a future re-ingest will fix
		// it; in the meantime the SPA hides the card.
		return nil, nil //nolint:nilnil // null when blob is unparseable.
	}
	// Multipart sum: every additional part's duration adds onto the
	// lead part's. Codec / resolution / audio shape stay identical
	// across parts so we don't need to merge anything else. Parts
	// with empty / unparseable blobs contribute zero — same fallback
	// as the lead-part nil branch above.
	for i := 1; i < len(versions); i++ {
		next := strings.TrimSpace(versions[i].MediaInfoJSON)
		if next == "" || next == "null" || next == "{}" {
			continue
		}
		decoded, decodedOK := decodeMediaInfoBlob(next)
		if !decodedOK {
			continue
		}
		info.DurationSeconds += decoded.DurationSeconds
	}
	return mediaInfoToGraphQL(info, versions[0].Container), nil
}

// recordingLocalReleaseFormat is the resolver body for
// Recording.localReleaseFormat. Delegates to
// storage.ComputeFormatString so multipart-merge + same-format
// grouping happens in one place — that helper produces a single
// "MP4 - x264 + AAC - 1080p - 8.70 GB - 2 files" line for two-act
// captures rather than two near-identical bracket groups, and
// surfaces truly different masters / qualities as separate
// bracketed entries.
func (r *Resolver) recordingLocalReleaseFormat(
	ctx context.Context, recordingID int64,
) (string, error) {
	versions, err := storage.ListVersions(ctx, r.client, recordingID)
	if err != nil {
		return "", fmt.Errorf(
			"graphql: list versions for recording %d: %w", recordingID, err)
	}
	return storage.ComputeFormatString(versions), nil
}

// decodeMediaInfoBlob unmarshals the persisted JSON blob into the
// probe.MediaInfo shape. The bool reports whether decoding succeeded
// — recordingMediaInfo treats a parse failure as "no media info" and
// returns nil to the GraphQL field. Pulled out so the resolver-side
// nil-on-error branch doesn't trip the nilerr lint.
func decodeMediaInfoBlob(blob string) (probe.MediaInfo, bool) {
	var info probe.MediaInfo
	if err := json.Unmarshal([]byte(blob), &info); err != nil {
		return probe.MediaInfo{}, false
	}
	return info, true
}

// mediaInfoToGraphQL projects probe.MediaInfo onto the GraphQL
// MediaInfo type. Container falls back to the persisted
// versions[0].Container value when the persisted blob doesn't carry
// one (defensive — the encode path always sets it via filepath.Ext on
// the destination, but legacy / partial blobs aren't guaranteed to).
func mediaInfoToGraphQL(info probe.MediaInfo, fallbackContainer string) *MediaInfo {
	out := &MediaInfo{
		Container:       info.Container,
		VideoCodec:      info.VideoCodec,
		Width:           info.Width,
		Height:          info.Height,
		VideoBitDepth:   info.VideoBitDepth,
		VideoFps:        info.VideoFps,
		DurationSeconds: info.DurationSeconds,
		ScanType:        info.ScanType,
		AudioStreams:    make([]*AudioStream, 0, len(info.AudioStreams)),
		SubtitleStreams: make([]*SubtitleStream, 0, len(info.SubtitleStreams)),
	}
	if out.Container == "" {
		// Legacy fallback — strip dot, uppercase to match the rename
		// path's convention.
		out.Container = strings.ToUpper(fallbackContainer)
	}
	for _, a := range info.AudioStreams {
		out.AudioStreams = append(out.AudioStreams, &AudioStream{
			Codec:         a.Codec,
			ChannelLayout: a.ChannelLayout,
			Bitrate:       a.Bitrate,
			Language:      a.Language,
		})
	}
	for _, s := range info.SubtitleStreams {
		out.SubtitleStreams = append(out.SubtitleStreams, &SubtitleStream{
			Codec:    s.Codec,
			Language: s.Language,
		})
	}
	return out
}

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
// before paginating in memory. The SPA's list pages drop visible
// pagination and request the full catalog in a single fetch, so this
// cap doubles as the practical upper bound on catalog size we'll
// surface — 100k is comfortably above any realistic personal Broadway
// catalog while still keeping the in-memory sort + filter bounded.
const maxStateScan = 100_000

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
			DateVariant:    m.dateVariant,
			Master:         m.master,
			Status:         string(st.Status),
			InCollection:   st.InCollection,
			InWants:        st.InWants,
			FileCount:      st.FileCount,
			EncoraFormat:   st.EncoraFormat,
			LocalFormat:    st.LocalFormat,
		}
		// LocalReleaseFormat is the locally-derived "what files do
		// you have" string. Empty when there are no versions, so we
		// only walk the version rows when FileCount > 0 — the
		// recordingLocalReleaseFormat helper handles the empty case
		// internally too, but skipping the call when we know it's
		// empty keeps the no-files row cheap.
		if st.FileCount > 0 {
			rf, rfErr := r.recordingLocalReleaseFormat(ctx, st.RecordingID)
			if rfErr != nil {
				return nil, rfErr
			}
			item.LocalReleaseFormat = rf
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
	showID      int64
	show        string
	tour        string
	dateFull    string
	monthKnown  bool
	dayKnown    bool
	dateVariant *string
	master      string
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
			showID:      rec.ShowID,
			show:        showNames[rec.ShowID],
			tour:        rec.Tour,
			dateFull:    rec.DateFull,
			monthKnown:  rec.DateMonthKnown,
			dayKnown:    rec.DateDayKnown,
			dateVariant: rec.DateVariant,
			master:      rec.Master,
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
			DateVariant:    rec.DateVariant,
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
			DateVariant:    rec.DateVariant,
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
// inside int32 so the cast is safe. Classification is decoded from
// the row's JSON blob — empty / unparseable inputs produce an empty
// (non-nil) QueueClassification so the schema's non-null promise
// still holds for legacy rows.
func queueEntryToGraphQL(e storage.QueueEntry) *QueueEntry {
	out := &QueueEntry{
		ID:                  e.ID,
		FilePath:            e.FilePath,
		FileSizeBytes:       int(e.FileSizeBytes),
		DiscoveredAt:        e.DiscoveredAt,
		LastSeenAt:          e.LastSeenAt,
		SuggestedConfidence: e.SuggestedConfidence,
		Notes:               e.Notes,
		ExtrasCount:         e.ExtrasCount,
		Classification:      decodeQueueClassification(e.ClassificationJSON),
	}
	if e.SuggestedRecordingID != nil {
		v := *e.SuggestedRecordingID
		out.SuggestedRecordingID = &v
	}
	return out
}

// decodeQueueClassification turns the JSON blob the scanner persists
// onto manual_import_queue.classification_json into the GraphQL
// QueueClassification shape. Empty / unparseable inputs return an
// empty (non-nil) QueueClassification so the schema's non-null
// promise still holds for legacy rows.
func decodeQueueClassification(raw string) *QueueClassification {
	out := &QueueClassification{
		Parts:       []*QueueClassifiedFile{},
		Extras:      []*QueueClassifiedFile{},
		ExternalIDs: []*QueueExternalID{},
	}
	if raw == "" {
		return out
	}
	var decoded struct {
		Parts []struct {
			Path          string `json:"path"`
			SizeBytes     int64  `json:"sizeBytes"`
			SuggestedKind string `json:"suggestedKind"`
			PartIndex     int    `json:"partIndex"`
		} `json:"parts"`
		Extras []struct {
			Path          string `json:"path"`
			SizeBytes     int64  `json:"sizeBytes"`
			SuggestedKind string `json:"suggestedKind"`
			PartIndex     int    `json:"partIndex"`
		} `json:"extras"`
		Ambiguous bool `json:"ambiguous"`
		// ExternalIDs mirrors the externalids.ExternalID JSON shape;
		// the externalids package owns the wire tag casing (lower
		// camelCase). omitempty at the source means the field is
		// absent on pre-tag rows — json.Unmarshal leaves the local
		// slice nil in that case, which is the empty-classification
		// branch we want.
		ExternalIDs []struct {
			Provider   string `json:"provider"`
			ExternalID string `json:"externalID"`
		} `json:"externalIDs"`
		// DiscFormat is the scanner's disc-shape marker: empty for
		// non-disc folders, "dvd" when a VIDEO_TS layout was
		// detected. The SPA renders a small badge off this value.
		DiscFormat string `json:"discFormat"`
	}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		// Defensive: a row with a corrupt blob still surfaces as an
		// empty classification rather than failing the queue read.
		return out
	}
	out.Ambiguous = decoded.Ambiguous
	out.DiscFormat = decoded.DiscFormat
	for _, p := range decoded.Parts {
		out.Parts = append(out.Parts, &QueueClassifiedFile{
			Path:          p.Path,
			SizeBytes:     int(p.SizeBytes),
			SuggestedKind: p.SuggestedKind,
			PartIndex:     p.PartIndex,
		})
	}
	for _, x := range decoded.Extras {
		out.Extras = append(out.Extras, &QueueClassifiedFile{
			Path:          x.Path,
			SizeBytes:     int(x.SizeBytes),
			SuggestedKind: x.SuggestedKind,
			PartIndex:     x.PartIndex,
		})
	}
	for _, eid := range decoded.ExternalIDs {
		url := externalids.URLFor(externalids.Provider(eid.Provider), eid.ExternalID)
		entry := &QueueExternalID{
			Provider:   eid.Provider,
			ExternalID: eid.ExternalID,
		}
		if url != "" {
			urlCopy := url
			entry.URL = &urlCopy
		}
		out.ExternalIDs = append(out.ExternalIDs, entry)
	}
	return out
}

// importFileAssignmentsFromInput translates the GraphQL
// FileAssignmentInput slice into the ingest.FileAssignment shape the
// engine consumes. Returns nil when the input is empty so the engine
// stays in legacy single-file mode for callers that don't yet drive
// the modal picker. Skips nil input entries defensively (gqlgen
// doesn't actually emit them, but the contract here is "keep
// trustworthy assignments only").
func importFileAssignmentsFromInput(in []*FileAssignmentInput) []ingest.FileAssignment {
	if len(in) == 0 {
		return nil
	}
	out := make([]ingest.FileAssignment, 0, len(in))
	for _, fa := range in {
		if fa == nil {
			continue
		}
		assignment := ingest.FileAssignment{
			SourcePath: fa.SourcePath,
			Kind:       fa.Kind,
		}
		if fa.Label != nil {
			assignment.Label = *fa.Label
		}
		out = append(out, assignment)
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

	early, conflictErr := r.preflightConflictCheck(ctx, entry, recordingID, input)
	if conflictErr != nil {
		return nil, conflictErr
	}
	if early != nil {
		return early, nil
	}

	opts := buildImportOptions(entry, recordingID, input)
	res, err := r.ingestEngine.Ingest(ctx, entry.FilePath, opts)
	if err != nil {
		return nil, fmt.Errorf("graphql: ingest queue entry %d: %w", input.QueueID, err)
	}
	if res == nil || len(res.Items) == 0 {
		return nil, fmt.Errorf("graphql: ingest queue entry %d: no result", input.QueueID)
	}

	item := res.Items[0]
	out := &ImportQueueEntryPayload{Action: item.Action}
	switch {
	case item.ExternallyManaged:
		// Catalog-only imports leave the file at its source path —
		// surface that as the "destination" so the SPA's success
		// toast points at the right path.
		out.Dest = item.Source
	case item.Plan != nil:
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
				"queue_id": input.QueueID,
				//nolint:goconst // map keys for a single audit event payload; constants would obscure the schema.
				"source":    entry.FilePath,
				"encora_id": recordingID,
				//nolint:goconst // see "source".
				"dest": out.Dest,
				//nolint:goconst // see "source".
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

// preflightConflictCheck dispatches the destination-conflict check
// when the import is moving the file. Externally-managed imports
// never overwrite anything (the file stays at the source path) so
// the check is skipped entirely.
//
// Returns (nil, nil) when the import should proceed, (payload, nil)
// when the conflict pre-flight short-circuits with a duplicate /
// overwrite-required outcome, and (nil, err) on a fatal probe /
// plan-build failure.
func (r *Resolver) preflightConflictCheck(
	ctx context.Context,
	entry *storage.QueueEntry,
	recordingID int64,
	input ImportQueueEntryInput,
) (*ImportQueueEntryPayload, error) {
	if optBool(input.ExternallyManaged) {
		return nil, nil //nolint:nilnil // by design — caller proceeds.
	}
	return r.maybeShortCircuitOnConflict(
		ctx, entry, recordingID, input.QueueID, optBool(input.Overwrite))
}

// buildImportOptions assembles the ingest.Options struct from the
// queue entry + mutation input. Pulled out so the importQueueEntry
// resolver stays under the gocognit threshold; the call site reads
// the helper's name as a single decision step.
//
// Folder-as-unit drops (ExtrasCount > 0) stamp the parent dir on
// SourceFolder so the recording detail page can enumerate sibling
// files later. FileAssignments, when supplied, switches the engine
// into multi-file mode. ExternallyManaged threads onto every per-
// item ingest in a batch.
func buildImportOptions(
	entry *storage.QueueEntry, recordingID int64, input ImportQueueEntryInput,
) ingest.Options {
	opts := ingest.Options{FlagEncoraID: int(recordingID)}
	if entry.ExtrasCount > 0 {
		opts.SourceFolder = filepath.Dir(entry.FilePath)
	}
	opts.FileAssignments = importFileAssignmentsFromInput(input.FileAssignments)
	opts.ExternallyManaged = optBool(input.ExternallyManaged)
	opts.ExternalIDs = decodeClassificationExternalIDs(entry.ClassificationJSON)
	opts.DiscFormat, opts.DiscScaffolding = decodeClassificationDisc(entry.ClassificationJSON)
	return opts
}

// decodeClassificationDisc pulls the discFormat + discScaffolding
// list out of the queue row's classification_json blob. Empty /
// unparseable input yields the zero values so the importQueueEntry
// path falls through to the non-DVD legacy behaviour. Mirrors
// decodeClassificationExternalIDs in structure — local one-shot
// struct to keep the decode self-contained.
func decodeClassificationDisc(raw string) (string, []string) {
	if raw == "" {
		return "", nil
	}
	var decoded struct {
		DiscFormat      string   `json:"discFormat"`
		DiscScaffolding []string `json:"discScaffolding"`
	}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return "", nil
	}
	return decoded.DiscFormat, decoded.DiscScaffolding
}

// decodeClassificationExternalIDs pulls the external_ids list out of
// the queue row's classification_json blob. Mirrors
// decodeQueueClassification's external-ids decode but returns the
// externalids.ExternalID wire shape ingest.Options consumes. Empty /
// unparseable input yields nil so the importQueueEntry path falls
// through to the legacy "no external ids" behaviour.
func decodeClassificationExternalIDs(raw string) []externalids.ExternalID {
	if raw == "" {
		return nil
	}
	var decoded struct {
		ExternalIDs []struct {
			Provider   string `json:"provider"`
			ExternalID string `json:"externalID"`
		} `json:"externalIDs"`
	}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil
	}
	if len(decoded.ExternalIDs) == 0 {
		return nil
	}
	out := make([]externalids.ExternalID, 0, len(decoded.ExternalIDs))
	for _, eid := range decoded.ExternalIDs {
		if eid.Provider == "" || eid.ExternalID == "" {
			continue
		}
		out = append(out, externalids.ExternalID{
			Provider:   externalids.Provider(eid.Provider),
			ExternalID: eid.ExternalID,
		})
	}
	return out
}

// maybeShortCircuitOnConflict runs the destination-conflict check
// before ingest. Returns (nil, nil) when the import should proceed,
// (payload, nil) when conflict short-circuits with a duplicate /
// overwrite-required outcome, and (nil, err) on a fatal probe /
// plan-build failure. The duplicate path also removes the queue row
// so the modal closes cleanly.
func (r *Resolver) maybeShortCircuitOnConflict(
	ctx context.Context,
	entry *storage.QueueEntry,
	recordingID, queueID int64,
	overwrite bool,
) (*ImportQueueEntryPayload, error) {
	if !r.libraryPlan.Configured() || r.libraryPlan.Prober == nil {
		return nil, nil //nolint:nilnil // by design — caller proceeds.
	}
	conflict, action, errMsg, err := r.checkDestinationConflict(
		ctx, entry, recordingID, overwrite)
	if err != nil {
		return nil, err
	}
	if action == "" {
		return nil, nil //nolint:nilnil // by design — caller proceeds.
	}
	out := &ImportQueueEntryPayload{
		Action: action,
		Dest:   conflict.destAbsolute,
		Error:  errMsg,
	}
	if action == importActionDuplicate {
		if removeErr := storage.RemoveQueueEntry(
			ctx, r.client, queueID,
		); removeErr != nil {
			r.logger.Warn().Err(removeErr).Int64("queue_id", queueID).
				Msg("failed to remove queue entry on duplicate import")
		}
	}
	return out, nil
}

// importActionDuplicate / importActionOverwriteRequired are the
// non-Moved action values the importQueueEntry resolver returns when
// the destination-conflict check intercepts the request before
// reaching the ingest engine. The SPA renders different UX for each
// — duplicate is informational, overwrite-required prompts the user
// for explicit confirmation.
const (
	importActionDuplicate         = "duplicate"
	importActionOverwriteRequired = "overwrite_required"
)

// destConflictResult captures the destination-conflict shape the
// import mutation needs to decide whether to bail or proceed.
type destConflictResult struct {
	destAbsolute string
	destExists   bool
	isSameFile   bool
	isDuplicate  bool
}

// checkDestinationConflict reproduces the Plan-build path the
// previewQueueImport resolver runs, then stat / hash-compares the
// planned destination. Returns:
//
//   - (result, "", "", nil)             — no conflict; caller proceeds.
//   - (result, "duplicate", msg, nil)   — destination exists with
//     identical content; caller returns the duplicate payload.
//   - (result, "overwrite_required",
//     msg, nil)                         — destination exists with
//     different content + overwrite flag is false; caller returns the
//     overwrite-required payload.
//   - (zero, "", "", err)               — fatal error (load recording,
//     probe, plan-build); caller surfaces to gqlgen's errors array.
func (r *Resolver) checkDestinationConflict(
	ctx context.Context,
	entry *storage.QueueEntry,
	recordingID int64,
	overwrite bool,
) (destConflictResult, string, string, error) {
	loaded, err := storage.LoadRecording(ctx, r.client, recordingID)
	if err != nil {
		return destConflictResult{}, "", "",
			fmt.Errorf("graphql: load recording %d: %w", recordingID, err)
	}
	info, perr := r.libraryPlan.Prober.Probe(ctx, entry.FilePath)
	if perr != nil {
		return destConflictResult{}, "", "",
			fmt.Errorf("graphql: probe %s: %w", entry.FilePath, perr)
	}
	parsed := match.Parse(filepath.Base(entry.FilePath))
	plan, err := rename.BuildPlan(rename.PlanInputs{
		Recording:      loaded.Recording,
		Source:         entry.FilePath,
		LibraryRoot:    r.libraryPlan.Root,
		FolderTemplate: r.libraryPlan.FolderTemplate,
		FileTemplate:   r.libraryPlan.FileTemplate,
		MediaInfo:      info,
		Part:           parsed.PartIndex,
	})
	if err != nil {
		return destConflictResult{}, "", "",
			fmt.Errorf("graphql: build plan: %w", err)
	}
	// DVD imports route the file into a VIDEO_TS/ subfolder with
	// the source basename intact. Apply the override before
	// computing the dest so the conflict check stat's the right
	// path. Empty DiscFormat preserves the legacy AbsoluteFile().
	discFormat, _ := decodeClassificationDisc(entry.ClassificationJSON)
	if discFormat == ingest.DiscFormatDVD {
		plan.DestSubfolder = ingest.DiscDestSubfolder
		plan.DestBasename = filepath.Base(entry.FilePath)
	}
	dest := plan.AbsoluteFile()
	conflict, _ := inspectDestinationConflict(ctx, entry.FilePath, dest)
	result := destConflictResult{
		destAbsolute: dest,
		destExists:   conflict.destExists,
		isSameFile:   conflict.isSameFile,
		isDuplicate:  conflict.isDuplicate,
	}
	if !conflict.destExists {
		return result, "", "", nil
	}
	// Same-file is the library-root backfill case — the file is
	// already at its canonical location. Plan.Apply short-circuits to
	// a no-op so the import (NFO + sidecar + recording_versions row)
	// proceeds normally; no overwrite confirmation needed.
	if conflict.isSameFile {
		return result, "", "", nil
	}
	if conflict.isDuplicate {
		return result, importActionDuplicate,
			"destination already holds an identical copy of this file; nothing to import",
			nil
	}
	if !overwrite {
		return result, importActionOverwriteRequired,
			"destination already holds a different file at this path; re-submit with overwrite=true to replace",
			nil
	}
	return result, "", "", nil
}

// optBool dereferences an optional bool input, defaulting to false
// when the pointer is nil. gqlgen surfaces nullable boolean inputs
// as *bool.
func optBool(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
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
			DateVariant:    rec.DateVariant,
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
	if optBool(input.ExternallyManaged) {
		// Catalog-only previews short-circuit the Plan path: the
		// "destination" IS the source path, no folder / file template
		// applies, and the destination-conflict check is irrelevant
		// (promptbook never moves the file). The modal renders a
		// "File stays at source · {src}" line off this shape.
		return &ImportPreview{
			DestFolder:   filepath.Dir(entry.FilePath),
			DestFile:     filepath.Base(entry.FilePath),
			DestAbsolute: entry.FilePath,
			DestExists:   false,
			IsDuplicate:  false,
		}, nil
	}
	loaded, err := storage.LoadRecording(ctx, r.client, input.RecordingID)
	if err != nil {
		return nil, fmt.Errorf(
			"graphql: load recording %d: %w", input.RecordingID, err)
	}
	if r.libraryPlan.Prober == nil {
		return nil, errors.New("graphql: ingest probe not configured")
	}
	info, perr := r.libraryPlan.Prober.Probe(ctx, entry.FilePath)
	if perr != nil {
		return nil, fmt.Errorf(
			"graphql: probe %s: %w", entry.FilePath, perr)
	}
	parsed := match.Parse(filepath.Base(entry.FilePath))
	plan, err := rename.BuildPlan(rename.PlanInputs{
		Recording:      loaded.Recording,
		Source:         entry.FilePath,
		LibraryRoot:    r.libraryPlan.Root,
		FolderTemplate: r.libraryPlan.FolderTemplate,
		FileTemplate:   r.libraryPlan.FileTemplate,
		MediaInfo:      info,
		Part:           parsed.PartIndex,
	})
	if err != nil {
		return nil, fmt.Errorf("graphql: build plan: %w", err)
	}
	// DVD imports preserve the source basename inside a VIDEO_TS/
	// subfolder rather than running the file template. Override
	// the plan's destination so the preview matches what the
	// engine will actually do.
	discFormat, _ := decodeClassificationDisc(entry.ClassificationJSON)
	if discFormat == ingest.DiscFormatDVD {
		plan.DestSubfolder = ingest.DiscDestSubfolder
		plan.DestBasename = filepath.Base(entry.FilePath)
	}
	conflict, conflictErr := inspectDestinationConflict(
		ctx, entry.FilePath, plan.AbsoluteFile())
	if conflictErr != nil {
		// Hash failures already collapse to destExists=true,
		// isDuplicate=false inside the helper — log via the error
		// returned for visibility but don't fail the preview.
		_ = conflictErr
	}
	destFolder := plan.TargetFolder
	destFile := plan.TargetFile + plan.Extension
	if plan.DestBasename != "" {
		// DVD overrides: surface the rendered location the engine
		// will actually use so the preview's "destination" line
		// reads as "{recordingFolder}/VIDEO_TS/VTS_01_1.VOB".
		destFolder = filepath.Join(plan.TargetFolder, plan.DestSubfolder)
		destFile = plan.DestBasename
	}
	return &ImportPreview{
		DestFolder:   destFolder,
		DestFile:     destFile,
		DestAbsolute: plan.AbsoluteFile(),
		DestExists:   conflict.destExists,
		IsSameFile:   conflict.isSameFile,
		IsDuplicate:  conflict.isDuplicate,
	}, nil
}

// errNFORefreshNotConfigured surfaces when the regenerateRecordingNFO
// mutation or the apply-rename's post-move rewrite tries to use a
// nil nforefresh.Service. Production wires the service whenever an
// image cache is configured; tests can pass nil to exercise the
// degraded path.
var errNFORefreshNotConfigured = errors.New(
	"graphql: nfo refresh not configured")

// previewRecordingRename runs rename.BuildPlan against every version
// of the recording and returns one preview row per version. Pure
// dry-run — no files move, no DB writes. Per-version planning errors
// (probe failure, template rendering failure, ...) populate the row's
// `error` field rather than aborting the batch, so the SPA can render
// a partial result set.
func (r *Resolver) previewRecordingRename(
	ctx context.Context, recordingID int64,
) ([]*RenamePreviewItem, error) {
	if !r.libraryPlan.Configured() {
		return nil, errLibraryNotConfigured
	}
	if recordingID <= 0 {
		return nil, errors.New("graphql: recordingID must be positive")
	}
	loaded, err := storage.LoadRecording(ctx, r.client, recordingID)
	if err != nil {
		return nil, fmt.Errorf(
			"graphql: load recording %d: %w", recordingID, err)
	}
	versions, err := storage.ListVersions(ctx, r.client, recordingID)
	if err != nil {
		return nil, fmt.Errorf(
			"graphql: list versions for recording %d: %w", recordingID, err)
	}
	out := make([]*RenamePreviewItem, 0, len(versions))
	for _, v := range versions {
		out = append(out, r.renamePreviewForVersion(ctx, loaded.Recording, v))
	}
	return out, nil
}

// renamePreviewForVersion builds the per-version preview row. Probe
// failures, template failures, etc. land in the row's `error` field
// so the caller can surface them without aborting the batch.
func (r *Resolver) renamePreviewForVersion(
	ctx context.Context,
	rec encoraRecordingValue,
	v storage.RecordingVersion,
) *RenamePreviewItem {
	item := &RenamePreviewItem{
		VersionID: v.ID,
		Source:    v.FilePath,
	}
	plan, err := r.buildVersionPlan(ctx, rec, v)
	if err != nil {
		item.Error = err.Error()
		return item
	}
	item.Destination = plan.AbsoluteFile()
	item.WillMove = item.Source != item.Destination
	return item
}

// applyRecordingRename runs BuildPlan + Plan.Apply for every version
// of the recording. Per-version failures populate the row's `error`
// field rather than aborting the batch. After every version moves
// successfully the recording's NFO is rewritten at the new folder so
// the file the media server scans matches the canonical name. The
// old source folder is deliberately NOT auto-deleted; the user
// manages cleanup so an empty parent doesn't disappear out from under
// them.
func (r *Resolver) applyRecordingRename(
	ctx context.Context, recordingID int64,
) ([]*RenameResultItem, error) {
	if !r.libraryPlan.Configured() {
		return nil, errLibraryNotConfigured
	}
	if recordingID <= 0 {
		return nil, errors.New("graphql: recordingID must be positive")
	}
	loaded, err := storage.LoadRecording(ctx, r.client, recordingID)
	if err != nil {
		return nil, fmt.Errorf(
			"graphql: load recording %d: %w", recordingID, err)
	}
	versions, err := storage.ListVersions(ctx, r.client, recordingID)
	if err != nil {
		return nil, fmt.Errorf(
			"graphql: list versions for recording %d: %w", recordingID, err)
	}
	out := make([]*RenameResultItem, 0, len(versions))
	allMovedOrCanonical := true
	for _, v := range versions {
		item := r.applyRenameForVersion(ctx, loaded.Recording, v)
		out = append(out, item)
		if item.Error != "" {
			allMovedOrCanonical = false
		}
	}
	// Best-effort NFO rewrite at the new folder. We only fan out when
	// every version landed (or was already canonical) so the writer
	// targets the canonical destination rather than an in-flight mix
	// of old + new locations. Failures are logged, not surfaced — the
	// rename outcome is the headline result for this mutation.
	if allMovedOrCanonical && len(versions) > 0 && r.nfoRefresh != nil {
		if rfErr := r.nfoRefresh.RewriteForRecording(ctx, recordingID); rfErr != nil {
			r.logger.Warn().
				Err(rfErr).
				Int64("recording_id", recordingID).
				Msg("graphql: nfo rewrite after rename failed")
		}
	}
	return out, nil
}

// applyRenameForVersion builds + applies the plan for one version,
// then updates the recording_versions row's file_path on success.
// The result row carries the planned destination either way so the
// SPA can show "would have gone to..." on failure.
func (r *Resolver) applyRenameForVersion(
	ctx context.Context,
	rec encoraRecordingValue,
	v storage.RecordingVersion,
) *RenameResultItem {
	item := &RenameResultItem{
		VersionID: v.ID,
		Source:    v.FilePath,
	}
	plan, err := r.buildVersionPlan(ctx, rec, v)
	if err != nil {
		item.Error = err.Error()
		return item
	}
	item.Destination = plan.AbsoluteFile()
	if item.Source == item.Destination {
		// Already canonical — record as a no-op (moved=false, no
		// error) so the SPA can render "Already canonical".
		return item
	}
	dest, applyErr := plan.Apply()
	if applyErr != nil {
		item.Error = applyErr.Error()
		return item
	}
	item.Destination = dest
	item.Moved = true
	// Persist the new file_path on the existing row. UpsertVersion
	// keys on (recording_id, file_path), so we delete the stale row
	// first to avoid leaving the old path behind.
	if delErr := storage.DeleteVersion(ctx, r.client, v.ID); delErr != nil {
		item.Error = fmt.Errorf(
			"delete stale version row: %w", delErr).Error()
		return item
	}
	updated := v
	updated.FilePath = dest
	if upErr := storage.UpsertVersion(ctx, r.client, updated); upErr != nil {
		item.Error = fmt.Errorf(
			"persist new version path: %w", upErr).Error()
		return item
	}
	return item
}

// buildVersionPlan probes + parses + renders a rename.Plan for a
// single version. Pulled out so preview + apply share the exact same
// per-version path; callers differ only in what they do with the
// resulting Plan. Returns a typed error on probe / template failure;
// the caller surfaces it on the per-row `error` field.
func (r *Resolver) buildVersionPlan(
	ctx context.Context,
	rec encoraRecordingValue,
	v storage.RecordingVersion,
) (*rename.Plan, error) {
	if r.libraryPlan.Prober == nil {
		return nil, errors.New("graphql: ingest probe not configured")
	}
	info, perr := r.libraryPlan.Prober.Probe(ctx, v.FilePath)
	if perr != nil {
		return nil, fmt.Errorf("probe %s: %w", v.FilePath, perr)
	}
	parsed := match.Parse(filepath.Base(v.FilePath))
	plan, err := rename.BuildPlan(rename.PlanInputs{
		Recording:      rec,
		Source:         v.FilePath,
		LibraryRoot:    r.libraryPlan.Root,
		FolderTemplate: r.libraryPlan.FolderTemplate,
		FileTemplate:   r.libraryPlan.FileTemplate,
		MediaInfo:      info,
		Part:           parsed.PartIndex,
	})
	if err != nil {
		return nil, fmt.Errorf("build plan: %w", err)
	}
	return plan, nil
}

// regenerateRecordingNFO is the resolver body for the
// regenerateRecordingNFO mutation. Thin wrapper around
// nforefresh.Service.RewriteForRecording — the service's own contract
// treats "no version row" as a successful no-op, which we surface
// straight through (ok=true). Errors land in `error` with ok=false.
func (r *Resolver) regenerateRecordingNFO(
	ctx context.Context, recordingID int64,
) (*RegenerateNFOResult, error) {
	if recordingID <= 0 {
		return nil, errors.New("graphql: recordingID must be positive")
	}
	if r.nfoRefresh == nil {
		return nil, errNFORefreshNotConfigured
	}
	if err := r.nfoRefresh.RewriteForRecording(ctx, recordingID); err != nil {
		// Surface the failure on the payload rather than as a GraphQL
		// error so the SPA can render an inline message; the resolver
		// itself succeeded (it ran the rewrite), the rewrite is the
		// thing that returned the error.
		//nolint:nilerr // intentional: error surfaces on the payload.
		return &RegenerateNFOResult{Ok: false, Error: err.Error()}, nil
	}
	return &RegenerateNFOResult{Ok: true}, nil
}

// setRecordingExternallyManaged is the resolver body for the
// setRecordingExternallyManaged mutation. Flipping ON drops the
// .promptbook-externally-managed sentinel in the recording's source
// folder (the parent dir of the first version's file_path) and
// deletes any existing movie.nfo at that folder; flipping OFF
// removes the sentinel. Files stay in place in either direction —
// the user can hit "Preview rename" afterwards if they want
// promptbook to take ownership.
//
// The disk writes are best-effort: a missing source folder (no
// version row, or the file is gone) just skips those steps and the
// flag still flips. The mutation only fails when the DB write fails
// or the recording id doesn't resolve.
func (r *Resolver) setRecordingExternallyManaged(
	ctx context.Context, recordingID int64, externallyManaged bool,
) (*ent.Recording, error) {
	if recordingID <= 0 {
		return nil, errors.New("graphql: recordingID must be positive")
	}
	versions, err := storage.ListVersions(ctx, r.client, recordingID)
	if err != nil {
		return nil, fmt.Errorf(
			"graphql: list versions for recording %d: %w", recordingID, err)
	}
	dirs := uniqueVersionDirs(versions)
	if externallyManaged {
		r.flipExternallyManagedOn(dirs, recordingID)
	} else {
		r.flipExternallyManagedOff(dirs, recordingID)
	}
	if flagErr := storage.SetRecordingExternallyManaged(
		ctx, r.client, recordingID, externallyManaged,
	); flagErr != nil {
		return nil, fmt.Errorf(
			"graphql: set externally_managed for %d: %w", recordingID, flagErr)
	}
	row, getErr := r.client.Recording.Get(ctx, recordingID)
	if getErr != nil {
		return nil, fmt.Errorf(
			"graphql: reload recording %d after toggle: %w", recordingID, getErr)
	}
	return row, nil
}

// setRecordingPrivateNotes is the resolver body for the
// setRecordingPrivateNotes mutation. Overwrites the local-only
// private_notes column verbatim — no trimming, no sanitization (the
// SPA's textarea is the trust boundary). Empty string is the
// "cleared notes" path. Reloads the recording so the returned
// payload reflects the just-saved value.
func (r *Resolver) setRecordingPrivateNotes(
	ctx context.Context, recordingID int64, notes string,
) (*ent.Recording, error) {
	if recordingID <= 0 {
		return nil, errors.New("graphql: recordingID must be positive")
	}
	if err := storage.SetRecordingPrivateNotes(
		ctx, r.client, recordingID, notes,
	); err != nil {
		return nil, fmt.Errorf(
			"graphql: set private_notes for %d: %w", recordingID, err)
	}
	row, getErr := r.client.Recording.Get(ctx, recordingID)
	if getErr != nil {
		return nil, fmt.Errorf(
			"graphql: reload recording %d after notes save: %w", recordingID, getErr)
	}
	return row, nil
}

// uniqueVersionDirs returns the deduplicated set of parent
// directories across the recording's versions. Multipart recordings
// share one folder; loose-file legacy imports may sit in disparate
// directories, in which case the toggle writes the sentinel to all
// of them so a later scan recognizes any path-drift.
func uniqueVersionDirs(versions []storage.RecordingVersion) []string {
	seen := make(map[string]struct{}, len(versions))
	out := make([]string, 0, len(versions))
	for _, v := range versions {
		dir := filepath.Dir(v.FilePath)
		if dir == "" || dir == "." {
			continue
		}
		if _, ok := seen[dir]; ok {
			continue
		}
		seen[dir] = struct{}{}
		out = append(out, dir)
	}
	return out
}

// flipExternallyManagedOn writes the sentinel + deletes any existing
// movie.nfo in each of the recording's version directories.
// Best-effort: every failure logs at warn and we continue, since the
// flag flip in the DB is the load-bearing assertion.
func (r *Resolver) flipExternallyManagedOn(dirs []string, recordingID int64) {
	for _, dir := range dirs {
		sentinel := filepath.Join(dir, ingest.ExternallyManagedSentinel)
		if err := os.WriteFile(sentinel, nil, externallyManagedFilePerm); err != nil {
			r.logger.Warn().Err(err).Str("path", sentinel).
				Int64("recording_id", recordingID).
				Msg("failed to write externally-managed sentinel during toggle")
		}
		nfoPath := filepath.Join(dir, "movie.nfo")
		if err := os.Remove(nfoPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			r.logger.Warn().Err(err).Str("path", nfoPath).
				Int64("recording_id", recordingID).
				Msg("failed to remove movie.nfo during externally-managed toggle")
		}
	}
}

// flipExternallyManagedOff removes the sentinel from each version
// directory. The file stays where it is — the user can hit
// "Preview rename" afterwards to move it under canonical templates.
func (r *Resolver) flipExternallyManagedOff(dirs []string, recordingID int64) {
	for _, dir := range dirs {
		sentinel := filepath.Join(dir, ingest.ExternallyManagedSentinel)
		if err := os.Remove(sentinel); err != nil && !errors.Is(err, fs.ErrNotExist) {
			r.logger.Warn().Err(err).Str("path", sentinel).
				Int64("recording_id", recordingID).
				Msg("failed to remove externally-managed sentinel during toggle")
		}
	}
}

// externallyManagedFilePerm matches the ingest engine's sidecar
// permission so the sentinel stays world-readable across consumers
// (Jellyfin / Plex containers running as different uids).
const externallyManagedFilePerm = 0o644

// encoraRecordingValue aliases encora.Recording so the per-version
// helpers can carry the value type without each call site importing
// the encora package directly. Pulled into a named type so the
// signatures stay short.
type encoraRecordingValue = encora.Recording
