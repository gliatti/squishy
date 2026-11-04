package tsql

import (
	"testing"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// mustParse parses a script and fails the test on any error.
func mustParse(t *testing.T, src string) []ast.Stmt {
	t.Helper()
	stmts, errs := Parse(src)
	if len(errs) > 0 {
		t.Fatalf("Parse(%q): %v", src, errs)
	}
	return stmts
}

// mustParseOne parses a script holding exactly one statement of type T.
func mustParseOne[T ast.Stmt](t *testing.T, src string) T {
	t.Helper()
	stmts := mustParse(t, src)
	if len(stmts) != 1 {
		t.Fatalf("Parse(%q): want 1 statement, got %d (%#v)", src, len(stmts), stmts)
	}
	s, ok := stmts[0].(T)
	if !ok {
		var zero T
		t.Fatalf("Parse(%q): want %T, got %T", src, zero, stmts[0])
	}
	return s
}

// mustBody parses a routine body and fails the test on any error.
func mustBody(t *testing.T, src string) []ast.PLStmt {
	t.Helper()
	stmts, errs := ParseRoutineBody(src)
	if len(errs) > 0 {
		t.Fatalf("ParseRoutineBody(%q): %v", src, errs)
	}
	return stmts
}

// mustBodyOne parses a body holding exactly one statement of type T.
func mustBodyOne[T ast.PLStmt](t *testing.T, src string) T {
	t.Helper()
	stmts := mustBody(t, src)
	if len(stmts) != 1 {
		t.Fatalf("ParseRoutineBody(%q): want 1 statement, got %d (%#v)", src, len(stmts), stmts)
	}
	s, ok := stmts[0].(T)
	if !ok {
		var zero T
		t.Fatalf("ParseRoutineBody(%q): want %T, got %T", src, zero, stmts[0])
	}
	return s
}

// mustExpr parses one expression and fails the test on any error.
func mustExpr(t *testing.T, src string) ast.Expr {
	t.Helper()
	e, errs := ParseExpr(src)
	if len(errs) > 0 {
		t.Fatalf("ParseExpr(%q): %v", src, errs)
	}
	return e
}

// mustSelect parses one SELECT and fails the test on any error.
func mustSelect(t *testing.T, src string) *ast.SelectStmt {
	t.Helper()
	s, errs := ParseSelect(src)
	if len(errs) > 0 {
		t.Fatalf("ParseSelect(%q): %v", src, errs)
	}
	if s == nil {
		t.Fatalf("ParseSelect(%q): nil statement", src)
	}
	return s
}

// as asserts the dynamic type of a node.
func as[T any](t *testing.T, n any) T {
	t.Helper()
	v, ok := n.(T)
	if !ok {
		var zero T
		t.Fatalf("want %T, got %T (%#v)", zero, n, n)
	}
	return v
}

// identText joins the parts of an *ast.Ident.
func identText(t *testing.T, e ast.Expr) string {
	t.Helper()
	id := as[*ast.Ident](t, e)
	out := ""
	for i, p := range id.Parts {
		if i > 0 {
			out += "."
		}
		out += p
	}
	return out
}
