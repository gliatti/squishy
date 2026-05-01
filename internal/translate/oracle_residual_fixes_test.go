package translate

import (
	"strings"
	"testing"

	"gitlab.com/dalibo/squishy/internal/dialects"
)

// PRC_RCO_CTRL — `FOR rec IN (SELECT … (+) …) LOOP UPDATE T SET col=val
// WHERE …;` had two issues that compounded:
// 1. The CursorForStmt writer dropped the parens around the inline SELECT.
// 2. Without parens, the (+) outer-join rewriter walked past the SELECT
//    looking for a clause terminator and found the UPDATE's WHERE keyword
//    — terminating the SELECT WHERE there.
// Result: the SELECT lost its WHERE, and the UPDATE acquired two WHERE
// clauses. PG raised `syntax error at or near "WHERE"`.
func TestCursorForWithOuterJoinKeepsWhere(t *testing.T) {
	src := `BEGIN
  FOR c_rec IN (
    SELECT a.x, a.y
    FROM t1 a, t2 b
    WHERE a.id = b.id (+)
      AND a.flag = 1
  ) LOOP
    UPDATE other SET val = c_rec.x WHERE id = c_rec.y;
  END LOOP;
END;`
	got, _, _ := TranslateRoutineBody(src, dialects.KindOracle)
	// The SELECT's WHERE filter must be inside the FOR loop's parens.
	if strings.Count(got, "WHERE") < 2 {
		t.Fatalf("expected SELECT WHERE + UPDATE WHERE, got:\n%s", got)
	}
	// Specifically, no UPDATE statement should have two consecutive
	// WHERE clauses.
	if strings.Contains(got, "WHERE a.flag = 1\nWHERE") || strings.Contains(got, "WHERE A.FLAG = 1\nWHERE") {
		t.Fatalf("UPDATE has stacked WHEREs (pre-fix bug):\n%s", got)
	}
	if !strings.Contains(got, "LEFT JOIN") {
		t.Fatalf("(+) outer-join not converted to LEFT JOIN:\n%s", got)
	}
}

// BIR#CAR_ART_CLC$ID_CAR_ART_CLC — Oracle dba_source wraps lines and
// inserts a `--` line comment in the middle of a SELECT projection. When
// joined back to a single line the comment swallows the rest of the
// query (including INTO/FROM). PG sees `SELECT expr;` followed by stray
// `END IF`, raising `syntax error at or near "IF"`. Fix: strip `--`
// comments from captured selectList and raw clauses.
func TestSelectIntoWithDashCommentInProjection(t *testing.T) {
	src := `BEGIN
  IF x IS NULL THEN
    SELECT seq_foo.NEXTVAL --   select lpad(seq_foo.nextval,9,'0')
      INTO :new.id FROM DUAL;
  END IF;
END;`
	got, _, _ := TranslateRoutineBody(src, dialects.KindOracle)
	if !strings.Contains(strings.ToLower(got), "into new.id") &&
		!strings.Contains(strings.ToLower(got), `into "new"."id"`) {
		t.Fatalf("INTO target not preserved (comment swallowed it):\n%s", got)
	}
}

// PRC_TLR — `FETCH cur BULK COLLECT INTO rec.field1, rec.field2 LIMIT n`
// had two issues: dotted INTO targets stopped at the first `.`, and the
// trailing LIMIT clause leaked as phantom CallStmts. Fix: support dotted
// targets in BULK COLLECT INTO and consume LIMIT.
func TestFetchBulkCollectIntoLimit(t *testing.T) {
	src := `DECLARE
  CURSOR c IS SELECT 1, 2 FROM dual;
  TYPE arr IS TABLE OF NUMBER INDEX BY BINARY_INTEGER;
  TYPE rec IS RECORD (a arr, b arr);
  r rec;
BEGIN
  OPEN c;
  FETCH c BULK COLLECT INTO r.a, r.b LIMIT 100;
  CLOSE c;
END;`
	got, _, _ := TranslateRoutineBody(src, dialects.KindOracle)
	if strings.Contains(got, `CALL "LIMIT"`) || strings.Contains(got, `CALL "100"`) {
		t.Fatalf("LIMIT clause leaked as phantom CALL:\n%s", got)
	}
}

// BIR#ECR_CLI — nested anonymous block `if … then DECLARE x TYPE; BEGIN
// SELECT … INTO x; END; end if;` had the DECLARE/variables fall through
// to parseAssignOrCall, producing phantom `CALL "X"()` statements. The
// SELECT body then referenced an undeclared `x` and PG raised
// `"x" is not a known variable`. Fix: handle DECLARE as a block opener
// in parsePLStmt's dispatcher.
func TestNestedDeclareBlockInIf(t *testing.T) {
	src := `BEGIN
  IF :new.flag = '1' THEN
    DECLARE
      p_x cpt_cli.num_cta_abt%TYPE;
    BEGIN
      SELECT c.num_cta_abt INTO p_x FROM cpt_cli c WHERE c.id = :new.id;
    END;
  END IF;
END;`
	got, _, _ := TranslateRoutineBody(src, dialects.KindOracle)
	if strings.Contains(got, `CALL "P_X"()`) {
		t.Fatalf("DECLARE variable parsed as phantom CALL:\n%s", got)
	}
	if !strings.Contains(strings.ToLower(got), "declare") {
		t.Fatalf("DECLARE block not preserved:\n%s", got)
	}
}
