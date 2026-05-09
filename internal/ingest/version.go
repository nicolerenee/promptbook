package ingest

import (
	"fmt"
	"strings"
)

// bytesPerGB is the divisor used to render Encora-style "X.YY GB" sizes.
// We deliberately use the binary 1024^3 rather than the SI 1000^3 because
// every existing Encora format string in our fixtures matches binary GB
// (e.g. an 8.74 GB MKV is 9_384_503_541 bytes / 1024^3 = 8.74).
const bytesPerGB = 1024 * 1024 * 1024

// codecLabelHEVC and codecLabelH264 are the canonical labels we emit for
// the two video codecs Encora masters most often. Defined as constants
// to satisfy goconst.
const (
	codecLabelHEVC = "HEVC"
	codecLabelH264 = "h.264"
)

// maxFormatSpecParts is the upper bound for the slice that holds the
// "(quality - codec)" portion of the format label. Defined to keep mnd
// happy without inflating the make's first read.
const maxFormatSpecParts = 2

// qualityTokens is the case-insensitive substring set used by ParseQuality
// to recognize a resolution token in a filename. Order matters: 2160p must
// be checked before "4k" so that filenames carrying both still surface the
// canonical "2160p" label.
//
//nolint:gochecknoglobals // immutable lookup table
var qualityTokens = []string{"2160p", "4k", "1080p", "720p", "480p", "sd"}

// videoCodecAliases maps every alias we want to recognize back to the
// canonical codec label used in format strings. Encora's own format
// strings render h264 as "h.264" — we follow suit.
//
//nolint:gochecknoglobals // immutable lookup table
var videoCodecAliases = []struct {
	needle string
	label  string
}{
	{"h265", codecLabelHEVC},
	{"hevc", codecLabelHEVC},
	{"x265", codecLabelHEVC},
	{"h264", codecLabelH264},
	{"x264", codecLabelH264},
	{"avc", codecLabelH264},
	{"av1", "AV1"},
}

// audioCodecAliases mirrors videoCodecAliases for the audio side.
//
//nolint:gochecknoglobals // immutable lookup table
var audioCodecAliases = []struct {
	needle string
	label  string
}{
	{"atmos", "Atmos"},
	{"truehd", "TrueHD"},
	{"dts", "DTS"},
	{"eac3", "EAC3"},
	{"ac3", "AC3"},
	{"aac", "AAC"},
}

// DefaultFormatLabel renders an Encora-style format label for a freshly
// ingested file. Mirrors the shape of strings already in our collection
// fixtures (e.g. "MKV (1080p - h.264) - 8.74 GB"). Empty fields are
// dropped gracefully so we never emit "() - " or trailing dashes.
func DefaultFormatLabel(container, quality, videoCodec string, sizeBytes int64) string {
	containerUpper := strings.ToUpper(container)
	size := fmt.Sprintf("%.2f GB", float64(sizeBytes)/float64(bytesPerGB))

	specParts := make([]string, 0, maxFormatSpecParts)
	if quality != "" {
		specParts = append(specParts, quality)
	}
	if videoCodec != "" {
		specParts = append(specParts, videoCodec)
	}

	switch {
	case containerUpper == "" && len(specParts) == 0:
		return size
	case len(specParts) == 0:
		return fmt.Sprintf("%s - %s", containerUpper, size)
	case containerUpper == "":
		return fmt.Sprintf("(%s) - %s", strings.Join(specParts, " - "), size)
	default:
		return fmt.Sprintf("%s (%s) - %s", containerUpper, strings.Join(specParts, " - "), size)
	}
}

// ParseQuality scans name (case-insensitive) for the first recognized
// resolution token. "2160p" wins over "4k" when both appear so the result
// is consistent across UHD release naming conventions. Returns "" when
// nothing matches.
func ParseQuality(name string) string {
	lower := strings.ToLower(name)
	for _, token := range qualityTokens {
		if strings.Contains(lower, token) {
			return token
		}
	}
	return ""
}

// ParseVideoCodec scans name (case-insensitive) for the first recognized
// video codec alias and returns its canonical label. Returns "" when
// nothing matches.
func ParseVideoCodec(name string) string {
	return matchAlias(name, videoCodecAliases)
}

// ParseAudioCodec scans name (case-insensitive) for the first recognized
// audio codec alias and returns its canonical label. Returns "" when
// nothing matches.
func ParseAudioCodec(name string) string {
	return matchAlias(name, audioCodecAliases)
}

// matchAlias walks aliases in declared order and returns the canonical
// label for the first needle found in name (case-insensitive). The
// declared order is significant: more-specific tokens (e.g. "eac3") must
// come before substrings of themselves (e.g. "ac3").
func matchAlias(name string, aliases []struct {
	needle string
	label  string
},
) string {
	lower := strings.ToLower(name)
	for _, a := range aliases {
		if strings.Contains(lower, a.needle) {
			return a.label
		}
	}
	return ""
}
