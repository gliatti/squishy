package translate

import (
	"strings"
	"testing"
)

// TRANSLATE / sys_context / strip_sys / dbms_sql.parse / decode /
// to_char / dbms_utility.format_call_stack / trunc / dictionary_views
// / collection_tokens / nextval / listagg / nested_aggregates / rownum
// / hash_in_lit / mod / userenv — formerly tested via direct calls to
// the legacy text passes. Phase 5 retired those passes; the AST
// visitor unit tests in oracle_visitors_funcs_test.go and
// oracle_visitors_select_test.go cover the same patterns
// structurally. End-to-end coverage flows through
// oracle_translate_test.go's TestRewriteOracleAST_PipelineIntegration.

func TestRewriteOraclePlusOuterJoins_Simple(t *testing.T) {
	in := `SELECT i.product_id, d.translated_name
FROM   product_information i, product_descriptions d
WHERE  d.product_id (+) = i.product_id
  AND  d.language_id (+) = current_setting('squishy.lang', true)`
	got := rewriteOraclePlusOuterJoins(in)
	if strings.Contains(got, "(+)") {
		t.Fatalf("residual (+) marker:\n%s", got)
	}
	if !strings.Contains(got, "LEFT JOIN product_descriptions d") {
		t.Fatalf("missing LEFT JOIN:\n%s", got)
	}
	if !strings.Contains(got, "ON d.product_id = i.product_id") {
		t.Fatalf("missing ON clause:\n%s", got)
	}
	if strings.Contains(got, "WHERE") {
		t.Fatalf("WHERE should be empty after promotion:\n%s", got)
	}
}

func TestRewriteOraclePlusOuterJoins_KeepsInnerCondition(t *testing.T) {
	in := `SELECT *
FROM a x, b y
WHERE y.id (+) = x.id
  AND x.flag = 'Y'`
	got := rewriteOraclePlusOuterJoins(in)
	if !strings.Contains(got, "LEFT JOIN b y") {
		t.Fatalf("missing LEFT JOIN:\n%s", got)
	}
	if !strings.Contains(got, "WHERE x.flag = 'Y'") {
		t.Fatalf("inner condition should remain in WHERE:\n%s", got)
	}
}

func TestRewriteOraclePlusOuterJoins_FullOuter(t *testing.T) {
	in := `SELECT *
FROM a x, b y
WHERE x.id (+) = y.id (+)`
	got := rewriteOraclePlusOuterJoins(in)
	if strings.Contains(got, "(+)") {
		t.Fatalf("residual (+) marker:\n%s", got)
	}
	if !strings.Contains(got, "FULL JOIN b y") {
		t.Fatalf("missing FULL JOIN (b is the later FROM table):\n%s", got)
	}
	if strings.Contains(got, "LEFT JOIN") {
		t.Fatalf("FULL pair should not produce LEFT JOIN:\n%s", got)
	}
	if !strings.Contains(got, "ON x.id = y.id") {
		t.Fatalf("missing ON clause:\n%s", got)
	}
}

func TestRewriteOraclePlusOuterJoins_FullOuterMultiCondition(t *testing.T) {
	in := `SELECT *
FROM a x, b y
WHERE x.id (+) = y.id (+)
  AND x.kind (+) = y.kind (+)`
	got := rewriteOraclePlusOuterJoins(in)
	if strings.Count(got, "FULL JOIN") != 1 {
		t.Fatalf("expected exactly one FULL JOIN (conditions grouped):\n%s", got)
	}
	if !strings.Contains(got, "x.id = y.id") || !strings.Contains(got, "x.kind = y.kind") {
		t.Fatalf("both conditions should be in the FULL JOIN ON:\n%s", got)
	}
}

func TestRewriteOraclePlusOuterJoins_LeftAndFullMixed(t *testing.T) {
	// y full-joins to x, z left-joins to x — verify both join kinds coexist.
	in := `SELECT *
FROM a x, b y, c z
WHERE x.id (+) = y.id (+)
  AND z.id (+) = x.id`
	got := rewriteOraclePlusOuterJoins(in)
	if !strings.Contains(got, "FULL JOIN b y") {
		t.Fatalf("missing FULL JOIN b y:\n%s", got)
	}
	if !strings.Contains(got, "LEFT JOIN c z") {
		t.Fatalf("missing LEFT JOIN c z:\n%s", got)
	}
}

func TestRewriteOraclePlusOuterJoins_NoOpWithoutPlus(t *testing.T) {
	in := `SELECT * FROM a x JOIN b y ON x.id = y.id WHERE x.flag = 'Y'`
	got := rewriteOraclePlusOuterJoins(in)
	if got != in {
		t.Fatalf("should be unchanged when no (+):\n got: %q\nwant: %q", got, in)
	}
}
