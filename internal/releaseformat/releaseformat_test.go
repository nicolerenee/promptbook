package releaseformat_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nicolerenee/promptbook/internal/probe"
	"github.com/nicolerenee/promptbook/internal/releaseformat"
)

// Canonical fixture sizes used across tests. 8.57 GB is the Velvet Antlers
// 2160p case; 4.20 GB is the synthetic 1080p case. Sizes are exact
// 1024-base bytes so the IEC formatter renders the expected
// fractional digits. Computed at runtime because Go requires an
// explicit int64 cast on a non-integer constant expression.
//
//nolint:gochecknoglobals // test-scoped fixture sizes.
var (
	bytes8_57GB = int64(math.Round(8.57 * 1024 * 1024 * 1024))
	bytes4_20GB = int64(math.Round(4.20 * 1024 * 1024 * 1024))
	bytes512MB  = int64(512 * 1024 * 1024)
)

// TestComposeSingleVersion covers the single-version bare-format
// branch. Establishes the canonical "Velvet Antlers 2160p" string the
// design doc uses as the working example.
func TestComposeSingleVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		versions []releaseformat.VersionInfo
		want     string
	}{
		{
			name: "velvet-antlers_2160p_hevc_aac",
			versions: []releaseformat.VersionInfo{{
				Container:  "MP4",
				VideoCodec: "hevc",
				AudioCodec: "aac",
				Height:     2160,
				SizeBytes:  bytes8_57GB,
			}},
			want: "MP4 - x265 / AAC - 2160p - 8.57 GB",
		},
		{
			name: "single_h264_1080p_mkv",
			versions: []releaseformat.VersionInfo{{
				Container:  "MKV",
				VideoCodec: "h264",
				AudioCodec: "ac3",
				Height:     1080,
				SizeBytes:  bytes4_20GB,
			}},
			want: "MKV - x264 / AC3 - 1080p - 4.20 GB",
		},
		{
			name: "missing_video_codec",
			versions: []releaseformat.VersionInfo{{
				Container:  "MP4",
				AudioCodec: "aac",
				Height:     2160,
				SizeBytes:  bytes8_57GB,
			}},
			want: "MP4 - ? / AAC - 2160p - 8.57 GB",
		},
		{
			name: "missing_audio_codec",
			versions: []releaseformat.VersionInfo{{
				Container:  "MP4",
				VideoCodec: "hevc",
				Height:     2160,
				SizeBytes:  bytes8_57GB,
			}},
			want: "MP4 - x265 / ? - 2160p - 8.57 GB",
		},
		{
			name: "missing_quality",
			versions: []releaseformat.VersionInfo{{
				Container:  "MP4",
				VideoCodec: "hevc",
				AudioCodec: "aac",
				SizeBytes:  bytes8_57GB,
			}},
			want: "MP4 - x265 / AAC - ? - 8.57 GB",
		},
		{
			name: "all_codec_fields_empty_keeps_container",
			versions: []releaseformat.VersionInfo{{
				Container: "MKV",
				SizeBytes: bytes4_20GB,
			}},
			want: "MKV - ? / ? - ? - 4.20 GB",
		},
		{
			name: "small_file_renders_in_megabytes",
			versions: []releaseformat.VersionInfo{{
				Container:  "MP4",
				VideoCodec: "h264",
				AudioCodec: "aac",
				Height:     720,
				SizeBytes:  bytes512MB,
			}},
			want: "MP4 - x264 / AAC - 720p - 512.00 MB",
		},
		{
			name: "unknown_video_codec_passes_through",
			versions: []releaseformat.VersionInfo{{
				Container:  "MKV",
				VideoCodec: "av1",
				AudioCodec: "opus",
				Height:     2160,
				SizeBytes:  bytes8_57GB,
			}},
			want: "MKV - av1 / OPUS - 2160p - 8.57 GB",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := releaseformat.Compose(tt.versions)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestComposeMultiVersion covers the bracketed multi-version branch.
// The sort key is Height descending so the best version leads.
func TestComposeMultiVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		versions []releaseformat.VersionInfo
		want     string
	}{
		{
			name: "two_versions_sorted_desc",
			versions: []releaseformat.VersionInfo{
				{
					Container:  "MP4",
					VideoCodec: "hevc",
					AudioCodec: "aac",
					Height:     2160,
					SizeBytes:  bytes8_57GB,
				},
				{
					Container:  "MP4",
					VideoCodec: "h264",
					AudioCodec: "aac",
					Height:     1080,
					SizeBytes:  bytes4_20GB,
				},
			},
			want: "[MP4 - x265 / AAC - 2160p - 8.57 GB] " +
				"[MP4 - x264 / AAC - 1080p - 4.20 GB]",
		},
		{
			name: "input_order_reversed_sort_still_desc",
			versions: []releaseformat.VersionInfo{
				{
					Container:  "MP4",
					VideoCodec: "h264",
					AudioCodec: "aac",
					Height:     1080,
					SizeBytes:  bytes4_20GB,
				},
				{
					Container:  "MP4",
					VideoCodec: "hevc",
					AudioCodec: "aac",
					Height:     2160,
					SizeBytes:  bytes8_57GB,
				},
			},
			want: "[MP4 - x265 / AAC - 2160p - 8.57 GB] " +
				"[MP4 - x264 / AAC - 1080p - 4.20 GB]",
		},
		{
			name: "mixed_completeness_keeps_question_marks",
			versions: []releaseformat.VersionInfo{
				{
					Container:  "MKV",
					VideoCodec: "hevc",
					AudioCodec: "aac",
					Height:     2160,
					SizeBytes:  bytes8_57GB,
				},
				{
					Container: "MP4",
					Height:    1080,
					SizeBytes: bytes4_20GB,
				},
			},
			want: "[MKV - x265 / AAC - 2160p - 8.57 GB] " +
				"[MP4 - ? / ? - 1080p - 4.20 GB]",
		},
		{
			name: "zero_height_sorts_last",
			versions: []releaseformat.VersionInfo{
				{
					Container: "MKV",
					Height:    0,
					SizeBytes: bytes4_20GB,
				},
				{
					Container:  "MP4",
					VideoCodec: "hevc",
					AudioCodec: "aac",
					Height:     2160,
					SizeBytes:  bytes8_57GB,
				},
			},
			want: "[MP4 - x265 / AAC - 2160p - 8.57 GB] " +
				"[MKV - ? / ? - ? - 4.20 GB]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := releaseformat.Compose(tt.versions)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestComposeEmpty asserts that an empty version slice returns the
// empty string — the recording exists but has no files registered.
func TestComposeEmpty(t *testing.T) {
	t.Parallel()
	assert.Empty(t, releaseformat.Compose(nil))
	assert.Empty(t, releaseformat.Compose([]releaseformat.VersionInfo{}))
}

// TestFromVersionAndMediaInfo covers the canonical constructor.
// Includes the legacy "no MediaInfo blob" path that callers fall
// through when re-rendering pre-probe imports.
func TestFromVersionAndMediaInfo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		info        probe.MediaInfo
		size        int64
		fallbackExt string
		want        releaseformat.VersionInfo
	}{
		{
			name: "full_blob",
			info: probe.MediaInfo{
				Container:  "MP4",
				VideoCodec: "hevc",
				Height:     2160,
				AudioStreams: []probe.AudioStreamInfo{
					{Codec: "aac"},
					{Codec: "ac3"},
				},
			},
			size:        bytes8_57GB,
			fallbackExt: ".mp4",
			want: releaseformat.VersionInfo{
				Container:  "MP4",
				VideoCodec: "hevc",
				AudioCodec: "aac",
				Height:     2160,
				SizeBytes:  bytes8_57GB,
			},
		},
		{
			name:        "empty_blob_falls_back_to_extension",
			info:        probe.MediaInfo{},
			size:        bytes4_20GB,
			fallbackExt: ".mkv",
			want: releaseformat.VersionInfo{
				Container: "MKV",
				SizeBytes: bytes4_20GB,
			},
		},
		{
			name:        "blob_container_wins_over_fallback",
			info:        probe.MediaInfo{Container: "MP4"},
			size:        bytes512MB,
			fallbackExt: ".mkv",
			want: releaseformat.VersionInfo{
				Container: "MP4",
				SizeBytes: bytes512MB,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := releaseformat.FromVersionAndMediaInfo(tt.info, tt.size, tt.fallbackExt)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestComposeLegacyImportRendering pins the rendered output of a
// legacy "no MediaInfo blob" version. Important because that path is
// the failure mode for older catalog rows that pre-date the probe
// capture; the format string still needs to read cleanly.
func TestComposeLegacyImportRendering(t *testing.T) {
	t.Parallel()
	v := releaseformat.FromVersionAndMediaInfo(
		probe.MediaInfo{}, bytes4_20GB, ".mkv",
	)
	got := releaseformat.Compose([]releaseformat.VersionInfo{v})
	assert.Equal(t, "MKV - ? / ? - ? - 4.20 GB", got)
}
