package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/ingest"
	"github.com/nicolerenee/promptbook/internal/server"
	"github.com/nicolerenee/promptbook/internal/stagemedia"
	"github.com/nicolerenee/promptbook/internal/storage"
	syncpkg "github.com/nicolerenee/promptbook/internal/sync"
)

const fixturesDir = "../encora/testdata"

// fixtureBackedServer returns a fully-wired *server.Server with a
// fixture-seeded SQLite cache behind it. Used for the API + page tests
// so each one starts from a known shape.
func fixtureBackedServer(t *testing.T) *server.Server {
	t.Helper()

	mux := http.NewServeMux()
	for path, file := range map[string]string{
		"/api/profile":    "profile.json",
		"/api/collection": "collection.json",
		"/api/wants":      "wants.json",
	} {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			b, err := os.ReadFile(filepath.Join(fixturesDir, file))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("X-Ratelimit-Remaining", "25")
			_, _ = w.Write(b)
		})
	}
	upstream := httptest.NewServer(mux)
	t.Cleanup(upstream.Close)

	sqlDB, db, err := storage.OpenEnt(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	c, err := encora.New(encora.Options{BaseURL: upstream.URL, APIKey: "test"})
	require.NoError(t, err)
	_, err = syncpkg.Sync(t.Context(), c, db, syncpkg.Options{BurstReserve: 2})
	require.NoError(t, err)

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)
	return srv
}

func TestAPIHealth(t *testing.T) {
	t.Parallel()

	srv := fixtureBackedServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/health", nil)
	srv.Handler().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var got map[string]string
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	assert.Equal(t, "ok", got["status"])
}

// TestSPAShell asserts the SPA migration's catch-all route serves the
// same Mithril shell for every browser-facing path, that the shell
// embeds the DaisyUI CDN link + the #app mount point Mithril needs,
// and that more-specific /api/v1/* routes still win over the catch-
// all (i.e. the JSON API is not shadowed by the SPA handler). A
// single test covers all three claims because they're tightly coupled
// — if the catch-all is registered wrong, all three break together.
func TestSPAShell(t *testing.T) {
	t.Parallel()

	srv := fixtureBackedServer(t)

	t.Run("root_serves_shell", func(t *testing.T) {
		t.Parallel()
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
		srv.Handler().ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		body := rr.Body.String()
		assert.Contains(t, body, `<div id="app"></div>`,
			"SPA shell must include the Mithril mount point")
		assert.Contains(t, body, "cdn.jsdelivr.net/npm/daisyui@5",
			"SPA shell must load DaisyUI from the CDN")
	})

	t.Run("recording_detail_serves_shell", func(t *testing.T) {
		t.Parallel()
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(
			t.Context(), http.MethodGet, "/recordings/123", nil)
		srv.Handler().ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		body := rr.Body.String()
		assert.Contains(t, body, `<div id="app"></div>`,
			"deep links must hit the same SPA shell — Mithril routes client-side")
	})

	t.Run("api_route_not_shadowed", func(t *testing.T) {
		t.Parallel()
		// /api/v1/health stays REST after the GraphQL migration; the
		// rest of the read surface moved to /graphql. Asserts the API
		// group still beats the SPA catch-all on prefix match.
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(
			t.Context(), http.MethodGet, "/api/v1/health", nil)
		srv.Handler().ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assert.Contains(t, rr.Header().Get("Content-Type"), "application/json",
			"API routes must beat the SPA catch-all on more-specific match")
	})
}

func TestServeStartCancels(t *testing.T) {
	t.Parallel()

	srv := fixtureBackedServer(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel before Start so it returns immediately
	require.NoError(t, srv.Start(ctx, "127.0.0.1:0"))
}
func TestServerStagemediaAccessor(t *testing.T) {
	t.Parallel()

	sqlDB, db, openErr := storage.OpenEnt(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, openErr)
	t.Cleanup(func() { _ = sqlDB.Close() })

	t.Run("nil when not configured", func(t *testing.T) {
		t.Parallel()
		srv, err := server.New(server.Options{DB: db})
		require.NoError(t, err)
		assert.Nil(t, srv.Stagemedia())
	})

	t.Run("returns the configured client", func(t *testing.T) {
		t.Parallel()
		// Sentinel: empty Client value is enough since the test only
		// verifies pointer-equality plumbing — no methods are invoked.
		sentinel := &stagemedia.Client{}
		srv, err := server.New(server.Options{DB: db, Stagemedia: sentinel})
		require.NoError(t, err)
		assert.Same(t, sentinel, srv.Stagemedia())
	})
}

// fakeStagemediaImageClient is a deterministic stand-in for
// stagemedia.Client that satisfies server.StagemediaImageClient. It
// records each call and returns a scripted Performers slice — enough
// to exercise the headshot-resolution branch of /api/v1/people/{id}
// without an httptest server. The mu/calls fields stay
// concurrency-safe so the race detector is happy.
//
// postersByShow maps show_id to the Posters slice to return for that
// show; nil means "empty Posters" so the picker tests can exercise
// the no-options path. posterCalls records the show ids the picker
// endpoints forwarded so tests can assert on the right show was hit.
type fakeStagemediaImageClient struct {
	mu          sync.Mutex
	calls       []fakeStagemediaCall
	posterCalls []int64
	// performers, when non-empty, is returned verbatim as the Images
	// payload. err takes precedence: when non-nil it short-circuits the
	// call before the recording is appended to calls.
	performers    []stagemedia.Performer
	postersByShow map[int64][]string
	err           error
}

// fakeStagemediaCall captures the arguments of one Images call so tests
// can verify the show id + performer id batch the server forwarded.
type fakeStagemediaCall struct {
	ShowID       int64
	PerformerIDs []int64
}

func (f *fakeStagemediaImageClient) Images(
	_ context.Context, showID int64, performerIDs []int64,
) (stagemedia.Images, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return stagemedia.Images{}, f.err
	}
	idsCopy := make([]int64, len(performerIDs))
	copy(idsCopy, performerIDs)
	f.calls = append(f.calls, fakeStagemediaCall{ShowID: showID, PerformerIDs: idsCopy})
	// When the test only set up posters for a given show, return them
	// alongside the performers slice so the show-poster-options +
	// recording-poster-options endpoints (which call Images with no
	// performer ids) get the scripted strip.
	posters := f.postersByShow[showID]
	// Track this as a poster fan-out when the caller passed no
	// performer ids OR the [1] sentinel — that's the calling pattern
	// picker-options uses (StageMedia rejects truly-empty actor_ids).
	isPosterFanOut := len(performerIDs) == 0 ||
		(len(performerIDs) == 1 && performerIDs[0] == 1)
	if isPosterFanOut {
		f.posterCalls = append(f.posterCalls, showID)
	}
	return stagemedia.Images{Performers: f.performers, Posters: posters}, nil
}

// Posters satisfies the StagemediaImageClient interface. The headshot
// tests don't exercise this branch so it returns an empty slice.
func (f *fakeStagemediaImageClient) Posters(
	_ context.Context, _ int64,
) ([]string, error) {
	return nil, f.err
}

func (f *fakeStagemediaImageClient) Calls() []fakeStagemediaCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeStagemediaCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// PosterCalls returns the show ids the server forwarded to Images
// when fetching posters (i.e. with an empty performer-id slice). Used
// by picker-options tests that assert the right show was queried.
func (f *fakeStagemediaImageClient) PosterCalls() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int64, len(f.posterCalls))
	copy(out, f.posterCalls)
	return out
}
func TestAPIHistoryEmpty(t *testing.T) {
	t.Parallel()

	sqlDB, db, err := storage.OpenEnt(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/history", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Empty(t, body.Items)
}

func TestAPIHistoryLists(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sqlDB, db, err := storage.OpenEnt(ctx, filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for _, e := range []storage.HistoryEvent{
		{OccurredAt: base.Add(1 * time.Hour), Kind: storage.HistoryKindIngest, Summary: "ingested foo"},
		{OccurredAt: base.Add(2 * time.Hour), Kind: storage.HistoryKindRename, Summary: "renamed foo"},
		{OccurredAt: base.Add(3 * time.Hour), Kind: storage.HistoryKindNFOWrite, Summary: "wrote nfo"},
	} {
		_, recErr := storage.RecordEvent(ctx, db, e)
		require.NoError(t, recErr)
	}

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/history", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		Items []struct {
			Kind    string `json:"kind"`
			Summary string `json:"summary"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	require.Len(t, body.Items, 3)
	// DESC by occurred_at: newest first.
	assert.Equal(t, storage.HistoryKindNFOWrite, body.Items[0].Kind)
	assert.Equal(t, storage.HistoryKindRename, body.Items[1].Kind)
	assert.Equal(t, storage.HistoryKindIngest, body.Items[2].Kind)
}

func TestAPIHistoryFilterByKind(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sqlDB, db, err := storage.OpenEnt(ctx, filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for _, e := range []storage.HistoryEvent{
		{OccurredAt: base.Add(1 * time.Hour), Kind: storage.HistoryKindIngest, Summary: "ingested foo"},
		{OccurredAt: base.Add(2 * time.Hour), Kind: storage.HistoryKindRename, Summary: "renamed foo"},
		{OccurredAt: base.Add(3 * time.Hour), Kind: storage.HistoryKindNFOWrite, Summary: "wrote nfo"},
	} {
		_, recErr := storage.RecordEvent(ctx, db, e)
		require.NoError(t, recErr)
	}

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/history?kind=ingest", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		Items []struct {
			Kind    string `json:"kind"`
			Summary string `json:"summary"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	require.Len(t, body.Items, 1)
	assert.Equal(t, storage.HistoryKindIngest, body.Items[0].Kind)
	assert.Equal(t, "ingested foo", body.Items[0].Summary)
}

func TestAPIHistoryFilterByRecordingID(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sqlDB, db, err := storage.OpenEnt(ctx, filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	rec42 := int64(42)
	rec99 := int64(99)
	for _, e := range []storage.HistoryEvent{
		{
			OccurredAt:  base.Add(1 * time.Hour),
			Kind:        storage.HistoryKindIngest,
			RecordingID: &rec42,
			Summary:     "ingested 42",
		},
		{
			OccurredAt:  base.Add(2 * time.Hour),
			Kind:        storage.HistoryKindRename,
			RecordingID: &rec99,
			Summary:     "renamed 99",
		},
		{
			OccurredAt: base.Add(3 * time.Hour),
			Kind:       storage.HistoryKindSync,
			Summary:    "global sync",
		},
	} {
		_, recErr := storage.RecordEvent(ctx, db, e)
		require.NoError(t, recErr)
	}

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	t.Run("matches", func(t *testing.T) {
		t.Parallel()
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/history?recording_id=42", nil)
		srv.Handler().ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

		var body struct {
			Items []struct {
				Kind        string `json:"kind"`
				Summary     string `json:"summary"`
				RecordingID *int64 `json:"recording_id"`
			} `json:"items"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
		require.Len(t, body.Items, 1)
		assert.Equal(t, storage.HistoryKindIngest, body.Items[0].Kind)
		assert.Equal(t, "ingested 42", body.Items[0].Summary)
		require.NotNil(t, body.Items[0].RecordingID)
		assert.Equal(t, int64(42), *body.Items[0].RecordingID)
	})

	t.Run("no_match", func(t *testing.T) {
		t.Parallel()
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/history?recording_id=12345", nil)
		srv.Handler().ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

		var body struct {
			Items []map[string]any `json:"items"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
		assert.Empty(t, body.Items)
	})

	t.Run("malformed_400", func(t *testing.T) {
		t.Parallel()
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/history?recording_id=abc", nil)
		srv.Handler().ServeHTTP(rr, req)
		assert.Equal(t, http.StatusBadRequest, rr.Code)
	})

	t.Run("zero_400", func(t *testing.T) {
		t.Parallel()
		rr := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/history?recording_id=0", nil)
		srv.Handler().ServeHTTP(rr, req)
		assert.Equal(t, http.StatusBadRequest, rr.Code)
	})
}

// seedMismatchRecording inserts a minimal recordings row plus the
// optional collection/wants/version rows the four-cases mismatch
// fixture exercises. Used by the mismatch test suite below.
func seedMismatchRecording(
	t *testing.T,
	db *ent.Client,
	showID, recordingID int64,
	showName string,
	inCollection, inWants, hasFile bool,
	encoraFormat, localFormat string,
) {
	t.Helper()
	ctx := t.Context()
	require.NoError(t, db.Show.Create().SetID(showID).SetName(showName).Exec(ctx))
	require.NoError(t, db.Recording.Create().
		SetID(recordingID).
		SetShowID(showID).
		SetRawJSON("{}").
		Exec(ctx))
	if inCollection {
		require.NoError(t, db.CollectionEntry.Create().
			SetID(recordingID).
			SetFormat(encoraFormat).
			Exec(ctx))
	}
	if inWants {
		require.NoError(t, db.WantsEntry.Create().SetID(recordingID).Exec(ctx))
	}
	if hasFile {
		require.NoError(t, storage.UpsertVersion(ctx, db, storage.RecordingVersion{
			RecordingID: recordingID,
			FilePath:    "/store/" + showName + ".mkv",
			FormatLabel: localFormat,
		}))
	}
}

func TestAPIMismatchesEmpty(t *testing.T) {
	t.Parallel()

	sqlDB, db, err := storage.OpenEnt(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/mismatches", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Empty(t, body.Items)
}

// TestAPIMismatchesEnumerates seeds four recordings — one synced, one
// orphan (file present, not in collection or wants), one in-collection
// without a backing file, and one wants-without-file — and asserts the
// endpoint returns exactly the three non-Synced rows.
func TestAPIMismatchesEnumerates(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sqlDB, db, err := storage.OpenEnt(ctx, filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	// synced: in collection + matching local format.
	seedMismatchRecording(t, db, 9101, 91001, "SyncedShow",
		true, false, true, "MKV 1080p", "MKV 1080p")
	// orphan: file present, not in collection or wants.
	seedMismatchRecording(t, db, 9102, 91002, "OrphanShow",
		false, false, true, "", "MKV 720p")
	// missing: in collection, no local file.
	seedMismatchRecording(t, db, 9103, 91003, "MissingShow",
		true, false, false, "MKV 1080p", "")
	// wanted: in wants, no local file.
	seedMismatchRecording(t, db, 9104, 91004, "WantedShow",
		false, true, false, "", "")

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/mismatches", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		Items []struct {
			Type        string `json:"type"`
			RecordingID int64  `json:"recording_id"`
			Show        string `json:"show"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	require.Len(t, body.Items, 3)

	byType := make(map[string]int64, len(body.Items))
	for _, item := range body.Items {
		byType[item.Type] = item.RecordingID
	}
	assert.Equal(t, int64(91002), byType["add_to_collection"])
	assert.Equal(t, int64(91003), byType["missing_file"])
	assert.Equal(t, int64(91004), byType["wanted_file"])
}

// TestAPIMismatchesFilterByType seeds the same four-case fixture and
// asserts ?type=add_to_collection narrows the response to just the
// orphan row.
func TestAPIMismatchesFilterByType(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sqlDB, db, err := storage.OpenEnt(ctx, filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	seedMismatchRecording(t, db, 9201, 92001, "SyncedShow",
		true, false, true, "MKV 1080p", "MKV 1080p")
	seedMismatchRecording(t, db, 9202, 92002, "OrphanShow",
		false, false, true, "", "MKV 720p")
	seedMismatchRecording(t, db, 9203, 92003, "MissingShow",
		true, false, false, "MKV 1080p", "")
	seedMismatchRecording(t, db, 9204, 92004, "WantedShow",
		false, true, false, "", "")

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet,
		"/api/v1/mismatches?type=add_to_collection", nil)
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		Items []struct {
			Type        string `json:"type"`
			RecordingID int64  `json:"recording_id"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	require.Len(t, body.Items, 1)
	assert.Equal(t, "add_to_collection", body.Items[0].Type)
	assert.Equal(t, int64(92002), body.Items[0].RecordingID)
}

// (Per-page shell tests for mismatches / recording detail were retired
// when the SPA migration replaced server-rendered shells with the
// single Mithril shell. The new TestSPAShell covers the catch-all
// behavior; data contracts still live in the API tests
// TestAPIMismatchesEnumerates / TestAPIRecordingByID etc.)

// stubEncoraClient is a minimal in-memory implementation of the
// EncoraWriteClient interface used by the apply tests. It records each
// call so assertions can verify the right endpoint fired with the right
// arguments, and lets each test slot a per-call response (success or
// sentinel error) into the queue.
type stubEncoraClient struct {
	mu                sync.Mutex
	addCalls          []int64
	formatCalls       []stubFormatCall
	addErr            error
	formatErr         error
	addRateLimit      encora.RateLimitInfo
	formatRateLimit   encora.RateLimitInfo
	updateCallCounter int
	// addResponses, when non-empty, supplies per-call (rate-limit,
	// error) responses in FIFO order; once drained, the stub falls back
	// to the addErr/addRateLimit defaults. Used by the retry-after test
	// to script a 429 followed by a success on the next call.
	addResponses []stubResponse
	// observedAddCtxCancelled captures whether the context handed to
	// AddToCollection was already cancelled at call time. Lets the
	// detached-context test verify pushCtx survives a request-context
	// cancel.
	observedAddCtxCancelled []bool
	// onAdd, when non-nil, is invoked at the start of every
	// AddToCollection call before the response is returned. Lets a
	// test cancel the request context mid-batch so the next iteration
	// of the apply loop can be observed receiving a detached context.
	onAdd func()
}

// stubResponse is one scripted (rate-limit, error) reply consumed in
// FIFO order from stubEncoraClient.addResponses.
type stubResponse struct {
	rl  encora.RateLimitInfo
	err error
}

type stubFormatCall struct {
	ID     int64
	Format string
}

func (s *stubEncoraClient) AddToCollection(ctx context.Context, id int64) (encora.RateLimitInfo, error) {
	s.mu.Lock()
	hook := s.onAdd
	s.addCalls = append(s.addCalls, id)
	s.observedAddCtxCancelled = append(s.observedAddCtxCancelled, ctx.Err() != nil)
	var resp stubResponse
	used := false
	if len(s.addResponses) > 0 {
		resp = s.addResponses[0]
		s.addResponses = s.addResponses[1:]
		used = true
	}
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	if used {
		return resp.rl, resp.err
	}
	return s.addRateLimit, s.addErr
}

func (s *stubEncoraClient) UpdateCollectionFormat(
	_ context.Context, id int64, format string,
) (encora.RateLimitInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.formatCalls = append(s.formatCalls, stubFormatCall{ID: id, Format: format})
	s.updateCallCounter++
	return s.formatRateLimit, s.formatErr
}

// applyTestServer wires a fresh DB + stub encora client into a server
// and returns both so tests can assert on history rows + recorded
// upstream calls without the fixture-backed sync.
func applyTestServer(t *testing.T) (*server.Server, *ent.Client, *stubEncoraClient) {
	t.Helper()
	sqlDB, db, err := storage.OpenEnt(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	stub := &stubEncoraClient{}
	srv, err := server.New(server.Options{DB: db, Encora: stub})
	require.NoError(t, err)
	return srv, db, stub
}

func TestAPIApplyHandlesAddToCollection(t *testing.T) {
	t.Parallel()
	srv, db, stub := applyTestServer(t)

	// Seed an orphan: file present, not in collection or wants. The new
	// validation pass requires the action to match the live mismatch
	// oracle, so without this seed loadMismatches returns an empty set
	// and the action would be rejected before ever reaching the stub.
	seedMismatchRecording(t, db, 99001, 12345, "OrphanShow",
		false, false, true, "", "MKV 1080p")

	body, err := json.Marshal(map[string]any{
		"actions": []map[string]any{
			{"type": "add_to_collection", "recording_id": 12345},
		},
	})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/apply", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp struct {
		Results []struct {
			OK         bool   `json:"ok"`
			Error      string `json:"error"`
			HTTPStatus int    `json:"http_status"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Results, 1)
	assert.True(t, resp.Results[0].OK, "expected ok=true; got error %q", resp.Results[0].Error)
	assert.Equal(t, http.StatusOK, resp.Results[0].HTTPStatus)

	assert.Equal(t, []int64{12345}, stub.addCalls,
		"AddToCollection should be invoked exactly once with the supplied id")

	// History event must be written for successful pushes so the audit
	// log captures every change the server made upstream.
	events, err := storage.ListHistory(t.Context(), db, storage.ListHistoryOptions{
		Kinds: []string{storage.HistoryKindEncoraPush},
	})
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, storage.HistoryKindEncoraPush, events[0].Kind)
	require.NotNil(t, events[0].RecordingID)
	assert.Equal(t, int64(12345), *events[0].RecordingID)
	assert.Contains(t, events[0].Summary, "12345")
}

func TestAPIApplyHandlesFormatMismatch(t *testing.T) {
	t.Parallel()
	srv, db, stub := applyTestServer(t)

	// Seed a format mismatch: in collection with encoraFormat differing
	// from the local file's FormatLabel. The validation pass also
	// requires the submitted NewFormat to match the local oracle, so we
	// make local FormatLabel == "MKV 1080p" (the value the test pushes)
	// and Encora's recorded format something different.
	seedMismatchRecording(t, db, 99002, 90100222, "FormatShow",
		true, false, true, "MKV 720p", "MKV 1080p")

	body, err := json.Marshal(map[string]any{
		"actions": []map[string]any{
			{
				"type":         "format_mismatch",
				"recording_id": 90100222,
				"new_format":   "MKV 1080p",
			},
		},
	})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/apply", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	require.Len(t, stub.formatCalls, 1, "UpdateCollectionFormat should fire once")
	assert.Equal(t, int64(90100222), stub.formatCalls[0].ID)
	assert.Equal(t, "MKV 1080p", stub.formatCalls[0].Format,
		"format string must be forwarded as-is to the encora client")

	events, err := storage.ListHistory(t.Context(), db, storage.ListHistoryOptions{
		Kinds: []string{storage.HistoryKindEncoraPush},
	})
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Contains(t, events[0].Summary, "MKV 1080p")
}

func TestAPIApplyMissingFileShortCircuits(t *testing.T) {
	t.Parallel()
	srv, db, stub := applyTestServer(t)

	// Seed a missing-file mismatch: in collection but no local version
	// row. The validation pass needs the action to match the live state.
	seedMismatchRecording(t, db, 99003, 99, "MissingShow",
		true, false, false, "MKV 1080p", "")

	body, err := json.Marshal(map[string]any{
		"actions": []map[string]any{
			{"type": "missing_file", "recording_id": 99},
		},
	})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/apply", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp struct {
		Results []struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Results, 1)
	assert.False(t, resp.Results[0].OK)
	assert.Contains(t, resp.Results[0].Error, "requires a downloaded file")

	assert.Empty(t, stub.addCalls, "encora client must not be called for missing_file")
	assert.Empty(t, stub.formatCalls, "encora client must not be called for missing_file")

	// No history row should be written for a non-push.
	events, err := storage.ListHistory(t.Context(), db, storage.ListHistoryOptions{
		Kinds: []string{storage.HistoryKindEncoraPush},
	})
	require.NoError(t, err)
	assert.Empty(t, events)
}

func TestAPIApplyWithoutEncoraClient(t *testing.T) {
	t.Parallel()

	sqlDB, db, err := storage.OpenEnt(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	body, err := json.Marshal(map[string]any{
		"actions": []map[string]any{
			{"type": "add_to_collection", "recording_id": 1},
		},
	})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/apply", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req)

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "encora client not configured")
}

func TestAPIApplySurfacesEncoraError(t *testing.T) {
	t.Parallel()
	srv, db, stub := applyTestServer(t)
	stub.addErr = encora.ErrRateLimited
	stub.addRateLimit = encora.RateLimitInfo{RetryAfter: 60 * time.Second}

	// Seed an orphan so the validation pass lets the action through to
	// applyOne where the rate-limit error surfaces.
	seedMismatchRecording(t, db, 99004, 7, "ErrShow",
		false, false, true, "", "MKV 1080p")

	body, err := json.Marshal(map[string]any{
		"actions": []map[string]any{
			{"type": "add_to_collection", "recording_id": 7},
		},
	})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/apply", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp struct {
		Results []struct {
			OK         bool   `json:"ok"`
			Error      string `json:"error"`
			HTTPStatus int    `json:"http_status"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Results, 1)
	assert.False(t, resp.Results[0].OK)
	assert.Contains(t, strings.ToLower(resp.Results[0].Error), "rate limited")
	assert.Equal(t, http.StatusTooManyRequests, resp.Results[0].HTTPStatus)

	// Failure means no history row — the server only logs successful
	// pushes so the audit trail isn't polluted with non-events.
	events, err := storage.ListHistory(t.Context(), db, storage.ListHistoryOptions{
		Kinds: []string{storage.HistoryKindEncoraPush},
	})
	require.NoError(t, err)
	assert.Empty(t, events,
		"failed pushes must not produce HistoryKindEncoraPush events")
}

// applyTestServerWithSleeper is applyTestServer's sibling that lets a
// test inject a recording sleeper so the Retry-After honor logic stays
// asserrtable without spending wall-clock time. Returns the recorded
// durations slice (locked behind a mutex) the caller can read after
// the request returns.
func applyTestServerWithSleeper(
	t *testing.T,
) (*server.Server, *ent.Client, *stubEncoraClient, *sleepRecorder) {
	t.Helper()
	sqlDB, db, err := storage.OpenEnt(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	stub := &stubEncoraClient{}
	rec := &sleepRecorder{}
	srv, err := server.New(server.Options{DB: db, Encora: stub, Sleeper: rec.Sleep})
	require.NoError(t, err)
	return srv, db, stub, rec
}

// sleepRecorder captures every duration handed to Sleep without
// blocking. Concurrency-safe so the apply batch driver can call into it
// from any goroutine the echo runtime spins up.
type sleepRecorder struct {
	mu        sync.Mutex
	durations []time.Duration
}

func (r *sleepRecorder) Sleep(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.durations = append(r.durations, d)
}

func (r *sleepRecorder) Calls() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]time.Duration, len(r.durations))
	copy(out, r.durations)
	return out
}

// TestAPIApplyRejectsStaleAction asserts the validation pass refuses to
// forward an action whose (type, recording_id) pair isn't in the live
// mismatch oracle — e.g. someone POSTs an add_to_collection for a
// recording that's already in the collection. The encora client must
// not be invoked, and no history row should be written.
func TestAPIApplyRejectsStaleAction(t *testing.T) {
	t.Parallel()
	srv, db, stub := applyTestServer(t)

	// Seed a recording in StatusSynced — collected, with a matching
	// local file, so loadMismatches returns zero rows for it.
	seedMismatchRecording(t, db, 99100, 90004242, "SyncedShow",
		true, false, true, "MKV 1080p", "MKV 1080p")

	body, err := json.Marshal(map[string]any{
		"actions": []map[string]any{
			{"type": "add_to_collection", "recording_id": 90004242},
		},
	})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/apply", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp struct {
		Results []struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Results, 1)
	assert.False(t, resp.Results[0].OK,
		"action against a synced recording must be rejected")
	assert.Contains(t, resp.Results[0].Error, "no longer applies",
		"rejection reason should mention the stale-state guard")

	assert.Empty(t, stub.addCalls,
		"encora client must not be invoked for a stale action")
	assert.Empty(t, stub.formatCalls,
		"encora client must not be invoked for a stale action")

	events, err := storage.ListHistory(t.Context(), db, storage.ListHistoryOptions{
		Kinds: []string{storage.HistoryKindEncoraPush},
	})
	require.NoError(t, err)
	assert.Empty(t, events,
		"a stale-action rejection must not produce an encora_push history row")
}

// TestAPIApplyRejectsTamperedFormat asserts that a FormatMismatch action
// whose NewFormat doesn't match the live LocalFormat oracle is refused
// before the encora client is touched. Without this guard a malicious
// or stale form could push arbitrary format strings.
func TestAPIApplyRejectsTamperedFormat(t *testing.T) {
	t.Parallel()
	srv, db, stub := applyTestServer(t)

	// Seed a format mismatch with local format MKV 1080p; the test
	// submits NewFormat=MKV 4K instead, simulating a tampered form.
	seedMismatchRecording(t, db, 99101, 5151, "TamperShow",
		true, false, true, "MKV 720p", "MKV 1080p")

	body, err := json.Marshal(map[string]any{
		"actions": []map[string]any{
			{
				"type":         "format_mismatch",
				"recording_id": 5151,
				"new_format":   "MKV 4K",
			},
		},
	})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/apply", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp struct {
		Results []struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Results, 1)
	assert.False(t, resp.Results[0].OK)
	assert.Contains(t, strings.ToLower(resp.Results[0].Error), "format")

	assert.Empty(t, stub.formatCalls,
		"a tampered format must not reach the encora client")
}

// TestAPIApplyHonorsRetryAfter scripts a 429 (RetryAfter=2s) on the
// first action and a success on the second, asserts both actions
// complete in a single response, and verifies the sleeper was called
// with the parsed RetryAfter before the second call fired. This is the
// regression fence against "one 429 cascades into N more 429s" because
// nothing slept between calls.
func TestAPIApplyHonorsRetryAfter(t *testing.T) {
	t.Parallel()
	srv, db, stub, sleepRec := applyTestServerWithSleeper(t)

	// Two orphan recordings so two add_to_collection actions can both
	// pass validation and go into the apply loop.
	seedMismatchRecording(t, db, 99200, 8001, "RetryShowA",
		false, false, true, "", "MKV 1080p")
	seedMismatchRecording(t, db, 99201, 8002, "RetryShowB",
		false, false, true, "", "MKV 1080p")

	stub.addResponses = []stubResponse{
		{rl: encora.RateLimitInfo{RetryAfter: 2 * time.Second}, err: encora.ErrRateLimited},
		{rl: encora.RateLimitInfo{}, err: nil},
	}

	body, err := json.Marshal(map[string]any{
		"actions": []map[string]any{
			{"type": "add_to_collection", "recording_id": 8001},
			{"type": "add_to_collection", "recording_id": 8002},
		},
	})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/apply", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp struct {
		Results []struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Results, 2)
	assert.False(t, resp.Results[0].OK,
		"first action should report the 429")
	assert.Contains(t, strings.ToLower(resp.Results[0].Error), "rate limited")
	assert.True(t, resp.Results[1].OK,
		"second action should still fire after the Retry-After sleep")

	assert.Equal(t, []int64{8001, 8002}, stub.addCalls,
		"both actions should reach the encora client")

	calls := sleepRec.Calls()
	require.Len(t, calls, 1,
		"sleeper should fire exactly once between the 429 and the second action")
	assert.Equal(t, 2*time.Second, calls[0],
		"sleeper should honor the parsed Retry-After")
}

// TestAPIApplyDetachedContext asserts the apply push loop runs against
// a context that survives a cancellation of the request context. We
// fire two actions and let the stub's onAdd hook cancel the request
// context during the first call. The second call's observed context
// state must still be non-cancelled — proving context.WithoutCancel
// was applied to the push loop. Validation runs first against the
// (still-live) request context, which is why we cancel from inside
// the stub instead of pre-cancelling: a pre-cancel would surface as a
// 500 from the local DB driver before the loop could even start.
func TestAPIApplyDetachedContext(t *testing.T) {
	t.Parallel()
	srv, db, stub := applyTestServer(t)

	seedMismatchRecording(t, db, 99300, 6001, "DetachShowA",
		false, false, true, "", "MKV 1080p")
	seedMismatchRecording(t, db, 99301, 6002, "DetachShowB",
		false, false, true, "", "MKV 1080p")

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	// Cancel the request context synchronously during the first
	// upstream call. The hook fires only on the first AddToCollection
	// because the test reuses cancel()'s idempotency.
	stub.onAdd = cancel

	body, err := json.Marshal(map[string]any{
		"actions": []map[string]any{
			{"type": "add_to_collection", "recording_id": 6001},
			{"type": "add_to_collection", "recording_id": 6002},
		},
	})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost,
		"/api/v1/apply", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req)

	// The handler ran to completion because pushCtx is detached from
	// the cancelled request context.
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	require.Equal(t, []int64{6001, 6002}, stub.addCalls,
		"both upstream calls should fire even after the request context was cancelled mid-batch")
	require.Len(t, stub.observedAddCtxCancelled, 2)
	for i, cancelled := range stub.observedAddCtxCancelled {
		assert.False(t, cancelled,
			"call %d: encora client should receive a non-cancelled context (detached)", i)
	}
}

// stubIngestRunner records the (src, opts) tuple of every Ingest call
// and returns a scripted ingest.Result. Used by the importQueueEntry
// GraphQL mutation tests so the resolver runs without spinning up the
// real engine (which would require a live Encora client + library
// config).
type stubIngestRunner struct {
	mu    sync.Mutex
	calls []stubIngestCall
	// nextResult, when non-nil, is returned verbatim from the next
	// Ingest call. Defaults to a single moved item with the canonical
	// dest path baked in.
	nextResult *ingest.Result
	// nextErr, when non-nil, is returned as the engine-level error.
	nextErr error
}

type stubIngestCall struct {
	Src  string
	Opts ingest.Options
}

func (s *stubIngestRunner) Ingest(_ context.Context, src string, opts ingest.Options) (*ingest.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, stubIngestCall{Src: src, Opts: opts})
	if s.nextErr != nil {
		return nil, s.nextErr
	}
	if s.nextResult != nil {
		return s.nextResult, nil
	}
	// Default: report a successful move with no plan attached. The
	// handler tolerates a nil Plan (dest stays empty in the response),
	// which is fine for tests that only assert the success path.
	return &ingest.Result{
		Items: []ingest.ItemResult{{
			Source:   src,
			EncoraID: int64(opts.FlagEncoraID),
			Action:   ingest.ActionMoved,
		}},
	}, nil
}

// queueImportTestServer wires a fresh DB + stub ingest runner into a
// server and returns both. Mirrors applyTestServer for the
// importQueueEntry GraphQL mutation path. Pass a nil runner to
// simulate the not-configured wiring.
func queueImportTestServer(t *testing.T, runner server.IngestRunner) (*server.Server, *ent.Client) {
	t.Helper()
	sqlDB, db, err := storage.OpenEnt(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	srv, err := server.New(server.Options{DB: db, IngestEngine: runner})
	require.NoError(t, err)
	return srv, db
}
