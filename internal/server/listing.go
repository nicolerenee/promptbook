package server

// listing.go — shared paging + sorting helpers for the JSON list
// endpoints (recordings, people, shows, wants, history). Every list
// response uses the same {items, total, limit, offset} envelope so the
// SPA's Pagination component can drive Prev/Next + page indicators off
// a single shape.
//
// total is the count of rows matching the same filter (status / kind /
// recording_id) the items slice reflects — NOT the entire table. This
// lets the UI render "Page N of M" against the active view (e.g. "12
// orphans" rather than "1,234 recordings") so the indicator stays
// honest when filters narrow the set.
//
// Sort vocabulary lives per-handler because the legal columns differ
// (recordings sort on date_full, shows sort on first_year, etc.).
// Post-ent cutover sorts run in memory via per-key compare functions;
// the legacy whitelist-driven SQL ORDER BY fragments are gone.

import (
	"strings"

	"github.com/labstack/echo/v4"
)

// totalKey is the JSON field name carrying the unfiltered count for
// the active filter set. Pulled into a constant so callers don't drift
// from the literal string.
const totalKey = "total"

// sortDir is "asc" or "desc"; "" / unknown values normalize to "asc".
type sortDir string

const (
	sortAsc  sortDir = "asc"
	sortDesc sortDir = "desc"
)

// parseSortDir reads ?dir=asc|desc, falling back to asc when the value
// is missing or unrecognized. The handler-specific defaultDirForKey
// can override this when the user passed no ?dir at all.
func parseSortDir(c echo.Context) sortDir {
	switch strings.ToLower(c.QueryParam("dir")) {
	case string(sortDesc):
		return sortDesc
	default:
		return sortAsc
	}
}

// pageEnvelope wraps the JSON response body with a stable shape every
// list handler emits. Callers build {items, total, limit, offset} via
// this helper so the field names stay consistent — the SPA's shared
// Pagination component depends on it.
func pageEnvelope(items any, total, limit, offset int) map[string]any {
	return map[string]any{
		itemsKey:  items,
		totalKey:  total,
		limitKey:  limit,
		offsetKey: offset,
	}
}

// cmpString returns -1/0/+1 comparing a and b case-insensitively. Used
// by every in-memory recordings/people/shows sort so the comparator
// matches the legacy client-side cmpStr helper.
func cmpString(a, b string) int {
	la := strings.ToLower(a)
	lb := strings.ToLower(b)
	switch {
	case la < lb:
		return -1
	case la > lb:
		return 1
	default:
		return 0
	}
}

// cmpInt returns -1/0/+1 comparing a and b numerically.
func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// cmpEmptyLast pins empty strings to the bottom regardless of direction
// so unmatched rows don't dominate the top of an asc sort. Mirrors the
// legacy client-side cmpEmptyLast helper for local_format / wants_added.
func cmpEmptyLast(a, b string) int {
	ea := a == ""
	eb := b == ""
	switch {
	case ea && !eb:
		return 1
	case !ea && eb:
		return -1
	case ea && eb:
		return 0
	default:
		return cmpString(a, b)
	}
}
