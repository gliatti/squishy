package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/mysql"
)

// Per-column DESC must round-trip into the emitted DDL — PG supports it
// natively and dropping it changes plan choice for ORDER BY scans.
func TestIndexDescPerColumnPreserved(t *testing.T) {
	src := `CREATE TABLE t (
		id INT PRIMARY KEY,
		ts INT NOT NULL,
		seq INT NOT NULL,
		KEY ix_ts_seq (ts DESC, seq ASC)
	);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})
	require.Len(t, res.Plan.Indexes, 1)
	require.Equal(t, []string{"DESC", "ASC"}, res.Plan.Indexes[0].ColumnDirs)
	require.Contains(t, res.DDLPostCopy, `("ts" DESC,"seq" ASC)`)
}

// Functional/expression indexes lift to PG expression indexes —
// `(expr)` inside the col list, no qIdent quoting.
func TestIndexFunctionalEmitsPGExpressionIndex(t *testing.T) {
	src := `CREATE TABLE t (
		id INT PRIMARY KEY,
		title VARCHAR(200),
		KEY ix_lower_title ((LOWER(title)))
	);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})
	require.Len(t, res.Plan.Indexes, 1)
	require.True(t, res.Plan.Indexes[0].ColumnIsExpr[0])
	require.Equal(t, "LOWER(title)", res.Plan.Indexes[0].Columns[0])
	require.Contains(t, res.DDLPostCopy, `((LOWER(title)))`)
}

// Prefix-length indexes don't translate cleanly — emit the full-column
// index with a warning so the user can opt into a functional `LEFT(col, N)`
// index if storage matters.
func TestIndexPrefixLengthWarns(t *testing.T) {
	src := `CREATE TABLE t (
		id INT PRIMARY KEY,
		body TEXT NOT NULL,
		KEY ix_body (body(50))
	);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})
	require.Len(t, res.Plan.Indexes, 1)
	// The full column is indexed (no prefix length expressed in PG).
	require.Equal(t, []string{"body"}, res.Plan.Indexes[0].Columns)
	found := false
	for _, w := range res.Warnings {
		if w.Kind == "index.prefix" && strings.Contains(w.Message, "ix_body") {
			found = true
		}
	}
	require.True(t, found, "expected index.prefix warning")
}

// INVISIBLE is a planner-visibility hint with no PG equivalent. The index
// is still emitted; the user is informed.
func TestIndexInvisibleEmitsExplanation(t *testing.T) {
	src := `CREATE TABLE t (
		id INT PRIMARY KEY,
		flag INT,
		KEY ix_flag (flag) INVISIBLE
	);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})
	require.Len(t, res.Plan.Indexes, 1, "INVISIBLE indexes still get created (no PG equivalent)")
	found := false
	for _, e := range res.Explanations {
		if e.Source == "INVISIBLE" {
			found = true
		}
	}
	require.True(t, found, "expected INVISIBLE explanation")
}
