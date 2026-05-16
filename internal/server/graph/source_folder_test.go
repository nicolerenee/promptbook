package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCommonParentDir pins the "deepest shared ancestor" contract
// the importQueueEntry resolver uses to derive opts.SourceFolder
// for folder-as-unit drops. The critical case is nested-subfolder
// classifications (e.g. multi-scaffold DVDs with Act 1/ + Act 2/):
// without a real common ancestor, the DVD per-file destination
// resolver computes "../foo" paths that escape the recording
// folder when joined.
func TestCommonParentDir(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   []string
		want string
	}{
		{
			name: "empty input returns empty",
			in:   nil,
			want: "",
		},
		{
			name: "single path returns its parent dir",
			in:   []string{"/watch/show/VTS_01_1.VOB"},
			want: "/watch/show",
		},
		{
			name: "all paths in same dir returns that dir",
			in: []string{
				"/watch/show/VTS_01_1.VOB",
				"/watch/show/VTS_01_2.VOB",
				"/watch/show/info.txt",
			},
			want: "/watch/show",
		},
		{
			// The Waitress case: lead file lives in Act 1/, but
			// classification spans Act 1/ + Act 2/. Common parent
			// is the show folder itself.
			name: "multi-scaffold layout finds the shared show folder",
			in: []string{
				"/watch/Waitress/Act 1/VTS_01_1.VOB",
				"/watch/Waitress/Act 1/VTS_01_2.VOB",
				"/watch/Waitress/Act 2/VTS_01_1.VOB",
				"/watch/Waitress/Act 2/VTS_01_2.VOB",
				"/watch/Waitress/info.txt",
			},
			want: "/watch/Waitress",
		},
		{
			// Defensive: similar prefixes that aren't actually
			// ancestors must NOT collapse — "/foo/bar" and
			// "/foo/bar2" should resolve to "/foo", not "/foo/bar".
			name: "similar prefixes that aren't ancestors walk up correctly",
			in: []string{
				"/watch/bar/file.mkv",
				"/watch/bar2/file.mkv",
			},
			want: "/watch",
		},
		{
			name: "paths under a deep shared subtree keep the deeper ancestor",
			in: []string{
				"/watch/show/season 1/disc 1/part1.mkv",
				"/watch/show/season 1/disc 1/part2.mkv",
			},
			want: "/watch/show/season 1/disc 1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := commonParentDir(tt.in)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestClassificationPaths confirms the JSON decoder picks up both
// parts and extras paths, skips empty entries, and returns nil for
// invalid / empty input so the caller falls back to the legacy
// filepath.Dir(FilePath) source folder.
func TestClassificationPaths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{
			name: "empty string returns nil",
			raw:  "",
			want: nil,
		},
		{
			name: "invalid json returns nil",
			raw:  "not json",
			want: nil,
		},
		{
			name: "parts and extras paths combine",
			raw: `{
				"parts": [
					{"path": "/watch/show/part1.mkv"},
					{"path": "/watch/show/part2.mkv"}
				],
				"extras": [
					{"path": "/watch/show/bows.mkv"}
				]
			}`,
			want: []string{
				"/watch/show/part1.mkv",
				"/watch/show/part2.mkv",
				"/watch/show/bows.mkv",
			},
		},
		{
			name: "empty path entries are skipped",
			raw: `{
				"parts": [{"path": ""}, {"path": "/x.mkv"}],
				"extras": []
			}`,
			want: []string{"/x.mkv"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := classificationPaths(tt.raw)
			assert.Equal(t, tt.want, got)
		})
	}
}
