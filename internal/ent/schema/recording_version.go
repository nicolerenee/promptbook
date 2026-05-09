package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// RecordingVersion maps to `recording_versions` — one row per local
// file that backs a recording. Multiple versions per recording are
// uniquely identified by (recording_id, file_path).
type RecordingVersion struct {
	ent.Schema
}

// Annotations sets the table name to `recording_versions`.
func (RecordingVersion) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "recording_versions"},
	}
}

// Fields of RecordingVersion.
func (RecordingVersion) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("recording_id"),
		field.Text("file_path"),
		field.Int64("file_size_bytes").Default(0),
		field.Text("container").Default(""),
		field.Text("quality").Default(""),
		field.Text("video_codec").Default(""),
		field.Text("audio_codec").Default(""),
		field.Text("format_label").Default(""),
		field.Text("notes").Default(""),
		field.Time("added_at").
			Default(time.Now).
			SchemaType(sqliteSchema(typeDatetime)).
			Annotations(entsql.Default("CURRENT_TIMESTAMP")),
		field.Time("last_seen_at").
			Default(time.Now).
			SchemaType(sqliteSchema(typeDatetime)).
			Annotations(entsql.Default("CURRENT_TIMESTAMP")),
	}
}

// Edges of RecordingVersion.
func (RecordingVersion) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("recording", Recording.Type).
			Ref("versions").
			Field("recording_id").
			Unique().
			Required(),
	}
}

// Indexes of RecordingVersion.
func (RecordingVersion) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("recording_id"),
		index.Fields("recording_id", "file_path").Unique(),
	}
}
