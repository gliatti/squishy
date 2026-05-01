package postgres

import (
	"strings"
	"testing"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// TestWritePLStmt_Assign — `target := expr;` round-trip with a typed
// expression operand.
func TestWritePLStmt_Assign(t *testing.T) {
	got := WritePLStmt(&ast.AssignStmt{
		Target: "x",
		Expr: &ast.BinaryExpr{
			Op:  "+",
			Lhs: &ast.Ident{Parts: []string{"a"}},
			Rhs: &ast.Literal{Kind: "number", Text: "1"},
		},
	})
	want := `x := "a" + 1;` + "\n"
	if got != want {
		t.Errorf("Assign: got %q, want %q", got, want)
	}
}

// TestWritePLStmt_If — IF / ELSIF / ELSE / END IF; structure.
func TestWritePLStmt_If(t *testing.T) {
	stmt := &ast.IfStmt{
		Branches: []ast.IfBranch{
			{
				Cond: &ast.BinaryExpr{Op: ">", Lhs: &ast.Ident{Parts: []string{"x"}}, Rhs: &ast.Literal{Kind: "number", Text: "0"}},
				Body: []ast.PLStmt{&ast.AssignStmt{Target: "y", Expr: &ast.Literal{Kind: "number", Text: "1"}}},
			},
			{
				Cond: &ast.BinaryExpr{Op: "<", Lhs: &ast.Ident{Parts: []string{"x"}}, Rhs: &ast.Literal{Kind: "number", Text: "0"}},
				Body: []ast.PLStmt{&ast.AssignStmt{Target: "y", Expr: &ast.Literal{Kind: "number", Text: "-1"}}},
			},
		},
		Else: []ast.PLStmt{&ast.AssignStmt{Target: "y", Expr: &ast.Literal{Kind: "number", Text: "0"}}},
	}
	got := WritePLStmt(stmt)
	want := `IF "x" > 0 THEN
  y := 1;
ELSIF "x" < 0 THEN
  y := -1;
ELSE
  y := 0;
END IF;
`
	if got != want {
		t.Errorf("If:\n  got\n%s\n  want\n%s", got, want)
	}
}

// TestWritePLStmt_Loop — labelled LOOP with EXIT WHEN inside.
func TestWritePLStmt_Loop(t *testing.T) {
	stmt := &ast.LoopStmt{
		Label: "outer",
		Body: []ast.PLStmt{
			&ast.LeaveStmt{
				Label: "outer",
				WhenCond: &ast.BinaryExpr{
					Op:  ">=",
					Lhs: &ast.Ident{Parts: []string{"i"}},
					Rhs: &ast.Literal{Kind: "number", Text: "10"},
				},
			},
		},
	}
	got := WritePLStmt(stmt)
	if !strings.Contains(got, "<<outer>>") {
		t.Errorf("missing label: %q", got)
	}
	if !strings.Contains(got, `EXIT outer WHEN "i" >= 10;`) {
		t.Errorf("missing labelled EXIT WHEN: %q", got)
	}
}

// TestWritePLStmt_NumericFor — FOR i IN low..high LOOP, with REVERSE.
func TestWritePLStmt_NumericFor(t *testing.T) {
	stmt := &ast.NumericForStmt{
		Var:     "I",
		Reverse: true,
		Low:     &ast.Literal{Kind: "number", Text: "1"},
		High:    &ast.Literal{Kind: "number", Text: "10"},
		Body: []ast.PLStmt{
			&ast.NullStmt{},
		},
	}
	got := WritePLStmt(stmt)
	want := `FOR i IN REVERSE 1..10 LOOP
  NULL;
END LOOP;
`
	if got != want {
		t.Errorf("NumericFor:\n  got\n%s\n  want\n%s", got, want)
	}
}

// TestWriteBlock_WithDeclareAndException — full procedure shape.
func TestWriteBlock_WithDeclareAndException(t *testing.T) {
	blk := &ast.Block{
		Decls: []ast.PLDecl{
			&ast.DeclareVar{
				Name: "x",
				Type: &ast.UserDefinedType{Name: "integer"},
				Default: &ast.Literal{Kind: "number", Text: "0"},
			},
		},
		Stmts: []ast.PLStmt{
			&ast.AssignStmt{Target: "x", Expr: &ast.Literal{Kind: "number", Text: "1"}},
		},
		Except: &ast.ExceptionBlock{
			Handlers: []ast.ExceptionHandler{
				{
					Names: []string{"OTHERS"},
					Body:  []ast.PLStmt{&ast.RaiseStmt{}},
				},
			},
		},
	}
	got := WriteBlock(blk)
	mustContain := []string{
		"DECLARE",
		"x integer := 0;",
		"BEGIN",
		"x := 1;",
		"EXCEPTION",
		"WHEN OTHERS THEN",
		"RAISE;",
		"END;",
	}
	for _, m := range mustContain {
		if !strings.Contains(got, m) {
			t.Errorf("Block missing %q in:\n%s", m, got)
		}
	}
}

// TestWritePLStmt_ExecuteImmediate — INTO and USING clauses.
func TestWritePLStmt_ExecuteImmediate(t *testing.T) {
	stmt := &ast.ExecuteImmediateStmt{
		SQL: &ast.BinaryExpr{
			Op:  "||",
			Lhs: &ast.Literal{Kind: "string", Text: "SELECT count(*) FROM "},
			Rhs: &ast.Ident{Parts: []string{"v_table"}},
		},
		Into:  []string{"v_count"},
		Using: []ast.Expr{&ast.Ident{Parts: []string{"v_arg"}}},
	}
	got := WritePLStmt(stmt)
	want := `EXECUTE 'SELECT count(*) FROM ' || "v_table" INTO v_count USING "v_arg";` + "\n"
	if got != want {
		t.Errorf("ExecuteImmediate:\n  got %q\n  want %q", got, want)
	}
}

// TestWritePLStmt_NilSafe — nil input returns "".
func TestWritePLStmt_NilSafe(t *testing.T) {
	if got := WritePLStmt(nil); got != "" {
		t.Errorf("WritePLStmt(nil) = %q", got)
	}
	if got := WriteBlock(nil); got != "" {
		t.Errorf("WriteBlock(nil) = %q", got)
	}
}
