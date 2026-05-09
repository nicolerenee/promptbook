package rename

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nicolerenee/promptbook/internal/encora"
)

// Tokens supported by the template engine. Anything else in the template
// is left intact (so static `[encora-{EncoraID}]` works even though the
// brackets are not tokens).
//
// Date tokens depend on the partial-date flags coming from Encora:
//
//	{Date}        smart: ISO when full, "December 2009" if !DayKnown,
//	              "2009" if !MonthKnown.
//	{DateUS}      "December 1, 2009" / "December 2009" / "2009"
//	{DateNumeric} "12-01-2009" / "12-2009" / "2009"
//	{Year}        always 4-digit year
//	{Variant}     blank or "v2" / "v4" matching DateVariant
//
// Other tokens are straight string substitution.
var tokenPattern = regexp.MustCompile(`\{([A-Za-z]+)\}`)

// isoYearLen is the length of the year prefix on Encora's ISO date strings.
const isoYearLen = 4

// Apply renders tmpl by substituting every {Token} against r. Unknown
// tokens are left in place so a downstream linter can surface them. The
// result is then sanitized for filesystem use (slashes/colons removed).
func Apply(tmpl string, r encora.Recording) (string, error) {
	if tmpl == "" {
		return "", nil
	}

	var rerr error
	out := tokenPattern.ReplaceAllStringFunc(tmpl, func(match string) string {
		token := match[1 : len(match)-1] // strip { }
		val, err := resolveToken(token, r)
		if err != nil {
			if rerr == nil {
				rerr = err
			}
			return match
		}
		return val
	})
	if rerr != nil {
		return "", rerr
	}
	return Sanitize(out), nil
}

// Sanitize cleans a rendered name for use as a filename or folder name on
// macOS/Linux. Strips path-traversing characters and collapses runs of
// whitespace. Does not lowercase or transliterate — preserves user intent.
func Sanitize(s string) string {
	// Replace path separators and other unsafe characters.
	for _, c := range []string{"/", "\\", "\x00"} {
		s = strings.ReplaceAll(s, c, "")
	}
	// Colons read poorly on macOS Finder; replace with " - ".
	s = strings.ReplaceAll(s, ":", " -")
	// Collapse internal whitespace runs.
	s = strings.Join(strings.Fields(s), " ")
	return s
}

func resolveToken(token string, r encora.Recording) (string, error) {
	switch token {
	case "Show":
		return r.Show, nil
	case "Tour":
		return r.Tour, nil
	case "Master":
		return r.Master, nil
	case "EncoraID":
		return strconv.FormatInt(r.ID, 10), nil
	case "Format":
		if r.ReleaseFormat != nil {
			return *r.ReleaseFormat, nil
		}
		return "", nil
	case "Date":
		return formatDateSmart(r.Date), nil
	case "DateUS":
		return formatDateUS(r.Date), nil
	case "DateNumeric":
		return formatDateNumeric(r.Date), nil
	case "Year":
		return formatYear(r.Date), nil
	case "Variant":
		if r.Date.DateVariant != nil && *r.Date.DateVariant != "" {
			return "v" + *r.Date.DateVariant, nil
		}
		return "", nil
	default:
		return "", fmt.Errorf("rename: unknown template token %q", token)
	}
}

// parseISO best-effort: returns the ISO yyyy-mm-dd as a time.Time. Encora
// always emits ISO even when month/day are flagged unknown — they fill
// with placeholders (typically "01"). That's why callers must check
// MonthKnown/DayKnown rather than relying on parse errors.
func parseISO(s string) (time.Time, bool) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func formatDateSmart(d encora.Date) string {
	t, ok := parseISO(d.FullDate)
	if !ok {
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

func formatDateUS(d encora.Date) string {
	t, ok := parseISO(d.FullDate)
	if !ok {
		return d.FullDate
	}
	switch {
	case !d.MonthKnown:
		return t.Format("2006")
	case !d.DayKnown:
		return t.Format("January 2006")
	default:
		return t.Format("January 2, 2006")
	}
}

func formatDateNumeric(d encora.Date) string {
	t, ok := parseISO(d.FullDate)
	if !ok {
		return d.FullDate
	}
	switch {
	case !d.MonthKnown:
		return t.Format("2006")
	case !d.DayKnown:
		return t.Format("01-2006")
	default:
		return t.Format("01-02-2006")
	}
}

func formatYear(d encora.Date) string {
	if t, ok := parseISO(d.FullDate); ok {
		return t.Format("2006")
	}
	if len(d.FullDate) >= isoYearLen {
		return d.FullDate[:isoYearLen]
	}
	return d.FullDate
}
