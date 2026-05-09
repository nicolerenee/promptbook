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

// titleShrinkFloor caps how aggressively the title font may shrink when
// the show name would otherwise overflow the band. 0.7 means we'll go
// down to 70 % of the configured size; beyond that, we accept overflow
// rather than drawing illegible 14 px text.
const titleShrinkFloor = 0.7

// loadFont parses one of the embedded TTFs and caches the result.
func loadFont(weight string) (*opentype.Font, error) {
	switch weight {
	case "regular":
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

// compose builds the burned-in image. Returns a *image.RGBA so callers
// can hand it straight to jpeg.Encode.
func (r *Renderer) compose(src image.Image, title, subtitle string, style Style) *image.RGBA {
	bounds := src.Bounds()
	dst := image.NewRGBA(bounds)
	draw.Draw(dst, bounds, src, bounds.Min, draw.Src)

	if title == "" && subtitle == "" {
		// No overlay text at all — the user explicitly cleared both
		// override and the recording is missing metadata. Return the
		// raw image; rendered.jpg will be a faithful copy.
		return dst
	}

	bandH := max(int(float64(bounds.Dy())*style.BandHeightFraction), 1)
	bandRect := image.Rect(bounds.Min.X, bounds.Max.Y-bandH, bounds.Max.X, bounds.Max.Y)
	draw.Draw(dst, bandRect, &image.Uniform{C: style.BandColor}, image.Point{}, draw.Over)

	// Uppercase the title; that's the playbill aesthetic. Subtitle stays
	// as-is so " · " separators look natural.
	titleUpper := strings.ToUpper(title)
	subtitleUpper := strings.ToUpper(subtitle)

	// Resolve sizes + padding relative to the BAND when the user hasn't
	// pinned absolute pixel values. StageMedia posters arrive at ~230x345
	// and Encora screen-grabs at ~1280x720; a fixed 48 px title was sized
	// for the latter and overflowed posters by 3x. Fractional defaults
	// scale cleanly across both ends.
	resolved := resolveStyle(style, bandRect)
	maxTextWidth := bandRect.Dx() - hPadFactor*resolved.PadX
	if maxTextWidth < 1 {
		maxTextWidth = bandRect.Dx()
	}

	titleSpec := FontSpec{SizePx: resolved.TitleSizePx, Weight: style.Title.Weight}
	titleFace := pickFittingFace(titleUpper, titleSpec, maxTextWidth)
	subtitleSpec := FontSpec{SizePx: resolved.SubtitleSizePx, Weight: style.Subtitle.Weight}
	subtitleFace := pickFittingFace(subtitleUpper, subtitleSpec, maxTextWidth)

	// Layout: title centered on the upper third of the band, subtitle on
	// the lower third. When subtitle is empty, the title takes the
	// vertical center.
	drawCenteredText(dst, titleUpper, titleFace, style.TextColor, bandRect, subtitleUpper != "", true)
	if subtitleUpper != "" {
		drawCenteredText(dst, subtitleUpper, subtitleFace, style.TextColor, bandRect, true, false)
	}

	return dst
}

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
	titleSizeBandFraction    = 0.50
	subtitleSizeBandFraction = 0.28
	// 7% per side ≈ 14% total horizontal margin. The previous 4% (8%
	// total) read as edge-to-edge once the subtitle text was long
	// enough to use most of the width. The shrink-to-fit logic still
	// scales fonts down when content overflows, but the visual breathing
	// room around even a fitted line wants more than a thin sliver.
	padXImageFraction = 0.07
	minFontSizePx     = 8
	minPadXPx         = 6
)

// resolved bundles the post-fraction-resolution sizes the compose
// function works in.
type resolvedStyle struct {
	PadX           int
	TitleSizePx    float64
	SubtitleSizePx float64
}

// resolveStyle fills in font sizes + padding from band dimensions
// when the user-supplied (or default-baked) absolute values are
// zero. Caller-supplied positive values pass through verbatim so an
// explicit `size_px` override still wins.
func resolveStyle(style Style, bandRect image.Rectangle) resolvedStyle {
	out := resolvedStyle{
		PadX:           style.PadX,
		TitleSizePx:    style.Title.SizePx,
		SubtitleSizePx: style.Subtitle.SizePx,
	}
	bandH := bandRect.Dy()
	if out.TitleSizePx <= 0 {
		out.TitleSizePx = float64(bandH) * titleSizeBandFraction
	}
	if out.SubtitleSizePx <= 0 {
		out.SubtitleSizePx = float64(bandH) * subtitleSizeBandFraction
	}
	if out.PadX <= 0 {
		out.PadX = int(float64(bandRect.Dx()) * padXImageFraction)
	}
	if out.TitleSizePx < minFontSizePx {
		out.TitleSizePx = minFontSizePx
	}
	if out.SubtitleSizePx < minFontSizePx {
		out.SubtitleSizePx = minFontSizePx
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

// drawCenteredText renders s horizontally centered inside band, with
// the vertical placement chosen by the topHalf flag — true puts the
// baseline above the band's vertical center (title row), false puts it
// below (subtitle row). When hasSubtitle is false the title falls back
// to the band's vertical center.
func drawCenteredText(
	dst draw.Image,
	s string,
	face font.Face,
	col color.Color,
	band image.Rectangle,
	hasSubtitle bool,
	topHalf bool,
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

	var baselineY int
	switch {
	case !hasSubtitle:
		// Single-line: center the line vertically inside the band.
		bandCenter := band.Min.Y + band.Dy()/centerDivisor
		baselineY = bandCenter + (ascent-descent)/centerDivisor
	case topHalf:
		// Two-line, upper row: baseline at ~38 % down the band so the
		// title sits visually above center.
		baselineY = band.Min.Y + band.Dy()*twoLineTopNum/twoLineTopDen
	default:
		// Two-line, lower row: baseline at ~78 % down the band so the
		// subtitle clears the title comfortably.
		baselineY = band.Min.Y + band.Dy()*twoLineBotNum/twoLineBotDen
	}
	// Defensive guards so a shrunken band doesn't push the baseline off
	// the destination image.
	if baselineY-ascent < band.Min.Y {
		baselineY = band.Min.Y + ascent
	}
	if baselineY+descent > band.Max.Y {
		baselineY = band.Max.Y - descent
	}

	d.Dot = fixed.Point26_6{X: x, Y: fixed.I(baselineY)}
	d.DrawString(s)
}

// Layout fractions for the two-line band. Encoded as integer
// numerators/denominators so mnd lint stays happy.
const (
	twoLineTopNum = 38
	twoLineTopDen = 100
	twoLineBotNum = 78
	twoLineBotDen = 100
	// centerDivisor is the integer 2 used in (a+b)/2 averages and
	// width/2 centering math. Pulled out so mnd lint doesn't flag the
	// arithmetic.
	centerDivisor = 2
)
