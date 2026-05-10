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
//	{Date}             smart: ISO when full, "December 2009" if !DayKnown,
//	                   "2009" if !MonthKnown.
//	{DateUS}           "December 1, 2009" / "December 2009" / "2009"
//	{DateNumeric}      "12-01-2009" / "12-2009" / "2009"
//	{Year}             always 4-digit year
//	{Variant}          blank or "v2" / "v4" matching DateVariant
//	{DateWithVariant}  ISO-partial date with " (N)" variant suffix:
//	                   "2009-12" / "2024-09-15 (4)" / "1979".
//
// Probe-derived tokens (only resolve when PlanInputs.MediaInfo / Part
// are populated by the caller — empty otherwise):
//
//	{Container}   uppercase extension, e.g. "MKV"
//	{VideoCodec}  ffprobe codec_name, e.g. "h264"
//	{Quality}     height-mapped label, e.g. "1080p"
//	{Part}        integer part index, e.g. "1"; empty when Part==0
//
// Optional segments use a `{?...}` wrapper. The body may mix literals
// and `{Token}` references; the entire segment renders only when every
// referenced token resolves to a non-empty string. A segment that
// references no tokens is a parse error — there is nothing to make it
// conditional on, and the user almost certainly meant `{?{Foo}}` or a
// plain literal.
//
// Other tokens are straight string substitution.

// tokenPattern matches `{Identifier}`. Used inside required-token
// substitution and inside an optional segment's body.
var tokenPattern = regexp.MustCompile(`\{([A-Za-z]+)\}`)

// optionalSegmentPattern matches a `{?...}` block. The inner body is
// captured and inspected separately. The body excludes the literal
// `{` and `}` characters so a block can't accidentally swallow another
// optional segment — nested optionals are not supported (and rarely
// useful given the conjunctive semantics).
var optionalSegmentPattern = regexp.MustCompile(`\{\?([^{}]*(?:\{[A-Za-z]+\}[^{}]*)*)\}`)

// isoYearLen is the length of the year prefix on Encora's ISO date strings.
const isoYearLen = 4

// templateContext bundles the rendering inputs so resolveToken can pull
// from media-probe + filename-parse results in addition to the plain
// Recording. PlanInputs flattens this back into a single struct at the
// caller; Apply takes a pointer so the zero value is fine for callers
// that only need the recording-driven tokens.
type templateContext struct {
	Recording encora.Recording
	Container string
	Codec     string
	Quality   string
	Part      int
}

// Apply renders tmpl by substituting every {Token} against r. Unknown
// tokens are left in place so a downstream linter can surface them. The
// result is then sanitized for filesystem use (slashes/colons removed).
//
// Apply only consumes recording-driven tokens; ApplyContext exposes the
// probe-derived tokens. Kept as a thin wrapper so existing call sites
// stay source-compatible.
func Apply(tmpl string, r encora.Recording) (string, error) {
	return ApplyContext(tmpl, templateContext{Recording: r})
}

// ApplyContext is the full-fat entry point used by BuildPlan. The
// templateContext carries probe-derived fields ({Container} /
// {VideoCodec} / {Quality} / {Part}) alongside the encora.Recording.
func ApplyContext(tmpl string, ctx templateContext) (string, error) {
	if tmpl == "" {
		return "", nil
	}

	// First pass: resolve optional segments. We do this before the
	// required-token pass so a segment that drops out doesn't leave
	// dangling token references for the second pass to choke on.
	rendered, err := renderOptionalSegments(tmpl, ctx)
	if err != nil {
		return "", err
	}

	// Second pass: substitute the remaining required {Token}s.
	var rerr error
	out := tokenPattern.ReplaceAllStringFunc(rendered, func(match string) string {
		token := match[1 : len(match)-1] // strip { }.
		val, terr := resolveToken(token, ctx)
		if terr != nil {
			if rerr == nil {
				rerr = terr
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

// renderOptionalSegments expands every `{?...}` block. A block renders
// to its inner body (with `{Token}` substitutions applied) when every
// referenced token resolves to a non-empty string, or to "" when any
// reference is empty. A block with no token references is rejected as
// a parse error.
func renderOptionalSegments(tmpl string, ctx templateContext) (string, error) {
	var rerr error
	out := optionalSegmentPattern.ReplaceAllStringFunc(tmpl, func(match string) string {
		expanded, err := renderOptionalSegment(match, ctx)
		if err != nil && rerr == nil {
			rerr = err
		}
		return expanded
	})
	if rerr != nil {
		return "", rerr
	}
	return out, nil
}

// renderOptionalSegment evaluates a single `{?...}` block. Returns the
// expanded body (or "" when any referenced token resolved empty), plus
// any unknown-token / token-less error encountered along the way.
func renderOptionalSegment(match string, ctx templateContext) (string, error) {
	// Strip the leading `{?` and trailing `}` to recover the body.
	body := match[2 : len(match)-1]
	refs := tokenPattern.FindAllStringSubmatch(body, -1)
	if len(refs) == 0 {
		return match, fmt.Errorf(
			"rename: optional segment %q references no tokens", match)
	}
	// Resolve each referenced token. Bail to "" the moment any
	// resolves to an empty string. Any unknown-token error
	// short-circuits the whole render.
	for _, ref := range refs {
		val, err := resolveToken(ref[1], ctx)
		if err != nil {
			return match, err
		}
		if val == "" {
			return "", nil
		}
	}
	// All tokens resolved non-empty; substitute them inside the body
	// and emit the literal-around plus values.
	var rerr error
	expanded := tokenPattern.ReplaceAllStringFunc(body, func(m string) string {
		val, err := resolveToken(m[1:len(m)-1], ctx)
		if err != nil {
			if rerr == nil {
				rerr = err
			}
			return m
		}
		return val
	})
	return expanded, rerr
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

func resolveToken(token string, ctx templateContext) (string, error) {
	r := ctx.Recording
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
	case "DateWithVariant":
		return formatDateWithVariant(r.Date), nil
	case "Year":
		return formatYear(r.Date), nil
	case "Variant":
		if r.Date.DateVariant != nil && *r.Date.DateVariant != "" {
			return "v" + *r.Date.DateVariant, nil
		}
		return "", nil
	case "Container":
		return ctx.Container, nil
	case "VideoCodec":
		return ctx.Codec, nil
	case "Quality":
		return ctx.Quality, nil
	case "Part":
		if ctx.Part <= 0 {
			return "", nil
		}
		return strconv.Itoa(ctx.Part), nil
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

// formatDateISOPartial is the {DateWithVariant} body. Unlike
// formatDateSmart, partial-month dates render as "2009-12" rather than
// "December 2009" so the new default templates produce a clean
// ISO-flavored "(YYYY-MM)" disambiguator.
func formatDateISOPartial(d encora.Date) string {
	t, ok := parseISO(d.FullDate)
	if !ok {
		return d.FullDate
	}
	switch {
	case !d.MonthKnown:
		return t.Format("2006")
	case !d.DayKnown:
		return t.Format("2006-01")
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

// formatDateWithVariant renders an ISO-flavored partial date and tacks
// on a " (N)" disambiguator when DateVariant is set. The new default
// templates inline this so users get e.g. "Into the Woods (2022-05 (4))"
// without having to compose two separate tokens. Distinct from {Date}
// in that partial-month dates render as "2009-12" instead of
// "December 2009" — the bracket-friendly form chosen for the new
// canonical filename layout.
func formatDateWithVariant(d encora.Date) string {
	base := formatDateISOPartial(d)
	if d.DateVariant == nil {
		return base
	}
	v := strings.TrimSpace(*d.DateVariant)
	if v == "" {
		return base
	}
	return base + " (" + v + ")"
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
