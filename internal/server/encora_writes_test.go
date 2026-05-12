package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/encora"
	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/server"
	"github.com/nicolerenee/promptbook/internal/storage"
)

// stubDestructiveClient is a minimal in-memory implementation of the
// EncoraDestructiveClient interface used by the destructive-endpoint
// tests. Every call is recorded so assertions can verify the right
// upstream method fired with the right id, and per-method (rateLimit,
// err) defaults can be slotted before each test.
type stubDestructiveClient struct {
	mu sync.Mutex

	removeCollectionCalls []int64
	removeWantsCalls      []int64
	addWantsCalls         []int64
	addCollectionCalls    []int64

	removeCollectionErr error
	removeWantsErr      error
	addWantsErr         error
	addCollectionErr    error

	removeCollectionRL encora.RateLimitInfo
	removeWantsRL      encora.RateLimitInfo
	addWantsRL         encora.RateLimitInfo
	addCollectionRL    encora.RateLimitInfo
}

func (s *stubDestructiveClient) RemoveFromCollection(
	_ context.Context, id int64,
) (encora.RateLimitInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeCollectionCalls = append(s.removeCollectionCalls, id)
	return s.removeCollectionRL, s.removeCollectionErr
}

func (s *stubDestructiveClient) RemoveFromWants(
	_ context.Context, id int64,
) (encora.RateLimitInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeWantsCalls = append(s.removeWantsCalls, id)
	return s.removeWantsRL, s.removeWantsErr
}

func (s *stubDestructiveClient) AddToWants(
	_ context.Context, id int64,
) (encora.RateLimitInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addWantsCalls = append(s.addWantsCalls, id)
	return s.addWantsRL, s.addWantsErr
}

func (s *stubDestructiveClient) AddToCollection(
	_ context.Context, id int64,
) (encora.RateLimitInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addCollectionCalls = append(s.addCollectionCalls, id)
	return s.addCollectionRL, s.addCollectionErr
}

// destructiveTestServer wires a fresh DB + stub destructive client into
// a server and returns both so tests can assert on history rows +
// recorded upstream calls without spinning up a real Encora HTTP server.
func destructiveTestServer(
	t *testing.T,
) (*server.Server, *ent.Client, *stubDestructiveClient) {
	t.Helper()
	sqlDB, db, err := storage.OpenEnt(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	stub := &stubDestructiveClient{}
	srv, err := server.New(server.Options{DB: db, EncoraDestructive: stub})
	require.NoError(t, err)
	return srv, db, stub
}

// postDestructive issues a POST against the supplied path and decodes
// the JSON envelope into a (status, ok, error) triple. Used by the
// destructive-endpoint tests to trim per-test boilerplate.
func postDestructive(
	t *testing.T, srv *server.Server, path string,
) (int, bool, string) {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodPost, path, http.NoBody)
	srv.Handler().ServeHTTP(rr, req)

	var body struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if rr.Body.Len() > 0 {
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body), rr.Body.String())
	}
	return rr.Code, body.OK, body.Error
}

// loadEncoraPushEvents pulls the HistoryKindEncoraPush rows from the
// supplied DB, used by the destructive-endpoint tests to assert the
// audit trail captured (or didn't capture) the call.
func loadEncoraPushEvents(t *testing.T, db *ent.Client) []storage.HistoryEvent {
	t.Helper()
	events, err := storage.ListHistory(t.Context(), db, storage.ListHistoryOptions{
		Kinds: []string{storage.HistoryKindEncoraPush},
	})
	require.NoError(t, err)
	return events
}

func TestAPIRemoveFromCollectionSuccess(t *testing.T) {
	t.Parallel()
	srv, db, stub := destructiveTestServer(t)

	// Seed an in-collection recording so the membership pre-check passes.
	seedMismatchRecording(t, db, 50001, 7777, "RemoveColShow",
		true, false, true, "MKV 1080p", "MKV 1080p")

	status, ok, errMsg := postDestructive(t, srv, "/api/v1/encora/collection/7777/remove")

	require.Equal(t, http.StatusOK, status, errMsg)
	assert.True(t, ok)
	assert.Empty(t, errMsg)

	assert.Equal(t, []int64{7777}, stub.removeCollectionCalls,
		"RemoveFromCollection should be invoked exactly once with the supplied id")

	events := loadEncoraPushEvents(t, db)
	require.Len(t, events, 1, "successful destructive push must record one history row")
	assert.Equal(t, storage.HistoryKindEncoraPush, events[0].Kind)
	require.NotNil(t, events[0].RecordingID)
	assert.Equal(t, int64(7777), *events[0].RecordingID)
	assert.Contains(t, events[0].Summary, "7777")
	assert.Contains(t, events[0].Summary, "collection")
	assert.Equal(t, "remove_from_collection", events[0].Details["action"])
}

func TestAPIRemoveFromCollectionNotInCollection(t *testing.T) {
	t.Parallel()
	srv, db, stub := destructiveTestServer(t)

	// Seed a recording that's only in wants — RemoveFromCollection must
	// reject because there's nothing in the collection to remove.
	seedMismatchRecording(t, db, 50002, 7778, "WantsOnlyShow",
		false, true, false, "", "")

	status, ok, errMsg := postDestructive(t, srv, "/api/v1/encora/collection/7778/remove")

	assert.Equal(t, http.StatusConflict, status)
	assert.False(t, ok)
	assert.Contains(t, errMsg, "not in your collection")
	assert.Empty(t, stub.removeCollectionCalls,
		"upstream client must not be called when the recording isn't in collection")

	events := loadEncoraPushEvents(t, db)
	assert.Empty(t, events,
		"a 409 rejection must not produce an encora_push history row")
}

func TestAPIRemoveFromCollectionWithoutEncoraClient(t *testing.T) {
	t.Parallel()

	sqlDB, db, err := storage.OpenEnt(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	// No EncoraDestructive on Options — the destructive endpoints must
	// degrade to 503 rather than panic.
	srv, err := server.New(server.Options{DB: db})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/encora/collection/9999/remove", http.NoBody)
	srv.Handler().ServeHTTP(rr, req)

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "encora client not configured")
}

// TestAPIRemoveFromCollectionRateLimited asserts a 429 from the upstream
// surfaces as a 429 to the caller, matching describeEncoraError's mapping
// for the apply pipeline. (We picked 429 over 502 so the client can
// distinguish "back off and retry" from "upstream is broken".) No
// history row is written for a failed push.
func TestAPIRemoveFromCollectionRateLimited(t *testing.T) {
	t.Parallel()
	srv, db, stub := destructiveTestServer(t)
	stub.removeCollectionErr = encora.ErrRateLimited
	stub.removeCollectionRL = encora.RateLimitInfo{RetryAfter: 60 * time.Second}

	seedMismatchRecording(t, db, 50003, 7779, "RateLimitShow",
		true, false, true, "MKV 1080p", "MKV 1080p")

	status, ok, errMsg := postDestructive(t, srv, "/api/v1/encora/collection/7779/remove")

	assert.Equal(t, http.StatusTooManyRequests, status)
	assert.False(t, ok)
	assert.Contains(t, errMsg, "rate limited")

	assert.Equal(t, []int64{7779}, stub.removeCollectionCalls,
		"the call must reach the upstream before the rate-limit error surfaces")

	events := loadEncoraPushEvents(t, db)
	assert.Empty(t, events,
		"a failed push must not produce an encora_push history row")
}

// TestAPIRemoveFromCollectionBadID asserts a non-numeric path parameter
// is rejected with 400 before the destructive client is touched.
func TestAPIRemoveFromCollectionBadID(t *testing.T) {
	t.Parallel()
	srv, _, stub := destructiveTestServer(t)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/encora/collection/notanumber/remove", http.NoBody)
	srv.Handler().ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
	assert.Empty(t, stub.removeCollectionCalls)
}

func TestAPIRemoveFromWantsSuccess(t *testing.T) {
	t.Parallel()
	srv, db, stub := destructiveTestServer(t)

	// Seed a wants-only recording (in_wants=true, not in collection) so
	// the membership check accepts the remove.
	seedMismatchRecording(t, db, 50101, 8881, "RemoveWantShow",
		false, true, false, "", "")

	status, ok, errMsg := postDestructive(t, srv, "/api/v1/encora/wants/8881/remove")

	require.Equal(t, http.StatusOK, status, errMsg)
	assert.True(t, ok)
	assert.Equal(t, []int64{8881}, stub.removeWantsCalls)

	events := loadEncoraPushEvents(t, db)
	require.Len(t, events, 1)
	require.NotNil(t, events[0].RecordingID)
	assert.Equal(t, int64(8881), *events[0].RecordingID)
	assert.Contains(t, events[0].Summary, "wants")
	assert.Equal(t, "remove_from_wants", events[0].Details["action"])
}

func TestAPIRemoveFromWantsNotInWants(t *testing.T) {
	t.Parallel()
	srv, db, stub := destructiveTestServer(t)

	// Seed a collected recording — RemoveFromWants must 409 because
	// there's nothing on the wants list to remove.
	seedMismatchRecording(t, db, 50102, 8882, "ColOnlyShow",
		true, false, true, "MKV 1080p", "MKV 1080p")

	status, ok, errMsg := postDestructive(t, srv, "/api/v1/encora/wants/8882/remove")

	assert.Equal(t, http.StatusConflict, status)
	assert.False(t, ok)
	assert.Contains(t, errMsg, "not on your wants list")
	assert.Empty(t, stub.removeWantsCalls)

	events := loadEncoraPushEvents(t, db)
	assert.Empty(t, events)
}

func TestAPIAddToWantsSuccess(t *testing.T) {
	t.Parallel()
	srv, db, stub := destructiveTestServer(t)

	// Seed a recording row that's neither in collection nor wants. The
	// recording table needs an entry so the rest of the catalog can
	// surface it; we just don't link it from collection / wants.
	require.NoError(t, db.Show.Create().
		SetID(50201).SetName("AddWantShow").Exec(t.Context()))
	require.NoError(t, db.Recording.Create().
		SetID(9991).SetShowID(50201).SetRawJSON("{}").Exec(t.Context()))

	status, ok, errMsg := postDestructive(t, srv, "/api/v1/encora/wants/9991/add")

	require.Equal(t, http.StatusOK, status, errMsg)
	assert.True(t, ok)
	assert.Equal(t, []int64{9991}, stub.addWantsCalls)

	events := loadEncoraPushEvents(t, db)
	require.Len(t, events, 1)
	assert.Contains(t, events[0].Summary, "Added")
	assert.Contains(t, events[0].Summary, "wants")
	assert.Equal(t, "add_to_wants", events[0].Details["action"])
}

func TestAPIAddToWantsAlreadyInWants(t *testing.T) {
	t.Parallel()
	srv, db, stub := destructiveTestServer(t)

	// Already on wants list — the add must 409 because there's nothing
	// to add.
	seedMismatchRecording(t, db, 50202, 9992, "AlreadyWantShow",
		false, true, false, "", "")

	status, ok, errMsg := postDestructive(t, srv, "/api/v1/encora/wants/9992/add")

	assert.Equal(t, http.StatusConflict, status)
	assert.False(t, ok)
	assert.Contains(t, errMsg, "already on your wants list")
	assert.Empty(t, stub.addWantsCalls)

	events := loadEncoraPushEvents(t, db)
	assert.Empty(t, events)
}

func TestAPIAddToWantsAlreadyInCollection(t *testing.T) {
	t.Parallel()
	srv, db, stub := destructiveTestServer(t)

	// Already collected — no point wanting something you own. The add
	// must 409 even though wants is empty.
	seedMismatchRecording(t, db, 50203, 9993, "AlreadyOwnedShow",
		true, false, true, "MKV 1080p", "MKV 1080p")

	status, ok, errMsg := postDestructive(t, srv, "/api/v1/encora/wants/9993/add")

	assert.Equal(t, http.StatusConflict, status)
	assert.False(t, ok)
	assert.Contains(t, errMsg, "already in your collection")
	assert.Empty(t, stub.addWantsCalls)

	events := loadEncoraPushEvents(t, db)
	assert.Empty(t, events)
}
