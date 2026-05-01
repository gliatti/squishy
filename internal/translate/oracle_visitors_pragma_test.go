package translate

import (
	"strings"
	"testing"

	"gitlab.com/dalibo/squishy/internal/dialects"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

func TestVisitOracleExceptionInit_StampsRaiseStmt(t *testing.T) {
	// PRAGMA EXCEPTION_INIT(my_excp, -20001); ...; RAISE my_excp;
	blk := &ast.Block{
		Decls: []ast.PLDecl{
			&ast.PragmaStmt{Kind: "EXCEPTION_INIT", Text: "my_excp, -20001"},
		},
		Stmts: []ast.PLStmt{
			&ast.RaiseStmt{Name: "my_excp"},
		},
	}
	out := VisitOracleExceptionInit(blk)
	rep, ok := out.(*ast.Block)
	if !ok {
		t.Fatalf("want Block, got %T", out)
	}
	rs, ok := rep.Stmts[0].(*ast.RaiseStmt)
	if !ok {
		t.Fatalf("Stmts[0] want RaiseStmt, got %T", rep.Stmts[0])
	}
	if rs.SQLState == "" {
		t.Fatalf("SQLState must be stamped on RaiseStmt")
	}
	if !strings.HasPrefix(rs.SQLState, "45") {
		t.Errorf("SQLState should be in 45XXX class (user-defined), got %q", rs.SQLState)
	}
	if len(rs.SQLState) != 5 {
		t.Errorf("SQLState must be 5 chars, got %q", rs.SQLState)
	}
}

func TestVisitOracleExceptionInit_CaseInsensitiveMatch(t *testing.T) {
	blk := &ast.Block{
		Decls: []ast.PLDecl{
			&ast.PragmaStmt{Kind: "EXCEPTION_INIT", Text: "FK_VIOLATION, -2291"},
		},
		Stmts: []ast.PLStmt{
			&ast.RaiseStmt{Name: "fk_violation"}, // lowercase here
		},
	}
	out := VisitOracleExceptionInit(blk).(*ast.Block)
	rs := out.Stmts[0].(*ast.RaiseStmt)
	if rs.SQLState == "" {
		t.Errorf("case-insensitive name match should have stamped SQLSTATE")
	}
}

func TestVisitOracleExceptionInit_NoMatchLeavesAlone(t *testing.T) {
	// RAISE name without a matching PRAGMA — leave SQLState empty so
	// the translator falls back to the P0001 generic class.
	blk := &ast.Block{
		Decls: []ast.PLDecl{
			&ast.PragmaStmt{Kind: "EXCEPTION_INIT", Text: "my_excp, -20001"},
		},
		Stmts: []ast.PLStmt{
			&ast.RaiseStmt{Name: "no_data_found"}, // built-in, not in pragma
		},
	}
	out := VisitOracleExceptionInit(blk).(*ast.Block)
	rs := out.Stmts[0].(*ast.RaiseStmt)
	if rs.SQLState != "" {
		t.Errorf("non-matched RAISE should not have SQLState; got %q", rs.SQLState)
	}
}

func TestVisitOracleExceptionInit_DescendsIntoNestedBlocks(t *testing.T) {
	// IF cond THEN RAISE my_excp; END IF; — the visitor must reach
	// the RaiseStmt buried inside the IfStmt body.
	inner := &ast.RaiseStmt{Name: "my_excp"}
	blk := &ast.Block{
		Decls: []ast.PLDecl{
			&ast.PragmaStmt{Kind: "EXCEPTION_INIT", Text: "my_excp, -20001"},
		},
		Stmts: []ast.PLStmt{
			&ast.IfStmt{
				Branches: []ast.IfBranch{
					{
						Cond: &ast.Literal{Kind: "bool", Text: "TRUE"},
						Body: []ast.PLStmt{inner},
					},
				},
			},
		},
	}
	VisitOracleExceptionInit(blk)
	if inner.SQLState == "" {
		t.Errorf("nested RaiseStmt was not stamped: %#v", inner)
	}
}

func TestVisitOracleExceptionInit_NoBindingsNoOp(t *testing.T) {
	blk := &ast.Block{
		Stmts: []ast.PLStmt{&ast.RaiseStmt{Name: "x"}},
	}
	out := VisitOracleExceptionInit(blk)
	if out != ast.Node(blk) {
		t.Errorf("Block without EXCEPTION_INIT pragma should be unchanged")
	}
}

func TestVisitOracleExceptionInit_PipelineIntegration(t *testing.T) {
	// End-to-end through TranslateRoutineBody — the rendered PL/pgSQL
	// must contain RAISE SQLSTATE '<code>' (not bare RAISE name).
	src := `
		DECLARE
		  fk_violation EXCEPTION;
		  PRAGMA EXCEPTION_INIT(fk_violation, -2291);
		BEGIN
		  IF 1 = 1 THEN
		    RAISE fk_violation;
		  END IF;
		END;
	`
	out, _, _ := TranslateRoutineBody(src, dialects.KindOracle)
	if !strings.Contains(out, "RAISE EXCEPTION") {
		t.Errorf("output must use RAISE EXCEPTION form:\n%s", out)
	}
	if !strings.Contains(out, "ERRCODE = '45") {
		t.Errorf("output must carry an ERRCODE = '45XXX' (user-defined SQLSTATE):\n%s", out)
	}
	if strings.Contains(out, "TODO: Oracle PRAGMA EXCEPTION_INIT dropped") {
		t.Errorf("EXCEPTION_INIT should no longer be flagged as dropped:\n%s", out)
	}
}

func TestParseExceptionInitArgs(t *testing.T) {
	cases := []struct {
		in   string
		name string
		code string
		ok   bool
	}{
		{"my_excp, -20001", "my_excp", "-20001", true},
		{" fk_violation , -2291 ", "fk_violation", "-2291", true},
		{"(my_excp, -20001)", "my_excp", "-20001", true},
		{"only_name", "", "", false},
		{",", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		name, code, ok := parseExceptionInitArgs(c.in)
		if ok != c.ok || name != c.name || code != c.code {
			t.Errorf("parseExceptionInitArgs(%q) = (%q, %q, %v); want (%q, %q, %v)",
				c.in, name, code, ok, c.name, c.code, c.ok)
		}
	}
}
