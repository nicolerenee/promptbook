package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
)

// JobState maps to `job_state` — one row per job_name with the
// last-known status buttonshot. job_name is the natural PK.
type JobState struct {
	ent.Schema
}

// Annotations sets the table name to `job_state`.
func (JobState) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "job_state"},
	}
}

// Fields of JobState.
func (JobState) Fields() []ent.Field {
	return []ent.Field{
		field.Text("id").
			StorageKey("job_name").
			Immutable(),
		field.Time("last_started_at").
			Optional().
			Nillable().
			SchemaType(sqliteSchema(typeTimestamp)),
		field.Time("last_ended_at").
			Optional().
			Nillable().
			SchemaType(sqliteSchema(typeTimestamp)),
		field.Int("last_duration_ms").Optional().Nillable(),
		field.Text("last_status").Optional().Nillable(),
	}
}
