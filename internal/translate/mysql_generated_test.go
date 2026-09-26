package translate

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects"
	mysqldialect "gitlab.com/dalibo/squishy/internal/dialects/mysql"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// genConcatTable is a MariaDB SHOW CREATE TABLE with one column of each
// type the tests below concatenate, plus the generated column `g`.
func genConcatTable(gen string) string {
	return "CREATE TABLE `u` (\n  `id` int(11) NOT NULL,\n" +
		"  `v` varchar(20) DEFAULT NULL,\n  `tx` text DEFAULT NULL,\n  `mt` mediumtext DEFAULT NULL,\n" +
		"  `ti` tinyint(4) DEFAULT NULL,\n  `ub` bigint(20) unsigned DEFAULT NULL,\n  `mi` mediumint(9) DEFAULT NULL,\n" +
		"  `flag` tinyint(1) DEFAULT NULL,\n  `zf` int(5) unsigned zerofill DEFAULT NULL,\n" +
		"  `dbl` double(10,2) DEFAULT NULL,\n  `fl` float(7,2) DEFAULT NULL,\n  `f` float DEFAULT NULL,\n  `dec` decimal(10,2) DEFAULT NULL,\n" +
		"  `ch` char(4) DEFAULT NULL,\n  `en` enum('a','b') DEFAULT NULL,\n  `dt` date DEFAULT NULL,\n  `vb` varbinary(10) DEFAULT NULL,\n" +
		"  `vbin` varchar(10) CHARACTER SET binary DEFAULT NULL,\n" +
		"  `vbc` varchar(10) COLLATE utf8mb4_bin DEFAULT NULL,\n  `l1` varchar(10) CHARACTER SET latin1 DEFAULT NULL,\n" +
		"  `g` varchar(200) GENERATED ALWAYS AS (" + gen + ") VIRTUAL,\n" +
		"  PRIMARY KEY (`id`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;\n"
}

// requireNoGeneratedError asserts the column was translated without a
// refusal.
func requireNoGeneratedError(t *testing.T, res *Result, object string) {
	t.Helper()
	for _, w := range res.Warnings {
		require.NotEqual(t, "table.generated_concat", w.Kind, "unexpected refusal: %+v", w)
		require.NotEqual(t, "table.generated_parse_error", w.Kind, "unexpected refusal: %+v", w)
	}
	for _, e := range res.Explanations {
		require.False(t, e.Object == object && e.Level == "error", "unexpected error explanation: %+v", e)
	}
}

// requireGeneratedError asserts the refusal of table.col is reported
// everywhere: an "error" explanation, a blocking table.generated_concat
// warning and a blocking manual-review prerequisite that name the column
// and the source expression, and the DDL keeps the untranslated call
// (keptCall) as the whole generation expression.
func requireGeneratedError(t *testing.T, res *Result, table, col, reason, keptCall string) {
	t.Helper()
	requireGeneratedRefusal(t, res, "table.generated_concat", table, col, reason, keptCall)
}

// requireGeneratedRefusal is requireGeneratedError for a given warning
// kind (table.generated_concat or table.generated_parse_error).
func requireGeneratedRefusal(t *testing.T, res *Result, kind, table, col, reason, keptCall string) {
	t.Helper()
	object := table + "." + col
	srcExpr := object + " AS ("
	var expl bool
	for _, e := range res.Explanations {
		if e.Object == object && e.Level == "error" {
			require.Contains(t, e.Reason, "refuses to guess MySQL's text conversion")
			require.Contains(t, e.Reason, srcExpr)
			require.Contains(t, e.Reason, reason)
			expl = true
		}
	}
	require.True(t, expl, "no error explanation: %+v", res.Explanations)
	var warned bool
	for _, w := range res.Warnings {
		if w.Kind == kind && w.Object == object {
			require.Equal(t, string(SeverityBlocking), w.Severity)
			require.Contains(t, w.Message, reason)
			warned = true
		}
	}
	require.True(t, warned, "no %s warning: %+v", kind, res.Warnings)
	var blocking bool
	for _, p := range res.Prerequisites {
		if p.Severity == SeverityBlocking && p.Object == object && p.Category == CatManualReview {
			require.Contains(t, p.Title, object)
			require.Contains(t, p.Description, srcExpr)
			require.Contains(t, p.Description, "refuses to guess MySQL's text conversion")
			require.Contains(t, p.Remediation, "rewrite the generation expression by hand")
			require.Contains(t, p.Remediation, "drop the generated column")
			blocking = true
		}
	}
	require.True(t, blocking, "no blocking prerequisite: %+v", res.Prerequisites)
	require.Equal(t, keptCall, planColumn(t, planTable(t, res, table), col).Generated.Expr)
	require.Contains(t, res.DDLScript, keptCall)
}

// Run bug (mariadb-sysver-history sample, sv_orders): a generated column
// `label varchar(40) GENERATED ALWAYS AS (concat('order#',id)) VIRTUAL`
// was emitted as `GENERATED ALWAYS AS (concat('order#', "id")) STORED`,
// and create_ddl failed with "generation expression is not immutable"
// (PG's concat() is STABLE). A generation expression that is exactly a
// CONCAT whose arguments are all on the whitelist becomes an immutable
// || chain, which also keeps MySQL's NULL rule (NULL as soon as one
// argument is NULL).
func TestMySQLGeneratedConcatWhitelist(t *testing.T) {
	cases := []struct{ name, gen, want string }{
		{"string literal + int column", "concat('order#',`id`)", `('order#' || CAST("id" AS text))`},
		{"varchar and text columns", "concat(`v`,'-',`tx`,'/',`mt`)", `("v" || '-' || "tx" || '/' || "mt")`},
		{"integer columns", "concat(`ti`,':',`ub`,':',`mi`)", `(CAST("ti" AS text) || ':' || CAST("ub" AS text) || ':' || CAST("mi" AS text))`},
		{"integer literal", "concat('n=',42,`v`)", `('n=' || CAST(42 AS text) || "v")`},
		{"single argument", "concat(`v`)", `("v")`},
		{"nested concat", "concat(`v`,concat('#',`id`))", `("v" || ('#' || CAST("id" AS text)))`},
		{"parenthesised nested concat", "concat(`v`,(concat('#',`id`)))", `("v" || ('#' || CAST("id" AS text)))`},
		{"parenthesised whole expression", "(concat(`v`,'x'))", `("v" || 'x')`},
		{"non-ASCII literal into utf8mb4", "concat(`v`,'é')", `("v" || 'é')`},
		{"collation-only column of the same charset", "concat(`vbc`,'x')", `("vbc" || 'x')`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := translateMariaDBSysver(t, genConcatTable(tc.gen), "")
			requireNoGeneratedError(t, res, "u.g")
			require.Equal(t, tc.want, planColumn(t, planTable(t, res, "u"), "g").Generated.Expr)
			require.NotContains(t, res.DDLScript, "concat(")
		})
	}
}

// The same whitelist applies to a MySQL source.
func TestMySQLGeneratedConcatWhitelistMySQLSource(t *testing.T) {
	stmts, errs := mysqldialect.Parse("CREATE TABLE `u` (`id` int NOT NULL, `v` varchar(20), `g` varchar(40) AS (concat(`v`,'#',`id`)) STORED, PRIMARY KEY (`id`));")
	require.Empty(t, errs)
	res := Translate(stmts, Options{SourceKind: dialects.KindMySQL, TargetSchema: "mig"})
	requireNoGeneratedError(t, res, "u.g")
	require.Equal(t, `("v" || '#' || CAST("id" AS text))`, planColumn(t, planTable(t, res, "u"), "g").Generated.Expr)
}

// Columns of one explicit non-default charset, into a generated column
// of that same charset, are whitelisted (no conversion in MySQL).
func TestMySQLGeneratedConcatSameExplicitCharset(t *testing.T) {
	res := translateMariaDBSysver(t, "CREATE TABLE `u` (\n  `id` int(11) NOT NULL,\n"+
		"  `l` varchar(20) CHARACTER SET latin1 COLLATE latin1_swedish_ci DEFAULT NULL,\n"+
		"  `g` varchar(40) CHARACTER SET latin1 COLLATE latin1_swedish_ci GENERATED ALWAYS AS (concat(`l`,'-',`id`)) VIRTUAL,\n"+
		"  PRIMARY KEY (`id`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;\n", "")
	requireNoGeneratedError(t, res, "u.g")
	require.Equal(t, `("l" || '-' || CAST("id" AS text))`, planColumn(t, planTable(t, res, "u"), "g").Generated.Expr)
}

// User decision after seven review rounds of a type-inference engine
// that kept storing different text on PG (TINYINT(1) printed as true,
// DOUBLE(M,D) losing its decimals, hex literals read in the wrong
// charset, collation-dependent comparisons, FLOAT single precision,
// DECIMAL scale…): anything outside the whitelist is an error, never a
// guess. The DDL keeps concat() / concat_ws() — the whole expression is
// left untouched — so a forced run fails at create_ddl instead of
// storing a different value.
func TestMySQLGeneratedConcatRefused(t *testing.T) {
	cases := []struct{ name, gen, reason, kept string }{
		{"tinyint(1)", "concat('a=',`flag`)", "TINYINT(1) column", `concat('a=', "flag")`},
		{"double(10,2)", "concat('d=',`dbl`)", "DOUBLE(10,2) column", `concat('d=', "dbl")`},
		{"float(7,2)", "concat('f=',`fl`)", "FLOAT(7,2) column", `concat('f=', "fl")`},
		{"float", "concat('f=',`f`)", "FLOAT column", `concat('f=', "f")`},
		{"decimal", "concat('p=',`dec`)", "DECIMAL(10,2) column", `concat('p=', "dec")`},
		{"char(n)", "concat('c=',`ch`)", "CHAR(4) column", `concat('c=', "ch")`},
		{"enum", "concat('e=',`en`)", "ENUM column", `concat('e=', "en")`},
		{"date", "concat('d=',`dt`)", "DATE column", `concat('d=', "dt")`},
		{"zerofill", "concat('z=',`zf`)", "INT(5) UNSIGNED ZEROFILL column", `concat('z=', "zf")`},
		{"varbinary", "concat('b=',`vb`)", "column", `concat('b=', "vb")`},
		{"varchar charset binary", "concat('b=',`vbin`)", "binary charset", `concat('b=', "vbin")`},
		{"hex literal", "concat('h=',X'41')", "hex literal", `concat('h=', 41)`},
		{"0x literal", "concat('h=',0x41)", "hex literal", `concat('h=', 41)`},
		{"decimal literal", "concat('n=',1.50)", "non-integer number literal", `concat('n=', 1.50)`},
		{"NULL literal", "concat('n=',NULL)", "NULL literal", `concat('n=', NULL)`},
		{"boolean literal", "concat('n=',TRUE)", "boolean literal", `concat('n=', TRUE)`},
		{"comparison inside concat", "concat('ok=',`id` > 3)", "operator >", `concat('ok=', "id" > 3)`},
		{"arithmetic inside concat", "concat('t=',`id` * 2)", "operator *", `concat('t=', "id" * 2)`},
		{"unary minus", "concat('t=',-`id`)", "operator -", `concat('t=', -"id")`},
		{"function", "concat('t=',abs(`id`))", "call to ABS", `concat('t=', ABS("id"))`},
		{"concat_ws", "concat_ws('-',`v`,`id`)", "CONCAT_WS is never rewritten", `CONCAT_WS('-', "v", "id")`},
		{"nested refused concat", "concat(`v`,concat('#',`dbl`))", "DOUBLE(10,2) column", `concat("v", concat('#', "dbl"))`},
		{"concat inside concat_ws", "concat_ws('-',concat(`v`,'x'),`id`)", "CONCAT_WS is never rewritten", `CONCAT_WS('-', concat("v", 'x'), "id")`},
		{"concat_ws inside concat", "concat(`v`,concat_ws('-',`v`,'x'))", "CONCAT_WS is never rewritten", `concat("v", CONCAT_WS('-', "v", 'x'))`},
		// MariaDB prints CONCAT and || of a table created in
		// sql_mode=ORACLE as concat_operator_oracle(…), which skips NULLs.
		{"concat_operator_oracle", "concat_operator_oracle(`v`,'x')", "CONCAT_OPERATOR_ORACLE", `CONCAT_OPERATOR_ORACLE("v", 'x')`},
		{"concat_operator_oracle inside concat", "concat(`v`,concat_operator_oracle(`v`,'x'))", "CONCAT_OPERATOR_ORACLE", `concat("v", CONCAT_OPERATOR_ORACLE("v", 'x'))`},
		{"concat inside another function", "upper(concat(`v`,'x'))", "inside a larger expression", `UPPER(concat("v", 'x'))`},
		{"length of concat", "octet_length(concat(`v`,'x'))", "inside a larger expression", `OCTET_LENGTH(concat("v", 'x'))`},
		{"md5 of concat", "md5(concat(`v`,'x'))", "inside a larger expression", `MD5(concat("v", 'x'))`},
		{"coalesce of concat", "coalesce(concat(`v`,'x'),'-')", "inside a larger expression", `COALESCE(concat("v", 'x'), '-')`},
		{"latin1 operand into a utf8mb4 target", "concat(`v`,`l1`)", "charset latin1", `concat("v", "l1")`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := translateMariaDBSysver(t, genConcatTable(tc.gen), "")
			requireGeneratedError(t, res, "u", "g", tc.reason, tc.kept)
		})
	}
}

// Reviewer counterexample: a CONCAT that is only part of the generation
// expression. Rewriting the inner call would make the whole expression
// immutable, so PG would accept it and run the comparison / LIKE / IN /
// CASE with its own collation (MariaDB's utf8mb4_uca1400_ai_ci is
// case-insensitive: concat(v, 'x') = 'ABCx' is 1 for v = 'abc', PG
// gives false). Refused, concat() kept.
func TestMySQLGeneratedConcatInsideLargerExpressionRefused(t *testing.T) {
	cases := []struct{ name, typ, gen, kept string }{
		{"comparison", "tinyint(1)", "concat(`v`,'') = 'ABC'", `concat("v", '') = 'ABC'`},
		{"like", "tinyint(1)", "concat(`v`,'x') like 'A%'", `concat("v", 'x') LIKE 'A%'`},
		{"in", "tinyint(1)", "concat(`v`) in ('A','b')", `concat("v") IN ('A', 'b')`},
		{"case", "varchar(10)", "case when concat(`v`,'x') = 'Ax' then 'y' else 'n' end", "concat("},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := translateMariaDBSysver(t, "CREATE TABLE `u` (\n  `id` int(11) NOT NULL,\n  `v` varchar(20) DEFAULT NULL,\n"+
				"  `g` "+tc.typ+" GENERATED ALWAYS AS ("+tc.gen+") VIRTUAL,\n"+
				"  PRIMARY KEY (`id`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_uca1400_ai_ci;\n", "")
			expr := planColumn(t, planTable(t, res, "u"), "g").Generated.Expr
			require.Contains(t, expr, tc.kept)
			require.NotContains(t, expr, "||")
			requireGeneratedError(t, res, "u", "g", "inside a larger expression", expr)
		})
	}
}

// Reviewer counterexample: the MariaDB JSON idiom (LONGTEXT or VARCHAR +
// CHECK (json_valid(col))) is promoted to JSONB, so || would run jsonb
// operators: concat(j,'[1]') on '[0]' would store [0, 1] instead of
// [0][1], and concat(j,v) would store the normalised jsonb text.
func TestMySQLGeneratedConcatJSONColumnRefused(t *testing.T) {
	for _, typ := range []string{"longtext", "varchar(200)"} {
		t.Run(typ, func(t *testing.T) {
			res := translateMariaDBSysver(t, "CREATE TABLE `u` (\n  `id` int(11) NOT NULL,\n  `v` varchar(20) DEFAULT NULL,\n"+
				"  `j` "+typ+" CHARACTER SET utf8mb4 COLLATE utf8mb4_bin DEFAULT NULL CHECK (json_valid(`j`)),\n"+
				"  `g1` varchar(200) GENERATED ALWAYS AS (concat(`j`,`v`)) VIRTUAL,\n"+
				"  `g2` varchar(200) GENERATED ALWAYS AS (concat(`j`,'[1]')) VIRTUAL,\n"+
				"  PRIMARY KEY (`id`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;\n", "")
			require.Equal(t, "JSONB", planColumn(t, planTable(t, res, "u"), "j").Type)
			requireGeneratedError(t, res, "u", "g1", "json_valid", `concat("j", "v")`)
			requireGeneratedError(t, res, "u", "g2", "json_valid", `concat("j", '[1]')`)
		})
	}
}

// Reviewer counterexample: a latin1 generated column over a utf8mb4
// column. MariaDB converts '日本' to '??'; the PG chain would keep '日本'.
// Refused unless every text operand and the target share one charset.
func TestMySQLGeneratedConcatCharsetMismatchRefused(t *testing.T) {
	res := translateMariaDBSysver(t, "CREATE TABLE `u` (\n  `id` int(11) NOT NULL,\n  `v` varchar(20) DEFAULT NULL,\n"+
		"  `g` varchar(20) CHARACTER SET latin1 GENERATED ALWAYS AS (concat(`v`,'')) VIRTUAL,\n"+
		"  PRIMARY KEY (`id`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;\n", "")
	requireGeneratedError(t, res, "u", "g", "charset utf8mb4 while the generated column is in latin1", `concat("v", '')`)
}

// A non-ASCII string literal is only accepted into a utf8mb4 generated
// column: MySQL converts the literal to the column charset.
func TestMySQLGeneratedConcatNonASCIILiteralRefused(t *testing.T) {
	res := translateMariaDBSysver(t, "CREATE TABLE `u` (\n  `id` int(11) NOT NULL,\n  `v` varchar(20) DEFAULT NULL,\n"+
		"  `g` varchar(20) GENERATED ALWAYS AS (concat(`v`,'日本')) VIRTUAL,\n"+
		"  PRIMARY KEY (`id`)\n) ENGINE=InnoDB DEFAULT CHARSET=latin1;\n", "")
	requireGeneratedError(t, res, "u", "g", "non-ASCII string literal", `concat("v", '日本')`)
}

// Only a VARCHAR(n) generated column receives a || chain: CHAR(n) pads
// on PG where MySQL strips trailing spaces; other targets are refused too
// rather than left to PG's assignment casts.
//
// Reviewer counterexamples (verified on mariadb-sample 11.8): MariaDB
// caps TINYTEXT / TEXT / MEDIUMTEXT in bytes and silently truncates the
// generated value (`g tinytext AS (concat(v,'xy')) VIRTUAL` with v =
// repeat('a',254) is 255 characters ending in 'aax'; PG TEXT stored 256
// ending in 'axy'), and CONCAT returns NULL above max_allowed_packet
// (concat(a,b) of two 9,000,000-character LONGTEXTs into LONGTEXT is
// NULL; PG stored 18,000,000 characters). The TEXT family is refused;
// PG VARCHAR(n) raises an error where MySQL would truncate.
func TestMySQLGeneratedConcatTargetTypeRefused(t *testing.T) {
	cases := []struct{ name, typ, reason string }{
		{"char(n)", "char(10)", "the generated column is a CHAR(10) column"},
		{"tinytext", "tinytext", "the generated column is a TINYTEXT column"},
		{"text", "text", "the generated column is a TEXT column"},
		{"mediumtext", "mediumtext", "the generated column is a MEDIUMTEXT column"},
		{"longtext", "longtext", "the generated column is a LONGTEXT column"},
		{"int", "int(11)", "the generated column is a INT(11) column"},
		{"json-promoted target", "longtext CHECK (json_valid(`g`))", "(PG JSONB)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := translateMariaDBSysver(t, "CREATE TABLE `u` (\n  `id` int(11) NOT NULL,\n  `v` varchar(20) DEFAULT NULL,\n"+
				"  `g` "+tc.typ+" GENERATED ALWAYS AS (concat(`v`,'x')) VIRTUAL,\n"+
				"  PRIMARY KEY (`id`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;\n", "")
			requireGeneratedError(t, res, "u", "g", tc.reason, `concat("v", 'x')`)
		})
	}
}

// The whitelist is checked against the final PG column types: an
// integer column retyped by harmonizeFKTypes (here to BOOLEAN, the type
// of the TINYINT(1) key it references) is refused.
func TestMySQLGeneratedConcatUsesFinalPGTypes(t *testing.T) {
	res := translateMariaDBSysver(t, "CREATE TABLE `p` (\n  `id` tinyint(1) NOT NULL,\n  PRIMARY KEY (`id`)\n) ENGINE=InnoDB;\n"+
		"CREATE TABLE `u` (\n  `id` int(11) NOT NULL,\n  `pid` tinyint(4) DEFAULT NULL,\n"+
		"  `g` varchar(20) GENERATED ALWAYS AS (concat('p',`pid`)) VIRTUAL,\n"+
		"  PRIMARY KEY (`id`),\n  CONSTRAINT `fk` FOREIGN KEY (`pid`) REFERENCES `p` (`id`)\n) ENGINE=InnoDB;\n", "")
	require.Equal(t, "BOOLEAN", planColumn(t, planTable(t, res, "u"), "pid").Type)
	requireGeneratedError(t, res, "u", "g", "(PG BOOLEAN)", `concat('p', "pid")`)
}

// A nested refused CONCAT is reported once for the column.
func TestMySQLGeneratedConcatNestedRefusalReportedOnce(t *testing.T) {
	res := translateMariaDBSysver(t, genConcatTable("concat(`v`,concat('#',`dbl`))"), "")
	var n int
	for _, w := range res.Warnings {
		if w.Kind == "table.generated_concat" {
			n++
			require.Contains(t, w.Message, "DOUBLE(10,2) column")
		}
	}
	require.Equal(t, 1, n)
}

// Two refused columns get two prerequisites (the title names the column,
// so the dedup by title does not merge their expressions).
func TestMySQLGeneratedConcatOnePrerequisitePerColumn(t *testing.T) {
	res := translateMariaDBSysver(t, "CREATE TABLE `u` (\n  `id` int NOT NULL,\n  `d` double DEFAULT NULL,\n"+
		"  `g1` varchar(40) AS (concat('a',`d`)) VIRTUAL,\n  `g2` varchar(40) AS (concat_ws('-',`id`)) VIRTUAL,\n"+
		"  PRIMARY KEY (`id`)\n) ENGINE=InnoDB;\n", "")
	objs := map[string]bool{}
	for _, p := range res.Prerequisites {
		if p.Severity == SeverityBlocking && p.Category == CatManualReview && (p.Object == "u.g1" || p.Object == "u.g2") {
			objs[p.Object] = true
		}
	}
	require.Len(t, objs, 2, "%+v", res.Prerequisites)
}

// Generated columns without CONCAT keep their pre-existing translation:
// no inference, no rewrite, no refusal.
func TestMySQLGeneratedWithoutConcatUnchanged(t *testing.T) {
	res := translateMariaDBSysver(t, "CREATE TABLE `g` (\n  `id` int(11) NOT NULL,\n  `qty` int(11) NOT NULL,\n  `price` decimal(10,2) NOT NULL,\n"+
		"  `total` decimal(12,2) GENERATED ALWAYS AS (`qty` * `price`) STORED,\n"+
		"  `up` varchar(20) GENERATED ALWAYS AS (upper(`id`)) VIRTUAL,\n"+
		"  PRIMARY KEY (`id`)\n) ENGINE=InnoDB;\n", "")
	for _, w := range res.Warnings {
		require.NotEqual(t, "table.generated_concat", w.Kind)
	}
	tbl := planTable(t, res, "g")
	require.Equal(t, `"qty" * "price"`, planColumn(t, tbl, "total").Generated.Expr)
	require.Equal(t, `UPPER("id")`, planColumn(t, tbl, "up").Generated.Expr)
}

func TestMySQLGenUsesConcat(t *testing.T) {
	require.False(t, mysqlGenUsesConcat(nil))
	require.False(t, mysqlGenUsesConcat(&ast.FuncCall{Name: "UPPER", Args: []ast.Expr{&ast.Ident{Parts: []string{"v"}}}}))
	require.True(t, mysqlGenUsesConcat(&ast.BinaryExpr{Op: "=", Lhs: &ast.FuncCall{Name: "concat"}, Rhs: &ast.Literal{Kind: "string", Text: "x"}}))
	require.True(t, mysqlGenUsesConcat(&ast.FuncCall{Name: "UPPER", Args: []ast.Expr{&ast.FuncCall{Name: "Concat_Ws"}}}))
	require.True(t, mysqlGenUsesConcat(&ast.FuncCall{Name: "concat_operator_oracle"}))
}

func TestMySQLGenCharset(t *testing.T) {
	require.Equal(t, "latin1", mysqlGenCharset(&ast.ColumnDef{Type: &ast.CharType{Name: "VARCHAR", Charset: "LATIN1"}}, ast.TableOptions{Charset: "utf8mb4"}))
	require.Equal(t, "utf8mb4", mysqlGenCharset(&ast.ColumnDef{Type: &ast.TextType{Name: "TEXT", Collation: "utf8mb4_bin"}}, ast.TableOptions{Charset: "latin1"}))
	require.Equal(t, "utf8mb4", mysqlGenCharset(&ast.ColumnDef{Type: &ast.TextType{Name: "TEXT"}}, ast.TableOptions{Charset: "utf8mb4"}))
	require.Equal(t, "latin1", mysqlGenCharset(&ast.ColumnDef{Type: &ast.TextType{Name: "TEXT"}}, ast.TableOptions{Collate: "latin1_swedish_ci"}))
	require.Equal(t, "", mysqlGenCharset(&ast.ColumnDef{Type: &ast.TextType{Name: "TEXT"}}, ast.TableOptions{}))
	require.Equal(t, "binary", mysqlGenCharset(&ast.ColumnDef{Charset: "binary", Type: &ast.CharType{Name: "VARCHAR"}}, ast.TableOptions{}))
}

func TestMySQLGenIsPlainInteger(t *testing.T) {
	for _, ok := range []string{"0", "7", "42", "123456789012345678"} {
		require.True(t, mysqlGenIsPlainInteger(ok), ok)
	}
	for _, ko := range []string{"", "007", "1.5", "1e3", "1234567890123456789", ".5"} {
		require.False(t, mysqlGenIsPlainInteger(ko), ko)
	}
}

// The column-level NOT ENFORCED is kept on the AST.
func TestMySQLColumnCheckNotEnforcedParsed(t *testing.T) {
	stmts, errs := mysqldialect.Parse("CREATE TABLE t (a tinyint(1) CHECK (a in (0,1)) NOT ENFORCED, b tinyint(1) CHECK (b in (0,1)) ENFORCED, c int CHECK (c > 0));")
	require.Empty(t, errs)
	ct := stmts[0].(*ast.CreateTable)
	require.True(t, ct.Columns[0].CheckNotEnforced)
	require.False(t, ct.Columns[1].CheckNotEnforced)
	require.False(t, ct.Columns[2].CheckNotEnforced)
}

// Review finding (verified on mariadb-sample): MariaDB prints
// `g1 varchar(40) AS (concat(v,'x') regexp 'ax') STORED` as
// `concat(`v`,'x') regexp 'ax'`. The parser stops at REGEXP but its error
// recovery still returns the table with the generation expression cut
// down to `concat(`v`,'x')`, a whitelisted CONCAT: the column was
// rewritten into ("v" || 'x') and PG stored 'ax' where MariaDB stores 1.
// With parse errors in the source DDL nothing is rewritten, the refusal
// is blocking (table.generated_parse_error), and the DDL keeps concat()
// (create_ddl fails loudly).
func TestMySQLGeneratedConcatRefusedOnParseErrors(t *testing.T) {
	src := "CREATE TABLE `u` (\n  `id` int(11) NOT NULL,\n  `v` varchar(20) DEFAULT NULL,\n" +
		"  `g1` varchar(40) AS (concat(`v`,'x') regexp 'a') STORED,\n" +
		"  PRIMARY KEY (`id`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;\n"
	// Same entry point as the plan handler (dialects.Dialect.Parse).
	stmts, perr := mysqldialect.New(dialects.KindMariaDB, "MariaDB").Parse(src)
	require.Error(t, perr)
	res := Translate(stmts, Options{SourceKind: dialects.KindMariaDB, TargetSchema: "mig", ParseError: perr})
	// The recovered AST really is the shortened, otherwise whitelisted
	// CONCAT: only the parse error stops the rewrite.
	requireGeneratedRefusal(t, res, "table.generated_parse_error", "u", "g1", "the source DDL has parse errors", `concat("v", 'x')`)
	for _, e := range res.Explanations {
		require.False(t, e.Object == "u.g1" && e.Level == "info", "unexpected rewrite explanation: %+v", e)
	}
	require.NotContains(t, res.DDLScript, `"v" || 'x'`)

	// Without the parse error the same AST would be rewritten: the
	// refusal above comes from ParseError, not from the whitelist.
	clean := Translate(stmts, Options{SourceKind: dialects.KindMariaDB, TargetSchema: "mig"})
	require.Equal(t, `("v" || 'x')`, planColumn(t, planTable(t, clean, "u"), "g1").Generated.Expr)
}

// Review finding: MySQL table names are case-sensitive
// (lower_case_table_names=0). The pending column was resolved against the
// first plan table matching its name case-insensitively, so U's rewrite
// landed on u (wrong data) while U kept concat() with an "info"
// explanation. Each pending column is now resolved against its own table.
func TestMySQLGeneratedConcatCaseDistinctTables(t *testing.T) {
	res := translateMariaDBSysver(t, "CREATE TABLE `u` (\n  `id` int(11) NOT NULL,\n  `v` varchar(20) DEFAULT NULL,\n  `dbl` double DEFAULT NULL,\n"+
		"  `g` varchar(40) AS (concat(`v`,`dbl`)) VIRTUAL,\n  PRIMARY KEY (`id`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;\n"+
		"CREATE TABLE `U` (\n  `id` int(11) NOT NULL,\n  `v` varchar(20) DEFAULT NULL,\n"+
		"  `g` varchar(40) AS (concat(`v`,`id`)) VIRTUAL,\n  PRIMARY KEY (`id`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;\n", "")
	requireGeneratedError(t, res, "u", "g", "DOUBLE column", `concat("v", "dbl")`)
	require.Equal(t, `("v" || CAST("id" AS text))`, planColumn(t, planTable(t, res, "U"), "g").Generated.Expr)
	for _, w := range res.Warnings {
		require.False(t, w.Kind == "table.generated_concat" && w.Object == "U.g", "unexpected refusal: %+v", w)
	}
	var info bool
	for _, e := range res.Explanations {
		if e.Object == "U.g" && e.Level == "info" {
			require.Contains(t, e.Target, `("v" || CAST("id" AS text))`)
			info = true
		}
		require.False(t, e.Object == "u.g" && e.Level == "info", "u.g reported as rewritten: %+v", e)
		require.False(t, e.Object == "U.g" && e.Level == "error", "U.g reported as refused: %+v", e)
	}
	require.True(t, info)
}

// Review finding: parser error recovery can drop the CONCAT entirely
// (`binary concat(v,'x')` comes back as the identifier "binary"), so no
// CONCAT is left to trigger the refusal. With parse errors every MySQL
// generated column is refused, CONCAT or not, and keeps its parsed
// expression.
func TestMySQLGeneratedRefusedOnParseErrorsWithoutConcat(t *testing.T) {
	src := "CREATE TABLE `w` (\n  `id` int(11) NOT NULL,\n  `n` int(11) DEFAULT NULL,\n" +
		"  `d` int(11) AS (`n` * 2) VIRTUAL,\n  PRIMARY KEY (`id`)\n) ENGINE=InnoDB;\n" +
		"CREATE TABLE `u` (\n  `id` int(11) NOT NULL,\n  `v` varchar(20) DEFAULT NULL,\n" +
		"  `g1` varchar(40) AS (binary concat(`v`,'x')) STORED,\n" +
		"  PRIMARY KEY (`id`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;\n"
	stmts, perr := mysqldialect.New(dialects.KindMariaDB, "MariaDB").Parse(src)
	require.Error(t, perr)
	res := Translate(stmts, Options{SourceKind: dialects.KindMariaDB, TargetSchema: "mig", ParseError: perr})
	g1 := planColumn(t, planTable(t, res, "u"), "g1").Generated.Expr
	requireGeneratedRefusal(t, res, "table.generated_parse_error", "u", "g1", "the source DDL has parse errors", g1)
	requireGeneratedRefusal(t, res, "table.generated_parse_error", "w", "d", "the source DDL has parse errors", `"n" * 2`)
	for _, w := range res.Warnings {
		require.NotEqual(t, "table.generated_concat", w.Kind, "parse-error refusal reported as a CONCAT refusal: %+v", w)
	}

	// Without parse errors a generated column without CONCAT keeps its
	// translation and gets no refusal.
	clean := Translate(stmts, Options{SourceKind: dialects.KindMariaDB, TargetSchema: "mig"})
	requireNoGeneratedError(t, clean, "w.d")
	require.Equal(t, `"n" * 2`, planColumn(t, planTable(t, clean, "w"), "d").Generated.Expr)
}

// genTextConvTable is a MariaDB SHOW CREATE TABLE with a boolean-mapped
// TINYINT(1) and BIT(1) column, an INT column, and the generated column
// `g` of type typ.
func genTextConvTable(typ, gen string) string {
	return "CREATE TABLE `u` (\n  `id` int(11) NOT NULL,\n  `qty` int(11) NOT NULL,\n" +
		"  `paid` tinyint(1) NOT NULL DEFAULT 0,\n  `b1` bit(1) DEFAULT NULL,\n  `v` varchar(20) DEFAULT NULL,\n" +
		"  `g` " + typ + " GENERATED ALWAYS AS (" + gen + ") VIRTUAL,\n" +
		"  PRIMARY KEY (`id`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;\n"
}

// Run bug (mariadb-sysver-history sample, sv_orders): generated columns
// without CONCAT that convert a value to text were emitted as is, and PG
// accepted them while storing a different text than MariaDB:
//
//   - `mark varchar(4) AS (X'41')`: MariaDB stores 'A', PG stored '41';
//   - `paid_c varchar(4) AS (cast(paid as char charset utf8mb4))` on a
//     TINYINT(1) → BOOLEAN column: MariaDB stores '1' / '0', PG 't' / 'f';
//   - `paid_v varchar(5) AS (paid)`: MariaDB stores '1' / '0', PG's
//     assignment cast 'true' / 'false'.
//
// Same treatment as a refused CONCAT: an error explanation, a blocking
// table.generated_concat warning and a blocking manual-review
// prerequisite; the DDL keeps the expression as translated.
func TestMySQLGeneratedTextConversionRefused(t *testing.T) {
	cases := []struct{ name, typ, gen, reason, kept string }{
		// (a) hex / bit literals, anywhere in the expression.
		{"hex literal value", "varchar(4)", "X'41'", "hex literal 41", "41"},
		{"0x literal value", "varchar(4)", "0x41", "hex literal 41", "41"},
		{"bit literal value", "varchar(4)", "b'1000001'", "bit literal", ""},
		{"hex literal in arithmetic into int", "int(11)", "X'41' + 0", "hex literal 41", "41 + 0"},
		{"hex literal inside a function", "varchar(10)", "upper(X'61')", "hex literal 61", "UPPER(61)"},
		// (b) CAST / CONVERT of a boolean value.
		{"cast tinyint(1) as char", "varchar(4)", "cast(`paid` as char charset utf8mb4)", "converts a boolean value", ""},
		{"cast bit(1) as char", "varchar(4)", "cast(`b1` as char charset utf8mb4)", "converts a boolean value", ""},
		{"cast comparison as char", "varchar(4)", "cast(`qty` > 3 as char charset utf8mb4)", "converts a boolean value", ""},
		{"cast tinyint(1) as char into text", "text", "cast(`paid` as char charset utf8mb4)", "converts a boolean value", ""},
		{"cast tinyint(1) inside a function", "varchar(10)", "upper(cast(`paid` as char charset utf8mb4))", "converts a boolean value", ""},
		// (c) boolean value stored in a text generated column.
		{"tinyint(1) column into varchar", "varchar(5)", "`paid`", "is a boolean", `"paid"`},
		{"bit(1) column into varchar", "varchar(5)", "`b1`", "is a boolean", `"b1"`},
		{"tinyint(1) column into char", "char(5)", "`paid`", "is a boolean", `"paid"`},
		{"tinyint(1) column into text", "text", "`paid`", "is a boolean", `"paid"`},
		{"comparison into varchar", "varchar(5)", "`qty` > 3", "is a boolean", `"qty" > 3`},
		{"not into varchar", "varchar(5)", "!`paid`", "is a boolean", ""},
		{"true into varchar", "varchar(5)", "TRUE", "is a boolean", "TRUE"},
		{"in into varchar", "varchar(5)", "`qty` in (1,2)", "is a boolean", `"qty" IN (1, 2)`},
		{"case returning a boolean into varchar", "varchar(5)", "case when `qty` > 3 then `paid` else `paid` end", "is a boolean", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := translateMariaDBSysver(t, genTextConvTable(tc.typ, tc.gen), "")
			kept := planColumn(t, planTable(t, res, "u"), "g").Generated.Expr
			if tc.kept != "" {
				require.Equal(t, tc.kept, kept)
			}
			requireGeneratedError(t, res, "u", "g", tc.reason, kept)
			for _, e := range res.Explanations {
				if e.Object == "u.g" && e.Level == "error" {
					require.Contains(t, e.Target, "PostgreSQL may store a different text")
				}
			}
			for _, p := range res.Prerequisites {
				if p.Object == "u.g" && p.Severity == SeverityBlocking {
					require.Contains(t, p.Title, "MySQL text conversion not translated")
					require.Contains(t, p.Description, "PostgreSQL may accept while storing a different text")
				}
			}
		})
	}
}

// Negative cases: an integer or VARCHAR value into a text generated
// column, a CAST of an integer to CHAR, and a boolean into a non-text
// generated column keep their translation without a refusal.
func TestMySQLGeneratedTextConversionNotRefused(t *testing.T) {
	cases := []struct{ name, typ, gen, want string }{
		{"int column into varchar", "varchar(10)", "`qty`", `"qty"`},
		{"int arithmetic into varchar", "varchar(10)", "`qty` * 2", `"qty" * 2`},
		{"varchar column into varchar", "varchar(20)", "`v`", `"v"`},
		{"boolean into tinyint(1)", "tinyint(1)", "`qty` > 3", `"qty" > 3`},
		{"string literal into varchar", "varchar(4)", "'A'", `'A'`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := translateMariaDBSysver(t, genTextConvTable(tc.typ, tc.gen), "")
			requireNoGeneratedError(t, res, "u.g")
			require.Equal(t, tc.want, planColumn(t, planTable(t, res, "u"), "g").Generated.Expr)
		})
	}
	// CAST of an integer column to CHAR: no boolean, no refusal.
	res := translateMariaDBSysver(t, genTextConvTable("varchar(10)", "cast(`qty` as char charset utf8mb4)"), "")
	requireNoGeneratedError(t, res, "u.g")
}

// The whole sv_orders table of the mariadb-sysver-history sample, as
// MariaDB 11.8 prints it: every generated column that converts a value
// to text PG prints differently is refused, the others keep their
// translation.
func TestMySQLGeneratedSysverOrdersSample(t *testing.T) {
	res := translateMariaDBSysver(t, "CREATE TABLE `sv_orders` (\n  `id` int(11) NOT NULL,\n  `qty` int(11) NOT NULL,\n"+
		"  `unit_price` decimal(10,2) NOT NULL,\n  `paid` tinyint(1) NOT NULL DEFAULT 0,\n  `code` char(4) DEFAULT NULL,\n  `note` varchar(20) DEFAULT NULL,\n"+
		"  `total_v` decimal(12,2) GENERATED ALWAYS AS (`qty` * `unit_price`) VIRTUAL,\n"+
		"  `total_s` decimal(12,2) GENERATED ALWAYS AS (`qty` * `unit_price`) STORED,\n"+
		"  `label` varchar(40) GENERATED ALWAYS AS (concat('order#',`id`)) VIRTUAL,\n"+
		"  `summary` varchar(80) GENERATED ALWAYS AS (concat('paid=',`paid`,';t=',`qty` * `unit_price`,';','#',`note`,';ok=',`qty` > 3)) STORED,\n"+
		"  `tags` varchar(80) GENERATED ALWAYS AS (concat_ws('|',`note`,`paid`,`id`,`code`)) VIRTUAL,\n"+
		"  `paid_c` varchar(4) GENERATED ALWAYS AS (cast(`paid` as char charset utf8mb4)) VIRTUAL,\n"+
		"  `paid_v` varchar(5) GENERATED ALWAYS AS (`paid`) VIRTUAL,\n"+
		"  `total_t` varchar(16) GENERATED ALWAYS AS (`qty` * `unit_price`) STORED,\n"+
		"  `mark` varchar(4) GENERATED ALWAYS AS (X'41') VIRTUAL,\n"+
		"  `note_len` int(11) GENERATED ALWAYS AS (octet_length(`note`)) VIRTUAL,\n"+
		"  `sys_start` timestamp(6) GENERATED ALWAYS AS ROW START,\n  `sys_end` timestamp(6) GENERATED ALWAYS AS ROW END,\n"+
		"  PRIMARY KEY (`id`,`sys_end`),\n  PERIOD FOR SYSTEM_TIME (`sys_start`, `sys_end`)\n"+
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci WITH SYSTEM VERSIONING;\n", mariadb118RowEndMax)
	tbl := planTable(t, res, "sv_orders")
	requireGeneratedError(t, res, "sv_orders", "mark", "hex literal 41", "41")
	requireGeneratedError(t, res, "sv_orders", "paid_c", "converts a boolean value", planColumn(t, tbl, "paid_c").Generated.Expr)
	requireGeneratedError(t, res, "sv_orders", "paid_v", "is a boolean", `"paid"`)
	requireGeneratedError(t, res, "sv_orders", "summary", "TINYINT(1) column", planColumn(t, tbl, "summary").Generated.Expr)
	requireGeneratedError(t, res, "sv_orders", "tags", "CONCAT_WS is never rewritten", planColumn(t, tbl, "tags").Generated.Expr)
	require.Equal(t, `('order#' || CAST("id" AS text))`, planColumn(t, tbl, "label").Generated.Expr)
	for _, col := range []string{"label", "total_v", "total_s", "total_t", "note_len"} {
		obj := "sv_orders." + col
		for _, w := range res.Warnings {
			require.False(t, w.Object == obj && (w.Kind == "table.generated_concat" || w.Kind == "table.generated_parse_error"), "unexpected refusal: %+v", w)
		}
		for _, e := range res.Explanations {
			require.False(t, e.Object == obj && e.Level == "error", "unexpected error explanation: %+v", e)
		}
	}
	require.Equal(t, `"qty" * "unit_price"`, planColumn(t, tbl, "total_t").Generated.Expr)
}
