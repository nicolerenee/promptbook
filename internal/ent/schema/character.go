package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
)

// Character is the canonical role entity — one row per Encora
// character ID.
type Character struct {
	ent.Schema
}

// Annotations sets the table name to `characters`.
func (Character) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "characters"},
	}
}

// Fields of Character.
func (Character) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id").
			StorageKey("character_id").
			Immutable(),
		field.Text("name"),
		field.Text("slug").Default(""),
		field.Text("url").Default(""),
		field.Time("last_seen_at").
			Default(time.Now).
			SchemaType(sqliteSchema(typeDatetime)).
			Annotations(entsql.Default("CURRENT_TIMESTAMP")),
	}
}
