// Package web bundles the html/template files and static assets used by
// the promptbook server. Everything ships in the binary via embed so
// deployments don't need a sidecar files volume.
package web

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

// pageNames is now a single-element list: the SPA shell. The legacy per-
// page templates (home.html, recording.html, wants.html, sync.html,
// queue.html, people.html, person.html, history.html, mismatches.html,
// settings.html, apply_result.html) plus the _layout.html / _sidebar.html
// / _topbar.html shared partials remain on disk under templates/ as
// porting reference for the agents wiring the remaining client-side
// routes; they are not registered here and are unreachable via HTTP.
var pageNames = []string{ //nolint:gochecknoglobals // immutable lookup table
	"index.html",
}

// PageSet is a map of page-name to fully-parsed template tree (one tree
// per page, so each page's `body` block doesn't collide with the others
// when html/template parses them all into a single namespace).
type PageSet map[string]*template.Template

// ParsePages returns a PageSet ready for rendering. The SPA shell
// (index.html) is a single self-contained template — no shared layout /
// sidebar / topbar partials are involved, since the Mithril client
// composes those itself.
func ParsePages() (PageSet, error) {
	out := make(PageSet, len(pageNames))
	for _, name := range pageNames {
		t, err := template.New(name).
			ParseFS(templatesFS, "templates/"+name)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		out[name] = t
	}
	return out, nil
}

// StaticHandler serves the embedded static directory. Mount at /static.
func StaticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	return http.FileServer(http.FS(sub))
}

// (smartDate / humanSize / firstPoster / monthName were retired with
// the SPA migration. The Mithril client now formats dates and sizes in
// internal/web/static/app/utils/format.js.)
