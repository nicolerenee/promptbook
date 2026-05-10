package graph

// Two-stage codegen lives behind `go generate ./internal/...`:
//
//  1. `go generate ./internal/ent` runs entc + the entgql extension —
//     produces internal/ent/gql_*.go and the ent.graphql schema in
//     this directory.
//  2. `go generate ./internal/server/graph` runs gqlgen — reads the
//     two .graphql files and (re)writes generated.go +
//     {ent,schema}.resolvers.go in this directory. Hand-written
//     resolver bodies survive the regenerate pass; only the function
//     signatures stay sourced from the schema.
//
// Run them in order. Calling `go generate ./...` does that
// automatically because internal/ent sorts before internal/server/graph.

//go:generate go run -mod=mod github.com/99designs/gqlgen generate
