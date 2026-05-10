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

	"github.com/nicolerenee/promptbook/internal/ingest"
	"github.com/nicolerenee/promptbook/internal/storage"
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

// graphqlPostVars is the variable-aware sibling of graphqlPost. The
// queue tests use it so the mutation input can flow through the
// VARIABLES path the SPA actually exercises (string literals would
// short-circuit the prefixed-id unmarshal-from-variable case).
func graphqlPostVars(
	t *testing.T, handler http.Handler, query string, variables map[string]any,
) ([]byte, *httptest.ResponseRecorder) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
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
// callers don't need to wrestle with the Relay node(id:) machinery.
// Validates that the row loads with its denormalized fields populated
// and the show edge resolves through to the parent.
//
// IDs are wire-formatted as `<type>-<int64>` strings — see
// graph/prefixed_id.go for the mapping. The test pins the literal
// "recording-90100222" so a regression in either the marshal or the
// expected-prefix unmarshal path surfaces immediately.
func TestGraphQLRecordingByID(t *testing.T) {
	t.Parallel()
	srv := fixtureBackedServer(t)

	// Recording 90100222 (Marigold Junction) is the same fixture row the
	// REST tests use as their canary; it stays in the seeded data so
	// long as collection.json keeps shipping it.
	query := `{
		recording(id: "recording-90100222") {
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
				ID       string `json:"id"`
				Tour     string `json:"tour"`
				DateFull string `json:"dateFull"`
				Master   string `json:"master"`
				Show     struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"show"`
			} `json:"recording"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	assert.Equal(t, "recording-90100222", resp.Data.Recording.ID)
	assert.True(t, strings.HasPrefix(resp.Data.Recording.Show.ID, "show-"),
		"show id must carry the show- prefix: %s", resp.Data.Recording.Show.ID)
	assert.Equal(t, "Marigold Junction", resp.Data.Recording.Show.Name)
}

// TestGraphQLRecordingEnrichment exercises the server-side enrichment
// resolvers added in Phase 4b — the SPA's recording-detail page reads
// status / localPosterURL / overlayDisabled / bannerLayout / etc.
// off this surface instead of the legacy REST handler. Pulls
// recording 90100222 (Marigold Junction) and asserts the enrichment
// fields render with sensible defaults even when no overlay style is
// persisted (the fixture doesn't pre-seed image_choice rows, so this
// also covers the empty-string fallthrough path).
func TestGraphQLRecordingEnrichment(t *testing.T) {
	t.Parallel()
	srv := fixtureBackedServer(t)

	query := `{
		recording(id: "recording-90100222") {
			id
			status
			localPosterURL
			localFanartURL
			overlayDisabled
			overlayTextOverride
			bannerLayout { position imageRegion }
		}
	}`
	body, rr := graphqlPost(t, srv.Handler(), query)
	require.Equal(t, http.StatusOK, rr.Code, string(body))
	assert.NotContains(t, string(body), `"errors":`, string(body))

	var resp struct {
		Data struct {
			Recording struct {
				ID                  string  `json:"id"`
				Status              string  `json:"status"`
				LocalPosterURL      string  `json:"localPosterURL"`
				LocalFanartURL      string  `json:"localFanartURL"`
				OverlayDisabled     bool    `json:"overlayDisabled"`
				OverlayTextOverride *string `json:"overlayTextOverride"`
				BannerLayout        struct {
					Position    string `json:"position"`
					ImageRegion string `json:"imageRegion"`
				} `json:"bannerLayout"`
			} `json:"recording"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	assert.Equal(t, "recording-90100222", resp.Data.Recording.ID)
	// Recording 90100222 sits in the collection with no local files in the
	// fixture; the reconciler resolves to "missing" (collection row,
	// no version row).
	assert.Equal(t, "missing", resp.Data.Recording.Status)
	// Image cache is unconfigured in this server setup, so URL fields
	// fall through to "" rather than the canonical /images/... path.
	// The SPA renders the placeholder branch on empty strings.
	assert.Empty(t, resp.Data.Recording.LocalPosterURL)
	assert.Empty(t, resp.Data.Recording.LocalFanartURL)
	assert.False(t, resp.Data.Recording.OverlayDisabled)
	assert.Nil(t, resp.Data.Recording.OverlayTextOverride)
	assert.Empty(t, resp.Data.Recording.BannerLayout.Position)
	assert.Empty(t, resp.Data.Recording.BannerLayout.ImageRegion)
}

// TestGraphQLShowEnrichment validates the show-side custom resolvers —
// localBannerURL / recordingCount / firstYear / lastYear /
// stateCounts — return the same shape the legacy REST handler
// produced. The Marigold fixture has 1 recording (id 90100222, in wants), so
// we expect recordingCount=1, stateCounts.wanted=1, and a year span
// matching the recording's date_full.
func TestGraphQLShowEnrichment(t *testing.T) {
	t.Parallel()
	srv := fixtureBackedServer(t)

	query := `{
		shows(first: 100, where: {name: "Marigold Junction"}) {
			edges {
				node {
					id
					name
					recordingCount
					firstYear
					lastYear
					localBannerURL
					stateCounts {
						synced formatMismatch missing wanted orphan
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
						ID             string `json:"id"`
						Name           string `json:"name"`
						RecordingCount int    `json:"recordingCount"`
						FirstYear      *int   `json:"firstYear"`
						LastYear       *int   `json:"lastYear"`
						LocalBannerURL string `json:"localBannerURL"`
						StateCounts    struct {
							Synced         int `json:"synced"`
							FormatMismatch int `json:"formatMismatch"`
							Missing        int `json:"missing"`
							Wanted         int `json:"wanted"`
							Orphan         int `json:"orphan"`
						} `json:"stateCounts"`
					} `json:"node"`
				} `json:"edges"`
			} `json:"shows"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	require.Len(t, resp.Data.Shows.Edges, 1)
	show := resp.Data.Shows.Edges[0].Node
	assert.True(t, strings.HasPrefix(show.ID, "show-"))
	assert.GreaterOrEqual(t, show.RecordingCount, 1)
	assert.GreaterOrEqual(t, show.StateCounts.Missing, 1,
		"marigold 90100222 lives in collection (no local file) → missing")
	assert.NotNil(t, show.FirstYear)
	assert.NotNil(t, show.LastYear)
	// Image cache is unconfigured here; localBannerURL falls through to "".
	assert.Empty(t, show.LocalBannerURL)
}

// TestGraphQLRecordingsListByStatus exercises the recordingsList
// custom resolver — the offset-paginated, status-aware query the SPA
// recordings page hits. Filters to `missing` and asserts the page
// contains at least the 28 collection rows we expect, and that every
// item carries the prefixed-id wire format.
func TestGraphQLRecordingsListByStatus(t *testing.T) {
	t.Parallel()
	srv := fixtureBackedServer(t)

	query := `{
		recordingsList(status: "missing", limit: 5, offset: 0, sort: "date", dir: "desc") {
			total
			limit
			offset
			items {
				id
				showID
				show
				status
				dateFull
			}
		}
	}`
	body, rr := graphqlPost(t, srv.Handler(), query)
	require.Equal(t, http.StatusOK, rr.Code, string(body))
	assert.NotContains(t, string(body), `"errors":`, string(body))

	var resp struct {
		Data struct {
			RecordingsList struct {
				Total  int `json:"total"`
				Limit  int `json:"limit"`
				Offset int `json:"offset"`
				Items  []struct {
					ID       string `json:"id"`
					ShowID   string `json:"showID"`
					Show     string `json:"show"`
					Status   string `json:"status"`
					DateFull string `json:"dateFull"`
				} `json:"items"`
			} `json:"recordingsList"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	assert.GreaterOrEqual(t, resp.Data.RecordingsList.Total, 1,
		"the fixture seeds the collection with at least one missing recording")
	assert.LessOrEqual(t, len(resp.Data.RecordingsList.Items), 5)
	for _, it := range resp.Data.RecordingsList.Items {
		assert.True(t, strings.HasPrefix(it.ID, "recording-"))
		assert.True(t, strings.HasPrefix(it.ShowID, "show-"))
		assert.Equal(t, "missing", it.Status,
			"status filter should pin every row to the requested label")
	}
}

// TestGraphQLShowsList exercises the showsList resolver — the
// offset-paginated by-show aggregate the SPA /shows page hits.
func TestGraphQLShowsList(t *testing.T) {
	t.Parallel()
	srv := fixtureBackedServer(t)

	query := `{
		showsList(limit: 5, offset: 0, sort: "name") {
			total
			items {
				id
				name
				recordingCount
				stateCounts { synced missing wanted orphan formatMismatch }
			}
		}
	}`
	body, rr := graphqlPost(t, srv.Handler(), query)
	require.Equal(t, http.StatusOK, rr.Code, string(body))
	assert.NotContains(t, string(body), `"errors":`, string(body))

	var resp struct {
		Data struct {
			ShowsList struct {
				Total int `json:"total"`
				Items []struct {
					ID             string `json:"id"`
					Name           string `json:"name"`
					RecordingCount int    `json:"recordingCount"`
				} `json:"items"`
			} `json:"showsList"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	assert.GreaterOrEqual(t, resp.Data.ShowsList.Total, 1)
	for _, it := range resp.Data.ShowsList.Items {
		assert.True(t, strings.HasPrefix(it.ID, "show-"))
	}
}

// TestGraphQLPrefixedIDValidation asserts that a wrong-prefix ID
// argument produces a GraphQL error (not a silent null). Without
// the prefix check a `show-...` value passed where `recording-...`
// is expected would silently load the wrong row — exactly the kind
// of cross-type confusion the prefixed-id scheme exists to prevent.
func TestGraphQLPrefixedIDValidation(t *testing.T) {
	t.Parallel()
	srv := fixtureBackedServer(t)

	// recording(id:) must reject a `show-1234` literal. The error
	// message points at the format violation so a confused caller
	// sees "expected recording, got show" in the path.
	query := `{ recording(id: "show-90100222") { id } }`
	body, rr := graphqlPost(t, srv.Handler(), query)
	require.Equal(t, http.StatusOK, rr.Code, string(body))
	assert.Contains(t, string(body), `"errors":`,
		"wrong-prefix id arg must surface an error: %s", string(body))
	assert.Contains(t, string(body), "expected", string(body))
}

// TestGraphQLNodeByID exercises the Relay node(id:) resolver through
// a prefixed string ID. Recording 90100222 dispatches via the recording-
// prefix to the recording loader and resolves the same row the typed
// `recording(id:)` query returns. The __typename lands on the union
// branch entgql wires for ent.Recording so a client can discriminate
// without an explicit fragment.
func TestGraphQLNodeByID(t *testing.T) {
	t.Parallel()
	srv := fixtureBackedServer(t)

	query := `{
		node(id: "recording-90100222") {
			__typename
			... on Recording {
				id
				tour
			}
		}
	}`
	body, rr := graphqlPost(t, srv.Handler(), query)
	require.Equal(t, http.StatusOK, rr.Code, string(body))
	assert.NotContains(t, string(body), `"errors":`, string(body))
	assert.Contains(t, string(body), `"__typename":"Recording"`)
	assert.Contains(t, string(body), `"id":"recording-90100222"`)
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
						ID     string `json:"id"`
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
	for _, e := range resp.Data.Recordings.Edges {
		assert.True(t, strings.HasPrefix(e.Node.ID, "recording-"),
			"every edge node id must carry the recording- prefix: %s",
			e.Node.ID)
	}
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
						ID         string `json:"id"`
						Name       string `json:"name"`
						Recordings struct {
							TotalCount int `json:"totalCount"`
							Edges      []struct {
								Node struct {
									ID string `json:"id"`
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
	assert.True(t, strings.HasPrefix(show.ID, "show-"),
		"show id must carry the show- prefix: %s", show.ID)
	assert.GreaterOrEqual(t, show.Recordings.TotalCount, 1, "show should have at least one recording")
	// Recording 90100222 must be in the edge list.
	found := false
	ids := make([]string, 0, len(show.Recordings.Edges))
	for _, e := range show.Recordings.Edges {
		ids = append(ids, e.Node.ID)
		if e.Node.ID == "recording-90100222" {
			found = true
		}
	}
	assert.True(t, found, "expected recording-90100222 under Marigold Junction: %s",
		strings.Join(ids, ","))
}

// TestGraphQLQueueEmpty asserts that the queue resolver returns an
// empty (non-nil) array when no rows have been enqueued. Mirrors the
// legacy TestAPIQueueEmpty against the same DB fixture path.
func TestGraphQLQueueEmpty(t *testing.T) {
	t.Parallel()
	srv, _ := queueImportTestServer(t, &stubIngestRunner{})

	body, rr := graphqlPost(t, srv.Handler(), `{ queue { id filePath } }`)
	require.Equal(t, http.StatusOK, rr.Code, string(body))
	assert.NotContains(t, string(body), `"errors":`, string(body))

	var resp struct {
		Data struct {
			Queue []map[string]any `json:"queue"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	assert.Empty(t, resp.Data.Queue)
}

// TestGraphQLQueueLists exercises the queue read against a populated
// table. Asserts the prefixed-id wire format on both QueueEntry.id
// (queue-N) and QueueEntry.suggestedRecordingID (recording-N) so a
// regression in either marshal path surfaces immediately.
func TestGraphQLQueueLists(t *testing.T) {
	t.Parallel()
	srv, db := queueImportTestServer(t, &stubIngestRunner{})

	suggested := int64(90100222)
	_, err := storage.EnqueueFile(t.Context(), db, storage.QueueEntry{
		FilePath:            "/incoming/marigold.mkv",
		FileSizeBytes:       1024,
		SuggestedConfidence: storage.ConfidenceLow,
		Notes:               "no match",
	})
	require.NoError(t, err)
	_, err = storage.EnqueueFile(t.Context(), db, storage.QueueEntry{
		FilePath:             "/incoming/greenwich-beacon.mkv",
		FileSizeBytes:        2048,
		SuggestedRecordingID: &suggested,
		SuggestedConfidence:  storage.ConfidenceHigh,
	})
	require.NoError(t, err)

	body, rr := graphqlPost(t, srv.Handler(), `{
		queue {
			id
			filePath
			fileSizeBytes
			suggestedRecordingID
			suggestedConfidence
			notes
		}
	}`)
	require.Equal(t, http.StatusOK, rr.Code, string(body))
	assert.NotContains(t, string(body), `"errors":`, string(body))

	var resp struct {
		Data struct {
			Queue []struct {
				ID                   string  `json:"id"`
				FilePath             string  `json:"filePath"`
				FileSizeBytes        int     `json:"fileSizeBytes"`
				SuggestedRecordingID *string `json:"suggestedRecordingID"`
				SuggestedConfidence  string  `json:"suggestedConfidence"`
				Notes                string  `json:"notes"`
			} `json:"queue"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	require.Len(t, resp.Data.Queue, 2)

	paths := make(map[string]bool, len(resp.Data.Queue))
	for _, it := range resp.Data.Queue {
		paths[it.FilePath] = true
		assert.True(t, strings.HasPrefix(it.ID, "queue-"),
			"queue id must carry the queue- prefix: %s", it.ID)
	}
	assert.True(t, paths["/incoming/marigold.mkv"])
	assert.True(t, paths["/incoming/greenwich-beacon.mkv"])

	// Find the greenwich-beacon row and assert its suggestedRecordingID came back
	// with the recording- prefix.
	var greenwich-beacon *string
	for _, it := range resp.Data.Queue {
		if it.FilePath == "/incoming/greenwich-beacon.mkv" {
			greenwich-beacon = it.SuggestedRecordingID
			break
		}
	}
	require.NotNil(t, greenwich-beacon, "greenwich-beacon row missing suggestedRecordingID")
	assert.Equal(t, "recording-90100222", *greenwich-beacon)
}

// TestGraphQLImportQueueEntrySuccess covers the happy path: the
// resolver runs the engine, removes the queue row, and writes a
// manual_import history event.
func TestGraphQLImportQueueEntrySuccess(t *testing.T) {
	t.Parallel()
	stub := &stubIngestRunner{}
	srv, db := queueImportTestServer(t, stub)

	suggested := int64(90100222)
	queueID, err := storage.EnqueueFile(t.Context(), db, storage.QueueEntry{
		FilePath:             "/incoming/greenwich-beacon.mkv",
		FileSizeBytes:        2048,
		SuggestedRecordingID: &suggested,
		SuggestedConfidence:  storage.ConfidenceHigh,
	})
	require.NoError(t, err)

	mutation := `mutation Import($input: ImportQueueEntryInput!) {
		importQueueEntry(input: $input) {
			ok
			action
			error
			dest
		}
	}`
	body, rr := graphqlPostVars(t, srv.Handler(), mutation, map[string]any{
		"input": map[string]any{
			"queueID": "queue-" + strconv.FormatInt(queueID, 10),
		},
	})
	require.Equal(t, http.StatusOK, rr.Code, string(body))
	assert.NotContains(t, string(body), `"errors":`, string(body))

	var resp struct {
		Data struct {
			ImportQueueEntry struct {
				OK     bool   `json:"ok"`
				Action string `json:"action"`
				Error  string `json:"error"`
				Dest   string `json:"dest"`
			} `json:"importQueueEntry"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	assert.True(t, resp.Data.ImportQueueEntry.OK)
	assert.Equal(t, ingest.ActionMoved, resp.Data.ImportQueueEntry.Action)

	// Engine called with the suggested id, source = entry.FilePath.
	require.Len(t, stub.calls, 1)
	assert.Equal(t, "/incoming/greenwich-beacon.mkv", stub.calls[0].Src)
	assert.Equal(t, int(suggested), stub.calls[0].Opts.FlagEncoraID)

	// Queue row removed on success.
	_, err = storage.LoadQueueEntry(t.Context(), db, queueID)
	require.ErrorIs(t, err, storage.ErrQueueEntryNotFound)

	// History event recorded with kind=manual_import + recording_id.
	events, err := storage.ListHistory(t.Context(), db, storage.ListHistoryOptions{
		Kinds: []string{storage.HistoryKindManualImport},
	})
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, storage.HistoryKindManualImport, events[0].Kind)
	require.NotNil(t, events[0].RecordingID)
	assert.Equal(t, suggested, *events[0].RecordingID)
}

// TestGraphQLImportQueueEntryNotFound covers the resolver's
// ErrQueueEntryNotFound branch — the GraphQL surface returns a
// non-nil errors array; the engine must not fire.
func TestGraphQLImportQueueEntryNotFound(t *testing.T) {
	t.Parallel()
	stub := &stubIngestRunner{}
	srv, _ := queueImportTestServer(t, stub)

	mutation := `mutation Import($input: ImportQueueEntryInput!) {
		importQueueEntry(input: $input) { ok }
	}`
	body, rr := graphqlPostVars(t, srv.Handler(), mutation, map[string]any{
		"input": map[string]any{"queueID": "queue-9999"},
	})
	require.Equal(t, http.StatusOK, rr.Code, string(body))
	assert.Contains(t, string(body), `"errors":`,
		"missing queue entry must surface a GraphQL error: %s", string(body))
	assert.Contains(t, string(body), "not found", string(body))
	assert.Empty(t, stub.calls, "engine must not run when the queue entry is missing")
}

// TestGraphQLImportQueueEntryEngineNotConfigured covers the 503-equivalent
// branch: the resolver was constructed with a nil ingestEngine, so the
// mutation surfaces an error and the queue row stays in place.
func TestGraphQLImportQueueEntryEngineNotConfigured(t *testing.T) {
	t.Parallel()
	srv, db := queueImportTestServer(t, nil)

	suggested := int64(90100222)
	queueID, err := storage.EnqueueFile(t.Context(), db, storage.QueueEntry{
		FilePath:             "/incoming/greenwich-beacon.mkv",
		SuggestedRecordingID: &suggested,
		SuggestedConfidence:  storage.ConfidenceHigh,
	})
	require.NoError(t, err)

	mutation := `mutation Import($input: ImportQueueEntryInput!) {
		importQueueEntry(input: $input) { ok }
	}`
	body, rr := graphqlPostVars(t, srv.Handler(), mutation, map[string]any{
		"input": map[string]any{
			"queueID": "queue-" + strconv.FormatInt(queueID, 10),
		},
	})
	require.Equal(t, http.StatusOK, rr.Code, string(body))
	assert.Contains(t, string(body), `"errors":`,
		"missing ingest engine must surface a GraphQL error: %s", string(body))
	assert.Contains(t, string(body), "ingest not configured", string(body))

	// Queue row preserved.
	_, err = storage.LoadQueueEntry(t.Context(), db, queueID)
	require.NoError(t, err)
}

// TestGraphQLImportQueueEntryExplicitID asserts that a non-nil
// recordingID input overrides the queue entry's suggestedRecordingID,
// matching the legacy TestAPIImportQueueExplicitID behavior.
func TestGraphQLImportQueueEntryExplicitID(t *testing.T) {
	t.Parallel()
	stub := &stubIngestRunner{}
	srv, db := queueImportTestServer(t, stub)

	suggested := int64(1111)
	queueID, err := storage.EnqueueFile(t.Context(), db, storage.QueueEntry{
		FilePath:             "/incoming/greenwich-beacon.mkv",
		SuggestedRecordingID: &suggested,
		SuggestedConfidence:  storage.ConfidenceLow,
	})
	require.NoError(t, err)

	mutation := `mutation Import($input: ImportQueueEntryInput!) {
		importQueueEntry(input: $input) { ok action }
	}`
	body, rr := graphqlPostVars(t, srv.Handler(), mutation, map[string]any{
		"input": map[string]any{
			"queueID":     "queue-" + strconv.FormatInt(queueID, 10),
			"recordingID": "recording-9999",
		},
	})
	require.Equal(t, http.StatusOK, rr.Code, string(body))
	assert.NotContains(t, string(body), `"errors":`, string(body))

	require.Len(t, stub.calls, 1)
	assert.Equal(t, 9999, stub.calls[0].Opts.FlagEncoraID,
		"explicit recordingID must override the suggested id")
}
