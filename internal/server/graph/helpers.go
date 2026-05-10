package graph

import (
	"errors"
	"fmt"

	"github.com/nicolerenee/promptbook/internal/ent"
)

// errNodeUnsupported is returned by the Relay node/nodes resolvers
// because promptbook does not maintain ent's `ent_types` table — that
// table only exists when migrations run via ent's
// migrate.WithGlobalUniqueID; we own our schema via goose. Callers
// should reach the typed `recording(id:)` / `show(id:)` / etc.
// shortcuts defined in schema.graphql instead.
//
// Lives in this file (not the gqlgen-managed *.resolvers.go) so the
// helper survives gqlgen regeneration runs.
var errNodeUnsupported = errors.New(
	"graphql: node(id:) lookups are not supported; use the typed " +
		"recording/show/performer/collectionEntry/wantsEntry fields",
)

// nilOnNotFound returns (nil, nil) when err is an ent NotFoundError so
// the gqlgen field resolver renders a `null` for the GraphQL field
// (the GraphQL convention for "no such row") instead of bubbling a
// hard error. Any other error passes through wrapped for context.
//
// (nil, nil) is the GraphQL idiom for "field exists, no value" — the
// alternative would be making the per-type fetchers non-null and
// returning errors for missing rows, which is strictly worse UX.
//
// Lives in this file (not the gqlgen-managed *.resolvers.go) so the
// helper survives gqlgen regeneration runs.
func nilOnNotFound[T any](node *T, err error, what string) (*T, error) {
	if err == nil {
		return node, nil
	}
	var nfe *ent.NotFoundError
	if errors.As(err, &nfe) {
		return nil, nil //nolint:nilnil // (nil, nil) signals "no row" to gqlgen.
	}
	return nil, fmt.Errorf("graphql: load %s: %w", what, err)
}
