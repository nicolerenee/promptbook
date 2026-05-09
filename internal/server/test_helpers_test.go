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

// readFixture pulls the named file out of internal/encora/testdata so
// tests can echo a real-API response from a stubbed httptest mux. The
// fixture path is relative to the server package's test file location
// (mirrors fixturesDir in server_test.go). Returns (bytes, nil) on
// success, (nil, err) if the file is missing or unreadable.
func readFixture(t *testing.T, name string) ([]byte, error) {
	t.Helper()
	return os.ReadFile(filepath.Join("../encora/testdata", name))
}
