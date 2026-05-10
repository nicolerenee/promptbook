// Package probe wraps an external ffprobe binary to extract media-info
// fields used by the rename template engine ({Container} /
// {VideoCodec} / {Quality}). The wrapper is deliberately small: a
// single Probe(ctx, path) function plus a MediaInfo result struct. The
// rename templates read these fields directly; nothing else in the
// pipeline depends on probe.
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
	"strings"
)

// MediaInfo carries the subset of ffprobe output the rename templates
// consume. Width/Height are unused outside of Quality(); they're
// surfaced anyway so future tokens (e.g. {Resolution}, {AspectRatio})
// can be added without re-shaping the struct.
type MediaInfo struct {
	VideoCodec string // ffprobe codec_name, e.g. "h264", "hevc".
	Width      int
	Height     int
	Container  string // uppercase extension, e.g. "MKV".
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

// Probe shells out to ffprobe and parses its JSON output for codec,
// width, and height of the first video stream. Container is derived
// from the file extension (uppercase, dot-stripped) — ffprobe's
// container detection is accurate but inconsistent across muxers, and
// the template's intent is to label the *file* not the *bitstream*.
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

	cmd := exec.CommandContext(ctx, binary,
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=codec_name,width,height",
		"-of", "json",
		path,
	)
	stdout, err := cmd.Output()
	if err != nil {
		return MediaInfo{}, decodeExecError(binary, err)
	}

	var raw struct {
		Streams []struct {
			CodecName string `json:"codec_name"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
		} `json:"streams"`
	}
	if jerr := json.Unmarshal(stdout, &raw); jerr != nil {
		return MediaInfo{}, fmt.Errorf("probe: parse ffprobe json for %s: %w", path, jerr)
	}

	info := MediaInfo{
		Container: strings.ToUpper(strings.TrimPrefix(filepath.Ext(path), ".")),
	}
	if len(raw.Streams) > 0 {
		info.VideoCodec = raw.Streams[0].CodecName
		info.Width = raw.Streams[0].Width
		info.Height = raw.Streams[0].Height
	}
	return info, nil
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
