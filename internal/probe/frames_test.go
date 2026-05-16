package probe_test

// frames_test.go drives FrameExtractor.Extract end-to-end against a
// fake ffmpeg binary. The fake is a tiny shell script that emulates
// the real binary's behaviour for our purposes — it ignores its
// arguments other than reading the trailing positional file path,
// then writes a few JPEG-magic bytes there so the caller's "got a
// file" check succeeds.
//
// Why a shell script: the production code shells to ffmpeg via
// os/exec; injecting a fake at the FrameExtractor.Path level
// exercises the actual exec path (-ss / -i / -vf parsing, output
// redirection, missing-binary diagnosis) rather than mocking it
// out. Mirrors cmd/library_scan_test.go's fakeFFProbeScript pattern
// the user already keeps clean elsewhere in the repo.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/probe"
)

// fixedRand returns the supplied float64 from every Float64 call,
// so tests can assert exact bucket-offset positions without dealing
// with the real PRNG. The two values most useful in tests are 0
// (offset = bucketStart, the bottom of each bucket) and a positive
// fraction like 0.5 (offset = bucketStart + 0.5*bucketSize, the
// middle).
type fixedRand struct{ v float64 }

func (f fixedRand) Float64() float64 { return f.v }

// captureRand records every Float64 call so tests can assert how
// many buckets the extractor visited and in which order. Wraps
// fixedRand so the recorded sequence is deterministic.
type captureRand struct {
	mu    sync.Mutex
	calls int
	v     float64
}

func (c *captureRand) Float64() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.v
}

// fakeFFmpegScript writes a tiny shell script that emulates the
// frame-extractor's contract: ignore every flag, peel the last
// positional argument off ($@) as the destination path, and write a
// minimal JPEG header to it. The wrapper looks for a non-empty
// file at outDir/<idx>.jpg after each call, so any plausible JPEG
// (the SOI marker FF D8 alone is enough for "this exists") works.
func fakeFFmpegScript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ffmpeg")
	// The script pulls the destination path off the last positional
	// argument (ffmpeg's convention) and writes the two-byte JPEG
	// SOI marker plus a stub end-of-image. Real JPEG decoders would
	// reject this minimal payload, but the extractor doesn't decode
	// — it only checks that the file landed.
	body := "#!/bin/sh\n" +
		`dest="${@: -1}"` + "\n" +
		`printf '\xff\xd8\xff\xd9' > "$dest"` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o755))
	return path
}

// TestFrameExtractorExtractAllFrames verifies the happy path: a 10-
// frame request against a fake ffmpeg lays down 10 numerically-
// named files (0.jpg through 9.jpg) in outDir and the returned
// slice carries each path in order. captureRand asserts the rng
// was hit exactly once per bucket so the spread-and-jitter logic
// is exercised end-to-end.
func TestFrameExtractorExtractAllFrames(t *testing.T) {
	t.Parallel()

	bin := fakeFFmpegScript(t)
	outDir := t.TempDir()
	rng := &captureRand{v: 0.5}

	const count = 10
	const duration = 600.0 // 10 minute fake recording.

	frames, err := probe.FrameExtractor{Path: bin}.Extract(
		t.Context(), "/does/not/matter.mkv", outDir, duration, count, rng,
	)
	require.NoError(t, err)
	require.Len(t, frames, count)
	for i, p := range frames {
		want := filepath.Join(outDir, strconv.Itoa(i)+".jpg")
		assert.Equal(t, want, p, "frame %d path", i)
		info, statErr := os.Stat(p)
		require.NoError(t, statErr, "frame %d missing on disk", i)
		assert.False(t, info.IsDir(), "frame %d is a directory", i)
		assert.Positive(t, info.Size(), "frame %d empty", i)
	}
	assert.Equal(t, count, rng.calls,
		"rng should be called exactly once per bucket")
}

// TestFrameExtractorMissingBinary asserts a missing ffmpeg binary
// surfaces a clear "not available" error wrapping exec.ErrNotFound,
// mirroring the FFProbe missing-binary contract — callers can
// branch on the wrapped sentinel for custom recovery.
func TestFrameExtractorMissingBinary(t *testing.T) {
	t.Parallel()

	outDir := t.TempDir()
	frames, err := probe.FrameExtractor{
		Path: "definitely-not-a-real-binary-zzz9999",
	}.Extract(
		t.Context(), "/some/file.mkv", outDir, 600, 3, fixedRand{v: 0.5},
	)
	require.Error(t, err)
	assert.Empty(t, frames, "extract should bail on first missing-binary error")
	assert.ErrorIs(t, err, exec.ErrNotFound,
		"missing binary should wrap exec.ErrNotFound")
}

// TestFrameExtractorBadInputs covers the fast-fail branches that
// reject obviously broken arguments before any exec.
func TestFrameExtractorBadInputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		path     string
		out      string
		duration float64
		count    int
		want     string
	}{
		{
			name: "empty_video_path", path: "", out: t.TempDir(),
			duration: 60, count: 3, want: "videoPath is empty",
		},
		{
			name: "empty_out_dir", path: "/x.mkv", out: "",
			duration: 60, count: 3, want: "outDir is empty",
		},
		{
			name: "zero_count", path: "/x.mkv", out: t.TempDir(),
			duration: 60, count: 0, want: "count must be positive",
		},
		{
			name: "negative_count", path: "/x.mkv", out: t.TempDir(),
			duration: 60, count: -1, want: "count must be positive",
		},
		{
			name: "zero_duration", path: "/x.mkv", out: t.TempDir(),
			duration: 0, count: 3, want: "durationSeconds must be positive",
		},
		{
			name: "negative_duration", path: "/x.mkv", out: t.TempDir(),
			duration: -1, count: 3, want: "durationSeconds must be positive",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := probe.FrameExtractor{}.Extract(
				t.Context(), tt.path, tt.out, tt.duration, tt.count, fixedRand{v: 0},
			)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

// TestFrameExtractorNilRNGSeedsItself asserts that passing a nil
// rng works — the extractor seeds its own from crypto/rand, so the
// happy path doesn't require callers to plumb a *math/rand.Rand
// through their request lifecycle.
func TestFrameExtractorNilRNGSeedsItself(t *testing.T) {
	t.Parallel()

	bin := fakeFFmpegScript(t)
	outDir := t.TempDir()
	frames, err := probe.FrameExtractor{Path: bin}.Extract(
		t.Context(), "/x.mkv", outDir, 60, 3, nil,
	)
	require.NoError(t, err)
	assert.Len(t, frames, 3)
}

// TestFrameExtractorPartialFailureSkips covers the per-frame skip
// path: when ffmpeg exits non-zero on the second call (and only
// the second), the extractor should still return the frames that
// succeeded rather than failing the whole batch on one missing
// keyframe. The fake script reads a counter file and decides per-
// invocation whether to write or exit 1.
func TestFrameExtractorPartialFailureSkips(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	bin := filepath.Join(dir, "ffmpeg")
	counter := filepath.Join(dir, "counter")
	body := "#!/bin/sh\n" +
		`n=$(cat "` + counter + `" 2>/dev/null || echo 0)` + "\n" +
		`echo $((n+1)) > "` + counter + `"` + "\n" +
		// Exit 1 on the second invocation. Every other call writes a
		// fake JPEG to the destination path.
		`if [ "$n" -eq 1 ]; then` + "\n" +
		`  echo "fake ffmpeg: synthetic failure" >&2` + "\n" +
		`  exit 1` + "\n" +
		`fi` + "\n" +
		`dest="${@: -1}"` + "\n" +
		`printf '\xff\xd8\xff\xd9' > "$dest"` + "\n"
	require.NoError(t, os.WriteFile(bin, []byte(body), 0o755))

	outDir := t.TempDir()
	frames, err := probe.FrameExtractor{Path: bin}.Extract(
		t.Context(), "/x.mkv", outDir, 100, 4, fixedRand{v: 0.5},
	)
	require.NoError(t, err, "per-frame failure must not fail the batch")
	// Three frames written, one skipped (the second call).
	assert.Len(t, frames, 3)
}
