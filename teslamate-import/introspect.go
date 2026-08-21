package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// tableColumns discovers which columns actually exist on table in the
// source database via information_schema, so import queries can adapt
// to schema drift across TeslaMate versions (e.g. ascent/descent were
// added to drives in a 2025 migration; not every install has them)
// instead of hardcoding a column list that might 42703 on an older or
// newer TeslaMate schema than the one this tool was written against.
func tableColumns(ctx context.Context, pg *pgx.Conn, table string) (map[string]bool, error) {
	rows, err := pg.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_schema='public' AND table_name=$1`, table)
	if err != nil {
		return nil, fmt.Errorf("introspect columns of %s: %w", table, err)
	}
	defer rows.Close()

	cols := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("table %q not found in source database (empty column set)", table)
	}
	return cols, nil
}

// pick returns column if it exists in cols, or the literal SQL "NULL"
// otherwise - used to build SELECT lists that degrade gracefully on
// fields a particular TeslaMate version/schema doesn't have, instead
// of failing the whole import over one optional column.
func pick(cols map[string]bool, column string) string {
	if cols[column] {
		return column
	}
	return "NULL"
}

// pickQualified is pick, but for a query that table-qualifies its
// columns (e.g. "d.start_km") - NULL must stay unqualified ("d.NULL"
// is a syntax error), so this only prepends alias+"." when the column
// actually exists.
func pickQualified(cols map[string]bool, alias, column string) string {
	if cols[column] {
		return alias + "." + column
	}
	return "NULL"
}
