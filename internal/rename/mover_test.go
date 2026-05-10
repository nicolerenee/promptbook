package rename_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/rename"
)

func TestPlanApplyMoves(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	library := filepath.Join(root, "library")
	source := filepath.Join(root, "incoming", "Marigold.mp4")
	require.NoError(t, os.MkdirAll(filepath.Dir(source), 0o755))
	require.NoError(t, os.WriteFile(source, []byte("video bytes"), 0o644))

	plan := rename.Plan{
		Source:       source,
		LibraryRoot:  library,
		TargetFolder: "Marigold - Broadway - December 2009 [encora-90100222]",
		TargetFile:   "Marigold - Broadway - December 2009 [pro-shot]",
		Extension:    ".mp4",
	}

	dest, err := plan.Apply()
	require.NoError(t, err)
	assert.Equal(t, plan.AbsoluteFile(), dest)

	// Source is gone, dest exists with the original bytes.
	_, err = os.Stat(source)
	assert.True(t, os.IsNotExist(err))

	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "video bytes", string(got))
}

func TestPlanApplySameFileNoOp(t *testing.T) {
	t.Parallel()

	// Source IS the canonical destination — library-root backfill case
	// where the file already lives at the path the rename templates
	// would produce. Apply must short-circuit to a no-op success
	// instead of refusing because the destination is "occupied".
	root := t.TempDir()
	plan := rename.Plan{
		LibraryRoot:  root,
		TargetFolder: "Marigold - Broadway - December 2009 [encora-90100222]",
		TargetFile:   "Marigold - Broadway - December 2009 [pro-shot]",
		Extension:    ".mp4",
	}
	require.NoError(t, os.MkdirAll(plan.AbsoluteFolder(), 0o755))
	require.NoError(t, os.WriteFile(plan.AbsoluteFile(), []byte("v1"), 0o644))
	plan.Source = plan.AbsoluteFile()

	dest, err := plan.Apply()
	require.NoError(t, err)
	assert.Equal(t, plan.AbsoluteFile(), dest)

	// File still exists with original bytes — nothing was moved or
	// copied or removed.
	got, err := os.ReadFile(plan.AbsoluteFile())
	require.NoError(t, err)
	assert.Equal(t, "v1", string(got))
}

func TestPlanApplyRefusesOverwrite(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := filepath.Join(root, "Marigold.mp4")
	require.NoError(t, os.WriteFile(source, []byte("v1"), 0o644))

	plan := rename.Plan{
		Source:       source,
		LibraryRoot:  root,
		TargetFolder: "out",
		TargetFile:   "Marigold",
		Extension:    ".mp4",
	}
	require.NoError(t, os.MkdirAll(plan.AbsoluteFolder(), 0o755))
	require.NoError(t, os.WriteFile(plan.AbsoluteFile(), []byte("v2"), 0o644))

	_, err := plan.Apply()
	require.ErrorIs(t, err, rename.ErrTargetExists)
}

func TestPlanEnsureSidecar(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	plan := rename.Plan{
		LibraryRoot:  root,
		TargetFolder: "out",
		TargetFile:   "f",
		Extension:    ".mp4",
	}
	require.NoError(t, os.MkdirAll(plan.AbsoluteFolder(), 0o755))
	require.NoError(t, plan.EnsureSidecar(90100222))

	got, err := os.ReadFile(filepath.Join(plan.AbsoluteFolder(), rename.SidecarFilename))
	require.NoError(t, err)
	assert.Equal(t, "90100222\n", string(got))
}
