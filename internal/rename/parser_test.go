package rename_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/brianvoe/gofakeit/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/rename"
)

func TestResolveFromName(t *testing.T) {
	t.Parallel()
	gofakeit.Seed(0)

	tests := []struct {
		name   string
		base   string
		wantID int64
		wantOK bool
	}{
		{
			name: "encora_brackets", base: "Marigold - Broadway - 2009 [encora-90100222].mp4",
			wantID: 90100222, wantOK: true,
		},
		{name: "e_brackets", base: "Some Show [e-1234].mp4", wantID: 1234, wantOK: true},
		{name: "e_braces", base: "Some Show {e-9999}.mp4", wantID: 9999, wantOK: true},
		{name: "encora_parens", base: "Some Show (encora-77).mp4", wantID: 77, wantOK: true},
		{name: "case_insensitive", base: "Some Show [ENCORA-555].mp4", wantID: 555, wantOK: true},
		{name: "no_match", base: "Random Recording.mp4"},
		{name: "wrong_separator", base: "Show [encora_8222].mp4"},
		{name: "zero_id_invalid", base: "Show [encora-0].mp4"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, tt.base)
			require.NoError(t, os.WriteFile(path, []byte("video"), 0o644))

			id, source, err := rename.Resolve(path, 0)
			if !tt.wantOK {
				require.ErrorIs(t, err, rename.ErrNoEncoraID)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantID, id)
			assert.Equal(t, rename.SourceFilename, source)
		})
	}
}

func TestResolvePrecedence(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".encora-id"), []byte("90100222\n"), 0o644))
	videoPath := filepath.Join(dir, "Some Show [encora-1234].mp4")
	require.NoError(t, os.WriteFile(videoPath, []byte("video"), 0o644))

	tests := []struct {
		name       string
		flagID     int
		path       string
		wantID     int64
		wantSource rename.ResolveSource
	}{
		{
			name:   "flag_wins_over_everything",
			flagID: 9999, path: videoPath,
			wantID: 9999, wantSource: rename.SourceFlag,
		},
		{
			name:   "sidecar_wins_over_filename",
			path:   videoPath,
			wantID: 90100222, wantSource: rename.SourceSidecar,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			id, source, err := rename.Resolve(tt.path, tt.flagID)
			require.NoError(t, err)
			assert.Equal(t, tt.wantID, id)
			assert.Equal(t, tt.wantSource, source)
		})
	}
}

func TestResolveFolderName(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	folder := filepath.Join(dir, "Show - Broadway - 2024-01-21 [encora-90118317]")
	require.NoError(t, os.MkdirAll(folder, 0o755))
	video := filepath.Join(folder, "Random Filename.mkv")
	require.NoError(t, os.WriteFile(video, []byte("v"), 0o644))

	id, source, err := rename.Resolve(video, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(90118317), id)
	assert.Equal(t, rename.SourceFolder, source)
}
