package translate

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// ast_only_guard_test.go — enforces the CLAUDE.md "No regex anywhere —
// AST only" rule on the MySQL/MariaDB translation path with go/parser +
// go/ast (no regex, no substring search on source text):
//
//	(a) no package imports regexp or regexp/syntax;
//	(b) internal/translate never calls the lexer of an AST-only dialect
//	    (MySQL / MariaDB, SQL Server: NewLexer / Tokenize) — those paths
//	    parse once and rewrite the AST; parser entry points
//	    (ParseRoutineBody, ParseSelect, ParseExpr…) stay allowed;
//	(c) the removed text rewriters are not resurrected under their old
//	    names;
//	(d) body_rewrite.go / routine_body.go do not come back.

// astOnlyLexerDialect describes one AST-only dialect package for check
// (b): the selectors of it that re-lex SQL text, and the dialect label
// used in the violation message.
type astOnlyLexerDialect struct {
	label string
	sels  map[string]bool
}

// astOnlyLexerDialects maps the import path of every AST-only dialect
// package to its forbidden lexer selectors.
var astOnlyLexerDialects = map[string]astOnlyLexerDialect{
	"gitlab.com/dalibo/squishy/internal/dialects/mysql": {label: "MySQL", sels: map[string]bool{"NewLexer": true, "Tokenize": true}},
	"gitlab.com/dalibo/squishy/internal/dialects/tsql":  {label: "T-SQL", sels: map[string]bool{"NewLexer": true, "Tokenize": true}},
}

// astOnlyTranslateDir is the logical directory whose files are subject
// to check (b).
const astOnlyTranslateDir = "internal/translate"

// astOnlyForbiddenImports are the import paths rejected by check (a).
// Keys are raw string literals, so a plain grep for the quoted import
// path over the repo only ever finds real imports.
var astOnlyForbiddenImports = map[string]bool{
	`regexp`:        true,
	`regexp/syntax`: true,
}

// astOnlyForbiddenIdents are the legacy text-rewriter names rejected by
// check (c).
var astOnlyForbiddenIdents = map[string]bool{
	"rewriteMySQLBody":       true,
	"rewriteMySQLInterval":   true,
	"RewriteRoutineBody":     true,
	"rewriteGroupConcat":     true,
	"rewriteJSONExtractPath": true,
	"rewriteBareJoinAsCross": true,
}

// astOnlyForbiddenFiles are the deleted files rejected by check (d),
// relative to internal/translate.
var astOnlyForbiddenFiles = []string{"body_rewrite.go", "routine_body.go"}

// astOnlyGuardRoots are the trees scanned by the repo-level guard, walked
// recursively (relative to this package's directory), with the logical
// path of each root used in the violation report and by check (b). Check
// (a) holds for every package of the repository: CLAUDE.md forbids regex
// in discovery, inspection, copy and connection code as much as in the
// translator, and a new package is covered without being listed.
var astOnlyGuardRoots = []struct{ fsDir, logical string }{
	{"..", "internal"},
	{"../../cmd", "cmd"},
	{"../../pkg", "pkg"},
	{"../../mcp-server", "mcp-server"},
}

// checkASTOnly runs checks (a), (b) and (c) over fsys, a map of logical
// slash-separated file name (e.g. "internal/translate/foo.go") → Go
// source. It returns one "file:line: message" string per violation, in
// deterministic order. Check (b) applies only to files whose directory
// is internal/translate. A file that does not parse is itself reported:
// the guard cannot vouch for what it cannot read.
func checkASTOnly(fsys map[string]string) []string {
	names := make([]string, 0, len(fsys))
	for name := range fsys {
		names = append(names, name)
	}
	sort.Strings(names)

	var out []string
	for _, name := range names {
		src := fsys[name]
		report := func(fset *token.FileSet, pos token.Pos, format string, args ...any) {
			p := fset.Position(pos)
			out = append(out, fmt.Sprintf("%s:%d: %s", name, p.Line, fmt.Sprintf(format, args...)))
		}

		// (a) imports only.
		importFset := token.NewFileSet()
		importsOnly, err := parser.ParseFile(importFset, name, src, parser.ImportsOnly)
		if err != nil {
			out = append(out, fmt.Sprintf("%s: parse error (imports): %v", name, err))
			continue
		}
		for _, imp := range importsOnly.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				report(importFset, imp.Pos(), "unreadable import path %s", imp.Path.Value)
				continue
			}
			if astOnlyForbiddenImports[p] {
				report(importFset, imp.Pos(), "forbidden import of package %s (CLAUDE.md: no regex anywhere, AST only)", p)
			}
		}

		// (b) and (c) need the full file.
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			out = append(out, fmt.Sprintf("%s: parse error: %v", name, err))
			continue
		}

		inTranslate := path.Dir(name) == astOnlyTranslateDir
		// local package name → the AST-only dialect it imports.
		lexerNames := map[string]astOnlyLexerDialect{}
		if inTranslate {
			for _, imp := range file.Imports {
				p, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					continue
				}
				d, ok := astOnlyLexerDialects[p]
				if !ok {
					continue
				}
				local := path.Base(p)
				if imp.Name != nil {
					local = imp.Name.Name
				}
				switch local {
				case "_":
					// Blank import: no selector can reach the lexer.
				case ".":
					// A dot import would let NewLexer appear unqualified.
					report(fset, imp.Pos(), "dot import of %s hides %s lexer calls; import it under a name", p, d.label)
				default:
					lexerNames[local] = d
				}
			}
		}

		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if !inTranslate {
					return true
				}
				id, ok := x.X.(*ast.Ident)
				if !ok {
					return true
				}
				if d, ok := lexerNames[id.Name]; ok && d.sels[x.Sel.Name] {
					report(fset, x.Pos(), "%s.%s re-lexes SQL text with the %s lexer; parse once and use ast.Rewrite + the PG writer", id.Name, x.Sel.Name, d.label)
				}
			case *ast.Ident:
				if astOnlyForbiddenIdents[x.Name] {
					report(fset, x.Pos(), "identifier %s belongs to the removed MySQL text rewriter", x.Name)
				}
			}
			return true
		})
	}
	return out
}

// loadASTOnlyGuardSources reads every non-test .go file under the guarded
// roots (testdata and vendor trees aside) into a checkASTOnly input map
// keyed by logical path. internal must be there; the other roots are
// skipped when the checkout has none.
func loadASTOnlyGuardSources(t *testing.T) map[string]string {
	t.Helper()
	fsys := map[string]string{}
	for _, d := range astOnlyGuardRoots {
		if _, err := os.Stat(d.fsDir); err != nil {
			if os.IsNotExist(err) && d.logical != "internal" {
				continue
			}
			t.Fatalf("stat %s: %v", d.fsDir, err)
		}
		err := filepath.WalkDir(d.fsDir, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			n := e.Name()
			if e.IsDir() {
				if n == "testdata" || n == "vendor" || n == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if path.Ext(n) != ".go" || strings.HasSuffix(n, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(d.fsDir, p)
			if err != nil {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			fsys[path.Join(d.logical, filepath.ToSlash(rel))] = string(b)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", d.fsDir, err)
		}
	}
	return fsys
}

// checkASTOnlyRepo runs the guard on the real tree: checks (a)–(c) on
// the loaded sources, plus check (d) via os.Stat.
func checkASTOnlyRepo(t *testing.T) []string {
	t.Helper()
	fsys := loadASTOnlyGuardSources(t)
	if len(fsys) == 0 {
		t.Fatal("AST-only guard loaded no source file; wrong working directory?")
	}
	violations := checkASTOnly(fsys)
	for _, f := range astOnlyForbiddenFiles {
		if _, err := os.Stat(f); err == nil {
			violations = append(violations, fmt.Sprintf("%s: removed legacy text-rewriter file is back", path.Join(astOnlyTranslateDir, f)))
		} else if !os.IsNotExist(err) {
			violations = append(violations, fmt.Sprintf("%s: stat: %v", path.Join(astOnlyTranslateDir, f), err))
		}
	}
	return violations
}

func TestASTOnlyGuard_DetectsViolations(t *testing.T) {
	cases := []struct {
		name string
		file string
		src  string
		// want is the message fragment expected in the single violation;
		// "" means the file must be clean.
		want string
	}{
		{
			name: "regexp import",
			file: "internal/planner/re.go",
			// Escaped quotes keep the literal import line, and the
			// split selector the literal call, out of plain greps
			// (make check-no-regex greps for both).
			src:  "package planner\n\nimport \"regexp\"\n\nvar re = regexp." + "MustCompile(\"x\")\n",
			want: "forbidden import of package regexp ",
		},
		{
			name: "regexp/syntax import",
			file: "internal/worker/re.go",
			src: `package worker

import rs "regexp/syntax"

var _ = rs.Perl
`,
			want: "forbidden import of package regexp/syntax ",
		},
		{
			name: "aliased MySQL lexer in translate",
			file: "internal/translate/relex.go",
			src: `package translate

import my "gitlab.com/dalibo/squishy/internal/dialects/mysql"

func relex(s string) { _ = my.NewLexer(s) }
`,
			want: "my.NewLexer re-lexes SQL text",
		},
		{
			name: "unaliased MySQL Tokenize in translate",
			file: "internal/translate/relex2.go",
			src: `package translate

import "gitlab.com/dalibo/squishy/internal/dialects/mysql"

func relex(s string) { _ = mysql.Tokenize(s) }
`,
			want: "mysql.Tokenize re-lexes SQL text",
		},
		{
			name: "legacy rewriter declared",
			file: "internal/translate/legacy.go",
			src: `package translate

func rewriteMySQLBody(s string) string { return s }
`,
			want: "identifier rewriteMySQLBody belongs to the removed MySQL text rewriter",
		},
		{
			name: "MySQL parser entry point is allowed",
			file: "internal/translate/ok.go",
			src: `package translate

import mysqldialect "gitlab.com/dalibo/squishy/internal/dialects/mysql"

func parse(s string) { _, _ = mysqldialect.ParseRoutineBody(s) }
`,
		},
		{
			name: "MySQL lexer outside translate is not check (b)",
			file: "internal/worker/lex.go",
			src: `package worker

import "gitlab.com/dalibo/squishy/internal/dialects/mysql"

func lex(s string) { _ = mysql.NewLexer(s) }
`,
		},
		{
			name: "NewLexer on another package is allowed",
			file: "internal/translate/oracle.go",
			src: `package translate

import "gitlab.com/dalibo/squishy/internal/dialects/oracle"

func lex(s string) { _ = oracle.NewLexer(s) }
`,
		},
		{
			name: "aliased T-SQL lexer in translate",
			file: "internal/translate/relex3.go",
			src: `package translate

import tsqldialect "gitlab.com/dalibo/squishy/internal/dialects/tsql"

func relex(s string) { _ = tsqldialect.NewLexer(s) }
`,
			want: "tsqldialect.NewLexer re-lexes SQL text with the T-SQL lexer",
		},
		{
			name: "unaliased T-SQL Tokenize in translate",
			file: "internal/translate/relex4.go",
			src: `package translate

import "gitlab.com/dalibo/squishy/internal/dialects/tsql"

func relex(s string) { _, _ = tsql.Tokenize(s) }
`,
			want: "tsql.Tokenize re-lexes SQL text with the T-SQL lexer",
		},
		{
			name: "dot import of the T-SQL dialect",
			file: "internal/translate/dot.go",
			src: `package translate

import . "gitlab.com/dalibo/squishy/internal/dialects/tsql"

func parse(s string) { _, _ = ParseRoutineBody(s) }
`,
			want: "hides T-SQL lexer calls",
		},
		{
			name: "T-SQL parser entry point is allowed",
			file: "internal/translate/ok2.go",
			src: `package translate

import tsqldialect "gitlab.com/dalibo/squishy/internal/dialects/tsql"

func parse(s string) { _, _ = tsqldialect.ParseRoutineBody(s); _, _ = tsqldialect.ParseExpr(s) }
`,
		},
		{
			name: "T-SQL lexer outside translate is not check (b)",
			file: "internal/planner/lex.go",
			src: `package planner

import "gitlab.com/dalibo/squishy/internal/dialects/tsql"

func lex(s string) { _ = tsql.NewLexer(s) }
`,
		},
		{
			name: "forbidden names in strings and comments are ignored",
			file: "internal/translate/clean.go",
			src: `package translate

// rewriteMySQLBody was removed; regexp is forbidden.
const note = "rewriteMySQLBody regexp"
`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := checkASTOnly(map[string]string{c.file: c.src})
			if c.want == "" {
				if len(got) != 0 {
					t.Fatalf("clean file reported violations:\n%s", strings.Join(got, "\n"))
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("want exactly 1 violation, got %d:\n%s", len(got), strings.Join(got, "\n"))
			}
			// Matching the fixed message text of a single violation line,
			// not scanning SQL or Go source.
			if !strings.Contains(got[0], c.want) || !strings.HasPrefix(got[0], c.file+":") {
				t.Fatalf("violation %q does not name %s / %q", got[0], c.file, c.want)
			}
		})
	}

	t.Run("violations across files are all reported", func(t *testing.T) {
		got := checkASTOnly(map[string]string{
			"internal/httpapi/a.go":   "package httpapi\n\nimport _ \"regexp\"\n",
			"internal/translate/b.go": "package translate\n\nfunc rewriteGroupConcat() {}\n",
			"internal/translate/c.go": "package translate\n",
		})
		if len(got) != 2 {
			t.Fatalf("want 2 violations, got %d:\n%s", len(got), strings.Join(got, "\n"))
		}
	})
}

func TestASTOnlyGuard_RepoIsClean(t *testing.T) {
	violations := checkASTOnlyRepo(t)
	if len(violations) != 0 {
		t.Fatalf("AST-only rule violated (%d):\n%s", len(violations), strings.Join(violations, "\n"))
	}
}

// Check (a) covers every package, not only the translation pipeline:
// the SQL Server discovery, inspection, copy and connection code are
// loaded too (CLAUDE.md forbids regex there as well).
func TestASTOnlyGuard_CoversEveryPackage(t *testing.T) {
	fsys := loadASTOnlyGuardSources(t)
	for _, f := range []string{
		"internal/inspect/sqlserver.go",
		"internal/discover/sqlserver.go",
		"internal/dataxfer/source_dialect.go",
		"internal/connection/types.go",
		"internal/dialects/tsql/parser.go",
		"internal/translate/tsql_visitors.go",
		"cmd/squishy/main.go",
	} {
		require.Contains(t, fsys, f)
	}
}
