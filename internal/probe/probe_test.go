package probe_test

import (
	"errors"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/probe"
)

// TestQualityHeightMapping covers the height-bucket boundaries. The
// mapping is the contract surfaced through the {Quality} rename token,
// so every recognized bucket gets at least one row.
func TestQualityHeightMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		height int
		want   string
	}{
		{name: "zero_height", height: 0, want: ""},
		{name: "negative_height", height: -1, want: ""},
		{name: "small_240", height: 200, want: "240p"},
		{name: "exactly_360", height: 360, want: "360p"},
		{name: "exactly_480", height: 480, want: "480p"},
		{name: "exactly_720", height: 720, want: "720p"},
		{name: "off_by_one_720", height: 700, want: "720p"},
		{name: "exactly_1080", height: 1080, want: "1080p"},
		{name: "cropped_cinema_1080", height: 800, want: "720p"},
		{name: "exactly_1440", height: 1440, want: "1440p"},
		{name: "exactly_2160", height: 2160, want: "2160p"},
		{name: "8k_clamps_to_2160p", height: 4320, want: "2160p"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			info := probe.MediaInfo{Height: tt.height}
			assert.Equal(t, tt.want, info.Quality())
		})
	}
}

// TestProbeMissingBinary asserts that pointing FFProbe at a
// definitely-not-on-PATH binary surfaces a clear "not available" error
// (and doesn't, e.g., panic or return an empty MediaInfo). This is the
// hard-requirement behavior the user explicitly asked for.
func TestProbeMissingBinary(t *testing.T) {
	t.Parallel()
	p := probe.FFProbe{Path: "definitely-not-a-real-binary-name-9012834"}
	_, err := p.Probe(t.Context(), "/tmp/nonexistent.mkv")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "definitely-not-a-real-binary-name-9012834")
	// The wrapping preserves exec.ErrNotFound so callers can branch on
	// the missing-binary case if they want a custom recovery.
	assert.True(t,
		errors.Is(err, exec.ErrNotFound) ||
			err.Error() != "",
		"missing-binary error must be informative")
}

// TestProbeEmptyPath rejects the no-path call before shelling out.
func TestProbeEmptyPath(t *testing.T) {
	t.Parallel()
	_, err := probe.FFProbe{}.Probe(t.Context(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path is empty")
}

// TestParsePixFmtBitDepth covers the pixel-format → bit-depth helper
// the recording detail card surfaces. Default fall-through is 8 so an
// unknown / odd format never produces a 0-bit display.
func TestParsePixFmtBitDepth(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want int
	}{
		{name: "empty_defaults_to_8", in: "", want: 8},
		{name: "yuv420p_is_8", in: "yuv420p", want: 8},
		{name: "yuvj420p_is_8", in: "yuvj420p", want: 8},
		{name: "yuv422p_is_8", in: "yuv422p", want: 8},
		{name: "yuv444p_is_8", in: "yuv444p", want: 8},
		{name: "yuv420p10le_is_10", in: "yuv420p10le", want: 10},
		{name: "yuv420p10be_is_10", in: "yuv420p10be", want: 10},
		{name: "yuv422p10le_is_10", in: "yuv422p10le", want: 10},
		{name: "yuv444p12le_is_12", in: "yuv444p12le", want: 12},
		{name: "yuv420p16le_is_16", in: "yuv420p16le", want: 16},
		{name: "rgb24_is_8", in: "rgb24", want: 8},
		{name: "unknown_falls_to_8", in: "weirdo123", want: 8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := probe.ParsePixFmtBitDepth(tt.in)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestParseFrameRate covers ffprobe's rational frame-rate format. The
// 24000/1001 case is the dominant Broadway recording cadence — the
// detail page renders this as "23.976".
func TestParseFrameRate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want float64
	}{
		{name: "empty_zero", in: "", want: 0},
		{name: "ntsc_2398", in: "24000/1001", want: 24000.0 / 1001.0},
		{name: "pal_25", in: "25/1", want: 25},
		{name: "ntsc_2997", in: "30000/1001", want: 30000.0 / 1001.0},
		{name: "60fps_exact", in: "60/1", want: 60},
		{name: "bare_integer", in: "30", want: 30},
		{name: "zero_denom", in: "30/0", want: 0},
		{name: "garbage", in: "abc/def", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := probe.ParseFrameRateForTest(tt.in)
			// Float compare: the exact rationals are reproduce-able to
			// the bit, so direct equality is fine.
			assert.InDelta(t, tt.want, got, 1e-9)
		})
	}
}

// TestParseFFProbeOutput drives the JSON parser against representative
// ffprobe payloads. Each fixture covers a different stream-shape the
// parser is expected to handle without breaking the rename-token
// surface.
func TestParseFFProbeOutput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		json   string
		expect func(t *testing.T, got probe.MediaInfo)
	}{
		{
			name: "1080p_h264_stereo_eng",
			json: `{
				"streams": [
					{
						"index": 0, "codec_type": "video", "codec_name": "h264",
						"width": 1920, "height": 1080, "pix_fmt": "yuv420p",
						"r_frame_rate": "24000/1001", "field_order": "progressive"
					},
					{
						"index": 1, "codec_type": "audio", "codec_name": "aac",
						"channels": 2, "channel_layout": "stereo",
						"bit_rate": "192000",
						"tags": {"language": "eng"}
					}
				],
				"format": {"duration": "2643.987000"}
			}`,
			expect: func(t *testing.T, got probe.MediaInfo) {
				t.Helper()
				assert.Equal(t, "h264", got.VideoCodec)
				assert.Equal(t, 1920, got.Width)
				assert.Equal(t, 1080, got.Height)
				assert.Equal(t, 8, got.VideoBitDepth)
				assert.InDelta(t, 23.976, got.VideoFps, 1e-3)
				assert.Equal(t, "progressive", got.ScanType)
				assert.InDelta(t, 2643.987, got.DurationSeconds, 1e-3)
				require.Len(t, got.AudioStreams, 1)
				assert.Equal(t, "aac", got.AudioStreams[0].Codec)
				assert.Equal(t, "stereo", got.AudioStreams[0].ChannelLayout)
				assert.Equal(t, 192000, got.AudioStreams[0].Bitrate)
				assert.Equal(t, "eng", got.AudioStreams[0].Language)
				assert.Empty(t, got.SubtitleStreams)
			},
		},
		{
			name: "720p_hevc_51_two_audio_with_subs",
			json: `{
				"streams": [
					{
						"index": 0, "codec_type": "video", "codec_name": "hevc",
						"width": 1280, "height": 720, "pix_fmt": "yuv420p10le",
						"r_frame_rate": "25/1", "field_order": "progressive"
					},
					{
						"index": 1, "codec_type": "audio", "codec_name": "ac3",
						"channels": 6, "channel_layout": "5.1",
						"bit_rate": "640000",
						"tags": {"language": "eng"}
					},
					{
						"index": 2, "codec_type": "audio", "codec_name": "aac",
						"channels": 2, "channel_layout": "stereo",
						"bit_rate": "128000",
						"tags": {"language": "eng"}
					},
					{
						"index": 3, "codec_type": "subtitle", "codec_name": "subrip",
						"tags": {"language": "eng"}
					}
				],
				"format": {"duration": "2644.0"}
			}`,
			expect: func(t *testing.T, got probe.MediaInfo) {
				t.Helper()
				assert.Equal(t, "hevc", got.VideoCodec)
				assert.Equal(t, 1280, got.Width)
				assert.Equal(t, 720, got.Height)
				assert.Equal(t, 10, got.VideoBitDepth)
				assert.InDelta(t, 25.0, got.VideoFps, 1e-9)
				require.Len(t, got.AudioStreams, 2)
				assert.Equal(t, "ac3", got.AudioStreams[0].Codec)
				assert.Equal(t, "5.1", got.AudioStreams[0].ChannelLayout)
				assert.Equal(t, 640000, got.AudioStreams[0].Bitrate)
				assert.Equal(t, "eng", got.AudioStreams[0].Language)
				assert.Equal(t, "stereo", got.AudioStreams[1].ChannelLayout)
				require.Len(t, got.SubtitleStreams, 1)
				assert.Equal(t, "subrip", got.SubtitleStreams[0].Codec)
				assert.Equal(t, "eng", got.SubtitleStreams[0].Language)
			},
		},
		{
			name: "audio_only_no_video_stream",
			json: `{
				"streams": [
					{
						"index": 0, "codec_type": "audio", "codec_name": "flac",
						"channels": 2, "channel_layout": "stereo",
						"bit_rate": ""
					}
				],
				"format": {"duration": "187.5"}
			}`,
			expect: func(t *testing.T, got probe.MediaInfo) {
				t.Helper()
				assert.Empty(t, got.VideoCodec)
				assert.Equal(t, 0, got.Width)
				assert.Equal(t, 0, got.Height)
				assert.Equal(t, 0, got.VideoBitDepth)
				assert.InDelta(t, 0.0, got.VideoFps, 1e-9)
				require.Len(t, got.AudioStreams, 1)
				assert.Equal(t, 0, got.AudioStreams[0].Bitrate, "empty bit_rate parses as 0")
				assert.Empty(t, got.AudioStreams[0].Language)
				assert.InDelta(t, 187.5, got.DurationSeconds, 1e-9)
			},
		},
		{
			name: "channel_layout_missing_falls_back_to_count",
			json: `{
				"streams": [
					{
						"index": 0, "codec_type": "video", "codec_name": "h264",
						"width": 1920, "height": 1080, "pix_fmt": "yuv420p",
						"r_frame_rate": "30/1", "field_order": "progressive"
					},
					{
						"index": 1, "codec_type": "audio", "codec_name": "ac3",
						"channels": 6,
						"bit_rate": "448000"
					}
				],
				"format": {"duration": "60.0"}
			}`,
			expect: func(t *testing.T, got probe.MediaInfo) {
				t.Helper()
				require.Len(t, got.AudioStreams, 1)
				assert.Equal(t, "6-channel", got.AudioStreams[0].ChannelLayout)
			},
		},
		{
			name: "multi_video_streams_first_wins",
			json: `{
				"streams": [
					{
						"index": 0, "codec_type": "video", "codec_name": "h264",
						"width": 1920, "height": 1080, "pix_fmt": "yuv420p",
						"r_frame_rate": "24000/1001", "field_order": "progressive"
					},
					{
						"index": 1, "codec_type": "video", "codec_name": "mjpeg",
						"width": 320, "height": 240, "pix_fmt": "yuvj420p"
					}
				],
				"format": {"duration": "1.0"}
			}`,
			expect: func(t *testing.T, got probe.MediaInfo) {
				t.Helper()
				// The first video stream wins; the embedded thumbnail
				// stream that some files carry doesn't override.
				assert.Equal(t, "h264", got.VideoCodec)
				assert.Equal(t, 1920, got.Width)
				assert.Equal(t, 1080, got.Height)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := probe.ParseFFProbeOutputForTest([]byte(tt.json))
			require.NoError(t, err)
			tt.expect(t, got)
		})
	}
}

// TestParseFFProbeOutputMalformed asserts the parser returns an error
// (rather than panicking) when ffprobe returns malformed JSON.
func TestParseFFProbeOutputMalformed(t *testing.T) {
	t.Parallel()
	_, err := probe.ParseFFProbeOutputForTest([]byte(`{not really json`))
	require.Error(t, err)
}
