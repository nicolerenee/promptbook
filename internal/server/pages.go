package server

// pages.go contains the HTML page handler for the SPA shell. As of the
// SPA migration the server no longer renders per-page shells with
// embedded body templates — every browser-facing route resolves to the
// same `index.html` shell that boots the Mithril SPA at
// /static/app/main.js. The router lives entirely client-side; the
// catch-all `GET /*` registered in routes() lands here for any path
// that didn't match `/api/v1/*` or `/static/*`.
//
// The legacy per-page templates (home.html, recording.html, etc.) and
// per-page JS files (library.js, recording.js, etc.) stay on disk
// under internal/web/ as porting reference for the agents wiring the
// remaining routes; they are not registered in PageSet and are not
// reachable via HTTP.

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// spaShellData is the (deliberately small) view-model the index.html
// template renders. Title is the document <title>; everything else the
// SPA needs lives in the JS bundle, so this struct stays minimal on
// purpose.
type spaShellData struct {
	Title string
}

// handleSPA renders the SPA shell for any browser-facing route. The
// echo router serves /api/v1/* and /static/* via more-specific
// handlers, so this catch-all only fires for actual page navigations.
// Mithril takes over from there and resolves the path against its own
// route table.
func (s *Server) handleSPA(c echo.Context) error {
	return c.Render(http.StatusOK, "index.html", spaShellData{
		Title: "promptbook",
	})
}
