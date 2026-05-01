package dataxfer

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// SourceDialect isolates the quoting + placeholder + catalog-query variations
// between source DB engines so partitioner.go and copier.go stay agnostic.
//
// Every implementation must honour the three contracts used at run time:
//   - Quote(id) returns the dialect-appropriate quoted identifier.
//   - Placeholders are positional 1-based; `PlaceholderAt(1)` returns the
//     first placeholder ("?" for MySQL, ":1" for Oracle, etc.). Callers
//     build the args slice in the same order.
//   - The three query builders below return ready-to-Exec SQL plus an args
//     slice when the dialect needs bind arguments for catalog lookups.
type SourceDialect interface {
	Kind() string
	Quote(id string) string
	Qualify(schema, name string) string
	PlaceholderAt(n int) string

	CountQuery(schema, table string) string
	MinMaxQuery(schema, table, pkCol string) string
	SelectRangeQuery(schema, table string, cols []string, rangeCol string) string
	SelectOffsetQuery(schema, table string, cols []string) string

	// Catalog lookups — the caller passes (owner, table[, column]) as
	// positional args; the returned SQL uses the dialect's native
	// placeholders.
	ListColumnsQuery() string
	ColumnDataTypeQuery() string
	PKColumnsQuery() string
	IsNumericDataType(dt string) bool
}

// ---------------------------------------------------------------------------
// MySQL / MariaDB dialect
// ---------------------------------------------------------------------------

type mysqlDialect struct{}

func MySQLSource() SourceDialect { return mysqlDialect{} }

func (mysqlDialect) Kind() string            { return "mysql" }
func (mysqlDialect) Quote(id string) string  { return "`" + strings.ReplaceAll(id, "`", "``") + "`" }
func (mysqlDialect) PlaceholderAt(n int) string { return "?" }
func (d mysqlDialect) Qualify(schema, name string) string {
	if schema == "" {
		return d.Quote(name)
	}
	return d.Quote(schema) + "." + d.Quote(name)
}

func (d mysqlDialect) CountQuery(schema, table string) string {
	return "SELECT COUNT(*) FROM " + d.Qualify(schema, table)
}
func (d mysqlDialect) MinMaxQuery(schema, table, pkCol string) string {
	return fmt.Sprintf("SELECT MIN(%s), MAX(%s) FROM %s",
		d.Quote(pkCol), d.Quote(pkCol), d.Qualify(schema, table))
}
func (d mysqlDialect) SelectRangeQuery(schema, table string, cols []string, rangeCol string) string {
	return fmt.Sprintf("SELECT %s FROM %s WHERE %s BETWEEN %s AND %s ORDER BY %s",
		joinQuoted(d, cols), d.Qualify(schema, table),
		d.Quote(rangeCol), d.PlaceholderAt(1), d.PlaceholderAt(2), d.Quote(rangeCol))
}
func (d mysqlDialect) SelectOffsetQuery(schema, table string, cols []string) string {
	// MySQL's non-standard LIMIT ? OFFSET ? — limit first to match existing arg order.
	return fmt.Sprintf("SELECT %s FROM %s LIMIT %s OFFSET %s",
		joinQuoted(d, cols), d.Qualify(schema, table),
		d.PlaceholderAt(1), d.PlaceholderAt(2))
}

// ListColumnsQuery — skip generated columns and spatial types that the
// copier doesn't know how to coerce into PG binary format.
func (mysqlDialect) ListColumnsQuery() string {
	return `
		SELECT COLUMN_NAME FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA=? AND TABLE_NAME=?
		   AND EXTRA NOT LIKE '%VIRTUAL%'
		   AND EXTRA NOT LIKE '%STORED GENERATED%'
		   AND DATA_TYPE NOT IN ('geometry','point','linestring','polygon',
		                         'multipoint','multilinestring','multipolygon',
		                         'geometrycollection')
		 ORDER BY ORDINAL_POSITION`
}
func (mysqlDialect) ColumnDataTypeQuery() string {
	return `SELECT DATA_TYPE FROM information_schema.COLUMNS
	         WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND COLUMN_NAME=?`
}
func (mysqlDialect) PKColumnsQuery() string {
	return `SELECT COLUMN_NAME
	          FROM information_schema.KEY_COLUMN_USAGE
	         WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND CONSTRAINT_NAME='PRIMARY'
	         ORDER BY ORDINAL_POSITION`
}
func (mysqlDialect) IsNumericDataType(dt string) bool {
	switch strings.ToLower(dt) {
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint", "decimal", "numeric":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Oracle dialect
// ---------------------------------------------------------------------------

type oracleDialect struct{}

func OracleSource() SourceDialect { return oracleDialect{} }

func (oracleDialect) Kind() string { return "oracle" }
func (oracleDialect) Quote(id string) string {
	return `"` + strings.ReplaceAll(id, `"`, `""`) + `"`
}
func (oracleDialect) PlaceholderAt(n int) string { return fmt.Sprintf(":%d", n) }
func (d oracleDialect) Qualify(schema, name string) string {
	if schema == "" {
		return d.Quote(name)
	}
	return d.Quote(schema) + "." + d.Quote(name)
}

func (d oracleDialect) CountQuery(schema, table string) string {
	return "SELECT COUNT(*) FROM " + d.Qualify(schema, table)
}
func (d oracleDialect) MinMaxQuery(schema, table, pkCol string) string {
	return fmt.Sprintf("SELECT MIN(%s), MAX(%s) FROM %s",
		d.Quote(pkCol), d.Quote(pkCol), d.Qualify(schema, table))
}
func (d oracleDialect) SelectRangeQuery(schema, table string, cols []string, rangeCol string) string {
	return fmt.Sprintf("SELECT %s FROM %s WHERE %s BETWEEN %s AND %s ORDER BY %s",
		joinQuoted(d, cols), d.Qualify(schema, table),
		d.Quote(rangeCol), d.PlaceholderAt(1), d.PlaceholderAt(2), d.Quote(rangeCol))
}
func (d oracleDialect) SelectOffsetQuery(schema, table string, cols []string) string {
	// Oracle 12c+ ISO-syntax. Arg order: 1=offset, 2=limit — so the caller
	// must pass (low["offset"], high["limit"]) in that order.
	return fmt.Sprintf("SELECT %s FROM %s OFFSET %s ROWS FETCH NEXT %s ROWS ONLY",
		joinQuoted(d, cols), d.Qualify(schema, table),
		d.PlaceholderAt(1), d.PlaceholderAt(2))
}

func (oracleDialect) ListColumnsQuery() string {
	// Use ALL_TAB_COLS (extended dictionary view) which exposes VIRTUAL_COLUMN
	// and HIDDEN_COLUMN flags; ALL_TAB_COLUMNS omits them. We skip both so
	// the copier never tries to SELECT a virtual/hidden column (and the
	// target PG schema has the virtual columns as GENERATED STORED — PG
	// recomputes them on its own).
	return `
		SELECT column_name FROM all_tab_cols
		 WHERE owner = :1 AND table_name = :2
		   AND virtual_column = 'NO'
		   AND hidden_column  = 'NO'
		   AND data_type NOT IN ('SDO_GEOMETRY','BFILE')
		 ORDER BY column_id`
}
func (oracleDialect) ColumnDataTypeQuery() string {
	return `SELECT data_type FROM all_tab_columns
	         WHERE owner = :1 AND table_name = :2 AND column_name = :3`
}
func (oracleDialect) PKColumnsQuery() string {
	return `SELECT cc.column_name
	          FROM all_constraints c
	          JOIN all_cons_columns cc
	            ON c.owner = cc.owner AND c.constraint_name = cc.constraint_name
	         WHERE c.owner = :1 AND c.table_name = :2 AND c.constraint_type = 'P'
	         ORDER BY cc.position`
}
func (oracleDialect) IsNumericDataType(dt string) bool {
	switch strings.ToUpper(dt) {
	case "NUMBER", "INTEGER", "SMALLINT", "FLOAT", "BINARY_FLOAT", "BINARY_DOUBLE":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// IBM DB2 dialect (LUW + z/OS — SourceDialect.Kind() routes catalog queries)
// ---------------------------------------------------------------------------

type db2Dialect struct{ zos bool }

func DB2Source() SourceDialect    { return db2Dialect{zos: false} }
func DB2zOSSource() SourceDialect { return db2Dialect{zos: true} }

func (d db2Dialect) Kind() string {
	if d.zos {
		return "db2zos"
	}
	return "db2"
}

func (db2Dialect) Quote(id string) string {
	return `"` + strings.ReplaceAll(id, `"`, `""`) + `"`
}

// DB2 placeholders are positional `?` (same shape as MySQL).
func (db2Dialect) PlaceholderAt(n int) string { return "?" }

func (d db2Dialect) Qualify(schema, name string) string {
	if schema == "" {
		return d.Quote(name)
	}
	return d.Quote(schema) + "." + d.Quote(name)
}

func (d db2Dialect) CountQuery(schema, table string) string {
	return "SELECT COUNT(*) FROM " + d.Qualify(schema, table)
}

func (d db2Dialect) MinMaxQuery(schema, table, pkCol string) string {
	return fmt.Sprintf("SELECT MIN(%s), MAX(%s) FROM %s",
		d.Quote(pkCol), d.Quote(pkCol), d.Qualify(schema, table))
}

func (d db2Dialect) SelectRangeQuery(schema, table string, cols []string, rangeCol string) string {
	return fmt.Sprintf("SELECT %s FROM %s WHERE %s BETWEEN %s AND %s ORDER BY %s",
		joinQuoted(d, cols), d.Qualify(schema, table),
		d.Quote(rangeCol), d.PlaceholderAt(1), d.PlaceholderAt(2), d.Quote(rangeCol))
}

// DB2 11+ (LUW) and z/OS 12+ accept the ISO-standard form. DB2 requires
// an explicit ORDER BY when combined with FETCH NEXT in many contexts —
// we order by the first projected column for a deterministic batch shape.
// Arg order: 1=offset, 2=limit (cohérent avec oracle).
func (d db2Dialect) SelectOffsetQuery(schema, table string, cols []string) string {
	return fmt.Sprintf("SELECT %s FROM %s ORDER BY 1 OFFSET %s ROWS FETCH NEXT %s ROWS ONLY",
		joinQuoted(d, cols), d.Qualify(schema, table),
		d.PlaceholderAt(1), d.PlaceholderAt(2))
}

// ---- Catalog queries — branches LUW (SYSCAT.*) vs z/OS (SYSIBM.*) ----

func (d db2Dialect) ListColumnsQuery() string {
	if d.zos {
		// z/OS columns: SYSIBM.SYSCOLUMNS keyed on (TBCREATOR, TBNAME).
		// HIDDEN='P' marks partition-by-default hidden columns; ROWID is
		// not portable to PG.
		return `SELECT NAME FROM SYSIBM.SYSCOLUMNS
		         WHERE TBCREATOR=? AND TBNAME=?
		           AND HIDDEN <> 'P'
		           AND COLTYPE NOT IN ('ROWID')
		         ORDER BY COLNO`
	}
	// LUW: SYSCAT.COLUMNS — drop hidden / GENERATED ALWAYS / ROWID + REF.
	// PG recomputes virtual columns, so we never SELECT the source-side
	// generated value.
	return `SELECT colname FROM SYSCAT.COLUMNS
	         WHERE tabschema=? AND tabname=?
	           AND hidden = 'N'
	           AND generated <> 'A'
	           AND typename NOT IN ('ROWID', 'REFERENCE')
	         ORDER BY colno`
}

func (d db2Dialect) ColumnDataTypeQuery() string {
	if d.zos {
		return `SELECT COLTYPE FROM SYSIBM.SYSCOLUMNS
		         WHERE TBCREATOR=? AND TBNAME=? AND NAME=?`
	}
	return `SELECT typename FROM SYSCAT.COLUMNS
	         WHERE tabschema=? AND tabname=? AND colname=?`
}

func (d db2Dialect) PKColumnsQuery() string {
	if d.zos {
		return `SELECT k.COLNAME
		          FROM SYSIBM.SYSKEYS k
		          JOIN SYSIBM.SYSINDEXES i
		            ON k.IXNAME = i.NAME AND k.IXCREATOR = i.CREATOR
		          JOIN SYSIBM.SYSTABCONST c
		            ON c.IXNAME = i.NAME AND c.IXCREATOR = i.CREATOR
		         WHERE c.TBCREATOR=? AND c.TBNAME=? AND c.TYPE='P'
		         ORDER BY k.COLSEQ`
	}
	return `SELECT k.colname
	          FROM SYSCAT.TABCONST c
	          JOIN SYSCAT.KEYCOLUSE k
	            ON c.constname=k.constname AND c.tabschema=k.tabschema
	         WHERE c.tabschema=? AND c.tabname=? AND c.type='P'
	         ORDER BY k.colseq`
}

func (db2Dialect) IsNumericDataType(dt string) bool {
	switch strings.ToUpper(dt) {
	case "SMALLINT", "INTEGER", "INT", "BIGINT",
		"DECIMAL", "DEC", "NUMERIC", "DECFLOAT",
		"REAL", "DOUBLE", "FLOAT":
		return true
	}
	return false
}

// joinQuoted quotes each column and joins with commas, for SELECT list.
func joinQuoted(d SourceDialect, cols []string) string {
	parts := make([]string, 0, len(cols))
	for _, c := range cols {
		parts = append(parts, d.Quote(c))
	}
	return strings.Join(parts, ",")
}

// ---------------------------------------------------------------------------
// Convenience: catalog-lookup helpers that know how to pass owner/table to
// the underlying *sql.DB whatever the placeholder flavour.
// ---------------------------------------------------------------------------

// listSourceColumns runs the dialect's ListColumnsQuery against db.
func listSourceColumns(ctx context.Context, db *sql.DB, d SourceDialect, schema, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, d.ListColumnsQuery(), schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func sourceColumnDataType(ctx context.Context, db *sql.DB, d SourceDialect, schema, table, col string) (string, error) {
	var dt string
	err := db.QueryRowContext(ctx, d.ColumnDataTypeQuery(), schema, table, col).Scan(&dt)
	return dt, err
}

func sourcePKColumns(ctx context.Context, db *sql.DB, d SourceDialect, schema, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, d.PKColumnsQuery(), schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}
