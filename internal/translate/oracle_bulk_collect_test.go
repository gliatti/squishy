package translate

import (
	"strings"
	"testing"

	"gitlab.com/dalibo/squishy/internal/dialects"
)

// `SELECT col BULK COLLECT INTO arr FROM t` is Oracle's row-set-into-array
// form. The PG equivalent is `SELECT array_agg(col) INTO arr FROM t`.
// squishy's parser strips `BULK COLLECT` and the writer wraps each
// projection in array_agg(...) so the bulk semantics survive translation.
func TestSelectBulkCollectIntoSingleColumn(t *testing.T) {
	src := `BEGIN
  SELECT id BULK COLLECT INTO l_ids FROM t WHERE flag = 1;
END;`
	got, _, _ := TranslateRoutineBody(src, dialects.KindOracle)
	if !strings.Contains(strings.ToLower(got), "array_agg(id)") {
		t.Fatalf("expected array_agg(id) wrapping the projection:\n%s", got)
	}
	if strings.Contains(strings.ToUpper(got), "BULK COLLECT") {
		t.Fatalf("BULK COLLECT keywords not stripped:\n%s", got)
	}
}

// Multi-column BULK COLLECT INTO must wrap each projection independently,
// preserving column aliases.
func TestSelectBulkCollectIntoMultiColumn(t *testing.T) {
	src := `BEGIN
  SELECT id, name AS n, count(*) c BULK COLLECT INTO l_ids, l_names, l_cnts FROM t;
END;`
	got, _, _ := TranslateRoutineBody(src, dialects.KindOracle)
	gotL := strings.ToLower(got)
	if !strings.Contains(gotL, "array_agg(id)") {
		t.Fatalf("array_agg(id) missing:\n%s", got)
	}
	if !strings.Contains(gotL, "array_agg(name)") {
		t.Fatalf("array_agg(name) (with stripped AS alias) missing:\n%s", got)
	}
	if !strings.Contains(gotL, "array_agg(count(*))") {
		t.Fatalf("array_agg(count(*)) missing — paren-aware split broken:\n%s", got)
	}
}

// FETCH cursor BULK COLLECT INTO with dotted record-field targets and
// optional LIMIT — the parser must accept dotted idents and consume LIMIT
// without phantom CallStmt leakage.
func TestFetchBulkCollectIntoDottedTargetsWithLimit(t *testing.T) {
	src := `DECLARE
  CURSOR c IS SELECT 1 FROM dual;
BEGIN
  OPEN c;
  FETCH c BULK COLLECT INTO rec.field_a, rec.field_b LIMIT 100;
END;`
	got, _, _ := TranslateRoutineBody(src, dialects.KindOracle)
	if strings.Contains(got, `CALL "LIMIT"`) || strings.Contains(strings.ToUpper(got), `CALL "REC"`) {
		t.Fatalf("LIMIT/dotted target leaked as phantom CALL:\n%s", got)
	}
}
