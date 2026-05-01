package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/mysql"
)

// MySQL HASH(id) PARTITIONS N must lift to a PG HASH-partitioned parent
// with N children carrying MODULUS/REMAINDER pairs.
func TestMySQLHashPartitioningEmitsPGDeclarative(t *testing.T) {
	src := `CREATE TABLE orders (id INT NOT NULL, payload VARCHAR(40))
		PARTITION BY HASH(id) PARTITIONS 4;`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})

	require.Len(t, res.Plan.Tables, 1)
	tbl := res.Plan.Tables[0]
	require.NotNil(t, tbl.Partitioning, "MySQL HASH partitioning must lift to Plan.Tables[0].Partitioning")
	require.Equal(t, "HASH", tbl.Partitioning.Method)
	require.Equal(t, []string{"id"}, tbl.Partitioning.Columns)
	require.Len(t, tbl.Partitioning.Partitions, 4)
	for i, p := range tbl.Partitioning.Partitions {
		require.Equal(t, 4, p.Modulus)
		require.Equal(t, i, p.Remainder)
	}

	require.Contains(t, res.DDLScript, `PARTITION BY HASH ("id")`)
	require.Contains(t, res.DDLScript,
		`CREATE TABLE "mig"."p_h0" PARTITION OF "mig"."orders" FOR VALUES WITH (MODULUS 4, REMAINDER 0);`)
	require.Contains(t, res.DDLScript,
		`CREATE TABLE "mig"."p_h3" PARTITION OF "mig"."orders" FOR VALUES WITH (MODULUS 4, REMAINDER 3);`)
}

// MySQL RANGE COLUMNS multi-column lifts to PG RANGE on a column tuple.
// Bounds chain across partitions, with MAXVALUE preserved per-component.
func TestMySQLRangeColumnsEmitsPGDeclarative(t *testing.T) {
	src := `CREATE TABLE measurements (a INT NOT NULL, b INT NOT NULL)
		PARTITION BY RANGE COLUMNS(a, b) (
		  PARTITION p1 VALUES LESS THAN (10, 100),
		  PARTITION p2 VALUES LESS THAN (20, 200),
		  PARTITION p3 VALUES LESS THAN (MAXVALUE, MAXVALUE)
		);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})

	require.Len(t, res.Plan.Tables, 1)
	tbl := res.Plan.Tables[0]
	require.NotNil(t, tbl.Partitioning)
	require.Equal(t, "RANGE", tbl.Partitioning.Method, "COLUMNS suffix dropped — PG RANGE handles tuples natively")
	require.Equal(t, []string{"a", "b"}, tbl.Partitioning.Columns)
	require.Len(t, tbl.Partitioning.Partitions, 3)
	require.Equal(t, "10,100", tbl.Partitioning.Partitions[0].UpperBound)
	require.Equal(t, "MAXVALUE,MAXVALUE", tbl.Partitioning.Partitions[2].UpperBound)

	require.Contains(t, res.DDLScript, `PARTITION BY RANGE ("a","b")`)
	require.Contains(t, res.DDLScript,
		`CREATE TABLE "mig"."p1" PARTITION OF "mig"."measurements" FOR VALUES FROM (MINVALUE) TO (10,100);`)
	require.Contains(t, res.DDLScript,
		`CREATE TABLE "mig"."p3" PARTITION OF "mig"."measurements" FOR VALUES FROM (20,200) TO (MAXVALUE,MAXVALUE);`)
}

// MySQL LIST single-column lifts to PG LIST.
func TestMySQLListPartitioningEmitsPGDeclarative(t *testing.T) {
	src := `CREATE TABLE customers (region VARCHAR(2) NOT NULL)
		PARTITION BY LIST COLUMNS(region) (
		  PARTITION p_eu VALUES IN ('FR','DE','IT'),
		  PARTITION p_as VALUES IN ('JP','CN')
		);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})

	tbl := res.Plan.Tables[0]
	require.NotNil(t, tbl.Partitioning)
	require.Equal(t, "LIST", tbl.Partitioning.Method)
	require.Equal(t, []string{"region"}, tbl.Partitioning.Columns)
	require.Len(t, tbl.Partitioning.Partitions, 2)
	require.Equal(t, []string{"'FR'", "'DE'", "'IT'"}, tbl.Partitioning.Partitions[0].Values)

	require.Contains(t, res.DDLScript, `PARTITION BY LIST ("region")`)
	require.Contains(t, res.DDLScript,
		`CREATE TABLE "mig"."p_eu" PARTITION OF "mig"."customers" FOR VALUES IN ('FR','DE','IT');`)
}

// MySQL KEY → PG HASH degrades with an explanation that surfaces the
// distribution caveat in the wizard.
func TestMySQLKeyPartitioningDegradesToHashWithNote(t *testing.T) {
	src := `CREATE TABLE t (id INT NOT NULL, k INT NOT NULL)
		PARTITION BY KEY (id, k) PARTITIONS 4;`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})

	tbl := res.Plan.Tables[0]
	require.NotNil(t, tbl.Partitioning)
	require.Equal(t, "HASH", tbl.Partitioning.Method)
	require.Equal(t, []string{"id", "k"}, tbl.Partitioning.Columns)

	// The KEY → HASH note must appear in the explanations stream so the
	// wizard surfaces the distribution caveat to the user.
	found := false
	for _, e := range res.Explanations {
		if strings.Contains(e.Reason, "KEY mapped to PG HASH") {
			found = true
		}
	}
	require.True(t, found, "expected KEY→HASH explanation note; got %#v", res.Explanations)
}

// Subpartitioning is not natively expressible in PG declarative
// partitioning; squishy must drop it with a warn-level explanation while
// still lifting the top-level RANGE.
func TestMySQLSubpartitionDroppedWithNote(t *testing.T) {
	src := `CREATE TABLE t (id INT NOT NULL, ts INT NOT NULL)
		PARTITION BY RANGE (ts)
		SUBPARTITION BY HASH(id) SUBPARTITIONS 2 (
		  PARTITION p1 VALUES LESS THAN (2020) (SUBPARTITION s1, SUBPARTITION s2),
		  PARTITION p2 VALUES LESS THAN (2021) (SUBPARTITION s3, SUBPARTITION s4)
		);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})

	tbl := res.Plan.Tables[0]
	require.NotNil(t, tbl.Partitioning)
	require.Equal(t, "RANGE", tbl.Partitioning.Method)
	require.Len(t, tbl.Partitioning.Partitions, 2)

	found := false
	for _, e := range res.Explanations {
		if strings.Contains(e.Reason, "SUBPARTITION BY clause dropped") {
			found = true
		}
	}
	require.True(t, found, "expected SUBPARTITION-dropped explanation note")
}

// HASH/RANGE/LIST with an expression key (e.g. YEAR(d)) is not yet
// supported by the writer; the table must downgrade to unpartitioned with
// a warning so the migration still runs.
func TestMySQLPartitioningExpressionKeyDegradesToWarning(t *testing.T) {
	src := `CREATE TABLE sales (id INT, ts DATETIME)
		PARTITION BY RANGE (YEAR(ts)) (
		  PARTITION p2018 VALUES LESS THAN (2019),
		  PARTITION p2019 VALUES LESS THAN (2020)
		);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})

	tbl := res.Plan.Tables[0]
	require.Nil(t, tbl.Partitioning, "expression keys not yet supported — should downgrade")
	require.NotEmpty(t, res.Warnings)
	found := false
	for _, w := range res.Warnings {
		if w.Kind == "partitioning" && strings.Contains(w.Message, "could not translate") {
			found = true
		}
	}
	require.True(t, found, "expected partitioning warning; got %#v", res.Warnings)
}
