package schema

// Dialect-specific schema-type literals shared across the schema files.
// Centralizing them here also makes future "rename sqlite3 -> sqlite"
// or column-type tweaks a one-stop edit.
const (
	dialectSQLite3 = "sqlite3"
	typeDatetime   = "datetime"
	typeTimestamp  = "timestamp"
)

// sqliteSchema returns the SchemaType map for the given on-disk type
// keyed by the SQLite dialect. Centralizing the helper saves a
// repeated map-literal in every Time field.
func sqliteSchema(t string) map[string]string {
	return map[string]string{dialectSQLite3: t}
}
