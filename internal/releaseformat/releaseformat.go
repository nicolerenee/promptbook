// Package releaseformat builds the canonical "what files do you have"
// release-format string from a recording's local versions + their
// MediaInfo blobs. Replaces the legacy free-text encora.release_format
// field as the display surface on the recording detail + list pages.
//
// Format:
//
//	{Container} - {VideoCodec} / {AudioCodec} - {Quality} - {Size}
//
// Single-version recordings render bare; multi-version recordings wrap
// each version in [...] and join with a single space, ordered by
// height descending (best first):
//
//	MP4 - x265 / AAC - 2160p - 8.57 GB
//	[MP4 - x265 / AAC - 2160p - 8.57 GB] [MP4 - x264 / AAC - 1080p - 4.20 GB]
//
// Missing fields render as "?" so each slot stays visible — the
// structure is what makes the string readable at a glance.
package releaseformat

import (
	"fmt"
	"sort"
	"strings"

	"github.com/nicolerenee/promptbook/internal/probe"
)

// missingPlaceholder is the literal rendered for any missing
// VideoCodec / AudioCodec / Quality slot. Container is intentionally
// excluded — the file extension is the canonical fallback there.
const missingPlaceholder = "?"

// IEC binary divisors for size rendering. Mirrors the Mithril SPA's
// utils/format.js humanSize so server-rendered + client-rendered
// strings agree on byte-thresholds.
const (
	bytesKiB = 1024
	bytesMiB = bytesKiB * 1024
	bytesGiB = bytesMiB * 1024
	bytesTiB = bytesGiB * 1024
)

// VersionInfo is the per-version input to Compose. One entry per
// recording_versions row; FromVersionAndMediaInfo is the canonical
// constructor for callers that already hold a probe.MediaInfo + a
// version row size.
type VersionInfo struct {
	// Container is the uppercase extension ("MP4", "MKV"). Empty
	// container collapses the leading slot to "?".
	Container string
	// VideoCodec is the raw ffprobe codec_name ("h264", "hevc"). The
	// renderer maps it through formatVideoCodec to the Sonarr-style
	// label (h264 → x264, hevc → x265). Empty renders as "?".
	VideoCodec string
	// AudioCodec is the raw ffprobe audio codec_name. The renderer
	// uppercases it ("aac" → "AAC"). Empty renders as "?".
	AudioCodec string
	// Height is the encoded video height in pixels. Used both for the
	// Quality slot ("1080p" / "2160p" / etc.) and as the multi-version
	// sort key (descending). Zero / negative renders Quality as "?"
	// and sorts last.
	Height int
	// SizeBytes is the file size on disk. Rendered through formatSize
	// to match the SPA's humanSize convention.
	SizeBytes int64
}

// FromVersionAndMediaInfo is the canonical constructor used by every
// caller. Pulls Container / VideoCodec / first-AudioCodec / Height off
// the MediaInfo blob and combines them with the version row's size.
//
// Empty MediaInfo (legacy imports that pre-date the probe capture) is
// handled cleanly: Container falls back to fallbackExt (the file
// extension, uppercased), and the rest stay empty so the renderer
// emits "{ext} - ? / ? - ? - {size}".
func FromVersionAndMediaInfo(
	info probe.MediaInfo, sizeBytes int64, fallbackExt string,
) VersionInfo {
	v := VersionInfo{
		Container:  info.Container,
		VideoCodec: info.VideoCodec,
		Height:     info.Height,
		SizeBytes:  sizeBytes,
	}
	if v.Container == "" {
		// Legacy fallback: strip leading dot, uppercase. Matches the
		// rename path's container convention (probe.Probe does this
		// too on the live capture path).
		v.Container = strings.ToUpper(strings.TrimPrefix(fallbackExt, "."))
	}
	if len(info.AudioStreams) > 0 {
		v.AudioCodec = info.AudioStreams[0].Codec
	}
	return v
}

// Compose returns the release-format string for the supplied versions.
//
//   - Empty input → "" (no files registered).
//   - Single version → bare format, no brackets.
//   - Multi-version → each version wrapped in "[...]", joined by a
//     single space, ordered by Height descending (best first). Ties
//     and zero-height entries fall to the back in input order.
func Compose(versions []VersionInfo) string {
	if len(versions) == 0 {
		return ""
	}
	if len(versions) == 1 {
		return renderVersion(versions[0])
	}
	sorted := make([]VersionInfo, len(versions))
	copy(sorted, versions)
	sort.SliceStable(sorted, func(i, j int) bool {
		// Descending by Height. Stable sort preserves input order on
		// ties so two 1080p versions render in the order the caller
		// supplied (matches ListVersions' size-desc tiebreak).
		return sorted[i].Height > sorted[j].Height
	})
	parts := make([]string, 0, len(sorted))
	for _, v := range sorted {
		parts = append(parts, "["+renderVersion(v)+"]")
	}
	return strings.Join(parts, " ")
}

// renderVersion emits the bare "{Container} - {VideoCodec} /
// {AudioCodec} - {Quality} - {Size}" string for a single version.
// Empty slots render as "?" rather than collapsing the structure.
func renderVersion(v VersionInfo) string {
	container := v.Container
	if container == "" {
		container = missingPlaceholder
	}
	videoCodec := formatVideoCodec(v.VideoCodec)
	if videoCodec == "" {
		videoCodec = missingPlaceholder
	}
	audioCodec := strings.ToUpper(v.AudioCodec)
	if audioCodec == "" {
		audioCodec = missingPlaceholder
	}
	quality := qualityFromHeight(v.Height)
	if quality == "" {
		quality = missingPlaceholder
	}
	return fmt.Sprintf("%s - %s / %s - %s - %s",
		container, videoCodec, audioCodec, quality, formatSize(v.SizeBytes))
}

// formatVideoCodec maps the raw ffprobe codec_name onto the
// Sonarr-style display label. h264 → x264, hevc → x265, anything else
// passes through unchanged. Empty input stays empty so the caller can
// distinguish "no value" from "unrecognized value".
func formatVideoCodec(codec string) string {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "":
		return ""
	case "h264", "avc":
		return "x264"
	case "hevc", "h265":
		return "x265"
	default:
		return codec
	}
}

// Quality bucket thresholds. Mirrors probe.MediaInfo.Quality so the
// release-format string and the rename {Quality} token button heights
// to the same labels.
const (
	qualityHeight2160 = 2000
	qualityHeight1440 = 1300
	qualityHeight1080 = 1000
	qualityHeight720  = 700
	qualityHeight480  = 450
	qualityHeight360  = 300
)

// qualityFromHeight maps an encoded video height onto a resolution
// label. Same thresholds as probe.MediaInfo.Quality so the two
// surfaces (rename token + release format) agree on what a given
// height is called. Returns "" for non-positive heights so the
// renderer can collapse to "?".
func qualityFromHeight(height int) string {
	switch {
	case height <= 0:
		return ""
	case height >= qualityHeight2160:
		return "2160p"
	case height >= qualityHeight1440:
		return "1440p"
	case height >= qualityHeight1080:
		return "1080p"
	case height >= qualityHeight720:
		return "720p"
	case height >= qualityHeight480:
		return "480p"
	case height >= qualityHeight360:
		return "360p"
	default:
		return "240p"
	}
}

// formatSize renders a byte count as the human-readable IEC binary
// string the SPA uses ("8.57 GB", "512 MB", "1024 B"). Mirrors
// internal/web/static/app/utils/format.js humanSize so the server's
// release-format display agrees with anywhere else the SPA shows a
// size.
func formatSize(b int64) string {
	switch {
	case b >= bytesTiB:
		return fmt.Sprintf("%.2f TB", float64(b)/float64(bytesTiB))
	case b >= bytesGiB:
		return fmt.Sprintf("%.2f GB", float64(b)/float64(bytesGiB))
	case b >= bytesMiB:
		return fmt.Sprintf("%.2f MB", float64(b)/float64(bytesMiB))
	case b >= bytesKiB:
		return fmt.Sprintf("%.2f KB", float64(b)/float64(bytesKiB))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
