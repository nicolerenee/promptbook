package imagecache_test

import (
	"io"
	"testing"

	"github.com/rs/zerolog"
)

// zerologTest returns a zerolog.Logger that writes to io.Discard. The
// imagecache package's Logger field is non-pointer; tests build their
// own throwaway logger here so the live debug/warn output doesn't
// pollute `go test -v`.
func zerologTest(t *testing.T) zerolog.Logger {
	t.Helper()
	return zerolog.New(io.Discard)
}
