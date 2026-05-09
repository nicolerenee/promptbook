package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
)

// Profile maps to the singleton `profile` table — id is constrained
// to 1 in the existing schema; we model it as a normal int PK and
// rely on a CHECK constraint emitted by ent's atlas dialect.
type Profile struct {
	ent.Schema
}

// Annotations sets the table name to `profile` and adds the singleton
// CHECK constraint that's been on the table since migration 7.
func (Profile) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{
			Table: "profile",
			Check: "id = 1",
		},
	}
}

// Fields of Profile.
func (Profile) Fields() []ent.Field {
	return []ent.Field{
		field.Int("id").Immutable(),
		field.Int64("encora_id"),
		field.Text("name").Default(""),
		field.Text("slug").Default(""),
		field.Text("username").Default(""),
		field.Text("status").Default(""),
		field.Int("recordings_count").Default(0),
		field.Int("wants_count").Default(0),
		field.Text("last_seen_at").Default(""),
		field.Text("profile_visibility").Default(""),
		field.Text("col_visibility").Default(""),
		field.Time("last_synced_at").
			Default(time.Now).
			SchemaType(sqliteSchema(typeDatetime)).
			Annotations(entsql.Default("CURRENT_TIMESTAMP")),
	}
}
