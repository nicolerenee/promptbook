package schema

import (
	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// JobRun maps to `job_runs` — one row per scheduled-job invocation.
// status and trigger are kept as plain strings (not enums) to match
// the existing migration's TEXT NOT NULL columns.
type JobRun struct {
	ent.Schema
}

// Annotations sets the table name to `job_runs`. The type is hidden
// from GraphQL — its default `int` PK would clash with the `int64` IDs
// on Recording/Show/Performer; the jobs surface stays REST-only.
func (JobRun) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "job_runs"},
		entgql.Skip(entgql.SkipAll),
	}
}

// Fields of JobRun.
func (JobRun) Fields() []ent.Field {
	return []ent.Field{
		field.Text("job_name"),
		field.Time("queued_at").
			SchemaType(sqliteSchema(typeTimestamp)),
		field.Time("started_at").
			Optional().
			Nillable().
			SchemaType(sqliteSchema(typeTimestamp)),
		field.Time("ended_at").
			Optional().
			Nillable().
			SchemaType(sqliteSchema(typeTimestamp)),
		field.Text("status"),
		field.Text("error").Default(""),
		field.Text("trigger"),
		field.Text("args").Default(""),
	}
}

// Indexes of JobRun.
func (JobRun) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("job_name", "queued_at").
			StorageKey("idx_job_runs_name_queued").
			Annotations(entsql.Desc()),
		index.Fields("queued_at").
			StorageKey("idx_job_runs_queued").
			Annotations(entsql.Desc()),
	}
}
