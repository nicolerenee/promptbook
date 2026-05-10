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

// Recording maps to the `recordings` table — one row per Encora
// recording. raw_json stores the JSON-encoded encora.Recording payload
// so callers can re-derive any field that wasn't denormalized.
type Recording struct {
	ent.Schema
}

// Annotations sets the table name to `recordings` and exposes the
// type to GraphQL with a `recording(id:)` query field plus a Relay
// `recordings` connection.
func (Recording) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "recordings"},
		entgql.RelayConnection(),
		entgql.QueryField(),
	}
}

// Fields of Recording.
func (Recording) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id").
			StorageKey("recording_id").
			Immutable(),
		field.Int64("show_id"),
		field.Text("tour").Default("").
			Annotations(entgql.OrderField("TOUR")),
		field.Text("date_full").Default("").
			Annotations(entgql.OrderField("DATE_FULL")),
		field.Bool("date_month_known").Default(false),
		field.Bool("date_day_known").Default(false),
		field.Text("date_variant").Optional().Nillable(),
		field.Text("date_time").Default("unknown"),
		field.Text("master").Default("").
			Annotations(entgql.OrderField("MASTER")),
		field.Text("nft_date").Optional().Nillable(),
		field.Bool("nft_forever").Default(false),
		field.Text("notes").Default(""),
		field.Text("master_notes").Optional().Nillable(),
		// release_format holds the encora-supplied free-text format
		// string (e.g. "MKV (1080p - h.264) - 4.20 GB"). Kept on disk
		// for completeness in raw_json round-trips, but skipped from
		// the GraphQL surface — display callers read the locally-
		// derived Recording.releaseFormat enrichment field, which is
		// computed from the recording's versions + their MediaInfo.
		field.Text("release_format").Optional().Nillable().
			Annotations(entgql.Skip(entgql.SkipType)),
		field.Text("venue").Default(""),
		field.Text("city").Default(""),
		field.Text("media_type").Default(""),
		field.Text("recording_type").Default(""),
		field.Text("amount_recorded").Default(""),
		field.Text("gifting_status").Default(""),
		field.Text("limited_status").Default(""),
		field.Bool("is_opening").Default(false),
		field.Bool("is_closing").Default(false),
		field.Bool("is_preview").Default(false),
		field.Bool("is_concert").Default(false),
		field.Bool("is_nfs").Default(false),
		field.Bool("is_favourite").Default(false),
		field.Bool("has_screenshots").Default(false),
		field.Bool("has_subtitles").Default(false),
		field.Bool("boot_camp_recommended").Default(false),
		field.Int("owners_count").Default(0),
		field.Int("wanters_count").Default(0),
		field.Text("last_updated").Default("").
			Annotations(entgql.OrderField("LAST_UPDATED")),
		// raw_json holds the JSON-encoded encora.Recording. Plain Text
		// keeps marshaling simple — callers serialize/deserialize at
		// the package boundary.
		field.Text("raw_json"),
		field.Time("last_seen_at").
			Default(time.Now).
			SchemaType(sqliteSchema(typeDatetime)).
			Annotations(entsql.Default("CURRENT_TIMESTAMP")),
	}
}

// Edges of Recording.
//
// Note: collection / wants / image_choices are NOT declared as edges
// here. They share recordings.recording_id as both PK and FK, which
// ent's edge model can't express without an extra column. The FK
// wiring lives at the SQL layer (the migration emits the foreign-key
// references); the ent client treats them as siblings keyed by the
// same int64, and storage helpers join them manually for now.
func (Recording) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("show", Show.Type).
			Ref("recordings").
			Field("show_id").
			Unique().
			Required(),
		edge.To("cast_entries", CastEntry.Type).
			Annotations(
				entsql.OnDelete(entsql.Cascade),
				entgql.RelayConnection(),
			),
		edge.To("versions", RecordingVersion.Type).
			Annotations(
				entsql.OnDelete(entsql.Cascade),
				entgql.RelayConnection(),
			),
		// extras is the back-edge to the recording_extras rows. The
		// edge is annotation-skipped so it doesn't surface as a Relay
		// connection on the generated GraphQL — the SPA reads extras
		// through the hand-rolled Recording.extras resolver instead.
		// Points at ExtraEntry (rather than a same-named
		// RecordingExtra schema) to avoid colliding with the
		// hand-rolled GraphQL `RecordingExtra` model gqlgen would
		// otherwise autobind by name.
		edge.To("extras", ExtraEntry.Type).
			Annotations(
				entsql.OnDelete(entsql.Cascade),
				entgql.Skip(entgql.SkipAll),
			),
	}
}

// Indexes of Recording.
func (Recording) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("show_id"),
	}
}
