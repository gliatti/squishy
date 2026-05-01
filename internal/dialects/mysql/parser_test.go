package mysql

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

func TestParseCreateTableBasic(t *testing.T) {
	src := "CREATE TABLE users (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT, email VARCHAR(255) NOT NULL, PRIMARY KEY (id), UNIQUE KEY uq_email (email));"
	stmts, errs := Parse(src)
	require.Empty(t, errs, "errors: %v", errs)
	require.Len(t, stmts, 1)
	ct, ok := stmts[0].(*ast.CreateTable)
	require.True(t, ok)
	require.Equal(t, "users", ct.Name)
	require.Len(t, ct.Columns, 2)
	require.Equal(t, "id", ct.Columns[0].Name)
	require.True(t, ct.Columns[0].AutoInc)
	require.True(t, ct.Columns[0].NotNull)
	// Constraints: PK + UQ
	require.Len(t, ct.Constraints, 2)
	_, isPK := ct.Constraints[0].(*ast.PKConstraint)
	_, isUQ := ct.Constraints[1].(*ast.UQConstraint)
	require.True(t, isPK)
	require.True(t, isUQ)
}

func TestParseEnumAndJson(t *testing.T) {
	src := "CREATE TABLE o (id INT, status ENUM('a','b','c') NOT NULL DEFAULT 'a', meta JSON);"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	ct := stmts[0].(*ast.CreateTable)
	require.Len(t, ct.Columns, 3)
	et, ok := ct.Columns[1].Type.(*ast.EnumType)
	require.True(t, ok)
	require.Equal(t, []string{"a", "b", "c"}, et.Values)
	_, jok := ct.Columns[2].Type.(*ast.JSONType)
	require.True(t, jok)
}

func TestParseTriggerRawBody(t *testing.T) {
	src := `
		DELIMITER //
		CREATE TRIGGER trg AFTER INSERT ON orders FOR EACH ROW BEGIN
		  INSERT INTO audit(oid) VALUES (NEW.id);
		END //
		DELIMITER ;
	`
	stmts, errs := Parse(src)
	require.Empty(t, errs, "errors: %v", errs)
	require.Len(t, stmts, 1)
	tg, ok := stmts[0].(*ast.CreateTrigger)
	require.True(t, ok)
	require.Equal(t, "AFTER", tg.Time)
	require.Equal(t, "INSERT", tg.Event)
	require.Contains(t, tg.Body, "INSERT INTO audit")
}

func TestParseProcedureParams(t *testing.T) {
	src := `CREATE PROCEDURE p(IN a INT, INOUT b INT, OUT c INT) BEGIN SET c=a+b; END;`
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	p := stmts[0].(*ast.CreateProcedure)
	require.Len(t, p.Params, 3)
	require.Equal(t, "IN", p.Params[0].Direction)
	require.Equal(t, "INOUT", p.Params[1].Direction)
	require.Equal(t, "OUT", p.Params[2].Direction)
}

func TestTokenizeDelimiterSwap(t *testing.T) {
	toks := Tokenize(`
		DELIMITER $$
		CREATE PROCEDURE p() BEGIN SELECT 1; END $$
		DELIMITER ;
	`)
	// quick sanity: the DELIMITER body has a SELECT.
	found := false
	for _, tk := range toks {
		if strings.ToUpper(tk.Lit) == "SELECT" {
			found = true
		}
	}
	require.True(t, found)
}
