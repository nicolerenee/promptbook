package stagemedia_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/stagemedia"
)

// fixtureServer dispatches /api/images calls to the right testdata file
// based on the show_id query and presence/absence of actor_ids.
//
// Routes:
//   - show_id=90100728 + actor_ids present → 200 + fixture-show.json
//   - show_id=90100728 + actor_ids absent  → 400 + fixture-show-no-actor.json
//   - show_id=invalid-slug + actor_ids present → 400 + fixture-show-invalid.json
//     (used by the invalid-show-id test, where a synthetic httptest server
//     forces this route regardless of the int64 the client serializes — the
//     fixture's job is to exercise the 400 error-shape parser).
//
// Anything else 404s so a misrouted test fails loudly instead of silently
// passing on an empty body.
func fixtureServer(t *testing.T) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/images", func(w http.ResponseWriter, r *http.Request) {
		showID := r.URL.Query().Get("show_id")
		actorIDs := r.URL.Query().Get("actor_ids")

		switch {
		case showID == "90100728" && actorIDs != "":
			writeFixture(t, w, http.StatusOK, "fixture-show.json")
		case showID == "90100728" && actorIDs == "":
			writeFixture(t, w, http.StatusBadRequest, "fixture-show-no-actor.json")
		case showID == "invalid-slug":
			writeFixture(t, w, http.StatusBadRequest, "fixture-show-invalid.json")
		default:
			http.NotFound(w, r)
		}
	})
	return httptest.NewServer(mux)
}

func writeFixture(t *testing.T, w http.ResponseWriter, status int, name string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func newTestClient(t *testing.T, baseURL string) *stagemedia.Client {
	t.Helper()
	c, err := stagemedia.New(stagemedia.Options{BaseURL: baseURL, APIKey: "test"})
	require.NoError(t, err)
	return c
}

// TestPostersHappyPath fetches show_id=90100728 against the fixture server,
// expecting an empty posters list (the fixture has 0 posters) and no error.
func TestPostersHappyPath(t *testing.T) {
	t.Parallel()

	srv := fixtureServer(t)
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL)
	posters, err := c.Posters(context.Background(), 90100728)
	require.NoError(t, err)
	assert.NotNil(t, posters, "Posters should never return nil — empty slice on no posters")
	assert.Empty(t, posters)
}

// TestPostersAlsoSetsActorIDsSentinel verifies the client sends
// actor_ids=1 as the magic sentinel value when only posters are wanted.
func TestPostersAlsoSetsActorIDsSentinel(t *testing.T) {
	t.Parallel()

	var sawActorIDs string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawActorIDs = r.URL.Query().Get("actor_ids")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"posters":[],"performers":[],"error":null}`))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL)
	_, err := c.Posters(context.Background(), 90100728)
	require.NoError(t, err)
	assert.Equal(t, "1", sawActorIDs, "Posters should send actor_ids=1 sentinel")
}

// TestImagesHappyPath fetches show_id=90100728 with performer_ids=[1] and
// expects the parsed Performer URL back.
func TestImagesHappyPath(t *testing.T) {
	t.Parallel()

	srv := fixtureServer(t)
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL)
	imgs, err := c.Images(context.Background(), 90100728, []int64{1})
	require.NoError(t, err)
	assert.Empty(t, imgs.Posters)
	require.Len(t, imgs.Performers, 1)
	assert.Equal(t, int64(1), imgs.Performers[0].ID)
	assert.Equal(
		t,
		"https://fixture.invalid/storage/headshots/01JFIXTUREHEADSHOT00000000000.jpg",
		imgs.Performers[0].URL,
	)
	assert.Nil(t, imgs.Error, "Error should be nil on a 200 response")
}

// TestImagesEmptyPerformerIDsErrors confirms the client short-circuits with
// ErrBadRequest before issuing a request — the test server fails hard if
// the client erroneously contacts it.
func TestImagesEmptyPerformerIDsErrors(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("client made an HTTP call when performerIDs was empty")
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL)
	_, err := c.Images(context.Background(), 90100728, nil)
	assert.ErrorIs(t, err, stagemedia.ErrBadRequest)
}

// TestImagesInvalidShowIDError uses a hand-rolled server that always
// returns the fixture-show-invalid.json fixture (400 + "Invalid show_id")
// regardless of the show_id integer the client serializes. The fixture's
// role is to exercise the 400 error-shape parser — making the upstream
// message available in the wrapped error.
func TestImagesInvalidShowIDError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		b, err := os.ReadFile(filepath.Join("testdata", "fixture-show-invalid.json"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL)
	_, err := c.Images(context.Background(), 0, []int64{1})
	require.ErrorIs(t, err, stagemedia.ErrBadRequest)
	assert.Contains(t, err.Error(), "Invalid show_id")
}

// TestUnauthorized confirms a 401 maps to the typed sentinel.
func TestUnauthorized(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		call func(ctx context.Context, c *stagemedia.Client) error
	}{
		{
			name: "posters",
			call: func(ctx context.Context, c *stagemedia.Client) error {
				_, err := c.Posters(ctx, 90100728)
				return err
			},
		},
		{
			name: "images",
			call: func(ctx context.Context, c *stagemedia.Client) error {
				_, err := c.Images(ctx, 90100728, []int64{1})
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
			}))
			t.Cleanup(srv.Close)

			c := newTestClient(t, srv.URL)
			err := tt.call(context.Background(), c)
			assert.ErrorIs(t, err, stagemedia.ErrUnauthorized)
		})
	}
}

// TestSetsBearerAuth verifies that the Authorization: Bearer header is
// attached to every outbound request.
func TestSetsBearerAuth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		call func(ctx context.Context, c *stagemedia.Client) error
	}{
		{
			name: "posters",
			call: func(ctx context.Context, c *stagemedia.Client) error {
				_, err := c.Posters(ctx, 90100728)
				return err
			},
		},
		{
			name: "images",
			call: func(ctx context.Context, c *stagemedia.Client) error {
				_, err := c.Images(ctx, 90100728, []int64{1, 2, 3})
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var sawAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sawAuth = r.Header.Get("Authorization")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"posters":[],"performers":[],"error":null}`))
			}))
			t.Cleanup(srv.Close)

			c := newTestClient(t, srv.URL)
			err := tt.call(context.Background(), c)
			require.NoError(t, err)
			assert.Equal(t, "Bearer test", sawAuth)
		})
	}
}
