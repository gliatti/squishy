package translate

import (
	"strings"

	pgast "gitlab.com/dalibo/squishy/internal/dialects/postgres"
	tsqldialect "gitlab.com/dalibo/squishy/internal/dialects/tsql"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// tsql_computed_trigger.go — computed columns that call a scalar
// function of the migrated script.
//
// A PostgreSQL generated column may only call IMMUTABLE functions, and a
// translated T-SQL function is a PL/pgSQL function created after the
// tables. AdventureWorks' Sales.Customer.AccountNumber is
//
//	AS (ISNULL('AW' + dbo.ufnLeadingZeros(CustomerID), ''))
//
// with ufnLeadingZeros a WITH SCHEMABINDING function computing its
// result from its argument only. When every such call targets a
// deterministic function of the script (tsqlDeterministicFunc: schema
// bound, reads no table, calls nothing time- or session-dependent), the
// column's value depends only on the row: computing it when the row is
// written (a BEFORE INSERT OR UPDATE trigger, `NEW.col := expr`) gives
// the value SQL Server computes on read. The column is a plain column,
// copied from the source like a refused generated column, and the
// trigger is created after the copy (Plan.PostActions). It is an
// emulation PostgreSQL hosts exactly: an info explanation, no blocking
// prerequisite. A value the application writes into the column is
// overwritten (SQL Server rejects it).

// tsqlComputedTrigger is a computed column emulated by a trigger.
type tsqlComputedTrigger struct {
	table int    // Plan.Tables index
	col   string // PG column name
	expr  ast.Expr
	src   string // source expression (explanations)
}

// tsqlDeterministicFunc reports whether the scalar function of the
// script a call names (`dbo.ufnLeadingZeros`) computes its result from
// its arguments only.
func (t *translator) tsqlDeterministicFunc(name string) bool {
	return t.tsqlDeterministicFuncKey(tsqlFuncKey(name), map[string]bool{})
}

func (t *translator) tsqlDeterministicFuncKey(key string, visiting map[string]bool) bool {
	st := t.tsql
	if st == nil || visiting[key] {
		return false
	}
	if v, ok := st.deterministic[key]; ok {
		return v
	}
	visiting[key] = true
	fn := st.funcDefs[key]
	ok := fn != nil && fn.Returns != nil && fn.ReturnsTableVar == "" && !fn.ReturnsInlineTable && tsqlHasOption(fn.Options, "SCHEMABINDING")
	if ok {
		body, errs := tsqldialect.ParseRoutineBody(fn.Body)
		ok = len(errs) == 0 && len(body) > 0
		for _, s := range body {
			if !ok {
				break
			}
			ast.Walk(tsqlVisitFunc(func(n ast.Node) bool {
				if !ok {
					return false
				}
				switch x := n.(type) {
				case *ast.FromTable, *ast.FromFunc, *ast.InsertStmt, *ast.UpdateStmt, *ast.DeleteStmt,
					*ast.MergeStmt, *ast.CallStmt, *ast.ExecuteImmediateStmt, *ast.OpenStmt, *ast.FetchStmt,
					*ast.SequenceRef, *ast.SubqueryExpr, *ast.ExistsExpr:
					ok = false
				case *ast.Ident:
					if x.Variable && len(x.Parts) == 1 && isTSQLSystemVar(x.Parts[0]) {
						ok = false
					}
				case *ast.FuncCall:
					up := strings.ToUpper(x.Name)
					switch {
					case tsqlNonDeterministicFuncs[up]:
						ok = false
					case st.funcDefs[tsqlFuncKey(x.Name)] != nil:
						ok = t.tsqlDeterministicFuncKey(tsqlFuncKey(x.Name), visiting)
					}
				}
				return ok
			}), s)
		}
	}
	delete(visiting, key)
	st.deterministic[key] = ok
	return ok
}

// tsqlNonDeterministicFuncs are the T-SQL built-ins whose result is not
// a function of their arguments.
var tsqlNonDeterministicFuncs = map[string]bool{
	"GETDATE": true, "GETUTCDATE": true, "SYSDATETIME": true, "SYSUTCDATETIME": true,
	"SYSDATETIMEOFFSET": true, "CURRENT_TIMESTAMP": true, "NEWID": true, "NEWSEQUENTIALID": true,
	"RAND": true, "CRYPT_GEN_RANDOM": true, "SUSER_SNAME": true, "SUSER_NAME": true, "USER_NAME": true,
	"CURRENT_USER": true, "SESSION_USER": true, "SYSTEM_USER": true, "HOST_NAME": true, "APP_NAME": true,
	"DB_NAME": true, "DB_ID": true, "OBJECT_ID": true, "OBJECT_NAME": true, "@@SPID": true,
	"SESSION_CONTEXT": true, "CONTEXT_INFO": true, "IDENT_CURRENT": true, "SCOPE_IDENTITY": true,
}

// tsqlHasOption reports whether a routine's WITH options include opt
// (one option keyword, compared case-insensitively).
func tsqlHasOption(opts []string, opt string) bool {
	for _, o := range opts {
		if strings.EqualFold(strings.TrimSpace(o), opt) {
			return true
		}
	}
	return false
}

// emitTSQLComputedTriggers creates the BEFORE INSERT OR UPDATE triggers
// computing the columns tsqlComputedColumn left to them.
func (t *translator) emitTSQLComputedTriggers() {
	st := t.tsql
	if len(st.computedTriggers) == 0 {
		return
	}
	taken := t.sysverTakenNames()
	for _, ct := range st.computedTriggers {
		tbl := &t.res.Plan.Tables[ct.table]
		trg := taken.triggersOn(tbl.Name)
		fn := claimIdent(tbl.Name, "_"+ct.col+"_compute_fn", taken.functions)
		name := claimIdent(tbl.Name, "_"+ct.col+"_compute", trg)
		expr := tsqlNewRowRefs(ct.expr)
		body := &pgast.PLBlock{Body: []pgast.PLStmt{
			&pgast.PLAssign{Target: "NEW." + pgQuote(ct.col), Expr: pgast.WriteExpr(expr)},
			&pgast.PLReturn{Expr: "NEW"},
		}}
		t.res.Plan.PostActions = append(t.res.Plan.PostActions, pgast.Write([]pgast.Stmt{
			&pgast.CreateFunction{Schema: tbl.Schema, Name: fn, Returns: "trigger", Body: pgast.WritePLpgSQL(body)},
			&pgast.CreateTrigger{
				Name: name, Timing: "BEFORE", Events: []string{"INSERT", "UPDATE"},
				Schema: tbl.Schema, Table: tbl.Name, FnName: fn, ForEach: "ROW",
			},
		}))
		obj := tbl.Schema + "." + tbl.Name + "." + ct.col
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: "table." + obj,
			Source: "AS (" + ct.src + ")",
			Target: "plain column + BEFORE INSERT OR UPDATE trigger " + name + " (NEW." + ct.col + " := " + pgast.WriteExpr(expr) + ")",
			Reason: "computed column calling a deterministic scalar function of the script (WITH SCHEMABINDING, reading no table): a PostgreSQL generated column cannot call it (only IMMUTABLE functions, created before the table), so the value is computed by a trigger when the row is written — the value SQL Server computes on read, since it depends on the row only. The copied rows keep their source values; a value written into the column is replaced by the computed one.",
			Level:  "info",
		})
	}
}

// tsqlNewRowRefs qualifies the column references of a computed column's
// PG expression with NEW (the row of the trigger computing it). e is
// copied, never modified.
func tsqlNewRowRefs(e ast.Expr) ast.Expr {
	out, ok := ast.Rewrite(tsqlCloneExprShallow(e), func(n ast.Node) ast.Node {
		id, isID := n.(*ast.Ident)
		if !isID || id.Variable || len(id.Parts) != 1 {
			return n
		}
		return &ast.Ident{Parts: []string{"NEW", id.Parts[0]}, P: id.P}
	}).(ast.Expr)
	if !ok || out == nil {
		return e
	}
	return out
}

// tsqlCloneExprShallow returns a copy of e that Rewrite may modify in
// place without touching e (expressions of computed columns are small
// trees of the node kinds tsqlComputedProof accepts).
func tsqlCloneExprShallow(e ast.Expr) ast.Expr {
	switch x := e.(type) {
	case *ast.ParenExpr:
		return &ast.ParenExpr{Inner: tsqlCloneExprShallow(x.Inner), P: x.P}
	case *ast.BinaryExpr:
		cp := *x
		cp.Lhs, cp.Rhs = tsqlCloneExprShallow(x.Lhs), tsqlCloneExprShallow(x.Rhs)
		return &cp
	case *ast.UnaryExpr:
		cp := *x
		cp.Rhs = tsqlCloneExprShallow(x.Rhs)
		return &cp
	case *ast.FuncCall:
		cp := *x
		cp.Args = make([]ast.Expr, len(x.Args))
		for i, a := range x.Args {
			cp.Args[i] = tsqlCloneExprShallow(a)
		}
		return &cp
	case *ast.CastExpr:
		cp := *x
		cp.Expr = tsqlCloneExprShallow(x.Expr)
		return &cp
	case *ast.CaseExpr:
		cp := *x
		cp.Operand = tsqlCloneExprShallow(x.Operand)
		cp.Whens = make([]ast.CaseExprWhen, len(x.Whens))
		for i, w := range x.Whens {
			cp.Whens[i] = ast.CaseExprWhen{Match: tsqlCloneExprShallow(w.Match), Then: tsqlCloneExprShallow(w.Then)}
		}
		cp.Else = tsqlCloneExprShallow(x.Else)
		return &cp
	case *ast.Ident:
		cp := *x
		cp.Parts = append([]string(nil), x.Parts...)
		return &cp
	case *ast.Literal:
		cp := *x
		return &cp
	}
	return e
}
