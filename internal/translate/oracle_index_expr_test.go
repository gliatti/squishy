package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// Function-based index — `CREATE INDEX ix ON t (LOWER(col))` — must lift
// to a PG expression index, with the function call passed through verbatim.
func TestOracleFunctionBasedIndex(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."CUSTOMERS" (
		  "ID"   NUMBER NOT NULL,
		  "NAME" VARCHAR2(40) NOT NULL
		);
		CREATE INDEX "MIG"."IX_CUST_LOWER_NAME"
		  ON "MIG"."CUSTOMERS" (LOWER("NAME"));`

	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Indexes, 1)
	idx := res.Plan.Indexes[0]
	require.True(t, idx.ColumnIsExpr[0],
		"function-based key must surface as an expression index, not a bare column name")
	require.Equal(t, `LOWER("NAME")`, idx.Columns[0],
		"raw expression text must be preserved (Oracle quoted ident kept)")

	require.Contains(t, res.DDLPostCopy, `(LOWER("NAME"))`,
		"emitted PG DDL must wrap the expression in its own parens")
}

// Multi-key arithmetic expression — `CREATE INDEX ix ON t (col1 + col2)` —
// must capture the entire expression even though it starts with an IDENT
// (not a paren). The translator emits it as a single expression key.
func TestOracleArithmeticExpressionIndex(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."ORDERS" (
		  "ID"  NUMBER NOT NULL,
		  "QTY" NUMBER NOT NULL,
		  "PRICE" NUMBER NOT NULL
		);
		CREATE INDEX "MIG"."IX_TOTAL"
		  ON "MIG"."ORDERS" ("QTY" * "PRICE");`

	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Indexes, 1)
	idx := res.Plan.Indexes[0]
	require.True(t, idx.ColumnIsExpr[0])
	require.Equal(t, `"QTY" * "PRICE"`, idx.Columns[0])
	require.Contains(t, res.DDLPostCopy, `("QTY" * "PRICE")`)
}

// Mixed bare-column + expression keys must coexist in a single index, each
// with its own ColumnIsExpr flag — and per-column ASC/DESC must still apply.
func TestOracleMixedBareAndExpressionIndex(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."EVENTS" (
		  "TS"   TIMESTAMP NOT NULL,
		  "NAME" VARCHAR2(60) NOT NULL,
		  "SEQ"  NUMBER NOT NULL
		);
		CREATE INDEX "MIG"."IX_EVT"
		  ON "MIG"."EVENTS" ("TS" DESC, UPPER("NAME") ASC, "SEQ");`

	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Indexes, 1)
	idx := res.Plan.Indexes[0]
	require.Equal(t, []bool{false, true, false}, idx.ColumnIsExpr)
	require.Equal(t, []string{"DESC", "ASC", ""}, idx.ColumnDirs)
	// Bare cols are case-folded by the Oracle pipeline, expressions keep
	// their source-quoted identifiers.
	require.Equal(t, []string{"ts", `UPPER("NAME")`, "seq"}, idx.Columns)

	require.Contains(t, res.DDLPostCopy, `"ts" DESC`)
	require.Contains(t, res.DDLPostCopy, `(UPPER("NAME")) ASC`)
	require.Contains(t, res.DDLPostCopy, `"seq"`)
}

// Concatenation expression — Oracle's `||` operator — must capture the
// entire infix expression and pass through to PG which uses the same
// operator.
func TestOracleConcatExpressionIndex(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."PEOPLE" (
		  "FIRST_NAME" VARCHAR2(40) NOT NULL,
		  "LAST_NAME"  VARCHAR2(40) NOT NULL
		);
		CREATE INDEX "MIG"."IX_FULLNAME"
		  ON "MIG"."PEOPLE" ("FIRST_NAME" || ' ' || "LAST_NAME");`

	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Indexes, 1)
	idx := res.Plan.Indexes[0]
	require.True(t, idx.ColumnIsExpr[0])
	require.True(t, strings.Contains(idx.Columns[0], "||"),
		"concatenation operator must be preserved verbatim, got %q", idx.Columns[0])
}

// Bare-column indexes must continue to work unchanged — regression
// guard for the new expression-aware code path.
func TestOracleBareColumnIndexStillWorks(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."T" (
		  "A" NUMBER NOT NULL,
		  "B" NUMBER NOT NULL
		);
		CREATE INDEX "MIG"."IX_A_B" ON "MIG"."T" ("A", "B" DESC);`

	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Indexes, 1)
	idx := res.Plan.Indexes[0]
	require.Equal(t, []string{"a", "b"}, idx.Columns)
	// Bare-column-only indexes leave ColumnIsExpr nil (omitted from JSON
	// and the DDL writer takes the bare-column path automatically).
	require.Nil(t, idx.ColumnIsExpr)
	require.Equal(t, []string{"", "DESC"}, idx.ColumnDirs)
}
