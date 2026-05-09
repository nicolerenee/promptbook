package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
)

// WantsEntry maps to `wants` — 1:1 with the user's wants list.
// recording_id is both PK and FK, modeled the same way as
// CollectionEntry.
type WantsEntry struct {
	ent.Schema
}

// Annotations sets the table name to `wants`.
func (WantsEntry) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "wants"},
	}
}

// Fields of WantsEntry.
func (WantsEntry) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id").
			StorageKey("recording_id").
			Immutable(),
		field.Time("last_synced_at").
			Default(time.Now).
			SchemaType(sqliteSchema(typeDatetime)).
			Annotations(entsql.Default("CURRENT_TIMESTAMP")),
	}
}

// Edges of WantsEntry. See the note on CollectionEntry.Edges — same
// shared-PK rationale.
func (WantsEntry) Edges() []ent.Edge {
	return nil
}
