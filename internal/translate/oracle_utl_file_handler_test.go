package translate

import (
	"strings"
	"testing"
)

// Regression for a real-world failure: an apostrophe in a
// French line comment (`-- d'une ligne`) was latching the local string
// tracker in prependPerformOnStatementCalls to true for the rest of the
// body, so the WHEN ... THEN utl_file.put_line(...) calls in the nested
// EXCEPTION blocks downstream never got their PERFORM prefix and PG
// rejected the routine with `syntax error at or near "utl_file"`.
func TestPrependPerform_ApostropheInCommentDoesNotBreakPerform(t *testing.T) {
	in := `BEGIN
  -- lors d'une ligne rejetée, on journalise
  IF x THEN NULL; END IF;
EXCEPTION
  WHEN NO_DATA_FOUND THEN utl_file.put_line(fo,'no row 1');
END;`
	got := prependPerformOnStatementCalls(in, "utl_file")
	if !strings.Contains(got, "PERFORM utl_file.put_line(fo,'no row 1')") {
		t.Fatalf("apostrophe-in-comment latched inStr — PERFORM not prepended:\n%s", got)
	}
}

// A file-export procedure has `WHEN NO_DATA_FOUND THEN utl_file.put_line(...);` —
// the call sits inline after THEN and must get a PERFORM prefix to compile
// in PG plpgsql. prependPerformOnStatementCalls' boundary detector must
// recognise THEN as the opening of a statement list.
func TestPrependPerform_AfterThenInExceptionHandler(t *testing.T) {
	in := `BEGIN
  NULL;
EXCEPTION
  WHEN NO_DATA_FOUND THEN utl_file.put_line(fo,'oops');
    null;
  WHEN OTHERS THEN utl_file.put_line(fo,'boom');
END;`
	got := prependPerformOnStatementCalls(in, "utl_file")
	if !strings.Contains(got, "PERFORM utl_file.put_line(fo,'oops')") {
		t.Fatalf("expected PERFORM prefix on inline THEN call, got:\n%s", got)
	}
	if !strings.Contains(got, "PERFORM utl_file.put_line(fo,'boom')") {
		t.Fatalf("expected PERFORM prefix on second THEN call, got:\n%s", got)
	}
}
