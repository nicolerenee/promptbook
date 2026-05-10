package schema

import (
	"entgo.io/contrib/entgql"
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

// Annotations sets the table name to `sync_runs` and exposes the
// type to GraphQL with a Relay `syncRuns` connection.
func (SyncRun) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "sync_runs"},
		entgql.RelayConnection(),
		entgql.QueryField(),
	}
}

// Fields of SyncRun.
//
// The auto-increment `id` is declared explicitly as Int64 so it shares
// the same Go type as Recording/Show/Performer IDs — entgql refuses to
// generate a Relay schema with mixed PK Go types.
func (SyncRun) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Text("kind"),
		field.Time("started_at").
			SchemaType(sqliteSchema(typeDatetime)).
			Annotations(entgql.OrderField("STARTED_AT")),
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
