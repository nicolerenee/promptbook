package server_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// stageImage writes a placeholder file at path so cache.HasX checks
// resolve true. The bytes are arbitrary — handlers don't read them,
// they only stat the path.
func stageImage(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
}
