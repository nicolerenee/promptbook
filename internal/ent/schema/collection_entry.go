package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
)

// CollectionEntry maps to `collection` — 1:1 with the user's owned
// recordings. recording_id is both PK and FK; we model it as ent's `id`
// field with a storage-key override so the row is keyed by the upstream
// recording ID rather than an auto-increment integer.
type CollectionEntry struct {
	ent.Schema
}

// Annotations sets the table name to `collection`.
func (CollectionEntry) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "collection"},
	}
}

// Fields of CollectionEntry.
func (CollectionEntry) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id").
			StorageKey("recording_id").
			Immutable(),
		field.Text("format").Default(""),
		field.Text("user_notes").Optional().Nillable(),
		field.Bool("user_watched").Default(false),
		field.Time("collected_at").
			Optional().
			Nillable().
			SchemaType(sqliteSchema(typeDatetime)),
		field.Time("updated_at").
			Optional().
			Nillable().
			SchemaType(sqliteSchema(typeDatetime)),
		field.Time("last_synced_at").
			Default(time.Now).
			SchemaType(sqliteSchema(typeDatetime)).
			Annotations(entsql.Default("CURRENT_TIMESTAMP")),
	}
}

// Edges of CollectionEntry.
//
// Note: the FK to recordings is implicit via the shared PK
// (id == recording_id at the SQL layer). We do not declare an ent
// edge here because ent's edge-field model requires a non-id FK
// column; expressing the PK-as-FK relationship purely through the
// storage-key override keeps the on-disk schema identical to the
// hand-rolled migration without forcing an extra column.
func (CollectionEntry) Edges() []ent.Edge {
	return nil
}
