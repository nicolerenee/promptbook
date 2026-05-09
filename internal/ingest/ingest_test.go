package ingest_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/ingest"
	"github.com/nicolerenee/promptbook/internal/rename"
	"github.com/nicolerenee/promptbook/internal/storage"
	syncpkg "github.com/nicolerenee/promptbook/internal/sync"
)

const fixturesDir = "../encora/testdata"

func newFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	routes := map[string]string{
		"/api/profile":                  "profile.json",
		"/api/collection":               "collection.json",
		"/api/wants":                    "wants.json",
		"/api/recording/90100222/subtitles": "recording_8222_subtitles.json",
	}
	mux := http.NewServeMux()
	for path, file := range routes {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			b, err := os.ReadFile(filepath.Join(fixturesDir, file))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("X-RateLimit-Limit", "30")
			w.Header().Set("X-RateLimit-Remaining", "25")
			_, _ = w.Write(b)
		})
	}
	return httptest.NewServer(mux)
}

// staticSubtitleServer serves any GET /<token>.srt with a known body.
// Used to verify HTTPSubtitleFetcher writes files end-to-end.
func staticSubtitleServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
}

// stubFetcher records what was asked for without touching the network.
type stubFetcher struct {
	called   bool
	folder   string
	written  []string
	failNext bool
}

func (s *stubFetcher) Fetch(
	_ context.Context,
	_ ingest.Client,
	_ encora.Recording,
	plan rename.Plan,
) ([]string, error) {
	s.called = true
	s.folder = plan.AbsoluteFolder()
	if s.failNext {
		return nil, errors.New("simulated fetch failure")
	}
	dest := filepath.Join(plan.AbsoluteFolder(), plan.TargetFile+".eng.srt")
	if err := os.WriteFile(dest, []byte("subtitle"), 0o644); err != nil {
		return nil, err
	}
	s.written = []string{dest}
	return s.written, nil
}

// stubClient implements ingest.Client without ever calling out.
type stubClient struct {
	subs            []encora.Subtitle
	addCalledForIDs []int64
}

func (s *stubClient) Subtitles(_ context.Context, _ int64) ([]encora.Subtitle, encora.RateLimitInfo, error) {
	return s.subs, encora.RateLimitInfo{Remaining: 30}, nil
}

func (s *stubClient) AddToCollection(_ context.Context, id int64) (encora.RateLimitInfo, error) {
	s.addCalledForIDs = append(s.addCalledForIDs, id)
	return encora.RateLimitInfo{Remaining: 30}, nil
}

// seededDBPath spins up a tempdir, runs the fixture-backed sync, and
// returns the path to the resulting SQLite file. Tests reopen the DB
// with their own context.
func seededDBPath(t *testing.T) string {
	t.Helper()
	srv := newFixtureServer(t)
	t.Cleanup(srv.Close)

	dbPath := filepath.Join(t.TempDir(), "promptbook.db")
	db, err := storage.Open(t.Context(), dbPath)
	require.NoError(t, err)

	c, err := encora.New(encora.Options{BaseURL: srv.URL, APIKey: "test"})
	require.NoError(t, err)
	_, err = syncpkg.Sync(t.Context(), c, db, syncpkg.Options{BurstReserve: 2})
	require.NoError(t, err)
	require.NoError(t, db.Close())

	return dbPath
}

func TestEngineIngestSingleFileDryRun(t *testing.T) {
	t.Parallel()

	dbPath := seededDBPath(t)
	db, err := storage.Open(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "Marigold [encora-90100222].mp4")
	require.NoError(t, os.WriteFile(src, []byte("video"), 0o644))

	libRoot := filepath.Join(t.TempDir(), "library")
	engine := &ingest.Engine{
		DB:             db,
		Client:         &stubClient{},
		LibraryRoot:    libRoot,
		FolderTemplate: "{Show} - {Tour} - {Date} [encora-{EncoraID}]",
		FileTemplate:   "{Show} - {Tour} - {Date} [{Master}]",
	}

	res, err := engine.Ingest(t.Context(), src, ingest.Options{DryRun: true})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	item := res.Items[0]
	assert.Equal(t, "would-move", item.Action)
	assert.Equal(t, int64(90100222), item.EncoraID)
	require.NotNil(t, item.Plan)

	// Source file must still exist; nothing in dest.
	_, err = os.Stat(src)
	require.NoError(t, err)
	_, err = os.Stat(item.Plan.AbsoluteFile())
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestEngineIngestRealMove(t *testing.T) {
	t.Parallel()

	dbPath := seededDBPath(t)
	db, err := storage.Open(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	srcDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, ".encora-id"), []byte("90100222\n"), 0o644))
	src := filepath.Join(srcDir, "Random Filename.mp4")
	require.NoError(t, os.WriteFile(src, []byte("video bytes"), 0o644))

	libRoot := filepath.Join(t.TempDir(), "library")
	fetcher := &stubFetcher{}
	engine := &ingest.Engine{
		DB:              db,
		Client:          &stubClient{},
		LibraryRoot:     libRoot,
		FolderTemplate:  "{Show} - {Tour} - {Date} [encora-{EncoraID}]",
		FileTemplate:    "{Show} - {Tour} - {Date} [{Master}]",
		SubtitleFetcher: fetcher,
	}

	res, err := engine.Ingest(t.Context(), src, ingest.Options{})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	item := res.Items[0]
	assert.Equal(t, "moved", item.Action)
	require.NoError(t, item.Err)

	// Destination got the bytes; source is gone.
	got, err := os.ReadFile(item.Plan.AbsoluteFile())
	require.NoError(t, err)
	assert.Equal(t, "video bytes", string(got))
	_, err = os.Stat(src)
	assert.True(t, os.IsNotExist(err))

	// Sidecar should be in place for re-runs.
	_, err = os.Stat(filepath.Join(item.Plan.AbsoluteFolder(), rename.SidecarFilename))
	require.NoError(t, err)

	// NFO written.
	_, err = os.Stat(item.NFOPath)
	require.NoError(t, err)

	// Marigold's fixture has has_subtitles=true, so the fetcher must be
	// called and at least one path must come back.
	assert.True(t, fetcher.called, "marigold fixture has has_subtitles=true")
	assert.NotEmpty(t, fetcher.written)
}

func TestEngineIngestSkipsUnknownID(t *testing.T) {
	t.Parallel()

	dbPath := seededDBPath(t)
	db, err := storage.Open(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	src := filepath.Join(t.TempDir(), "video.mp4")
	require.NoError(t, os.WriteFile(src, []byte("v"), 0o644))

	engine := &ingest.Engine{
		DB:             db,
		Client:         &stubClient{},
		LibraryRoot:    t.TempDir(),
		FolderTemplate: "x",
		FileTemplate:   "y",
	}

	res, err := engine.Ingest(t.Context(), src, ingest.Options{})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	assert.Equal(t, "skipped", res.Items[0].Action)
	assert.NotEmpty(t, res.Items[0].SkippedReason)
}

func TestEngineAddToCollectionMockOnly(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "promptbook.db")
	db, err := storage.Open(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	src := filepath.Join(t.TempDir(), "Show [encora-99999].mp4")
	require.NoError(t, os.WriteFile(src, []byte("v"), 0o644))

	stub := &stubClient{}
	engine := &ingest.Engine{
		DB:             db,
		Client:         stub,
		LibraryRoot:    t.TempDir(),
		FolderTemplate: "x",
		FileTemplate:   "y",
	}

	res, err := engine.Ingest(t.Context(), src, ingest.Options{AddToCollection: true})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	item := res.Items[0]
	assert.Equal(t, "skipped", item.Action)
	require.Len(t, stub.addCalledForIDs, 1, "should have invoked AddToCollection on the mock client")
	assert.Equal(t, int64(99999), stub.addCalledForIDs[0])
}

func TestEngineIngestDirectoryWalk(t *testing.T) {
	t.Parallel()

	dbPath := seededDBPath(t)
	db, err := storage.Open(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	srcRoot := t.TempDir()
	for _, name := range []string{"Marigold [encora-90100222].mp4", "skip-me.txt", "subdir/Other [encora-90001143].mkv"} {
		dest := filepath.Join(srcRoot, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o755))
		require.NoError(t, os.WriteFile(dest, []byte("v"), 0o644))
	}

	engine := &ingest.Engine{
		DB:             db,
		Client:         &stubClient{},
		LibraryRoot:    t.TempDir(),
		FolderTemplate: "{Show} [encora-{EncoraID}]",
		FileTemplate:   "{Show}",
	}

	res, err := engine.Ingest(t.Context(), srcRoot, ingest.Options{DryRun: true})
	require.NoError(t, err)
	require.Len(t, res.Items, 2, ".txt should be skipped")

	ids := []int64{res.Items[0].EncoraID, res.Items[1].EncoraID}
	assert.ElementsMatch(t, []int64{90100222, 90001143}, ids)
}

func TestHTTPSubtitleFetcher(t *testing.T) {
	t.Parallel()

	srv := staticSubtitleServer(t, "subtitle bytes")
	t.Cleanup(srv.Close)

	encoraSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Return two English subtitles + one Turkish to exercise
		// multi-author-disambiguation.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
		  {"recording_id":1,"language":"English","author":"Unknown","file_type":"SRT","coverage":"complete","url":"` + srv.URL + `/a"},
		  {"recording_id":1,"language":"English","author":"erdist","file_type":"SRT","coverage":"complete","url":"` + srv.URL + `/b"},
		  {"recording_id":1,"language":"Turkish","author":"erdist","file_type":"SRT","coverage":"complete","url":"` + srv.URL + `/c"}
		]`))
	}))
	t.Cleanup(encoraSrv.Close)

	c, err := encora.New(encora.Options{BaseURL: encoraSrv.URL, APIKey: "k"})
	require.NoError(t, err)

	dir := t.TempDir()
	plan := rename.Plan{
		LibraryRoot:  dir,
		TargetFolder: "out",
		TargetFile:   "Show",
		Extension:    ".mp4",
	}
	require.NoError(t, os.MkdirAll(plan.AbsoluteFolder(), 0o755))

	fetcher := &ingest.HTTPSubtitleFetcher{HTTP: srv.Client()}
	rec := encora.Recording{
		ID:       1,
		Metadata: encora.RecordingMeta{HasSubtitles: true},
	}
	written, err := fetcher.Fetch(t.Context(), c, rec, plan)
	require.NoError(t, err)
	require.Len(t, written, 3)

	// English entries get author-disambiguated, Turkish does not.
	for _, p := range written {
		_, statErr := os.Stat(p)
		require.NoError(t, statErr, "expected %s on disk", p)
	}
	assert.Contains(t, written[0], "english.")
	assert.Contains(t, written[1], "english.")

	expectedAuthor := "english.Unknown.srt"
	hasUnknown := false
	for _, p := range written {
		if filepath.Base(p) == "Show."+expectedAuthor {
			hasUnknown = true
		}
	}
	assert.True(t, hasUnknown, "expected one english subtitle to include Unknown author suffix")
}
