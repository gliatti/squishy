package translate

import (
	"strings"

	pgast "gitlab.com/dalibo/squishy/internal/dialects/postgres"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// tsql_call_args.go — INOUT parameters of a CALL inside a translated
// routine.
//
// A T-SQL OUTPUT parameter is an INOUT parameter on PostgreSQL, and
// PL/pgSQL's CALL must pass every INOUT parameter a writable argument (a
// variable), even one the caller does not care about:
//
//   - `EXEC dbo.uspLogError;` omits @ErrorLogID (OUTPUT, DEFAULT 0): PG
//     refuses `CALL dbo.usplogerror()` ("argument is not writable");
//   - `EXEC p @x = 5` passes a constant to an OUTPUT parameter without
//     the OUTPUT keyword (T-SQL then discards the value written back);
//   - a procedure returning result sets has trailing INOUT refcursor
//     parameters (translateTSQLProcedure) its T-SQL callers know nothing
//     about.
//
// Each such parameter gets a local of the parameter's type: a value
// argument is assigned to it before the CALL, an omitted one passes it
// NULL — or the parameter's default, which T-SQL would have used. The
// callee is looked up among the procedures of the migrated script;
// calls to other procedures are left as they are.

// tsqlCalleeParam is one parameter of a procedure of the script as
// emitted on PG.
type tsqlCalleeParam struct {
	src     string // source name (with its @) — "" for a result cursor
	name    string // PG name
	inout   bool
	typ     ast.DataType
	pgType  string // set for result cursors (refcursor)
	deflt   ast.Expr
	isParam bool
}

// tsqlProcParams returns the PG parameters of the script's procedure a
// call names (srcSchema "" resolves like SQL Server: the caller's schema,
// then dbo), nil when the script does not define it.
func (t *translator) tsqlProcParams(srcSchema, name, callerSchema string) []tsqlCalleeParam {
	st := t.tsql
	if st == nil {
		return nil
	}
	var def *ast.CreateProcedure
	cands := []string{srcSchema}
	if srcSchema == "" {
		cands = []string{callerSchema, "dbo"}
	}
	for _, s := range cands {
		if d := st.procDefs[sourceObjectKey(s, name)]; d != nil {
			def = d
			break
		}
	}
	if def == nil {
		return nil
	}
	var out []tsqlCalleeParam
	for _, p := range def.Params {
		out = append(out, tsqlCalleeParam{
			src: p.Name, name: tsqlParamName(p.Name), typ: p.Type, deflt: p.Default, isParam: true,
			inout: p.Output || strings.EqualFold(p.Direction, "INOUT") || strings.EqualFold(p.Direction, "OUT"),
		})
	}
	if t.tsqlBodyReturnsResultSet(def.Body) {
		if n, ok := t.tsqlResultSetCount(def.Body); ok && n > 0 {
			params := make([]RoutineParam, len(out))
			for i, p := range out {
				params[i] = RoutineParam{Name: p.name}
			}
			for _, c := range tsqlResultCursorNames(n, params) {
				out = append(out, tsqlCalleeParam{name: c, inout: true, pgType: "refcursor"})
			}
		}
	}
	return out
}

// callArgs renders the arguments of x for PG; pre holds the assignments
// that must run before the CALL.
func (b *tsqlBody) callArgs(x *ast.CallStmt) (args []string, pre []pgast.PLStmt) {
	var callee []tsqlCalleeParam
	if b.ctx.Run != nil && b.ctx.Run.t != nil {
		callee = b.ctx.Run.t.tsqlProcParams(x.Schema, x.Name, b.ctx.SourceSchema)
	}
	byName := map[string]int{}
	for i, p := range callee {
		if p.isParam {
			byName[tsqlVarName(p.src)] = i
		}
	}
	covered := make([]bool, len(callee))
	// writable reports whether an argument is a variable reference (PG
	// can assign an INOUT result back to it).
	writable := func(e ast.Expr) bool {
		id, ok := e.(*ast.Ident)
		return ok && id.Variable && len(id.Parts) == 1 && !isTSQLSystemVar(id.Parts[0])
	}
	// value renders an argument for parameter i, through a local when
	// the parameter is INOUT and the argument is not an OUTPUT variable:
	// a constant cannot be assigned, and T-SQL does not write an OUTPUT
	// parameter back to a variable passed without OUTPUT.
	value := func(i int, e ast.Expr, output bool) string {
		if i < 0 || i >= len(callee) || !callee[i].inout || (output && writable(e)) {
			return b.expr(e)
		}
		tmp := b.calleeLocal(x, callee[i])
		pre = append(pre, &pgast.PLAssign{Target: tmp.target(), Expr: b.expr(e)})
		return tmp.target()
	}
	pos := 0
	named := false
	for i, a := range x.Args {
		if na, ok := a.(*ast.NamedArg); ok {
			if tsqlIsDefault(na.Value) {
				if na.Name == "" {
					b.warn("EXEC " + x.Name + ": a positional DEFAULT argument has no PostgreSQL counterpart")
				}
				continue
			}
			if na.Name != "" {
				named = true
				idx := -1
				if j, ok := byName[tsqlVarName(na.Name)]; ok {
					idx = j
					covered[j] = true
				}
				args = append(args, quoteIdent(tsqlVarName(na.Name))+" => "+value(idx, na.Value, na.Output))
				continue
			}
			if pos < len(covered) {
				covered[pos] = true
			}
			args = append(args, value(pos, na.Value, na.Output))
			pos++
			continue
		}
		if tsqlIsDefault(a) {
			if !tsqlAllDefault(x.Args[i:]) {
				b.warn("EXEC " + x.Name + ": a positional DEFAULT argument before other arguments has no PostgreSQL counterpart")
			}
			break
		}
		if pos < len(covered) {
			covered[pos] = true
		}
		args = append(args, value(pos, a, false))
		pos++
	}
	// Omitted INOUT parameters: a local holding their default (NULL
	// for the result cursors), passed by name.
	for i, p := range callee {
		if covered[i] || !p.inout {
			continue
		}
		tmp := b.calleeLocal(x, p)
		if p.deflt != nil && !tsqlIsDefault(p.deflt) {
			pre = append(pre, &pgast.PLAssign{Target: tmp.target(), Expr: b.ddlExpr(p.deflt, tmp.pgType)})
		}
		if !named && i == pos && p.isParam {
			// Still positional: keep the call positional.
			args = append(args, tmp.target())
			pos++
			continue
		}
		args = append(args, quoteIdent(p.name)+" => "+tmp.target())
		named = true
	}
	return args, pre
}

// calleeLocal declares the local passed to an INOUT parameter of a
// called procedure.
func (b *tsqlBody) calleeLocal(x *ast.CallStmt, p tsqlCalleeParam) *tsqlVar {
	typ := p.pgType
	if typ == "" {
		typ = b.mapType(p.typ)
	}
	return b.addLocal("_"+normalizeTSQLIdent(x.Name)+"_"+p.name, typ, "")
}
