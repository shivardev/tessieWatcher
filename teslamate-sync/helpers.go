package main

import "database/sql"

// Small conversion helpers: database/sql's Null* wrapper types in ->
// plain Go pointers out. pgx treats a nil pointer as SQL NULL, so this
// is the one place NULL-ness gets translated between the two driver
// worlds, rather than scattering `.Valid` checks through every sync
// function.

func nullString(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}

func nullFloat(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	f := v.Float64
	return &f
}

func nullInt(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	i := v.Int64
	return &i
}

// nullBool converts teslalog's SQLite INTEGER-as-bool columns (0/1,
// scanned as sql.NullInt64) to a bool pointer.
func nullBool(v sql.NullInt64) *bool {
	if !v.Valid {
		return nil
	}
	b := v.Int64 != 0
	return &b
}
