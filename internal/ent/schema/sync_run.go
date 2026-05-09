package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// SyncRun maps to `sync_runs` — one row per call to sync.Sync. The
// `id` is auto-increment (no natural key from upstream).
type SyncRun struct {
	ent.Schema
}

// Annotations sets the table name to `sync_runs`.
func (SyncRun) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "sync_runs"},
	}
}

// Fields of SyncRun.
func (SyncRun) Fields() []ent.Field {
	return []ent.Field{
		field.Text("kind"),
		field.Time("started_at").
			SchemaType(sqliteSchema(typeDatetime)),
		field.Time("finished_at").
			Optional().
			Nillable().
			SchemaType(sqliteSchema(typeDatetime)),
		field.Int("ok_count").Default(0),
		field.Int("error_count").Default(0),
		field.Int("rate_limit_remaining").Default(0),
		field.Text("error_text").Default(""),
	}
}

// Indexes of SyncRun.
func (SyncRun) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("started_at"),
	}
}
