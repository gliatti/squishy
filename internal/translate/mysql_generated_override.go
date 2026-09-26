package translate

import (
	"fmt"
	"strings"

	pgast "gitlab.com/dalibo/squishy/internal/dialects/postgres"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// Per-column generation expression overrides (MySQL/MariaDB sources).
//
// squishy refuses to translate a generated column whose text conversion
// it cannot prove identical in PostgreSQL (mysql_generated.go): the column
// becomes a plain column copied from the source. The user can then give
// the PostgreSQL generation expression of that column themselves; it is
// stored on the migration (squishy.migrations.generated_overrides, kept
// across re-plans) and handed to the translator through
// Options.GeneratedOverrides.
//
// Validation of the override text. internal/dialects/postgres has no
// expression parser (its Dialect.Parse is a stub: PostgreSQL is only a
// target), and the MySQL expression parser cannot validate PostgreSQL
// text: its lexical rules differ (`"` delimits a string, backticks quote
// identifiers, || may be OR, …), so it would reject or — worse — silently
// misread valid PostgreSQL. Per the AST-only rule the text is neither
// matched by a regex nor split: it is carried as the AST's verbatim node
// (*ast.RawExpr, rendered as is by rawExpr) into PGGenerated.Expr, and
// the pgast writer emits it inside `GENERATED ALWAYS AS (…) STORED`.
//
// Two checks guard it, in this order:
//
//  1. Structure, here, on every plan, whatever wrote the stored value.
//     create_ddl runs the DDL script through the simple query protocol,
//     which executes several statements, so a text that closes the
//     clause (`'x') STORED); DROP SCHEMA squishy CASCADE; …`), starts a
//     second statement or comments out the rest would run arbitrary SQL
//     on the target. pgast.CheckExprFragment — a hand-rolled lexical
//     scanner (the lexer exception of the AST-only rule), not a parser —
//     proves the text stays one parenthesised expression: non-empty,
//     balanced parentheses, terminated strings and quoted identifiers,
//     no ';', no comment, no dollar quote. An override that fails it is
//     refused: blocking error (table.generated_override_invalid), and the
//     column gets the treatment it would have without an override (a
//     column squishy refuses stays a plain column copied from the
//     source). The PUT endpoint runs the same check
//     (ValidateGeneratedOverrides) before storing anything.
//  2. Semantics, by PostgreSQL — the only authoritative PostgreSQL
//     parser: syntax, types and immutability, eagerly when the override
//     is saved (the PUT endpoint's rolled-back CREATE TABLE probe, which
//     only ever sees texts that passed check 1) and definitively when
//     create_ddl creates the table.
//
// Two overrides of the same table.column are ambiguous: neither applies,
// and the column is refused like an invalid override.
//
// An override wins over every other treatment of the column: the CONCAT
// → || rewrite, the text-conversion refusal and the parse-error refusal
// (the user states the expression; the parsed source expression is only
// quoted in the explanation). The column keeps its generation on
// PostgreSQL, so it is not copied (PG computes it on the current table;
// the history table of an emulated system-versioned table still stores
// the source values, like any generated column).

// GeneratedOverride is the PostgreSQL generation expression a user gives
// to one MySQL/MariaDB generated column.
type GeneratedOverride struct {
	// Table is the source table name, compared exactly (MySQL table
	// names are case-sensitive with lower_case_table_names=0, and the
	// plan keeps the source spelling).
	Table string `json:"table"`
	// Column is the source column name, compared case-insensitively
	// (MySQL column names are case-insensitive).
	Column string `json:"column"`
	// Expr is the PostgreSQL expression, emitted verbatim once it passes
	// ValidateGeneratedOverrideExpr.
	Expr string `json:"expression"`
}

// ValidateGeneratedOverrideExpr checks that expr can be embedded as the
// generation expression of a column without leaving its clause (see the
// comment at the top of this file). It does not check that expr is a
// valid PostgreSQL expression: PostgreSQL does.
func ValidateGeneratedOverrideExpr(expr string) error {
	return pgast.CheckExprFragment(expr)
}

// ValidateGeneratedOverrides checks a whole set of overrides before it is
// stored: a table and a column on each, a structurally safe expression
// (ValidateGeneratedOverrideExpr), and at most one override per
// table.column (tables compared exactly, columns case-insensitively, as
// the translator matches them).
func ValidateGeneratedOverrides(ovs []GeneratedOverride) error {
	for i, ov := range ovs {
		if ov.Table == "" || ov.Column == "" {
			return fmt.Errorf("override %d: table and column are required", i)
		}
		if err := ValidateGeneratedOverrideExpr(ov.Expr); err != nil {
			return fmt.Errorf("override %d (%s.%s): %w", i, ov.Table, ov.Column, err)
		}
		for _, prev := range ovs[:i] {
			if prev.Table == ov.Table && strings.EqualFold(prev.Column, ov.Column) {
				return fmt.Errorf("override %d (%s.%s): duplicate override of the same column", i, ov.Table, ov.Column)
			}
		}
	}
	return nil
}

// generatedOverrides returns the indexes in o.GeneratedOverrides of the
// overrides of table.column (normally zero or one).
func (o Options) generatedOverrides(table, column string) []int {
	var out []int
	for i, ov := range o.GeneratedOverrides {
		if ov.Table == table && strings.EqualFold(ov.Column, column) {
			out = append(out, i)
		}
	}
	return out
}

// takeGeneratedOverride looks up the override of tbl.Columns[idx] (source
// table.column), marks every match in used, and either applies it (true:
// the column is done) or, when it is duplicated or structurally unsafe,
// refuses it as a blocking error and returns false so the column gets its
// usual treatment.
func (t *translator) takeGeneratedOverride(tbl *PGTable, idx int, table, column, obj, src string, used []bool) bool {
	matches := t.opt.generatedOverrides(table, column)
	if len(matches) == 0 {
		return false
	}
	for _, i := range matches {
		used[i] = true
	}
	if len(matches) > 1 {
		t.refuseGeneratedOverride(obj, src, fmt.Sprintf("%d generation expression overrides target %s (duplicate override): none is applied, keep exactly one", len(matches), obj))
		return false
	}
	ov := t.opt.GeneratedOverrides[matches[0]]
	if err := ValidateGeneratedOverrideExpr(ov.Expr); err != nil {
		t.refuseGeneratedOverride(obj, src, "the generation expression override of "+obj+" is refused and not applied: "+err.Error()+
			". An override must be one PostgreSQL expression that stays inside GENERATED ALWAYS AS (…): non-empty, balanced parentheses, terminated strings and quoted identifiers, no ';', no comment, no dollar quote")
		return false
	}
	t.applyGeneratedOverride(tbl, idx, obj, src, ov)
	return true
}

// refuseGeneratedOverride reports an override that is not applied as a
// blocking error (table.generated_override_invalid). The override text is
// deliberately not echoed in the DDL; it only appears in the message.
func (t *translator) refuseGeneratedOverride(obj, src, msg string) {
	msg += ". The column is translated as if it had no override (a generated column squishy refuses stays a plain column whose values are copied from the source); fix or delete the override, then re-plan."
	t.res.Explanations = append(t.res.Explanations, Explanation{
		Object: obj,
		Source: "GENERATED ALWAYS AS (" + src + ")",
		Target: "(generation expression override refused)",
		Reason: msg,
		Level:  "error",
	})
	t.warnSev(obj, "table.generated_override_invalid", msg, SeverityBlocking)
}

// applyGeneratedOverride gives tbl.Columns[idx] the user's generation
// expression and records it in an info explanation. src is the rendered
// source generation expression. ov.Expr has passed
// ValidateGeneratedOverrideExpr.
func (t *translator) applyGeneratedOverride(tbl *PGTable, idx int, obj, src string, ov GeneratedOverride) {
	tbl.Columns[idx].Generated = &PGGenerated{Expr: rawExpr(&ast.RawExpr{Text: ov.Expr}), Stored: true}
	t.res.Explanations = append(t.res.Explanations, Explanation{
		Object: obj,
		Source: "GENERATED ALWAYS AS (" + src + ")",
		Target: "GENERATED ALWAYS AS (" + ov.Expr + ") STORED (user override)",
		Reason: "The generation expression is the PostgreSQL expression provided as an override for this column; squishy only checks that it stays one expression inside the clause and does not translate or verify it (PostgreSQL validates syntax, types and immutability when the table is created). Check it prints exactly what MySQL stores.",
		Level:  "info",
	})
}

// reportUnusedGeneratedOverrides reports every override that matched no
// MySQL/MariaDB generated column of the plan (used[i] is false): the
// column was renamed or dropped, is no longer generated on the source,
// or the table / column name is misspelled.
func (t *translator) reportUnusedGeneratedOverrides(used []bool) {
	for i, ov := range t.opt.GeneratedOverrides {
		if used[i] {
			continue
		}
		obj := ov.Table + "." + ov.Column
		msg := "the generation expression override of " + obj + " (" + ov.Expr + ") matches no generated column of the source schema: the table or column does not exist (table names are case-sensitive), or the column is not generated on the source. It is ignored; delete it or fix the table / column name."
		t.warnSev(obj, "table.generated_override_unused", msg, SeverityInfo)
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: obj,
			Source: "override GENERATED ALWAYS AS (" + ov.Expr + ")",
			Target: "(ignored)",
			Reason: msg,
			Level:  "info",
		})
	}
}
