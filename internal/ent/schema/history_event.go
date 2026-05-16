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

// HistoryEvent maps to the `history` table — auto-increment id with a
// nullable recording_id (events not tied to a specific recording set
// it to NULL).
type HistoryEvent struct {
	ent.Schema
}

// Annotations sets the table name to `history`. The type is hidden
// from GraphQL — its default `int` PK would clash with the `int64` IDs
// on the in-scope nodes; the history surface stays REST-only.
func (HistoryEvent) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "history"},
		entgql.Skip(entgql.SkipAll),
	}
}

// Fields of HistoryEvent.
func (HistoryEvent) Fields() []ent.Field {
	return []ent.Field{
		field.Time("occurred_at").
			Default(time.Now).
			SchemaType(sqliteSchema(typeDatetime)).
			Annotations(entsql.Default("CURRENT_TIMESTAMP")),
		field.Text("kind"),
		field.Int64("recording_id").Optional().Nillable(),
		field.Text("summary").Default(""),
		field.Text("details_json").Default("{}"),
	}
}

// Indexes of HistoryEvent.
func (HistoryEvent) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("occurred_at"),
		index.Fields("recording_id"),
		index.Fields("kind"),
	}
}
