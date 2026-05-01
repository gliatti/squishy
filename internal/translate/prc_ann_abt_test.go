package translate

import (
	"strings"
	"testing"

	"gitlab.com/dalibo/squishy/internal/dialects"
)

// Mirrors PRC_ANN_ABT's exception handler shape — bare lowercase
// dbms_output / dbms_application_info calls captured as raw text in
// the EXCEPTION block.
func TestPRC_ANN_ABT_ExceptionHandlerRewritten(t *testing.T) {
	// Oracle body shape captured from DRSE.PRC_ANN_ABT (everything between
	// IS … END PRC_ANN_ABT) — full thing with cursor + EXCEPTION block.
	src := `
CURSOR C_ABT IS
	SELECT	A1.NUM_CTA_ABT
	  FROM	ABT A1
   	 WHERE 	A1.COD_ETA_CTA = '9';

NB_ABT 		INTEGER;

BEGIN
	dbms_output.enable(1000000);
	NB_ABT	    := 0;

	FOR C_ABT_REC IN C_ABT LOOP
		if PKG_UTI_GAB.F_ANNUL_ABT(C_ABT_REC.NUM_CTA_ABT,'BATCH')=0 then
			NB_ABT := NB_ABT + 1;
		end if;
	END LOOP;

COMMIT;

EXCEPTION
	when NO_DATA_FOUND then
		null;

	when OTHERS then
		rollback;
		dbms_output.put_line('Erreur :'||SQLERRM);
		dbms_application_info.set_action ('ERR :'||SQLERRM);

END PRC_ANN_ABT;`
	got, _, _ := TranslateRoutineBody(src, dialects.KindOracle)
	if strings.Contains(got, "dbms_output.put_line") {
		t.Fatalf("dbms_output.put_line not rewritten in full body:\n%s", got)
	}
	if strings.Contains(got, "dbms_application_info.set_action") {
		t.Fatalf("dbms_application_info.set_action not rewritten:\n%s", got)
	}
	if !strings.Contains(got, "RAISE NOTICE") {
		t.Fatalf("expected RAISE NOTICE in body:\n%s", got)
	}
	t.Logf("rewritten body:\n%s", got)
}

// Regression for the apostrophe-in-comment bug: a French comment containing
// `d'un` was latching findKeywordCI's `inStr` to true for the rest of the
// body, silently disabling every downstream keyword-driven rewrite (SYSDATE,
// dbms_output.*, dbms_application_info.*). The actual PRC_ANN_ABT body has
// such a comment between the second and third `EXISTS` clauses of its cursor;
// after the comment, both a SYSDATE and the entire EXCEPTION block went
// untouched in production.
func TestApostropheInCommentDoesNotBreakRewrites(t *testing.T) {
	src := `BEGIN
  -- abonnés résiliés d'un traité terminé passent en annulés
  IF X <= ADD_MONTHS(SYSDATE, -3) THEN NULL; END IF;
EXCEPTION
  WHEN OTHERS THEN
    dbms_output.put_line('boom');
END;`
	got, _, _ := TranslateRoutineBody(src, dialects.KindOracle)
	if strings.Contains(got, "SYSDATE") {
		t.Fatalf("SYSDATE not rewritten — likely the comment-apostrophe bug:\n%s", got)
	}
	if strings.Contains(got, "dbms_output.put_line") {
		t.Fatalf("dbms_output.put_line not rewritten:\n%s", got)
	}
}
