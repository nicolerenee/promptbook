package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// CastEntry maps to `cast_entries` — many rows per recording, one per
// performer-character pairing. The denormalized performer/character
// columns are kept for downstream consumers that haven't migrated to
// joining through the canonical performers/characters tables.
type CastEntry struct {
	ent.Schema
}

// Annotations sets the table name to `cast_entries`.
func (CastEntry) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "cast_entries"},
	}
}

// Fields of CastEntry.
func (CastEntry) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("recording_id"),
		field.Int64("performer_id"),
		field.Text("performer_name"),
		field.Text("performer_slug").Default(""),
		field.Text("performer_url").Default(""),
		field.Int64("character_id"),
		field.Text("character_name"),
		field.Text("character_slug").Default(""),
		field.Text("character_url").Default(""),
		field.Int("character_order").Default(0),
		field.Text("status_label").Optional().Nillable(),
		field.Text("status_abbrev").Optional().Nillable(),
	}
}

// Edges of CastEntry.
func (CastEntry) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("recording", Recording.Type).
			Ref("cast_entries").
			Field("recording_id").
			Unique().
			Required(),
	}
}

// Indexes of CastEntry.
func (CastEntry) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("recording_id"),
		index.Fields("performer_id"),
	}
}
