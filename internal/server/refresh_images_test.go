package server_test

// refresh_images_test.go — covers the per-entity "Refresh from
// upstream" endpoints. Each handler enqueues a background job; we
// register a stub job under the expected name and assert the args
// the handler forwarded.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/jobs"
	"github.com/nicolerenee/promptbook/internal/server"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// eventuallyTimeout / eventuallyTick bound the require.Eventually
// polls. The runner ticks once per second so 5s gives the worker
// pool ample time to pick up an enqueued run; 50ms keeps the polling
// cheap.
const (
	eventuallyTimeout = 5 * time.Second
	eventuallyTick    = 50 * time.Millisecond
)

// recordingJobStub captures invocations of the stubbed
// refresh-recording-images / refresh-show-images jobs so tests can
// assert on the args the handler forwarded.
type recordingJobStub struct {
	name string
	mu   sync.Mutex
	args []jobs.JobArgs
}

func (s *recordingJobStub) Name() string { return s.name }
func (s *recordingJobStub) Run(_ context.Context, args jobs.JobArgs) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.args = append(s.args, args)
	return nil
}
func (s *recordingJobStub) buttonshot() []jobs.JobArgs {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]jobs.JobArgs, len(s.args))
	copy(out, s.args)
	return out
}

// refreshTestServer wires an in-memory DB + a runner with the named
// refresh jobs registered. Returns the server, runner, and the two
// stubs so tests can inspect the args the handler forwarded.
func refreshTestServer(
	t *testing.T, recordingID, showID int64,
) (*server.Server, *jobs.Runner, *recordingJobStub, *recordingJobStub) {
	t.Helper()
	ctx := t.Context()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.ExecContext(ctx,
		`INSERT INTO shows (show_id, name) VALUES (?, ?)`, showID, "RefreshShow")
	require.NoError(t, err)
	rawJSON, err := json.Marshal(map[string]any{
		"id": recordingID, "show": "RefreshShow",
		"metadata": map[string]any{"show_id": showID},
	})
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO recordings (recording_id, show_id, tour, date_full, raw_json)
		VALUES (?, ?, '', '', ?)
	`, recordingID, showID, string(rawJSON))
	require.NoError(t, err)

	runner := jobs.New(jobs.Options{
		DB: db, Logger: zerolog.Nop(), Workers: 1,
	})
	recStub := &recordingJobStub{name: "refresh-recording-images"}
	showStub := &recordingJobStub{name: "refresh-show-images"}
	require.NoError(t, runner.Register(jobs.JobDef{Job: recStub}))
	require.NoError(t, runner.Register(jobs.JobDef{Job: showStub}))

	srv, err := server.New(server.Options{DB: db, JobRunner: runner})
	require.NoError(t, err)
	return srv, runner, recStub, showStub
}

func TestRefreshRecordingImagesEnqueuesJob(t *testing.T) {
	t.Parallel()
	const recID, showID int64 = 9101, 7301
	srv, runner, recStub, showStub := refreshTestServer(t, recID, showID)

	// Start the runner so the queued run actually executes — that's
	// the only way to read back the args the handler forwarded.
	ctx, cancel := context.WithCancel(t.Context())
	go func() { _ = runner.Start(ctx) }()
	defer cancel()

	body := bytes.NewBufferString(`{"force": true}`)
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/recordings/9101/refresh-images", body)
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp struct {
		RunID int64 `json:"run_id"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Positive(t, resp.RunID, "response must echo the enqueued run_id")

	require.Eventually(t, func() bool {
		return len(recStub.buttonshot()) > 0
	}, eventuallyTimeout, eventuallyTick,
		"refresh-recording-images must execute after the endpoint enqueues it")

	args := recStub.buttonshot()[0]
	assert.Equal(t, recID, args.GetInt64("recording_id"))
	assert.True(t, args.GetBool("force"),
		"force=true on the request body must round-trip into the job args")
	assert.Empty(t, showStub.buttonshot(),
		"refresh-show-images must not run when only the recording endpoint fires")
}

func TestRefreshShowImagesEnqueuesJob(t *testing.T) {
	t.Parallel()
	const recID, showID int64 = 9102, 7302
	srv, runner, _, showStub := refreshTestServer(t, recID, showID)

	// Start the runner so the queued run actually executes and we can
	// observe the args the stub captured.
	ctx, cancel := context.WithCancel(t.Context())
	go func() { _ = runner.Start(ctx) }()
	defer cancel()

	body := bytes.NewBufferString(`{"force": true}`)
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/shows/7302/refresh-images", body)
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	// Wait for the worker to pick up the run. The runner ticks once
	// per second, so we poll for up to ~3s before bailing.
	require.Eventually(t, func() bool {
		return len(showStub.buttonshot()) > 0
	}, eventuallyTimeout, eventuallyTick,
		"refresh-show-images job must be invoked after the endpoint enqueues it")

	args := showStub.buttonshot()[0]
	assert.Equal(t, showID, args.GetInt64("show_id"))
	assert.True(t, args.GetBool("force"),
		"force=true on the request body must round-trip into the job args")
}

func TestRefreshRecordingImages404OnUnknown(t *testing.T) {
	t.Parallel()
	const recID, showID int64 = 9103, 7303
	srv, _, _, _ := refreshTestServer(t, recID, showID)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/recordings/99999999/refresh-images", nil)
	srv.Handler().ServeHTTP(rr, req)
	assert.Equal(t, http.StatusNotFound, rr.Code, rr.Body.String())
}

func TestRefreshImages503WhenNoRunner(t *testing.T) {
	t.Parallel()
	db, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/recordings/1/refresh-images", nil)
	srv.Handler().ServeHTTP(rr, req)
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
}
