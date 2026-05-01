package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// litStr builds a Literal that follows the parser convention: Text
// holds the unescaped body (no surrounding quotes). The PG writer
// re-adds the quotes at emission time.
func litStr(s string) *ast.Literal {
	return &ast.Literal{Kind: "string", Text: s}
}

func ident(name string) *ast.Ident { return &ast.Ident{Parts: []string{name}} }

func concatN(parts ...ast.Expr) ast.Expr {
	if len(parts) == 1 {
		return parts[0]
	}
	out := parts[0]
	for i := 1; i < len(parts); i++ {
		out = &ast.BinaryExpr{Op: "||", Lhs: out, Rhs: parts[i]}
	}
	return out
}

// findInnerBlock returns the synthetic wrapper Block emitted by the
// dyn-trigger visitor. Since the new design wraps the rewritten range
// in a nested DECLARE block (so the body variable can be declared
// locally), every successful rewrite produces exactly one ast.Block at
// the position of the original initiator. Tests look INTO that block to
// inspect the retargeted contributors and the synthesised EXECUTEs.
func findInnerBlock(t *testing.T, stmts []ast.PLStmt) *ast.Block {
	t.Helper()
	for _, s := range stmts {
		if blk, ok := s.(*ast.Block); ok {
			return blk
		}
	}
	t.Fatalf("expected an inner ast.Block in the rewritten stmts, got %d stmts", len(stmts))
	return nil
}

// assertBodyVarDecl asserts that the wrapper Block declares a single
// `<var>_body TEXT` variable. The body variable name is derived from
// the original trigger-builder variable's name with a `_body` suffix.
func assertBodyVarDecl(t *testing.T, blk *ast.Block, baseVar string) {
	t.Helper()
	require.Equal(t, 1, len(blk.Decls), "wrapper block should declare exactly the body variable")
	dv := blk.Decls[0].(*ast.DeclareVar)
	require.Equal(t, strings.ToLower(baseVar)+"_body", dv.Name)
	udt, ok := dv.Type.(*ast.UserDefinedType)
	require.True(t, ok)
	require.Equal(t, "text", udt.Name)
}

// TestDynamicTrigger_StructuralRewrite confirms the visitor wraps the
// canonical "build a CREATE TRIGGER then EXECUTE IMMEDIATE" run in a
// nested DECLARE Block that splits the variable into a header half
// (the original <var>) and a body half (<var>_body). The two
// trailing EXECUTEs invoke CREATE FUNCTION (using the body) and
// CREATE TRIGGER (using the header).
func TestDynamicTrigger_StructuralRewrite(t *testing.T) {
	// Block:
	//   L_VC_STMT := 'CREATE OR REPLACE trigger ' || 'TRG_X '
	//             || 'BEFORE INSERT ON T1 FOR EACH ROW '
	//             || 'DECLARE v varchar2(3); '
	//             || 'BEGIN NEW.X := 1; END;';
	//   EXECUTE IMMEDIATE L_VC_STMT;
	blk := &ast.Block{
		Stmts: []ast.PLStmt{
			&ast.AssignStmt{Target: "L_VC_STMT", Expr: concatN(
				litStr("CREATE OR REPLACE trigger "),
				litStr("TRG_X "),
				litStr("BEFORE INSERT ON T1 FOR EACH ROW "),
				litStr("DECLARE v varchar2(3); "),
				litStr("BEGIN NEW.X := 1; END;"),
			)},
			&ast.ExecuteImmediateStmt{SQL: ident("L_VC_STMT")},
		},
	}

	counter := 0
	v := MakeOracleDynamicTriggerBuildVisitor("mig", &counter)
	rewritten := ast.Rewrite(blk, v)
	rb := rewritten.(*ast.Block)

	// Top-level should hold exactly one synthetic wrapper Block.
	require.Equal(t, 1, len(rb.Stmts), "want 1 wrapper Block at top level, got: %d", len(rb.Stmts))
	wrapper := findInnerBlock(t, rb.Stmts)
	assertBodyVarDecl(t, wrapper, "L_VC_STMT")

	// Wrapper body: <var> hdr-init, <var>_body init, EXEC fn, EXEC trg.
	require.Equal(t, 4, len(wrapper.Stmts), "wrapper should contain 4 stmts; got %#v", wrapper.Stmts)

	// stmt[0]: AssignStmt to L_VC_STMT (header init).
	hdr := wrapper.Stmts[0].(*ast.AssignStmt)
	require.Equal(t, "L_VC_STMT", hdr.Target)
	hdrParts := flattenConcat(hdr.Expr)
	hdrFirst := hdrParts[0].(*ast.Literal)
	require.NotContains(t, strings.ToUpper(hdrFirst.Text), "CREATE OR REPLACE TRIGGER", "header init must NOT start with CREATE TRIGGER prefix")

	// stmt[1]: AssignStmt to l_vc_stmt_body (body init starting with
	// DECLARE).
	body := wrapper.Stmts[1].(*ast.AssignStmt)
	require.Equal(t, "l_vc_stmt_body", body.Target)
	bodyParts := flattenConcat(body.Expr)
	bodyFirst := bodyParts[0].(*ast.Literal)
	require.True(t,
		strings.HasPrefix(strings.ToUpper(bodyFirst.Text), "DECLARE") || strings.HasPrefix(strings.ToUpper(bodyFirst.Text), "BEGIN"),
		"body init must start with DECLARE or BEGIN, got: %q", bodyFirst.Text)

	// stmt[2]: EXECUTE IMMEDIATE for the function DDL — its SQL expr
	// references l_vc_stmt_body.
	fn := wrapper.Stmts[2].(*ast.ExecuteImmediateStmt)
	fnParts := flattenConcat(fn.SQL)
	fnFirst := fnParts[0].(*ast.Literal)
	require.Contains(t, strings.ToUpper(fnFirst.Text), "CREATE OR REPLACE FUNCTION")
	require.Contains(t, strings.ToUpper(fnFirst.Text), "RETURNS TRIGGER")
	require.Contains(t, fnFirst.Text, "$f$")

	// stmt[3]: EXECUTE IMMEDIATE for the trigger DDL — its SQL expr
	// references l_vc_stmt.
	trg := wrapper.Stmts[3].(*ast.ExecuteImmediateStmt)
	trgParts := flattenConcat(trg.SQL)
	trgFirst := trgParts[0].(*ast.Literal)
	require.Equal(t, "CREATE TRIGGER ", trgFirst.Text)
	trgLast := trgParts[len(trgParts)-1].(*ast.Literal)
	require.Contains(t, trgLast.Text, "EXECUTE FUNCTION")
}

// TestDynamicTrigger_NoMatch leaves a non-matching block untouched.
func TestDynamicTrigger_NoMatch(t *testing.T) {
	blk := &ast.Block{
		Stmts: []ast.PLStmt{
			&ast.AssignStmt{Target: "X", Expr: litStr("hello")},
		},
	}
	counter := 0
	v := MakeOracleDynamicTriggerBuildVisitor("mig", &counter)
	rewritten := ast.Rewrite(blk, v)
	rb := rewritten.(*ast.Block)
	require.Equal(t, 1, len(rb.Stmts))
}

// TestDynamicTrigger_DbmsSqlParseSinkInIfBranch covers the DRSE
// pattern: the trigger build is sunk via CALL dbms_sql.parse(cur,
// var) inside an IfStmt (chosen at runtime between scalar/array
// chunked variants). The new design REPLACES the entire IfStmt with
// the two EXECUTE stmts (the runtime chunking is unnecessary in PG —
// TEXT has no varchar2 size limit), so the IfStmt's branches don't
// survive.
func TestDynamicTrigger_DbmsSqlParseSinkInIfBranch(t *testing.T) {
	parseCall := func(varName string) ast.PLStmt {
		return &ast.CallStmt{
			Schema: "dbms_sql",
			Name:   "parse",
			Args: []ast.Expr{
				ident("cur"),
				ident(varName),
			},
		}
	}
	blk := &ast.Block{
		Stmts: []ast.PLStmt{
			&ast.AssignStmt{Target: "L_SQLCMD", Expr: concatN(
				litStr("CREATE OR REPLACE trigger "),
				litStr("T BEFORE INSERT ON X FOR EACH ROW "),
				litStr("BEGIN NEW.x := 1; END;"),
			)},
			&ast.IfStmt{
				Branches: []ast.IfBranch{{
					Cond: ident("dummy"),
					Body: []ast.PLStmt{parseCall("L_SQLCMD_S")},
				}},
				Else: []ast.PLStmt{parseCall("L_SQLCMD")},
			},
		},
	}
	counter := 0
	v := MakeOracleDynamicTriggerBuildVisitor("mig", &counter)
	rewritten := ast.Rewrite(blk, v)
	rb := rewritten.(*ast.Block)

	// Top-level: one wrapper Block.
	require.Equal(t, 1, len(rb.Stmts))
	wrapper := findInnerBlock(t, rb.Stmts)
	assertBodyVarDecl(t, wrapper, "L_SQLCMD")

	// Wrapper body: hdr-init, body-init, EXEC fn, EXEC trg, no-op
	// dbms_sql.parse (so the surrounding cursor execute/close still
	// find a parsed statement on the cursor).
	require.Equal(t, 5, len(wrapper.Stmts))

	// No surviving IfStmt — the dyn-chunk runtime IF is replaced by
	// direct EXECUTEs.
	for i, s := range wrapper.Stmts {
		_, isIf := s.(*ast.IfStmt)
		require.False(t, isIf, "stmt[%d] should not be an IfStmt anymore", i)
	}

	// stmt[2]: EXECUTE IMMEDIATE for the function — must reference
	// l_sqlcmd_body, NOT l_sqlcmd_s (the unused chunk variable).
	fn := wrapper.Stmts[2].(*ast.ExecuteImmediateStmt)
	fnParts := flattenConcat(fn.SQL)
	hasBodyRef := false
	for _, p := range fnParts {
		if id, ok := p.(*ast.Ident); ok && len(id.Parts) == 1 && strings.EqualFold(id.Parts[0], "l_sqlcmd_body") {
			hasBodyRef = true
			break
		}
	}
	require.True(t, hasBodyRef, "function EXECUTE should reference L_SQLCMD_body; parts: %#v", fnParts)
}

// TestDynamicTrigger_NoDeclare handles the no-DECLARE shape: trigger
// body starts directly with BEGIN…END.
func TestDynamicTrigger_NoDeclare(t *testing.T) {
	blk := &ast.Block{
		Stmts: []ast.PLStmt{
			&ast.AssignStmt{Target: "S", Expr: concatN(
				litStr("CREATE OR REPLACE trigger "),
				litStr("T2 BEFORE INSERT ON X FOR EACH ROW "),
				litStr("BEGIN NEW.x := 1; END;"),
			)},
			&ast.ExecuteImmediateStmt{SQL: ident("S")},
		},
	}
	counter := 0
	v := MakeOracleDynamicTriggerBuildVisitor("mig", &counter)
	rewritten := ast.Rewrite(blk, v)
	rb := rewritten.(*ast.Block)
	require.Equal(t, 1, len(rb.Stmts))
	wrapper := findInnerBlock(t, rb.Stmts)
	assertBodyVarDecl(t, wrapper, "S")
	require.Equal(t, 4, len(wrapper.Stmts))
}
