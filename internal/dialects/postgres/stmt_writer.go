package postgres

import (
	"strings"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// stmt_writer.go — PG-side renderer for SELECT / INSERT / UPDATE /
// DELETE / MERGE.
//
// Every output is a single PG-compatible SQL fragment without trailing
// `;` — the caller appends the terminator when wrapping the result.
//
// As with WriteExpr, the writer is "pure": no source-dialect-specific
// translation. Visitors run before the writer to substitute Oracle/
// MySQL-only constructs.

// init wires WriteExpr's forward-declared WriteSelectStmt to the real
// implementation. Defined here so expr_writer.go can stay free of a
// circular dependency.
func init() {
	WriteSelectStmt = writeSelect
}

// WriteStmt is the type-switching entry point for any DML statement.
// Returns "" for nil or unsupported types.
func WriteStmt(s ast.Stmt) string {
	if s == nil {
		return ""
	}
	switch x := s.(type) {
	case *ast.SelectStmt:
		return writeSelect(x)
	case *ast.InsertStmt:
		return writeInsert(x)
	case *ast.UpdateStmt:
		return writeUpdate(x)
	case *ast.DeleteStmt:
		return writeDelete(x)
	case *ast.MergeStmt:
		return writeMerge(x)
	}
	return ""
}

// writeSelect renders a complete SELECT (with optional WITH preamble,
// UNION/INTERSECT/MINUS chain, ORDER BY, LIMIT/OFFSET, lock clause).
func writeSelect(s *ast.SelectStmt) string {
	if s == nil {
		return ""
	}
	var b strings.Builder
	if s.With != nil {
		b.WriteString(writeWith(s.With))
		b.WriteByte(' ')
	}
	b.WriteString(writeQueryBody(s))
	for _, op := range s.SetOps {
		b.WriteByte(' ')
		b.WriteString(op.Op)
		b.WriteByte(' ')
		b.WriteString(writeQueryBody(op.Stmt))
	}
	if len(s.OrderBy) > 0 {
		b.WriteString(" ORDER BY ")
		items := make([]string, len(s.OrderBy))
		for i, oi := range s.OrderBy {
			items[i] = writeOrderItem(oi)
		}
		b.WriteString(strings.Join(items, ", "))
	}
	if s.Offset != nil {
		b.WriteString(" OFFSET ")
		b.WriteString(WriteExpr(s.Offset))
	}
	if s.Limit != nil {
		b.WriteString(" LIMIT ")
		b.WriteString(WriteExpr(s.Limit))
	}
	if s.ForUpdate != "" {
		b.WriteByte(' ')
		b.WriteString(s.ForUpdate)
	}
	return b.String()
}

// writeWith renders WITH [RECURSIVE] cte AS (…) [, …]+.
func writeWith(w *ast.WithClause) string {
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
		b.WriteString(qIdent(c.Name))
		if len(c.Columns) > 0 {
			cols := make([]string, len(c.Columns))
			for j, n := range c.Columns {
				cols[j] = qIdent(n)
			}
			b.WriteString(" (")
			b.WriteString(strings.Join(cols, ", "))
			b.WriteString(")")
		}
		b.WriteString(" AS (")
		b.WriteString(writeSelect(c.Body))
		b.WriteString(")")
	}
	return b.String()
}

// writeQueryBody renders one SELECT branch — projection, FROM, WHERE,
// GROUP BY, HAVING. Used for the head of a query and for each branch
// of a UNION/INTERSECT/MINUS chain.
func writeQueryBody(s *ast.SelectStmt) string {
	var b strings.Builder
	b.WriteString("SELECT")
	if s.Distinct {
		b.WriteString(" DISTINCT")
	}
	b.WriteByte(' ')
	cols := make([]string, len(s.Cols))
	for i, c := range s.Cols {
		cols[i] = writeSelectItem(c)
	}
	b.WriteString(strings.Join(cols, ", "))
	if len(s.From) > 0 {
		b.WriteString(" FROM ")
		from := make([]string, len(s.From))
		for i, f := range s.From {
			from[i] = writeFromItem(f)
		}
		b.WriteString(strings.Join(from, ", "))
	}
	if s.Where != nil {
		b.WriteString(" WHERE ")
		b.WriteString(WriteExpr(s.Where))
	}
	if len(s.GroupBy) > 0 {
		b.WriteString(" GROUP BY ")
		items := make([]string, len(s.GroupBy))
		for i, e := range s.GroupBy {
			items[i] = WriteExpr(e)
		}
		b.WriteString(strings.Join(items, ", "))
	}
	if s.Having != nil {
		b.WriteString(" HAVING ")
		b.WriteString(WriteExpr(s.Having))
	}
	return b.String()
}

// writeSelectItem covers '*', qualifier.*, and `<expr> [AS] <alias>?`.
func writeSelectItem(it ast.SelectItem) string {
	if it.Star {
		if it.Qualifier == "" {
			return "*"
		}
		return qIdent(it.Qualifier) + ".*"
	}
	out := WriteExpr(it.Expr)
	if it.Alias != "" {
		out += " AS " + qIdent(it.Alias)
	}
	return out
}

// writeFromItem walks the FromTable / FromSubquery / FromJoin tree
// and emits the matching FROM-clause text.
func writeFromItem(f ast.FromItem) string {
	switch x := f.(type) {
	case *ast.FromTable:
		out := qualifiedTableName(x.Schema, x.Name)
		if x.Alias != "" {
			out += " " + qIdent(x.Alias)
		}
		return out
	case *ast.FromSubquery:
		out := "(" + writeSelect(x.Stmt) + ")"
		if x.Alias != "" {
			out += " " + qIdent(x.Alias)
		}
		if len(x.Cols) > 0 {
			cols := make([]string, len(x.Cols))
			for i, n := range x.Cols {
				cols[i] = qIdent(n)
			}
			out += " (" + strings.Join(cols, ", ") + ")"
		}
		return out
	case *ast.FromJoin:
		left := writeFromItem(x.Left)
		right := writeFromItem(x.Right)
		kw := x.Kind.String()
		if x.Natural {
			kw = "NATURAL " + kw
		}
		out := left + " " + kw + " " + right
		switch {
		case x.On != nil:
			out += " ON " + WriteExpr(x.On)
		case len(x.Using) > 0:
			cols := make([]string, len(x.Using))
			for i, n := range x.Using {
				cols[i] = qIdent(n)
			}
			out += " USING (" + strings.Join(cols, ", ") + ")"
		}
		return out
	}
	return ""
}

// qualifiedTableName quotes [schema.]name. Empty schema yields a bare
// quoted name (the search_path resolves it).
func qualifiedTableName(schema, name string) string {
	if schema == "" {
		return qIdent(name)
	}
	return qIdent(schema) + "." + qIdent(name)
}

// writeInsert renders an INSERT statement — VALUES, INSERT … SELECT,
// optional ON CONFLICT, optional RETURNING.
func writeInsert(s *ast.InsertStmt) string {
	if s == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(qualifiedTableName(s.Table.Schema, s.Table.Name))
	if len(s.Cols) > 0 {
		cols := make([]string, len(s.Cols))
		for i, c := range s.Cols {
			cols[i] = qIdent(c)
		}
		b.WriteString(" (")
		b.WriteString(strings.Join(cols, ", "))
		b.WriteString(")")
	}
	switch {
	case s.Select != nil:
		b.WriteByte(' ')
		b.WriteString(writeSelect(s.Select))
	case len(s.Values) > 0:
		b.WriteString(" VALUES ")
		rows := make([]string, len(s.Values))
		for i, row := range s.Values {
			cells := make([]string, len(row))
			for j, e := range row {
				cells[j] = WriteExpr(e)
			}
			rows[i] = "(" + strings.Join(cells, ", ") + ")"
		}
		b.WriteString(strings.Join(rows, ", "))
	}
	if s.OnConflict != nil {
		b.WriteString(writeOnConflict(s.OnConflict))
	}
	if len(s.Returning) > 0 {
		b.WriteString(" RETURNING ")
		items := make([]string, len(s.Returning))
		for i, it := range s.Returning {
			items[i] = writeSelectItem(it)
		}
		b.WriteString(strings.Join(items, ", "))
	}
	return b.String()
}

// writeOnConflict renders the PG-flavoured ON CONFLICT clause.
func writeOnConflict(oc *ast.OnConflict) string {
	var b strings.Builder
	b.WriteString(" ON CONFLICT")
	if len(oc.Target) > 0 {
		cols := make([]string, len(oc.Target))
		for i, c := range oc.Target {
			cols[i] = qIdent(c)
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
		parts[i] = quoteColumnRef(a.Col) + " = " + WriteExpr(a.Expr)
	}
	b.WriteString(strings.Join(parts, ", "))
	if oc.Where != nil {
		b.WriteString(" WHERE ")
		b.WriteString(WriteExpr(oc.Where))
	}
	return b.String()
}

// writeUpdate renders an UPDATE statement, including the multi-table
// shape that maps Oracle/MySQL UPDATE … FROM joins to PG's UPDATE …
// FROM (target stays canonical, additional sources move to FROM).
func writeUpdate(s *ast.UpdateStmt) string {
	if s == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("UPDATE ")
	if s.Table.Name != "" {
		b.WriteString(qualifiedTableName(s.Table.Schema, s.Table.Name))
		if s.Alias != "" {
			b.WriteByte(' ')
			b.WriteString(qIdent(s.Alias))
		}
	} else if len(s.From) > 0 {
		b.WriteString(writeFromItem(s.From[0]))
	}
	b.WriteString(" SET ")
	parts := make([]string, len(s.Sets))
	for i, a := range s.Sets {
		if len(a.TupleCols) > 0 {
			cols := make([]string, len(a.TupleCols))
			for j, c := range a.TupleCols {
				cols[j] = quoteColumnRef(c)
			}
			parts[i] = "(" + strings.Join(cols, ", ") + ") = " + WriteExpr(a.Expr)
			continue
		}
		parts[i] = quoteColumnRef(a.Col) + " = " + WriteExpr(a.Expr)
	}
	b.WriteString(strings.Join(parts, ", "))
	if s.Table.Name != "" && len(s.From) > 0 {
		b.WriteString(" FROM ")
		from := make([]string, len(s.From))
		for i, f := range s.From {
			from[i] = writeFromItem(f)
		}
		b.WriteString(strings.Join(from, ", "))
	} else if s.Table.Name == "" && len(s.From) > 1 {
		b.WriteString(" FROM ")
		from := make([]string, len(s.From)-1)
		for i, f := range s.From[1:] {
			from[i] = writeFromItem(f)
		}
		b.WriteString(strings.Join(from, ", "))
	}
	if s.Where != nil {
		b.WriteString(" WHERE ")
		b.WriteString(WriteExpr(s.Where))
	}
	if len(s.Returning) > 0 {
		b.WriteString(" RETURNING ")
		items := make([]string, len(s.Returning))
		for i, it := range s.Returning {
			items[i] = writeSelectItem(it)
		}
		b.WriteString(strings.Join(items, ", "))
	}
	return b.String()
}

// writeDelete renders a DELETE statement (single-target and the
// multi-target `DELETE FROM t USING …` shape).
func writeDelete(s *ast.DeleteStmt) string {
	if s == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("DELETE FROM ")
	if s.Table.Name != "" {
		b.WriteString(qualifiedTableName(s.Table.Schema, s.Table.Name))
		if s.Alias != "" {
			b.WriteByte(' ')
			b.WriteString(qIdent(s.Alias))
		}
	} else if len(s.Using) > 0 {
		b.WriteString(writeFromItem(s.Using[0]))
	}
	if s.Table.Name != "" && len(s.Using) > 0 {
		b.WriteString(" USING ")
		using := make([]string, len(s.Using))
		for i, f := range s.Using {
			using[i] = writeFromItem(f)
		}
		b.WriteString(strings.Join(using, ", "))
	} else if s.Table.Name == "" && len(s.Using) > 1 {
		b.WriteString(" USING ")
		using := make([]string, len(s.Using)-1)
		for i, f := range s.Using[1:] {
			using[i] = writeFromItem(f)
		}
		b.WriteString(strings.Join(using, ", "))
	}
	if s.Where != nil {
		b.WriteString(" WHERE ")
		b.WriteString(WriteExpr(s.Where))
	}
	if len(s.Returning) > 0 {
		b.WriteString(" RETURNING ")
		items := make([]string, len(s.Returning))
		for i, it := range s.Returning {
			items[i] = writeSelectItem(it)
		}
		b.WriteString(strings.Join(items, ", "))
	}
	return b.String()
}

// writeMerge renders a MERGE statement (PG 15+).
func writeMerge(s *ast.MergeStmt) string {
	if s == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("MERGE INTO ")
	b.WriteString(qualifiedTableName(s.Target.Schema, s.Target.Name))
	if s.TargetAlias != "" {
		b.WriteByte(' ')
		b.WriteString(qIdent(s.TargetAlias))
	}
	b.WriteString(" USING ")
	b.WriteString(writeFromItem(s.Source))
	b.WriteString(" ON ")
	b.WriteString(WriteExpr(s.On))
	for _, m := range s.WhenMatched {
		b.WriteString(" WHEN MATCHED")
		if m.Cond != nil {
			b.WriteString(" AND ")
			b.WriteString(WriteExpr(m.Cond))
		}
		b.WriteString(" THEN ")
		b.WriteString(writeMergeAction(m))
	}
	for _, m := range s.WhenNotMatched {
		b.WriteString(" WHEN NOT MATCHED")
		if m.Cond != nil {
			b.WriteString(" AND ")
			b.WriteString(WriteExpr(m.Cond))
		}
		b.WriteString(" THEN ")
		b.WriteString(writeMergeAction(m))
	}
	return b.String()
}

// writeMergeAction renders the action body of a MERGE branch.
func writeMergeAction(m ast.MergeAction) string {
	switch m.Kind {
	case "UPDATE":
		parts := make([]string, len(m.Sets))
		for i, a := range m.Sets {
			parts[i] = quoteColumnRef(a.Col) + " = " + WriteExpr(a.Expr)
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
				cols[i] = qIdent(c)
			}
			b.WriteString(" (")
			b.WriteString(strings.Join(cols, ", "))
			b.WriteString(")")
		}
		if len(m.InsertValues) > 0 {
			vals := make([]string, len(m.InsertValues))
			for i, e := range m.InsertValues {
				vals[i] = WriteExpr(e)
			}
			b.WriteString(" VALUES (")
			b.WriteString(strings.Join(vals, ", "))
			b.WriteString(")")
		}
		return b.String()
	case "DO NOTHING":
		return "DO NOTHING"
	}
	return "/* unsupported merge action */"
}

// quoteColumnRef quotes a possibly-dotted column reference (e.g.
// `t.qty` → `"t"."qty"`) — used wherever Assign.Col / InsertCol
// values are emitted, since the parser collapses dotted column refs
// into a single string.
func quoteColumnRef(s string) string {
	if !strings.Contains(s, ".") {
		return qIdent(s)
	}
	parts := strings.Split(s, ".")
	for i, p := range parts {
		parts[i] = qIdent(p)
	}
	return strings.Join(parts, ".")
}
