package cmd_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/cmd"
	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/storage"
	syncpkg "github.com/nicolerenee/promptbook/internal/sync"
)

// fixtureSync seeds dbPath with the encora collection + wants fixtures.
// Returns the dbPath the caller should point AppConfig at.
func fixtureSync(t *testing.T) string {
	t.Helper()

	mux := http.NewServeMux()
	for path, file := range map[string]string{
		"/api/collection": "../internal/encora/testdata/collection.json",
		"/api/wants":      "../internal/encora/testdata/wants.json",
	} {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			b, err := os.ReadFile(file)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("X-RateLimit-Remaining", "25")
			_, _ = w.Write(b)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	dbPath := filepath.Join(t.TempDir(), "promptbook.db")
	sqlDB, db, err := storage.OpenEnt(t.Context(), dbPath)
	require.NoError(t, err)

	c, err := encora.New(encora.Options{BaseURL: srv.URL, APIKey: "test"})
	require.NoError(t, err)
	_, err = syncpkg.Sync(t.Context(), c, db, syncpkg.Options{BurstReserve: 2})
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
	return dbPath
}

// fakeFFProbeScript writes a tiny shell script that emulates ffprobe's
// JSON output for the wrapper. Lets the cmd-level smoke test stay
// hermetic — no dependency on a system ffprobe binary at `go test`
// time. The script ignores its input and emits a fixed 1080p/h264
// stream descriptor.
func fakeFFProbeScript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ffprobe")
	body := "#!/bin/sh\n" +
		`echo '{"streams":[{"codec_name":"h264","width":1920,"height":1080}]}'` +
		"\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o755))
	return path
}

// TestLibraryScanCommand verifies the cobra wiring: pointing
// `library scan` at a video file with a recognized encora id surfaces a
// "would-move" line in the output. Not Parallel — exercises global
// cobra/viper state.
func TestLibraryScanCommand(t *testing.T) {
	dbPath := fixtureSync(t)

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "Marigold [encora-90100222].mp4")
	require.NoError(t, os.WriteFile(src, []byte("video"), 0o644))

	libraryRoot := filepath.Join(t.TempDir(), "library")
	ffprobe := fakeFFProbeScript(t)

	// Configure via env vars so cmd.Execute pulls them through viper.
	t.Setenv("PROMPTBOOK_STORAGE_DATABASEPATH", dbPath)
	t.Setenv("PROMPTBOOK_LIBRARY_ROOT", libraryRoot)
	t.Setenv("PROMPTBOOK_LIBRARY_FOLDERTEMPLATE", "{Show} - {Tour} - {Date} [encora-{EncoraID}]")
	t.Setenv("PROMPTBOOK_LIBRARY_FILETEMPLATE", "{Show} - {Tour} - {Date} [{Master}]")
	t.Setenv("PROMPTBOOK_LIBRARY_FFPROBEPATH", ffprobe)
	t.Setenv("PROMPTBOOK_ENCORA_APIKEY", "stub")

	var buf bytes.Buffer
	require.NoError(t, cmd.RunForTest(t.Context(), []string{"library", "scan", src}, &buf))

	out := buf.String()
	assert.Contains(t, out, "encora id: 90100222")
	assert.Contains(t, out, "would-move")
}
