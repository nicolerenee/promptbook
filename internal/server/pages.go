package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// stagemediaPosterTimeout caps how long the recording detail page will
// wait on the StageMedia.me poster lookup. Stagemedia is best-effort
// enrichment — the page must render in well under a second even when
// the upstream is sluggish.
const stagemediaPosterTimeout = 5 * time.Second

// nftDateLayout is the ISO date layout returned by Encora in NFT.NFTDate.
const nftDateLayout = "2006-01-02"

// pageScanLimit caps how many recordings the home/wants pages will
// fetch in one go. Higher than the API default since pages render the
// whole list inline.
const pageScanLimit = 200

// homeStatuses lists the status filter tabs the home page renders, in
// display order. The empty status is "All" — the canonical "no filter"
// link in the nav.
//
//nolint:gochecknoglobals // immutable display-order lookup table.
var homeStatuses = []struct {
	Key   storage.Status
	Label string
}{
	{Key: "", Label: allTabLabel},
	{Key: storage.StatusSynced, Label: "Synced"},
	{Key: storage.StatusFormatMismatch, Label: "Format mismatch"},
	{Key: storage.StatusMissing, Label: "Missing"},
	{Key: storage.StatusWanted, Label: "Wanted"},
	{Key: storage.StatusOrphan, Label: "Orphan"},
}

type homeStatusTab struct {
	Key    string
	Label  string
	Count  int
	Active bool
}

type homeData struct {
	Title        string
	Recordings   []RecordingListItem
	ActiveStatus string
	Counts       map[string]int
	Tabs         []homeStatusTab
}

func (s *Server) handleHomePage(c echo.Context) error {
	statusParam := c.QueryParam("status")
	statuses := parseStatusParam(statusParam)

	ctx := c.Request().Context()
	items, err := loadStatefulRecordings(ctx, s.db, statuses, "", pageScanLimit, 0)
	if err != nil {
		return err
	}

	counts, err := computeStatusCounts(ctx, s.db)
	if err != nil {
		return err
	}

	tabs := make([]homeStatusTab, 0, len(homeStatuses))
	totalAll := 0
	for _, c := range counts {
		totalAll += c
	}
	for _, hs := range homeStatuses {
		key := string(hs.Key)
		count := counts[key]
		if hs.Key == "" {
			count = totalAll
		}
		tabs = append(tabs, homeStatusTab{
			Key:    key,
			Label:  hs.Label,
			Count:  count,
			Active: key == statusParam,
		})
	}

	return c.Render(http.StatusOK, "home.html", homeData{
		Title:        "Promptbook",
		Recordings:   items,
		ActiveStatus: statusParam,
		Counts:       counts,
		Tabs:         tabs,
	})
}

// computeStatusCounts returns a map from status key to the count of
// recordings currently reporting that status. Uses ListStates so the
// numbers match what the table renders.
func computeStatusCounts(ctx context.Context, db *sql.DB) (map[string]int, error) {
	all, err := storage.ListStates(ctx, db, storage.ListStatesOptions{Limit: maxStateScan})
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int, len(homeStatuses))
	for _, st := range all {
		counts[string(st.Status)]++
	}
	return counts, nil
}

type recordingPageData struct {
	Title     string
	Recording *storage.LoadedRecording
	// State carries the reconciled status badge + booleans used by the
	// detail header; nil when LoadState fails (defensive — handler logs
	// and continues so the page still renders).
	State *storage.RecordingState
	// Posters is the curated StageMedia.me poster URL list. Empty when
	// stagemedia is disabled, the show has no posters, or the upstream
	// call timed out / errored.
	Posters []string
	// NFTWarning is the human-readable NFT callout. Empty when the
	// recording is not NFT-restricted.
	NFTWarning string
}

func (s *Server) handleRecordingPage(c echo.Context) error {
	ctx := c.Request().Context()
	id, err := storage.ParseRecordingID(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	loaded, err := storage.LoadRecording(ctx, s.db, id)
	if errors.Is(err, storage.ErrRecordingNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}
	if err != nil {
		return err
	}

	state, sErr := storage.LoadState(ctx, s.db, id)
	if sErr != nil {
		// State is decorative — log and render without the badge rather
		// than 500'ing the whole page.
		s.logger.Warn().Err(sErr).Int64("recording_id", id).
			Msg("recording detail: load state failed")
		state = nil
	}

	posters := s.fetchPosters(ctx, loaded)

	return c.Render(http.StatusOK, "recording.html", recordingPageData{
		Title:      loaded.Recording.Show + " — " + loaded.Recording.Tour,
		Recording:  loaded,
		State:      state,
		Posters:    posters,
		NFTWarning: nftWarning(loaded.Recording.NFT, time.Now()),
	})
}

// fetchPosters calls StageMedia.me for curated posters, capped at
// stagemediaPosterTimeout. Any error or timeout yields an empty slice
// so the page still renders against a placeholder.
func (s *Server) fetchPosters(
	ctx context.Context, loaded *storage.LoadedRecording,
) []string {
	if s.Stagemedia() == nil || loaded.Recording.Metadata.ShowID <= 0 {
		return nil
	}
	smCtx, cancel := context.WithTimeout(ctx, stagemediaPosterTimeout)
	defer cancel()
	posters, perr := s.Stagemedia().Posters(smCtx, loaded.Recording.Metadata.ShowID)
	if perr != nil {
		s.logger.Warn().Err(perr).
			Int64("show_id", loaded.Recording.Metadata.ShowID).
			Msg("recording detail: stagemedia posters failed")
		return nil
	}
	return posters
}

// nftWarning maps an encora NFT block to a human-readable callout.
// Returns "" when the recording is not NFT-restricted. now is injected
// so callers and tests can render against a fixed clock.
func nftWarning(nft encora.NFT, now time.Time) string {
	if nft.NFTForever {
		return "NFT forever"
	}
	if nft.NFTDate == nil || *nft.NFTDate == "" {
		return ""
	}
	parsed, err := time.Parse(nftDateLayout, *nft.NFTDate)
	if err != nil {
		// Treat unparseable date as a non-fatal hint — surface the raw
		// string instead of dropping the warning entirely.
		return fmt.Sprintf("NFT (release %s)", *nft.NFTDate)
	}
	if parsed.After(now) {
		return "NFT until " + *nft.NFTDate
	}
	return fmt.Sprintf("NFT (released %s)", *nft.NFTDate)
}

// wantsPageData is the view-model for /wants. Kept separate from
// homeData so the home-page status tabs don't bleed into the wants
// template.
type wantsPageData struct {
	Title      string
	Recordings []recordingsListItem
}

func (s *Server) handleWantsPage(c echo.Context) error {
	items, err := loadRecordingsList(c.Request().Context(), s.db, pageScanLimit, 0, "false")
	if err != nil {
		return err
	}
	wants := make([]recordingsListItem, 0, len(items))
	for _, item := range items {
		if item.InWants {
			wants = append(wants, item)
		}
	}
	return c.Render(http.StatusOK, "wants.html", wantsPageData{
		Title:      "Wants",
		Recordings: wants,
	})
}

type syncPageData struct {
	Title string
	Runs  []syncRunSummary
}

type syncRunSummary struct {
	ID                 int64
	Kind               string
	StartedAt          string
	FinishedAt         string
	OkCount            int
	ErrorCount         int
	RateLimitRemaining int
	ErrorText          string
}

func (s *Server) handleSyncPage(c echo.Context) error {
	rows, err := s.db.QueryContext(c.Request().Context(), `
		SELECT id, kind, started_at, COALESCE(finished_at, ''), ok_count, error_count,
		       rate_limit_remaining, error_text
		FROM sync_runs
		ORDER BY id DESC
		LIMIT 30
	`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	var runs []syncRunSummary
	for rows.Next() {
		var r syncRunSummary
		if scanErr := rows.Scan(&r.ID, &r.Kind, &r.StartedAt, &r.FinishedAt,
			&r.OkCount, &r.ErrorCount, &r.RateLimitRemaining, &r.ErrorText); scanErr != nil {
			return scanErr
		}
		runs = append(runs, r)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return rowsErr
	}
	return c.Render(http.StatusOK, "sync.html", syncPageData{
		Title: "Sync runs",
		Runs:  runs,
	})
}
