// Package makemkv wraps the `makemkvcon` CLI for promptbook's DVD
// remux feature. Two operations:
//
//   - Info: fast read-only scan that lists every title on the disc
//     image (duration, size, chapters, video / audio / subtitle
//     streams). The SPA renders the result so the user picks which
//     titles to convert.
//
//   - Mkv: the actual conversion. Streams pass through losslessly
//     (--decrypt would be DVD-CSS handling; promptbook only targets
//     unencrypted home-rip DVDs so this isn't needed). The output
//     MKV preserves the original MPEG-2 video + AC3 audio bit-for-
//     bit; no transcoding.
//
// makemkvcon's wire format is the line-based "robot" mode triggered
// by `--robot` — every line is `TAG:csv`. Parsing is a hand-rolled
// CSV split per line because each tag uses a different schema and
// pulling in encoding/csv for this is overkill.
package makemkv

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// DefaultBinary is the executable name when caller doesn't override
// via config. Resolved through PATH.
const DefaultBinary = "makemkvcon"

// makemkvcon's robot output is a tagged + numbered protocol; the
// numeric codes come straight from the AP_AvFs.h / AP_ConnectAp.h
// headers in the makemkv sources. Pulled out as named constants so
// the parser reads as "title duration" rather than "case 9". Codes
// are stable across makemkv versions per the upstream API contract.
const (
	tinfoName           = 2  // Title display name.
	tinfoChapterCount   = 8  // Chapter count.
	tinfoDuration       = 9  // Duration "H:MM:SS".
	tinfoSizeBytes      = 11 // Disk size in bytes.
	tinfoSourceFilename = 16 // Source .VOB filename for title.

	sinfoStreamType        = 1  // "Video" / "Audio" / "Subtitles".
	sinfoLanguageCode      = 3  // ISO 639-2 language code.
	sinfoCodecID           = 6  // V_MPEG2 / A_AC3 / S_VOBSUB etc.
	sinfoChannelLayout     = 19 // Audio channel layout ("5.1 ch").
	sinfoVideoSampleFormat = 20 // Video resolution / audio rate.
)

// splitRobotCSV's expected field counts for each tag. TINFO body
// has 4 fields (title, code, flags, value); SINFO has 5 (title,
// stream, code, flags, value).
const (
	tinfoFieldCount = 4
	sinfoFieldCount = 5
)

// scannerBufferCap is the bufio.Scanner ceiling. makemkvcon's
// info lines stay small but a noisy progress message can be long;
// 1 MiB is comfortably above any observed line length.
const scannerBufferCap = 1024 * 1024

// scannerInitialBuffer is the starting buffer size for the scanner.
// 64 KiB matches Go's bufio.MaxScanTokenSize default and is plenty
// for typical robot lines.
const scannerInitialBuffer = 64 * 1024

// durationFields is the number of components in makemkvcon's
// H:MM:SS duration format.
const durationFields = 3

const (
	secondsPerHour   = 3600
	secondsPerMinute = 60
)

// Title is one entry in the disc's title set — typically a movie's
// main feature, a bonus piece, or a menu. The SPA renders the list
// so the user picks which to convert.
type Title struct {
	// Index is the title id makemkvcon uses on the command line
	// (0-based). Pass this back to Mkv() to convert this specific
	// title.
	Index int
	// Name is makemkvcon's auto-generated title name (typically
	// derived from the disc's volume label + a t00 / t01 suffix).
	// Empty when makemkvcon doesn't supply one.
	Name string
	// SourceFilename is the .vob filename the title's first chunk
	// maps to (e.g. "VTS_01_1.VOB"). Useful for the SPA's "which
	// title is the main feature" disambiguation.
	SourceFilename string
	// Duration is the title's runtime in seconds. Zero when
	// makemkvcon didn't supply a duration (shouldn't happen for a
	// healthy rip).
	Duration int
	// SizeBytes is the title's combined chunk size on disk.
	SizeBytes int64
	// Chapters is the chapter-mark count. 1 means "no chapters"
	// (the single-chapter default). Zero is the makemkvcon "not
	// reported" sentinel and reads the same in the UI.
	Chapters int
	// VideoCodec is the per-title video stream's codec token
	// ("V_MPEG2", "V_MPEG4/ISO/AVC", etc.). Lower-cased and
	// stripped of the V_ prefix on render.
	VideoCodec string
	// VideoResolution is "720x480" / "720x576" / etc.
	VideoResolution string
	// AudioTracks is a per-track audio summary (codec + language).
	AudioTracks []AudioTrack
	// SubtitleTracks lists the per-track subtitle languages.
	SubtitleTracks []SubtitleTrack
}

// AudioTrack is one audio stream of a title.
type AudioTrack struct {
	Index    int
	Codec    string // "A_AC3", "A_DTS", etc.
	Language string // ISO code from makemkvcon; "eng", "fra", etc.
	Channels string // "5.1 ch", "2.0 ch" — verbatim from makemkvcon.
}

// SubtitleTrack is one subtitle stream of a title.
type SubtitleTrack struct {
	Index    int
	Language string
}

// Client wraps a `makemkvcon` binary. Constructed with the binary
// path (or empty for "use DefaultBinary on PATH") and a Runner that
// tests can stub. Production wiring constructs one Client per server
// instance.
type Client struct {
	// Binary is the path to makemkvcon. Empty resolves to
	// DefaultBinary on PATH.
	Binary string
	// Runner produces an exec.Cmd. Defaults to exec.CommandContext
	// when nil; tests inject a fake.
	Runner Runner
}

// Runner is the exec.CommandContext seam. Lets tests stub the
// makemkvcon invocation without shelling out.
type Runner func(ctx context.Context, name string, args ...string) Cmd

// Cmd is the minimal subset of *exec.Cmd we use. Lets tests return
// a fake that streams a pre-canned robot transcript without
// actually invoking the binary.
type Cmd interface {
	// StdoutPipe matches *exec.Cmd.StdoutPipe.
	StdoutPipe() (io.ReadCloser, error)
	// Start matches *exec.Cmd.Start.
	Start() error
	// Wait matches *exec.Cmd.Wait. Returns the exit error.
	Wait() error
}

// Info runs `makemkvcon --robot info file:{folder}` and parses the
// robot output into a list of titles. The folder should be the
// PARENT of VIDEO_TS (makemkvcon reads the IFO files inside to
// discover titles). Returns a non-nil error when makemkvcon failed
// to start or exited non-zero with no recoverable output.
func (c *Client) Info(
	ctx context.Context, folder string, minlengthSec int,
) ([]Title, error) {
	args := []string{"--robot", "--noscan", "info"}
	if minlengthSec > 0 {
		args = append(args, fmt.Sprintf("--minlength=%d", minlengthSec))
	}
	args = append(args, "file:"+folder)

	titles, err := c.runAndParse(ctx, args)
	if err != nil {
		return nil, fmt.Errorf("makemkv info %q: %w", folder, err)
	}
	sort.Slice(titles, func(i, j int) bool {
		return titles[i].Index < titles[j].Index
	})
	return titles, nil
}

// Mkv runs `makemkvcon --robot mkv file:{folder} {titleIndex}
// {outDir}` and returns the produced .mkv file path. outDir must
// exist before the call. Multi-title runs: invoke once per title;
// makemkvcon does support an `all` token but it's less ergonomic
// for the "convert this specific selection" UX.
func (c *Client) Mkv(
	ctx context.Context, folder string, titleIndex int, outDir string,
) error {
	if titleIndex < 0 {
		return fmt.Errorf("makemkv: titleIndex must be non-negative, got %d", titleIndex)
	}
	args := []string{
		"--robot", "--noscan", "mkv",
		"file:" + folder,
		strconv.Itoa(titleIndex),
		outDir,
	}
	if _, err := c.runAndParse(ctx, args); err != nil {
		return fmt.Errorf("makemkv mkv %q title %d: %w", folder, titleIndex, err)
	}
	return nil
}

// runAndParse executes the binary with args, streams stdout through
// the robot-line parser, and returns the discovered titles.
// makemkvcon writes progress messages on the same stdout stream the
// title info lands on; we parse the latter and ignore the former.
//
// Lines flow through scanRobot which dispatches to parseRobotLine
// per recognised tag. Errors mid-stream get attached to the run's
// error rather than aborting the parse (best-effort: a noisy
// progress line shouldn't drop the title list).
func (c *Client) runAndParse(ctx context.Context, args []string) ([]Title, error) {
	bin := c.Binary
	if bin == "" {
		bin = DefaultBinary
	}
	runner := c.Runner
	if runner == nil {
		runner = defaultRunner
	}
	cmd := runner(ctx, bin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if startErr := cmd.Start(); startErr != nil {
		return nil, fmt.Errorf("start: %w", startErr)
	}

	titles := map[int]*Title{}
	scanErr := scanRobot(stdout, titles)

	waitErr := cmd.Wait()
	if waitErr != nil {
		// makemkvcon's exit code is non-zero on partial successes
		// too (one bad title, rest succeed). Don't shadow a parse
		// success with a wait error when titles came through —
		// the caller can decide what to do.
		if len(titles) == 0 {
			return nil, fmt.Errorf("wait: %w", waitErr)
		}
	}
	if scanErr != nil && len(titles) == 0 {
		return nil, scanErr
	}

	out := make([]Title, 0, len(titles))
	for _, t := range titles {
		// Stable-sort audio + subtitle slices by their stream
		// index so render order matches the disc layout.
		sort.Slice(t.AudioTracks, func(i, j int) bool {
			return t.AudioTracks[i].Index < t.AudioTracks[j].Index
		})
		sort.Slice(t.SubtitleTracks, func(i, j int) bool {
			return t.SubtitleTracks[i].Index < t.SubtitleTracks[j].Index
		})
		out = append(out, *t)
	}
	return out, nil
}

// defaultRunner wraps exec.CommandContext into the Runner shape.
func defaultRunner(ctx context.Context, name string, args ...string) Cmd {
	return &execCmd{cmd: exec.CommandContext(ctx, name, args...)}
}

// execCmd adapts *exec.Cmd to the Cmd interface so tests can stub.
type execCmd struct{ cmd *exec.Cmd }

func (e *execCmd) StdoutPipe() (io.ReadCloser, error) { return e.cmd.StdoutPipe() }
func (e *execCmd) Start() error                       { return e.cmd.Start() }
func (e *execCmd) Wait() error                        { return e.cmd.Wait() }

// scanRobot walks stdout line-by-line and feeds each robot-tagged
// line into parseRobotLine. Non-tagged lines (info messages,
// progress) are ignored.
func scanRobot(r io.Reader, titles map[int]*Title) error {
	scanner := bufio.NewScanner(r)
	// Some titles' details lines run long; bump the buffer ceiling.
	scanner.Buffer(make([]byte, 0, scannerInitialBuffer), scannerBufferCap)
	for scanner.Scan() {
		parseRobotLine(scanner.Text(), titles)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read robot output: %w", err)
	}
	return nil
}

// parseRobotLine handles a single line. Tags we care about:
//
//	TCOUNT:<n>                              total title count (ignored;
//	                                        we discover titles via TINFO).
//	TINFO:<title>,<code>,<flags>,"<value>"  per-title attribute.
//	SINFO:<title>,<stream>,<code>,<flags>,"<value>"
//	                                        per-stream attribute.
//
// Unknown tags + malformed lines are silently dropped.
func parseRobotLine(line string, titles map[int]*Title) {
	const tinfoTag = "TINFO:"
	const sinfoTag = "SINFO:"
	switch {
	case strings.HasPrefix(line, tinfoTag):
		parseTINFO(strings.TrimPrefix(line, tinfoTag), titles)
	case strings.HasPrefix(line, sinfoTag):
		parseSINFO(strings.TrimPrefix(line, sinfoTag), titles)
	}
}

// parseTINFO handles `TINFO:title,code,flags,"value"`. Only a
// subset of codes are surfaced; unknowns are dropped silently.
func parseTINFO(body string, titles map[int]*Title) {
	parts := splitRobotCSV(body, tinfoFieldCount)
	if len(parts) < tinfoFieldCount {
		return
	}
	titleIdx, err := strconv.Atoi(parts[0])
	if err != nil {
		return
	}
	code, err := strconv.Atoi(parts[1])
	if err != nil {
		return
	}
	value := unquoteRobot(parts[3])
	t := getOrInsertTitle(titles, titleIdx)
	switch code {
	case tinfoName:
		t.Name = value
	case tinfoChapterCount:
		if n, perr := strconv.Atoi(value); perr == nil {
			t.Chapters = n
		}
	case tinfoDuration:
		t.Duration = parseDuration(value)
	case tinfoSizeBytes:
		if n, perr := strconv.ParseInt(value, 10, 64); perr == nil {
			t.SizeBytes = n
		}
	case tinfoSourceFilename:
		t.SourceFilename = value
	}
}

// parseSINFO handles `SINFO:title,stream,code,flags,"value"`.
// Same drop-on-malformed posture as TINFO.
func parseSINFO(body string, titles map[int]*Title) {
	parts := splitRobotCSV(body, sinfoFieldCount)
	if len(parts) < sinfoFieldCount {
		return
	}
	titleIdx, err := strconv.Atoi(parts[0])
	if err != nil {
		return
	}
	streamIdx, err := strconv.Atoi(parts[1])
	if err != nil {
		return
	}
	code, err := strconv.Atoi(parts[2])
	if err != nil {
		return
	}
	value := unquoteRobot(parts[4])
	t := getOrInsertTitle(titles, titleIdx)
	streamType := streamTypeFor(t, streamIdx)
	switch code {
	case sinfoStreamType:
		ensureStream(t, streamIdx, value)
	case sinfoCodecID:
		setStreamCodec(t, streamIdx, streamType, value)
	case sinfoLanguageCode:
		setStreamLanguage(t, streamIdx, streamType, value)
	case sinfoChannelLayout:
		if streamType == streamAudio {
			for i := range t.AudioTracks {
				if t.AudioTracks[i].Index == streamIdx {
					t.AudioTracks[i].Channels = value
					return
				}
			}
		}
	case sinfoVideoSampleFormat:
		// For video this is the resolution ("720x480"); for audio
		// it's a sample rate, which we don't surface today.
		if streamType == streamVideo {
			t.VideoResolution = value
		}
	}
}

// Stream-type sentinels used by the per-stream dispatch above.
const (
	streamUnknown = iota
	streamVideo
	streamAudio
	streamSubtitles
)

// streamTypeFor reports whether a stream index on a title is video
// / audio / subtitle. Decided lazily based on the SINFO code-1
// values that have already been observed; returns streamUnknown
// until a code-1 line has landed for the stream.
func streamTypeFor(t *Title, streamIdx int) int {
	for i := range t.AudioTracks {
		if t.AudioTracks[i].Index == streamIdx {
			return streamAudio
		}
	}
	for i := range t.SubtitleTracks {
		if t.SubtitleTracks[i].Index == streamIdx {
			return streamSubtitles
		}
	}
	// Video stream isn't tracked as a slot — there's only one
	// per title, and VideoCodec / VideoResolution live directly
	// on the Title struct.
	if t.VideoCodec != "" || t.VideoResolution != "" {
		return streamVideo
	}
	return streamUnknown
}

// ensureStream registers a stream slot for streamIdx based on its
// type label. Video streams are tracked directly on the title;
// audio and subtitle each get their own slice slot.
func ensureStream(t *Title, streamIdx int, typeLabel string) {
	switch typeLabel {
	case "Video":
		// Touch a field so streamTypeFor recognises this index as
		// video on subsequent SINFO lines.
		if t.VideoCodec == "" {
			t.VideoCodec = " "
		}
	case "Audio":
		for i := range t.AudioTracks {
			if t.AudioTracks[i].Index == streamIdx {
				return
			}
		}
		t.AudioTracks = append(t.AudioTracks, AudioTrack{Index: streamIdx})
	case "Subtitles":
		for i := range t.SubtitleTracks {
			if t.SubtitleTracks[i].Index == streamIdx {
				return
			}
		}
		t.SubtitleTracks = append(t.SubtitleTracks, SubtitleTrack{Index: streamIdx})
	}
}

// setStreamCodec sets the codec field on the right slot based on
// the stream type. Video codecs strip the "V_" prefix on render so
// the SPA shows "MPEG2" not "V_MPEG2"; audio / subtitle codecs
// keep their raw token for now (rare enough that the user reads
// them).
func setStreamCodec(t *Title, streamIdx, streamType int, codec string) {
	switch streamType {
	case streamVideo:
		t.VideoCodec = strings.TrimPrefix(codec, "V_")
	case streamAudio:
		for i := range t.AudioTracks {
			if t.AudioTracks[i].Index == streamIdx {
				t.AudioTracks[i].Codec = strings.TrimPrefix(codec, "A_")
				return
			}
		}
	}
}

// setStreamLanguage sets the language field on the right slot
// based on the stream type.
func setStreamLanguage(t *Title, streamIdx, streamType int, lang string) {
	switch streamType {
	case streamAudio:
		for i := range t.AudioTracks {
			if t.AudioTracks[i].Index == streamIdx {
				t.AudioTracks[i].Language = lang
				return
			}
		}
	case streamSubtitles:
		for i := range t.SubtitleTracks {
			if t.SubtitleTracks[i].Index == streamIdx {
				t.SubtitleTracks[i].Language = lang
				return
			}
		}
	}
}

// getOrInsertTitle returns the existing title slot for idx or
// creates one.
func getOrInsertTitle(titles map[int]*Title, idx int) *Title {
	if t, ok := titles[idx]; ok {
		return t
	}
	t := &Title{Index: idx}
	titles[idx] = t
	return t
}

// splitRobotCSV splits a robot-line body into up to maxFields
// fields. Honors double-quoted strings (which may contain commas).
// Returns a slice of raw fields; quoted values still carry their
// surrounding quotes — call unquoteRobot to strip.
func splitRobotCSV(s string, maxFields int) []string {
	out := make([]string, 0, maxFields)
	var cur strings.Builder
	inQuotes := false
	for i := range len(s) {
		c := s[i]
		if c == '"' {
			inQuotes = !inQuotes
			cur.WriteByte(c)
			continue
		}
		if c == ',' && !inQuotes {
			out = append(out, cur.String())
			cur.Reset()
			if len(out) == maxFields-1 {
				// Remaining text goes verbatim into the last
				// field — covers the case where the quoted value
				// contains internal commas we've already handled
				// above, but also the safety net for malformed
				// trailing data.
				out = append(out, s[i+1:])
				return out
			}
			continue
		}
		cur.WriteByte(c)
	}
	out = append(out, cur.String())
	return out
}

// unquoteRobot strips surrounding double quotes from a robot-CSV
// value. Internal escapes are rare in makemkvcon output (titles
// don't carry stray quotes) so a simple trim is enough.
func unquoteRobot(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// parseDuration parses makemkvcon's "H:MM:SS" duration into
// seconds. Returns 0 on malformed input.
func parseDuration(s string) int {
	parts := strings.Split(s, ":")
	if len(parts) != durationFields {
		return 0
	}
	h, herr := strconv.Atoi(parts[0])
	m, merr := strconv.Atoi(parts[1])
	sec, serr := strconv.Atoi(parts[2])
	if herr != nil || merr != nil || serr != nil {
		return 0
	}
	return h*secondsPerHour + m*secondsPerMinute + sec
}

// ErrBinaryMissing surfaces when makemkvcon isn't on PATH and the
// caller didn't configure an explicit Binary path. The job runner
// returns this so the SPA can render a "Configure library.makemkvPath"
// hint rather than a cryptic exec-failed error.
var ErrBinaryMissing = errors.New("makemkv: binary not found — set library.makemkvPath")
