// Package ent hosts the generated ent client. Schemas live in
// internal/ent/schema; everything else under this directory is produced
// by `go generate ./internal/ent` (i.e. by entc.go).
//
// The same generate run also writes ../server/graph/ent.graphql via
// entgql; the gqlgen pass that translates that schema into resolver
// stubs is wired separately under internal/server/graph.
package ent

//go:generate go run -mod=mod entc.go
