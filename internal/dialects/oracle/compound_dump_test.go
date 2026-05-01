package oracle

import (
	"fmt"
	"testing"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// Inspect what the Oracle parser stores in CreateTrigger.Body for a
// COMPOUND TRIGGER as DBMS_METADATA emits it. Used to validate the
// compound-splitter assumptions in translator.go.
func TestDumpCompoundTriggerBody(t *testing.T) {
	src := `CREATE OR REPLACE EDITIONABLE TRIGGER "SQUISHY"."TRG_COMPOUND"
FOR INSERT OR UPDATE ON orders
COMPOUND TRIGGER
  BEFORE EACH ROW IS
  BEGIN
    IF :NEW.total < 0 THEN
      :NEW.total := 0;
    END IF;
  END BEFORE EACH ROW;
END trg_compound;
/`
	stmts, errs := Parse(src)
	if len(errs) > 0 {
		t.Logf("parse errors: %v", errs)
	}
	if len(stmts) != 1 {
		t.Fatalf("expected 1 stmt, got %d", len(stmts))
	}
	ct, ok := stmts[0].(*ast.CreateTrigger)
	if !ok {
		t.Fatalf("expected *ast.CreateTrigger, got %T", stmts[0])
	}
	fmt.Printf("=== Time:  %q\n", ct.Time)
	fmt.Printf("=== Event: %q\n", ct.Event)
	fmt.Printf("=== Table: schema=%q name=%q\n", ct.Table.Schema, ct.Table.Name)
	fmt.Printf("=== Body length: %d\n", len(ct.Body))
	fmt.Printf("=== Body verbatim:\n>>>%s<<<\n", ct.Body)
}
