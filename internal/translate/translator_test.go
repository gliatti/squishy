package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/mysql"
	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

func TestTypeMappings(t *testing.T) {
	src := `
		CREATE TABLE t (
		  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
		  flag TINYINT(1) NOT NULL DEFAULT 0,
		  big BIGINT UNSIGNED NOT NULL,
		  status ENUM('a','b','c') NOT NULL DEFAULT 'a',
		  meta JSON,
		  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		  stamped TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		  PRIMARY KEY (id)
		);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig"})
	require.Len(t, res.Plan.Tables, 1)

	byName := map[string]PGColumn{}
	for _, c := range res.Plan.Tables[0].Columns {
		byName[c.Name] = c
	}
	// BIGINT UNSIGNED AUTO_INCREMENT is mapped to NUMERIC(20,0) backed by an
	// explicit sequence rather than a PG IDENTITY column, because PG IDENTITY
	// requires a fixed-width integer type and NUMERIC(20,0) is the only PG
	// type that preserves the full 0..2^64-1 range of BIGINT UNSIGNED.
	require.Equal(t, "NUMERIC(20,0)", byName["id"].Type)
	require.False(t, byName["id"].Identity, "explicit sequence (not IDENTITY) for BIGINT UNSIGNED")
	require.Equal(t, "BOOLEAN", byName["flag"].Type, "TINYINT(1) should map to BOOLEAN")
	require.Equal(t, "NUMERIC(20,0)", byName["big"].Type, "BIGINT UNSIGNED should map to NUMERIC(20,0)")
	require.Equal(t, "TEXT", byName["status"].Type)
	require.Contains(t, byName["status"].Check, "IN (")
	require.Equal(t, "JSONB", byName["meta"].Type)
	require.Equal(t, "TIMESTAMP", byName["created_at"].Type)
	require.Equal(t, "TIMESTAMPTZ", byName["stamped"].Type)
	require.NotEmpty(t, res.DDLScript)
	require.True(t, strings.Contains(res.DDLScript, "CREATE SEQUENCE"))
	require.True(t, strings.Contains(res.DDLScript, "nextval("))
}

// Two tables sharing an index name (MySQL legal, per-table scope) must
// produce distinct PG index names after dedup — the employees sample trips
// this with `dept_no` on both dept_emp and dept_manager.
func TestIndexNameDedup_PerTableCollision(t *testing.T) {
	src := `
		CREATE TABLE dept_emp (
		  emp_no INT NOT NULL,
		  dept_no CHAR(4) NOT NULL,
		  PRIMARY KEY (emp_no, dept_no),
		  KEY dept_no (dept_no)
		);
		CREATE TABLE dept_manager (
		  emp_no INT NOT NULL,
		  dept_no CHAR(4) NOT NULL,
		  PRIMARY KEY (emp_no, dept_no),
		  KEY dept_no (dept_no)
		);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig"})

	names := map[string]int{}
	for _, idx := range res.Plan.Indexes {
		names[idx.Name]++
	}
	for name, count := range names {
		require.Equalf(t, 1, count, "index name %q duplicated in plan", name)
	}
	require.Contains(t, names, "dept_no", "first occurrence keeps original name")
	require.Contains(t, names, "dept_manager_dept_no", "duplicate gets table-prefixed")
}

// Oracle stores unquoted identifiers in uppercase. Tables, columns and routine
// params are lowercased downstream so the PG schema reads `mig.profits`,
// `mig.sales`, etc. The view name must follow the same rule — otherwise the
// view ends up as mig."PROFITS" while its referenced tables are mig.sales,
// every caller has to quote, and `validate` can't find it.
func TestOracleViewNameNormalized(t *testing.T) {
	src := `
		CREATE TABLE "SH"."SALES" (
		  "PROD_ID" NUMBER(6,0) NOT NULL,
		  "AMOUNT_SOLD" NUMBER(10,2) NOT NULL
		);
		CREATE OR REPLACE FORCE VIEW "SH"."PROFITS" ("PROD_ID", "AMOUNT_SOLD") AS
		  SELECT s.prod_id, s.amount_sold FROM sales s;`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Views, 1, "PROFITS view must land in Plan.Views")
	v := res.Plan.Views[0]
	require.Equal(t, "profits", v.Name, "Oracle uppercase ident must be lowercased like tables")
	require.NotEmpty(t, v.DDL, "view DDL must be generated so create_routine has something to execute")
	require.Contains(t, v.DDL, "\"profits\"", "emitted DDL targets the lowercased name")
	require.NotContains(t, v.DDL, "\"PROFITS\"", "uppercased form would create mig.\"PROFITS\" instead of mig.profits")
}

// Oracle RANGE-partitioned tables (Sales History's SALES, COSTS) must surface
// as PG declarative-partitioned tables: a parent with PARTITION BY RANGE plus
// one CREATE TABLE … PARTITION OF … per child, with bounds chained
// (FROM = previous upper, TO = own upper, MINVALUE for the first).
func TestOracleRangePartitioningEmitsPGDeclarative(t *testing.T) {
	src := `
		CREATE TABLE "SH"."SALES" (
		  "PROD_ID" NUMBER(6,0) NOT NULL,
		  "TIME_ID" DATE NOT NULL,
		  "AMOUNT_SOLD" NUMBER(10,2) NOT NULL
		)
		PARTITION BY RANGE ("TIME_ID")
		(PARTITION "SALES_2018"   VALUES LESS THAN (TO_DATE(' 2019-01-01 00:00:00', 'SYYYY-MM-DD HH24:MI:SS', 'NLS_CALENDAR=GREGORIAN')),
		 PARTITION "SALES_H1_2019" VALUES LESS THAN (TO_DATE(' 2019-07-01 00:00:00', 'SYYYY-MM-DD HH24:MI:SS', 'NLS_CALENDAR=GREGORIAN')),
		 PARTITION "SALES_OPEN"    VALUES LESS THAN (MAXVALUE));`

	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Tables, 1)
	tbl := res.Plan.Tables[0]
	require.NotNil(t, tbl.Partitioning, "Plan.Tables[0].Partitioning must be populated")
	require.Equal(t, "RANGE", tbl.Partitioning.Method)
	require.Equal(t, []string{"time_id"}, tbl.Partitioning.Columns,
		"partition column must be lowercased like the rest of the Oracle pipeline")
	require.Len(t, tbl.Partitioning.Partitions, 3)

	// Bounds: TO_DATE() literals stripped to their first string arg, MAXVALUE preserved.
	require.Equal(t, "sales_2018", tbl.Partitioning.Partitions[0].Name)
	require.Equal(t, "'2019-01-01 00:00:00'", tbl.Partitioning.Partitions[0].UpperBound)
	require.Equal(t, "'2019-07-01 00:00:00'", tbl.Partitioning.Partitions[1].UpperBound)
	require.Equal(t, "MAXVALUE", tbl.Partitioning.Partitions[2].UpperBound)

	// DDL emission: parent has PARTITION BY RANGE (col), children chain
	// bounds with MINVALUE on the first.
	require.Contains(t, res.DDLScript, `CREATE TABLE "mig"."sales"`)
	require.Contains(t, res.DDLScript, `PARTITION BY RANGE ("time_id")`)
	require.Contains(t, res.DDLScript,
		`CREATE TABLE "mig"."sales_2018" PARTITION OF "mig"."sales" FOR VALUES FROM (MINVALUE) TO ('2019-01-01 00:00:00');`)
	require.Contains(t, res.DDLScript,
		`CREATE TABLE "mig"."sales_h1_2019" PARTITION OF "mig"."sales" FOR VALUES FROM ('2019-01-01 00:00:00') TO ('2019-07-01 00:00:00');`)
	require.Contains(t, res.DDLScript,
		`CREATE TABLE "mig"."sales_open" PARTITION OF "mig"."sales" FOR VALUES FROM ('2019-07-01 00:00:00') TO (MAXVALUE);`)
}

// LIST partitioning: Oracle `PARTITION x VALUES (v1, v2, ...)` must map to
// PG `FOR VALUES IN (v1, v2, ...)`. The catch-all `VALUES (DEFAULT)` lifts
// to PG's `FOR VALUES IN (DEFAULT)`.
func TestOracleListPartitioning(t *testing.T) {
	src := `
		CREATE TABLE "SH"."CUSTOMERS_BY_REGION" (
		  "CUST_ID" NUMBER NOT NULL,
		  "REGION" VARCHAR2(2) NOT NULL
		)
		PARTITION BY LIST ("REGION")
		(PARTITION "P_EU"  VALUES ('FR','DE','IT'),
		 PARTITION "P_AS"  VALUES ('JP','CN'),
		 PARTITION "P_OTH" VALUES (DEFAULT));`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Tables, 1)
	tbl := res.Plan.Tables[0]
	require.NotNil(t, tbl.Partitioning)
	require.Equal(t, "LIST", tbl.Partitioning.Method)
	require.Equal(t, []string{"region"}, tbl.Partitioning.Columns)
	require.Len(t, tbl.Partitioning.Partitions, 3)
	require.Equal(t, []string{"'FR'", "'DE'", "'IT'"}, tbl.Partitioning.Partitions[0].Values)
	require.True(t, tbl.Partitioning.Partitions[2].IsDefault, "DEFAULT partition flagged")

	require.Contains(t, res.DDLScript, `PARTITION BY LIST ("region")`)
	require.Contains(t, res.DDLScript,
		`CREATE TABLE "mig"."p_eu" PARTITION OF "mig"."customers_by_region" FOR VALUES IN ('FR','DE','IT');`)
	require.Contains(t, res.DDLScript,
		`CREATE TABLE "mig"."p_oth" PARTITION OF "mig"."customers_by_region" FOR VALUES IN (DEFAULT);`)
}

// HASH "PARTITIONS N" sugar form: Oracle assigns N child partitions
// implicitly. PG requires explicit MODULUS/REMAINDER, so we synthesise
// remainders 0..N-1 in source order.
func TestOracleHashPartitioningSugarForm(t *testing.T) {
	src := `
		CREATE TABLE "SH"."CUST_HASHED" (
		  "CUST_ID" NUMBER NOT NULL,
		  "PAYLOAD" VARCHAR2(40)
		)
		PARTITION BY HASH ("CUST_ID") PARTITIONS 4;`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	tbl := res.Plan.Tables[0]
	require.NotNil(t, tbl.Partitioning)
	require.Equal(t, "HASH", tbl.Partitioning.Method)
	require.Len(t, tbl.Partitioning.Partitions, 4)
	for i, p := range tbl.Partitioning.Partitions {
		require.Equal(t, 4, p.Modulus)
		require.Equal(t, i, p.Remainder)
	}

	require.Contains(t, res.DDLScript, `PARTITION BY HASH ("cust_id")`)
	require.Contains(t, res.DDLScript,
		`CREATE TABLE "mig"."p_h0" PARTITION OF "mig"."cust_hashed" FOR VALUES WITH (MODULUS 4, REMAINDER 0);`)
	require.Contains(t, res.DDLScript,
		`CREATE TABLE "mig"."p_h3" PARTITION OF "mig"."cust_hashed" FOR VALUES WITH (MODULUS 4, REMAINDER 3);`)
}

// HASH explicit-list form: Oracle's DBMS_METADATA expands `PARTITIONS N`
// into a literal `(PARTITION p1, PARTITION p2, …)` list. We must take that
// shape too and synthesise modulus/remainder in source order.
func TestOracleHashPartitioningExplicitList(t *testing.T) {
	src := `
		CREATE TABLE "SH"."CUST_HASHED" (
		  "CUST_ID" NUMBER NOT NULL
		)
		PARTITION BY HASH ("CUST_ID")
		(PARTITION "SYS_P1", PARTITION "SYS_P2", PARTITION "SYS_P3");`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	tbl := res.Plan.Tables[0]
	require.NotNil(t, tbl.Partitioning)
	require.Equal(t, "HASH", tbl.Partitioning.Method)
	require.Len(t, tbl.Partitioning.Partitions, 3)
	require.Equal(t, "sys_p1", tbl.Partitioning.Partitions[0].Name)
	require.Equal(t, 3, tbl.Partitioning.Partitions[2].Modulus)
	require.Equal(t, 2, tbl.Partitioning.Partitions[2].Remainder)
	require.Contains(t, res.DDLScript,
		`CREATE TABLE "mig"."sys_p1" PARTITION OF "mig"."cust_hashed" FOR VALUES WITH (MODULUS 3, REMAINDER 0);`)
}

// RANGE bounds beyond TO_DATE: numeric literals, TO_TIMESTAMP, INTERVAL,
// raw single-quoted strings — all must round-trip without losing fidelity.
func TestOracleRangeBoundsExtended(t *testing.T) {
	cases := []struct {
		name    string
		oracle  string
		pg      string
	}{
		{"numeric", "1000", "1000"},
		{"negative numeric", "-42", "-42"},
		{"decimal", "12.34", "12.34"},
		{"string literal", "'M'", "'M'"},
		{"to_date", "TO_DATE(' 2020-01-01 00:00:00', 'SYYYY-MM-DD HH24:MI:SS', 'NLS_CALENDAR=GREGORIAN')", "'2020-01-01 00:00:00'"},
		{"to_timestamp", "TO_TIMESTAMP(' 2020-01-01 00:00:00.123', 'SYYYY-MM-DD HH24:MI:SS.FF')", "'2020-01-01 00:00:00.123'"},
		{"to_timestamp_tz", "TO_TIMESTAMP_TZ(' 2020-01-01 00:00:00 +00:00', '...')", "'2020-01-01 00:00:00 +00:00'"},
		{"interval year", "INTERVAL '1' YEAR", "INTERVAL '1' YEAR"},
		{"interval year to month", "INTERVAL '1-3' YEAR TO MONTH", "INTERVAL '1-3' YEAR TO MONTH"},
		{"maxvalue", "MAXVALUE", "MAXVALUE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := translateOracleScalarBound(tc.oracle)
			require.NoError(t, err)
			require.Equal(t, tc.pg, got)
		})
	}
}
