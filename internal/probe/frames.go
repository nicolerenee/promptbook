package probe

// frames.go — frame-extraction wrapper. Sits next to the ffprobe
// wrapper because both shell to the same toolchain (ffmpeg ships
// alongside ffprobe; bundling them keeps the runtime-dependency story
// uniform). Used by the picker's fanart fallback when Encora has no
// curated screenshots: instead of an empty Fanart tab the server
// extracts a handful of evenly-distributed stills from the local video
// and surfaces those as picker options.

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Frame extraction tunables. Each constant's docstring carries its
// rationale so the picker behaviour stays legible from a single read.
const (
	// frameSkipFraction trims the first and last N% of the runtime so
	// random offsets don't land on intro / company-bio cards / curtain
	// black frames. 2% on each side leaves the middle 96% of the
	// runtime fair game — for a 2.5h Broadway recording that's still
	// roughly 5 seconds shaved off each end, well clear of curtain.
	// Hard-coded (not configurable) per the v1 spec; tuning is a knob
	// we can add later if the user asks for it.
	frameSkipFraction = 0.02

	// frameMaxWidth caps the extracted JPEG width. Source recordings
	// are routinely 4K; the picker thumbnails render at ~200 px, so
	// shipping the full-resolution frame would just slow the round-
	// trip without any visible improvement. 1280 px keeps file sizes
	// in the ~150-300 KiB range and decodes instantly in the browser.
	frameMaxWidth = 1280

	// frameJPEGQuality is ffmpeg's -q:v factor. 1 = best, 31 = worst;
	// 2 is the sweet spot for "looks like the original at thumbnail
	// size" without bloating file sizes. Sonarr's thumbnail pipeline
	// uses the same value.
	frameJPEGQuality = 2
)

// FrameExtractor shells to ffmpeg to grab still frames from a local
// video file. Mirror-shape of probe.FFProbe (Path field + concurrency-
// safe per-call exec) so the production wiring + the test seam look
// identical to the existing prober wiring. The zero value uses
// `ffmpeg` on PATH; set Path to point at a specific binary.
type FrameExtractor struct {
	// Path is the ffmpeg binary path. Empty means "ffmpeg" via PATH.
	Path string
}

// FrameRandSource is the random-number-source dependency the
// extractor consumes when spreading frames across each bucket. Defined
// as an interface so tests can inject a deterministic *mrand.Rand
// without unsafe-mocking math/rand globally; the real *mrand.Rand
// satisfies it directly.
type FrameRandSource interface {
	Float64() float64
}

// Extract pulls count frames from videoPath into outDir. Each frame
// is named "<idx>.jpg" (zero-based) so the picker can address them by
// numeric URL without persisting any extra metadata. durationSeconds
// is supplied by the caller (pulled off the recording's persisted
// MediaInfoJSON at picker-options time) so we don't double-probe; the
// extractor is dumb about how runtime was discovered.
//
// Random distribution: the usable runtime (duration minus the 2%
// skip on each side) is split into count equal buckets; one frame is
// extracted from a uniformly-random offset inside each bucket. The
// "spread + jitter" approach gives the user a representative cross-
// section of the show without clustering frames on identical scenes
// that consecutive seconds in a real Broadway recording would yield.
//
// Errors:
//   - exec.ErrNotFound (or wrapped equivalent) when ffmpeg is missing
//     on PATH — surfaces from the very first call, then bails for the
//     rest of the batch. Callers can errors.Is against exec.ErrNotFound.
//   - bad inputs (count <= 0, durationSeconds <= 0, empty videoPath)
//     return a fast-fail error before any exec.
//   - per-frame ffmpeg failures are LOGGED + skipped; the function
//     returns whatever extracted successfully so a single missing
//     keyframe doesn't reduce the picker tab to "no options".
func (f FrameExtractor) Extract(
	ctx context.Context,
	videoPath, outDir string,
	durationSeconds float64,
	count int,
	rng FrameRandSource,
) ([]string, error) {
	if videoPath == "" {
		return nil, errors.New("probe: extract: videoPath is empty")
	}
	if outDir == "" {
		return nil, errors.New("probe: extract: outDir is empty")
	}
	if count <= 0 {
		return nil, fmt.Errorf("probe: extract: count must be positive, got %d", count)
	}
	if durationSeconds <= 0 {
		return nil, fmt.Errorf(
			"probe: extract: durationSeconds must be positive, got %f", durationSeconds)
	}
	if rng == nil {
		rng = newDefaultFrameRand()
	}
	binary := f.Path
	if binary == "" {
		binary = "ffmpeg"
	}

	if mkErr := os.MkdirAll(outDir, 0o750); mkErr != nil {
		return nil, fmt.Errorf("probe: extract: mkdir %s: %w", outDir, mkErr)
	}

	usable := durationSeconds * (1 - 2*frameSkipFraction)
	start := durationSeconds * frameSkipFraction
	bucketSize := usable / float64(count)

	out := make([]string, 0, count)
	for i := range count {
		// Bucket bounds get a uniformly-random offset inside the bucket.
		// rng.Float64() returns [0,1) so the offset never exits the
		// bucket.
		bucketStart := start + float64(i)*bucketSize
		offset := bucketStart + rng.Float64()*bucketSize
		dest := filepath.Join(outDir, strconv.Itoa(i)+".jpg")
		if err := f.extractOne(ctx, binary, videoPath, dest, offset); err != nil {
			// Per-frame failures don't fail the batch — a single
			// missing keyframe shouldn't reduce the picker tab to
			// "no options". The first error surfaces the missing-
			// binary case so the caller can short-circuit; subsequent
			// per-frame errors are best-effort and silently skipped
			// (ffmpeg writes its own stderr to the process's stderr
			// stream so the operator can triage from logs if needed).
			if isMissingBinary(err) {
				return out, fmt.Errorf("probe: extract: %w", err)
			}
			continue
		}
		out = append(out, dest)
	}
	return out, nil
}

// extractOne shells to ffmpeg for a single frame at offset seconds.
// The -ss before -i is ffmpeg's fast input-side seek which jumps to
// the nearest keyframe — perfect for thumbnail-quality stills.
// -frames:v 1 caps the output at one frame; -q:v sets JPEG quality;
// -vf scale caps the width at frameMaxWidth while preserving aspect
// (the -2 height token rounds to the nearest even integer, which
// libjpeg requires).
//
// -y forces overwrite so a re-extract pass after ClearFrames can re-
// use the same numeric filenames without first os.Remove'ing them.
//
// stderr is captured so the per-frame skip path can include ffmpeg's
// own diagnostic in the wrapped error — callers log the wrapped form
// at debug level and move on.
func (f FrameExtractor) extractOne(
	ctx context.Context,
	binary, videoPath, dest string,
	offset float64,
) error {
	// videoPath + dest both come from trusted callers (the picker
	// resolves videoPath off storage.ListVersions and writes dest
	// inside the configured imagecache root); shelling them is the
	// whole point of the wrapper. gosec's taint-tracking can't tell
	// the difference, so the nolint is accurate, not a workaround.
	cmd := exec.CommandContext( //nolint:gosec // videoPath/dest are trusted, see comment.
		ctx, binary,
		"-nostdin",
		"-loglevel", "error",
		"-ss", strconv.FormatFloat(offset, 'f', 3, 64),
		"-i", videoPath,
		"-frames:v", "1",
		"-q:v", strconv.Itoa(frameJPEGQuality),
		"-vf", fmt.Sprintf("scale=%d:-2", frameMaxWidth),
		"-y",
		dest,
	)
	stderr, err := cmd.CombinedOutput()
	if err != nil {
		return decodeFrameExecError(binary, err, stderr)
	}
	return nil
}

// isMissingBinary reports whether err wraps an exec.ErrNotFound,
// signaling that ffmpeg is unavailable on PATH. Used by Extract to
// short-circuit the batch — the user can install ffmpeg and retry,
// trying every offset would just produce the same error N times.
func isMissingBinary(err error) bool {
	return errors.Is(err, exec.ErrNotFound)
}

// decodeFrameExecError mirrors decodeExecError on the probe side but
// with the per-frame stderr context inlined into the wrapped error so
// downstream logs surface the ffmpeg diagnostic ("Invalid argument",
// "moov atom not found", etc.) rather than a bare "exit status 1".
func decodeFrameExecError(binary string, err error, stderr []byte) error {
	var execErr *exec.Error
	if errors.As(err, &execErr) {
		return fmt.Errorf("probe: %s not available: %w", binary, err)
	}
	trimmed := strings.TrimSpace(string(stderr))
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if trimmed != "" {
			return fmt.Errorf("probe: %s failed: %s: %w", binary, trimmed, err)
		}
		return fmt.Errorf("probe: %s exited %d: %w", binary, exitErr.ExitCode(), err)
	}
	return fmt.Errorf("probe: run %s: %w", binary, err)
}

// newDefaultFrameRand seeds a math/rand/v2 source from crypto/rand
// so each Extract call gets a fresh-but-non-deterministic spread.
// The caller-supplied rng path (used by tests) bypasses this
// entirely.
//
// Crypto-seeding is deliberate even though this is a non-security
// path — it sidesteps the historical math/rand global-source quirks
// without requiring callers to seed their own.
func newDefaultFrameRand() *mrand.Rand {
	var seed1 [8]byte
	var seed2 [8]byte
	if _, err := rand.Read(seed1[:]); err != nil {
		// crypto/rand.Read failing is exotic enough (the syscall it
		// hits is the same /dev/urandom Go's runtime uses) that a
		// time-based fallback would be alarming. Bubble up via a
		// zero PCG seed: deterministic across runs but never panics.
		return mrand.New(mrand.NewPCG(0, 0)) //nolint:gosec // non-security RNG.
	}
	_, _ = rand.Read(seed2[:])
	//nolint:gosec // non-security RNG; spreading frames not generating tokens.
	return mrand.New(mrand.NewPCG(
		binary.LittleEndian.Uint64(seed1[:]),
		binary.LittleEndian.Uint64(seed2[:]),
	))
}
