package translate

import (
	"strconv"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// mysqlBoolDefault converts the DEFAULT of a MySQL/MariaDB column mapped
// to PG BOOLEAN (TINYINT(1), BOOL, BIT(1) — see types_map_mysql.go).
// SHOW CREATE TABLE prints those defaults as integers (`DEFAULT 0`,
// `DEFAULT 1`), bit literals (`DEFAULT b'1'`) or quoted digits
// (`DEFAULT '1'`), and PG rejects an integer default on a boolean column
// ("default expression is of type integer", SQLSTATE 42804) at
// create_ddl.
//
// A literal becomes FALSE when its numeric value is 0 and TRUE otherwise
// (MySQL's truth rule); NULL and TRUE / FALSE are kept. Any other
// expression is converted the way the value would be: CAST(CAST(e AS
// integer) AS boolean). A literal that is not a number is kept as is, so
// PG rejects it loudly rather than squishy guessing a value.
//
// Typed AST pass: the only text read is the digits of a single literal
// token.
func mysqlBoolDefault(e ast.Expr) ast.Expr {
	inner := unwrapParens(e)
	lit, ok := inner.(*ast.Literal)
	if !ok {
		if inner == nil {
			return e
		}
		return &ast.CastExpr{
			Expr: &ast.CastExpr{Expr: e, Type: &ast.PGType{Name: "integer"}, P: e.Pos()},
			Type: &ast.PGType{Name: "boolean"}, P: e.Pos()}
	}
	var zero, parsed bool
	switch lit.Kind {
	case "null", "bool":
		return lit
	case "number", "string":
		if f, err := strconv.ParseFloat(lit.Text, 64); err == nil {
			zero, parsed = f == 0, true
		}
	case "bit":
		if v, err := strconv.ParseUint(lit.Text, 2, 64); err == nil {
			zero, parsed = v == 0, true
		}
	case "hex":
		if v, err := strconv.ParseUint(lit.Text, 16, 64); err == nil {
			zero, parsed = v == 0, true
		}
	}
	if !parsed {
		return e
	}
	text := "TRUE"
	if zero {
		text = "FALSE"
	}
	return &ast.Literal{Kind: "bool", Text: text, P: lit.P}
}
