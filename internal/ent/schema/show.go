// Package schema holds the ent schema definitions for promptbook's data
// layer. Each file describes one entity; ent codegen turns them into a
// fully typed client under internal/ent.
package schema

import (
	"time"

	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// Show maps to the `shows` table. The PK is the upstream Encora show_id;
// we set ID immutable so callers must populate it explicitly during
// sync — there is no auto-increment here.
type Show struct {
	ent.Schema
}

// Annotations sets the table name to `shows` and exposes the type to
// GraphQL with a `show(id:)` query field plus a Relay `shows` connection.
func (Show) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "shows"},
		entgql.RelayConnection(),
		entgql.QueryField(),
	}
}

// Fields of Show.
func (Show) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id").
			StorageKey("show_id").
			Immutable(),
		field.Text("name").
			Annotations(entgql.OrderField("NAME")),
		field.Text("description_html").
			Default(""),
		field.Time("last_seen_at").
			Default(time.Now).
			SchemaType(sqliteSchema(typeDatetime)).
			Annotations(entsql.Default("CURRENT_TIMESTAMP")),
	}
}

// Edges of Show.
func (Show) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("recordings", Recording.Type).
			Annotations(entgql.RelayConnection()),
	}
}
