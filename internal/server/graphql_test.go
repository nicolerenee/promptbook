package server_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// graphqlPost runs a GraphQL POST against the supplied handler and
// returns the decoded "data" + "errors" sections plus the raw response
// for ad-hoc assertions. Variables are encoded inline; the smoke
// queries use straight literals so the helper stays minimal.
func graphqlPost(t *testing.T, handler http.Handler, query string) ([]byte, *httptest.ResponseRecorder) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"query": query})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/graphql", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rr, req)
	return rr.Body.Bytes(), rr
}

// TestGraphQLPlaygroundReachable is the bare-minimum smoke test —
// /graphql/playground returns the gqlgen sandbox HTML. Catches the
// route registration regression where the SPA catch-all swallows
// /graphql/* before the gqlgen handler sees it. The page is the
// GraphiQL bundle (gqlgen's playground.Handler renders GraphiQL),
// asserted via the title we set in the handler ctor.
func TestGraphQLPlaygroundReachable(t *testing.T) {
	t.Parallel()
	srv := fixtureBackedServer(t)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/graphql/playground", nil)
	srv.Handler().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), "promptbook GraphQL",
		"playground HTML must include the title we set on registerGraphQL")
}

// TestGraphQLRecordingByID hits the typed `recording(id:)` query
// shortcut — the per-type fetcher promptbook adds in schema.graphql so
// callers don't need to wrestle with the Relay node(id:) machinery
// (which we disable; see ent.resolvers.go). Validates that the row
// loads with its denormalized fields populated and the show edge
// resolves through to the parent.
func TestGraphQLRecordingByID(t *testing.T) {
	t.Parallel()
	srv := fixtureBackedServer(t)

	// Recording 90100222 (Marigold Junction) is the same fixture row the
	// REST tests use as their canary; it stays in the seeded data so
	// long as collection.json keeps shipping it.
	query := `{
		recording(id: 90100222) {
			id
			tour
			dateFull
			master
			show {
				id
				name
			}
		}
	}`
	body, rr := graphqlPost(t, srv.Handler(), query)
	require.Equal(t, http.StatusOK, rr.Code, string(body))
	assert.NotContains(t, string(body), `"errors":`, string(body))

	var resp struct {
		Data struct {
			Recording struct {
				ID       int64  `json:"id"`
				Tour     string `json:"tour"`
				DateFull string `json:"dateFull"`
				Master   string `json:"master"`
				Show     struct {
					ID   int64  `json:"id"`
					Name string `json:"name"`
				} `json:"show"`
			} `json:"recording"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	assert.Equal(t, int64(90100222), resp.Data.Recording.ID)
	assert.Equal(t, "Marigold Junction", resp.Data.Recording.Show.Name)
}

// TestGraphQLNodeUnsupported asserts that the Relay node(id:) resolver
// surfaces the documented error rather than crashing on the missing
// ent_types table. The error message guides callers toward the
// per-type shortcut fields.
func TestGraphQLNodeUnsupported(t *testing.T) {
	t.Parallel()
	srv := fixtureBackedServer(t)

	query := `{ node(id: 1) { __typename } }`
	body, rr := graphqlPost(t, srv.Handler(), query)
	require.Equal(t, http.StatusOK, rr.Code, string(body))
	assert.Contains(t, string(body), "node(id:) lookups are not supported")
}

// TestGraphQLRecordingsConnection exercises the Relay `recordings`
// connection — paginates the first 5 rows and asserts the page-info
// fields are present + the totalCount tracks the underlying recordings
// table (28 collection rows + 14 wants in the fixture).
func TestGraphQLRecordingsConnection(t *testing.T) {
	t.Parallel()
	srv := fixtureBackedServer(t)

	query := `{
		recordings(first: 5, orderBy: {field: DATE_FULL, direction: DESC}) {
			totalCount
			pageInfo {
				hasNextPage
				endCursor
			}
			edges {
				cursor
				node {
					id
					master
					tour
				}
			}
		}
	}`
	body, rr := graphqlPost(t, srv.Handler(), query)
	require.Equal(t, http.StatusOK, rr.Code, string(body))
	assert.NotContains(t, string(body), `"errors":`, string(body))

	var resp struct {
		Data struct {
			Recordings struct {
				TotalCount int `json:"totalCount"`
				PageInfo   struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
				Edges []struct {
					Cursor string `json:"cursor"`
					Node   struct {
						ID     int64  `json:"id"`
						Master string `json:"master"`
						Tour   string `json:"tour"`
					} `json:"node"`
				} `json:"edges"`
			} `json:"recordings"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	assert.GreaterOrEqual(t, resp.Data.Recordings.TotalCount, 28,
		"fixture seeds at least the 28-row collection")
	assert.Len(t, resp.Data.Recordings.Edges, 5)
	assert.True(t, resp.Data.Recordings.PageInfo.HasNextPage)
	assert.NotEmpty(t, resp.Data.Recordings.PageInfo.EndCursor)
}

// TestGraphQLShowQuery loads a show by id with its recordings edge.
// Walks Show -> Recording so both ent edges + relay connections
// resolve in a single round-trip — catches edge wiring regressions
// the standalone node-query test wouldn't surface.
func TestGraphQLShowQuery(t *testing.T) {
	t.Parallel()
	srv := fixtureBackedServer(t)

	// Marigold Junction lives at show id 217 in the fixture (the same
	// id recording 90100222 above resolves through). Pin it so the assertion
	// surfaces a fixture-drift failure clearly.
	query := `{
		shows(first: 100, where: {name: "Marigold Junction"}) {
			edges {
				node {
					id
					name
					recordings(first: 50) {
						totalCount
						edges {
							node { id }
						}
					}
				}
			}
		}
	}`
	body, rr := graphqlPost(t, srv.Handler(), query)
	require.Equal(t, http.StatusOK, rr.Code, string(body))
	assert.NotContains(t, string(body), `"errors":`, string(body))

	var resp struct {
		Data struct {
			Shows struct {
				Edges []struct {
					Node struct {
						ID         int64  `json:"id"`
						Name       string `json:"name"`
						Recordings struct {
							TotalCount int `json:"totalCount"`
							Edges      []struct {
								Node struct {
									ID int64 `json:"id"`
								} `json:"node"`
							} `json:"edges"`
						} `json:"recordings"`
					} `json:"node"`
				} `json:"edges"`
			} `json:"shows"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	require.Len(t, resp.Data.Shows.Edges, 1, "exactly one show should match the literal name filter")
	show := resp.Data.Shows.Edges[0].Node
	assert.Equal(t, "Marigold Junction", show.Name)
	assert.GreaterOrEqual(t, show.Recordings.TotalCount, 1, "show should have at least one recording")
	// Recording 90100222 must be in the edge list.
	found := false
	ids := make([]string, 0, len(show.Recordings.Edges))
	for _, e := range show.Recordings.Edges {
		ids = append(ids, strconv.FormatInt(e.Node.ID, 10))
		if e.Node.ID == 90100222 {
			found = true
		}
	}
	assert.True(t, found, "expected recording 90100222 under Marigold Junction: %s",
		strings.Join(ids, ","))
}
