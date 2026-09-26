package translate

import (
	"strings"

	"gitlab.com/dalibo/squishy/internal/dialects"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// mysql_call_casts.go — argument conversion for calls to the migrated
// MySQL / MariaDB stored routines.
//
// MySQL converts every routine argument to the declared parameter type
// (assignment conversion): `CALL add_job_history(OLD.employee_id, …)`
// works with an INT parameter even though employee_id is INT UNSIGNED,
// `f(42)` works with a VARCHAR parameter, and `g(1)` works with a
// TINYINT(1) parameter. PG resolves a call by argument types instead, and
// only *implicit* casts take part in that resolution. Implicit casts cover
// widening numeric conversions (int2 → int4 → int8 → numeric → float) and
// untyped literals, nothing else:
//
//   - narrowing integer / numeric conversions (int8 → int4, numeric →
//     int, float8 → numeric, float8 → float4) are assignment-only;
//   - every conversion to a string type from a non-string type is an
//     I/O-conversion cast, assignment-only;
//   - int4 → boolean is explicit-only, int8 → boolean does not exist;
//   - timestamp → date and the string → date / time / json conversions
//     are assignment-only or explicit.
//
// Since MySQL's unsigned integer columns are also widened on PG (INT
// UNSIGNED → BIGINT, see types_map_mysql.go), the same call on PG fails
// with "function f(bigint) does not exist".
//
// The translator collects, from the parsed CREATE PROCEDURE / FUNCTION
// statements, the PG conversion of every parameter (collectMySQLRoutine
// Sigs) and the rewriter built by makeMySQLCallArgCastVisitor wraps each
// argument of a call to such a routine in CAST(arg AS <param PG type>),
// reproducing MySQL's conversion whatever the parameter type. The cast
// target is the base type without typmod (varchar, bpchar, numeric, …):
// PG ignores parameter typmods, so no truncation is introduced. Untyped
// literals and NULL resolve to any parameter type and are left alone, as
// are integer literals passed to a parameter int4 widens to implicitly.
// OUT / INOUT arguments must stay assignable variables and are never
// cast. A parameter type with no faithful PG conversion (SET → text[])
// is not cast: the call is reported instead, never passed silently.
//
// The rewriter runs on every MySQL path that renders a call: routine and
// trigger bodies, view bodies, event schedules, and the table-level
// CHECK / DEFAULT / generated-column expressions.

// mysqlParamConv is the conversion applied to the arguments passed to one
// parameter of a migrated routine.
type mysqlParamConv struct {
	// Target is the PG type name (no typmod) arguments are cast to; ""
	// leaves the argument alone (OUT / INOUT parameter, unknown type).
	Target string
	// Bool marks a BOOLEAN parameter (TINYINT(1), BIT(1)). PG has an
	// int4 → boolean cast but none from int8 / numeric, so non-literal
	// arguments go through integer first: CAST(CAST(a AS integer) AS
	// boolean), which also accepts a boolean argument.
	Bool bool
	// Unsupported is the reason no faithful conversion exists for this
	// parameter; calls passing a non-NULL argument to it are reported.
	Unsupported string
}

// mysqlRoutineSigs maps the case-insensitive name of each routine defined
// in the migrated schema to the conversion of each of its parameters.
type mysqlRoutineSigs struct {
	procs map[string][]mysqlParamConv
	funcs map[string][]mysqlParamConv
}

// collectMySQLRoutineSigs builds the signature table from the parsed
// statements. Returns nil when the dump defines no routine.
func collectMySQLRoutineSigs(stmts []ast.Stmt, kind dialects.Kind, caps Caps) *mysqlRoutineSigs {
	sigs := &mysqlRoutineSigs{procs: map[string][]mysqlParamConv{}, funcs: map[string][]mysqlParamConv{}}
	found := false
	for _, s := range stmts {
		switch x := s.(type) {
		case *ast.CreateProcedure:
			sigs.procs[strings.ToLower(x.Name)] = mysqlParamConvs(x.Params, caps)
			found = true
		case *ast.CreateFunction:
			sigs.funcs[strings.ToLower(x.Name)] = mysqlParamConvs(x.Params, caps)
			found = true
		}
	}
	if !found {
		return nil
	}
	return sigs
}

// mysqlParamConvs returns the conversion of each parameter.
func mysqlParamConvs(params []ast.Param, caps Caps) []mysqlParamConv {
	out := make([]mysqlParamConv, len(params))
	for i, p := range params {
		if strings.EqualFold(p.Direction, "OUT") || strings.EqualFold(p.Direction, "INOUT") {
			continue
		}
		out[i] = mysqlParamConvFor(p.Type, caps)
	}
	return out
}

// mysqlParamConvFor derives the conversion from the parameter's typed
// MySQL data type. It mirrors mapMySQLType node for node, keeping only
// the base PG type name (TestMySQLParamConvMatchesTypeMap guards the
// correspondence).
func mysqlParamConvFor(t ast.DataType, caps Caps) mysqlParamConv {
	switch x := t.(type) {
	case nil:
		return mysqlParamConv{}
	case *ast.IntType:
		name := strings.ToUpper(x.Name)
		if name == "TINYINT" && x.HasWidth && x.Width == 1 && !x.Unsigned {
			return mysqlParamConv{Target: "boolean", Bool: true}
		}
		if x.Unsigned {
			switch name {
			case "TINYINT":
				return mysqlParamConv{Target: "smallint"}
			case "SMALLINT", "MEDIUMINT":
				return mysqlParamConv{Target: "integer"}
			case "INT", "INTEGER":
				return mysqlParamConv{Target: "bigint"}
			case "BIGINT":
				return mysqlParamConv{Target: "numeric"}
			}
		}
		switch name {
		case "TINYINT", "SMALLINT":
			return mysqlParamConv{Target: "smallint"}
		case "BIGINT":
			return mysqlParamConv{Target: "bigint"}
		}
		return mysqlParamConv{Target: "integer"}
	case *ast.FloatType:
		if strings.EqualFold(x.Name, "FLOAT") {
			if x.HasPS {
				return mysqlParamConv{Target: "numeric"}
			}
			return mysqlParamConv{Target: "real"}
		}
		return mysqlParamConv{Target: "double precision"}
	case *ast.DecimalType:
		return mysqlParamConv{Target: "numeric"}
	case *ast.BitType:
		if x.Width <= 1 {
			return mysqlParamConv{Target: "boolean", Bool: true}
		}
		return mysqlParamConv{Target: "bytea"}
	case *ast.CharType:
		if strings.EqualFold(x.Name, "VARCHAR") {
			return mysqlParamConv{Target: "varchar"}
		}
		// bpchar without typmod: plain `char` would mean char(1).
		return mysqlParamConv{Target: "bpchar"}
	case *ast.TextType, *ast.EnumType:
		return mysqlParamConv{Target: "text"}
	case *ast.BlobType, *ast.BinaryType:
		return mysqlParamConv{Target: "bytea"}
	case *ast.JSONType:
		return mysqlParamConv{Target: "jsonb"}
	case *ast.DateType:
		return mysqlParamConv{Target: "date"}
	case *ast.TimeType:
		return mysqlParamConv{Target: "time"}
	case *ast.DateTimeType:
		return mysqlParamConv{Target: "timestamp"}
	case *ast.TimestampType:
		return mysqlParamConv{Target: "timestamptz"}
	case *ast.YearType:
		return mysqlParamConv{Target: "smallint"}
	case *ast.SpatialType:
		if caps.HasPostGIS {
			return mysqlParamConv{Target: "geometry"}
		}
		return mysqlParamConv{Target: "text"}
	case *ast.VectorType:
		return mysqlParamConv{Target: "vector"}
	case *ast.SetType:
		return mysqlParamConv{Unsupported: "SET parameter is text[] on PG while MySQL passes a comma-separated string; convert the argument manually (e.g. string_to_array(arg, ','))"}
	}
	return mysqlParamConv{Unsupported: "parameter type " + t.TypeName() + " has no known PG argument conversion"}
}

// applyMySQLCallArgCasts runs makeMySQLCallArgCastVisitor over every
// statement of a parsed routine body and returns the rewritten statements
// plus the diagnostics of calls that could not be converted. nil sigs →
// stmts unchanged.
func applyMySQLCallArgCasts(stmts []ast.PLStmt, sigs *mysqlRoutineSigs) ([]ast.PLStmt, []string) {
	if sigs == nil || len(stmts) == 0 {
		return stmts, nil
	}
	var diags []string
	fn := makeMySQLCallArgCastVisitor(sigs, func(msg string) { diags = appendUnique(diags, msg) })
	out := make([]ast.PLStmt, len(stmts))
	for i, s := range stmts {
		if rs, ok := ast.Rewrite(s, fn).(ast.PLStmt); ok {
			out[i] = rs
		} else {
			out[i] = s
		}
	}
	return out, diags
}

// castMySQLCallArgsInSelect applies the call-argument casts to a
// rewritten MySQL SELECT (view body). nil sigs → sel unchanged.
func castMySQLCallArgsInSelect(sel *ast.SelectStmt, sigs *mysqlRoutineSigs) (*ast.SelectStmt, []string) {
	if sigs == nil || sel == nil {
		return sel, nil
	}
	var diags []string
	fn := makeMySQLCallArgCastVisitor(sigs, func(msg string) { diags = appendUnique(diags, msg) })
	if rs, ok := ast.Rewrite(sel, fn).(*ast.SelectStmt); ok {
		return rs, diags
	}
	return sel, diags
}

// castMySQLCallArgsInExpr applies the call-argument casts to a single
// MySQL expression (CHECK, DEFAULT, generated column, event schedule).
// nil sigs → e unchanged.
func castMySQLCallArgsInExpr(e ast.Expr, sigs *mysqlRoutineSigs) (ast.Expr, []string) {
	if sigs == nil || e == nil {
		return e, nil
	}
	var diags []string
	fn := makeMySQLCallArgCastVisitor(sigs, func(msg string) { diags = appendUnique(diags, msg) })
	if re, ok := ast.Rewrite(e, fn).(ast.Expr); ok {
		return re, diags
	}
	return e, diags
}

func appendUnique(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

// makeMySQLCallArgCastVisitor returns the rewriter casting the arguments
// of `CALL p(…)` statements and `f(…)` calls that target a routine of
// sigs with the same arity. Qualified names are matched on the routine
// name alone (a MySQL migration maps one source schema). report receives
// one message per argument that could not be converted.
func makeMySQLCallArgCastVisitor(sigs *mysqlRoutineSigs, report func(string)) ast.Rewriter {
	return func(n ast.Node) ast.Node {
		switch x := n.(type) {
		case *ast.CallStmt:
			convs, ok := sigs.procs[strings.ToLower(x.Name)]
			if !ok {
				return n
			}
			args, changed := castMySQLCallArgs("procedure "+x.Name, x.Args, convs, report)
			if !changed {
				return n
			}
			cp := *x
			cp.Args = args
			return &cp
		case *ast.FuncCall:
			convs, ok := sigs.funcs[strings.ToLower(x.Name)]
			if !ok {
				return n
			}
			args, changed := castMySQLCallArgs("function "+x.Name, x.Args, convs, report)
			if !changed {
				return n
			}
			cp := *x
			cp.Args = args
			return &cp
		}
		return n
	}
}

// castMySQLCallArgs returns a fresh argument slice with every argument
// converted per its parameter's conversion. Arity mismatches (overload,
// default arguments) leave the call alone.
func castMySQLCallArgs(routine string, args []ast.Expr, convs []mysqlParamConv, report func(string)) ([]ast.Expr, bool) {
	if len(args) != len(convs) {
		return args, false
	}
	var out []ast.Expr
	for i, a := range args {
		c := convs[i]
		if a == nil || isNullLiteral(a) {
			continue
		}
		if c.Unsupported != "" {
			if report != nil {
				report("call to " + routine + ": argument " + itoa(i+1) + " not converted — " + c.Unsupported)
			}
			continue
		}
		if c.Target == "" || mysqlArgNeedsNoCast(a, c) {
			continue
		}
		if out == nil {
			out = append([]ast.Expr(nil), args...)
		}
		out[i] = mysqlArgCast(a, c)
	}
	if out == nil {
		return args, false
	}
	return out, true
}

// mysqlArgCast builds the conversion of argument a. The cast targets are
// *ast.PGType nodes: already PG type names, emitted verbatim.
//
// PG only casts int4 to boolean, so a boolean parameter takes its
// argument through integer — except an int4-sized integer literal,
// which PG types as int4 and casts to boolean directly. Any other
// literal (decimal `1.5`, int8-sized `9999999999`) goes through integer
// like a non-literal argument: `CAST(1.5 AS boolean)` has no PG cast.
func mysqlArgCast(a ast.Expr, c mysqlParamConv) ast.Expr {
	if c.Bool && !isInt4NumberLiteral(a) {
		a = &ast.CastExpr{Expr: a, Type: &ast.PGType{Name: "integer"}, P: a.Pos()}
	}
	return &ast.CastExpr{Expr: a, Type: &ast.PGType{Name: c.Target}, P: a.Pos()}
}

// isInt4NumberLiteral reports whether a is a number literal PG types as
// int4 (see numberLitKind).
func isInt4NumberLiteral(a ast.Expr) bool {
	l, ok := a.(*ast.Literal)
	return ok && l.Kind == "number" && numberLitKind(l.Text) == numLitInt4
}

func isNullLiteral(a ast.Expr) bool {
	l, ok := a.(*ast.Literal)
	return ok && l.Kind == "null"
}

// mysqlArgNeedsNoCast reports whether argument a already resolves to a PG
// parameter of conversion c without a cast.
func mysqlArgNeedsNoCast(a ast.Expr, c mysqlParamConv) bool {
	switch x := a.(type) {
	case *ast.CastExpr:
		if pt, ok := x.Type.(*ast.PGType); ok && strings.EqualFold(pt.Name, c.Target) {
			return true
		}
	case *ast.Literal:
		switch x.Kind {
		case "string":
			// Untyped literal: PG resolves it to the parameter type.
			return true
		case "bool":
			return c.Bool
		case "number":
			switch numberLitKind(x.Text) {
			case numLitInt4:
				// int4 widens implicitly to int8 / numeric / float.
				switch c.Target {
				case "integer", "bigint", "numeric", "real", "double precision":
					return true
				}
			case numLitNumeric:
				// numeric widens implicitly to float.
				switch c.Target {
				case "numeric", "real", "double precision":
					return true
				}
			}
		}
	}
	return false
}

type numLitClass int

const (
	numLitOther   numLitClass = iota
	numLitInt4                // unsigned integer literal PG types as int4 (≤ 9 digits)
	numLitNumeric             // unsigned decimal literal PG types as numeric
)

// numberLitKind classifies the text of a single number literal token.
func numberLitKind(text string) numLitClass {
	if text == "" {
		return numLitOther
	}
	digits, dots := 0, 0
	for _, r := range text {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r == '.':
			dots++
		default:
			return numLitOther // exponent, sign, hex: let the cast decide
		}
	}
	switch {
	case dots == 0 && digits <= 9:
		return numLitInt4
	case dots == 0:
		return numLitOther // int8 or numeric depending on magnitude
	case dots == 1 && digits > 0:
		return numLitNumeric
	}
	return numLitOther
}
