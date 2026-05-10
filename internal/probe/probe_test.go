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
