package server

import (
	"time"

	"entgo.io/contrib/entgql"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/lru"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog"
	"github.com/vektah/gqlparser/v2/ast"

	"github.com/nicolerenee/promptbook/internal/ent"
	"github.com/nicolerenee/promptbook/internal/imagecache"
	"github.com/nicolerenee/promptbook/internal/server/graph"
)

// graphqlQueryCacheSize bounds the parsed-query LRU. The catalog SPA
// has ~10-20 queries; 256 leaves headroom for ad-hoc playground use
// without growing memory unbounded.
const graphqlQueryCacheSize = 256

// graphqlAPQCacheSize bounds the persisted-query LRU. The SPA does not
// use APQ today, but enabling the extension keeps the wire shape
// compatible with future Apollo-style clients.
const graphqlAPQCacheSize = 100

// websocketKeepAlive is the WS ping cadence. gqlgen's example uses 10s
// and that's a sane default; we don't ship subscriptions today but the
// transport stays wired so adding them later is a no-op.
const websocketKeepAlive = 10 * time.Second

// newGraphQLHandler builds a configured gqlgen server backed by the
// supplied ent client + image cache + ingest runner + library plan +
// logger. Mirrors gqlgen's deprecated NewDefaultServer (transports +
// introspection + APQ + LRU query cache) but pinned to the constants
// defined above. cache may be nil — the enrichment resolvers
// nil-check before reading. ingestEngine may be nil — the
// importQueueEntry mutation returns an "ingest not configured" error
// in that case so the rest of the schema stays usable. libraryPlan
// may be the zero value — the previewQueueImport query returns a
// "library not configured" error in that mode.
func newGraphQLHandler(
	client *ent.Client,
	cache *imagecache.Cache,
	ingestEngine graph.IngestRunner,
	libraryPlan graph.LibraryPlan,
	logger zerolog.Logger,
) *handler.Server {
	srv := handler.New(graph.NewSchema(client, cache, ingestEngine, libraryPlan, logger))
	srv.AddTransport(transport.Websocket{KeepAlivePingInterval: websocketKeepAlive})
	srv.AddTransport(transport.Options{})
	srv.AddTransport(transport.GET{})
	srv.AddTransport(transport.POST{})
	srv.AddTransport(transport.MultipartForm{})
	srv.SetQueryCache(lru.New[*ast.QueryDocument](graphqlQueryCacheSize))
	srv.Use(extension.Introspection{})
	srv.Use(extension.AutomaticPersistedQuery{
		Cache: lru.New[string](graphqlAPQCacheSize),
	})
	// Transactioner wraps every request in an ent transaction (read or
	// write). For a read-only schema this is mostly defensive — the
	// resolvers don't write — but it keeps consistent buttonshots within
	// a single multi-field query.
	srv.Use(entgql.Transactioner{TxOpener: client})
	return srv
}

// registerGraphQL wires the /graphql endpoint onto the Echo router.
// POST + GET land on the gqlgen handler (POST for queries, GET for
// CORS pre-flight handling and Apollo-compatible GET queries). The
// playground mounts at /graphql/playground so the live endpoint is
// JSON-only and tooling has a separate URL to bookmark. The image
// cache + ingest runner + logger are plumbed through so the
// enrichment resolvers + the importQueueEntry mutation can drive the
// same surfaces the legacy REST handlers used to.
func (s *Server) registerGraphQL() {
	gql := newGraphQLHandler(
		s.db, s.imageCache, s.ingestEngine, s.libraryPlan(), s.logger,
	)
	s.echo.POST("/graphql", echo.WrapHandler(gql))
	s.echo.GET("/graphql", echo.WrapHandler(gql))
	s.echo.GET(
		"/graphql/playground",
		echo.WrapHandler(playground.Handler("promptbook GraphQL", "/graphql")),
	)
}
