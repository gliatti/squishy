package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects"
	"gitlab.com/dalibo/squishy/internal/dialects/mysql"
	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// The MySQL parser fills the typed Events list (one event per trigger);
// the translator never splits the display string.
func TestCreateTriggerEventsTyped_MySQL(t *testing.T) {
	mstmts, merrs := mysql.Parse("CREATE TRIGGER trg BEFORE DELETE ON emp FOR EACH ROW BEGIN\n  SET @x = 1;\nEND;")
	require.Empty(t, merrs)
	var mtr *ast.CreateTrigger
	for _, s := range mstmts {
		if tr, ok := s.(*ast.CreateTrigger); ok {
			mtr = tr
		}
	}
	require.NotNil(t, mtr, "no CREATE TRIGGER in %+v", mstmts)
	require.Equal(t, []string{"DELETE"}, mtr.Events)
}

// A trigger without a typed Events list (hand-made AST, or a dialect
// parser that does not fill it) falls back to its single Event keyword.
func TestTriggerDMLEvents_FallbackToEvent(t *testing.T) {
	require.Equal(t, []string{"DELETE"}, triggerDMLEvents(&ast.CreateTrigger{Event: "DELETE"}))
	require.Equal(t, []string{"UPDATE"}, triggerDMLEvents(&ast.CreateTrigger{Event: "DELETE", Events: []string{"UPDATE"}}))
	require.Nil(t, triggerDMLEvents(&ast.CreateTrigger{}))
	require.Equal(t, "RETURN NEW;", triggerReturnStmt(mysqlTriggerReturnRow(nil)))
}

// The early bare `RETURN;` of a trigger body is replaced on the parsed
// AST (makeTriggerBareReturnVisitor), before the PG writer renders the
// body: nested bare RETURNs (IF branch, exception handler) all become
// the trigger's return statement, a RETURN carrying an expression is
// left alone, and each substitution is a fresh node.
func TestTriggerBareReturnVisitor_AST(t *testing.T) {
	stmts := []ast.PLStmt{&ast.Block{
		Stmts: []ast.PLStmt{
			&ast.IfStmt{Branches: []ast.IfBranch{{
				Cond: &ast.Ident{Parts: []string{"done"}},
				Body: []ast.PLStmt{&ast.ReturnStmt{}},
			}}},
		},
		Except: &ast.ExceptionBlock{Handlers: []ast.ExceptionHandler{{
			Names: []string{"OTHERS"},
			Body:  []ast.PLStmt{&ast.ReturnStmt{}},
		}}},
	}}
	stmts = applyPLRewriter(stmts, makeTriggerBareReturnVisitor("OLD"))

	var bare int
	var rets []*ast.ReturnStmt
	for _, s := range stmts {
		ast.Rewrite(s, func(n ast.Node) ast.Node {
			if r, ok := n.(*ast.ReturnStmt); ok {
				if r.Expr == nil {
					bare++
				} else {
					rets = append(rets, r)
				}
			}
			return n
		})
	}
	require.Zero(t, bare, "no bare RETURN may reach the writer")
	require.Len(t, rets, 2, "both bare RETURNs become the trigger return")
	require.NotSame(t, rets[0], rets[1])
	for _, r := range rets {
		id, ok := r.Expr.(*ast.Ident)
		require.True(t, ok)
		require.Equal(t, []string{"OLD"}, id.Parts)
	}

	// Non-bare RETURN: untouched.
	withExpr := &ast.ReturnStmt{Expr: &ast.Ident{Parts: []string{"NEW"}}}
	require.Same(t, withExpr, makeTriggerBareReturnVisitor("OLD")(withExpr))
}

// The event-only return rule (a DELETE trigger returns OLD, whatever its
// timing) applies to MySQL / MariaDB sources only. Oracle and DB2 keep
// the historical rule unchanged — OLD only for AFTER + DELETE, NEW
// otherwise — until their own change (known gap: an Oracle / DB2 BEFORE
// DELETE row trigger returns NEW, NULL on DELETE, so PG skips the row).
func TestTriggerReturnRow_GatedByDialect(t *testing.T) {
	del := &ast.CreateTrigger{Event: "DELETE", Events: []string{"DELETE"}}
	ins := &ast.CreateTrigger{Event: "INSERT", Events: []string{"INSERT"}}
	for _, k := range []dialects.Kind{dialects.KindMySQL, dialects.KindMariaDB} {
		tr := &translator{opt: Options{SourceKind: k}}
		require.Equal(t, "OLD", tr.triggerReturnRow(del, "BEFORE"), k)
		require.Equal(t, "OLD", tr.triggerReturnRow(del, "AFTER"), k)
		require.Equal(t, "NEW", tr.triggerReturnRow(ins, "BEFORE"), k)
		require.Equal(t, "NEW", tr.triggerReturnRow(ins, "AFTER"), k)
	}
	for _, k := range []dialects.Kind{dialects.KindOracle, dialects.KindOracle19, dialects.KindDB2, dialects.KindDB2zOS} {
		tr := &translator{opt: Options{SourceKind: k}}
		require.Equal(t, "NEW", tr.triggerReturnRow(del, "BEFORE"), k)
		require.Equal(t, "OLD", tr.triggerReturnRow(del, "AFTER"), k)
		require.Equal(t, "NEW", tr.triggerReturnRow(ins, "BEFORE"), k)
		require.Equal(t, "NEW", tr.triggerReturnRow(ins, "AFTER"), k)
		require.Equal(t, "NEW", tr.triggerReturnRow(&ast.CreateTrigger{Event: "INSERT OR DELETE"}, "AFTER"), k)
	}
}

// oracleTriggerRoutineDDL returns the DDL of the plan routine whose
// name is name.
func oracleTriggerRoutineDDL(t *testing.T, res *Result, name string) string {
	t.Helper()
	for _, r := range res.Plan.Routines {
		if r.Name == name {
			return r.DDL
		}
	}
	t.Fatalf("no routine %s in %+v", name, res.Plan.Routines)
	return ""
}

// Oracle compound trigger on DELETE, auto-split into one PG trigger per
// section: the closing RETURN and the bare early-exit RETURN of each
// section follow the historical rule (BEFORE EACH ROW → NEW, AFTER EACH
// ROW → OLD), exactly as before the MySQL-only event rule. The bare
// RETURN is substituted on the parsed AST.
func TestOracleCompoundTriggerDeleteReturn_Unchanged(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."ORDERS" ("ID" NUMBER NOT NULL);
		CREATE OR REPLACE TRIGGER "MIG"."TRG_DEL"
		  FOR DELETE ON "MIG"."ORDERS"
		COMPOUND TRIGGER
		  BEFORE EACH ROW IS BEGIN IF :OLD.ID IS NULL THEN RETURN; END IF; NULL; END BEFORE EACH ROW;
		  AFTER EACH ROW IS BEGIN IF :OLD.ID IS NULL THEN RETURN; END IF; NULL; END AFTER EACH ROW;
		END "TRG_DEL";
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: dialects.KindOracle})
	before := oracleTriggerRoutineDDL(t, res, "trg_del_before_row")
	after := oracleTriggerRoutineDDL(t, res, "trg_del_after_row")
	t.Logf("before:\n%s\nafter:\n%s", before, after)
	require.Contains(t, before, "RETURN NEW;")
	require.NotContains(t, before, "RETURN OLD;")
	require.Contains(t, after, "RETURN OLD;")
	require.NotContains(t, after, "RETURN NEW;")
	for _, ddl := range []string{before, after} {
		require.NotContains(t, strings.ReplaceAll(ddl, " ", ""), "RETURN;")
	}
}

// Plain Oracle row triggers keep the historical rule too: BEFORE DELETE
// returns NEW (bare RETURN included), AFTER DELETE returns OLD.
func TestOracleDeleteTriggerReturn_Unchanged(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."ORDERS" ("ID" NUMBER NOT NULL);
		CREATE OR REPLACE TRIGGER "MIG"."TRG_BD"
		  BEFORE DELETE ON "MIG"."ORDERS" FOR EACH ROW
		BEGIN
		  IF :OLD.ID IS NULL THEN RETURN; END IF;
		  NULL;
		END;
		/
		CREATE OR REPLACE TRIGGER "MIG"."TRG_AD"
		  AFTER DELETE ON "MIG"."ORDERS" FOR EACH ROW
		BEGIN
		  NULL;
		END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: dialects.KindOracle})
	var bd, ad string
	for _, r := range res.Plan.Routines {
		switch {
		case strings.EqualFold(r.Name, "TRG_BD"):
			bd = r.DDL
		case strings.EqualFold(r.Name, "TRG_AD"):
			ad = r.DDL
		}
	}
	require.NotEmpty(t, bd, "%+v", res.Plan.Routines)
	require.NotEmpty(t, ad, "%+v", res.Plan.Routines)
	t.Logf("bd:\n%s\nad:\n%s", bd, ad)
	require.Contains(t, bd, "RETURN NEW;")
	require.NotContains(t, bd, "RETURN OLD;")
	require.NotContains(t, strings.ReplaceAll(bd, " ", ""), "RETURN;")
	require.Contains(t, ad, "RETURN OLD;")
	require.NotContains(t, ad, "RETURN NEW;")
}

// MySQL / MariaDB: a BEFORE DELETE trigger returns OLD (NEW is NULL on
// DELETE and a BEFORE ROW trigger returning NULL skips the row).
func TestMySQLBeforeDeleteTriggerReturnsOld(t *testing.T) {
	for _, k := range []dialects.Kind{dialects.KindMySQL, dialects.KindMariaDB} {
		stmts, errs := mysql.Parse("CREATE TABLE `emp` (`id` int NOT NULL, PRIMARY KEY (`id`));\n" +
			"CREATE TRIGGER `trg_bd` BEFORE DELETE ON `emp` FOR EACH ROW BEGIN\n  SET @x = OLD.id;\nEND;")
		require.Empty(t, errs)
		res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: k})
		ddl := triggerFunctionDDL(t, res, "trg_bd")
		require.Contains(t, ddl, "RETURN OLD;", k)
		require.NotContains(t, ddl, "RETURN NEW;", k)
	}
}
