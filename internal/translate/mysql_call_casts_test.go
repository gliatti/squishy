package translate

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects"
	pgast "gitlab.com/dalibo/squishy/internal/dialects/postgres"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// A view calling a migrated stored function with an INT UNSIGNED column
// (BIGINT on PG) for an INT parameter failed on PG with "function
// last_first_name(bigint) does not exist": the call-argument casts only
// ran on routine and trigger bodies. They now run on view bodies too.
func TestMySQLCallArgCasts_View(t *testing.T) {
	res := translateMySQL(t, "CREATE TABLE `employees` (\n"+
		"  `employee_id` int unsigned NOT NULL,\n"+
		"  PRIMARY KEY (`employee_id`)\n"+
		") ENGINE=InnoDB;\n"+
		"CREATE FUNCTION `last_first_name`(empid INT) RETURNS varchar(255) DETERMINISTIC\n"+
		"BEGIN\n  RETURN CONCAT('Employee: ', empid);\nEND;\n"+
		"CREATE VIEW `v_names` AS SELECT `last_first_name`(`employees`.`employee_id`) AS `n` FROM `employees`;")
	require.Empty(t, res.Warnings)
	v := onlyView(t, res)
	t.Logf("view:\n%s", v.DDL)
	require.Contains(t, v.DDL, `(CAST("employees"."employee_id" AS integer)) AS "n"`)
}

// Non-integer parameters: MySQL converts the argument to the parameter
// type, PG only applies implicit casts during function resolution — an
// integer passed to a VARCHAR parameter, or an integer expression to a
// TINYINT(1) → BOOLEAN parameter, did not resolve. Every argument is now
// cast to its parameter's PG type (booleans through integer, since PG
// has no int8 → boolean cast); untyped literals and NULL are left alone.
func TestMySQLCallArgCasts_NonIntegerParams(t *testing.T) {
	res := translateMySQL(t,
		"CREATE PROCEDURE `p`(IN s VARCHAR(10), IN c CHAR(3), IN b TINYINT(1), IN d DATE, IN x DOUBLE, IN m DECIMAL(8,2), IN j JSON, IN r FLOAT)\n"+
			"BEGIN\n  SELECT 1;\nEND;\n"+
			"CREATE PROCEDURE `caller`(IN n BIGINT, IN ts DATETIME, IN q DOUBLE)\n"+
			"BEGIN\n"+
			"  CALL p(n, n, n > 0, ts, q, q, 'x', q);\n"+
			"  CALL p('lit', 'abc', 1, '2024-01-01', 2, 1.5, NULL, 3);\n"+
			"  CALL p(42, 7, TRUE, ts, n, n, 'y', 2.5);\n"+
			"END;")
	var caller string
	for _, r := range res.Plan.Routines {
		if r.Name == "caller" {
			caller = r.DDL
		}
	}
	t.Logf("caller:\n%s", caller)
	require.Contains(t, caller,
		`CALL "p"(CAST("n" AS varchar), CAST("n" AS bpchar), CAST(CAST("n" > 0 AS integer) AS boolean), CAST("ts" AS date), CAST("q" AS double precision), CAST("q" AS numeric), 'x', CAST("q" AS real));`)
	// Untyped literals / NULL resolve to any type; int4 and numeric
	// literals widen implicitly to numeric / float; an int literal is
	// cast to boolean directly (int4 → boolean exists).
	require.Contains(t, caller,
		`CALL "p"('lit', 'abc', CAST(1 AS boolean), '2024-01-01', 2, 1.5, NULL, 3);`)
	require.Contains(t, caller,
		`CALL "p"(CAST(42 AS varchar), CAST(7 AS bpchar), TRUE, CAST("ts" AS date), CAST("n" AS double precision), CAST("n" AS numeric), 'y', 2.5);`)
}

// A parameter with no faithful conversion (SET → text[]) is reported,
// never passed silently.
func TestMySQLCallArgCasts_UnsupportedParamWarns(t *testing.T) {
	res := translateMySQL(t,
		"CREATE PROCEDURE `p`(IN flags SET('a','b'))\nBEGIN\n  SELECT 1;\nEND;\n"+
			"CREATE PROCEDURE `caller`(IN v VARCHAR(10))\nBEGIN\n  CALL p(v);\n  CALL p(NULL);\nEND;")
	var msgs []string
	for _, w := range res.Warnings {
		if w.Object == "procedure.caller" {
			msgs = append(msgs, w.Message)
		}
	}
	require.Len(t, msgs, 1, "%+v", res.Warnings)
	require.Contains(t, msgs[0], "call to procedure p: argument 1 not converted")
	require.Contains(t, msgs[0], "SET parameter")
}

// Table-level expressions go through rawExpr: the PG-shaped cast target
// (an *ast.PGType) must be rendered verbatim, not mapped as a source
// type. MySQL rejects stored functions in CHECK / generated columns,
// so this is a defensive path: the casts keep it uniform.
func TestMySQLCallArgCasts_TableExpressions(t *testing.T) {
	sigs := &mysqlRoutineSigs{funcs: map[string][]mysqlParamConv{"f": {{Target: "integer"}}}, procs: map[string][]mysqlParamConv{}}
	tr := &translator{opt: Options{SourceKind: dialects.KindMySQL, TargetSchema: "mig"}, res: &Result{}, mysqlSigs: sigs}
	e := &ast.BinaryExpr{Op: ">", Lhs: ast.BuildFuncCall("f", ast.BuildIdent("col")), Rhs: &ast.Literal{Kind: "number", Text: "0"}}
	require.Equal(t, `f(CAST("col" AS integer)) > 0`, rawExpr(tr.castMySQLDDLExpr("table.t", e)))
	require.Empty(t, tr.res.Warnings)

	// An Oracle user-defined type keeps its MapType rendering.
	oe := &ast.CastExpr{Expr: ast.BuildIdent("x"), Type: &ast.UserDefinedType{Name: "MY_T"}}
	require.Equal(t, `CAST("x" AS TEXT)`, rawExpr(oe))
}

// mysqlParamConvFor mirrors mapMySQLType: each typed parameter must map
// to the base name of the PG column type the mapper emits.
func TestMySQLParamConvMatchesTypeMap(t *testing.T) {
	cases := []struct {
		typ    ast.DataType
		pg     string // mapMySQLType
		target string // mysqlParamConvFor
	}{
		{&ast.IntType{Name: "TINYINT", HasWidth: true, Width: 1}, "BOOLEAN", "boolean"},
		{&ast.IntType{Name: "TINYINT"}, "SMALLINT", "smallint"},
		{&ast.IntType{Name: "TINYINT", Unsigned: true}, "SMALLINT", "smallint"},
		{&ast.IntType{Name: "SMALLINT"}, "SMALLINT", "smallint"},
		{&ast.IntType{Name: "SMALLINT", Unsigned: true}, "INTEGER", "integer"},
		{&ast.IntType{Name: "MEDIUMINT"}, "INTEGER", "integer"},
		{&ast.IntType{Name: "MEDIUMINT", Unsigned: true}, "INTEGER", "integer"},
		{&ast.IntType{Name: "INT"}, "INTEGER", "integer"},
		{&ast.IntType{Name: "INT", Unsigned: true}, "BIGINT", "bigint"},
		{&ast.IntType{Name: "BIGINT"}, "BIGINT", "bigint"},
		{&ast.IntType{Name: "BIGINT", Unsigned: true}, "NUMERIC(20,0)", "numeric"},
		{&ast.FloatType{Name: "FLOAT"}, "REAL", "real"},
		{&ast.FloatType{Name: "DOUBLE"}, "DOUBLE PRECISION", "double precision"},
		{&ast.DecimalType{HasPrec: true, Precision: 8, Scale: 2}, "NUMERIC(8,2)", "numeric"},
		{&ast.BitType{Width: 1}, "BOOLEAN", "boolean"},
		{&ast.CharType{Name: "VARCHAR", HasLength: true, Length: 10}, "VARCHAR(10)", "varchar"},
		{&ast.CharType{Name: "CHAR", HasLength: true, Length: 3}, "CHAR(3)", "bpchar"},
		{&ast.TextType{Name: "TEXT"}, "TEXT", "text"},
		{&ast.JSONType{}, "JSONB", "jsonb"},
		{&ast.DateType{}, "DATE", "date"},
		{&ast.TimeType{}, "TIME", "time"},
		{&ast.DateTimeType{}, "TIMESTAMP", "timestamp"},
		{&ast.TimestampType{}, "TIMESTAMPTZ", "timestamptz"},
		{&ast.YearType{}, "SMALLINT", "smallint"},
	}
	for _, c := range cases {
		require.Equal(t, c.pg, MapType(dialects.KindMySQL, c.typ, "c", Caps{}).PG, c.typ.TypeName())
		require.Equal(t, c.target, mysqlParamConvFor(c.typ, Caps{}).Target, c.typ.TypeName())
	}
}

// Only an int4-sized integer literal is cast straight to boolean (PG has
// int4 → boolean only). A decimal or int8-sized literal goes through
// integer like a non-literal argument: `CAST(1.5 AS boolean)` and
// `CAST(9999999999 AS boolean)` have no PG cast.
func TestMySQLArgCast_BooleanLiterals(t *testing.T) {
	conv := mysqlParamConv{Target: "boolean", Bool: true}
	render := func(e ast.Expr) string { return rawExpr(mysqlArgCast(e, conv)) }
	require.Equal(t, `CAST(1 AS boolean)`, render(&ast.Literal{Kind: "number", Text: "1"}))
	require.Equal(t, `CAST(CAST(1.5 AS integer) AS boolean)`, render(&ast.Literal{Kind: "number", Text: "1.5"}))
	require.Equal(t, `CAST(CAST(9999999999 AS integer) AS boolean)`, render(&ast.Literal{Kind: "number", Text: "9999999999"}))
	require.Equal(t, `CAST(CAST("x" AS integer) AS boolean)`, render(ast.BuildIdent("x")))
}

// Cast targets synthesised by the MySQL passes are *ast.PGType nodes,
// rendered verbatim whatever the dialect. A source-dialect
// UserDefinedType whose name is a lowercase PG built-in (an Oracle quoted
// "date" / "vector" / "text" type) is not mistaken for one: it still goes
// through the type mapper, like any other user-defined type.
func TestCastTarget_PGTypeVsUserDefinedType(t *testing.T) {
	for _, name := range []string{"date", "vector", "text", "integer"} {
		pg := &ast.CastExpr{Expr: ast.BuildIdent("x"), Type: &ast.PGType{Name: name}}
		require.Equal(t, `CAST("x" AS `+name+`)`, rawExpr(pg))
		for _, k := range []dialects.Kind{dialects.KindMySQL, dialects.KindOracle, dialects.KindDB2} {
			require.Equal(t, name, MapType(k, &ast.PGType{Name: name}, "", Caps{}).PG)
		}
		udt := &ast.CastExpr{Expr: ast.BuildIdent("x"), Type: &ast.UserDefinedType{Name: name}}
		require.Equal(t, `CAST("x" AS TEXT)`, rawExpr(udt), "UserDefinedType %q must be mapped, not passed through", name)
	}
	// The PG routine-body writer renders the same node verbatim too.
	require.Equal(t, `CAST("x" AS double precision)`,
		pgast.WriteExpr(&ast.CastExpr{Expr: &ast.Ident{Parts: []string{"x"}}, Type: &ast.PGType{Name: "double precision"}}))
}
