package server_test

import (
	"os"
	"path/filepath"
	"testing"
)

// readFixture pulls the named file out of internal/encora/testdata so
// tests can echo a real-API response from a stubbed httptest mux. The
// fixture path is relative to the server package's test file location
// (mirrors fixturesDir in server_test.go). Returns (bytes, nil) on
// success, (nil, err) if the file is missing or unreadable.
func readFixture(t *testing.T, name string) ([]byte, error) {
	t.Helper()
	return os.ReadFile(filepath.Join("../encora/testdata", name))
}
