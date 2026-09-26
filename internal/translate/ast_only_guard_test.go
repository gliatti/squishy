package translate

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ast_only_guard_test.go — enforces the CLAUDE.md "No regex anywhere —
// AST only" rule on the MySQL/MariaDB translation path with go/parser +
// go/ast (no regex, no substring search on source text):
//
//	(a) no package imports regexp or regexp/syntax;
//	(b) internal/translate never calls the MySQL lexer (NewLexer /
//	    Tokenize) — the MySQL path parses once and rewrites the AST;
//	    parser entry points (ParseRoutineBody, ParseSelect, ParseExpr…)
//	    stay allowed;
//	(c) the removed text rewriters are not resurrected under their old
//	    names;
//	(d) body_rewrite.go / routine_body.go do not come back.

// astOnlyMySQLDialectPath is the import path of the MySQL dialect
// package, whose lexer must not be reached from internal/translate.
const astOnlyMySQLDialectPath = "gitlab.com/dalibo/squishy/internal/dialects/mysql"

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

// astOnlyForbiddenLexerSels are the MySQL dialect selectors rejected
// by check (b).
var astOnlyForbiddenLexerSels = map[string]bool{
	"NewLexer": true,
	"Tokenize": true,
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

// astOnlyGuardDirs maps each directory scanned by the repo-level guard
// (relative to this package's directory) to the logical path used in
// the violation report and by check (b).
var astOnlyGuardDirs = []struct{ fsDir, logical string }{
	{".", astOnlyTranslateDir},
	{"../planner", "internal/planner"},
	{"../worker", "internal/worker"},
	{"../httpapi", "internal/httpapi"},
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
		mysqlNames := map[string]bool{}
		if inTranslate {
			for _, imp := range file.Imports {
				p, err := strconv.Unquote(imp.Path.Value)
				if err != nil || p != astOnlyMySQLDialectPath {
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
					report(fset, imp.Pos(), "dot import of %s hides MySQL lexer calls; import it under a name", p)
				default:
					mysqlNames[local] = true
				}
			}
		}

		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if !inTranslate || !astOnlyForbiddenLexerSels[x.Sel.Name] {
					return true
				}
				if id, ok := x.X.(*ast.Ident); ok && mysqlNames[id.Name] {
					report(fset, x.Pos(), "%s.%s re-lexes SQL text with the MySQL lexer; parse once and use ast.Rewrite + the PG writer", id.Name, x.Sel.Name)
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

// loadASTOnlyGuardSources reads every non-test .go file of the guarded
// directories into a checkASTOnly input map keyed by logical path.
func loadASTOnlyGuardSources(t *testing.T) map[string]string {
	t.Helper()
	fsys := map[string]string{}
	for _, d := range astOnlyGuardDirs {
		entries, err := os.ReadDir(d.fsDir)
		if err != nil {
			t.Fatalf("read dir %s: %v", d.fsDir, err)
		}
		for _, e := range entries {
			n := e.Name()
			if e.IsDir() || path.Ext(n) != ".go" || strings.HasSuffix(n, "_test.go") {
				continue
			}
			b, err := os.ReadFile(path.Join(d.fsDir, n))
			if err != nil {
				t.Fatalf("read %s: %v", path.Join(d.fsDir, n), err)
			}
			fsys[path.Join(d.logical, n)] = string(b)
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
			// Escaped quotes keep the literal import line out of plain
			// greps for the quoted path.
			src:  "package planner\n\nimport \"regexp\"\n\nvar re = regexp.MustCompile(\"x\")\n",
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
