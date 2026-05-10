package imagerender

import (
	_ "embed" // for go:embed of the bundled serif TTF.
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"strings"
	"sync"

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

// titleShrinkFloor caps how aggressively a row's font may shrink when
// the text would otherwise overflow the band. 0.5 means we'll go down
// to 50 % of the configured size; below that pickFittingFace falls
// back to ellipsis truncation so long tour names + dates still read.
const titleShrinkFloor = 0.5

// ellipsis is the suffix appended when text won't fit even at the
// minimum font size. Single character so MeasureString predicts width
// correctly across the embedded serif.
const ellipsis = "…"

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
// top-to-bottom order: [date, tour, location]. Empty rows collapse —
// the rendered band only shows non-empty lines, evenly distributed
// over the band's vertical space. Returns a *image.RGBA so callers
// can hand it straight to jpeg.Encode.
func (r *Renderer) compose(src image.Image, rows []string, style Style) *image.RGBA {
	bounds := src.Bounds()
	dst := image.NewRGBA(bounds)
	draw.Draw(dst, bounds, src, bounds.Min, draw.Src)

	// Build the visible-row list. Each row's font size is fixed by its
	// position in the input slice (date small, tour large, location
	// small) — that hierarchy is part of the playbill aesthetic. Row
	// at index 1 (tour) is the headline; the others are eyebrows.
	type rowDraw struct {
		text string
		face font.Face
	}
	resolved := resolveStyle(style, image.Rect(0, 0, bounds.Dx(),
		max(int(float64(bounds.Dy())*style.BandHeightFraction), 1)))
	bandH := max(int(float64(bounds.Dy())*style.BandHeightFraction), 1)
	bandRect := image.Rect(bounds.Min.X, bounds.Max.Y-bandH, bounds.Max.X, bounds.Max.Y)
	maxTextWidth := bandRect.Dx() - hPadFactor*resolved.PadX
	if maxTextWidth < 1 {
		maxTextWidth = bandRect.Dx()
	}
	rowSizes := []float64{resolved.EyebrowSizePx, resolved.TitleSizePx, resolved.CaptionSizePx}
	rowWeights := []string{style.Eyebrow.Weight, style.Title.Weight, style.Caption.Weight}
	visible := make([]rowDraw, 0, len(rows))
	for i, raw := range rows {
		text := strings.TrimSpace(raw)
		if text == "" {
			continue
		}
		text = strings.ToUpper(text)
		spec := FontSpec{SizePx: rowSizes[i], Weight: rowWeights[i]}
		face := pickFittingFace(text, spec, maxTextWidth)
		visible = append(visible, rowDraw{
			text: truncateToFit(face, text, maxTextWidth),
			face: face,
		})
	}
	if len(visible) == 0 {
		// All rows empty — return the raw image unchanged. The user
		// explicitly cleared their overlay or the recording lacks
		// every piece of identifying metadata.
		return dst
	}

	// Paint the band only when there's something to draw on it.
	draw.Draw(dst, bandRect, &image.Uniform{C: style.BandColor}, image.Point{}, draw.Over)

	// Distribute rows evenly: each row's center sits at (i + 0.5)/n
	// of the band's height. With one row that's center; with three
	// it's the 1/6, 3/6, 5/6 marks.
	for i, row := range visible {
		fraction := (float64(i) + halfRow) / float64(len(visible))
		baselineY := bandRect.Min.Y + int(float64(bandRect.Dy())*fraction)
		drawCenteredAtBaseline(dst, row.text, row.face, style.TextColor, bandRect, baselineY)
	}

	return dst
}

// halfRow shifts each row's anchor from its top edge to its center
// when distributing N rows over the band. Pulled out as a const so
// mnd lint stays satisfied.
const halfRow = 0.5

// hPadFactor is multiplied by Style.PadX to compute the total
// horizontal padding (left + right) reserved inside the band. Pulled
// out as a const so mnd lint stays happy on the band-fit math.
const hPadFactor = 2

// Fractional sizing defaults. Title + subtitle font heights are
// expressed as fractions of the BAND height (not the image height):
// for a 14% band, the title at 0.50 fills the upper half of the
// band, the subtitle at 0.28 fills the lower portion. Padding is a
// fraction of the IMAGE width — 4% reads cleanly at any size.
//
// These kick in when style.Title.SizePx / Subtitle.SizePx / PadX are
// zero (the default) so the user can still pin an absolute value via
// the per-recording overlay_style_json blob if they want.
const (
	// Per-row font heights as fractions of the band height. Three
	// rows centered at 1/6, 3/6, 5/6 of the band each have ~1/3 of
	// the band's vertical space; font sizes leave breathing room
	// vertically. Title (the headline) is largest; eyebrow + caption
	// are smaller secondary lines.
	eyebrowSizeBandFraction = 0.22
	titleSizeBandFraction   = 0.34
	captionSizeBandFraction = 0.22
	// 9% per side = 18% total horizontal margin. The previous 7% still
	// read as edge-to-edge once a long date or venue used the interior
	// width fully — leaving real visual breathing room around even
	// a fitted line wants ~20% total.
	padXImageFraction = 0.09
	minFontSizePx     = 8
	minPadXPx         = 8
)

// resolved bundles the post-fraction-resolution sizes the compose
// function works in.
type resolvedStyle struct {
	PadX          int
	EyebrowSizePx float64
	TitleSizePx   float64
	CaptionSizePx float64
}

// resolveStyle fills in font sizes + padding from band dimensions
// when the user-supplied (or default-baked) absolute values are
// zero. Caller-supplied positive values pass through verbatim so an
// explicit `size_px` override still wins.
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
	bandH := bandRect.Dy()
	if out.EyebrowSizePx <= 0 {
		out.EyebrowSizePx = float64(bandH) * eyebrowSizeBandFraction
	}
	if out.TitleSizePx <= 0 {
		out.TitleSizePx = float64(bandH) * titleSizeBandFraction
	}
	if out.CaptionSizePx <= 0 {
		out.CaptionSizePx = float64(bandH) * captionSizeBandFraction
	}
	if out.PadX <= 0 {
		out.PadX = int(float64(bandRect.Dx()) * padXImageFraction)
	}
	if out.EyebrowSizePx < minFontSizePx {
		out.EyebrowSizePx = minFontSizePx
	}
	if out.TitleSizePx < minFontSizePx {
		out.TitleSizePx = minFontSizePx
	}
	if out.CaptionSizePx < minFontSizePx {
		out.CaptionSizePx = minFontSizePx
	}
	if out.PadX < minPadXPx {
		out.PadX = minPadXPx
	}
	return out
}

// pickFittingFace shrinks the configured size down to titleShrinkFloor
// until the rendered text fits maxWidth. Title and subtitle both go
// through this so neither row runs edge-to-edge on small images.
func pickFittingFace(text string, spec FontSpec, maxWidth int) font.Face {
	size := spec.SizePx
	if size <= 0 {
		// Should not happen — resolveStyle ensures sizes are positive
		// before this is called — but defensive fallback to the floor
		// keeps the renderer from blowing up if the path changes.
		size = minFontSizePx
	}
	floorSize := size * titleShrinkFloor
	if floorSize < minFontSizePx {
		floorSize = minFontSizePx
	}
	for size >= floorSize {
		f := loadFace(spec.Weight, int(size))
		if measureWidth(f, text) <= maxWidth {
			return f
		}
		size--
	}
	return loadFace(spec.Weight, int(floorSize))
}

// measureWidth returns the rendered pixel width of s in face f.
func measureWidth(f font.Face, s string) int {
	d := &font.Drawer{Face: f}
	return d.MeasureString(s).Round()
}

// truncateToFit drops trailing runes from text and appends an
// ellipsis until the rendered string fits maxWidth in face f. Used as
// a last resort after pickFittingFace has shrunk the font to its
// floor — a 35-character "FIRST US NATIONAL TOUR - 2024-09-22" would
// otherwise clip on a narrow poster. Returns the original text
// unchanged when it already fits.
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
		candidate := strings.TrimRight(string(runes), " -·,.") + ellipsis
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
