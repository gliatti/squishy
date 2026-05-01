package translate

import (
	"strings"
	"testing"
)

// Verifies that the EXCEPTION-handler body shape that landed in the
// PRC_ANN_ABT DDL — bare lowercase `dbms_output.put_line(...)` and
// `dbms_application_info.set_action(...)` calls without any leading CALL/
// PERFORM — gets rewritten into PG-valid `RAISE NOTICE` lines.
func TestRewriteDbmsOutput_RawLowercaseStmt(t *testing.T) {
	in := `BEGIN
  dbms_output.put_line('Erreur :'||SQLERRM);
  dbms_application_info.set_action ('ERR :'||SQLERRM);
END;`
	got := rewriteDbmsOutput(in)
	got = rewriteDbmsApplicationInfo(got)
	if strings.Contains(got, "dbms_output.put_line") {
		t.Fatalf("dbms_output.put_line not rewritten:\n%s", got)
	}
	if strings.Contains(got, "dbms_application_info.set_action") {
		t.Fatalf("dbms_application_info.set_action not rewritten:\n%s", got)
	}
	if !strings.Contains(got, "RAISE NOTICE") {
		t.Fatalf("expected RAISE NOTICE, got:\n%s", got)
	}
}
