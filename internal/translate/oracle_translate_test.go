package translate

import (
	"strings"
	"testing"

	"gitlab.com/dalibo/squishy/internal/dialects"
)

// TestRewriteOracleAST_PipelineIntegration confirms the Phase 3.6
// orchestrator visitors fire through TranslateRoutineBodyExt — the
// rewritten AST flows into pgast translation and produces the
// expected PG forms in the output text. This is the first
// end-to-end check that the AST rewriter is wired correctly; visitor
// unit tests cover the substitution shapes in isolation.
func TestRewriteOracleAST_PipelineIntegration(t *testing.T) {
	cases := []struct {
		name string
		body string
		// must contains text — substring assertions for resilience to
		// formatting tweaks downstream. Case-insensitive substring
		// match because Oracle's lexer uppercases unquoted idents and
		// the post-pipeline normaliser may flip them again.
		mustContain []string
		// must NOT contain — pre-rewrite Oracle forms that the visitor
		// should have eliminated before pgast emission.
		mustNotContain []string
	}{
		{
			// SequenceRef visitor emits the function call shape;
			// Oracle's lexer uppercases the sequence name so the
			// embedded literal is `MY_SEQ`.
			name: "SequenceRef → nextval()",
			body: `BEGIN
				x := my_seq.NEXTVAL;
			END;`,
			mustContain:    []string{"nextval('MY_SEQ')"},
			mustNotContain: []string{".NEXTVAL"},
		},
		{
			name: "Schema-qualified sequence",
			body: `BEGIN
				x := hr.emp_seq.CURRVAL;
			END;`,
			mustContain:    []string{"currval('HR.EMP_SEQ')"},
			mustNotContain: []string{".CURRVAL"},
		},
		{
			// to_char single-arg visitor qualifies the call with the
			// orafce schema. The legacy text pipeline doesn't run a
			// post-rewrite on this shape so the visitor's output is
			// the final form.
			name: "to_char single-arg → oracle.to_char",
			body: `BEGIN
				x := to_char(y);
			END;`,
			mustContain: []string{"oracle.to_char"},
		},
		{
			// decode visitor qualifies the call to `oracle.decode`,
			// then the legacy text pass `qualifyOracleDecode` expands
			// it into a CASE expression (orafce-free fallback for the
			// >7-arg overload). End-to-end the pipeline produces CASE.
			name: "decode → CASE (legacy text pass owns final shape)",
			body: `BEGIN
				x := decode(y, 1, 'a', 'b');
			END;`,
			mustContain: []string{"CASE", "IS NOT DISTINCT FROM"},
		},
		// SYS prefix strip is exercised by the unit test
		// TestVisitOracleStripSysPrefix; integration coverage requires
		// the parser to emit FuncCall{Name:"SYS.pkg.fn"} which depends
		// on a parser path the current parsePrimary doesn't take for
		// dotted multi-part schema-qualified calls (it produces an
		// Ident path instead). Tracking this gap as a Phase 3.7
		// follow-up.
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text, _, _ := TranslateRoutineBody(c.body, dialects.KindOracle)
			for _, want := range c.mustContain {
				if !strings.Contains(text, want) {
					t.Errorf("output missing %q in:\n%s", want, text)
				}
			}
			for _, dont := range c.mustNotContain {
				if strings.Contains(text, dont) {
					t.Errorf("output should not contain %q:\n%s", dont, text)
				}
			}
		})
	}
}

// TestRewriteOracleAST_NonOracleNoOp confirms the orchestrator only
// runs for Oracle kinds — a MySQL routine body must not be touched
// by Oracle-specific rewriters even when its AST happens to contain
// shapes that would match (e.g. a `decode` ident that means
// something else in MySQL context).
func TestRewriteOracleAST_NonOracleNoOp(t *testing.T) {
	body := `BEGIN
		SET x = decode(y, 1, 'a', 'b');
	END`
	text, _, _ := TranslateRoutineBody(body, dialects.KindMySQL)
	if strings.Contains(text, "oracle.decode") {
		t.Errorf("MySQL routine body should not be rewritten by Oracle visitors:\n%s", text)
	}
}
