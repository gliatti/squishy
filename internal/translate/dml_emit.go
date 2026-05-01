package translate

import (
	"fmt"
	"strings"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// ---------------------------------------------------------------------------
// Typed-DML → PostgreSQL text emitter.
//
// Walks the structured DML nodes produced by the per-dialect parsers in
// internal/dialects/{mysql,oracle}/parser_dml.go and renders Postgres SQL
// suitable for embedding inside a PL/pgSQL routine body (or for emission as
// a top-level statement). The emitter is dialect-neutral on purpose — any
// source-dialect-specific tweaks happen either:
//   - earlier, when the parser fills typed nodes (Oracle SYSDATE → FuncCall
//     with a known name handled by mapFunction); or
//   - later, in rewriteOraclePLpgSQL, which fixes leftover Oracle-only
//     identifiers like `:NEW.col`.
//
// Each entrypoint returns a single-line PG fragment (sub-clauses joined with
// spaces) without a trailing semicolon — the caller appends `;` when wrapping
// in a PLRawSQL.
// ---------------------------------------------------------------------------

// emitSelectStmt renders a complete SELECT statement (with optional WITH
// preamble, UNION/INTERSECT/MINUS chain, ORDER BY, LIMIT/OFFSET, and lock
// clause).
func emitSelectStmt(s *ast.SelectStmt) string {
	if s == nil {
		return ""
	}
	var b strings.Builder
	if s.With != nil {
		b.WriteString(emitWith(s.With))
		b.WriteByte(' ')
	}
	b.WriteString(emitQueryBody(s))
	for _, op := range s.SetOps {
		b.WriteByte(' ')
		b.WriteString(op.Op)
		b.WriteByte(' ')
		b.WriteString(emitQueryBody(op.Stmt))
	}
	if len(s.OrderBy) > 0 {
		b.WriteString(" ORDER BY ")
		items := make([]string, len(s.OrderBy))
		for i, oi := range s.OrderBy {
			items[i] = renderOrderItem(oi)
		}
		b.WriteString(strings.Join(items, ", "))
	}
	if s.Offset != nil {
		b.WriteString(" OFFSET ")
		b.WriteString(rawExpr(s.Offset))
	}
	if s.Limit != nil {
		b.WriteString(" LIMIT ")
		b.WriteString(rawExpr(s.Limit))
	}
	if s.ForUpdate != "" {
		b.WriteByte(' ')
		b.WriteString(s.ForUpdate)
	}
	return b.String()
}

// emitWith renders `WITH [RECURSIVE] cte1 AS (...) [, cte2 AS (...)]*`.
func emitWith(w *ast.WithClause) string {
	var b strings.Builder
	b.WriteString("WITH")
	if w.Recursive {
		b.WriteString(" RECURSIVE")
	}
	for i, c := range w.CTEs {
		if i == 0 {
			b.WriteByte(' ')
		} else {
			b.WriteString(", ")
		}
		b.WriteString(quoteIdent(c.Name))
		if len(c.Columns) > 0 {
			cols := make([]string, len(c.Columns))
			for j, n := range c.Columns {
				cols[j] = quoteIdent(n)
			}
			b.WriteString(" (")
			b.WriteString(strings.Join(cols, ", "))
			b.WriteString(")")
		}
		b.WriteString(" AS (")
		b.WriteString(emitSelectStmt(c.Body))
		b.WriteString(")")
	}
	return b.String()
}

// emitQueryBody renders a single SELECT body — projection, FROM, WHERE,
// GROUP BY, HAVING. Used for the head of a query expression and for each
// branch of a UNION chain.
func emitQueryBody(s *ast.SelectStmt) string {
	var b strings.Builder
	b.WriteString("SELECT")
	if s.Distinct {
		b.WriteString(" DISTINCT")
	}
	b.WriteByte(' ')
	cols := make([]string, len(s.Cols))
	for i, c := range s.Cols {
		cols[i] = renderSelectItem(c)
	}
	b.WriteString(strings.Join(cols, ", "))
	if len(s.From) > 0 {
		b.WriteString(" FROM ")
		from := make([]string, len(s.From))
		for i, f := range s.From {
			from[i] = renderFromItem(f)
		}
		b.WriteString(strings.Join(from, ", "))
	}
	if s.Where != nil {
		b.WriteString(" WHERE ")
		b.WriteString(rawExpr(s.Where))
	}
	if len(s.GroupBy) > 0 {
		b.WriteString(" GROUP BY ")
		items := make([]string, len(s.GroupBy))
		for i, e := range s.GroupBy {
			items[i] = rawExpr(e)
		}
		b.WriteString(strings.Join(items, ", "))
	}
	if s.Having != nil {
		b.WriteString(" HAVING ")
		b.WriteString(rawExpr(s.Having))
	}
	return b.String()
}

// renderSelectItem covers the projection grammar:
//   '*'                       → *
//   <qualifier>.*             → "qualifier".*
//   <expr> [AS] <alias>?      → <expr> [AS "alias"]
func renderSelectItem(it ast.SelectItem) string {
	if it.Star {
		if it.Qualifier == "" {
			return "*"
		}
		return quoteIdent(it.Qualifier) + ".*"
	}
	out := rawExpr(it.Expr)
	if it.Alias != "" {
		out += " AS " + quoteIdent(it.Alias)
	}
	return out
}

// renderFromItem walks the FromItem hierarchy (FromTable / FromSubquery /
// FromJoin) and emits the PG-flavoured FROM-clause text for each.
func renderFromItem(f ast.FromItem) string {
	switch x := f.(type) {
	case *ast.FromTable:
		out := qualifiedTableName(x.Schema, x.Name)
		if x.Alias != "" {
			out += " " + quoteIdent(x.Alias)
		}
		return out
	case *ast.FromSubquery:
		out := "(" + emitSelectStmt(x.Stmt) + ")"
		if x.Alias != "" {
			out += " " + quoteIdent(x.Alias)
		}
		if len(x.Cols) > 0 {
			cols := make([]string, len(x.Cols))
			for i, n := range x.Cols {
				cols[i] = quoteIdent(n)
			}
			out += " (" + strings.Join(cols, ", ") + ")"
		}
		return out
	case *ast.FromJoin:
		left := renderFromItem(x.Left)
		right := renderFromItem(x.Right)
		kw := x.Kind.String()
		if x.Natural {
			kw = "NATURAL " + kw
		}
		out := left + " " + kw + " " + right
		switch {
		case x.On != nil:
			out += " ON " + rawExpr(x.On)
		case len(x.Using) > 0:
			cols := make([]string, len(x.Using))
			for i, n := range x.Using {
				cols[i] = quoteIdent(n)
			}
			out += " USING (" + strings.Join(cols, ", ") + ")"
		}
		return out
	}
	return ""
}

// qualifiedTableName quotes [schema.]name. Empty schema produces a bare
// quoted name.
func qualifiedTableName(schema, name string) string {
	if schema == "" {
		return quoteIdent(name)
	}
	return quoteIdent(schema) + "." + quoteIdent(name)
}

// emitInsertStmt renders an INSERT statement.
func emitInsertStmt(s *ast.InsertStmt) string {
	if s == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(qualifiedTableName(s.Table.Schema, s.Table.Name))
	if len(s.Cols) > 0 {
		cols := make([]string, len(s.Cols))
		for i, c := range s.Cols {
			cols[i] = quoteIdent(c)
		}
		b.WriteString(" (")
		b.WriteString(strings.Join(cols, ", "))
		b.WriteString(")")
	}
	switch {
	case s.Select != nil:
		b.WriteByte(' ')
		b.WriteString(emitSelectStmt(s.Select))
	case len(s.Values) > 0:
		b.WriteString(" VALUES ")
		rows := make([]string, len(s.Values))
		for i, row := range s.Values {
			cells := make([]string, len(row))
			for j, e := range row {
				cells[j] = rawExpr(e)
			}
			rows[i] = "(" + strings.Join(cells, ", ") + ")"
		}
		b.WriteString(strings.Join(rows, ", "))
	}
	if s.OnConflict != nil {
		b.WriteString(emitOnConflict(s.OnConflict))
	}
	if len(s.Returning) > 0 {
		b.WriteString(" RETURNING ")
		items := make([]string, len(s.Returning))
		for i, it := range s.Returning {
			items[i] = renderSelectItem(it)
		}
		b.WriteString(strings.Join(items, ", "))
	}
	return b.String()
}

// emitOnConflict renders ON CONFLICT in PG syntax (mapped from MySQL's
// ON DUPLICATE KEY UPDATE — the conflict target is left implicit since
// the source dialect did not specify one).
func emitOnConflict(oc *ast.OnConflict) string {
	var b strings.Builder
	b.WriteString(" ON CONFLICT")
	if len(oc.Target) > 0 {
		cols := make([]string, len(oc.Target))
		for i, c := range oc.Target {
			cols[i] = quoteIdent(c)
		}
		b.WriteString(" (")
		b.WriteString(strings.Join(cols, ", "))
		b.WriteString(")")
	}
	if oc.DoNothing {
		b.WriteString(" DO NOTHING")
		return b.String()
	}
	b.WriteString(" DO UPDATE SET ")
	parts := make([]string, len(oc.Sets))
	for i, a := range oc.Sets {
		parts[i] = quoteColumnRef(a.Col) + " = " + rawExpr(a.Expr)
	}
	b.WriteString(strings.Join(parts, ", "))
	if oc.Where != nil {
		b.WriteString(" WHERE ")
		b.WriteString(rawExpr(oc.Where))
	}
	return b.String()
}

// emitUpdateStmt renders an UPDATE statement.
func emitUpdateStmt(s *ast.UpdateStmt) string {
	if s == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("UPDATE ")
	if s.Table.Name != "" {
		b.WriteString(qualifiedTableName(s.Table.Schema, s.Table.Name))
		if s.Alias != "" {
			b.WriteByte(' ')
			b.WriteString(quoteIdent(s.Alias))
		}
	} else if len(s.From) > 0 {
		// Multi-table UPDATE; the first FROM item plays the target role in
		// PG — the rest go into a USING list.
		b.WriteString(renderFromItem(s.From[0]))
	}
	b.WriteString(" SET ")
	parts := make([]string, len(s.Sets))
	for i, a := range s.Sets {
		// Tuple form `(c1, c2, ...) = (subquery|row)` — Oracle and PG share
		// this exact syntax. Emit verbatim with each column quoted.
		if len(a.TupleCols) > 0 {
			cols := make([]string, len(a.TupleCols))
			for j, c := range a.TupleCols {
				cols[j] = quoteColumnRef(c)
			}
			parts[i] = "(" + strings.Join(cols, ", ") + ") = " + rawExpr(a.Expr)
			continue
		}
		parts[i] = quoteColumnRef(a.Col) + " = " + rawExpr(a.Expr)
	}
	b.WriteString(strings.Join(parts, ", "))
	if s.Table.Name != "" && len(s.From) > 0 {
		b.WriteString(" FROM ")
		from := make([]string, len(s.From))
		for i, f := range s.From {
			from[i] = renderFromItem(f)
		}
		b.WriteString(strings.Join(from, ", "))
	} else if s.Table.Name == "" && len(s.From) > 1 {
		b.WriteString(" FROM ")
		from := make([]string, len(s.From)-1)
		for i, f := range s.From[1:] {
			from[i] = renderFromItem(f)
		}
		b.WriteString(strings.Join(from, ", "))
	}
	if s.Where != nil {
		b.WriteString(" WHERE ")
		b.WriteString(rawExpr(s.Where))
	}
	if len(s.Returning) > 0 {
		b.WriteString(" RETURNING ")
		items := make([]string, len(s.Returning))
		for i, it := range s.Returning {
			items[i] = renderSelectItem(it)
		}
		b.WriteString(strings.Join(items, ", "))
	}
	return b.String()
}

// emitDeleteStmt renders a DELETE statement.
func emitDeleteStmt(s *ast.DeleteStmt) string {
	if s == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("DELETE FROM ")
	if s.Table.Name != "" {
		b.WriteString(qualifiedTableName(s.Table.Schema, s.Table.Name))
		if s.Alias != "" {
			b.WriteByte(' ')
			b.WriteString(quoteIdent(s.Alias))
		}
	} else if len(s.Using) > 0 {
		// Multi-target DELETE — first Using item becomes the target. The PG
		// `DELETE FROM t USING …` shape keeps the rest as USING sources.
		b.WriteString(renderFromItem(s.Using[0]))
	}
	if s.Table.Name != "" && len(s.Using) > 0 {
		b.WriteString(" USING ")
		using := make([]string, len(s.Using))
		for i, f := range s.Using {
			using[i] = renderFromItem(f)
		}
		b.WriteString(strings.Join(using, ", "))
	} else if s.Table.Name == "" && len(s.Using) > 1 {
		b.WriteString(" USING ")
		using := make([]string, len(s.Using)-1)
		for i, f := range s.Using[1:] {
			using[i] = renderFromItem(f)
		}
		b.WriteString(strings.Join(using, ", "))
	}
	if s.Where != nil {
		b.WriteString(" WHERE ")
		b.WriteString(rawExpr(s.Where))
	}
	if len(s.Returning) > 0 {
		b.WriteString(" RETURNING ")
		items := make([]string, len(s.Returning))
		for i, it := range s.Returning {
			items[i] = renderSelectItem(it)
		}
		b.WriteString(strings.Join(items, ", "))
	}
	return b.String()
}

// emitMergeStmt renders a MERGE statement (PG 15+ supports MERGE).
func emitMergeStmt(s *ast.MergeStmt) string {
	if s == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("MERGE INTO ")
	b.WriteString(qualifiedTableName(s.Target.Schema, s.Target.Name))
	if s.TargetAlias != "" {
		b.WriteByte(' ')
		b.WriteString(quoteIdent(s.TargetAlias))
	}
	b.WriteString(" USING ")
	b.WriteString(renderFromItem(s.Source))
	b.WriteString(" ON ")
	b.WriteString(rawExpr(s.On))
	for _, m := range s.WhenMatched {
		b.WriteString(" WHEN MATCHED")
		if m.Cond != nil {
			b.WriteString(" AND ")
			b.WriteString(rawExpr(m.Cond))
		}
		b.WriteString(" THEN ")
		b.WriteString(renderMergeAction(m))
	}
	for _, m := range s.WhenNotMatched {
		b.WriteString(" WHEN NOT MATCHED")
		if m.Cond != nil {
			b.WriteString(" AND ")
			b.WriteString(rawExpr(m.Cond))
		}
		b.WriteString(" THEN ")
		b.WriteString(renderMergeAction(m))
	}
	return b.String()
}

// quoteColumnRef quotes a possibly-dotted column reference (e.g. "t.qty"
// → `"t"."qty"`). The unqualified form keeps a single quoted identifier.
// Used wherever Assign.Col / InsertCol values are emitted, since the
// parser collapses dotted column refs into a single string.
func quoteColumnRef(s string) string {
	if !strings.Contains(s, ".") {
		return quoteIdent(s)
	}
	parts := strings.Split(s, ".")
	for i, p := range parts {
		parts[i] = quoteIdent(p)
	}
	return strings.Join(parts, ".")
}

func renderMergeAction(m ast.MergeAction) string {
	switch m.Kind {
	case "UPDATE":
		parts := make([]string, len(m.Sets))
		for i, a := range m.Sets {
			parts[i] = quoteColumnRef(a.Col) + " = " + rawExpr(a.Expr)
		}
		return "UPDATE SET " + strings.Join(parts, ", ")
	case "DELETE":
		return "DELETE"
	case "INSERT":
		var b strings.Builder
		b.WriteString("INSERT")
		if len(m.InsertCols) > 0 {
			cols := make([]string, len(m.InsertCols))
			for i, c := range m.InsertCols {
				cols[i] = quoteIdent(c)
			}
			b.WriteString(" (")
			b.WriteString(strings.Join(cols, ", "))
			b.WriteString(")")
		}
		if len(m.InsertValues) > 0 {
			vals := make([]string, len(m.InsertValues))
			for i, e := range m.InsertValues {
				vals[i] = rawExpr(e)
			}
			b.WriteString(" VALUES (")
			b.WriteString(strings.Join(vals, ", "))
			b.WriteString(")")
		}
		return b.String()
	case "DO NOTHING":
		return "DO NOTHING"
	}
	return fmt.Sprintf("/* unsupported merge action %q */", m.Kind)
}
