package server

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/nicolerenee/promptbook/internal/storage"
)

// shellData is the unified view-model the empty-shell page templates
// render. Per the Wave 9 architectural shift, server-rendered pages are
// just shells — the JS at /static/{page}.js fetches data from the
// JSON API and populates the DOM client-side. Title is the <title>
// tag value, ActiveNav is the sidebar item to highlight, Version is
// stamped into the sidebar footer, and RecordingID / PersonID are
// stamped into the page-root data attribute when relevant.
type shellData struct {
	Title       string
	ActiveNav   string
	Version     string
	RecordingID int64
	PersonID    int64
}

func (s *Server) handleHomePage(c echo.Context) error {
	return c.Render(http.StatusOK, "home.html", shellData{
		Title:     "Library",
		ActiveNav: "library",
		Version:   s.version,
	})
}

func (s *Server) handleRecordingPage(c echo.Context) error {
	id, err := storage.ParseRecordingID(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	return c.Render(http.StatusOK, "recording.html", shellData{
		Title:       "Recording",
		ActiveNav:   "library",
		Version:     s.version,
		RecordingID: id,
	})
}

func (s *Server) handleWantsPage(c echo.Context) error {
	return c.Render(http.StatusOK, "wants.html", shellData{
		Title:     "Wants",
		ActiveNav: "wants",
		Version:   s.version,
	})
}

func (s *Server) handleSyncPage(c echo.Context) error {
	return c.Render(http.StatusOK, "sync.html", shellData{
		Title:     "Sync",
		ActiveNav: "sync",
		Version:   s.version,
	})
}
