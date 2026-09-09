package migrate

import "strings"

// coerce adapts a value read from one database to the type the other declares.
//
// Driven by the target's reported type rather than by a list of column names,
// so it stays correct when a column is added — and so it is not a third
// definition of the schema.
//
// Only one direction needs it in practice. SQLite has no boolean, so the three
// flag columns come back as int64; PostgreSQL's are real booleans and reject
// an integer outright. Everything else — text, integers, bytea from BLOB —
// crosses unchanged, which is why this function is small rather than a
// conversion table.
func coerce(v any, targetType string) any {
	if v == nil {
		return nil
	}
	switch normalizeType(targetType) {
	case "boolean":
		switch n := v.(type) {
		case int64:
			return n != 0
		case bool:
			return n
		}
	}
	return v
}

// normalizeType reduces a dialect's spelling to the shape that matters.
//
// PostgreSQL's information_schema says "boolean"; SQLite's pragma says
// "INTEGER" or "BOOLEAN" depending on how the column was declared, and a
// column with no declared type says "". Compared case-insensitively and by
// prefix, because "character varying(64)" and "timestamp with time zone" carry
// detail this does not need.
func normalizeType(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	switch {
	case strings.HasPrefix(t, "bool"):
		return "boolean"
	default:
		return t
	}
}
