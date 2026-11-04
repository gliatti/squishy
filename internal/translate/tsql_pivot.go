package translate

import (
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// tsql_pivot.go — T-SQL PIVOT rewritten as conditional aggregation.
//
//	FROM (SELECT a, b, v, k FROM …) AS s
//	PIVOT (SUM(v) FOR k IN ([x], [y])) AS p
//
// becomes the derived table
//
//	FROM (SELECT "s"."a", "s"."b",
//	             sum(CASE WHEN "s"."k" = 'x' THEN "s"."v" END) AS "x",
//	             sum(CASE WHEN "s"."k" = 'y' THEN "s"."v" END) AS "y"
//	      FROM (…) AS s GROUP BY "s"."a", "s"."b") AS p
//
// which is PIVOT's definition: the rows are grouped by every source
// column that is neither the pivot column nor read by the aggregate, and
// each IN value gets the aggregate of the rows whose pivot column equals
// it. An IN entry is a column name standing for a value of the pivot
// column; it is compared as an untyped literal, which PostgreSQL
// converts to the pivot column's type — as SQL Server converts it. The
// aggregate sees NULL for the other rows, which every aggregate PIVOT
// accepts ignores (COUNT(*) is not allowed in a PIVOT).
//
// The source columns must be known: a derived table with an explicit
// select list (no *), or a table of the migrated script. UNPIVOT is
// reported.

// pivot rewrites a FromPivot; the node is returned unchanged (and
// reported by the residue walk) when it cannot be.
func (rw *tsqlRW) pivot(x *ast.FromPivot) ast.Node {
	if x.Unpivot {
		return x
	}
	agg, ok := x.Agg.(*ast.FuncCall)
	if !ok || len(agg.Args) != 1 || x.ForCol == "" || len(x.InCols) == 0 {
		return x
	}
	srcAlias, cols := rw.pivotSourceColumns(x.Source)
	if cols == nil {
		rw.label("PIVOT over a source whose columns are not known (SELECT * or an unknown table)")
		return x
	}
	ref := func(col string) ast.Expr {
		if srcAlias == "" {
			return &ast.Ident{Parts: []string{col}}
		}
		return &ast.Ident{Parts: []string{srcAlias, col}}
	}
	forCol := normalizeTSQLIdent(x.ForCol)
	// Columns the aggregate reads (folded names of its column refs).
	read := map[string]bool{}
	ast.Walk(tsqlVisitFunc(func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && !id.Variable && len(id.Parts) > 0 {
			read[id.Parts[len(id.Parts)-1]] = true
		}
		return true
	}), agg)
	found := false
	sel := &ast.SelectStmt{}
	for _, c := range cols {
		if c == forCol {
			found = true
			continue
		}
		if read[c] {
			continue
		}
		sel.Cols = append(sel.Cols, ast.SelectItem{Expr: ref(c)})
		sel.GroupBy = append(sel.GroupBy, ref(c))
	}
	if !found {
		rw.label("PIVOT column " + x.ForCol + " is not a column of its source")
		return x
	}
	for _, in := range x.InCols {
		cp := *agg
		cp.Args = []ast.Expr{&ast.CaseExpr{
			Whens: []ast.CaseExprWhen{{
				Match: &ast.BinaryExpr{Op: "=", Lhs: ref(forCol), Rhs: &ast.Literal{Kind: "string", Text: in}},
				Then:  tsqlRequalify(agg.Args[0], srcAlias),
			}},
		}}
		sel.Cols = append(sel.Cols, ast.SelectItem{Expr: &cp, Alias: normalizeTSQLIdent(in)})
	}
	sel.From = []ast.FromItem{x.Source}
	alias := normalizeTSQLIdent(x.Alias)
	if alias == "" {
		alias = "pvt"
	}
	return &ast.FromSubquery{Stmt: sel, Alias: alias, P: x.P}
}

// pivotSourceColumns returns the alias the PIVOT source is referenced by
// and its folded output column names, in order (nil when unknown).
func (rw *tsqlRW) pivotSourceColumns(src ast.FromItem) (string, []string) {
	switch s := src.(type) {
	case *ast.FromSubquery:
		if s.Stmt == nil {
			return "", nil
		}
		if len(s.Cols) > 0 {
			return s.Alias, append([]string(nil), s.Cols...)
		}
		var out []string
		for _, it := range s.Stmt.Cols {
			switch {
			case it.Star:
				return "", nil
			case it.Alias != "":
				out = append(out, it.Alias)
			default:
				id, ok := it.Expr.(*ast.Ident)
				if !ok || len(id.Parts) == 0 {
					return "", nil
				}
				out = append(out, id.Parts[len(id.Parts)-1])
			}
		}
		return s.Alias, out
	case *ast.FromTable:
		if rw.run == nil {
			return "", nil
		}
		cols := rw.run.TableColumnNames(s.Schema, s.Name)
		if cols == nil {
			return "", nil
		}
		alias := s.Alias
		if alias == "" {
			alias = s.Name
		}
		return alias, cols
	}
	return "", nil
}

// tsqlRequalify returns e with its unqualified column references
// qualified by alias (the aggregate argument moves into a query whose
// FROM is the PIVOT source). Qualified references are kept.
func tsqlRequalify(e ast.Expr, alias string) ast.Expr {
	if alias == "" {
		return e
	}
	if id, ok := e.(*ast.Ident); ok && !id.Variable && len(id.Parts) == 1 {
		return &ast.Ident{Parts: []string{alias, id.Parts[0]}, P: id.P}
	}
	return e
}
