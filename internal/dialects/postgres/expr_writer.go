package postgres

import (
	"strings"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// expr_writer.go — PG-side AST → SQL rendering for expression nodes.
//
// Phase 2 of the AST-only refactor extracts the pure rendering layer
// from translator.rawExpr into the postgres package, mirroring the
// existing Oracle-side parser/lexer locality (each dialect package
// owns its parser and writer; the translator orchestrates between
// them).
//
// WriteExpr is "pure" in the sense that it does not perform Oracle-
// or MySQL-specific name remaps (mapFunction, MapType): every
// translation pass is the responsibility of a Phase 3 AST visitor
// run *before* the writer. When a visitor leaves a dialect-specific
// node in place (e.g. *ast.OuterJoinHint, *ast.CursorAttr) the writer
// emits a faithful but PG-invalid form so the failure surfaces at
// apply time rather than being silently swallowed — this keeps the
// "no fall-through invalid SQL" contract from CLAUDE.md.

// WriteExpr renders an ast.Expr as a PG-compatible SQL fragment. Pure
// AST → text — no translation, no name remaps, no schema rewriting.
//
// Returns "" for nil input. Unknown node types fall through to "" as
// well rather than panicking; the Phase 3 visitor that should have
// rewritten them surfaces the issue via prerequisites instead.
func WriteExpr(e ast.Expr) string {
	if e == nil {
		return ""
	}
	switch x := e.(type) {
	case *ast.Literal:
		return writeLiteral(x)
	case *ast.Ident:
		return writeIdent(x)
	case *ast.BinaryExpr:
		op := x.Op
		// Oracle DIV → PG `/` is a translation step that should be
		// owned by a visitor; until that visitor lands, fold the
		// keyword form here so DIV-bearing expressions emit valid PG.
		if strings.EqualFold(op, "DIV") {
			op = "/"
		}
		return WriteExpr(x.Lhs) + " " + op + " " + WriteExpr(x.Rhs)
	case *ast.UnaryExpr:
		if isWordOp(x.Op) {
			return x.Op + " " + WriteExpr(x.Rhs)
		}
		return x.Op + WriteExpr(x.Rhs)
	case *ast.ParenExpr:
		return "(" + WriteExpr(x.Inner) + ")"
	case *ast.FuncCall:
		return writeFuncCall(x)
	case *ast.CaseExpr:
		return writeCase(x)
	case *ast.BetweenExpr:
		op := "BETWEEN"
		if x.Not {
			op = "NOT BETWEEN"
		}
		return WriteExpr(x.Expr) + " " + op + " " + WriteExpr(x.Low) + " AND " + WriteExpr(x.High)
	case *ast.InExpr:
		return writeIn(x)
	case *ast.ExistsExpr:
		op := "EXISTS"
		if x.Not {
			op = "NOT EXISTS"
		}
		return op + " (" + WriteSelectStmt(x.Subquery) + ")"
	case *ast.SubqueryExpr:
		return "(" + WriteSelectStmt(x.Stmt) + ")"
	case *ast.WindowedAgg:
		return writeWindowedAgg(x)
	case *ast.OuterJoinHint:
		// Oracle (+) — a Phase 3 visitor rewrites the enclosing
		// SelectStmt's Where into ANSI LEFT JOINs and strips this
		// hint. When it still reaches the writer the visitor failed
		// or wasn't run, so emit the inner expression and let the
		// translator's prerequisite layer surface the diagnostic.
		return WriteExpr(x.Inner)
	case *ast.CastExpr:
		// MapType belongs in the translator (it knows the source
		// dialect's type taxonomy). When CastExpr survives into the
		// writer with a typed Type that isn't already PG-shaped, the
		// caller is expected to have run the type-mapping visitor
		// first. The writer falls back to TEXT to keep emitted SQL
		// parseable in the worst case.
		typ := writeType(x.Type)
		if typ == "" {
			typ = "text"
		}
		return "CAST(" + WriteExpr(x.Expr) + " AS " + typ + ")"
	case *ast.IntervalLit:
		val := x.Value
		if !strings.HasPrefix(val, "'") {
			val = "'" + val + "'"
		}
		if x.Unit == "" {
			return "INTERVAL " + val
		}
		return "INTERVAL " + val + " " + x.Unit
	case *ast.CursorAttr:
		// Oracle cursor attributes have no direct PG inline
		// equivalent — Phase 3's cursor-attr visitor rewrites them
		// into FOUND / NOT FOUND / pg_cursors lookups / GET
		// DIAGNOSTICS TODOs. When the visitor hasn't run, faithfully
		// echo the source form so the failure surfaces at apply time.
		return x.Cursor + "%" + x.Attr
	case *ast.SequenceRef:
		// Faithful Oracle echo when the visitor hasn't substituted
		// — Phase 3.4's VisitOracleSequenceRef rewrites this node to
		// `nextval('seq')` / `currval('seq')` (an *ast.FuncCall) so
		// the writer never reaches this branch in the canonical
		// pipeline. Kept defensively to surface unrewritten cases as
		// PG syntax errors instead of silent miscompiles.
		name := x.Name
		if x.Schema != "" {
			name = x.Schema + "." + name
		}
		return name + "." + x.Op
	case *ast.RawExpr:
		// Legacy escape hatch — kept for the transition window. Once
		// every parser site emits typed nodes (the Phase 1 sweep) and
		// every visitor produces typed substitutions, RawExpr can be
		// removed.
		return x.Text
	}
	return ""
}

// writeLiteral renders a *ast.Literal in PG-canonical form.
func writeLiteral(l *ast.Literal) string {
	if l == nil {
		return ""
	}
	switch l.Kind {
	case "string":
		return sqlString(l.Text)
	case "null":
		return "NULL"
	case "bool":
		// Booleans round-trip TRUE/FALSE in both Oracle (PL/SQL) and
		// PG; the lexer normalises case to upper/lower per source so
		// emit verbatim — visitors that care about casing rebuild.
		return l.Text
	case "hex", "bit":
		// PG accepts E'\\xNN…' and B'1010' verbatim; preserve the
		// source form. mysql-style x'…' / 0x… is normalised by the
		// MySQL parser before reaching the AST.
		return l.Text
	}
	// "number" and unknown kinds — emit verbatim text.
	return l.Text
}

// writeIdent renders a *ast.Ident as PG dotted form. Each part is
// double-quoted unconditionally to round-trip case-sensitive Oracle
// idents (which the parser surfaced as upper-case). The identifier-
// case-folding pass (lower unquoted Oracle UPPER → lower) lives on
// the Phase 3 normalisation visitor — it rewrites the Ident.Parts in
// place so the writer can stay dialect-neutral.
func writeIdent(id *ast.Ident) string {
	if id == nil || len(id.Parts) == 0 {
		return ""
	}
	parts := make([]string, 0, len(id.Parts))
	for _, p := range id.Parts {
		if p == "*" {
			parts = append(parts, "*")
			continue
		}
		// Trigger row pseudocols (NEW, OLD, TG_*) must NOT be
		// double-quoted in PG plpgsql — quoting changes the meaning
		// from the trigger record reference to a case-sensitive
		// relation lookup. The Phase 3 row-alias visitor rewrites
		// REFERENCING aliases (NR → NEW) at the AST level so that
		// branch reaches the writer with literal NEW/OLD parts; this
		// guard keeps them unquoted on the way out.
		if isPLpgSQLPseudoCol(p) {
			parts = append(parts, p)
			continue
		}
		parts = append(parts, qIdent(p))
	}
	return strings.Join(parts, ".")
}

// writeFuncCall renders a *ast.FuncCall. Niladic Oracle pseudocolumns
// (SYSDATE, ROWNUM, …) are emitted without trailing parens because PG
// rejects them. Function-name remapping (mapFunction in the legacy
// translator) is intentionally NOT performed here — that's a Phase 3
// visitor's job.
func writeFuncCall(fc *ast.FuncCall) string {
	if fc == nil {
		return ""
	}
	args := make([]string, 0, len(fc.Args))
	for _, a := range fc.Args {
		args = append(args, WriteExpr(a))
	}
	if len(args) == 0 {
		switch strings.ToUpper(fc.Name) {
		case "SYSDATE", "SYSTIMESTAMP", "CURRENT_TIMESTAMP", "CURRENT_DATE",
			"LOCALTIMESTAMP", "UID", "ROWNUM", "LEVEL":
			return fc.Name
		}
	}
	return fc.Name + "(" + strings.Join(args, ", ") + ")"
}

// writeCase renders simple- and searched-CASE expressions identically
// (PG accepts both; the discriminator is whether Operand is nil).
func writeCase(c *ast.CaseExpr) string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("CASE")
	if c.Operand != nil {
		b.WriteByte(' ')
		b.WriteString(WriteExpr(c.Operand))
	}
	for _, w := range c.Whens {
		b.WriteString(" WHEN ")
		b.WriteString(WriteExpr(w.Match))
		b.WriteString(" THEN ")
		b.WriteString(WriteExpr(w.Then))
	}
	if c.Else != nil {
		b.WriteString(" ELSE ")
		b.WriteString(WriteExpr(c.Else))
	}
	b.WriteString(" END")
	return b.String()
}

// writeIn renders both list-form `e IN (a, b, c)` and subquery-form
// `e IN (SELECT …)`.
func writeIn(in *ast.InExpr) string {
	op := "IN"
	if in.Not {
		op = "NOT IN"
	}
	var rhs string
	if in.Subquery != nil {
		rhs = "(" + WriteSelectStmt(in.Subquery) + ")"
	} else {
		parts := make([]string, len(in.List))
		for i, e := range in.List {
			parts[i] = WriteExpr(e)
		}
		rhs = "(" + strings.Join(parts, ", ") + ")"
	}
	return WriteExpr(in.Expr) + " " + op + " " + rhs
}

// writeWindowedAgg renders `agg(...) WITHIN GROUP (ORDER BY ...) OVER (...)`.
func writeWindowedAgg(w *ast.WindowedAgg) string {
	var b strings.Builder
	b.WriteString(WriteExpr(w.Func))
	if len(w.Within) > 0 {
		b.WriteString(" WITHIN GROUP (ORDER BY ")
		items := make([]string, len(w.Within))
		for i, oi := range w.Within {
			items[i] = writeOrderItem(oi)
		}
		b.WriteString(strings.Join(items, ", "))
		b.WriteString(")")
	}
	if w.Over != nil {
		b.WriteString(" OVER ")
		b.WriteString(writeWindowSpec(w.Over))
	}
	return b.String()
}

// writeOrderItem renders one ORDER BY element: expr [DESC] [NULLS …].
func writeOrderItem(oi ast.OrderItem) string {
	out := WriteExpr(oi.Expr)
	if oi.Desc {
		out += " DESC"
	}
	if oi.NullsLast != nil {
		if *oi.NullsLast {
			out += " NULLS LAST"
		} else {
			out += " NULLS FIRST"
		}
	}
	return out
}

// writeWindowSpec renders a *ast.WindowSpec: PARTITION BY / ORDER BY /
// frame.
func writeWindowSpec(w *ast.WindowSpec) string {
	var b strings.Builder
	b.WriteString("(")
	first := true
	if len(w.PartitionBy) > 0 {
		b.WriteString("PARTITION BY ")
		parts := make([]string, len(w.PartitionBy))
		for i, e := range w.PartitionBy {
			parts[i] = WriteExpr(e)
		}
		b.WriteString(strings.Join(parts, ", "))
		first = false
	}
	if len(w.OrderBy) > 0 {
		if !first {
			b.WriteByte(' ')
		}
		b.WriteString("ORDER BY ")
		items := make([]string, len(w.OrderBy))
		for i, oi := range w.OrderBy {
			items[i] = writeOrderItem(oi)
		}
		b.WriteString(strings.Join(items, ", "))
		first = false
	}
	if w.Frame != "" {
		if !first {
			b.WriteByte(' ')
		}
		b.WriteString(w.Frame)
	}
	b.WriteString(")")
	return b.String()
}

// writeType renders an ast.DataType to a PG type string. The pure
// renderer only handles already-PG types (the translator's MapType is
// the source-of-truth for cross-dialect mapping). Non-PG types fall
// through to "" so the caller can substitute a default.
func writeType(t ast.DataType) string {
	if t == nil {
		return ""
	}
	switch x := t.(type) {
	case *ast.UserDefinedType:
		// User-defined Oracle type — best-effort emit the name.
		return x.Name
	}
	return ""
}

// isWordOp reports whether op is a word-shaped unary operator that
// needs a trailing space in front of its operand.
func isWordOp(op string) bool {
	switch strings.ToUpper(op) {
	case "NOT", "PRIOR", "CONNECT_BY_ROOT":
		return true
	}
	return false
}

// isPLpgSQLPseudoCol reports whether p refers to one of PG plpgsql's
// trigger-context special names that must remain unquoted.
//
// Match is case-insensitive and tolerates the Oracle-style `:`
// bind-variable prefix that survives parsing as part of the Ident
// part text. Both `:NEW` and `NEW` round-trip unquoted.
func isPLpgSQLPseudoCol(p string) bool {
	if p == "" {
		return false
	}
	if p[0] == ':' {
		p = p[1:]
	}
	switch strings.ToUpper(p) {
	case "NEW", "OLD",
		"TG_OP", "TG_NAME", "TG_WHEN", "TG_LEVEL",
		"TG_RELID", "TG_TABLE_NAME", "TG_TABLE_SCHEMA",
		"TG_NARGS", "TG_ARGV":
		return true
	}
	return false
}

// sqlString returns a PG-quoted single-quoted string. Backslashes are
// not escaped — PG's standard_conforming_strings is on by default,
// matching the source data faithfully. Empty Text rounds to the empty
// string '', not to NULL.
//
// Mirrors the package-private sqlLit in writer.go but kept separate
// because the expression writer tests cover string-escape edge cases
// independently of the DDL writer.
func sqlString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// WriteSelectStmt is a forward declaration filled in by stmt_writer.go.
// Defined here as an indirection so expr_writer can emit subqueries
// without a circular dependency on the statement writer file.
var WriteSelectStmt = func(s *ast.SelectStmt) string { return "" }
