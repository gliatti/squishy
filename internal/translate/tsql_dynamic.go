package translate

// tsql_dynamic.go — dynamic SQL whose statement is a constant.
//
// `EXEC sp_executesql @stmt, N'@p type [OUTPUT], …', args…` and
// `EXEC (N'…')` run a T-SQL string. When that string is known at
// translation time — a string literal, or a variable declared with a
// string-literal initialiser and never assigned afterwards — it is T-SQL
// source like the rest of the body: it is parsed with the T-SQL parser
// (and the @params declaration list with ParseParamDecls), its
// parameters are bound to the call's arguments, and the statements
// replace the EXEC, so the routine translator handles them as static
// statements (visitors, variables, SELECT … INTO, @@ROWCOUNT, …). A
// string built at run time keeps the EXECUTE path and its warning.
//
// Only bodies whose meaning does not change when they run in the
// caller's scope are inlined: data statements (SELECT, SELECT @v = …,
// INSERT, UPDATE, DELETE, MERGE), SET @param = …, IF and BEGIN … END
// over them; no DECLARE, cursor, temporary table, transaction control,
// RETURN or nested EXEC (a dynamic batch has its own scope for those).
// Every variable the string reads must be one of its declared
// parameters (a dynamic batch does not see the caller's variables), each
// parameter is bound to exactly one argument — a literal, or a variable
// of the caller — and only OUTPUT parameters, bound to an OUTPUT
// variable argument, may be assigned. Each parameter reference then
// becomes its argument: an input parameter is a value (never assigned,
// so passing by value and substituting agree), converted to the
// parameter's declared type like sp_executesql binds it (dynArg); an
// OUTPUT one is the caller's variable, which sp_executesql copies back
// at the end; a caller variable bound to an OUTPUT parameter and to
// another one is not inlined (copy-in / copy-out would differ).
//
// Nothing here inspects SQL text: the strings go through the parser,
// the rest is AST rewriting.

import (
	"strings"
	"unicode/utf8"

	tsqldialect "gitlab.com/dalibo/squishy/internal/dialects/tsql"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// inlineConstantDynamicSQL replaces, in the source statements of the
// body, every EXEC of a constant statement that can be inlined by the
// statements it runs (in a BEGIN … END block).
func (b *tsqlBody) inlineConstantDynamicSQL(stmts []ast.PLStmt) []ast.PLStmt {
	b.dynVarTypes = b.tsqlDeclaredVarTypes(stmts)
	b.inlineDynamicSeq(stmts, tsqlWrittenVars(stmts), map[string]string{})
	return stmts
}

// inlineDynamicSeq inlines the constant dynamic SQL of a statement
// sequence. consts holds the constant variables declared so far by the
// sequence (and the BEGIN … END groups it runs straight through, which
// T-SQL does not scope).
func (b *tsqlBody) inlineDynamicSeq(stmts []ast.PLStmt, written map[string]bool, consts map[string]string) {
	for i, s := range stmts {
		if blk, ok := s.(*ast.Block); ok && !blk.TryCatch {
			b.inlineDynamicSeq(blk.Stmts, written, consts)
			continue
		}
		if ds, ok := s.(*ast.DeclareStmt); ok {
			// A top-level DECLARE runs before every later statement:
			// its initialiser is the value they see.
			for _, d := range ds.Decls {
				dv, ok := d.(*ast.DeclareVar)
				if !ok {
					continue
				}
				name := tsqlVarName(dv.Name)
				if lit, ok := tsqlUnparen(dv.Default).(*ast.Literal); ok && lit.Kind == "string" && !written[name] {
					if _, dup := consts[name]; !dup {
						consts[name] = lit.Text
						continue
					}
				}
				delete(consts, name)
				written[name] = true
			}
			continue
		}
		known := make(map[string]string, len(consts))
		for k, v := range consts {
			known[k] = v
		}
		out := ast.Rewrite(s, func(n ast.Node) ast.Node {
			x, ok := n.(*ast.ExecuteImmediateStmt)
			if !ok {
				return n
			}
			if dyn := b.inlineDynamic(x, known); dyn != nil {
				return dyn
			}
			return n
		})
		if ps, ok := out.(ast.PLStmt); ok {
			stmts[i] = ps
		}
	}
}

// tsqlWrittenVars returns the variables (folded names) a statement of
// the body assigns after their declaration: SET, SELECT @v = …, FETCH /
// EXECUTE … INTO, UPDATE … SET @v = …, OUTPUT arguments.
func tsqlWrittenVars(stmts []ast.PLStmt) map[string]bool {
	w := map[string]bool{}
	walkAll(stmts, func(n ast.Node) {
		for _, v := range tsqlAssignedVars(n) {
			w[tsqlVarName(v)] = true
		}
	})
	return w
}

// tsqlAssignedVars returns the variables (source spelling) node n
// assigns.
func tsqlAssignedVars(n ast.Node) []string {
	var out []string
	switch x := n.(type) {
	case *ast.AssignStmt:
		out = append(out, x.Target)
	case *ast.SelectInto:
		out = append(out, x.Vars...)
	case *ast.FetchStmt:
		out = append(out, x.Into...)
	case *ast.ExecuteImmediateStmt:
		out = append(out, x.Into...)
	case *ast.UpdateStmt:
		for _, a := range x.Sets {
			if len(a.Col) > 0 && a.Col[0] == '@' {
				out = append(out, a.Col)
			}
		}
	case *ast.NamedArg:
		if id, ok := x.Value.(*ast.Ident); ok && x.Output && id.Variable && len(id.Parts) == 1 {
			out = append(out, id.Parts[0])
		}
	}
	return out
}

// walkAll calls fn on every node of the statements.
func walkAll(stmts []ast.PLStmt, fn func(ast.Node)) {
	v := tsqlVisitFunc(func(n ast.Node) bool {
		fn(n)
		return true
	})
	for _, s := range stmts {
		ast.Walk(v, s)
	}
}

// tsqlConstString returns the text of a constant statement / parameter
// list expression: a string literal, or a variable of consts.
func tsqlConstString(e ast.Expr, consts map[string]string) (string, bool) {
	switch x := tsqlUnparen(e).(type) {
	case *ast.Literal:
		if x.Kind == "string" {
			return x.Text, true
		}
	case *ast.Ident:
		if x.Variable && len(x.Parts) == 1 && !isTSQLSystemVar(x.Parts[0]) {
			s, ok := consts[tsqlVarName(x.Parts[0])]
			return s, ok
		}
	}
	return "", false
}

// tsqlDynBinding is the argument a parameter of a dynamic statement is
// bound to.
type tsqlDynBinding struct {
	arg    ast.Expr // *ast.Literal or variable *ast.Ident (source form)
	output bool
	// typ is the declared type of the parameter: sp_executesql converts
	// an input argument to it when binding (char(2) truncates 'ABC').
	typ ast.DataType
}

// inlineDynamic returns the BEGIN … END block of the statements a
// constant dynamic SQL call runs, its parameters replaced by their
// arguments, or nil when the call is not inlined (see the file comment).
func (b *tsqlBody) inlineDynamic(x *ast.ExecuteImmediateStmt, consts map[string]string) ast.PLStmt {
	if len(x.Into) > 0 {
		return nil
	}
	text, ok := tsqlConstString(x.SQL, consts)
	if !ok {
		return nil
	}
	var params []ast.Param
	if x.SPExecuteSQL && x.ParamDecl != nil {
		decl, ok := tsqlConstString(x.ParamDecl, consts)
		if !ok {
			return nil
		}
		ps, errs := tsqldialect.ParseParamDecls(decl)
		if len(errs) > 0 {
			return nil
		}
		params = ps
	}
	if !x.SPExecuteSQL && len(x.Using) > 0 {
		return nil
	}
	bind, ok := tsqlBindDynamicArgs(params, x.Using)
	if !ok {
		return nil
	}
	dyn, errs := tsqldialect.ParseRoutineBody(text)
	if len(errs) > 0 || len(dyn) == 0 || !tsqlInlinableDynamic(dyn) {
		return nil
	}
	// Every variable read is a parameter; only OUTPUT ones are written.
	okVars := true
	walkAll(dyn, func(n ast.Node) {
		if id, isID := n.(*ast.Ident); isID && id.Variable && len(id.Parts) == 1 && !isTSQLSystemVar(id.Parts[0]) {
			if _, bound := bind[tsqlVarName(id.Parts[0])]; !bound {
				okVars = false
			}
		}
		for _, v := range tsqlAssignedVars(n) {
			if bd, bound := bind[tsqlVarName(v)]; !bound || !bd.output {
				okVars = false
			}
		}
	})
	if !okVars {
		return nil
	}
	for i, s := range dyn {
		out := ast.Rewrite(s, func(n ast.Node) ast.Node {
			id, isID := n.(*ast.Ident)
			if !isID || !id.Variable || len(id.Parts) != 1 {
				return n
			}
			bd, bound := bind[tsqlVarName(id.Parts[0])]
			if !bound {
				return n
			}
			return b.dynArg(bd, id.P)
		})
		if ps, ok := out.(ast.PLStmt); ok {
			dyn[i] = ps
		}
	}
	// The variables a statement assigns by name: an OUTPUT parameter is
	// the caller's variable.
	rename := func(v string) string { return bind[tsqlVarName(v)].arg.(*ast.Ident).Parts[0] }
	walkAll(dyn, func(n ast.Node) {
		switch y := n.(type) {
		case *ast.AssignStmt:
			y.Target = rename(y.Target)
		case *ast.SelectInto:
			for i := range y.Vars {
				y.Vars[i] = rename(y.Vars[i])
			}
		case *ast.UpdateStmt:
			for i := range y.Sets {
				if len(y.Sets[i].Col) > 0 && y.Sets[i].Col[0] == '@' {
					y.Sets[i].Col = rename(y.Sets[i].Col)
				}
			}
		}
	})
	what := "EXEC (…)"
	if x.SPExecuteSQL {
		what = "sp_executesql"
	}
	b.note(what + " with a constant statement is translated as static SQL: the string is parsed and translated like the rest of the body, its parameters replaced by the call's arguments (unqualified names resolve in the routine's schema, where SQL Server resolves a dynamic batch in the caller's default schema)")
	return &ast.Block{Stmts: dyn, P: x.P}
}

// tsqlBindDynamicArgs binds the declared parameters of a dynamic
// statement to the call's arguments (positional first, then
// `@p = value`), each bound exactly once to a literal or a variable,
// an OUTPUT parameter to an OUTPUT variable argument.
func tsqlBindDynamicArgs(params []ast.Param, args []ast.Expr) (map[string]tsqlDynBinding, bool) {
	bind := map[string]tsqlDynBinding{}
	byName := map[string]ast.Param{}
	for _, p := range params {
		name := tsqlVarName(p.Name)
		if _, dup := byName[name]; dup {
			return nil, false
		}
		byName[name] = p
	}
	pos := 0
	for _, a := range args {
		var name string
		value, output := a, false
		if na, ok := a.(*ast.NamedArg); ok {
			name, value, output = na.Name, na.Value, na.Output
		}
		if name == "" {
			if pos >= len(params) {
				return nil, false
			}
			name = params[pos].Name
			pos++
		}
		key := tsqlVarName(name)
		p, declared := byName[key]
		if !declared {
			return nil, false
		}
		if _, twice := bind[key]; twice {
			return nil, false
		}
		switch v := tsqlUnparen(value).(type) {
		case *ast.Literal:
			if p.Output || output {
				return nil, false
			}
			bind[key] = tsqlDynBinding{arg: v, typ: p.Type}
		case *ast.Ident:
			if !v.Variable || len(v.Parts) != 1 || isTSQLSystemVar(v.Parts[0]) || p.Output != output {
				return nil, false
			}
			bind[key] = tsqlDynBinding{arg: v, output: output, typ: p.Type}
		default:
			return nil, false
		}
	}
	if len(bind) != len(params) {
		return nil, false
	}
	// sp_executesql copies the arguments in and the OUTPUT parameters
	// out: a caller variable bound to an OUTPUT parameter and to another
	// parameter is read with its old value by the latter, which a
	// by-reference substitution would not reproduce.
	uses := map[string]int{}
	outs := map[string]bool{}
	for _, bd := range bind {
		if id, ok := bd.arg.(*ast.Ident); ok {
			name := tsqlVarName(id.Parts[0])
			uses[name]++
			outs[name] = outs[name] || bd.output
		}
	}
	for name, n := range uses {
		if n > 1 && outs[name] {
			return nil, false
		}
	}
	return bind, true
}

// dynArg is the expression replacing a reference to an input parameter
// of an inlined dynamic statement: its argument, converted to the
// parameter's declared type as sp_executesql does when it binds it
// (`N'@c char(2)'` bound to 'ABC' reads 'AB'; `N'@p int'` bound to a
// varchar variable compares as an integer). The conversion is left out
// when it cannot change the value: a variable of the same type, a NULL,
// an integer literal for an integer parameter, a string literal that
// fits a varchar parameter.
func (b *tsqlBody) dynArg(bd tsqlDynBinding, p ast.Position) ast.Expr {
	arg := tsqlCopyLeaf(bd.arg, p)
	if bd.output || bd.typ == nil {
		return arg
	}
	switch x := bd.arg.(type) {
	case *ast.Ident:
		if t := b.dynVarTypes[tsqlVarName(x.Parts[0])]; t != "" && t == b.mapType(bd.typ) {
			return arg
		}
	case *ast.Literal:
		switch x.Kind {
		case "null":
			return arg
		case "number":
			if _, isInt := bd.typ.(*ast.IntType); isInt && tsqlIsIntegerToken(x.Text) {
				return arg
			}
		case "string":
			if ch, ok := bd.typ.(*ast.CharType); ok && !strings.EqualFold(ch.Name, "CHAR") && !strings.EqualFold(ch.Name, "NCHAR") &&
				(ch.Max || ch.HasLength && utf8.RuneCountInString(x.Text) <= ch.Length) {
				return arg
			}
		}
	}
	return &ast.CastExpr{Expr: arg, Type: bd.typ, P: p}
}

// tsqlDeclaredVarTypes returns the PG type of every variable the body
// declares and of the routine's parameters (folded names), for dynArg.
func (b *tsqlBody) tsqlDeclaredVarTypes(stmts []ast.PLStmt) map[string]string {
	out := map[string]string{}
	for _, p := range b.ctx.Params {
		out[tsqlVarName(p.SourceName)] = p.Type
	}
	walkAll(stmts, func(n ast.Node) {
		if dv, ok := n.(*ast.DeclareVar); ok && dv.Type != nil {
			out[tsqlVarName(dv.Name)] = b.mapType(dv.Type)
		}
	})
	return out
}

// tsqlInlinableDynamic reports whether every statement of a dynamic
// batch keeps its meaning in the caller's scope.
func tsqlInlinableDynamic(stmts []ast.PLStmt) bool {
	for _, s := range stmts {
		switch x := s.(type) {
		case *ast.SelectStmt:
			if x.IntoTable != nil {
				return false
			}
		case *ast.SelectInto, *ast.InsertStmt, *ast.UpdateStmt, *ast.DeleteStmt, *ast.MergeStmt, *ast.AssignStmt:
		case *ast.IfStmt:
			for _, br := range x.Branches {
				if !tsqlInlinableDynamic(br.Body) {
					return false
				}
			}
			if !tsqlInlinableDynamic(x.Else) {
				return false
			}
		case *ast.Block:
			if x.TryCatch || !tsqlInlinableDynamic(x.Stmts) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// tsqlCopyLeaf returns a fresh copy of a bound argument (a literal or a
// variable reference) at position p: each parameter reference gets its
// own node, so the in-place rewrites that follow never see one twice.
func tsqlCopyLeaf(e ast.Expr, p ast.Position) ast.Expr {
	switch x := e.(type) {
	case *ast.Literal:
		cp := *x
		cp.P = p
		return &cp
	case *ast.Ident:
		cp := *x
		cp.Parts = append([]string(nil), x.Parts...)
		cp.P = p
		return &cp
	}
	return e
}
