package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nicolerenee/promptbook/internal/config"
	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/ingest"
	"github.com/nicolerenee/promptbook/internal/nforefresh"
	"github.com/nicolerenee/promptbook/internal/probe"
	"github.com/nicolerenee/promptbook/internal/server"
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
	// Recording 90100222 (Marigold) has master "pro-shot" in the fixture.
	// No row in recording_image_choices exists, so the resolver
	// falls back to the per-master default which is "disabled" for
	// pro-shot recordings — their poster art is finished broadcast
	// material and shouldn't be overlaid by default.
	assert.True(t, resp.Data.Recording.OverlayDisabled)
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
						synced outOfSync missing wanted orphan
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
							Synced    int `json:"synced"`
							OutOfSync int `json:"outOfSync"`
							Missing   int `json:"missing"`
							Wanted    int `json:"wanted"`
							Orphan    int `json:"orphan"`
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
				stateCounts { synced missing wanted orphan outOfSync }
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

// TestGraphQLQueueClassificationProjects pins the contract that the
// scanner's per-folder classification blob — persisted on
// manual_import_queue.classification_json — round-trips through the
// queue resolver as a typed QueueClassification subtype. Phase 2 of
// the multipart + extras feature: the modal (phase 3) reads the
// projection to render its multi-file picker without re-walking the
// folder.
func TestGraphQLQueueClassificationProjects(t *testing.T) {
	t.Parallel()
	srv, db := queueImportTestServer(t, &stubIngestRunner{})

	const blob = `{"parts":[{"path":"/a/main.mkv","sizeBytes":4096,` +
		`"suggestedKind":"main","partIndex":0}],"extras":[` +
		`{"path":"/a/audio/track-01.mp3","sizeBytes":2048,` +
		`"suggestedKind":"extra-audio","partIndex":0},` +
		`{"path":"/a/bows.mp4","sizeBytes":1024,` +
		`"suggestedKind":"extra-featurette","partIndex":0}],` +
		`"ambiguous":true}`
	_, err := storage.EnqueueFile(t.Context(), db, storage.QueueEntry{
		FilePath:           "/a/main.mkv",
		FileSizeBytes:      4096,
		ExtrasCount:        2,
		ClassificationJSON: blob,
	})
	require.NoError(t, err)

	body, rr := graphqlPost(t, srv.Handler(), `{
		queue {
			filePath
			classification {
				ambiguous
				parts { path sizeBytes suggestedKind partIndex }
				extras { path sizeBytes suggestedKind partIndex }
			}
		}
	}`)
	require.Equal(t, http.StatusOK, rr.Code, string(body))
	assert.NotContains(t, string(body), `"errors":`, string(body))

	var resp struct {
		Data struct {
			Queue []struct {
				FilePath       string `json:"filePath"`
				Classification struct {
					Ambiguous bool `json:"ambiguous"`
					Parts     []struct {
						Path          string `json:"path"`
						SizeBytes     int    `json:"sizeBytes"`
						SuggestedKind string `json:"suggestedKind"`
						PartIndex     int    `json:"partIndex"`
					} `json:"parts"`
					Extras []struct {
						Path          string `json:"path"`
						SizeBytes     int    `json:"sizeBytes"`
						SuggestedKind string `json:"suggestedKind"`
						PartIndex     int    `json:"partIndex"`
					} `json:"extras"`
				} `json:"classification"`
			} `json:"queue"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	require.Len(t, resp.Data.Queue, 1)

	row := resp.Data.Queue[0]
	assert.True(t, row.Classification.Ambiguous,
		"ambiguous flag must round-trip through the projection")
	require.Len(t, row.Classification.Parts, 1)
	assert.Equal(t, "/a/main.mkv", row.Classification.Parts[0].Path)
	assert.Equal(t, "main", row.Classification.Parts[0].SuggestedKind)
	assert.Equal(t, 4096, row.Classification.Parts[0].SizeBytes)

	require.Len(t, row.Classification.Extras, 2)
	kinds := map[string]string{}
	for _, ex := range row.Classification.Extras {
		kinds[ex.Path] = ex.SuggestedKind
	}
	assert.Equal(t, "extra-audio", kinds["/a/audio/track-01.mp3"])
	assert.Equal(t, "extra-featurette", kinds["/a/bows.mp4"])
}

// TestGraphQLQueueClassificationLegacyEmpty pins the contract that
// queue rows with no classification_json (legacy / loose-file) still
// project as a non-null QueueClassification with empty Parts/Extras.
// The schema's `classification: QueueClassification!` non-null
// promise relies on this — the modal can range over the lists safely.
func TestGraphQLQueueClassificationLegacyEmpty(t *testing.T) {
	t.Parallel()
	srv, db := queueImportTestServer(t, &stubIngestRunner{})

	_, err := storage.EnqueueFile(t.Context(), db, storage.QueueEntry{
		FilePath:      "/incoming/legacy.mkv",
		FileSizeBytes: 2048,
	})
	require.NoError(t, err)

	body, rr := graphqlPost(t, srv.Handler(), `{
		queue {
			classification {
				ambiguous
				parts { path }
				extras { path }
			}
		}
	}`)
	require.Equal(t, http.StatusOK, rr.Code, string(body))
	assert.NotContains(t, string(body), `"errors":`, string(body))

	var resp struct {
		Data struct {
			Queue []struct {
				Classification struct {
					Ambiguous bool `json:"ambiguous"`
					Parts     []struct {
						Path string `json:"path"`
					} `json:"parts"`
					Extras []struct {
						Path string `json:"path"`
					} `json:"extras"`
				} `json:"classification"`
			} `json:"queue"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	require.Len(t, resp.Data.Queue, 1)

	cls := resp.Data.Queue[0].Classification
	assert.False(t, cls.Ambiguous)
	assert.Empty(t, cls.Parts, "legacy rows project an empty parts list")
	assert.Empty(t, cls.Extras, "legacy rows project an empty extras list")
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

// TestGraphQLPreviewQueueImportNotConfigured covers the
// errLibraryNotConfigured branch: a server built without a library
// root surfaces an error rather than rendering a partial plan.
func TestGraphQLPreviewQueueImportNotConfigured(t *testing.T) {
	t.Parallel()
	srv, db := queueImportTestServer(t, &stubIngestRunner{})

	queueID, err := storage.EnqueueFile(t.Context(), db, storage.QueueEntry{
		FilePath: "/incoming/marigold.mkv",
	})
	require.NoError(t, err)

	query := `query Q($input: PreviewQueueImportInput!) {
		previewQueueImport(input: $input) { destAbsolute }
	}`
	body, rr := graphqlPostVars(t, srv.Handler(), query, map[string]any{
		"input": map[string]any{
			"queueID":     "queue-" + strconv.FormatInt(queueID, 10),
			"recordingID": "recording-90100222",
		},
	})
	require.Equal(t, http.StatusOK, rr.Code, string(body))
	assert.Contains(t, string(body), `"errors":`,
		"missing library config must surface a GraphQL error: %s", string(body))
	assert.Contains(t, string(body), "library not configured", string(body))
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

// TestGraphQLImportQueueEntryWithFileAssignments asserts that when the
// modal's multi-file picker forwards explicit fileAssignments, the
// resolver hands them through to ingest.Options.FileAssignments verbatim
// — preserving role tokens (main / part-N / extra-{kind}) and any
// user-supplied label. The legacy single-file flow stays untouched
// when the slice is omitted (covered by the other tests above).
func TestGraphQLImportQueueEntryWithFileAssignments(t *testing.T) {
	t.Parallel()
	stub := &stubIngestRunner{}
	srv, db := queueImportTestServer(t, stub)

	suggested := int64(7777)
	queueID, err := storage.EnqueueFile(t.Context(), db, storage.QueueEntry{
		FilePath:             "/incoming/folder/main.mkv",
		FileSizeBytes:        4096,
		SuggestedRecordingID: &suggested,
		SuggestedConfidence:  storage.ConfidenceHigh,
		ExtrasCount:          2,
	})
	require.NoError(t, err)

	mutation := `mutation Import($input: ImportQueueEntryInput!) {
		importQueueEntry(input: $input) { ok action }
	}`
	body, rr := graphqlPostVars(t, srv.Handler(), mutation, map[string]any{
		"input": map[string]any{
			"queueID": "queue-" + strconv.FormatInt(queueID, 10),
			"fileAssignments": []any{
				map[string]any{
					"sourcePath": "/incoming/folder/main.mkv",
					"kind":       "main",
				},
				map[string]any{
					"sourcePath": "/incoming/folder/bows.mp4",
					"kind":       "extra-featurette",
					"label":      "Bows — Baker / Marinerage",
				},
				map[string]any{
					"sourcePath": "/incoming/folder/photos",
					"kind":       "extra-photo",
				},
			},
		},
	})
	require.Equal(t, http.StatusOK, rr.Code, string(body))
	assert.NotContains(t, string(body), `"errors":`, string(body))

	// The resolver must have called the engine exactly once with the
	// assignments threaded through Options.FileAssignments verbatim.
	require.Len(t, stub.calls, 1)
	got := stub.calls[0].Opts.FileAssignments
	require.Len(t, got, 3, "every assignment must reach the engine")
	assert.Equal(t, "/incoming/folder/main.mkv", got[0].SourcePath)
	assert.Equal(t, "main", got[0].Kind)
	assert.Empty(t, got[0].Label)
	assert.Equal(t, "/incoming/folder/bows.mp4", got[1].SourcePath)
	assert.Equal(t, "extra-featurette", got[1].Kind)
	assert.Equal(t, "Bows — Baker / Marinerage", got[1].Label)
	assert.Equal(t, "/incoming/folder/photos", got[2].SourcePath)
	assert.Equal(t, "extra-photo", got[2].Kind)
}

// TestGraphQLImportQueueEntryEmptyAssignmentsLegacyFlow asserts that
// omitting fileAssignments preserves today's single-file flow exactly:
// the engine is still called, but Opts.FileAssignments is nil so the
// engine routes through the legacy ingestOne pipeline rather than
// ingestWithAssignments.
func TestGraphQLImportQueueEntryEmptyAssignmentsLegacyFlow(t *testing.T) {
	t.Parallel()
	stub := &stubIngestRunner{}
	srv, db := queueImportTestServer(t, stub)

	suggested := int64(2345)
	queueID, err := storage.EnqueueFile(t.Context(), db, storage.QueueEntry{
		FilePath:             "/incoming/loose.mkv",
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
	assert.NotContains(t, string(body), `"errors":`, string(body))

	require.Len(t, stub.calls, 1)
	assert.Empty(t, stub.calls[0].Opts.FileAssignments,
		"omitted fileAssignments must leave Opts.FileAssignments nil")
}

// stubProber returns the supplied MediaInfo verbatim from every Probe
// call. Used by the recording-rename tests so the resolver runs
// without an ffprobe binary on PATH.
type stubProber struct{ info probe.MediaInfo }

func (s stubProber) Probe(_ context.Context, _ string) (probe.MediaInfo, error) {
	return s.info, nil
}

// recordingRenameTestServer wires a server with the library config +
// templates the rename resolvers need + a stub probe + a hand-built
// nfo refresh service that points at a temp image cache. Returns the
// server, the ent client, the library root path, and a callable that
// sounds the apply path's NFO rewrite (used by the apply test to
// assert the post-move rewrite fires).
func recordingRenameTestServer(t *testing.T) (*server.Server, *ent.Client, string) {
	t.Helper()
	libRoot := t.TempDir()
	imageRoot := t.TempDir()
	sqlDB, db, err := storage.OpenEnt(t.Context(), filepath.Join(t.TempDir(), "promptbook.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	cache := imagecache.New(imageRoot, nil, zerolog.New(io.Discard))
	logger := zerolog.New(io.Discard)
	refresh := nforefresh.New(db, nil, cache, "", logger)

	srv, err := server.New(server.Options{
		DB:         db,
		Logger:     logger,
		Prober:     stubProber{},
		NFORefresh: refresh,
		ImageCache: cache,
		Config: config.Config{
			Library: config.LibraryConfig{
				Root:           libRoot,
				FolderTemplate: config.DefaultFolderTemplate,
				FileTemplate:   config.DefaultFileTemplate,
			},
		},
	})
	require.NoError(t, err)
	return srv, db, libRoot
}

// renameRecordingFixture is the minimal Recording row + raw_json blob
// the rename tests need. Mirrors seedRecordingForRender's shape but
// pinned to a recording id the apply test can re-load through the
// LoadRecording / LoadVersions path. metadata.show_id keeps the
// resolved-cast helpers happy.
const renameRecordingFixture = `{` +
	`"id":90004242,"show":"Greenwich Beacon","tour":"Broadway",` +
	`"date":{"full_date":"2017-04-21","month_known":true,"day_known":true,"time":"evening"},` +
	`"master":"X","metadata":{"show_id":7}` +
	`}`

func seedRenameRecording(ctx context.Context, t *testing.T, db *ent.Client) {
	t.Helper()
	require.NoError(t, db.Show.Create().SetID(7).SetName("Greenwich Beacon").Exec(ctx))
	require.NoError(t, db.Recording.Create().
		SetID(90004242).SetShowID(7).SetTour("Broadway").
		SetDateFull("2017-04-21").SetDateMonthKnown(true).SetDateDayKnown(true).
		SetMaster("X").
		SetRawJSON(renameRecordingFixture).
		Exec(ctx))
}

// TestGraphQLPreviewRecordingRenameNoVersions covers the empty-list
// branch: a recording with no on-disk versions returns an empty
// preview list rather than an error.
func TestGraphQLPreviewRecordingRenameNoVersions(t *testing.T) {
	t.Parallel()
	srv, db, _ := recordingRenameTestServer(t)
	seedRenameRecording(t.Context(), t, db)

	query := `query Q($id: ID!) {
		previewRecordingRename(recordingID: $id) {
			versionID
			source
			destination
			willMove
			error
		}
	}`
	body, rr := graphqlPostVars(t, srv.Handler(), query, map[string]any{
		"id": "recording-90004242",
	})
	require.Equal(t, http.StatusOK, rr.Code, string(body))
	assert.NotContains(t, string(body), `"errors":`, string(body))

	var resp struct {
		Data struct {
			PreviewRecordingRename []struct {
				VersionID   string `json:"versionID"`
				Source      string `json:"source"`
				Destination string `json:"destination"`
				WillMove    bool   `json:"willMove"`
				Error       string `json:"error"`
			} `json:"previewRecordingRename"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	assert.Empty(t, resp.Data.PreviewRecordingRename,
		"recording with no versions must return an empty preview list")
}

// TestGraphQLPreviewRecordingRenameOneVersion drives the happy path:
// one version with a non-canonical source path produces one preview
// row whose destination is rendered through the configured templates
// and whose willMove flag is true.
func TestGraphQLPreviewRecordingRenameOneVersion(t *testing.T) {
	t.Parallel()
	srv, db, libRoot := recordingRenameTestServer(t)
	seedRenameRecording(t.Context(), t, db)

	srcPath := filepath.Join(t.TempDir(), "old-name.mkv")
	require.NoError(t, os.WriteFile(srcPath, []byte("test"), 0o600))
	require.NoError(t, storage.UpsertVersion(t.Context(), db, storage.RecordingVersion{
		RecordingID: 90004242,
		FilePath:    srcPath,
	}))

	query := `query Q($id: ID!) {
		previewRecordingRename(recordingID: $id) {
			versionID
			source
			destination
			willMove
			error
		}
	}`
	body, rr := graphqlPostVars(t, srv.Handler(), query, map[string]any{
		"id": "recording-90004242",
	})
	require.Equal(t, http.StatusOK, rr.Code, string(body))
	assert.NotContains(t, string(body), `"errors":`, string(body))

	var resp struct {
		Data struct {
			PreviewRecordingRename []struct {
				VersionID   string `json:"versionID"`
				Source      string `json:"source"`
				Destination string `json:"destination"`
				WillMove    bool   `json:"willMove"`
				Error       string `json:"error"`
			} `json:"previewRecordingRename"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	require.Len(t, resp.Data.PreviewRecordingRename, 1)
	row := resp.Data.PreviewRecordingRename[0]
	assert.Empty(t, row.Error)
	assert.Equal(t, srcPath, row.Source)
	assert.True(t, row.WillMove)
	assert.True(t, strings.HasPrefix(row.VersionID, "version-"),
		"versionID must carry the version- prefix: %s", row.VersionID)
	assert.True(t, strings.HasPrefix(row.Destination, libRoot),
		"destination %q must live under library root %q", row.Destination, libRoot)
	assert.Contains(t, row.Destination, "encora-90004242",
		"destination must include the encora id from the default template")
}

// TestGraphQLApplyRecordingRename drives the full apply path: the
// resolver moves the source file to its canonical destination,
// updates the recording_versions row's file_path, and returns
// moved=true. The on-disk move is asserted by stat-ing both paths
// after the mutation returns.
func TestGraphQLApplyRecordingRename(t *testing.T) {
	t.Parallel()
	srv, db, libRoot := recordingRenameTestServer(t)
	seedRenameRecording(t.Context(), t, db)

	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "old-name.mkv")
	require.NoError(t, os.WriteFile(srcPath, []byte("test-bytes"), 0o600))
	require.NoError(t, storage.UpsertVersion(t.Context(), db, storage.RecordingVersion{
		RecordingID: 90004242,
		FilePath:    srcPath,
	}))

	mutation := `mutation M($id: ID!) {
		applyRecordingRename(recordingID: $id) {
			versionID
			source
			destination
			moved
			error
		}
	}`
	body, rr := graphqlPostVars(t, srv.Handler(), mutation, map[string]any{
		"id": "recording-90004242",
	})
	require.Equal(t, http.StatusOK, rr.Code, string(body))
	assert.NotContains(t, string(body), `"errors":`, string(body))

	var resp struct {
		Data struct {
			ApplyRecordingRename []struct {
				VersionID   string `json:"versionID"`
				Source      string `json:"source"`
				Destination string `json:"destination"`
				Moved       bool   `json:"moved"`
				Error       string `json:"error"`
			} `json:"applyRecordingRename"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	require.Len(t, resp.Data.ApplyRecordingRename, 1)
	row := resp.Data.ApplyRecordingRename[0]
	assert.Empty(t, row.Error)
	assert.True(t, row.Moved)
	assert.Equal(t, srcPath, row.Source)
	assert.True(t, strings.HasPrefix(row.Destination, libRoot),
		"destination %q must live under library root %q", row.Destination, libRoot)

	// Source must be gone, destination must exist.
	_, srcStatErr := os.Stat(srcPath)
	assert.True(t, os.IsNotExist(srcStatErr),
		"source must no longer exist after apply: %v", srcStatErr)
	_, destStatErr := os.Stat(row.Destination)
	require.NoError(t, destStatErr, "destination must exist after apply")

	// Version row must point at the new path.
	versions, err := storage.ListVersions(t.Context(), db, 90004242)
	require.NoError(t, err)
	require.Len(t, versions, 1)
	assert.Equal(t, row.Destination, versions[0].FilePath,
		"recording_versions row must update to the new file_path")
}

// TestGraphQLRecordingExtrasEmpty covers the loose-file path: a
// recording whose only version row has an empty source_folder must
// resolve Recording.extras to [] rather than an error or null. This is
// the dominant case — every legacy import + every loose-file queue
// drop ends up here.
func TestGraphQLRecordingExtrasEmpty(t *testing.T) {
	t.Parallel()
	srv, db, _ := recordingRenameTestServer(t)
	seedRenameRecording(t.Context(), t, db)
	require.NoError(t, storage.UpsertVersion(t.Context(), db, storage.RecordingVersion{
		RecordingID:  90004242,
		FilePath:     "/store/greenwich-beacon/main.mkv",
		SourceFolder: "",
	}))

	query := `query Q($id: ID!) {
		recording(id: $id) { extras { path name sizeBytes isDir } }
	}`
	body, rr := graphqlPostVars(t, srv.Handler(), query, map[string]any{
		"id": "recording-90004242",
	})
	require.Equal(t, http.StatusOK, rr.Code, string(body))
	assert.NotContains(t, string(body), `"errors":`, string(body))

	var resp struct {
		Data struct {
			Recording struct {
				Extras []map[string]any `json:"extras"`
			} `json:"recording"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	assert.Empty(t, resp.Data.Recording.Extras,
		"loose-file recording must resolve extras to []")
}

// TestRecordingExtrasFromTable pins the phase-1 swap of
// Recording.extras from the legacy source-folder walk to the typed
// recording_extras table. Two seeded rows surface with their kind +
// label populated; ordering follows file_path lexicographically.
func TestRecordingExtrasFromTable(t *testing.T) {
	t.Parallel()
	srv, db, _ := recordingRenameTestServer(t)
	seedRenameRecording(t.Context(), t, db)

	_, err := storage.UpsertExtra(t.Context(), db, storage.RecordingExtra{
		RecordingID:   90004242,
		FilePath:      "/library/Greenwich Beacon/featurettes/bows.mp4",
		Kind:          "featurette",
		Label:         "Bows — Baker / Marinerage",
		FileSizeBytes: 412 * 1024 * 1024,
	})
	require.NoError(t, err)
	_, err = storage.UpsertExtra(t.Context(), db, storage.RecordingExtra{
		RecordingID:   90004242,
		FilePath:      "/library/Greenwich Beacon/audio/01 - track.mp3",
		Kind:          "audio",
		FileSizeBytes: 5 * 1024 * 1024,
	})
	require.NoError(t, err)

	query := `query Q($id: ID!) {
		recording(id: $id) { extras { path name sizeBytes isDir kind label } }
	}`
	body, rr := graphqlPostVars(t, srv.Handler(), query, map[string]any{
		"id": "recording-90004242",
	})
	require.Equal(t, http.StatusOK, rr.Code, string(body))
	assert.NotContains(t, string(body), `"errors":`, string(body))

	var resp struct {
		Data struct {
			Recording struct {
				Extras []struct {
					Path      string `json:"path"`
					Name      string `json:"name"`
					SizeBytes int    `json:"sizeBytes"`
					IsDir     bool   `json:"isDir"`
					Kind      string `json:"kind"`
					Label     string `json:"label"`
				} `json:"extras"`
			} `json:"recording"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	require.Len(t, resp.Data.Recording.Extras, 2)

	// Ordered by file_path → audio/... before featurettes/...
	first := resp.Data.Recording.Extras[0]
	assert.Equal(t, "/library/Greenwich Beacon/audio/01 - track.mp3", first.Path)
	assert.Equal(t, "01 - track.mp3", first.Name)
	assert.Equal(t, "audio", first.Kind)
	assert.Empty(t, first.Label)
	assert.False(t, first.IsDir, "phase 1 extras are always file rows")

	second := resp.Data.Recording.Extras[1]
	assert.Equal(t, "/library/Greenwich Beacon/featurettes/bows.mp4", second.Path)
	assert.Equal(t, "featurette", second.Kind)
	assert.Equal(t, "Bows — Baker / Marinerage", second.Label)
}

// TestGraphQLRegenerateRecordingNFO covers the thin wrapper around
// nforefresh.Service.RewriteForRecording: a recording with no version
// rows still resolves to ok=true (the service's no-version-row case is
// a successful no-op).
func TestGraphQLRegenerateRecordingNFO(t *testing.T) {
	t.Parallel()
	srv, db, _ := recordingRenameTestServer(t)
	seedRenameRecording(t.Context(), t, db)

	mutation := `mutation M($id: ID!) {
		regenerateRecordingNFO(recordingID: $id) { ok error }
	}`
	body, rr := graphqlPostVars(t, srv.Handler(), mutation, map[string]any{
		"id": "recording-90004242",
	})
	require.Equal(t, http.StatusOK, rr.Code, string(body))
	assert.NotContains(t, string(body), `"errors":`, string(body))

	var resp struct {
		Data struct {
			RegenerateRecordingNFO struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			} `json:"regenerateRecordingNFO"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	assert.True(t, resp.Data.RegenerateRecordingNFO.OK,
		"regenerateRecordingNFO with no version row must report ok=true")
	assert.Empty(t, resp.Data.RegenerateRecordingNFO.Error)
}
