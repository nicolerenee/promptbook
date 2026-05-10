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

// Performer is the canonical actor entity — one row per Encora
// performer ID. cast_entries denormalizes the name/slug/url for legacy
// readers, but performers is the source of truth.
type Performer struct {
	ent.Schema
}

// Annotations sets the table name to `performers` and exposes the
// type to GraphQL with a `performer(id:)` query field plus a Relay
// `performers` connection.
func (Performer) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "performers"},
		entgql.RelayConnection(),
		entgql.QueryField(),
	}
}

// Fields of Performer.
func (Performer) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id").
			StorageKey("performer_id").
			Immutable(),
		field.Text("name").
			Annotations(entgql.OrderField("NAME")),
		field.Text("slug").Default(""),
		field.Text("url").Default(""),
		field.Time("last_seen_at").
			Default(time.Now).
			SchemaType(sqliteSchema(typeDatetime)).
			Annotations(entsql.Default("CURRENT_TIMESTAMP")),
	}
}

// Indexes of Performer.
func (Performer) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("name"),
	}
}
