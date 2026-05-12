//nolint:testpackage // exercises the unexported robot parser directly.
package makemkv

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeCmd plays a pre-canned transcript back over the StdoutPipe.
// Mirrors the subset of *exec.Cmd the Client uses without shelling
// out to the real binary.
type fakeCmd struct {
	stdout io.ReadCloser
	wait   error
}

func (f *fakeCmd) StdoutPipe() (io.ReadCloser, error) { return f.stdout, nil }
func (f *fakeCmd) Start() error                       { return nil }
func (f *fakeCmd) Wait() error                        { return f.wait }

// fakeReader is the no-op closer wrapper around a strings.Reader.
type fakeReader struct{ *strings.Reader }

func (fakeReader) Close() error { return nil }

// TestInfoParsesTypicalDVDTranscript walks a captured-robot-style
// transcript through the parser and asserts the discovered titles
// match the expected shape (counts + per-title metadata + per-
// stream summaries).
func TestInfoParsesTypicalDVDTranscript(t *testing.T) {
	t.Parallel()

	transcript := strings.Join([]string{
		// Title 0: main feature, MPEG-2 480p, AC3 stereo, eng subs.
		`TINFO:0,2,0,"_TITLE_t00"`,
		`TINFO:0,8,0,"12"`,
		`TINFO:0,9,0,"2:04:24"`,
		`TINFO:0,11,0,"4651909632"`,
		`TINFO:0,16,0,"VTS_01_1.VOB"`,
		`SINFO:0,0,1,6201,"Video"`,
		`SINFO:0,0,6,0,"V_MPEG2"`,
		`SINFO:0,0,20,0,"720x480"`,
		`SINFO:0,1,1,6202,"Audio"`,
		`SINFO:0,1,6,0,"A_AC3"`,
		`SINFO:0,1,3,0,"eng"`,
		`SINFO:0,1,19,0,"2.0 ch"`,
		`SINFO:0,2,1,6203,"Subtitles"`,
		`SINFO:0,2,6,0,"S_VOBSUB"`,
		`SINFO:0,2,3,0,"eng"`,
		// Title 1: bonus piece (15 min).
		`TINFO:1,9,0,"0:14:32"`,
		`TINFO:1,11,0,"512000000"`,
		`SINFO:1,0,1,6201,"Video"`,
		`SINFO:1,0,6,0,"V_MPEG2"`,
		// Random progress / info noise that should be ignored.
		`PRGT:5018,0,"Saving 2 titles into directory ..."`,
		`PRGV:0,0,65536`,
	}, "\n") + "\n"

	cli := &Client{
		Binary: "/bin/true",
		Runner: func(_ context.Context, _ string, _ ...string) Cmd {
			return &fakeCmd{stdout: fakeReader{strings.NewReader(transcript)}}
		},
	}
	got, err := cli.Info(t.Context(), "/disc", 0)
	require.NoError(t, err)
	require.Len(t, got, 2, "two titles in the transcript")

	main := got[0]
	assert.Equal(t, 0, main.Index)
	assert.Equal(t, "_TITLE_t00", main.Name)
	assert.Equal(t, 12, main.Chapters)
	assert.Equal(t, 7464, main.Duration, "2h04m24s = 7464s")
	assert.EqualValues(t, 4651909632, main.SizeBytes)
	assert.Equal(t, "VTS_01_1.VOB", main.SourceFilename)
	assert.Equal(t, "MPEG2", main.VideoCodec, "V_ prefix stripped")
	assert.Equal(t, "720x480", main.VideoResolution)
	require.Len(t, main.AudioTracks, 1)
	assert.Equal(t, "AC3", main.AudioTracks[0].Codec)
	assert.Equal(t, "eng", main.AudioTracks[0].Language)
	assert.Equal(t, "2.0 ch", main.AudioTracks[0].Channels)
	require.Len(t, main.SubtitleTracks, 1)
	assert.Equal(t, "eng", main.SubtitleTracks[0].Language)

	bonus := got[1]
	assert.Equal(t, 1, bonus.Index)
	assert.Equal(t, 872, bonus.Duration, "0h14m32s = 872s")
	assert.Equal(t, "MPEG2", bonus.VideoCodec)
}

// TestParseDurationEdgeCases covers the timestamp parser's
// degenerate inputs.
func TestParseDurationEdgeCases(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"1:00", 0},         // wrong format (mm:ss alone).
		{"1:30:45", 5445},   // typical movie.
		{"0:00:00", 0},      // zero-length.
		{"oops", 0},         // gibberish.
		{"10:00:00", 36000}, // 10-hour edge.
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, parseDuration(tt.in))
		})
	}
}
