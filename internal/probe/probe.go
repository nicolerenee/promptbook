// Package probe wraps an external ffprobe binary to extract media-info
// fields used by the rename template engine ({Container} /
// {VideoCodec} / {Quality}) and the recording detail page's media-info
// card. The wrapper is deliberately small: a single Probe(ctx, path)
// function plus a MediaInfo result struct.
//
// ffprobe is a hard runtime requirement — the engine errors loudly
// rather than rendering an empty mediainfo when the binary is missing.
// Default lookup is `ffprobe` on PATH; callers can override the binary
// path via the FFProbe.Path field.
package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// MediaInfo carries the ffprobe output the rename templates and the
// recording detail page consume. The first four fields
// (Container / VideoCodec / Width / Height) are the rename-token
// surface and stay backwards-compatible; the rest power the
// Sonarr/Radarr-style media-info card on the recording detail page.
//
// JSON tags use lowerCamelCase to match the GraphQL surface. The
// blob is persisted on recording_versions.media_info_json at ingest
// time and decoded by the GraphQL resolver at read time; no other
// callers touch the wire shape.
type MediaInfo struct {
	Container       string               `json:"container"`
	VideoCodec      string               `json:"videoCodec"`
	Width           int                  `json:"width"`
	Height          int                  `json:"height"`
	VideoBitDepth   int                  `json:"videoBitDepth"`
	VideoFps        float64              `json:"videoFps"`
	DurationSeconds float64              `json:"durationSeconds"`
	ScanType        string               `json:"scanType"`
	AudioStreams    []AudioStreamInfo    `json:"audioStreams"`
	SubtitleStreams []SubtitleStreamInfo `json:"subtitleStreams"`
}

// AudioStreamInfo is one entry per ffprobe audio stream. ChannelLayout
// falls back to the channel-count string when the stream did not carry
// an explicit layout tag (e.g. some legacy AC-3 muxes).
type AudioStreamInfo struct {
	Codec         string `json:"codec"`
	ChannelLayout string `json:"channelLayout"`
	Bitrate       int    `json:"bitrate"`
	Language      string `json:"language"`
}

// SubtitleStreamInfo is one entry per ffprobe subtitle stream.
type SubtitleStreamInfo struct {
	Codec    string `json:"codec"`
	Language string `json:"language"`
}

// Quality returns the height-mapped resolution label used by the
// {Quality} rename token. Returns "" for unknown / non-positive
// heights, which the optional-segment grammar in the template engine
// then collapses cleanly.
//
// Mapping is height-based to keep cropped / pillarboxed sources
// honest: a 1920x800 cinema crop is "1080p" because its frame is 1080
// tall in the encoded stream. Non-standard heights button to the nearest
// recognized bucket (so 1088 → 1080p, 1072 → 1080p, etc.).
func (m MediaInfo) Quality() string {
	switch {
	case m.Height <= 0:
		return ""
	case m.Height >= qualityHeight2160:
		return "2160p"
	case m.Height >= qualityHeight1440:
		return "1440p"
	case m.Height >= qualityHeight1080:
		return "1080p"
	case m.Height >= qualityHeight720:
		return "720p"
	case m.Height >= qualityHeight480:
		return "480p"
	case m.Height >= qualityHeight360:
		return "360p"
	default:
		return "240p"
	}
}

// Quality bucket thresholds. Each value is the minimum height that
// buttons to the named label — below 240 falls into a generic "240p"
// bucket so the token never renders blank just because the source is
// unusually small.
const (
	qualityHeight2160 = 2000
	qualityHeight1440 = 1300
	qualityHeight1080 = 1000
	qualityHeight720  = 700
	qualityHeight480  = 450
	qualityHeight360  = 300
)

// defaultBitDepth is what ParsePixFmtBitDepth returns when the
// pix_fmt is empty, on the known-8-bit list, or in any unrecognized
// shape. Promptbook's catalog is dominated by 8-bit h264 sources so a
// wrong guess here is purely cosmetic on the detail-page card.
const defaultBitDepth = 8

// frameRateParts is the expected number of "/" -separated tokens in
// ffprobe's r_frame_rate field — "24000/1001". Anything else falls
// back to bare-integer parsing.
const frameRateParts = 2

// FFProbe is the production prober. The zero value uses `ffprobe` on
// PATH; set Path to point at a specific binary. Concurrency-safe — each
// call shells out a fresh process.
type FFProbe struct {
	// Path is the ffprobe binary path. Empty means "ffprobe" via PATH.
	Path string
}

// Prober is the interface ingest + the GraphQL preview resolver depend
// on. Lets tests inject a stub that returns a fixed MediaInfo without
// running the real binary.
type Prober interface {
	Probe(ctx context.Context, path string) (MediaInfo, error)
}

// rawFFProbeOutput is the ffprobe JSON shape we care about. Pulled out
// of the function body so tests can drive the parser directly with a
// fixture without reshelling ffprobe.
type rawFFProbeOutput struct {
	Streams []rawFFProbeStream `json:"streams"`
	Format  rawFFProbeFormat   `json:"format"`
}

type rawFFProbeStream struct {
	Index         int    `json:"index"`
	CodecType     string `json:"codec_type"`
	CodecName     string `json:"codec_name"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	PixFmt        string `json:"pix_fmt"`
	RFrameRate    string `json:"r_frame_rate"`
	FieldOrder    string `json:"field_order"`
	BitRate       string `json:"bit_rate"`
	Channels      int    `json:"channels"`
	ChannelLayout string `json:"channel_layout"`
	Tags          struct {
		Language string `json:"language"`
	} `json:"tags"`
}

type rawFFProbeFormat struct {
	Duration string `json:"duration"`
}

// Probe shells out to ffprobe and parses every stream + format
// duration so the recording detail page can render a Sonarr-style
// media-info card. Container is derived from the file extension
// (uppercase, dot-stripped) — ffprobe's container detection is
// accurate but inconsistent across muxers, and the template's intent
// is to label the *file* not the *bitstream*.
//
// Errors:
//   - exec.ErrNotFound (or wrapped equivalent) when the binary is
//     missing on PATH; the wrapper preserves the wrapping so callers
//     can errors.Is against exec.ErrNotFound.
//   - non-zero exit from ffprobe (e.g. file-not-found, unreadable file)
//     wrapped with the binary stderr for context.
//   - malformed JSON from ffprobe surfaces as a json.SyntaxError.
func (p FFProbe) Probe(ctx context.Context, path string) (MediaInfo, error) {
	if path == "" {
		return MediaInfo{}, errors.New("probe: path is empty")
	}
	binary := p.Path
	if binary == "" {
		binary = "ffprobe"
	}

	// Show every stream (no -select_streams) plus the format duration.
	// The entries list keeps the JSON small — we only ever read these
	// keys, so requesting the full default dump would just bloat the
	// pipe.
	entries := "format=duration:" +
		"stream=index,codec_type,codec_name,width,height,pix_fmt," +
		"r_frame_rate,field_order,bit_rate,channels,channel_layout,tags"
	cmd := exec.CommandContext(ctx, binary,
		"-v", "error",
		"-show_entries", entries,
		"-of", "json",
		path,
	)
	stdout, err := cmd.Output()
	if err != nil {
		return MediaInfo{}, decodeExecError(binary, err)
	}

	info, err := parseFFProbeOutput(stdout)
	if err != nil {
		return MediaInfo{}, fmt.Errorf("probe: parse ffprobe json for %s: %w", path, err)
	}
	info.Container = strings.ToUpper(strings.TrimPrefix(filepath.Ext(path), "."))
	return info, nil
}

// parseFFProbeOutput converts the ffprobe JSON blob into a populated
// MediaInfo. Container is left empty because that field is derived
// from the file path, not the JSON; the public Probe wrapper fills it
// after this returns.
func parseFFProbeOutput(jsonBytes []byte) (MediaInfo, error) {
	var raw rawFFProbeOutput
	if err := json.Unmarshal(jsonBytes, &raw); err != nil {
		return MediaInfo{}, err
	}
	info := MediaInfo{}
	if raw.Format.Duration != "" {
		// ffprobe writes durations as decimal seconds with up to six
		// fractional digits. Errors are non-fatal — a missing duration
		// surfaces as 0 to the UI.
		if d, parseErr := strconv.ParseFloat(raw.Format.Duration, 64); parseErr == nil {
			info.DurationSeconds = d
		}
	}
	for _, s := range raw.Streams {
		switch s.CodecType {
		case "video":
			fillVideoStream(&info, s)
		case "audio":
			info.AudioStreams = append(info.AudioStreams, audioStreamFromRaw(s))
		case "subtitle":
			info.SubtitleStreams = append(info.SubtitleStreams, SubtitleStreamInfo{
				Codec:    s.CodecName,
				Language: s.Tags.Language,
			})
		}
	}
	return info, nil
}

// fillVideoStream populates the video-side fields on info from the
// first ffprobe video stream encountered. Subsequent video streams
// (extremely rare in promptbook's catalog — only weird grabbed-from-
// disc rips would carry one) are ignored so the UI fields stay simple.
func fillVideoStream(info *MediaInfo, s rawFFProbeStream) {
	// Only the first video stream wins; multi-video files are not in
	// scope for the rename templates or the detail-page card.
	if info.VideoCodec != "" || info.Width != 0 || info.Height != 0 {
		return
	}
	info.VideoCodec = s.CodecName
	info.Width = s.Width
	info.Height = s.Height
	info.VideoBitDepth = ParsePixFmtBitDepth(s.PixFmt)
	info.VideoFps = parseFrameRate(s.RFrameRate)
	info.ScanType = s.FieldOrder
}

// audioStreamFromRaw converts an ffprobe audio stream into the
// public AudioStreamInfo shape. Bitrate is parsed from the string
// form ffprobe writes ("640000"); empty / unparseable values land as 0
// so the UI can hide the row. ChannelLayout falls back to a
// "{n}-channel" string when the stream did not carry an explicit
// layout — matches Sonarr's behaviour for legacy AC-3 muxes that omit
// the layout tag.
func audioStreamFromRaw(s rawFFProbeStream) AudioStreamInfo {
	out := AudioStreamInfo{
		Codec:         s.CodecName,
		ChannelLayout: s.ChannelLayout,
		Language:      s.Tags.Language,
	}
	if out.ChannelLayout == "" && s.Channels > 0 {
		out.ChannelLayout = fmt.Sprintf("%d-channel", s.Channels)
	}
	if s.BitRate != "" {
		if br, err := strconv.Atoi(s.BitRate); err == nil {
			out.Bitrate = br
		}
	}
	return out
}

// pixFmtBitDepthRE captures the trailing depth digits before the
// endianness suffix — yuv420p10le → "10", rgb48be → "48"-no-match
// (rgb stride is the channel width, not bit-depth-per-channel; we let
// the unknown-pix-fmt branch in ParsePixFmtBitDepth handle those).
//
// The regex deliberately requires at least two digits before le/be so
// "yuv420p" (no digit suffix) doesn't accidentally match.
var pixFmtBitDepthRE = regexp.MustCompile(`p(\d{2,})(le|be)$`)

// pixFmtKnown8Bit lists the common 8-bit pixel formats whose names do
// not carry an explicit bit-depth suffix. Anything not in here and not
// matching the regex above falls through to the 8-bit default — that's
// the right answer for the vast majority of consumer-grade files.
//
//nolint:gochecknoglobals // immutable lookup table.
var pixFmtKnown8Bit = map[string]struct{}{
	"yuv420p":  {},
	"yuv422p":  {},
	"yuv444p":  {},
	"yuvj420p": {},
	"yuvj422p": {},
	"yuvj444p": {},
	"yuv411p":  {},
	"yuv410p":  {},
	"nv12":     {},
	"nv21":     {},
	"rgb24":    {},
	"bgr24":    {},
	"gbrp":     {},
	"gray":     {},
	"gray8":    {},
	"yuva420p": {},
	"yuva422p": {},
	"yuva444p": {},
	"yuyv422":  {},
	"uyvy422":  {},
}

// ParsePixFmtBitDepth maps an ffprobe pix_fmt name onto a bit-depth
// integer. yuv420p → 8, yuv420p10le → 10, yuv444p12le → 12, unknown or
// empty → 8 (the safe default — promptbook's catalog is dominated by
// 8-bit h264 sources). Exposed for testability; callers should
// generally rely on Probe filling MediaInfo.VideoBitDepth directly.
func ParsePixFmtBitDepth(pixFmt string) int {
	if pixFmt == "" {
		return defaultBitDepth
	}
	if _, known := pixFmtKnown8Bit[pixFmt]; known {
		return defaultBitDepth
	}
	if m := pixFmtBitDepthRE.FindStringSubmatch(pixFmt); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return n
		}
	}
	// Fallback: anything we don't recognize is treated as 8-bit. The
	// detail-page card displays an integer either way, so a wrong guess
	// here is purely cosmetic.
	return defaultBitDepth
}

// parseFrameRate parses ffprobe's r_frame_rate field. ffprobe writes
// frame rates as exact rationals ("24000/1001" → 23.976). A missing
// or malformed value returns 0 so the UI can hide the row.
func parseFrameRate(rate string) float64 {
	if rate == "" {
		return 0
	}
	parts := strings.SplitN(rate, "/", frameRateParts)
	if len(parts) != frameRateParts {
		// Some streams report a bare integer; honor it.
		if v, err := strconv.ParseFloat(rate, 64); err == nil {
			return v
		}
		return 0
	}
	num, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0
	}
	den, err := strconv.ParseFloat(parts[1], 64)
	if err != nil || den == 0 {
		return 0
	}
	return num / den
}

// decodeExecError maps an os/exec failure into a more legible error.
// The most common case in practice is ffprobe missing on PATH (returns
// *exec.Error wrapping exec.ErrNotFound) or ffprobe rejecting a
// malformed input (returns *exec.ExitError with stderr in .Stderr).
func decodeExecError(binary string, err error) error {
	var execErr *exec.Error
	if errors.As(err, &execErr) {
		return fmt.Errorf("probe: %s not available: %w", binary, err)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		stderr := strings.TrimSpace(string(exitErr.Stderr))
		if stderr != "" {
			return fmt.Errorf("probe: %s failed: %s: %w", binary, stderr, err)
		}
		return fmt.Errorf("probe: %s exited %d: %w", binary, exitErr.ExitCode(), err)
	}
	return fmt.Errorf("probe: run %s: %w", binary, err)
}
