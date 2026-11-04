package translate

import (
	"strings"

	pgwriter "gitlab.com/dalibo/squishy/internal/dialects/postgres"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// tsql_methods.go — T-SQL CLR / xml data-type method calls (ast.MethodCall)
// rewritten to PostgreSQL.
//
// hierarchyid. The type is migrated as text holding the canonical path
// ('/1/3/', what ToString() prints, see types_map_tsql.go), so its
// methods become the tsql.hierarchyid_* shim functions over that text
// (tsql_shim/70_hierarchyid.sql): GetLevel, GetAncestor,
// IsDescendantOf, GetReparentedValue, GetDescendant; ToString() is the
// value itself, hierarchyid::GetRoot() the root path '/' and
// hierarchyid::Parse(s) the canonical text s.
//
// xml. The XQuery argument was parsed by the T-SQL parser
// (MethodCall.XQuery); the rewrite works on that tree and the PG writer
// prints it as XPath 1.0 (pgwriter.WriteXPath1):
//
//	x.value(q, 'T')  → (SELECT "v" FROM XMLTABLE(ns, '/' PASSING (x)
//	                    COLUMNS "v" T PATH 'q') AS "_x")
//	x.exist(q)       → (SELECT "v" FROM XMLTABLE(ns, '/' PASSING (x)
//	                    COLUMNS "v" boolean PATH 'boolean(q)') AS "_x")
//	x.query(q)       → (SELECT xmlagg("v") FROM XMLTABLE(ns, 'q' PASSING
//	                    (x) COLUMNS "v" xml PATH '.') AS "_x")
//	x.nodes(q) AS T(c) (FROM / APPLY) → XMLTABLE(ns, 'q' PASSING (x)
//	                    COLUMNS "c" xml PATH '.') AS "t"
//
// value() evaluates q as the column expression over the one document row
// '/': PG returns the string value of the node it selects converted to T
// (NULL when it selects nothing, '' for string() of nothing — exactly
// what SQL Server returns), and a NULL x gives no row, hence NULL.
// Three differences with XQuery are handled on the tree:
//
//   - PG's XMLNAMESPACES has no default namespace: a `declare default
//     element namespace` becomes a generated prefix put on every
//     unprefixed element name test.
//   - A nodes() row is a node inside its document in SQL Server, but a
//     separate document holding a copy of it on PG (the xml column of
//     XMLTABLE): a path relative to the row (`Edu.Level`, `@LocationID`,
//     `.`) is anchored on that document's element (`/*/…`). An absolute
//     path read from a row is only the same when the row is its
//     document's element (`nodes('/Resume')`); otherwise it is reported.
//   - XPath 1.0 functions only: data() at the top of a value() query is
//     its argument's string value (string()), other XQuery functions are
//     reported.

// tsqlXMLRef describes the xml column of a nodes() row source (`T(c)`).
type tsqlXMLRef struct {
	// root: the row is the element of its source document (nodes() with
	// a one-step absolute path on a document), so absolute paths read
	// from it select the same nodes on PG.
	root bool
}

// xmlRefKey is the key of a nodes() row column: folded alias "." folded
// column.
func xmlRefKey(alias, col string) string {
	return normalizeTSQLIdent(alias) + "." + normalizeTSQLIdent(col)
}

// xmlRefOf returns the nodes() row column an (already folded or source)
// identifier names.
func (rw *tsqlRW) xmlRefOf(e ast.Expr) (tsqlXMLRef, bool) {
	id, ok := e.(*ast.Ident)
	if !ok || id.Variable || len(id.Parts) != 2 || rw.xmlRefs == nil {
		return tsqlXMLRef{}, false
	}
	r, ok := rw.xmlRefs[xmlRefKey(id.Parts[0], id.Parts[1])]
	return r, ok
}

// collectXMLRef records the row column of a nodes() source (prepare).
func (rw *tsqlRW) collectXMLRef(f *ast.FromFunc) {
	mc, ok := f.Func.(*ast.MethodCall)
	if !ok || !strings.EqualFold(mc.Method, "nodes") || f.Alias == "" || len(f.Cols) != 1 || rw.xmlRefs == nil {
		return
	}
	_, onRef := rw.xmlRefOf(mc.Target)
	root := mc.XQuery != nil && !onRef && xqIsDocumentElementPath(mc.XQuery.Body)
	rw.xmlRefs[xmlRefKey(f.Alias, f.Cols[0])] = tsqlXMLRef{root: root}
}

// xqIsDocumentElementPath reports whether e is `/name` (one child step
// from the document node, predicates allowed): it selects the document
// element.
func xqIsDocumentElementPath(e ast.XQExpr) bool {
	p, ok := e.(*ast.XQPath)
	if !ok || !p.Root || len(p.Steps) != 1 {
		return false
	}
	st := p.Steps[0]
	return !st.Desc && st.Test != nil && st.Test.Kind == "name" && (st.Axis == "" || st.Axis == "child")
}

// tsqlHierarchyMethods maps a hierarchyid instance method (lower case)
// to its shim function and argument count.
var tsqlHierarchyMethods = map[string]struct {
	fn    string
	nargs int
}{
	"getlevel":           {"tsql.hierarchyid_getlevel", 0},
	"getancestor":        {"tsql.hierarchyid_getancestor", 1},
	"isdescendantof":     {"tsql.hierarchyid_isdescendantof", 1},
	"getreparentedvalue": {"tsql.hierarchyid_getreparentedvalue", 2},
	"getdescendant":      {"tsql.hierarchyid_getdescendant", 2},
}

// methodCall rewrites a CLR / xml method call; an untranslated one is
// left on the tree for the residual report.
func (rw *tsqlRW) methodCall(x *ast.MethodCall) ast.Node {
	m := strings.ToLower(x.Method)
	if x.StaticType != "" {
		if strings.EqualFold(x.StaticType, "hierarchyid") {
			switch {
			case m == "getroot" && len(x.Args) == 0:
				return &ast.Literal{Kind: "string", Text: "/", P: x.P}
			case m == "parse" && len(x.Args) == 1:
				return &ast.CastExpr{Expr: x.Args[0], Type: &ast.PGType{Name: "text"}, P: x.P}
			}
		}
		return x
	}
	if h, ok := tsqlHierarchyMethods[m]; ok && len(x.Args) == h.nargs && x.Target != nil {
		args := append([]ast.Expr{x.Target}, x.Args...)
		return rw.fc(h.fn, x.P, args...)
	}
	switch m {
	case "tostring":
		if len(x.Args) != 0 || x.Target == nil {
			return x
		}
		switch pgTypeName(rw.exprType(x.Target)) {
		case "geography", "geometry":
			return rw.fc("ST_AsText", x.P, x.Target)
		}
		// hierarchyid: the text path is what ToString() prints.
		return &ast.CastExpr{Expr: x.Target, Type: &ast.PGType{Name: "text"}, P: x.P}
	case "value", "exist", "query":
		return rw.xmlScalarMethod(x, m)
	}
	return x
}

// exprType returns the PG type text of a column / variable reference
// ("" when unknown or not a reference).
func (rw *tsqlRW) exprType(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return rw.columnType(id)
	}
	return ""
}

// pgTypeName is the lower-cased base name of a PG type text.
func pgTypeName(typ string) string {
	for i := 0; i < len(typ); i++ {
		if typ[i] == '(' {
			typ = typ[:i]
			break
		}
	}
	return strings.ToLower(strings.TrimSpace(typ))
}

// tsqlMethodResidue is the residual label of a method call left on the
// tree.
func tsqlMethodResidue(x *ast.MethodCall) string {
	if x.StaticType == "" && (x.XQuery != nil || x.XQueryErr != "") {
		why := x.XQueryErr
		if why == "" {
			why = "not translated"
		}
		return xmlMethodLabel(x, why)
	}
	target := x.StaticType
	if target == "" {
		target = "column"
	}
	return "CLR method " + target + "." + x.Method + "() (spatial / CLR type method without a PostgreSQL translation)"
}

// xmlMethodLabel is the residual label of an xml method left untranslated.
func xmlMethodLabel(x *ast.MethodCall, why string) string {
	return "XML data-type method " + x.Method + "() (" + why + ")"
}

// xqPrepared is an XQuery brought to PG's XPath 1.0 for one method call.
type xqPrepared struct {
	ns   []ast.XQNamespace
	body ast.XQExpr
}

// prepareXQuery applies the default-namespace prefix and the nodes()-row
// anchoring to the parsed query of x; why is "" on success.
func (rw *tsqlRW) prepareXQuery(x *ast.MethodCall) (xqPrepared, string) {
	if x.XQuery == nil {
		why := x.XQueryErr
		if why == "" {
			why = "XQuery not parsed"
		}
		return xqPrepared{}, why
	}
	q := x.XQuery
	ns := append([]ast.XQNamespace(nil), q.Namespaces...)
	body := xqClone(q.Body)
	if q.DefaultElementNS != "" {
		prefix := xqFreePrefix(ns)
		ns = append(ns, ast.XQNamespace{Prefix: prefix, URI: q.DefaultElementNS})
		xqPrefixElements(body, prefix)
	}
	if why := xqCheckXPath1(body); why != "" {
		return xqPrepared{}, why
	}
	body = xqDropAtomicSingletons(body)
	if ref, onRef := rw.xmlRefOf(x.Target); onRef {
		var why string
		body, why = xqAnchorOnRow(body, ref.root)
		if why != "" {
			return xqPrepared{}, why
		}
	}
	return xqPrepared{ns: ns, body: body}, ""
}

// xmlScalarMethod rewrites value() / exist() / query().
func (rw *tsqlRW) xmlScalarMethod(x *ast.MethodCall, m string) ast.Node {
	if x.Target == nil {
		return x
	}
	if m == "value" {
		// A top-level data(e) is the string value of e: XPath 1.0
		// string(e) (data() is XQuery-only).
		if x.XQuery != nil {
			if fc, ok := x.XQuery.Body.(*ast.XQFuncCall); ok && strings.EqualFold(fc.Name, "data") && len(fc.Args) == 1 {
				cp := *x.XQuery
				cp.Body = &ast.XQFuncCall{Name: "string", Args: fc.Args}
				xc := *x
				xc.XQuery = &cp
				x = &xc
			}
		}
	}
	prep, why := rw.prepareXQuery(x)
	if why != "" {
		rw.label(xmlMethodLabel(x, why))
		return x
	}
	path := pgwriter.WriteXPath1(prep.body)
	switch m {
	case "value":
		if x.ValueType == nil {
			rw.label(xmlMethodLabel(x, "the SQL type argument is not a type literal"))
			return x
		}
		return xmlValueSubquery(prep.ns, x, rw.mapType(x.ValueType), path)
	case "exist":
		if !xqSelectsNodes(prep.body) {
			// exist() of a boolean expression is 1 even when it is false
			// (the result is a non-empty sequence): not boolean().
			rw.label(xmlMethodLabel(x, "exist() of an expression that is not a path"))
			return x
		}
		test := pgwriter.WriteXPath1(&ast.XQFuncCall{Name: "boolean", Args: []ast.XQExpr{prep.body}})
		return xmlValueSubquery(prep.ns, x, "boolean", test)
	case "query":
		if !xqSelectsNodes(prep.body) {
			rw.label(xmlMethodLabel(x, "query() of an expression that is not a path"))
			return x
		}
		xt := &ast.FromXMLTable{Namespaces: prep.ns, RowPath: path, Passing: x.Target,
			Columns: []ast.XMLTableColumn{{Name: "v", Type: "xml", Path: "."}}, Alias: "_x", P: x.P}
		sel := &ast.SelectStmt{Cols: []ast.SelectItem{{Expr: rw.fc("xmlagg", x.P, &ast.Ident{Parts: []string{"v"}})}}, From: []ast.FromItem{xt}}
		return &ast.SubqueryExpr{Stmt: sel, P: x.P}
	}
	return x
}

// xmlValueSubquery is `(SELECT "v" FROM XMLTABLE(ns, '/' PASSING (x)
// COLUMNS "v" typ PATH 'path') AS "_x")::typ`: path evaluated once over
// the document of x (no row, hence NULL, when x is NULL).
func xmlValueSubquery(ns []ast.XQNamespace, x *ast.MethodCall, typ, path string) ast.Expr {
	xt := &ast.FromXMLTable{Namespaces: ns, RowPath: "/", Passing: x.Target,
		Columns: []ast.XMLTableColumn{{Name: "v", Type: typ, Path: path}}, Alias: "_x", P: x.P}
	sel := &ast.SelectStmt{Cols: []ast.SelectItem{{Expr: &ast.Ident{Parts: []string{"v"}}}}, From: []ast.FromItem{xt}}
	// The cast types the subquery for the rewriters' value classes.
	return &ast.CastExpr{Expr: &ast.SubqueryExpr{Stmt: sel, P: x.P}, Type: &ast.PGType{Name: typ}, P: x.P}
}

// xmlNodesSource rewrites `x.nodes(q) AS T(c)` into an XMLTABLE source.
func (rw *tsqlRW) xmlNodesSource(f *ast.FromFunc) ast.Node {
	x, ok := f.Func.(*ast.MethodCall)
	if !ok || !strings.EqualFold(x.Method, "nodes") || x.Target == nil {
		return f
	}
	if f.Alias == "" || len(f.Cols) != 1 {
		rw.label(xmlMethodLabel(x, "nodes() needs an alias with one column, AS T(c)"))
		return f
	}
	prep, why := rw.prepareXQuery(x)
	if why == "" && !xqSelectsNodes(prep.body) {
		why = "nodes() of an expression that is not a path"
	}
	if why != "" {
		rw.label(xmlMethodLabel(x, why))
		return f
	}
	return &ast.FromXMLTable{
		Namespaces: prep.ns,
		RowPath:    pgwriter.WriteXPath1(prep.body),
		Passing:    x.Target,
		Columns:    []ast.XMLTableColumn{{Name: normalizeTSQLIdent(f.Cols[0]), Type: "xml", Path: "."}},
		Alias:      normalizeTSQLIdent(f.Alias),
		P:          f.P,
	}
}

// xqSelectsNodes reports whether e is a node-selecting expression (a
// path, a parenthesised one, a union).
func xqSelectsNodes(e ast.XQExpr) bool {
	switch x := e.(type) {
	case *ast.XQPath:
		if len(x.Steps) == 0 {
			return true
		}
		if first := x.Steps[0]; first.Primary != nil && len(x.Steps) == 1 {
			return xqSelectsNodes(first.Primary)
		}
		return true
	case *ast.XQParen:
		return xqSelectsNodes(x.Inner)
	case *ast.XQContextItem:
		return true
	case *ast.XQBinary:
		return x.Op == "|" && xqSelectsNodes(x.Lhs) && xqSelectsNodes(x.Rhs)
	}
	return false
}

// xqFreePrefix returns a namespace prefix no declaration of ns uses, for
// the default element namespace.
func xqFreePrefix(ns []ast.XQNamespace) string {
	used := map[string]bool{}
	for _, n := range ns {
		used[n.Prefix] = true
	}
	for i := 0; ; i++ {
		p := "dns"
		if i > 0 {
			p += strings.Repeat("_", i)
		}
		if !used[p] {
			return p
		}
	}
}

// xqClone deep-copies an XQuery expression tree (the rewrite never
// modifies the parsed MethodCall.XQuery).
func xqClone(e ast.XQExpr) ast.XQExpr {
	switch x := e.(type) {
	case *ast.XQPath:
		cp := &ast.XQPath{Root: x.Root, Steps: make([]*ast.XQStep, len(x.Steps))}
		for i, st := range x.Steps {
			cp.Steps[i] = xqCloneStep(st)
		}
		return cp
	case *ast.XQContextItem:
		return &ast.XQContextItem{}
	case *ast.XQFuncCall:
		cp := &ast.XQFuncCall{Prefix: x.Prefix, Name: x.Name, Args: make([]ast.XQExpr, len(x.Args))}
		for i, a := range x.Args {
			cp.Args[i] = xqClone(a)
		}
		return cp
	case *ast.XQLiteral:
		cp := *x
		return &cp
	case *ast.XQBinary:
		return &ast.XQBinary{Op: x.Op, Lhs: xqClone(x.Lhs), Rhs: xqClone(x.Rhs)}
	case *ast.XQUnary:
		return &ast.XQUnary{Op: x.Op, Expr: xqClone(x.Expr)}
	case *ast.XQParen:
		return &ast.XQParen{Inner: xqClone(x.Inner)}
	}
	return e
}

func xqCloneStep(st *ast.XQStep) *ast.XQStep {
	cp := &ast.XQStep{Desc: st.Desc, Axis: st.Axis}
	if st.Test != nil {
		t := *st.Test
		cp.Test = &t
	}
	if st.Primary != nil {
		cp.Primary = xqClone(st.Primary)
	}
	for _, p := range st.Preds {
		cp.Preds = append(cp.Preds, xqClone(p))
	}
	return cp
}

// xqWalk calls fn on every node of e (pre-order, predicates and
// arguments included).
func xqWalk(e ast.XQExpr, fn func(ast.XQExpr, *ast.XQStep)) {
	switch x := e.(type) {
	case *ast.XQPath:
		fn(x, nil)
		for _, st := range x.Steps {
			fn(nil, st)
			if st.Primary != nil {
				xqWalk(st.Primary, fn)
			}
			for _, p := range st.Preds {
				xqWalk(p, fn)
			}
		}
	case *ast.XQFuncCall:
		fn(x, nil)
		for _, a := range x.Args {
			xqWalk(a, fn)
		}
	case *ast.XQBinary:
		fn(x, nil)
		xqWalk(x.Lhs, fn)
		xqWalk(x.Rhs, fn)
	case *ast.XQUnary:
		fn(x, nil)
		xqWalk(x.Expr, fn)
	case *ast.XQParen:
		fn(x, nil)
		xqWalk(x.Inner, fn)
	case nil:
	default:
		fn(x, nil)
	}
}

// xqPrefixElements puts prefix on every unprefixed element name test of
// e (in place: e is a clone). Attribute tests and wildcards are not
// affected by a default element namespace.
func xqPrefixElements(e ast.XQExpr, prefix string) {
	xqWalk(e, func(_ ast.XQExpr, st *ast.XQStep) {
		if st == nil || st.Test == nil || st.Test.Kind != "name" || st.Test.Prefix != "" || st.Test.Local == "*" {
			return
		}
		switch st.Axis {
		case "attribute":
			return
		}
		st.Test.Prefix = prefix
	})
}

// xqXPath1Funcs are the XPath 1.0 core functions libxml2 evaluates.
var xqXPath1Funcs = map[string]bool{
	"last": true, "position": true, "count": true, "id": true, "local-name": true,
	"namespace-uri": true, "name": true, "string": true, "concat": true, "starts-with": true,
	"contains": true, "substring-before": true, "substring-after": true, "substring": true,
	"string-length": true, "normalize-space": true, "translate": true, "boolean": true,
	"not": true, "true": true, "false": true, "lang": true, "number": true, "sum": true,
	"floor": true, "ceiling": true, "round": true,
}

// xqCheckXPath1 returns why e is not XPath 1.0 ("" when it is).
func xqCheckXPath1(e ast.XQExpr) string {
	why := ""
	xqWalk(e, func(n ast.XQExpr, _ *ast.XQStep) {
		if why != "" {
			return
		}
		switch x := n.(type) {
		case *ast.XQFuncCall:
			if (x.Prefix != "" && x.Prefix != "fn") || !xqXPath1Funcs[x.Name] {
				why = "XQuery function " + x.Name + "() has no XPath 1.0 counterpart"
			}
		case *ast.XQBinary:
			if x.Op == "idiv" {
				why = "XQuery operator idiv"
			}
		}
	})
	return why
}

// xqDropAtomicSingletons removes the `[1]` XQuery requires on a
// function result (`string(.)[1]`): the XPath 1.0 core functions other
// than id() return a single string / number / boolean, which XPath 1.0
// cannot filter with a predicate (libxml2 raises "Invalid type").
// Returns the (possibly) simplified expression; e is a clone.
func xqDropAtomicSingletons(e ast.XQExpr) ast.XQExpr {
	p, ok := e.(*ast.XQPath)
	if ok && !p.Root && len(p.Steps) == 1 {
		st := p.Steps[0]
		if fc, isCall := st.Primary.(*ast.XQFuncCall); isCall && fc.Name != "id" && len(st.Preds) == 1 {
			if lit, isLit := st.Preds[0].(*ast.XQLiteral); isLit && !lit.String && lit.Text == "1" {
				return fc
			}
		}
	}
	xqWalk(e, func(_ ast.XQExpr, st *ast.XQStep) {
		if st == nil {
			return
		}
		for i, pr := range st.Preds {
			st.Preds[i] = xqDropAtomicSingletons(pr)
		}
		if st.Primary != nil {
			st.Primary = xqDropAtomicSingletons(st.Primary)
		}
	})
	return e
}

// xqAnchorOnRow rewrites the context-relative paths of a query read from
// a nodes() row (the element of a document of its own on PG) so they
// start from that element: `.` → `/*`, `a/b` → `/*/a/b`, `@x` →
// `/*/@x`. Predicates keep their own context. An absolute path is kept
// when the row is its source document's element (root), refused
// otherwise.
func xqAnchorOnRow(e ast.XQExpr, root bool) (ast.XQExpr, string) {
	elem := func() *ast.XQStep {
		return &ast.XQStep{Axis: "child", Test: &ast.XQNodeTest{Kind: "name", Local: "*"}}
	}
	switch x := e.(type) {
	case *ast.XQContextItem:
		return &ast.XQPath{Root: true, Steps: []*ast.XQStep{elem()}}, ""
	case *ast.XQPath:
		if x.Root {
			if !root {
				return nil, "an absolute path read from a nodes() row that is not its document's element"
			}
			return x, ""
		}
		first := x.Steps[0]
		if first.Primary != nil {
			inner, why := xqAnchorOnRow(first.Primary, root)
			if why != "" {
				return nil, why
			}
			first.Primary = inner
			return x, ""
		}
		switch first.Axis {
		case "", "child", "attribute", "descendant", "descendant-or-self":
			return &ast.XQPath{Root: true, Steps: append([]*ast.XQStep{elem()}, x.Steps...)}, ""
		case "self":
			// self::x on the row element is the document's element x.
			first.Axis = "child"
			return &ast.XQPath{Root: true, Steps: x.Steps}, ""
		}
		return nil, "the " + first.Axis + " axis read from a nodes() row"
	case *ast.XQParen:
		inner, why := xqAnchorOnRow(x.Inner, root)
		if why != "" {
			return nil, why
		}
		return &ast.XQParen{Inner: inner}, ""
	case *ast.XQFuncCall:
		cp := *x
		cp.Args = make([]ast.XQExpr, len(x.Args))
		for i, a := range x.Args {
			na, why := xqAnchorOnRow(a, root)
			if why != "" {
				return nil, why
			}
			cp.Args[i] = na
		}
		return &cp, ""
	case *ast.XQBinary:
		l, why := xqAnchorOnRow(x.Lhs, root)
		if why != "" {
			return nil, why
		}
		r, why := xqAnchorOnRow(x.Rhs, root)
		if why != "" {
			return nil, why
		}
		return &ast.XQBinary{Op: x.Op, Lhs: l, Rhs: r}, ""
	case *ast.XQUnary:
		in, why := xqAnchorOnRow(x.Expr, root)
		if why != "" {
			return nil, why
		}
		return &ast.XQUnary{Op: x.Op, Expr: in}, ""
	}
	return e, ""
}
