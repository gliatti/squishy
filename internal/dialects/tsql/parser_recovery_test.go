package tsql

import (
	"testing"

	"gitlab.com/dalibo/squishy/internal/dialects"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// Error recovery must report the error and resume at the next statement
// without ever swallowing it.
func TestRecovery_NextStatementSurvives(t *testing.T) {
	cases := []struct {
		name string
		src  string
		// the statement types expected after recovery, in order
		want []string
	}{
		{"broken table then table on the next line, no GO",
			"CREATE TABLE a (x int,, y)\nCREATE TABLE b (z int)",
			[]string{"*ast.CreateTable", "*ast.CreateTable"}},
		{"garbage then statements",
			") ) ;\nCREATE TABLE b (z int)\nGO\nCREATE INDEX ix ON b (z)",
			[]string{"*ast.CreateTable", "*ast.CreateIndex"}},
		{"truncated procedure body stays inside its batch",
			"CREATE PROCEDURE p AS BEGIN SELECT\nGO\nCREATE TABLE t (a int)",
			[]string{"*ast.CreateProcedure", "*ast.CreateTable"}},
		{"bad expression does not eat the next statement",
			"ALTER TABLE t ADD CONSTRAINT ck CHECK (a > )\nCREATE TABLE u (a int)",
			[]string{"*ast.AlterTable", "*ast.CreateTable"}},
		{"bad statement inside a script-level IF",
			"IF @x = SELECT 1\nCREATE TABLE u (a int)",
			[]string{"*ast.CreateTable"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stmts, errs := Parse(c.src)
			var got []string
			for _, s := range stmts {
				if _, noop := s.(*ast.NoopStmt); noop {
					continue
				}
				got = append(got, typeName(s))
			}
			if c.name == "truncated procedure body stays inside its batch" {
				// The body is raw: the error only shows when it is parsed.
				if len(errs) != 0 {
					t.Errorf("Parse must not parse the body: %v", errs)
				}
				p := as[*ast.CreateProcedure](t, stmts[0])
				if _, berrs := ParseRoutineBody(p.Body); len(berrs) == 0 {
					t.Errorf("ParseRoutineBody(%q) must fail", p.Body)
				}
			} else if len(errs) == 0 {
				t.Errorf("want a parse error")
			}
			if len(got) != len(c.want) {
				t.Fatalf("statements: want %v, got %v", c.want, got)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("stmt %d: want %s, got %s", i, c.want[i], got[i])
				}
			}
		})
	}
}

func typeName(n ast.Node) string {
	switch n.(type) {
	case *ast.CreateTable:
		return "*ast.CreateTable"
	case *ast.CreateIndex:
		return "*ast.CreateIndex"
	case *ast.CreateProcedure:
		return "*ast.CreateProcedure"
	case *ast.AlterTable:
		return "*ast.AlterTable"
	}
	return "other"
}

func TestRecovery_BodyStatementsResume(t *testing.T) {
	stmts, errs := ParseRoutineBody("BEGIN\n  SET @a = \n  SELECT 1\n  PRINT 'after'\nEND")
	if len(errs) == 0 {
		t.Fatalf("want an error")
	}
	blk := as[*ast.Block](t, stmts[0])
	last := blk.Stmts[len(blk.Stmts)-1]
	if r, ok := last.(*ast.RaiseStmt); !ok || r.Kind != "PRINT" {
		t.Errorf("the PRINT after the error must survive, got %#v", blk.Stmts)
	}
}

func TestRecovery_ErrorPositions(t *testing.T) {
	_, errs := Parse("CREATE TABLE t (\n  a int,\n  b )")
	if len(errs) == 0 {
		t.Fatal("want an error")
	}
	if e := errs[0]; e.Pos.Line != 3 || e.Pos.Col != 5 {
		t.Errorf("error position: %+v", e)
	}
}

func TestRegistered(t *testing.T) {
	d, err := dialects.Get("sqlserver")
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind() != dialects.KindSQLServer || !dialects.IsSQLServer(d.Kind()) || d.Name() != "Microsoft SQL Server 2016-2022" {
		t.Errorf("registered dialect: %v %q", d.Kind(), d.Name())
	}
}
