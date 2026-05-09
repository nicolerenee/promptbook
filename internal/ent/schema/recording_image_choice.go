package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
)

// RecordingImageChoice maps to `recording_image_choices` — 1:1 with
// recordings, recording_id is both PK and FK.
type RecordingImageChoice struct {
	ent.Schema
}

// Annotations sets the table name to `recording_image_choices`.
func (RecordingImageChoice) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "recording_image_choices"},
	}
}

// Fields of RecordingImageChoice.
func (RecordingImageChoice) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id").
			StorageKey("recording_id").
			Immutable(),
		field.Text("overlay_text_override").Optional().Nillable(),
		// overlay_style_json is a JSON blob stored as TEXT.
		field.Text("overlay_style_json").Optional().Nillable(),
		field.Bool("overlay_disabled").Default(false),
		field.Time("updated_at").
			Default(time.Now).
			SchemaType(sqliteSchema(typeTimestamp)).
			Annotations(entsql.Default("CURRENT_TIMESTAMP")),
	}
}

// Edges of RecordingImageChoice. See the note on
// CollectionEntry.Edges — same shared-PK rationale.
func (RecordingImageChoice) Edges() []ent.Edge {
	return nil
}
