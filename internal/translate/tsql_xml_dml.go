package translate

import (
	"strings"

	pgast "gitlab.com/dalibo/squishy/internal/dialects/postgres"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// tsql_xml_dml.go — UPDATE … SET col.modify(N'<XML DML>') in a routine
// body.
//
// XML DML (insert / delete / replace value of, applied in place to an
// xml value) has no PostgreSQL counterpart: PG's xml type has no update
// function, and rebuilding a document with string functions would be a
// text-level XML editor. Such an UPDATE is not translated. Leaving it out
// would let the routine silently skip a change SQL Server makes, and
// keeping it would fail the routine at CREATE; instead it becomes a guard
// that raises an error exactly when the UPDATE would modify a row:
//
//	IF EXISTS (SELECT 1 FROM <target> [, <FROM items>] WHERE <where>) THEN
//	    RAISE EXCEPTION 'XML DML … not supported on PostgreSQL';
//	END IF;
//
// The routine is created, behaves as SQL Server whenever the statement
// has nothing to do, and fails loudly otherwise. The blocking
// routine.untranslated_construct entry says so: the statement must be
// rewritten by hand (keep the value in a column, rebuild the document,
// or move the logic to the application).

// tsqlXMLModifyLabel is the residual label of modify() (tsqlMethodResidue
// of a modify() call: XQueryErr set by the parser).
const tsqlXMLModifyLabel = "XML data-type method modify() (XML DML (modify()) has no PostgreSQL counterpart)"

// xmlModifyGuard returns the raising guard of an UPDATE whose SET list
// applies an xml modify(), nil for any other UPDATE.
func (b *tsqlBody) xmlModifyGuard(x *ast.UpdateStmt) []pgast.PLStmt {
	col := ""
	for _, a := range x.Sets {
		if mc, ok := a.Expr.(*ast.MethodCall); ok && strings.EqualFold(mc.Method, "modify") && mc.StaticType == "" {
			col = a.Col
			break
		}
	}
	if col == "" {
		return nil
	}
	b.xmlDMLGuarded = true
	target := b.tableRef(x.Table)
	tname := target.Name
	if target.Schema != "" {
		tname = target.Schema + "." + tname
	}
	b.warn("UPDATE " + tname + " SET " + col + ".modify(…): XML DML has no PostgreSQL counterpart (PG cannot update an xml value in place). " +
		"The UPDATE is replaced by a check that raises an error whenever it would modify a row; rewrite it by hand " +
		"(for instance keep the value in its own column, or rebuild the document in the application)")
	sel := &ast.SelectStmt{
		Cols:  []ast.SelectItem{{Expr: ast.BuildIntLit(1)}},
		From:  append([]ast.FromItem{&ast.FromTable{Schema: target.Schema, Name: target.Name, Alias: x.Alias, P: x.P}}, x.From...),
		Where: x.Where,
	}
	cond := b.expr(&ast.ExistsExpr{Subquery: sel, P: x.P})
	msg := "T-SQL XML DML " + tname + "." + normalizeTSQLIdent(col) + ".modify() of " + b.ctx.Object + " is not supported on PostgreSQL"
	return []pgast.PLStmt{&pgast.PLIf{Branches: []pgast.PLIfBranch{{
		Cond: cond,
		Body: []pgast.PLStmt{&pgast.PLRaise{Level: "EXCEPTION", Msg: msg, ErrCode: "feature_not_supported"}},
	}}}}
}
