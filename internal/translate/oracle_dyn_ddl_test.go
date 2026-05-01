package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// TestDynamicDDL_AlterTableAddColumns confirms the generic dyn-DDL
// visitor rewrites a runtime-built ALTER TABLE … ADD ( … ) — Oracle's
// parenthesised multi-column form — into the PG-equivalent
// ALTER TABLE … ADD COLUMN … , ADD COLUMN ….
func TestDynamicDDL_AlterTableAddColumns(t *testing.T) {
	blk := &ast.Block{
		Stmts: []ast.PLStmt{
			&ast.AssignStmt{Target: "S", Expr: concatN(
				litStr("alter table T add ( c1 NUMBER(7), c2 VARCHAR2(7) )"),
			)},
			&ast.ExecuteImmediateStmt{SQL: ident("S")},
		},
	}
	v := MakeOracleDynamicDDLBuildVisitor("mig")
	rewritten := ast.Rewrite(blk, v)
	rb := rewritten.(*ast.Block)
	require.Equal(t, 2, len(rb.Stmts))

	a := rb.Stmts[0].(*ast.AssignStmt)
	parts := flattenConcat(a.Expr)
	first := parts[0].(*ast.Literal)
	upper := strings.ToUpper(first.Text)
	require.Contains(t, upper, "ALTER TABLE")
	require.Contains(t, upper, "ADD COLUMN")
	require.NotContains(t, upper, "VARCHAR2", "VARCHAR2 must be mapped to VARCHAR")
	require.NotContains(t, upper, "ADD (", "Oracle parenthesised ADD form must be replaced by per-column ADD COLUMN")
}

// TestDynamicDDL_AlterAddCrossVar covers the multi-stmt + cross-var
// shape that the DRSE PRC_MAJ_TRC procedure uses: the column list
// lives in a separate variable spliced via SUBSTR. The visitor's
// fallback path (parse fails because the sentinel for SUBSTR doesn't
// fit a column-def slot) must still rewrite the keyword Literals to
// PG-shape and wrap the SUBSTR in replace(…, ',', ', add column ').
func TestDynamicDDL_AlterAddCrossVar(t *testing.T) {
	blk := &ast.Block{
		Stmts: []ast.PLStmt{
			&ast.AssignStmt{Target: "S", Expr: concatN(
				litStr("alter table T add ( "),
			)},
			&ast.AssignStmt{Target: "S", Expr: concatN(
				ident("S"),
				&ast.FuncCall{Name: "SUBSTR", Args: []ast.Expr{ident("buf"), &ast.Literal{Kind: "number", Text: "2"}}},
			)},
			&ast.AssignStmt{Target: "S", Expr: concatN(
				ident("S"),
				litStr(" ) "),
			)},
			&ast.ExecuteImmediateStmt{SQL: ident("S")},
		},
	}
	v := MakeOracleDynamicDDLBuildVisitor("mig")
	rewritten := ast.Rewrite(blk, v)
	rb := rewritten.(*ast.Block)
	require.Equal(t, 4, len(rb.Stmts))

	// stmt[0]: initiator literal must be transformed `add ( ` → `add column `.
	a0 := rb.Stmts[0].(*ast.AssignStmt)
	parts0 := flattenConcat(a0.Expr)
	require.NotEmpty(t, parts0)
	first := parts0[0].(*ast.Literal)
	require.Contains(t, strings.ToLower(first.Text), "add column")
	require.NotContains(t, strings.ToLower(first.Text), "add ( ")

	// stmt[1]: SUBSTR must be wrapped in replace(SUBSTR(...), ',', ', add column ').
	a1 := rb.Stmts[1].(*ast.AssignStmt)
	parts1 := flattenConcat(a1.Expr)
	var foundReplace bool
	for _, p := range parts1 {
		if fc, ok := p.(*ast.FuncCall); ok && strings.EqualFold(fc.Name, "replace") {
			foundReplace = true
			require.Equal(t, 3, len(fc.Args), "replace() should take 3 args")
			break
		}
	}
	require.True(t, foundReplace, "SUBSTR call must be wrapped in replace(); got parts: %#v", parts1)

	// stmt[2]: trailing ` ) ` literal must be reduced to a single space.
	a2 := rb.Stmts[2].(*ast.AssignStmt)
	parts2 := flattenConcat(a2.Expr)
	last := parts2[len(parts2)-1].(*ast.Literal)
	require.NotContains(t, last.Text, ")", "closing paren should be dropped")
}

// TestDynamicDDL_NoCreateTriggerHandling confirms the visitor leaves
// CREATE TRIGGER alone (handled by the specialised trigger visitor).
func TestDynamicDDL_NoCreateTriggerHandling(t *testing.T) {
	blk := &ast.Block{
		Stmts: []ast.PLStmt{
			&ast.AssignStmt{Target: "S", Expr: concatN(
				litStr("CREATE OR REPLACE trigger T BEFORE INSERT ON X FOR EACH ROW BEGIN NEW.x := 1; END;"),
			)},
			&ast.ExecuteImmediateStmt{SQL: ident("S")},
		},
	}
	v := MakeOracleDynamicDDLBuildVisitor("mig")
	rewritten := ast.Rewrite(blk, v)
	rb := rewritten.(*ast.Block)
	// Untouched — still 2 stmts, original AssignStmt intact.
	require.Equal(t, 2, len(rb.Stmts))
	a := rb.Stmts[0].(*ast.AssignStmt)
	parts := flattenConcat(a.Expr)
	first := parts[0].(*ast.Literal)
	require.Contains(t, strings.ToUpper(first.Text), "CREATE OR REPLACE TRIGGER")
}
