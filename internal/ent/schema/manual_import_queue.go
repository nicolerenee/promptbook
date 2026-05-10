package schema

import (
	"time"

	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// ManualImportQueue maps to `manual_import_queue` — files discovered
// on disk that don't yet correspond to a known recording.
type ManualImportQueue struct {
	ent.Schema
}

// Annotations sets the table name to `manual_import_queue`. The type
// is hidden from GraphQL — its default `int` PK would clash with the
// `int64` IDs on the in-scope nodes; the queue surface stays REST-only.
func (ManualImportQueue) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "manual_import_queue"},
		entgql.Skip(entgql.SkipAll),
	}
}

// Fields of ManualImportQueue.
func (ManualImportQueue) Fields() []ent.Field {
	return []ent.Field{
		field.Text("file_path").Unique(),
		field.Int64("file_size_bytes").Default(0),
		field.Time("discovered_at").
			Default(time.Now).
			SchemaType(sqliteSchema(typeDatetime)).
			Annotations(entsql.Default("CURRENT_TIMESTAMP")),
		field.Time("last_seen_at").
			Default(time.Now).
			SchemaType(sqliteSchema(typeDatetime)).
			Annotations(entsql.Default("CURRENT_TIMESTAMP")),
		field.Int64("suggested_recording_id").Optional().Nillable(),
		field.Text("suggested_confidence").Default(""),
		field.Text("notes").Default(""),
	}
}

// Indexes of ManualImportQueue.
func (ManualImportQueue) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("discovered_at"),
	}
}
