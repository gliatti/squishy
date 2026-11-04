package translate

import (
	"strings"
	"testing"

	"gitlab.com/dalibo/squishy/internal/dialects"
)

// A procedure body with a cursor FOR loop and an EXCEPTION block whose
// handlers are bare lowercase dbms_output / dbms_application_info calls
// captured as raw text.
func TestOracleExceptionBlock_HandlerRewritten(t *testing.T) {
	// Body of a synthetic close_accounts (everything between
	// IS … END close_accounts): cursor + EXCEPTION block.
	src := `
CURSOR c_accounts IS
	SELECT	a.account_id
	  FROM	accounts a
   	 WHERE 	a.status = 'C';

nb_closed 		INTEGER;

BEGIN
	dbms_output.enable(1000000);
	nb_closed	    := 0;

	FOR c_accounts_rec IN c_accounts LOOP
		if app_util.fn_close_account(c_accounts_rec.account_id,'BATCH')=0 then
			nb_closed := nb_closed + 1;
		end if;
	END LOOP;

COMMIT;

EXCEPTION
	when NO_DATA_FOUND then
		null;

	when OTHERS then
		rollback;
		dbms_output.put_line('Error :'||SQLERRM);
		dbms_application_info.set_action ('ERR :'||SQLERRM);

END close_accounts;`
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
// dbms_output.*, dbms_application_info.*). After such a comment, both a
// SYSDATE and the entire EXCEPTION block went untouched.
func TestApostropheInCommentDoesNotBreakRewrites(t *testing.T) {
	src := `BEGIN
  -- lignes d'un lot terminé
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
