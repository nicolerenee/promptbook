// Package placeholder renders SVG placeholder images for the cache
// slots the imagecache package manages. The /images/* HTTP handler
// falls through to placeholder.Render on a cache miss so detail pages
// always have a usable poster/headshot/banner instead of a 404.
//
// SVG was chosen over rasterized output: it scales cleanly at any
// browser DPR, weighs ~1 KiB per image, and Go's standard library can
// assemble it with text/template alone. The rendered bytes carry
// `Content-Type: image/svg+xml` and the caller controls cache headers.
package placeholder

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
	"unicode"
	"unicode/utf8"
)

// Kind enumerates the slot types the cache stores. The placeholder
// renderer picks dimensions and visual treatment based on this.
type Kind int

const (
	// KindHeadshot renders a square, ~400x400 placeholder with
	// initials drawn inside a colored circle.
	KindHeadshot Kind = iota
	// KindShowBanner renders a 16:9 (~960x540) rectangle with the
	// label centered in serif bold against a solid color fill.
	KindShowBanner
	// KindRecordingFanart renders a wide 16:9 (~1920x1080) gradient
	// rectangle, label split on " · " separators across multiple lines.
	KindRecordingFanart
	// KindRecordingPoster renders a tall 2:3 (~600x900) gradient
	// rectangle, same multi-line treatment as KindRecordingFanart.
	KindRecordingPoster
)

// Slot dimensions. Picked to roughly match the canonical upstream
// image aspect ratios so the SVG slots into the same layout box as
// the real cached file would.
const (
	headshotSize     = 400
	showBannerWidth  = 960
	showBannerHeight = 540
	recordingFanartW = 1920
	recordingFanartH = 1080
	recordingPosterW = 600
	recordingPosterH = 900

	// halfDivisor centers a square in its container — the geometric
	// constant deserves a name so mnd doesn't flag the divide.
	halfDivisor = 2
	// circleRadiusPercent is the headshot circle radius as a percent
	// of the SVG side. 45 leaves a ~10 % margin so the rim isn't
	// flush with the slot's edge.
	circleRadiusPercent = 45
	// initialsTwoFontPercent / initialsOneFontPercent size the
	// initials text relative to headshotSize. One letter gets more
	// breathing room than two. percentDenom is the divisor that
	// turns a percent-encoded constant back into a fraction.
	initialsTwoFontPercent = 38
	initialsOneFontPercent = 52
	percentDenom           = 100

	// maxInitialsLetters caps the initials text. Three-or-more-letter
	// monograms read as a word, not initials, so we stop at two.
	maxInitialsLetters = 2

	// Banner layout tunables. lineHeightTenths ≈ 1.2 of font-size,
	// stored as tenths so the integer arithmetic stays exact.
	//
	// startFontPercent is the upper bound the auto-fit pass starts
	// from — 9% of slot height. The previous 14% was too aggressive
	// for short labels: a "Compass Theatre / Broadway / 2023" placeholder
	// fit easily at 14% and rendered with the text consuming most
	// of the canvas. Starting at 9% leaves a comfortable margin for
	// short labels while the shrink loop still tightens further when
	// content overflows.
	lineHeightTenths    = 12
	tenthsDenominator   = 10
	startFontPercent    = 9
	maxLineWidthFrac    = 0.78
	maxStackHeightFrac  = 0.55
	avgGlyphAdvanceEms  = 0.55
	verticalLineFactor  = 1.2
	minBannerFontSizePx = 14
	fontShrinkRatio     = 9

	// Color helpers. darkScale + lightScale derive gradient stops
	// from the base palette color.
	darkScale  = 0.7
	lightScale = 1.4
	hexBase    = 16
	hexAlpha10 = 10

	// rgbCeil clamps the [0,255] channel range when scaling.
	rgbCeil  = 255
	rgbHalf  = 0.5
	rgbHexLn = 7

	// fontFamily is the CSS font-family stack the SVG declares.
	// DejaVu Serif ships in our Docker image (the imagerender
	// package embeds the TTFs); the stack falls back to generic
	// serifs on hosts without it.
	fontFamily = "'DejaVu Serif',Georgia,'Times New Roman',serif"
)

// palette is the set of background colors a slot may render with. The
// chosen color is keyed off the entity id so the same person/show/
// recording always gets the same background, which makes a wall of
// placeholders read as distinct entities at a glance. Each color is
// dark enough that white text reads cleanly on top.
//
//nolint:gochecknoglobals // a deliberately velvet-antlers palette, read-only.
var palette = []string{
	"#1f3a5f", "#214c33", "#5b1a3b", "#3d2a5b",
	"#1a4d4d", "#5b3a14", "#2b3a4f", "#3a1f1a",
	"#1a3a2b", "#4d1a3a", "#2b1a4d", "#1a2b4d",
}

// pickColor returns a stable hex color for the supplied entity key.
// Same key → same palette entry; different keys hash into different
// slots (mod the palette size). Negative keys fold into the unsigned
// domain via a sign flip so the modulus stays in range.
func pickColor(key int64) string {
	abs := key
	if abs < 0 {
		abs = -abs
	}
	//nolint:gosec // sign is removed above; conversion stays in range.
	idx := uint64(abs)
	return palette[idx%uint64(len(palette))]
}

// Render returns SVG bytes for the placeholder. Callers write them to
// the HTTP response with Content-Type: image/svg+xml. label is the
// human-readable text drawn on the placeholder; for headshots it's
// the performer's full name (we extract the initials), for shows the
// show name, for recordings a pre-formatted "Show · Tour · Date"
// string. key is the entity id and drives a stable color choice from
// the curated palette.
func Render(kind Kind, key int64, label string) ([]byte, error) {
	color := pickColor(key)
	switch kind {
	case KindHeadshot:
		return renderHeadshot(color, label)
	case KindShowBanner:
		return renderBanner(showBannerWidth, showBannerHeight, color, label, false)
	case KindRecordingFanart:
		return renderBanner(recordingFanartW, recordingFanartH, color, label, true)
	case KindRecordingPoster:
		return renderBanner(recordingPosterW, recordingPosterH, color, label, true)
	default:
		return nil, fmt.Errorf("placeholder: unknown kind %d", kind)
	}
}

// initialsFor extracts up to maxInitialsLetters first letters from the
// space-separated words of label. Empty / whitespace-only labels yield
// "?", a deliberate sentinel the UI can still render meaningfully.
func initialsFor(label string) string {
	fields := strings.Fields(label)
	if len(fields) == 0 {
		return "?"
	}
	var b strings.Builder
	for _, w := range fields {
		r, size := utf8.DecodeRuneInString(w)
		if r == utf8.RuneError && size <= 1 {
			continue
		}
		b.WriteRune(unicode.ToUpper(r))
		if b.Len() >= maxInitialsLetters {
			break
		}
	}
	if b.Len() == 0 {
		return "?"
	}
	return b.String()
}

// headshotTmpl renders the circular headshot placeholder. text/template
// (rather than html/template) is correct here because SVG is its own
// XML dialect — html/template's escaping confuses CSS-style attributes
// like font-family. The inputs we interpolate are all server-derived
// (entity name from the DB, palette color, initials we just computed),
// so escape-on-output isn't load-bearing for security; it would just
// mangle valid characters like apostrophes.
//
//nolint:gochecknoglobals // immutable parsed template, lifetime == process.
var headshotTmpl = template.Must(template.New("headshot").Parse(
	`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {{.Size}} {{.Size}}" ` +
		`width="{{.Size}}" height="{{.Size}}" role="img" aria-label="{{.AriaLabel}}">
<rect width="{{.Size}}" height="{{.Size}}" fill="#f1f1f1"/>
<circle cx="{{.Cx}}" cy="{{.Cy}}" r="{{.Radius}}" fill="{{.Color}}"/>
<text x="50%" y="50%" text-anchor="middle" dominant-baseline="central" ` +
		`font-family="{{.FontFamily}}" font-weight="700" font-size="{{.FontSize}}" ` +
		`fill="#ffffff">{{.Initials}}</text>
</svg>`))

type headshotData struct {
	Size       int
	Cx         int
	Cy         int
	Radius     int
	FontSize   int
	Color      string
	Initials   string
	AriaLabel  string
	FontFamily string
}

// renderHeadshot fills headshotTmpl from label/color into bytes. The
// font-size scales loosely with the letter count so two-letter
// monograms don't run past the circle's diameter.
func renderHeadshot(color, label string) ([]byte, error) {
	initials := initialsFor(label)
	fontSize := headshotSize * initialsTwoFontPercent / percentDenom
	if utf8.RuneCountInString(initials) <= 1 {
		fontSize = headshotSize * initialsOneFontPercent / percentDenom
	}
	data := headshotData{
		Size:       headshotSize,
		Cx:         headshotSize / halfDivisor,
		Cy:         headshotSize / halfDivisor,
		Radius:     headshotSize*circleRadiusPercent/percentDenom - 1,
		FontSize:   fontSize,
		Color:      color,
		Initials:   initials,
		AriaLabel:  ariaLabelFor(label),
		FontFamily: fontFamily,
	}
	var buf bytes.Buffer
	if err := headshotTmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("placeholder: render headshot: %w", err)
	}
	return buf.Bytes(), nil
}

// bannerTmpl renders the rectangular slot kinds (show banner, fanart,
// poster). Multi-line labels are emitted as <tspan> rows; the gradient
// is included unconditionally and toggled by the Gradient flag.
//
//nolint:gochecknoglobals // immutable parsed template.
var bannerTmpl = template.Must(template.New("banner").Parse(
	`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {{.Width}} {{.Height}}" ` +
		`width="{{.Width}}" height="{{.Height}}" preserveAspectRatio="xMidYMid meet" ` +
		`role="img" aria-label="{{.AriaLabel}}">
<defs>
<linearGradient id="g" x1="0" y1="0" x2="1" y2="1">
<stop offset="0" stop-color="{{.DarkColor}}"/>
<stop offset="1" stop-color="{{.LightColor}}"/>
</linearGradient>
</defs>
<rect width="{{.Width}}" height="{{.Height}}" ` +
		`fill="{{if .Gradient}}url(#g){{else}}{{.Color}}{{end}}"/>
<text x="50%" y="50%" text-anchor="middle" dominant-baseline="central" ` +
		`font-family="{{.FontFamily}}" font-weight="700" font-size="{{.FontSize}}" ` +
		`fill="#ffffff">
{{- range $i, $line := .Lines}}
<tspan x="50%" dy="{{if eq $i 0}}{{$.FirstDy}}{{else}}{{$.LineHeight}}{{end}}">{{$line}}</tspan>
{{- end}}
</text>
</svg>`))

type bannerData struct {
	Width      int
	Height     int
	Color      string
	DarkColor  string
	LightColor string
	Gradient   bool
	FontSize   int
	LineHeight int
	FirstDy    int
	Lines      []string
	AriaLabel  string
	FontFamily string
}

// renderBanner fills bannerTmpl. gradient toggles between a solid fill
// (used for show banners — they're flatter, more "logo plate") and a
// diagonal gradient (used for recording slots — they evoke the
// imagerender backdrop look).
func renderBanner(width, height int, color, label string, gradient bool) ([]byte, error) {
	lines := splitLabel(label)
	if len(lines) == 0 {
		lines = []string{"?"}
	}
	fontSize := autoFontSize(width, height, lines)
	lineHeight := fontSize * lineHeightTenths / tenthsDenominator
	// FirstDy shifts the whole text block up by half the total stack
	// height so the rendered lines sit visually centered on y="50%".
	// With dominant-baseline:central, dy=0 puts line 1 on the center;
	// we want the midpoint of all lines on the center, so we subtract
	// ((n-1)/2) * lineHeight.
	firstDy := -((len(lines) - 1) * lineHeight) / halfDivisor
	dark, light := shadePair(color)
	data := bannerData{
		Width:      width,
		Height:     height,
		Color:      color,
		DarkColor:  dark,
		LightColor: light,
		Gradient:   gradient,
		FontSize:   fontSize,
		LineHeight: lineHeight,
		FirstDy:    firstDy,
		Lines:      lines,
		AriaLabel:  ariaLabelFor(label),
		FontFamily: fontFamily,
	}
	var buf bytes.Buffer
	if err := bannerTmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("placeholder: render banner: %w", err)
	}
	return buf.Bytes(), nil
}

// splitLabel breaks the label on " · " separators (the canonical
// recording-label separator the API + nfo layer use). Trims surrounding
// whitespace from each segment. Empty segments are dropped. An empty
// input yields a nil slice, which renderBanner converts to ["?"].
func splitLabel(label string) []string {
	if strings.TrimSpace(label) == "" {
		return nil
	}
	parts := strings.Split(label, " · ")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// autoFontSize picks a font size that keeps the longest line under
// ~85% of the slot width and the full text stack under ~80% of the
// height. The math approximates DejaVu Serif at ~0.55 ems per glyph
// average advance — close enough for placeholder use; we're not
// laying out a typeset paragraph here.
func autoFontSize(width, height int, lines []string) int {
	longest := 0
	for _, l := range lines {
		if n := utf8.RuneCountInString(l); n > longest {
			longest = n
		}
	}
	if longest == 0 {
		longest = 1
	}
	// Start at startFontPercent of the slot height; shrink until both
	// width and height constraints fit. Floor at minBannerFontSizePx
	// so glyphs stay legible.
	size := height * startFontPercent / percentDenom
	for size > minBannerFontSizePx {
		w := float64(longest) * avgGlyphAdvanceEms * float64(size)
		h := float64(len(lines)) * verticalLineFactor * float64(size)
		if w <= float64(width)*maxLineWidthFrac && h <= float64(height)*maxStackHeightFrac {
			return size
		}
		size = size * fontShrinkRatio / tenthsDenominator
	}
	return size
}

// shadePair returns a darker / lighter pair derived from the supplied
// hex base. The darker stop sits in the top-left of the gradient, the
// lighter stop in the bottom-right; together they evoke a stage-light
// wash without committing to a real image. Bad input falls through to
// the base color for both stops (no crash).
func shadePair(hex string) (string, string) {
	r, g, b, ok := parseHex(hex)
	if !ok {
		return hex, hex
	}
	dark := rgbHex(scaleChannel(r, darkScale), scaleChannel(g, darkScale), scaleChannel(b, darkScale))
	light := rgbHex(scaleChannel(r, lightScale), scaleChannel(g, lightScale), scaleChannel(b, lightScale))
	return dark, light
}

// parseHex expects "#rrggbb" and returns the three channels plus an ok
// flag. Anything else returns ok=false; callers fall back to the base
// color so a stray palette entry can't crash a render.
func parseHex(hex string) (int, int, int, bool) {
	if len(hex) != rgbHexLn || hex[0] != '#' {
		return 0, 0, 0, false
	}
	var v [3]int
	for i := range 3 {
		hi, ok1 := hexNibble(hex[1+i*2])
		lo, ok2 := hexNibble(hex[2+i*2])
		if !ok1 || !ok2 {
			return 0, 0, 0, false
		}
		v[i] = hi*hexBase + lo
	}
	return v[0], v[1], v[2], true
}

// hexNibble decodes a single hex digit. Returns ok=false on a non-hex
// byte rather than panicking; the caller handles the error path.
func hexNibble(c byte) (int, bool) {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0'), true
	case c >= 'a' && c <= 'f':
		return int(c-'a') + hexAlpha10, true
	case c >= 'A' && c <= 'F':
		return int(c-'A') + hexAlpha10, true
	default:
		return 0, false
	}
}

// scaleChannel multiplies a channel by factor, clamping to [0, 255].
// Used to derive lighter/darker palette stops without leaving the
// 8-bit RGB cube.
func scaleChannel(v int, factor float64) int {
	out := int(float64(v)*factor + rgbHalf)
	if out < 0 {
		return 0
	}
	if out > rgbCeil {
		return rgbCeil
	}
	return out
}

// rgbHex re-encodes three [0,255] channels as "#rrggbb".
func rgbHex(r, g, b int) string {
	return fmt.Sprintf("#%02x%02x%02x", r, g, b)
}

// ariaLabelFor returns a non-empty aria-label for the SVG. Empty input
// becomes "Placeholder" so the rendered SVG remains accessible even
// for unknown entities.
func ariaLabelFor(label string) string {
	if t := strings.TrimSpace(label); t != "" {
		return t
	}
	return "Placeholder"
}
