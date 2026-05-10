package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/ent/recording"
	"github.com/nicolerenee/promptbook/internal/ent/show"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// API tunables.
const (
	itemsKey = "items"

	// allTabLabel is the display label for the "no filter" entry in
	// every tab-style nav (status / kind / mismatch type). Lifted into
	// a shared constant because goconst flags the duplication once
	// three or more sibling tab tables exist.
	allTabLabel = "All"
)

func (s *Server) routes() {
	api := s.echo.Group("/api/v1")
	api.GET("/health", s.handleHealth)
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
	// Poster preview: composites the recording's overlay band over an
	// upstream URL the user has staged in the picker. Lets the SPA
	// show "what will this look like?" before the user clicks Save.
	// Allowlisted to the same upstream hosts as the proxy.
	api.GET("/recordings/:id/poster-preview", s.handleRecordingPosterPreview)
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
	api.GET("/queue", s.handleListQueue)
	api.POST("/queue/:id/import", s.handleImportQueue)
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

	// GraphQL endpoint (Phase 3). POST /graphql for queries, GET for
	// CORS preflight + Apollo GET, GET /graphql/playground for the
	// interactive sandbox. Registered before the SPA catch-all so Echo
	// routes /graphql to the gqlgen handler instead of the Mithril
	// shell. The schema covers Recording / Show / Performer /
	// CollectionEntry / WantsEntry / SyncRun; non-catalog surfaces
	// (jobs, history, settings, image picker) stay REST-only.
	s.registerGraphQL()

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
