package ingest

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDVDDestSubfolderFor pins the per-file destination subfolder
// contract for DVD imports. Single-scaffold layouts (flat or
// nested) reduce to DiscDestSubfolder so the legacy single-disc
// behaviour is preserved; multi-scaffold layouts (Act 1 + Act 2,
// each its own VIDEO_TS) keep the source-relative parent ahead of
// the VIDEO_TS/ leaf so per-scaffold .IFO / .BUP / VOB files land
// in distinct destinations and shared VTS_NN_M.VOB filenames don't
// collide.
func TestDVDDestSubfolderFor(t *testing.T) {
	t.Parallel()
	const source = "/watch/Multi DVD"
	tests := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "flat single scaffold lands in legacy VIDEO_TS/",
			src:  filepath.Join(source, "VTS_01_1.VOB"),
			want: "VIDEO_TS",
		},
		{
			name: "nested single scaffold collapses doubled VIDEO_TS",
			src:  filepath.Join(source, "VIDEO_TS", "VTS_01_1.VOB"),
			want: "VIDEO_TS",
		},
		{
			name: "multi-scaffold flat preserves source subdir",
			src:  filepath.Join(source, "Act 2", "VTS_01_1.VOB"),
			want: filepath.Join("Act 2", "VIDEO_TS"),
		},
		{
			name: "multi-scaffold nested collapses doubled VIDEO_TS",
			src:  filepath.Join(source, "Act 2", "VIDEO_TS", "VTS_01_1.VOB"),
			want: filepath.Join("Act 2", "VIDEO_TS"),
		},
		{
			name: "scaffolding file in subscaffold uses same per-scaffold subfolder",
			src:  filepath.Join(source, "Act 2", "VIDEO_TS.IFO"),
			want: filepath.Join("Act 2", "VIDEO_TS"),
		},
		{
			name: "empty source folder falls back to legacy VIDEO_TS/",
			src:  "/somewhere/VTS_01_1.VOB",
			want: "VIDEO_TS",
		},
		{
			// Wrong-source-folder safety net: the scanner stamps
			// SourceFolder = ".../Act 1" but the assignment also
			// covers ".../Act 2/..." files (common parent is one
			// level up). filepath.Rel returns a "../Act 2/..." path
			// that — joined naively with the recording folder —
			// would escape via filepath.Clean and write files to a
			// sibling-of-recording directory. The helper must
			// substitute the source file's immediate parent
			// basename so the destination stays inside the
			// recording folder.
			name: "rel path with .. falls back to immediate parent basename",
			src:  "/watch/Waitress/Act 2/VTS_01_1.VOB",
			want: filepath.Join("Act 2", "VIDEO_TS"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sourceFolder := source
			switch tt.name {
			case "empty source folder falls back to legacy VIDEO_TS/":
				sourceFolder = ""
			case "rel path with .. falls back to immediate parent basename":
				// Simulate the scanner's "wrong" SourceFolder — the
				// helper sees a sibling subfolder ancestor instead
				// of the true common parent.
				sourceFolder = "/watch/Waitress/Act 1"
			}
			got := dvdDestSubfolderFor(sourceFolder, tt.src)
			assert.Equal(t, tt.want, got)
		})
	}
}
