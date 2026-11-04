package tsql

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// Corpus tests: real SQL Server scripts must parse with zero errors, every
// CREATE TABLE / VIEW / PROCEDURE / FUNCTION / TRIGGER of the script must
// come out as a typed node, and the routine bodies / view queries must be
// fully typed (no RawSQL, no untyped RawExpr; only the deliberately opaque
// XML-method RawExprs are allowed).
//
// The sample dumps are not committed (testdata/*.sql is gitignored); fetch
// them with
//
//	curl -sSfL -o testdata/instnwnd.sql https://raw.githubusercontent.com/microsoft/sql-server-samples/master/samples/databases/northwind-pubs/instnwnd.sql
//	curl -sSfL -o testdata/instpubs.sql https://raw.githubusercontent.com/microsoft/sql-server-samples/master/samples/databases/northwind-pubs/instpubs.sql
//
// and unzip instawdb.sql of the sql-server-samples `adventureworks` release
// (AdventureWorks-oltp-install-script.zip) into testdata/ (testdata/*.sql is
// gitignored: keep the dumps flat in testdata/).
// A missing file skips its test.

func readCorpus(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(rel))
	if err != nil {
		t.Skipf("corpus %s not available: %v", rel, err)
	}
	return string(b)
}

// creates counts, from the token stream, the CREATE [OR ALTER] statements
// of each object kind (DDL / LOGON triggers, ON DATABASE / ON ALL SERVER,
// are not table triggers and are left out).
func creates(src string) map[string]int {
	toks := Tokenize(src)
	out := map[string]int{}
	for i := 0; i < len(toks); i++ {
		if !tokIsKw(toks[i], "CREATE") {
			continue
		}
		j := i + 1
		if j+1 < len(toks) && tokIsKw(toks[j], "OR") && tokIsKw(toks[j+1], "ALTER") {
			j += 2
		}
		if j >= len(toks) {
			break
		}
		kind := ""
		switch {
		case tokIsKw(toks[j], "TABLE"):
			kind = "TABLE"
		case tokIsKw(toks[j], "VIEW"):
			kind = "VIEW"
		case tokIsKw(toks[j], "PROC"), tokIsKw(toks[j], "PROCEDURE"):
			kind = "PROCEDURE"
		case tokIsKw(toks[j], "FUNCTION"):
			kind = "FUNCTION"
		case tokIsKw(toks[j], "TRIGGER"):
			kind = "TRIGGER"
			// skip the (possibly qualified) name, then look at the target
			k := j + 1
			for k+1 < len(toks) && toks[k+1].Kind == TOK_PUNCT && toks[k+1].Lit == "." {
				k += 2
			}
			if k+2 < len(toks) && tokIsKw(toks[k+1], "ON") && (tokIsKw(toks[k+2], "DATABASE") || tokIsKw(toks[k+2], "ALL")) {
				kind = ""
			}
		}
		if kind != "" {
			out[kind]++
		}
	}
	return out
}

// rawFinder collects the RawSQL nodes and the RawExpr nodes whose Kind is
// not in allowed.
type rawFinder struct {
	allowed map[string]bool
	found   []string
}

func (f *rawFinder) Visit(n ast.Node) ast.Visitor {
	switch x := n.(type) {
	case *ast.RawSQL:
		f.found = append(f.found, "RawSQL: "+x.Text)
	case *ast.RawExpr:
		if !f.allowed[x.Kind] {
			f.found = append(f.found, "RawExpr["+x.Kind+"]: "+x.Text)
		}
	}
	return f
}

type corpusCase struct {
	name string
	path string
}

func TestCorpus(t *testing.T) {
	cases := []corpusCase{
		{"fixture", "../../../test/fixtures/sqlserver_sample.sql"},
		{"northwind", "testdata/instnwnd.sql"},
		{"pubs", "testdata/instpubs.sql"},
		{"adventureworks", "testdata/instawdb.sql"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := readCorpus(t, c.path)
			stmts, errs := Parse(src)
			for i, e := range errs {
				if i >= 30 {
					t.Errorf("… and %d more errors", len(errs)-30)
					break
				}
				t.Errorf("parse error: %v", e)
			}
			want := creates(src)
			got := map[string]int{}
			allowed := map[string]bool{"XML_METHOD": true}
			for _, s := range stmts {
				switch x := s.(type) {
				case *ast.CreateTable:
					got["TABLE"]++
				case *ast.CreateView:
					got["VIEW"]++
					if x.Select == nil {
						t.Errorf("view %s.%s: body not typed: %s", x.View.Schema, x.View.Name, x.SelectParseError)
						continue
					}
					f := &rawFinder{allowed: allowed}
					ast.Walk(f, x.Select)
					for _, r := range f.found {
						t.Errorf("view %s.%s: %s", x.View.Schema, x.View.Name, r)
					}
				case *ast.CreateProcedure:
					got["PROCEDURE"]++
					checkBody(t, "procedure "+x.Schema+"."+x.Name, x.Body, allowed)
				case *ast.CreateFunction:
					got["FUNCTION"]++
					if x.ReturnsInlineTable {
						if x.Select == nil {
							t.Errorf("function %s.%s: inline query not typed: %s", x.Schema, x.Name, x.SelectParseError)
						}
						continue
					}
					checkBody(t, "function "+x.Schema+"."+x.Name, x.Body, allowed)
				case *ast.CreateTrigger:
					got["TRIGGER"]++
					checkBody(t, "trigger "+x.Schema+"."+x.Name, x.Body, allowed)
				}
			}
			for _, k := range []string{"TABLE", "VIEW", "PROCEDURE", "FUNCTION", "TRIGGER"} {
				if got[k] != want[k] {
					t.Errorf("CREATE %s: %d typed nodes, %d in the script", k, got[k], want[k])
				}
			}
			t.Logf("%d statements, creates %v", len(stmts), want)
		})
	}
}

func checkBody(t *testing.T, what, body string, allowed map[string]bool) {
	t.Helper()
	if strings.TrimSpace(body) == "" {
		t.Errorf("%s: empty body", what)
		return
	}
	plStmts, errs := ParseRoutineBody(body)
	for _, e := range errs {
		t.Errorf("%s: body parse error: %v", what, e)
	}
	f := &rawFinder{allowed: allowed}
	for _, s := range plStmts {
		ast.Walk(f, s)
	}
	for _, r := range f.found {
		t.Errorf("%s: %s", what, r)
	}
}
