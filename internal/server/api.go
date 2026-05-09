package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

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

	// allTabLabel is the display label for the "no filter" entry in
	// every tab-style nav (status / kind / mismatch type). Lifted into
	// a shared constant because goconst flags the duplication once
	// three or more sibling tab tables exist.
	allTabLabel = "All"

	// stagemediaPosterTimeout caps how long the recording-detail
	// handler will wait for the StageMedia poster fetch before
	// continuing without posters. The handler logs a warning and
	// returns an empty slice on timeout so the page still renders.
	stagemediaPosterTimeout = 5 * time.Second
)

// RecordingListItem is the shape returned by /api/v1/recordings and
// embedded in the home page view-model. Status and the per-recording
// format strings come from the storage.RecordingState reconciler so the
// JSON and HTML pages render the same five-status taxonomy.
//
// LocalPosterURL is the /images/... path of the user-curated poster
// for the recording's show, or empty when the cache is disabled or
// no poster is on disk yet. The grid view in Library.js prefers it
// over reaching for an upstream URL we don't trust to stay reachable.
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
	// Per-recording image choice surfaces. POSTs persist the user's
	// curated poster/backdrop selection or overlay-text override and
	// (for backdrop/overlay) trigger a re-render of rendered.jpg via
	// the imagerender stub. Bounds-checked against the cache's
	// CountPosters/CountBackdrops; 503 when image caching is off.
	api.POST("/recordings/:id/poster", s.handleSetPoster)
	api.POST("/recordings/:id/backdrop", s.handleSetBackdrop)
	api.POST("/recordings/:id/overlay", s.handleSetOverlay)
	api.POST("/recordings/:id/overlay-disabled", s.handleSetOverlayDisabled)
	// User image uploads. Multipart "file" field, 10 MiB cap, decoded
	// + re-encoded as JPEG into the same on-disk cache layout, at
	// indexes >= imagecache.UploadIndexFloor (100). The picker UI
	// follows up with POST .../poster or .../backdrop to make the
	// upload the active selection.
	api.POST("/recordings/:id/backdrop-upload", s.handleUploadBackdrop)
	api.POST("/recordings/:id/poster-upload", s.handleUploadRecordingPoster)
	api.POST("/shows/:id/poster-upload", s.handleUploadPoster)
	api.GET("/wants", s.handleListWants)
	api.GET("/sync/runs", s.handleSyncRuns)
	api.GET("/queue", s.handleListQueue)
	api.POST("/queue/:id/import", s.handleImportQueue)
	api.GET("/people", s.handleListPeople)
	api.GET("/people/:id", s.handleGetPerson)
	// By-show aggregate. Show detail (/shows/:id) is added by the
	// detail-page agent in a sibling commit; the list endpoint is the
	// only one this commit registers.
	api.GET("/shows", s.handleListShows)
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

	// Manual re-render of the burned-in backdrop. Picker handlers also
	// invoke imagerender.Regenerate directly when a choice changes;
	// this endpoint is the explicit "Re-render" affordance + a smoke
	// test surface. 503 when the renderer is nil.
	api.POST("/recordings/:id/regenerate-backdrop", s.handleRegenerateBackdrop)

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

	statuses := parseStatusParam(statusParam)

	// status takes precedence over the legacy owned filter when both are
	// supplied; owned is honored only when status is absent.
	ownedFilter := owned
	if statusParam != "" {
		ownedFilter = ""
	}

	ctx := c.Request().Context()
	items, err := loadStatefulRecordings(ctx, s.db, statuses, ownedFilter, limit, offset)
	if err != nil {
		return err
	}

	// Decorate each row with the on-disk poster URL keyed off the
	// show's curated poster index. Bulk-load show choices once per
	// request to avoid N+1 queries — for the unique-show subset of a
	// typical 50-row page this is a single SQL roundtrip.
	if posterErr := s.decorateLocalPosters(ctx, items); posterErr != nil {
		s.logger.Warn().Err(posterErr).Msg("decorate local poster urls failed; serving without")
	}

	return c.JSON(http.StatusOK, map[string]any{
		itemsKey:  items,
		limitKey:  limit,
		offsetKey: offset,
	})
}

// decorateLocalPosters sets LocalPosterURL on every item that maps to a
// cached poster on disk. Bulk-loads show_image_choices for the unique
// show ids in the page so the cost is one SQL query regardless of page
// size. Cache disabled / no rows yields a no-op (every URL stays "").
func (s *Server) decorateLocalPosters(ctx context.Context, items []RecordingListItem) error {
	cache := s.ImageCache()
	if cache == nil || cache.Disabled() || len(items) == 0 {
		return nil
	}
	showIDs := make(map[int64]struct{}, len(items))
	for _, it := range items {
		if it.ShowID != 0 {
			showIDs[it.ShowID] = struct{}{}
		}
	}
	choices, err := loadShowPosterChoices(ctx, s.db, showIDs)
	if err != nil {
		return err
	}
	for i := range items {
		showID := items[i].ShowID
		if showID == 0 {
			continue
		}
		idx := choices[showID] // 0 when not in map (default).
		items[i].LocalPosterURL = cache.PosterURL(showID, idx)
	}
	return nil
}

// loadShowPosterChoices returns a {show_id: poster_index} map populated
// from show_image_choices for the supplied show ids. Shows without a
// row are absent from the map — callers should treat absence as
// "fallback to index 0".
func loadShowPosterChoices(
	ctx context.Context, db *sql.DB, showIDs map[int64]struct{},
) (map[int64]int, error) {
	out := make(map[int64]int, len(showIDs))
	if len(showIDs) == 0 {
		return out, nil
	}
	placeholders := make([]string, 0, len(showIDs))
	args := make([]any, 0, len(showIDs))
	for id := range showIDs {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	//nolint:gosec // G202: placeholders are "?" markers, not user input.
	q := `
		SELECT show_id, poster_index
		FROM show_image_choices
		WHERE poster_index IS NOT NULL
		  AND show_id IN (` + strings.Join(placeholders, ",") + `)
	`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query show poster choices: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			showID int64
			idx    int
		)
		if scanErr := rows.Scan(&showID, &idx); scanErr != nil {
			return nil, fmt.Errorf("scan show poster choice: %w", scanErr)
		}
		out[showID] = idx
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate show poster choices: %w", rerr)
	}
	return out, nil
}

// recordingDetailResponse wraps storage.LoadedRecording with the
// server-side enrichment fields (StageMedia posters, on-disk NFO
// content + mtime). The embedded *storage.LoadedRecording flattens its
// PascalCase fields into the same JSON object so existing consumers see
// no shape change beyond the new lower-case keys.
//
// LocalPosterURLs is a sibling of Posters, same length and same index
// order. Each entry is a /images/... path when the matching poster is
// in the on-disk cache, "" otherwise — the frontend prefers local URLs
// when non-empty so the user doesn't hot-link StageMedia for content
// that's already on the server's filesystem.
//
// LocalBackdropURLs lists the cached screen-grab backdrops for the
// recording, in index order. Empty slice when caching is off or
// nothing's downloaded yet — the frontend falls back to no backdrop in
// that case rather than reaching for an upstream URL we don't trust to
// stay reachable.
//
// Cast performers grow a parallel local_headshot_url field on
// LoadedRecording.Cast at JSON marshal time via castWithLocalHeadshots.
// The legacy fields stay for callers that haven't migrated.
type recordingDetailResponse struct {
	*storage.LoadedRecording

	Posters           []string                `json:"posters"`
	LocalPosterURLs   []string                `json:"local_poster_urls"`
	LocalBackdropURLs []string                `json:"local_backdrop_urls"`
	Cast              []castEntryWithHeadshot `json:"cast"`
	NFOContent        string                  `json:"nfo_content"`
	NFOModifiedAt     *time.Time              `json:"nfo_modified_at"`

	// SelectedPosterIndex / SelectedBackdropIndex carry the user's
	// curated picks from recording_image_choices. Both nil when the
	// row is absent or the column is null — the UI then falls back
	// to highlighting index 0 to match storage.ImageChoice.Resolve*.
	SelectedPosterIndex   *int `json:"selected_poster_index"`
	SelectedBackdropIndex *int `json:"selected_backdrop_index"`
	// OverlayTextOverride is the user-supplied burned-in label, or
	// nil when no override has been saved (the renderer uses its own
	// auto-derived "show · tour · date" string in that case).
	OverlayTextOverride *string `json:"overlay_text_override"`
	// OverlayDisabled, when true, tells the renderer to skip the
	// playbill-style band and copy the raw selected backdrop through
	// to rendered.jpg unchanged. Mirrors the column on
	// recording_image_choices.
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

	posters := s.fetchPostersForRecording(ctx, loaded)
	localPosterURLs := s.localPosterURLs(loaded.Recording.Metadata.ShowID, posters)
	localBackdropURLs := s.localBackdropURLs(loaded.Recording.ID)
	castWithHeadshots := s.castWithLocalHeadshots(loaded.Cast)

	nfoContent, nfoModifiedAt := s.readNFOForRecording(loaded)

	// Image choices are best-effort enrichment — a query failure
	// shouldn't break the whole detail page. Log and serve a
	// zero-valued ImageChoice; the UI then highlights the default
	// (index 0) and shows no override, matching the resolve-fallback
	// contract.
	choice, choiceErr := storage.GetImageChoice(ctx, s.db, id)
	if choiceErr != nil {
		s.logger.Warn().
			Err(choiceErr).
			Int64("recording_id", id).
			Msg("get image choice failed; serving defaults")
		choice = storage.ImageChoice{}
	}

	return c.JSON(http.StatusOK, recordingDetailResponse{
		LoadedRecording:       loaded,
		Posters:               posters,
		LocalPosterURLs:       localPosterURLs,
		LocalBackdropURLs:     localBackdropURLs,
		Cast:                  castWithHeadshots,
		NFOContent:            nfoContent,
		NFOModifiedAt:         nfoModifiedAt,
		SelectedPosterIndex:   choice.PosterIndex,
		SelectedBackdropIndex: choice.BackdropIndex,
		OverlayTextOverride:   choice.OverlayTextOverride,
		OverlayDisabled:       choice.OverlayDisabled,
	})
}

// localPosterURLs returns a slice the same length as posters where
// each entry is the cached /images/... URL when the matching index is
// on disk, "" otherwise. Returns an empty (but length-matched) slice
// when caching is disabled so the JSON shape is stable.
func (s *Server) localPosterURLs(showID int64, posters []string) []string {
	out := make([]string, len(posters))
	cache := s.ImageCache()
	if cache == nil || cache.Disabled() || showID == 0 {
		return out
	}
	for i := range posters {
		out[i] = cache.PosterURL(showID, i)
	}
	return out
}

// localBackdropURLs enumerates every cached backdrop for the recording
// in index order. Returns a non-nil empty slice (so the JSON renders
// [] rather than null) when caching is disabled, the recording has no
// id, or nothing's been cached for this recording yet.
func (s *Server) localBackdropURLs(recordingID int64) []string {
	cache := s.ImageCache()
	if cache == nil || cache.Disabled() || recordingID == 0 {
		return []string{}
	}
	count := cache.CountBackdrops(recordingID)
	out := make([]string, 0, count)
	for i := range count {
		// CountBackdrops counts files in the directory but doesn't
		// guarantee they're contiguously numbered (a manual delete
		// could leave a gap). BackdropURL returns "" when the
		// specific index isn't present, which we filter out so the
		// returned slice is a list of usable URLs only.
		if u := cache.BackdropURL(recordingID, i); u != "" {
			out = append(out, u)
		}
	}
	return out
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

// fetchPostersForRecording asks StageMedia for poster URLs for the
// recording's show, with a hard 5-second timeout. Returns an empty
// (non-nil) slice when StageMedia is unconfigured, the recording has
// no show id, or the upstream call fails. Failures log a warning so
// operators can spot a misconfigured key without poisoning the page.
func (s *Server) fetchPostersForRecording(
	ctx context.Context, loaded *storage.LoadedRecording,
) []string {
	if s.Stagemedia() == nil {
		return []string{}
	}
	showID := loaded.Recording.Metadata.ShowID
	if showID == 0 {
		return []string{}
	}
	timedCtx, cancel := context.WithTimeout(ctx, stagemediaPosterTimeout)
	defer cancel()
	posters, err := s.Stagemedia().Posters(timedCtx, showID)
	if err != nil {
		s.logger.Warn().
			Err(err).
			Int64("recording_id", loaded.Recording.ID).
			Int64("show_id", showID).
			Msg("stagemedia: posters fetch failed; returning empty list")
		return []string{}
	}
	if posters == nil {
		return []string{}
	}
	return posters
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
	items, err := loadWantsList(c.Request().Context(), s.db, wantsScanLimit, 0)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{itemsKey: items})
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
	showID     int64
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
		SELECT r.recording_id, r.show_id, COALESCE(s.name, ''), r.tour, r.date_full,
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
			&id, &m.showID, &m.show, &m.tour, &m.dateFull,
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

// loadWantsList queries every wants row joined with its recording / show
// metadata and surfaces wants.last_synced_at as the per-row WantsAdded
// timestamp. Rows are returned newest-added first so the UI's "added"
// timeline reads chronologically. Excludes wants whose recording is also
// in the collection — those are no longer "wants" from a UX standpoint.
func loadWantsList(
	ctx context.Context,
	db *sql.DB,
	limit, offset int,
) ([]wantsListItem, error) {
	const q = `
		SELECT
			r.recording_id, COALESCE(s.name, ''), r.tour, r.date_full,
			r.date_month_known, r.date_day_known, r.master,
			c.recording_id IS NOT NULL AS in_collection,
			w.last_synced_at
		FROM wants w
		JOIN recordings r ON r.recording_id = w.recording_id
		LEFT JOIN shows s ON s.show_id = r.show_id
		LEFT JOIN collection c ON c.recording_id = r.recording_id
		WHERE c.recording_id IS NULL
		ORDER BY w.last_synced_at DESC, r.recording_id DESC
		LIMIT ? OFFSET ?
	`
	rows, err := db.QueryContext(ctx, q, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("query wants list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]wantsListItem, 0)
	for rows.Next() {
		var (
			item                 wantsListItem
			monthKnown, dayKnown int
			inColl               int
			lastSynced           sql.NullString
		)
		if scanErr := rows.Scan(
			&item.ID, &item.Show, &item.Tour, &item.DateFull,
			&monthKnown, &dayKnown, &item.Master,
			&inColl, &lastSynced,
		); scanErr != nil {
			return nil, fmt.Errorf("scan wants list row: %w", scanErr)
		}
		item.DateMonthKnown = monthKnown == 1
		item.DateDayKnown = dayKnown == 1
		item.InCollection = inColl == 1
		item.InWants = true
		if lastSynced.Valid {
			s := lastSynced.String
			item.WantsAdded = &s
		}
		out = append(out, item)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate wants list rows: %w", rerr)
	}
	return out, nil
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
