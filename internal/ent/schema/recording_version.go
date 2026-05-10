package schema

import (
	"time"

	"entgo.io/contrib/entgql"
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

// Annotations sets the table name to `recording_versions`. Versions
// are reachable through the `Recording.versions` Relay connection.
func (RecordingVersion) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "recording_versions"},
		entgql.RelayConnection(),
	}
}

// Fields of RecordingVersion.
//
// The auto-increment `id` is declared explicitly as Int64 so it shares
// the same Go type as Recording/Show/Performer IDs — entgql refuses to
// generate a Relay schema with mixed PK Go types.
func (RecordingVersion) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Int64("recording_id"),
		field.Text("file_path"),
		field.Int64("file_size_bytes").Default(0),
		field.Text("container").Default(""),
		field.Text("quality").Default(""),
		field.Text("video_codec").Default(""),
		field.Text("audio_codec").Default(""),
		field.Text("format_label").Default(""),
		field.Text("notes").Default(""),
		// media_info_json is the JSON-encoded probe.MediaInfo for this
		// version's source file. Persisted as a single TEXT blob (not
		// normalized to columns) because the data is display-only and
		// the shape evolves as ffprobe surfaces add fields. Empty
		// string when no probe ran (legacy imports before the field
		// existed); the GraphQL resolver decodes "" / "{}" / null as
		// "no media info".
		field.Text("media_info_json").Default(""),
		// source_folder is the directory the source file lived in at
		// ingest time, captured for folder-as-unit drops so the
		// recording detail page can enumerate sibling files (audio/,
		// photos/, etc.) as 'extras' even though the canonical move
		// only takes the main file. Empty for loose-file imports — the
		// recording's destination folder is the only location with
		// content.
		field.Text("source_folder").Default(""),
		// part_index is the 1-based ordinal of this file when the
		// version is split across multiple files (act-1 + act-2, pt-1
		// + pt-2, etc.). Zero means single-file (the default — most
		// recordings). Multiple rows with the same recording_id and
		// part_index >= 1 form one multipart version. Surfaced through
		// the rename engine's {Part} token.
		field.Int("part_index").Default(0),
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
