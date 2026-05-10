//go:build ignore

// entc.go is the ent codegen entry point. Run `go generate ./internal/ent`
// (or `go run -mod=mod entc.go` from this directory) to regenerate the
// client under internal/ent. The entgql extension is wired in so a
// future GraphQL phase can lift the schema without touching the
// generator config.
package main

import (
	"log"

	"entgo.io/ent/entc"
	"entgo.io/ent/entc/gen"
)

func main() {
	// Phase 1: stick with the stock entc generator. The entgql
	// extension lights up in Phase 3 when we add a /graphql endpoint;
	// adding it here today triggers entgql's "mixed id types" check
	// against Profile's int id vs the int64 PKs everywhere else
	// without giving us anything we use yet.
	cfg := &gen.Config{
		Target:  "./",
		Package: "github.com/nicolerenee/promptbook/internal/ent",
		Features: []gen.Feature{
			gen.FeatureVersionedMigration,
			gen.FeatureUpsert,
			gen.FeatureModifier,
		},
	}

	if err := entc.Generate("./schema", cfg); err != nil {
		log.Fatalf("running ent codegen: %v", err)
	}
}
