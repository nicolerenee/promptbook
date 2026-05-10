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

// ExtraEntry maps to `recording_extras` — one row per non-main file
// moved into a recording's canonical folder (featurettes, behind-
// the-scenes clips, audio rips, photos, etc.). The set of rows for
// a recording is the durable answer to "what bonus content lives
// next to this recording"; legacy folder-walking at read time
// (Recording.extras pre-phase-1) was a v0 of the same idea.
//
// Each row points at a real file under the recording's folder; the
// `kind` column maps to Jellyfin's extras subfolder vocabulary
// (featurettes/, scenes/, behindthescenes/, ...) plus two
// promptbook-only kinds (`audio`, `photo`) for content Jellyfin
// doesn't natively surface. The (recording_id, file_path) pair is
// unique so re-ingesting the same file is an upsert, not a
// duplicate.
//
// The ent type is named `ExtraEntry` rather than `RecordingExtra`
// to dodge a gqlgen autobind collision: the GraphQL surface keeps
// a hand-rolled `RecordingExtra` model (path/name/sizeBytes/isDir
// /kind/label) that the Recording.extras resolver composes from
// these rows; if the ent schema type matched that name, gqlgen
// would autobind the ent struct over the GraphQL one.
type ExtraEntry struct {
	ent.Schema
}

// Annotations names the table `recording_extras` and skips the type
// from the entgql-generated GraphQL surface — the SPA reads extras
// through the hand-rolled Recording.extras resolver, so the auto-
// generated Relay connection isn't needed and would force a Node
// implementation we don't want.
func (ExtraEntry) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "recording_extras"},
		entgql.Skip(entgql.SkipAll),
	}
}

// Fields of ExtraEntry.
func (ExtraEntry) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Int64("recording_id"),
		// file_path is the canonical absolute path of the moved file
		// (under the recording's folder + Jellyfin-shaped subfolder
		// for the kind, e.g. /library/.../featurettes/bows.mp4).
		field.Text("file_path"),
		// kind is one of Jellyfin's extras subfolder vocabulary
		// (featurette / scene / behindthescenes / interview /
		// trailer / deletedscenes / other) plus promptbook-only
		// audio / photo. Empty string means "legacy import" — the
		// resolver renders those as plain unlabeled rows.
		field.Text("kind"),
		// label is the optional user-supplied display label (e.g.
		// "Bows — Baker / Marinerage"). Empty when no label was set;
		// the SPA falls back to the file basename.
		field.Text("label").Default(""),
		field.Int64("file_size_bytes").Default(0),
		field.Time("added_at").
			Default(time.Now).
			SchemaType(sqliteSchema(typeDatetime)).
			Annotations(entsql.Default("CURRENT_TIMESTAMP")),
	}
}

// Edges of ExtraEntry.
func (ExtraEntry) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("recording", Recording.Type).
			Ref("extras").
			Field("recording_id").
			Unique().
			Required(),
	}
}

// Indexes of ExtraEntry.
func (ExtraEntry) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("recording_id"),
		// (recording_id, file_path) unique so re-ingesting the same
		// file is an upsert — the kind/label/size can change but the
		// row identity is the on-disk path.
		index.Fields("recording_id", "file_path").Unique(),
	}
}
