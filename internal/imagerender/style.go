package imagerender

import (
	"encoding/json"
	"fmt"
	"image/color"
	"strconv"
	"strings"
)

// FontSpec is the per-text-row font configuration the renderer uses.
// SizePx is the pixel height of an em-square. Weight is informational
// today (the embedded font ships in a single weight) and reserved for
// when we ship a regular + bold pair.
type FontSpec struct {
	SizePx float64 `json:"size_px"`
	Weight string  `json:"weight"`
}

// Style controls the bottom-band composite layout. Defaults live in
// DefaultStyle; per-recording overrides are merged on top via
// mergeStyle from a JSON blob stored in
// recording_image_choices.overlay_style_json.
type Style struct {
	// BandHeightFraction is the band's height as a fraction of the
	// source image height. 0.14 means 14 %.
	BandHeightFraction float64 `json:"band_height_fraction"`
	// BandColor fills the bottom band. Premultiplied-alpha aware: the
	// renderer composites with draw.Over so a slightly translucent band
	// lets the underlying art breathe.
	BandColor color.Color `json:"band_color"`
	// TextColor is the foreground color used for both title and
	// subtitle. The placeholder default is a muted gold (#c8a14a) — the
	// Playbill yellow is too bright; the user will dial in the final
	// color later.
	TextColor color.Color `json:"text_color"`
	// PadX is the horizontal padding inside the band, in source-image
	// pixels. The title shrinks to fit before this padding is breached.
	PadX int `json:"pad_x"`
	// Title and Subtitle carry the per-row font configuration.
	Title    FontSpec `json:"title"`
	Subtitle FontSpec `json:"subtitle"`
}

// Default style values. Pulled out as named consts so the mnd lint
// stays satisfied + future tweaks have one location to land.
//
// PadX / TitleSizePx / SubtitleSizePx default to ZERO so the renderer
// derives them as fractions of the resolved band (see resolveStyle in
// compose.go). Per-recording overrides via overlay_style_json with a
// positive integer pin an absolute pixel size for users who want to
// tune the look manually.
const (
	defaultBandHeightFraction = 0.14
	defaultPadX               = 0
	defaultTitleSizePx        = 0
	defaultSubtitleSizePx     = 0

	// Brand palette. Band is the muted playbill blue (#4577A0); text is
	// deep navy (#13284A). Solid alpha so the band reads as a real plate
	// against any underlying art. Matches the wordmark + app-mark accent
	// so the burned-in poster identifies as part of the same product
	// family.
	defaultBandR uint8 = 0x45
	defaultBandG uint8 = 0x77
	defaultBandB uint8 = 0xA0
	defaultBandA uint8 = 0xFF

	defaultTextR uint8 = 0x13
	defaultTextG uint8 = 0x28
	defaultTextB uint8 = 0x4A
	defaultTextA uint8 = 0xFF
)

// DefaultStyle is the renderer's baked-in default. Callers merge a
// per-recording JSON blob over this — fields the blob doesn't mention
// keep their default value.
//
//nolint:gochecknoglobals // baked-in default style; treated as a const-equivalent.
var DefaultStyle = Style{
	BandHeightFraction: defaultBandHeightFraction,
	BandColor:          color.NRGBA{R: defaultBandR, G: defaultBandG, B: defaultBandB, A: defaultBandA},
	TextColor:          color.NRGBA{R: defaultTextR, G: defaultTextG, B: defaultTextB, A: defaultTextA},
	PadX:               defaultPadX,
	Title:              FontSpec{SizePx: defaultTitleSizePx, Weight: "bold"},
	Subtitle:           FontSpec{SizePx: defaultSubtitleSizePx, Weight: "regular"},
}

// styleOverlay is the wire shape mergeStyle accepts. Every field is
// optional — missing keys fall through to DefaultStyle. Colors arrive
// as hex strings ("#aabbcc" or "#aabbccdd" with alpha) so the user can
// type a value into a future text field without learning Go's color
// package.
type styleOverlay struct {
	BandHeightFraction *float64  `json:"band_height_fraction,omitempty"`
	BandColor          *string   `json:"band_color,omitempty"`
	TextColor          *string   `json:"text_color,omitempty"`
	PadX               *int      `json:"pad_x,omitempty"`
	Title              *FontSpec `json:"title,omitempty"`
	Subtitle           *FontSpec `json:"subtitle,omitempty"`
}

// mergeStyle decodes overrideJSON and returns a Style with the named
// fields overlaid on base. Returns the original error when the JSON
// itself is malformed; per-field decode errors (a bad color string)
// fall through with the base value preserved so a single typo can't
// destroy the whole render.
func mergeStyle(base Style, overrideJSON string) (Style, error) {
	var o styleOverlay
	if err := json.Unmarshal([]byte(overrideJSON), &o); err != nil {
		return base, fmt.Errorf("parse overlay style: %w", err)
	}
	out := base
	if o.BandHeightFraction != nil {
		out.BandHeightFraction = *o.BandHeightFraction
	}
	if o.PadX != nil {
		out.PadX = *o.PadX
	}
	if o.Title != nil {
		if o.Title.SizePx > 0 {
			out.Title.SizePx = o.Title.SizePx
		}
		if o.Title.Weight != "" {
			out.Title.Weight = o.Title.Weight
		}
	}
	if o.Subtitle != nil {
		if o.Subtitle.SizePx > 0 {
			out.Subtitle.SizePx = o.Subtitle.SizePx
		}
		if o.Subtitle.Weight != "" {
			out.Subtitle.Weight = o.Subtitle.Weight
		}
	}
	if o.BandColor != nil {
		if c, ok := parseHexColor(*o.BandColor); ok {
			out.BandColor = c
		}
	}
	if o.TextColor != nil {
		if c, ok := parseHexColor(*o.TextColor); ok {
			out.TextColor = c
		}
	}
	return out, nil
}

// parseHexColor accepts "#rgb", "#rrggbb", or "#rrggbbaa" and returns a
// color.NRGBA. The leading "#" is optional. Invalid input yields ok=
// false; callers fall back to whatever they had before.
func parseHexColor(s string) (color.NRGBA, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "#")
	switch len(s) {
	case hexLenShort:
		// Expand "rgb" → "rrggbb" so a single decode path handles both.
		s = string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]})
		fallthrough
	case hexLenRGB:
		v, err := strconv.ParseUint(s, 16, 32)
		if err != nil {
			return color.NRGBA{}, false
		}
		return color.NRGBA{
			R: uint8((v >> bitsPerByteX2) & byteMask),
			G: uint8((v >> bitsPerByte) & byteMask),
			B: uint8(v & byteMask),
			A: alphaOpaque,
		}, true
	case hexLenRGBA:
		v, err := strconv.ParseUint(s, 16, 64)
		if err != nil {
			return color.NRGBA{}, false
		}
		return color.NRGBA{
			R: uint8((v >> bitsPerByteX3) & byteMask),
			G: uint8((v >> bitsPerByteX2) & byteMask),
			B: uint8((v >> bitsPerByte) & byteMask),
			A: uint8(v & byteMask),
		}, true
	default:
		return color.NRGBA{}, false
	}
}

const (
	hexLenShort   = 3
	hexLenRGB     = 6
	hexLenRGBA    = 8
	bitsPerByte   = 8
	bitsPerByteX2 = 16
	bitsPerByteX3 = 24
	byteMask      = 0xFF
	alphaOpaque   = 0xFF
)
