package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/placeholder"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// imagesHandler resolves a /images/... request against the on-disk
// imagecache and falls through to a generated SVG placeholder when no
// file is present. The handler stays in the server package (not the
// imagecache or placeholder package) because it needs DB access for
// the human-readable label.
//
// Path shapes recognised:
//
//	/images/actors/<id>.jpg              → headshot
//	/images/shows/<id>/banner.jpg        → show banner
//	/images/recordings/<id>/fanart.jpg   → recording fanart
//	/images/recordings/<id>/poster.jpg   → recording poster
//
// Anything else returns 404. Trailing extension matching is loose on
// purpose — the cache always stores .jpg, so a request for .png or
// .webp falls into the placeholder path naturally.
func (s *Server) imagesHandler(c echo.Context) error {
	cache := s.imageCache
	if cache == nil || cache.Disabled() {
		return c.NoContent(http.StatusNotFound)
	}

	rel := strings.TrimPrefix(c.Request().URL.Path, "/images/")
	rel = path.Clean("/" + rel)
	if strings.Contains(rel, "..") || rel == "/" {
		return c.NoContent(http.StatusNotFound)
	}

	slot, ok := parseImagePath(rel)
	if !ok {
		return c.NoContent(http.StatusNotFound)
	}

	diskPath := slot.diskPath(cache)
	if diskPath != "" {
		if info, err := os.Stat(diskPath); err == nil && !info.IsDir() {
			// no-cache forces the browser to revalidate via
			// If-Modified-Since on every request. http.ServeFile
			// honors the conditional and returns 304 (no body) when
			// the mtime is unchanged, so the round-trip is cheap; the
			// payoff is that picking a new image at the same canonical
			// path (e.g. shows/123/banner.jpg) is picked up
			// immediately instead of being masked by Safari's
			// heuristic freshness window.
			c.Response().Header().Set("Cache-Control", "no-cache")
			http.ServeFile(c.Response().Writer, c.Request(), diskPath)
			return nil
		}
	}

	// Fanart is the one slot that should NOT fall back to a generated
	// placeholder. Posters + headshots + show banners are surfaced
	// prominently in grid views where a missing image leaves a hole;
	// fanart is decorative and the absence is meaningful (the user
	// hasn't picked one). Returning 404 lets the picker render
	// "No image on disk yet" instead of a synthetic SVG that pretends
	// fanart exists.
	if slot.Kind == placeholder.KindRecordingFanart {
		return c.NoContent(http.StatusNotFound)
	}

	return s.servePlaceholder(c, slot)
}

// imageSlot is the parsed shape of an /images/... URL. ID is the
// entity primary key the cache + DB lookup both key off; Kind drives
// the placeholder dimensions + label format.
type imageSlot struct {
	Kind placeholder.Kind
	ID   int64
}

// diskPath returns the canonical on-disk path the cache would store
// this slot under, or "" when the cache is disabled.
func (s imageSlot) diskPath(cache *imagecache.Cache) string {
	switch s.Kind {
	case placeholder.KindHeadshot:
		return cache.HeadshotPath(s.ID)
	case placeholder.KindShowBanner:
		return cache.ShowBannerPath(s.ID)
	case placeholder.KindRecordingFanart:
		return cache.RecordingFanartPath(s.ID)
	case placeholder.KindRecordingPoster:
		return cache.RecordingPosterPath(s.ID)
	default:
		return ""
	}
}

// parseImagePath inspects the request path (already stripped of the
// /images/ prefix and clean-rooted at /) and returns the matching
// imageSlot. Returns ok=false on any path that doesn't exactly fit
// one of the four recognised shapes; those should 404.
func parseImagePath(rel string) (imageSlot, bool) {
	parts := strings.Split(strings.TrimPrefix(rel, "/"), "/")
	switch {
	case len(parts) == 2 && parts[0] == "actors":
		id, err := strconv.ParseInt(strings.TrimSuffix(parts[1], filepath.Ext(parts[1])), 10, 64)
		if err != nil || id <= 0 {
			return imageSlot{}, false
		}
		return imageSlot{Kind: placeholder.KindHeadshot, ID: id}, true
	case len(parts) == 3 && parts[0] == "shows" && parts[2] == "banner.jpg":
		id, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || id <= 0 {
			return imageSlot{}, false
		}
		return imageSlot{Kind: placeholder.KindShowBanner, ID: id}, true
	case len(parts) == 3 && parts[0] == "recordings" && parts[2] == "fanart.jpg":
		id, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || id <= 0 {
			return imageSlot{}, false
		}
		return imageSlot{Kind: placeholder.KindRecordingFanart, ID: id}, true
	case len(parts) == 3 && parts[0] == "recordings" && parts[2] == "poster.jpg":
		id, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || id <= 0 {
			return imageSlot{}, false
		}
		return imageSlot{Kind: placeholder.KindRecordingPoster, ID: id}, true
	default:
		return imageSlot{}, false
	}
}

// servePlaceholder renders a generated SVG for slot and writes it to
// the response. DB lookups for the human-readable label are best-
// effort: a missing entity falls back to "Unknown #<id>" rather than
// failing the request.
func (s *Server) servePlaceholder(c echo.Context, slot imageSlot) error {
	label := s.lookupLabel(c.Request().Context(), slot)
	body, err := placeholder.Render(slot.Kind, slot.ID, label)
	if err != nil {
		return fmt.Errorf("render placeholder %d: %w", slot.ID, err)
	}
	return c.Blob(http.StatusOK, "image/svg+xml; charset=utf-8", body)
}

// lookupLabel resolves the human-readable text the placeholder draws
// for the slot. Returns "Unknown #<id>" when the entity isn't on
// record locally (the cache may have been pre-warmed for an entity
// the next sync will introduce, or the URL just refers to a stale id).
func (s *Server) lookupLabel(ctx context.Context, slot imageSlot) string {
	switch slot.Kind {
	case placeholder.KindHeadshot:
		p, err := storage.LoadPerformer(ctx, s.db, slot.ID)
		if err != nil || p == nil || strings.TrimSpace(p.Name) == "" {
			return fallbackLabel(slot.ID)
		}
		return p.Name
	case placeholder.KindShowBanner:
		name, err := loadShowNameForPlaceholder(ctx, s.db, slot.ID)
		if err != nil || strings.TrimSpace(name) == "" {
			return fallbackLabel(slot.ID)
		}
		return name
	case placeholder.KindRecordingFanart, placeholder.KindRecordingPoster:
		loaded, err := storage.LoadRecording(ctx, s.db, slot.ID)
		if err != nil || loaded == nil {
			return fallbackLabel(slot.ID)
		}
		return recordingPlaceholderLabel(loaded.Recording)
	default:
		return fallbackLabel(slot.ID)
	}
}

// loadShowNameForPlaceholder fetches a show's display name without
// dragging in shows.go's errShowNotFound vocabulary. We only care
// about the empty/non-empty distinction here.
func loadShowNameForPlaceholder(ctx context.Context, db *sql.DB, id int64) (string, error) {
	var name string
	err := db.QueryRowContext(ctx, `SELECT name FROM shows WHERE show_id = ?`, id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("query show name %d: %w", id, err)
	}
	return name, nil
}

// recordingLabelMaxParts caps the make() pre-allocation for the
// recording label segment slice — show, tour, date.
const recordingLabelMaxParts = 3

// recordingPlaceholderLabel formats a recording into the canonical
// "Show · Tour · Date" shape the placeholder splits across multiple
// lines. Missing components are dropped quietly, so a recording with
// only a show name still renders cleanly.
func recordingPlaceholderLabel(r encora.Recording) string {
	parts := make([]string, 0, recordingLabelMaxParts)
	if t := strings.TrimSpace(r.Show); t != "" {
		parts = append(parts, t)
	}
	if t := strings.TrimSpace(r.Tour); t != "" {
		parts = append(parts, t)
	}
	if d := smartDateForPlaceholder(r.Date); d != "" {
		parts = append(parts, d)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " · ")
}

// smartDateForPlaceholder mirrors nfo.smartDate (which is unexported)
// so the placeholder label matches the rest of the UI's date
// formatting. Year-only and month-year partial dates are honored.
func smartDateForPlaceholder(d encora.Date) string {
	t, err := time.Parse("2006-01-02", d.FullDate)
	if err != nil {
		return d.FullDate
	}
	switch {
	case !d.MonthKnown:
		return t.Format("2006")
	case !d.DayKnown:
		return t.Format("January 2006")
	default:
		return t.Format("2006-01-02")
	}
}

// fallbackLabel is the human-readable text the placeholder uses when
// the entity isn't on file locally. Includes the id so support /
// debugging can identify the missing row at a glance.
func fallbackLabel(id int64) string {
	return "Unknown #" + strconv.FormatInt(id, 10)
}
