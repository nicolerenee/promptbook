//go:build ignore

// entc.go is the ent codegen entry point. Run `go generate ./internal/ent`
// (or `go run -mod=mod entc.go` from this directory) to regenerate the
// client under internal/ent. The entgql extension is wired in so the
// /graphql endpoint (Phase 3) can lift the catalog schema directly from
// the ent graph.
package main

import (
	"log"

	"entgo.io/contrib/entgql"
	"entgo.io/ent/entc"
	"entgo.io/ent/entc/gen"
)

func main() {
	// Wire the entgql extension. Out-of-scope entities (Profile, JobRun,
	// JobState, HistoryEvent, ManualImportQueue, Character,
	// RecordingImageChoice) carry entgql.Skip(entgql.SkipAll) on their
	// schema annotations so they never appear in the generated GraphQL
	// surface — that's what keeps mixed-id-types (int / string) out of
	// the relay schema while leaving the rest of the catalog reachable.
	//
	// The schema is written next to the gqlgen config + resolvers under
	// internal/server/graph so the server package can embed it directly.
	ex, err := entgql.NewExtension(
		entgql.WithSchemaGenerator(),
		entgql.WithSchemaPath("../server/graph/ent.graphql"),
		entgql.WithConfigPath("../server/graph/gqlgen.yml"),
		entgql.WithWhereInputs(true),
		entgql.WithNodeDescriptor(true),
	)
	if err != nil {
		log.Fatalf("creating entgql extension: %v", err)
	}

	cfg := &gen.Config{
		Target:  "./",
		Package: "github.com/nicolerenee/promptbook/internal/ent",
		Features: []gen.Feature{
			gen.FeatureVersionedMigration,
			gen.FeatureUpsert,
			gen.FeatureModifier,
		},
	}

	if err := entc.Generate("./schema", cfg, entc.Extensions(ex)); err != nil {
		log.Fatalf("running ent codegen: %v", err)
	}
}
