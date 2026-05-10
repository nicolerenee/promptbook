package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/ent/collectionentry"
	"github.com/nicolerenee/promptbook/internal/ent/recording"
	"github.com/nicolerenee/promptbook/internal/ent/show"
	"github.com/nicolerenee/promptbook/internal/ent/syncrun"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// API tunables.
const (
	defaultListLimit = 50
	itemsKey         = "items"
	limitKey         = "limit"
	offsetKey        = "offset"

	// allTabLabel is the display label for the "no filter" entry in
	// every tab-style nav (status / kind / mismatch type). Lifted into
	// a shared constant because goconst flags the duplication once
	// three or more sibling tab tables exist.
	allTabLabel = "All"

	// sortKeyDate / sortKeyMaster are the sort-key tokens shared by
	// the recordings + wants list comparators. Hoisted into constants
	// because goconst trips on the literal once the same token shows
	// up in three sibling switch statements.
	sortKeyDate   = "date"
	sortKeyMaster = "master"
)

// RecordingListItem is the shape returned by /api/v1/recordings and
// embedded in the home page view-model. Status and the per-recording
// format strings come from the storage.RecordingState reconciler so the
// JSON and HTML pages render the same five-status taxonomy.
//
// LocalPosterURL is the /images/... path of the show's chosen banner
// image (recordings on the library grid render the show banner, not a
// per-recording poster). Empty when the cache is disabled or no banner
// is on disk yet. The frontend prefers it over reaching for an
// upstream URL we don't trust to stay reachable.
type RecordingListItem struct {
	ID             int64  `json:"id"`
	ShowID         int64  `json:"show_id"`
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
	LocalPosterURL string `json:"local_poster_url"`
}

// wantsListItem is the JSON shape returned by /api/v1/wants. It surfaces
// the wants row's last_synced_at as wants_added so the UI can render an
// "Added" timestamp.
type wantsListItem struct {
	ID             int64   `json:"id"`
	Show           string  `json:"show"`
	Tour           string  `json:"tour"`
	DateFull       string  `json:"date_full"`
	DateMonthKnown bool    `json:"date_month_known"`
	DateDayKnown   bool    `json:"date_day_known"`
	Master         string  `json:"master"`
	InCollection   bool    `json:"in_collection"`
	InWants        bool    `json:"in_wants"`
	WantsAdded     *string `json:"wants_added"`
}

func (s *Server) routes() {
	api := s.echo.Group("/api/v1")
	api.GET("/health", s.handleHealth)
	api.GET("/profile", s.handleProfile)
	api.GET("/recordings", s.handleListRecordings)
	api.GET("/recordings/:id", s.handleGetRecording)
	// Per-recording overlay surfaces. The text override + the burn-in
	// opt-out sit on recording_image_choices. POSTs persist the value
	// and (for the override) trigger a re-render of poster.jpg.
	api.POST("/recordings/:id/overlay", s.handleSetOverlay)
	api.POST("/recordings/:id/overlay-disabled", s.handleSetOverlayDisabled)
	// User image uploads. Multipart "file" field, 10 MiB cap, decoded
	// + re-encoded as JPEG into the canonical slot under the v2 layout.
	// poster-upload writes to poster-src.jpg and triggers a render so
	// the burned-in poster.jpg lands on disk before the response
	// returns.
	api.POST("/recordings/:id/fanart-upload", s.handleUploadRecordingFanart)
	api.POST("/recordings/:id/poster-upload", s.handleUploadRecordingPoster)
	api.POST("/shows/:id/banner-upload", s.handleUploadShowBanner)
	api.POST("/actors/:id/headshot-upload", s.handleUploadActorHeadshot)
	// "Set from URL" endpoints: the picker UI POSTs the chosen upstream
	// URL; the server downloads it into the slot. These replace the
	// indexed-pick endpoints from the v1 cache layout.
	api.POST("/recordings/:id/fanart-from-url", s.handleSetRecordingFanartFromURL)
	api.POST("/recordings/:id/poster-from-url", s.handleSetRecordingPosterFromURL)
	api.POST("/shows/:id/banner-from-url", s.handleSetShowBannerFromURL)
	api.POST("/actors/:id/headshot-from-url", s.handleSetActorHeadshotFromURL)
	// Picker "options" endpoints: the modal hits these on open to
	// surface the live upstream URLs the user can pick from. These
	// are the ONLY API surfaces that talk StageMedia / Encora at
	// request time — every other endpoint serves the local DB +
	// cached images on disk.
	api.GET("/shows/:id/poster-options", s.handleListShowPosterOptions)
	api.GET("/recordings/:id/poster-options", s.handleListRecordingPosterOptions)
	api.GET("/recordings/:id/fanart-options", s.handleListRecordingFanartOptions)
	api.GET("/actors/:id/headshot-options", s.handleListActorHeadshotOptions)
	// Upstream image proxy. The picker thumbnails route through here
	// because Safari aborts cross-origin <img> loads from localhost to
	// stagemedia.me even with no-referrer. Allowlisted to StageMedia
	// + Encora hosts only.
	api.GET("/upstream-image", s.handleUpstreamImageProxy)
	// "Refresh from upstream" endpoints: fire the per-entity
	// refresh-images job with optional force=true. The picker modal
	// footer calls these when the user wants to re-pull StageMedia /
	// Encora and let the job's atomic write update the slot files.
	api.POST("/recordings/:id/refresh-images", s.handleRefreshRecordingImages)
	api.POST("/shows/:id/refresh-images", s.handleRefreshShowImages)
	api.GET("/wants", s.handleListWants)
	api.GET("/sync/runs", s.handleSyncRuns)
	api.GET("/queue", s.handleListQueue)
	api.POST("/queue/:id/import", s.handleImportQueue)
	api.GET("/people", s.handleListPeople)
	api.GET("/people/:id", s.handleGetPerson)
	// Shows: by-show aggregate list + per-show detail.
	api.GET("/shows", s.handleListShows)
	api.GET("/shows/:id", s.handleGetShow)
	api.GET("/history", s.handleListHistory)
	api.GET("/mismatches", s.handleListMismatches)
	api.GET("/settings", s.handleSettings)
	api.POST("/apply", s.handleAPIApply)

	// Destructive Encora endpoints. These are user-initiated single-
	// action POSTs (a click on the wants page or recording detail),
	// deliberately separate from the apply pipeline so the reconciler
	// can never trigger a remove. UI exposure lands in Wave 12 behind
	// an explicit confirmation gate.
	api.POST("/encora/collection/:id/remove", s.handleRemoveFromCollection)
	api.POST("/encora/wants/:id/remove", s.handleRemoveFromWants)
	api.POST("/encora/wants/:id/add", s.handleAddToWants)

	// Manual re-render of the burned-in poster. Overlay/upload paths
	// also invoke imagerender.Regenerate directly; this endpoint is the
	// explicit "Re-render" affordance + a smoke-test surface. 503 when
	// the renderer is nil.
	api.POST("/recordings/:id/regenerate-poster", s.handleRegeneratePoster)

	// Scheduled-jobs API. The Mithril /jobs page polls these every
	// 5s; the runner returns 503 when not wired (tests + no-config).
	api.GET("/jobs/scheduled", s.handleListScheduledJobs)
	api.GET("/jobs/queue", s.handleListJobQueue)
	api.POST("/jobs/scheduled/:name/run", s.handleRunJob)

	// SPA catch-all. Echo prefers more-specific matches, so /api/v1/*
	// (registered above) and /static/* (registered in server.New) win
	// over this for their respective prefixes. Every other GET — `/`,
	// `/recordings/:id`, `/wants`, `/queue`, …, `/settings`, plus any
	// future client-rendered route — lands on the SPA shell and the
	// Mithril router resolves the path client-side.
	s.echo.GET("/*", s.handleSPA)
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
	sortKey := c.QueryParam("sort")
	dir := parseSortDir(c)

	statuses := parseStatusParam(statusParam)

	// status takes precedence over the legacy owned filter when both are
	// supplied; owned is honored only when status is absent.
	ownedFilter := owned
	if statusParam != "" {
		ownedFilter = ""
	}

	ctx := c.Request().Context()
	items, total, err := loadStatefulRecordings(
		ctx, s.db, statuses, ownedFilter, sortKey, dir, limit, offset,
	)
	if err != nil {
		return err
	}

	// Decorate each row with the show's banner URL. Selection is
	// implicit by file existence under the v2 layout, so the helper
	// just stat()s each unique show banner path — no SQL fan-out.
	s.decorateLocalPosters(items)

	return c.JSON(http.StatusOK, pageEnvelope(items, total, limit, offset))
}

// decorateLocalPosters sets LocalPosterURL on every item to the
// recording's burned-in poster (recordings/<id>/poster.jpg). Library
// grid view renders this — each recording gets ITS poster, not the
// show's banner, so multiple recordings of the same show don't all
// look identical. The /images/* route falls through to a generated
// placeholder when the file isn't on disk yet.
func (s *Server) decorateLocalPosters(items []RecordingListItem) {
	cache := s.ImageCache()
	if cache == nil || cache.Disabled() || len(items) == 0 {
		return
	}
	for i := range items {
		items[i].LocalPosterURL = cache.RecordingPosterURL(items[i].ID)
	}
}

// recordingDetailResponse wraps storage.LoadedRecording with the
// server-side enrichment fields. The embedded *storage.LoadedRecording
// flattens its PascalCase fields into the same JSON object so existing
// consumers see no shape change beyond the new lower-case keys.
//
// LocalFanartURL is the /images/... path of the recording's fanart
// (wide, raw — no overlay). LocalPosterURL is the burned-in poster
// (vertical, with overlay). Both are empty strings when the slot file
// isn't on disk yet.
//
// Cast performers grow a parallel local_headshot_url field on
// LoadedRecording.Cast at JSON marshal time via castWithLocalHeadshots.
type recordingDetailResponse struct {
	*storage.LoadedRecording

	Cast           []castEntryWithHeadshot `json:"cast"`
	NFOContent     string                  `json:"nfo_content"`
	NFOModifiedAt  *time.Time              `json:"nfo_modified_at"`
	LocalFanartURL string                  `json:"local_fanart_url"`
	LocalPosterURL string                  `json:"local_poster_url"`

	// OverlayTextOverride is the user-supplied burned-in label, or
	// nil when no override has been saved (the renderer uses its own
	// auto-derived "show · tour · date" string in that case).
	OverlayTextOverride *string `json:"overlay_text_override"`
	// OverlayDisabled, when true, tells the renderer to skip the
	// playbill-style band and copy poster-src.jpg through to
	// poster.jpg unchanged.
	OverlayDisabled bool `json:"overlay_disabled"`
}

// castEntryWithHeadshot mirrors storage.ResolvedCastEntry but adds a
// local_headshot_url string sourced from the image cache. The shape
// stays decode-compatible with the JSON the SPA already consumes —
// JSON marshal of a Go struct emits fields in declared order, but the
// frontend keys off field names, so the new column is purely additive.
type castEntryWithHeadshot struct {
	storage.ResolvedCastEntry

	LocalHeadshotURL string `json:"local_headshot_url"`
}

func (s *Server) handleGetRecording(c echo.Context) error {
	id, err := storage.ParseRecordingID(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	ctx := c.Request().Context()
	loaded, err := storage.LoadRecording(ctx, s.db, id)
	if errors.Is(err, storage.ErrRecordingNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}
	if err != nil {
		return err
	}

	castWithHeadshots := s.castWithLocalHeadshots(loaded.Cast)
	nfoContent, nfoModifiedAt := s.readNFOForRecording(loaded)

	var localFanartURL, localPosterURL string
	if cache := s.ImageCache(); cache != nil && !cache.Disabled() {
		// Fanart is the only slot that does NOT fall through to a
		// generated placeholder — emit "" when the file isn't on disk
		// so the picker's "No image on disk yet" branch renders
		// instead of a broken-image icon.
		if cache.HasRecordingFanart(id) {
			localFanartURL = cache.RecordingFanartURL(id)
		}
		localPosterURL = cache.RecordingPosterURL(id)
	}

	// Image choices are best-effort enrichment — a query failure
	// shouldn't break the whole detail page. Log and serve a
	// zero-valued ImageChoice.
	choice, choiceErr := storage.GetImageChoice(ctx, s.db, id)
	if choiceErr != nil {
		s.logger.Warn().
			Err(choiceErr).
			Int64("recording_id", id).
			Msg("get image choice failed; serving defaults")
		choice = storage.ImageChoice{}
	}

	return c.JSON(http.StatusOK, recordingDetailResponse{
		LoadedRecording:     loaded,
		Cast:                castWithHeadshots,
		NFOContent:          nfoContent,
		NFOModifiedAt:       nfoModifiedAt,
		LocalFanartURL:      localFanartURL,
		LocalPosterURL:      localPosterURL,
		OverlayTextOverride: choice.OverlayTextOverride,
		OverlayDisabled:     choice.OverlayDisabled,
	})
}

// castWithLocalHeadshots wraps each ResolvedCastEntry with the local
// headshot URL (when cached). Returns a non-nil empty slice when the
// input is empty so the JSON renders [] rather than null.
func (s *Server) castWithLocalHeadshots(
	cast []storage.ResolvedCastEntry,
) []castEntryWithHeadshot {
	out := make([]castEntryWithHeadshot, len(cast))
	cache := s.ImageCache()
	for i, ce := range cast {
		out[i] = castEntryWithHeadshot{ResolvedCastEntry: ce}
		if cache != nil && !cache.Disabled() {
			out[i].LocalHeadshotURL = cache.HeadshotURL(ce.Performer.PerformerID)
		}
	}
	return out
}

// readNFOForRecording reads the movie.nfo sitting next to the
// recording's first version on disk. Returns ("", nil) when there are
// no versions, the file is absent, or any read/stat error occurs —
// callers don't need to distinguish "no nfo" from "bad nfo" because
// both render as the synthetic preview on the page.
func (s *Server) readNFOForRecording(loaded *storage.LoadedRecording) (string, *time.Time) {
	if len(loaded.Versions) == 0 {
		return "", nil
	}
	nfoPath := filepath.Join(filepath.Dir(loaded.Versions[0].FilePath), "movie.nfo")
	content, modifiedAt, err := readNFO(nfoPath)
	if err != nil {
		// fs.ErrNotExist is the common path; logging only when a real
		// error happens keeps the per-request output quiet for the
		// overwhelming majority of recordings that haven't been
		// renamed/written yet.
		if !errors.Is(err, fs.ErrNotExist) {
			s.logger.Warn().
				Err(err).
				Int64("recording_id", loaded.Recording.ID).
				Str("nfo_path", nfoPath).
				Msg("nfo: read failed; returning empty content")
		}
		return "", nil
	}
	return content, modifiedAt
}

// readNFO reads the file at path and returns its content + ModTime.
// Errors from ReadFile or Stat are returned verbatim so callers can
// branch on os.IsNotExist if needed.
func readNFO(path string) (string, *time.Time, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("read nfo %s: %w", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", nil, fmt.Errorf("stat nfo %s: %w", path, err)
	}
	mod := info.ModTime()
	return string(b), &mod, nil
}

func (s *Server) handleListWants(c echo.Context) error {
	limit := paramInt(c, "limit", defaultListLimit)
	offset := paramInt(c, "offset", 0)
	sortKey := c.QueryParam("sort")
	dir := parseSortDir(c)

	ctx := c.Request().Context()
	total, err := countWantsList(ctx, s.db)
	if err != nil {
		return err
	}
	items, err := loadWantsList(ctx, s.db, sortKey, dir, limit, offset)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, pageEnvelope(items, total, limit, offset))
}

// syncRunsListLimit is how many sync_runs rows /api/v1/sync-runs
// surfaces. The list fronts an admin debug surface and a small fixed
// cap is enough to spot runaway error patterns.
const syncRunsListLimit = 30

func (s *Server) handleSyncRuns(c echo.Context) error {
	rows, err := s.db.SyncRun.Query().
		Order(syncrun.ByID(entsql.OrderDesc())).
		Limit(syncRunsListLimit).
		All(c.Request().Context())
	if err != nil {
		return err
	}

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

	out := make([]runRow, 0, len(rows))
	for _, r := range rows {
		row := runRow{
			ID:                 int64(r.ID),
			Kind:               r.Kind,
			StartedAt:          r.StartedAt.Format("2006-01-02 15:04:05"),
			OkCount:            r.OkCount,
			ErrorCount:         r.ErrorCount,
			RateLimitRemaining: r.RateLimitRemaining,
			ErrorText:          r.ErrorText,
		}
		if r.FinishedAt != nil {
			f := r.FinishedAt.Format("2006-01-02 15:04:05")
			row.FinishedAt = &f
		}
		out = append(out, row)
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
//
// Sort happens in memory after filter + meta load — the recordings
// list is bounded by maxStateScan (1024) so an O(n log n) sort is
// cheap. sortKey is one of the keys in recordingsSortKeys; unknown
// keys fall back to the default (show name asc, then date asc).
//
// Returns (page, total) where total is the count of rows matching the
// status + owned filter (i.e. the SPA can compute "Page N of M"
// without re-querying).
func loadStatefulRecordings(
	ctx context.Context,
	client *ent.Client,
	statuses []storage.Status,
	ownedFilter, sortKey string,
	dir sortDir,
	limit, offset int,
) ([]RecordingListItem, int, error) {
	// Pull all matching states (ListStates does its own pagination, but
	// we want to apply the legacy owned filter before paginating, so ask
	// for a wide window and slice it ourselves).
	listOpts := storage.ListStatesOptions{Status: statuses, Limit: maxStateScan}
	states, err := storage.ListStates(ctx, client, listOpts)
	if err != nil {
		return nil, 0, fmt.Errorf("list states: %w", err)
	}

	if ownedFilter != "" {
		states = applyOwnedFilter(states, ownedFilter)
	}

	// Resolve metadata for the entire filtered set so we can sort by
	// show / date / master before paginating. Page-size is capped at
	// maxStateScan (1024), well within "load once" territory.
	meta, err := loadRecordingMeta(ctx, client, states)
	if err != nil {
		return nil, 0, err
	}

	full := make([]RecordingListItem, 0, len(states))
	for _, st := range states {
		m := meta[st.RecordingID]
		full = append(full, RecordingListItem{
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
		})
	}

	sortRecordings(full, sortKey, dir)

	total := len(full)
	if offset >= total {
		return []RecordingListItem{}, total, nil
	}
	end := min(offset+limit, total)
	return full[offset:end], total, nil
}

// sortRecordings orders items in-place by the supplied sortKey + dir.
// Unknown keys fall back to the default (show name asc, then
// date_full asc, with id as the final stable tiebreak).
func sortRecordings(items []RecordingListItem, sortKey string, dir sortDir) {
	cmp := recordingsCompareFn(sortKey)
	desc := dir == sortDesc
	sort.SliceStable(items, func(i, j int) bool {
		c := cmp(items[i], items[j])
		if desc {
			c = -c
		}
		if c != 0 {
			return c < 0
		}
		// Stable tiebreak on id so equal sort keys read deterministically
		// and pagination doesn't shuffle rows on a refresh.
		return items[i].ID < items[j].ID
	})
}

// recordingsCompareFn maps a sort key to a stable comparator returning
// -1 / 0 / +1. Unknown keys fall back to the default (show name then
// date_full then tour). Empty local_format always pins last so unmatched
// rows don't dominate the top of an asc sort — mirrors the legacy
// client-side cmpEmptyLast behavior.
func recordingsCompareFn(key string) func(a, b RecordingListItem) int {
	switch key {
	case "status":
		return func(a, b RecordingListItem) int { return cmpString(a.Status, b.Status) }
	case sortKeyDate:
		return func(a, b RecordingListItem) int { return cmpString(a.DateFull, b.DateFull) }
	case sortKeyMaster:
		return func(a, b RecordingListItem) int { return cmpString(a.Master, b.Master) }
	case "local_format":
		return func(a, b RecordingListItem) int { return cmpEmptyLast(a.LocalFormat, b.LocalFormat) }
	default: // "recording" or unknown.
		return func(a, b RecordingListItem) int {
			if c := cmpString(a.Show, b.Show); c != 0 {
				return c
			}
			if c := cmpString(a.DateFull, b.DateFull); c != 0 {
				return c
			}
			return cmpString(a.Tour, b.Tour)
		}
	}
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
	showID     int64
	show       string
	tour       string
	dateFull   string
	monthKnown bool
	dayKnown   bool
	master     string
}

// loadRecordingMeta resolves show/tour/date/master for a slice of
// states in a single ent query keyed on recording_id, then a follow-up
// shows fetch to pull the show name. Recordings that don't exist in
// the recordings table (orphaned versions sans payload) surface as the
// zero value.
func loadRecordingMeta(
	ctx context.Context,
	client *ent.Client,
	states []storage.RecordingState,
) (map[int64]recordingMeta, error) {
	if len(states) == 0 {
		return map[int64]recordingMeta{}, nil
	}
	ids := make([]int64, len(states))
	for i, st := range states {
		ids[i] = st.RecordingID
	}

	recs, err := client.Recording.Query().
		Where(recording.IDIn(ids...)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query recording meta: %w", err)
	}

	showIDSet := make(map[int64]struct{}, len(recs))
	for _, r := range recs {
		showIDSet[r.ShowID] = struct{}{}
	}
	showIDs := make([]int64, 0, len(showIDSet))
	for id := range showIDSet {
		showIDs = append(showIDs, id)
	}
	showNames := map[int64]string{}
	if len(showIDs) > 0 {
		shows, sErr := client.Show.Query().
			Where(show.IDIn(showIDs...)).
			All(ctx)
		if sErr != nil {
			return nil, fmt.Errorf("query shows: %w", sErr)
		}
		for _, sh := range shows {
			showNames[sh.ID] = sh.Name
		}
	}

	out := make(map[int64]recordingMeta, len(recs))
	for _, r := range recs {
		out[r.ID] = recordingMeta{
			showID:     r.ShowID,
			show:       showNames[r.ShowID],
			tour:       r.Tour,
			dateFull:   r.DateFull,
			monthKnown: r.DateMonthKnown,
			dayKnown:   r.DateDayKnown,
			master:     r.Master,
		}
	}
	return out, nil
}

// countWantsList returns the count of rows the wants list will surface
// (mirrors the filter in loadWantsList exactly): wants whose recording
// is NOT also in the collection. Powers the SPA pagination indicator.
func countWantsList(ctx context.Context, client *ent.Client) (int, error) {
	wantIDs, err := client.WantsEntry.Query().IDs(ctx)
	if err != nil {
		return 0, fmt.Errorf("count wants list: %w", err)
	}
	if len(wantIDs) == 0 {
		return 0, nil
	}
	colIDs, err := client.CollectionEntry.Query().
		Where(collectionentry.IDIn(wantIDs...)).
		IDs(ctx)
	if err != nil {
		return 0, fmt.Errorf("count wants ∩ collection: %w", err)
	}
	colSet := make(map[int64]struct{}, len(colIDs))
	for _, id := range colIDs {
		colSet[id] = struct{}{}
	}
	n := 0
	for _, id := range wantIDs {
		if _, ok := colSet[id]; !ok {
			n++
		}
	}
	return n, nil
}

// loadWantsList queries every wants row joined with its recording / show
// metadata and surfaces wants.last_synced_at as the per-row WantsAdded
// timestamp. Rows are returned newest-added first by default so the
// UI's "added" timeline reads chronologically; sortKey can override.
// Excludes wants whose recording is also in the collection — those are
// no longer "wants" from a UX standpoint.
//
// The legacy implementation pushed sort + pagination into SQL; the ent
// version pulls every want, filters out the collection overlap, then
// sorts + paginates in memory. Wants lists run O(hundreds) of rows in
// practice — well within materialize-once territory.
func loadWantsList(
	ctx context.Context,
	client *ent.Client,
	sortKey string,
	dir sortDir,
	limit, offset int,
) ([]wantsListItem, error) {
	wants, err := client.WantsEntry.Query().All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query wants list: %w", err)
	}
	if len(wants) == 0 {
		return []wantsListItem{}, nil
	}

	wantIDs := make([]int64, len(wants))
	wantsByID := make(map[int64]*ent.WantsEntry, len(wants))
	for i, w := range wants {
		wantIDs[i] = w.ID
		wantsByID[w.ID] = w
	}

	// Recording metadata for every want id.
	recs, err := client.Recording.Query().
		Where(recording.IDIn(wantIDs...)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query wants recordings: %w", err)
	}

	// Filter out wants whose recording is also in the collection.
	colIDs, err := client.CollectionEntry.Query().
		Where(collectionentry.IDIn(wantIDs...)).
		IDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("query wants ∩ collection: %w", err)
	}
	colSet := make(map[int64]struct{}, len(colIDs))
	for _, id := range colIDs {
		colSet[id] = struct{}{}
	}

	showIDSet := make(map[int64]struct{})
	for _, r := range recs {
		if _, drop := colSet[r.ID]; drop {
			continue
		}
		showIDSet[r.ShowID] = struct{}{}
	}
	showIDs := make([]int64, 0, len(showIDSet))
	for id := range showIDSet {
		showIDs = append(showIDs, id)
	}
	showNames := map[int64]string{}
	if len(showIDs) > 0 {
		shows, sErr := client.Show.Query().Where(show.IDIn(showIDs...)).All(ctx)
		if sErr != nil {
			return nil, fmt.Errorf("query wants shows: %w", sErr)
		}
		for _, sh := range shows {
			showNames[sh.ID] = sh.Name
		}
	}

	out := make([]wantsListItem, 0, len(recs))
	for _, r := range recs {
		if _, drop := colSet[r.ID]; drop {
			continue
		}
		w := wantsByID[r.ID]
		ts := w.LastSyncedAt.Format("2006-01-02 15:04:05")
		out = append(out, wantsListItem{
			ID:             r.ID,
			Show:           showNames[r.ShowID],
			Tour:           r.Tour,
			DateFull:       r.DateFull,
			DateMonthKnown: r.DateMonthKnown,
			DateDayKnown:   r.DateDayKnown,
			Master:         r.Master,
			InCollection:   false,
			InWants:        true,
			WantsAdded:     &ts,
		})
	}

	sortWants(out, sortKey, dir)

	if offset >= len(out) {
		return []wantsListItem{}, nil
	}
	end := min(offset+limit, len(out))
	return out[offset:end], nil
}

// sortWants orders the in-memory wants page in place. Mirrors the legacy
// SQL ORDER BY mapping: the default key is wants_added desc with
// recording_id desc as the stable tiebreaker; alternative keys map to
// the natural columns the SPA exposes.
func sortWants(items []wantsListItem, sortKey string, dir sortDir) {
	desc := dir == sortDesc
	if sortKey == "" {
		// Default: newest-added first, recording_id desc tiebreak.
		sort.SliceStable(items, func(i, j int) bool {
			a, b := items[i], items[j]
			ai, bi := "", ""
			if a.WantsAdded != nil {
				ai = *a.WantsAdded
			}
			if b.WantsAdded != nil {
				bi = *b.WantsAdded
			}
			if ai != bi {
				return ai > bi
			}
			return a.ID > b.ID
		})
		return
	}
	cmp := wantsCompareFn(sortKey)
	sort.SliceStable(items, func(i, j int) bool {
		c := cmp(items[i], items[j])
		if desc {
			c = -c
		}
		if c != 0 {
			return c < 0
		}
		// Stable tiebreak on id desc (matches legacy "ORDER BY ..., r.recording_id DESC").
		return items[i].ID > items[j].ID
	})
}

func wantsCompareFn(key string) func(a, b wantsListItem) int {
	switch key {
	case "wants_added":
		return func(a, b wantsListItem) int {
			ai, bi := "", ""
			if a.WantsAdded != nil {
				ai = *a.WantsAdded
			}
			if b.WantsAdded != nil {
				bi = *b.WantsAdded
			}
			return cmpString(ai, bi)
		}
	case sortKeyDate:
		return func(a, b wantsListItem) int { return cmpString(a.DateFull, b.DateFull) }
	case sortKeyMaster:
		return func(a, b wantsListItem) int { return cmpString(a.Master, b.Master) }
	case "show":
		return func(a, b wantsListItem) int { return cmpString(a.Show, b.Show) }
	case "recording":
		return func(a, b wantsListItem) int {
			if c := cmpString(a.Show, b.Show); c != 0 {
				return c
			}
			if c := cmpString(a.DateFull, b.DateFull); c != 0 {
				return c
			}
			return cmpString(a.Tour, b.Tour)
		}
	default:
		return func(a, b wantsListItem) int {
			ai, bi := "", ""
			if a.WantsAdded != nil {
				ai = *a.WantsAdded
			}
			if b.WantsAdded != nil {
				bi = *b.WantsAdded
			}
			return cmpString(ai, bi)
		}
	}
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
