package imagerender

import (
	_ "embed" // for go:embed of the bundled serif TTF.
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// Embedded fonts. DejaVu Serif is licensed under the Bitstream Vera
// Fonts license, which permits redistribution with software (see
// internal/imagerender/assets/LICENSE for the full text).
var (
	//go:embed assets/DejaVuSerif-Bold.ttf
	dejaVuSerifBoldBytes []byte

	//go:embed assets/DejaVuSerif.ttf
	dejaVuSerifRegularBytes []byte
)

// Font caches. opentype.Parse is non-trivial; cache the parsed font
// objects so a render-heavy session doesn't re-parse the TTF on every
// call. Faces are cheaper but still indexed by size, weight, and DPI.
//
//nolint:gochecknoglobals // package-level font caches; lifetime == process.
var (
	parsedBoldOnce    sync.Once
	parsedRegularOnce sync.Once
	parsedBold        *opentype.Font
	parsedRegular     *opentype.Font
	errParseBold      error
	errParseRegular   error

	faceMu sync.Mutex
	faces  = map[faceKey]font.Face{}
)

type faceKey struct {
	weight string
	sizePx int
}

// renderDPI is the DPI fed to opentype.NewFace. 72 DPI means 1 pt == 1
// pixel, which lets callers think in pixels without a unit dance.
const renderDPI = 72

// ellipsis is the suffix appended when text won't fit even at the
// minimum font size. Single character so MeasureString predicts width
// correctly across the embedded serif.
const ellipsis = "…"

// ellipsisTrimChars is the set of trailing characters stripped off a
// truncated string before the ellipsis is appended. We pull off
// whitespace, separators (- – — · , . ; :), and orphan openers
// (( [ { " ' “ ‘ «) so the result reads as the start of a phrase
// trailed by ellipsis rather than something like "TOUR (…" or
// "TOUR -…". Closing punctuation is intentionally not included since
// "TOUR)…" is already broken (the opener is gone) and we don't want
// to chew further than the truncation point would already.
const ellipsisTrimChars = " -–—·,.;:([{\"'“‘«"

// loadFont parses one of the embedded TTFs and caches the result.
func loadFont(weight string) (*opentype.Font, error) {
	switch weight {
	case WeightRegular:
		parsedRegularOnce.Do(func() {
			parsedRegular, errParseRegular = opentype.Parse(dejaVuSerifRegularBytes)
		})
		if errParseRegular != nil {
			return nil, fmt.Errorf("parse regular ttf: %w", errParseRegular)
		}
		return parsedRegular, nil
	default:
		// "bold" + anything we don't recognize maps to bold. The default
		// style uses bold for the title; subtitle is regular.
		parsedBoldOnce.Do(func() {
			parsedBold, errParseBold = opentype.Parse(dejaVuSerifBoldBytes)
		})
		if errParseBold != nil {
			return nil, fmt.Errorf("parse bold ttf: %w", errParseBold)
		}
		return parsedBold, nil
	}
}

// loadFace builds (or reuses) a font.Face for the given weight + pixel
// height. Falls back to basicfont.Face7x13 when the embedded TTF can't
// be parsed — keeps the renderer alive in degraded environments.
func loadFace(weight string, sizePx int) font.Face {
	if sizePx <= 0 {
		sizePx = 1
	}
	key := faceKey{weight: weight, sizePx: sizePx}
	faceMu.Lock()
	defer faceMu.Unlock()
	if f, ok := faces[key]; ok {
		return f
	}
	parsed, err := loadFont(weight)
	if err != nil {
		// Fallback to the bitmap face. Ugly, but it draws.
		faces[key] = basicfont.Face7x13
		return faces[key]
	}
	face, err := opentype.NewFace(parsed, &opentype.FaceOptions{
		Size:    float64(sizePx),
		DPI:     renderDPI,
		Hinting: font.HintingFull,
	})
	if err != nil {
		faces[key] = basicfont.Face7x13
		return faces[key]
	}
	faces[key] = face
	return face
}

// compose builds the burned-in image. rows is the per-line text in
// top-to-bottom order: [date, tour, location]. Each row occupies a
// FIXED slot at (i+0.5)/3 of the band height — an empty row stays
// empty (no shifting), so a recording missing the date renders the
// tour + venue at the same vertical positions as a complete
// 3-row recording. Per-row font size is driven by character budget
// (see charBudgetSize) capped by the slot height, so dates always
// render at the same scale across recordings regardless of length.
// Returns a *image.RGBA so callers can hand it straight to jpeg.Encode.
func compose(src image.Image, rows []string, style Style) *image.RGBA {
	bounds := src.Bounds()
	dst := image.NewRGBA(bounds)
	draw.Draw(dst, bounds, src, bounds.Min, draw.Src)

	resolved := resolveStyle(style, image.Rect(0, 0, bounds.Dx(),
		max(int(float64(bounds.Dy())*style.BandHeightFraction), 1)))
	bandH := max(int(float64(bounds.Dy())*style.BandHeightFraction), 1)
	bandRect := image.Rect(bounds.Min.X, bounds.Max.Y-bandH, bounds.Max.X, bounds.Max.Y)
	maxTextWidth := bandRect.Dx() - hPadFactor*resolved.PadX
	if maxTextWidth < 1 {
		maxTextWidth = bandRect.Dx()
	}

	// Per-slot configuration. Indexes line up with the rows arg the
	// caller passes in: 0=date (eyebrow), 1=tour (title, bold),
	// 2=location (caption). Three slots are ALWAYS allocated; empty
	// rows just don't draw anything in their slot, preserving vertical
	// alignment across recordings of different metadata completeness.
	//
	// The title slot gets ~2x the vertical room of the eyebrow/caption
	// slots so it dominates as the headline (Cresthaven-style). Center
	// fractions are explicit per-slot rather than (i+0.5)/3 because
	// the slots are unequal in height. maxChars > 0 hard-truncates the
	// row's text to that rune count (with the trailing rune being a
	// single ellipsis "…") before sizing — keeps long tour names from
	// shrinking the font into illegibility, and matches the legacy
	// "tour over 25 chars gets ellipsized" behavior.
	type slotCfg struct {
		minChars int
		maxChars int
		weight   string
		sizeCap  float64 // upper bound on font size to keep slot from overflowing.
		center   float64 // vertical center of the slot as a fraction of band height.
		override float64 // user-pinned absolute size (>0 wins over char budget).
	}
	slots := [overlayRowCount]slotCfg{
		{minChars: eyebrowMinChars, weight: style.Eyebrow.Weight,
			sizeCap: float64(bandH) * eyebrowSlotCap, center: eyebrowSlotCenter,
			override: resolved.EyebrowSizePx},
		{minChars: titleMinChars, maxChars: titleMaxChars,
			weight:  style.Title.Weight,
			sizeCap: float64(bandH) * titleSlotCap, center: titleSlotCenter,
			override: resolved.TitleSizePx},
		{minChars: captionMinChars, weight: style.Caption.Weight,
			sizeCap: float64(bandH) * captionSlotCap, center: captionSlotCenter,
			override: resolved.CaptionSizePx},
	}

	type rowDraw struct {
		text  string
		face  font.Face
		empty bool
	}
	var draws [overlayRowCount]rowDraw
	hasText := false
	for i := range overlayRowCount {
		var raw string
		if i < len(rows) {
			raw = rows[i]
		}
		text := strings.ToUpper(strings.TrimSpace(raw))
		if text == "" {
			draws[i] = rowDraw{empty: true}
			continue
		}
		// Hard char-count cap (currently only the title row uses it).
		// We truncate BEFORE char-budget sizing so a 35-char tour name
		// doesn't drag the font down to "size to fit 35 chars" — instead
		// it gets sized for 25 chars and the rest are dropped behind a
		// trailing ellipsis.
		if slots[i].maxChars > 0 {
			text = truncateToCharLimit(text, slots[i].maxChars)
		}
		hasText = true
		size := slots[i].override
		if size <= 0 {
			size = charBudgetSize(text, slots[i].minChars, slots[i].weight, maxTextWidth)
		}
		if size > slots[i].sizeCap && slots[i].sizeCap > 0 {
			size = slots[i].sizeCap
		}
		if size < minFontSizePx {
			size = minFontSizePx
		}
		face := loadFace(slots[i].weight, int(size))
		draws[i] = rowDraw{
			text: truncateToFit(face, text, maxTextWidth),
			face: face,
		}
	}
	if !hasText {
		// No rows have text. Return the raw image — don't paint a
		// blank band over the source.
		return dst
	}

	// Paint the band only when there's something to draw on it.
	draw.Draw(dst, bandRect, &image.Uniform{C: style.BandColor}, image.Point{}, draw.Over)

	// Fixed slot positions per slot configuration. Empty rows skip
	// drawing but still consume their slot so the visible rows stay
	// anchored regardless of which siblings have content. The "extra
	// padding at top + bottom" the band has at 0.20 frac comes from
	// the slot caps + slot centers being chosen to leave ~5 % margin
	// at each end and ~5 % between adjacent slots.
	for i, row := range draws {
		if row.empty {
			continue
		}
		baselineY := bandRect.Min.Y + int(float64(bandRect.Dy())*slots[i].center)
		drawCenteredAtBaseline(dst, row.text, row.face, style.TextColor, bandRect, baselineY)
	}

	return dst
}

// charBudgetSize returns the font pixel size where a string of
// max(minChars, len(text)) characters would fill availableWidth. Use:
//   - actual text wins when it has more characters than the minimum,
//     so a long tour name shrinks to fit instead of overflowing;
//   - a synthetic "M" * minChars sample wins when text is shorter,
//     locking the type at a stable size. Two dates of different lengths
//     ("2024" vs "2024-12-31") therefore render at the same scale.
//
// "M" is the widest cap glyph in DejaVu Serif so the sample is a
// conservative upper bound — actual text rendered at the resulting
// size always fits with margin.
func charBudgetSize(text string, minChars int, weight string, availableWidth int) float64 {
	const probeSize = 100.0
	sizingText := text
	if utf8.RuneCountInString(text) < minChars {
		sizingText = strings.Repeat("M", minChars)
	}
	probeFace := loadFace(weight, int(probeSize))
	probeWidth := measureWidth(probeFace, sizingText)
	if probeWidth <= 0 {
		return minFontSizePx
	}
	return probeSize * float64(availableWidth) / float64(probeWidth)
}

// hPadFactor is multiplied by Style.PadX to compute the total
// horizontal padding (left + right) reserved inside the band. Pulled
// out as a const so mnd lint stays happy on the band-fit math.
const hPadFactor = 2

// Layout constants. Fonts are driven primarily by character budget
// (eyebrowMinChars / titleMinChars / captionMinChars), with a per-row
// SLOT CAP that prevents a row from overflowing its allocated band
// region when the budget would yield a huge font (short text on a
// wide image). Per-recording overlay_style_json with size_px > 0
// bypasses both knobs.
//
// Visual hierarchy is title-centric: the title (tour name) is the
// headline so it gets ~2x the vertical room of the eyebrow (date)
// and caption (venue). Slot centers + caps are chosen so the band
// reads as
//
//	┌─5% top padding───────────┐
//	│  date (slot 1, ~20%)      │
//	├─5% gap─────────┤
//	│  TOUR (slot 2, ~40%)      │
//	├─5% gap─────────┤
//	│  venue (slot 3, ~20%)     │
//	└─5% bottom padding─────────┘
const (
	// Per-row font caps as fractions of the band height. Title gets
	// the largest share so it remains the visual headline; the date
	// + caption rows are quieter. The caption cap is generous enough
	// (0.28) that long venue strings can grow toward it on
	// high-resolution sources where horizontal space stops binding.
	eyebrowSlotCap = 0.20
	titleSlotCap   = 0.34
	captionSlotCap = 0.28
	// Per-row vertical centers as fractions of band height. The
	// title sits at the visual midpoint of its neighbors (0.15 + 0.82
	// → 0.485) rather than the band's geometric center (0.50). At
	// 0.50 the title visibly crowds the caption below it because the
	// caption slot reaches up further than the eyebrow reaches down.
	// Caption sits at 0.82 (a hair higher than 0.85) so the taller
	// caption cap doesn't push the row off the bottom edge. Empty
	// rows still consume their slot so a 2-row recording aligns to
	// the same Y coordinates as a 3-row one.
	eyebrowSlotCenter = 0.15
	titleSlotCenter   = 0.48
	captionSlotCenter = 0.82
	// Minimum character widths. The renderer sizes each row's font
	// against max(minChars, len(text)) characters, so the row's type
	// stays at a stable scale across recordings: a 4-char date
	// "2024" and a 10-char date "2024-12-31" render at the same
	// height because both are sized for the 12-char minimum.
	//
	// titleMinChars at 8 lets "BROADWAY" (the most common short tour
	// label, 8 chars) hit the title slot cap so it fills the full
	// slot height. captionMinChars at 22 lets the standard
	// "VENUE, CITY" pattern (typically 18-26 chars) get sized close
	// to its actual width without an oversized 25-char floor pinning
	// it down.
	eyebrowMinChars = 12
	titleMinChars   = 8
	captionMinChars = 22
	// titleMaxChars hard-caps the rendered tour name so a verbose
	// label like "FIRST US NATIONAL TOUR (NON-EQUITY)" doesn't shrink
	// the title font into illegibility. 25 total characters with the
	// 25th being a trailing "…" when truncation kicked in (so 24
	// content runes + ellipsis). Mirrors the pre-rewrite legacy
	// behavior; the eyebrow + caption rows have no hard cap because
	// dates and venue strings tend to fit naturally.
	titleMaxChars = 25
	// 6% per side = 12% total horizontal margin. The earlier 9 % was
	// over-cautious — on a 230-px-wide poster the difference between
	// 0.09 and 0.06 padding is the difference between a 12 px caption
	// font and a ~14 px one, which is the gap between "tiny" and
	// "readable" for venue text like "PALACE THEATRE, NEW YORK".
	padXImageFraction = 0.06
	minFontSizePx     = 8
	minPadXPx         = 8
	// overlayRowCount is the always-allocated number of slots in the
	// band: date (eyebrow), tour (title), location (caption). Each row
	// occupies its slot regardless of whether siblings are populated,
	// so a 2-row recording renders at the same vertical positions as
	// a 3-row recording would.
	overlayRowCount = 3
)

// resolved bundles the post-fraction-resolution sizes the compose
// function works in.
type resolvedStyle struct {
	PadX          int
	EyebrowSizePx float64
	TitleSizePx   float64
	CaptionSizePx float64
}

// resolveStyle returns the user-supplied (or default-baked) absolute
// values for each row plus the horizontal padding. Sizes default to
// 0 — meaning "let charBudgetSize compute the row's font" — and only
// non-zero values pass through as absolute overrides that bypass the
// char budget entirely. Padding falls back to padXImageFraction of
// the band width when unset.
//
// Legacy `Subtitle` is honored as an alias for Caption when set —
// keeps older overlay_style_json blobs working without forcing
// migration. Eyebrow has no legacy alias since it didn't exist before.
func resolveStyle(style Style, bandRect image.Rectangle) resolvedStyle {
	captionPx := style.Caption.SizePx
	if captionPx <= 0 {
		captionPx = style.Subtitle.SizePx
	}
	out := resolvedStyle{
		PadX:          style.PadX,
		EyebrowSizePx: style.Eyebrow.SizePx,
		TitleSizePx:   style.Title.SizePx,
		CaptionSizePx: captionPx,
	}
	if out.PadX <= 0 {
		out.PadX = int(float64(bandRect.Dx()) * padXImageFraction)
	}
	if out.PadX < minPadXPx {
		out.PadX = minPadXPx
	}
	return out
}

// measureWidth returns the rendered pixel width of s in face f.
func measureWidth(f font.Face, s string) int {
	d := &font.Drawer{Face: f}
	return d.MeasureString(s).Round()
}

// truncateToCharLimit caps text at maxChars runes total, replacing
// the last visible rune with a trailing ellipsis when truncation
// happens. So maxChars=25 means up to 24 source runes plus the "…".
// Trailing punctuation/whitespace is trimmed before the ellipsis is
// appended so we never produce strings like "FIRST US NATIONAL TO -…".
func truncateToCharLimit(text string, maxChars int) string {
	if maxChars <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= maxChars {
		return text
	}
	keep := maxChars - 1
	if keep < 1 {
		return ellipsis
	}
	cut := strings.TrimRight(string(runes[:keep]), ellipsisTrimChars)
	return cut + ellipsis
}

// truncateToFit drops trailing runes from text and appends an
// ellipsis until the rendered string fits maxWidth in face f. The
// char-budget sizer typically yields a font where the actual text
// fits with margin, but it can be wrong when the actual text is
// longer than expected (e.g., the slot cap binds before the budget
// would have, leaving the actual text too wide for the resulting
// face). Truncation is the last-resort safety net.
func truncateToFit(f font.Face, text string, maxWidth int) string {
	if maxWidth <= 0 {
		return text
	}
	if measureWidth(f, text) <= maxWidth {
		return text
	}
	// Walk runes from the end. Cheaper than shrinking by bytes which
	// would cut multibyte characters mid-codepoint.
	runes := []rune(text)
	for len(runes) > 1 {
		runes = runes[:len(runes)-1]
		candidate := strings.TrimRight(string(runes), ellipsisTrimChars) + ellipsis
		if measureWidth(f, candidate) <= maxWidth {
			return candidate
		}
	}
	// Single rune still won't fit (vanishingly small band). Return
	// the ellipsis alone rather than overflow.
	return ellipsis
}

// drawCenteredAtBaseline renders s horizontally centered inside band
// with its baseline at the supplied Y coordinate. The band's vertical
// distribution math (which row goes where) lives in compose.go's row
// loop — this function just paints one row.
//
// Defensive guards reposition the baseline if the requested Y would
// push the rendered text off the band's top or bottom edge.
func drawCenteredAtBaseline(
	dst draw.Image,
	s string,
	face font.Face,
	col color.Color,
	band image.Rectangle,
	baselineY int,
) {
	if s == "" {
		return
	}
	d := &font.Drawer{
		Dst:  dst,
		Src:  &image.Uniform{C: col},
		Face: face,
	}
	width := d.MeasureString(s)
	x := fixed.I(band.Min.X+band.Dx()/centerDivisor) - width/centerDivisor

	metrics := face.Metrics()
	ascent := metrics.Ascent.Round()
	descent := metrics.Descent.Round()
	// Center the visual line on the supplied baseline by shifting it
	// down by half the (ascent-descent) — the supplied baselineY is
	// the row's CENTER, not its true baseline.
	baselineY += (ascent - descent) / centerDivisor

	if baselineY-ascent < band.Min.Y {
		baselineY = band.Min.Y + ascent
	}
	if baselineY+descent > band.Max.Y {
		baselineY = band.Max.Y - descent
	}

	d.Dot = fixed.Point26_6{X: x, Y: fixed.I(baselineY)}
	d.DrawString(s)
}

// centerDivisor is the integer 2 used in (a+b)/2 averages and width/2
// centering math. Pulled out so mnd lint doesn't flag the arithmetic.
const centerDivisor = 2
