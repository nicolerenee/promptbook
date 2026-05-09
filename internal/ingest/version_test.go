package ingest_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nicolerenee/promptbook/internal/ingest"
)

// gigabyte is the binary GB used by Encora's existing format strings.
const gigabyte = int64(1024 * 1024 * 1024)

func TestDefaultFormatLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		container  string
		quality    string
		videoCodec string
		sizeBytes  int64
		want       string
	}{
		{
			name:       "all fields populated matches Encora shape",
			container:  "mkv",
			quality:    "1080p",
			videoCodec: "h.264",
			sizeBytes:  9_384_503_541, // 8.74 GB
			want:       "MKV (1080p - h.264) - 8.74 GB",
		},
		{
			name:       "video codec missing drops the dash",
			container:  "mkv",
			quality:    "1080p",
			videoCodec: "",
			sizeBytes:  9_384_503_541,
			want:       "MKV (1080p) - 8.74 GB",
		},
		{
			name:       "quality missing keeps the codec",
			container:  "mp4",
			quality:    "",
			videoCodec: "h.264",
			sizeBytes:  2 * gigabyte,
			want:       "MP4 (h.264) - 2.00 GB",
		},
		{
			name:       "everything empty falls back to size only",
			container:  "",
			quality:    "",
			videoCodec: "",
			sizeBytes:  0,
			want:       "0.00 GB",
		},
		{
			name:       "container only no specs",
			container:  "mkv",
			quality:    "",
			videoCodec: "",
			sizeBytes:  3 * gigabyte,
			want:       "MKV - 3.00 GB",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ingest.DefaultFormatLabel(tt.container, tt.quality, tt.videoCodec, tt.sizeBytes)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseQuality(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "1080p web-dl", input: "Show.2024.1080p.WEB-DL.mkv", want: "1080p"},
		// 2160p is preferred over 4k when both could match.
		{name: "2160p uhd", input: "show.2160p.uhd.mkv", want: "2160p"},
		// "4k" alone wins when no canonical token is present.
		{name: "4k spelled out", input: "show 4k.mkv", want: "4k"},
		{name: "720p", input: "show.720p.x264.mkv", want: "720p"},
		{name: "480p", input: "old-show.480p.mp4", want: "480p"},
		{name: "uppercase 1080P", input: "SHOW.1080P.MKV", want: "1080p"},
		{name: "no quality tokens", input: "no quality here.mkv", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, ingest.ParseQuality(tt.input))
		})
	}
}

func TestParseVideoCodec(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "h265 alias", input: "show.h265.mkv", want: "HEVC"},
		{name: "hevc explicit", input: "Show.2160p.HEVC.mkv", want: "HEVC"},
		{name: "x265 alias", input: "show.x265.mkv", want: "HEVC"},
		{name: "h264 alias", input: "show.h264.mkv", want: "h.264"},
		{name: "x264 alias", input: "show.x264.mp4", want: "h.264"},
		{name: "avc alias", input: "show.AVC.mkv", want: "h.264"},
		{name: "av1 alias", input: "show.av1.webm", want: "AV1"},
		{name: "no codec tokens", input: "Show 1080p.mkv", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, ingest.ParseVideoCodec(tt.input))
		})
	}
}

func TestParseAudioCodec(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "atmos", input: "show.Atmos.mkv", want: "Atmos"},
		{name: "truehd", input: "show.TrueHD.mkv", want: "TrueHD"},
		{name: "dts", input: "show.DTS-HD.mkv", want: "DTS"},
		// eac3 must beat ac3 — it's a superstring of "ac3".
		{name: "eac3 wins over ac3", input: "show.EAC3.mkv", want: "EAC3"},
		{name: "ac3", input: "show.ac3.mkv", want: "AC3"},
		{name: "aac", input: "show.AAC.mp4", want: "AAC"},
		{name: "no audio tokens", input: "show 1080p.mkv", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, ingest.ParseAudioCodec(tt.input))
		})
	}
}
