package translate

import (
	"strings"
	"testing"

	"gitlab.com/dalibo/squishy/internal/dialects"
)

// Oracle's multi-column UPDATE SET tuple form:
//
//	UPDATE ACCOUNTS
//	   SET (PARENT_ACCOUNT_ID, ACCOUNT_TYPE, ...) =
//	       (SELECT ACCOUNT_ID, ACCOUNT_TYPE, ... FROM ACCOUNTS WHERE ...)
//	 WHERE ACCOUNT_ID = V_ACCOUNT_ID;
//
// PG accepts the same syntax. Before this fix the Oracle parser
// dropped each column after the first into a CALL stmt and emitted
// `SET "" = (PARENT_ACCOUNT_ID), "ACCOUNT_TYPE" =;` — a hard syntax
// error.
func TestUpdateTupleSet(t *testing.T) {
	src := `BEGIN
  UPDATE accounts
     SET (parent_account_id, account_type, branch_code) =
         (SELECT account_id, account_type, branch_code FROM accounts WHERE account_id = v_parent_id)
   WHERE account_id = v_account_id;
END;`
	got, _, _ := TranslateRoutineBody(src, dialects.KindOracle)
	if !strings.Contains(strings.ToUpper(got), `("PARENT_ACCOUNT_ID", "ACCOUNT_TYPE", "BRANCH_CODE")`) {
		t.Fatalf("tuple SET column list not preserved:\n%s", got)
	}
	if !strings.Contains(got, "(SELECT") {
		t.Fatalf("tuple SET subquery not preserved:\n%s", got)
	}
	// Sanity: the broken pre-fix output had "" or stray CALL — guard:
	if strings.Contains(got, `SET ""`) {
		t.Fatalf("zero-length identifier in SET — pre-fix bug:\n%s", got)
	}
	if strings.Contains(got, "CALL \"BRANCH_CODE\"") {
		t.Fatalf("column dropped to a CALL stmt — pre-fix bug:\n%s", got)
	}
}
