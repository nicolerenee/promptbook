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

// pageNames are the page template files (excluding the underscore-
// prefixed shared layout).
var pageNames = []string{ //nolint:gochecknoglobals // immutable lookup table
	"apply_result.html",
	"history.html",
	"home.html",
	"mismatches.html",
	"people.html",
	"person.html",
	"queue.html",
	"recording.html",
	"settings.html",
	"wants.html",
	"sync.html",
}

// PageSet is a map of page-name to fully-parsed template tree (one tree
// per page, so each page's `body` block doesn't collide with the others
// when html/template parses them all into a single namespace).
type PageSet map[string]*template.Template

// ParsePages returns a PageSet ready for rendering. Each entry binds the
// shared layout (`_layout.html`) to that page's body.
func ParsePages() (PageSet, error) {
	funcs := template.FuncMap{
		"smartDate":   smartDate,
		"humanSize":   humanSize,
		"firstPoster": firstPoster,
	}

	out := make(PageSet, len(pageNames))
	for _, name := range pageNames {
		t, err := template.New(name).Funcs(funcs).
			ParseFS(
				templatesFS,
				"templates/_layout.html",
				"templates/_sidebar.html",
				"templates/_topbar.html",
				"templates/"+name,
			)
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

// ISO date prefix lengths used by smartDate.
const (
	isoYearLen      = 4 // "YYYY"
	isoYearMonthLen = 7 // "YYYY-MM"
)

// Binary unit thresholds used by humanSize.
const (
	bytesPerKiB = 1024
	bytesPerMiB = bytesPerKiB * 1024
	bytesPerGiB = bytesPerMiB * 1024
	bytesPerTiB = bytesPerGiB * 1024
)

// humanSize renders a byte count as an IEC binary string (KiB/MiB/GiB/TiB)
// abbreviated as KB/MB/GB/TB to match the Encora-facing UI's casual
// shorthand. Values under a kibibyte are rendered as raw bytes.
func humanSize(b int64) string {
	switch {
	case b >= bytesPerTiB:
		return fmt.Sprintf("%.2f TB", float64(b)/float64(bytesPerTiB))
	case b >= bytesPerGiB:
		return fmt.Sprintf("%.2f GB", float64(b)/float64(bytesPerGiB))
	case b >= bytesPerMiB:
		return fmt.Sprintf("%.2f MB", float64(b)/float64(bytesPerMiB))
	case b >= bytesPerKiB:
		return fmt.Sprintf("%.2f KB", float64(b)/float64(bytesPerKiB))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// firstPoster returns the first poster URL from the supplied slice, or
// "" when there are none. Used by the recording detail template to keep
// the URL-extraction logic out of the template body.
func firstPoster(posters []string) string {
	if len(posters) == 0 {
		return ""
	}
	return posters[0]
}

// smartDate is a template helper that mirrors the rename engine's
// {Date} token so home.html and recording.html render the same labels
// the user sees on disk.
func smartDate(fullDate string, monthKnown, dayKnown bool) string {
	if !monthKnown {
		if len(fullDate) >= isoYearLen {
			return fullDate[:isoYearLen]
		}
		return fullDate
	}
	if !dayKnown {
		if len(fullDate) >= isoYearMonthLen {
			return monthName(fullDate[5:7]) + " " + fullDate[:isoYearLen]
		}
		return fullDate
	}
	return fullDate
}

func monthName(mm string) string {
	switch mm {
	case "01":
		return "January"
	case "02":
		return "February"
	case "03":
		return "March"
	case "04":
		return "April"
	case "05":
		return "May"
	case "06":
		return "June"
	case "07":
		return "July"
	case "08":
		return "August"
	case "09":
		return "September"
	case "10":
		return "October"
	case "11":
		return "November"
	case "12":
		return "December"
	default:
		return mm
	}
}
