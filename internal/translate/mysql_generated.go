package translate

import (
	"strconv"
	"strings"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// MySQL/MariaDB generated columns that use CONCAT / CONCAT_WS.
//
// PG's concat() and concat_ws() are only STABLE (their variadic "any"
// arguments go through type output functions), so
// `GENERATED ALWAYS AS (concat(…)) STORED` fails with "generation
// expression is not immutable" (SQLSTATE 42P17).
//
// squishy does not guess how MySQL converts a value to text. The
// generation expression is rewritten ONLY when it is exactly
// CONCAT(a, b, …) — optionally inside parentheses — and it becomes the
// immutable chain (A || B || …) ONLY when:
//
//   - the generated column itself is VARCHAR(n) and keeps its plain PG
//     mapping VARCHAR(n). PG enforces n and raises an error when the value
//     is too long, where MySQL/MariaDB may silently truncate it (VIRTUAL
//     column, non-strict sql_mode). TINYTEXT / TEXT / MEDIUMTEXT /
//     LONGTEXT targets are refused: MariaDB caps them in bytes (255 /
//     65535 / 16M / 4G) and silently truncates the generated value, while
//     PG TEXT is unbounded. CHAR(n) is refused (MySQL strips trailing
//     spaces where PG bpchar pads), and so is a JSONB-promoted target;
//   - every argument is one of:
//   - a plain string literal (no hex / bit literal), ASCII-only unless
//     the generated column is utf8mb4. A charset introducer
//     (_utf8mb4'x') is not accepted by the parser: MySQL 8 prints one
//     before every string literal of SHOW CREATE TABLE, so on MySQL 8 a
//     CONCAT holding a string literal comes back with a parse error and
//     is refused (see below) — conservative, never silent;
//   - a column of the table whose source type is VARCHAR or TINYTEXT /
//     TEXT / MEDIUMTEXT / LONGTEXT and whose final PG type is still the
//     plain mapping of that type (VARCHAR(n) / TEXT) — a column promoted
//     to JSONB by the CHECK (json_valid(col)) idiom, or retyped by
//     harmonizeFKTypes, is refused: || would run jsonb (or bpchar)
//     semantics;
//   - an integer column (TINYINT … BIGINT, UNSIGNED included) that is not
//     mapped to BOOLEAN (TINYINT(1) / BOOL), not ZEROFILL, and still has
//     its plain PG mapping (SMALLINT / INTEGER / BIGINT / NUMERIC(20,0)):
//     CAST(c AS text) prints the same digits as MySQL;
//   - a plain integer literal (digits only, canonical form, at most 18
//     digits so it stays a BIGINT in both engines): CAST(n AS text);
//   - a nested CONCAT whose arguments satisfy the same rule;
//   - every text operand resolves to the same charset as the generated
//     column (column, then type, then collation prefix, then table
//     default): MySQL converts a mismatching operand to the target
//     charset (unrepresentable characters become '?'), PG does not.
//
// NULL semantics: MySQL's CONCAT returns NULL as soon as one argument is
// NULL; `text || text` is NULL as soon as one operand is NULL. The chain
// therefore returns NULL exactly when MySQL does (PG's concat() would
// skip the NULLs instead). A NULL literal argument is not whitelisted.
// MySQL's CONCAT also returns NULL (warning 1301) when its result is
// larger than max_allowed_packet; the || chain has no such limit. With a
// VARCHAR(n) target the stored value is at most 65535 bytes, so the rule
// only holds under the assumption that max_allowed_packet is at least
// the byte size of the target VARCHAR (default 16 MiB / 64 MiB; its
// minimum is 1 KiB). A larger intermediate result makes MySQL store NULL
// and PG raise "value too long", which is loud.
//
// Everything else is refused as a blocking error: any other argument
// (TINYINT(1), CHAR, ENUM, SET, DECIMAL, FLOAT, DOUBLE, BIT, temporal,
// JSON, binary, any other literal, expression, function, comparison or
// arithmetic), every CONCAT_WS, every CONCAT_OPERATOR_ORACLE (MariaDB
// prints both CONCAT and || of a table created in sql_mode=ORACLE that
// way; it skips NULL arguments), and a CONCAT that is only part of the
// generation expression (CONCAT(…) = 'x', LIKE, IN, CASE, COALESCE,
// LENGTH, MD5, UPPER… would then run with PG's collation, encoding and
// function semantics). A refused expression is left untouched: the DDL
// keeps concat() / concat_ws() / CONCAT_OPERATOR_ORACLE(), so create_ddl
// fails loudly on PG instead of storing a different value.
//
// When the source DDL had parse errors (Options.ParseError), no MySQL
// generated column is translated at all, whether or not its parsed
// expression still holds a CONCAT: the parser's error recovery can cut a
// generation expression short — MariaDB prints `concat(v,'x') regexp 'ax'`,
// the parser stops at REGEXP and keeps `concat(v,'x')`, whose || chain
// would store 'ax' where MySQL stores 1 — or drop the CONCAT entirely
// (`binary concat(v,'x')` comes back as the identifier "binary"). Every
// generated column then gets the same error explanation and a blocking
// prerequisite (table.generated_parse_error), and the DDL keeps the
// expression as parsed.
//
// A generation expression without CONCAT is kept as translated, unless
// it converts a value to text in a way PG prints differently
// (textConversionHazards: a hex / bit literal anywhere, a CAST / CONVERT
// of a boolean value, a boolean value stored in a CHAR / VARCHAR / TEXT
// generated column). That column gets the same blocking refusal
// (table.generated_concat): PG may accept its DDL and store 41 / t /
// true where MySQL stores 'A' / '1', so it must be rewritten by hand.

// mysqlGenPending is a MySQL/MariaDB generated column waiting for
// resolveMySQLGeneratedConcat.
type mysqlGenPending struct {
	table string
	// tableIdx is the index of the table in Plan.Tables (MySQL table
	// names are case-sensitive with lower_case_table_names=0).
	tableIdx int
	col      *ast.ColumnDef
	expr     ast.Expr // after castMySQLDDLExpr
	cols     []*ast.ColumnDef
	opts     ast.TableOptions
}

// mysqlGenUsesConcat reports whether an expression calls CONCAT,
// CONCAT_WS or CONCAT_OPERATOR_ORACLE anywhere.
func mysqlGenUsesConcat(e ast.Expr) bool {
	found := false
	ast.Rewrite(e, func(n ast.Node) ast.Node {
		if fc, ok := n.(*ast.FuncCall); ok && (strings.EqualFold(fc.Name, "CONCAT") || mysqlGenNeverRewritten(fc.Name) != "") {
			found = true
		}
		return n
	})
	return found
}

// mysqlGenNeverRewritten returns why a concatenation function (a single
// function name) is never rewritten into ||, or "" for any other name.
func mysqlGenNeverRewritten(name string) string {
	switch {
	case strings.EqualFold(name, "CONCAT_WS"):
		return "CONCAT_WS is never rewritten (MySQL skips NULL arguments and converts each one to text its own way)"
	case strings.EqualFold(name, "CONCAT_OPERATOR_ORACLE"):
		return "CONCAT_OPERATOR_ORACLE (CONCAT / || of a MariaDB table created in sql_mode=ORACLE) is never rewritten (it skips NULL arguments and converts each one to text its own way)"
	}
	return ""
}

// resolveMySQLGeneratedConcat rewrites or refuses every pending generated
// column. It runs after every PG column type is final (JSON promotion,
// harmonizeFKTypes), so the whitelist sees the types PG will actually
// apply || to, and the boolean rule sees the final BOOLEAN columns.
func (t *translator) resolveMySQLGeneratedConcat() {
	for _, p := range t.pendingGenConcat {
		if p.tableIdx < 0 || p.tableIdx >= len(t.res.Plan.Tables) || t.res.Plan.Tables[p.tableIdx].Name != p.table {
			continue
		}
		tbl := &t.res.Plan.Tables[p.tableIdx]
		idx := columnIndex(tbl.Columns, p.col.Name)
		if idx < 0 || tbl.Columns[idx].Generated == nil {
			continue
		}
		obj := p.table + "." + p.col.Name
		src := rawExpr(p.expr)
		if t.opt.ParseError != nil {
			// Parser error recovery may have cut the generation
			// expression short (`concat(v,'x') regexp 'ax'` comes back
			// as `concat(v,'x')`) or dropped its CONCAT (`binary
			// concat(v,'x')` comes back as "binary"): the parsed
			// expression is not proven to be the whole source
			// expression, so nothing is rewritten.
			msg := "generated column " + obj + " AS (" + src + "): the source DDL has parse errors (" + t.opt.ParseError.Error() +
				"), so the parsed generation expression may be only part of the source expression, or may have lost a CONCAT / CONCAT_WS. squishy refuses to guess MySQL's text conversion and does not translate this generated column: the DDL keeps the expression as parsed."
			t.res.Explanations = append(t.res.Explanations, Explanation{
				Object: obj,
				Source: "GENERATED ALWAYS AS (" + src + ")",
				Target: "GENERATED ALWAYS AS (" + src + ") STORED (as parsed, not verified: source DDL has parse errors)",
				Reason: msg,
				Level:  "error",
			})
			t.warnSev(obj, "table.generated_parse_error", msg, SeverityBlocking)
			continue
		}
		g := &mysqlGenConcat{t: t, p: p, pgCols: tbl.Columns}
		if !mysqlGenUsesConcat(p.expr) {
			// No CONCAT: the expression is kept as translated unless it
			// converts a value to text in a way PG prints differently
			// (hex / bit literal, boolean converted or stored as text).
			hazards := g.textConversionHazards(p.expr)
			if len(hazards) == 0 {
				continue
			}
			msg := "generated column " + obj + " AS (" + src + "): squishy refuses to guess MySQL's text conversion and does not translate it — " +
				strings.Join(hazards, " | ") +
				". PostgreSQL does not print these values as MySQL does: MySQL reads a hex / bit literal as a binary string (X'41' is 'A') and prints a boolean (TINYINT(1) / BIT(1) column, comparison, NOT, TRUE / FALSE) as the integer 1 / 0, where PostgreSQL has the number 41 and t / f or true / false. The DDL keeps the expression as translated; PostgreSQL may accept it and store a different text, so the column must be rewritten by hand before the run."
			t.res.Explanations = append(t.res.Explanations, Explanation{
				Object: obj,
				Source: "GENERATED ALWAYS AS (" + src + ")",
				Target: "GENERATED ALWAYS AS (" + src + ") STORED (untranslated: PostgreSQL may store a different text)",
				Reason: msg,
				Level:  "error",
			})
			t.warnSev(obj, "table.generated_concat", msg, SeverityBlocking)
			continue
		}
		out, refusals := g.rewrite(p.expr)
		if len(refusals) == 0 {
			tbl.Columns[idx].Generated.Expr = rawExpr(out)
			t.res.Explanations = append(t.res.Explanations, Explanation{
				Object: obj,
				Source: "GENERATED ALWAYS AS (" + src + ")",
				Target: "GENERATED ALWAYS AS (" + rawExpr(out) + ") STORED",
				Reason: "PG's concat() is not immutable, so CONCAT is rewritten into ||. The generated column is VARCHAR(n) (PG raises an error where MySQL may truncate) and every argument is a same-charset VARCHAR / TEXT column, a non-boolean integer column, a string literal or an integer literal, whose text is identical in both engines; || returns NULL as soon as one operand is NULL, like MySQL's CONCAT (assuming max_allowed_packet is at least the byte size of the VARCHAR).",
				Level:  "info",
			})
			continue
		}
		msg := "generated column " + obj + " AS (" + src + "): squishy refuses to guess MySQL's text conversion in CONCAT / CONCAT_WS and does not translate it — " +
			strings.Join(refusals, " | ") +
			". Only a generation expression that is exactly CONCAT(…) into a VARCHAR(n) column, over string literals, integer literals, same-charset VARCHAR / TEXT columns, non-boolean integer columns and nested CONCAT of those, is rewritten. The DDL keeps concat() / concat_ws() / CONCAT_OPERATOR_ORACLE(), which PostgreSQL rejects in a generation expression (not immutable), so create_ddl fails instead of storing a different value."
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: obj,
			Source: "GENERATED ALWAYS AS (" + src + ")",
			Target: "GENERATED ALWAYS AS (" + src + ") STORED (untranslated: fails at create_ddl)",
			Reason: msg,
			Level:  "error",
		})
		t.warnSev(obj, "table.generated_concat", msg, SeverityBlocking)
	}
}

// mysqlGenConcat is the state of one generated-column resolution.
type mysqlGenConcat struct {
	t      *translator
	p      mysqlGenPending
	pgCols []PGColumn
	// target is the effective charset of the generated column.
	target string
}

// rewrite returns the || chain of a whitelisted generation expression,
// or the expression untouched and why it is refused.
func (g *mysqlGenConcat) rewrite(e ast.Expr) (ast.Expr, []string) {
	fc, ok := unwrapParens(e).(*ast.FuncCall)
	switch {
	case ok && strings.EqualFold(fc.Name, "CONCAT"):
	case ok && mysqlGenNeverRewritten(fc.Name) != "":
		return e, []string{rawExpr(fc) + ": " + mysqlGenNeverRewritten(fc.Name)}
	default:
		return e, []string{"CONCAT / CONCAT_WS is used inside a larger expression: only a generation expression that is exactly CONCAT(…) is rewritten, because the enclosing operator or function (comparison, LIKE, IN, CASE, COALESCE, LENGTH, MD5, UPPER…) would then run with PostgreSQL's collation, encoding and function semantics"}
	}
	var reasons []string
	g.target = mysqlGenCharset(g.p.col, g.p.opts)
	if why := g.targetRefusal(); why != "" {
		reasons = append(reasons, why)
	}
	chain := g.concat(fc, &reasons)
	if len(reasons) > 0 {
		return e, reasons
	}
	return chain, nil
}

// targetRefusal returns why the generated column itself cannot receive a
// || chain, or "".
func (g *mysqlGenConcat) targetRefusal() string {
	c := g.p.col
	if vc, ok := c.Type.(*ast.CharType); !ok || !strings.EqualFold(vc.Name, "VARCHAR") || !g.keepsPlainMapping(c) {
		return "the generated column is a " + g.typeLabel(c) + " column: only a VARCHAR(n) generated column is rewritten (MariaDB silently truncates a value too long for TINYTEXT / TEXT / MEDIUMTEXT / LONGTEXT, and MySQL's CONCAT returns NULL above max_allowed_packet, while PG TEXT is unbounded; PG VARCHAR(n) raises an error instead)"
	}
	if g.target == "binary" {
		return "the generated column is in the binary charset"
	}
	return ""
}

// concat returns the (A || B || …) chain of a CONCAT call, appending to
// reasons every argument that is not on the whitelist.
func (g *mysqlGenConcat) concat(fc *ast.FuncCall, reasons *[]string) ast.Expr {
	if fc.Distinct || len(fc.Args) == 0 {
		*reasons = append(*reasons, rawExpr(fc)+": unexpected CONCAT form")
		return fc
	}
	operands := make([]ast.Expr, 0, len(fc.Args))
	for _, a := range fc.Args {
		if inner, ok := unwrapParens(a).(*ast.FuncCall); ok {
			if strings.EqualFold(inner.Name, "CONCAT") {
				operands = append(operands, g.concat(inner, reasons))
				continue
			}
			if why := mysqlGenNeverRewritten(inner.Name); why != "" {
				*reasons = append(*reasons, rawExpr(inner)+": "+why)
				continue
			}
		}
		op, why := g.operand(a)
		if why != "" {
			*reasons = append(*reasons, "argument "+rawExpr(a)+" is "+why)
			continue
		}
		operands = append(operands, op)
	}
	if len(operands) == 0 {
		return fc
	}
	chain := operands[0]
	for _, op := range operands[1:] {
		chain = &ast.BinaryExpr{Op: "||", Lhs: chain, Rhs: op, P: fc.P}
	}
	return &ast.ParenExpr{Inner: chain, P: fc.P}
}

// operand returns a whitelisted CONCAT argument as the PG text operand
// holding MySQL's text, or a non-empty description of why it is not on
// the whitelist.
func (g *mysqlGenConcat) operand(a ast.Expr) (ast.Expr, string) {
	switch x := unwrapParens(a).(type) {
	case *ast.Literal:
		switch x.Kind {
		case "string":
			if !mysqlGenIsASCII(x.Text) && g.target != "utf8mb4" {
				return nil, "a non-ASCII string literal and the generated column is not utf8mb4 (MySQL converts it to the column charset)"
			}
			return x, ""
		case "number":
			if mysqlGenIsPlainInteger(x.Text) {
				return mysqlGenCastText(x), ""
			}
			return nil, "a non-integer number literal"
		case "hex":
			return nil, "a hex literal (its text depends on the column charset)"
		case "bit":
			return nil, "a bit literal"
		case "null":
			return nil, "a NULL literal"
		case "bool":
			return nil, "a boolean literal (MySQL prints 1 / 0)"
		}
		return nil, "a " + x.Kind + " literal"
	case *ast.Ident:
		if len(x.Parts) != 1 {
			return nil, "a qualified column reference"
		}
		col := g.column(x.Parts[0])
		if col == nil || col.Type == nil {
			return nil, "not a column of the table"
		}
		if isJSONValidCheck(col.Check, col.Name) {
			return nil, "a " + g.typeLabel(col) + " column with CHECK (json_valid(…)), promoted to JSONB (|| would concatenate jsonb values)"
		}
		if !g.keepsPlainMapping(col) {
			return nil, "a " + g.typeLabel(col) + " column (its PG type is no longer the plain mapping of the source type)"
		}
		switch t := col.Type.(type) {
		case *ast.CharType, *ast.TextType:
			if !mysqlGenIsTextType(t) {
				break
			}
			cs := mysqlGenCharset(col, g.p.opts)
			if cs == "binary" {
				return nil, "a " + g.typeLabel(col) + " column in the binary charset"
			}
			if cs != g.target {
				return nil, "a " + g.typeLabel(col) + " column in charset " + mysqlGenCharsetLabel(cs) + " while the generated column is in " + mysqlGenCharsetLabel(g.target) + " (MySQL converts it, unrepresentable characters become '?')"
			}
			return x, ""
		case *ast.IntType:
			if !t.Zerofill && mysqlGenIsIntegerPG(g.pgType(col)) {
				return mysqlGenCastText(x), ""
			}
		}
		return nil, "a " + g.typeLabel(col) + " column"
	case *ast.FuncCall:
		return nil, "a call to " + x.Name
	case *ast.BinaryExpr:
		return nil, "an expression with operator " + x.Op
	case *ast.UnaryExpr:
		return nil, "an expression with operator " + x.Op
	}
	return nil, "an expression"
}

// column returns the source definition of a column of the table.
func (g *mysqlGenConcat) column(name string) *ast.ColumnDef {
	for _, c := range g.p.cols {
		if c != nil && strings.EqualFold(c.Name, name) {
			return c
		}
	}
	return nil
}

// pgType returns the final PG type of a column of the table ("" when the
// column is not in the plan).
func (g *mysqlGenConcat) pgType(c *ast.ColumnDef) string {
	if i := columnIndex(g.pgCols, c.Name); i >= 0 {
		return g.pgCols[i].Type
	}
	return ""
}

// keepsPlainMapping reports whether a column's final PG type is still
// the plain mapping of its source type (not promoted to JSONB, not
// retyped by harmonizeFKTypes).
func (g *mysqlGenConcat) keepsPlainMapping(c *ast.ColumnDef) bool {
	pg := g.pgType(c)
	return pg != "" && pg == MapType(g.t.opt.SourceKind, c.Type, c.Name, g.t.caps()).PG
}

// typeLabel names a column type for a refusal message: the MySQL type,
// with the attributes that matter to its text form, and the final PG
// type when it differs from the plain mapping.
func (g *mysqlGenConcat) typeLabel(c *ast.ColumnDef) string {
	s := mysqlGenTypeLabel(c.Type)
	if pg := g.pgType(c); pg != "" && !g.keepsPlainMapping(c) {
		s += " (PG " + pg + ")"
	}
	return s
}

// mysqlGenIsTextType reports whether a source type is VARCHAR or
// TINYTEXT / TEXT / MEDIUMTEXT / LONGTEXT.
func mysqlGenIsTextType(dt ast.DataType) bool {
	switch t := dt.(type) {
	case *ast.CharType:
		return strings.EqualFold(t.Name, "VARCHAR")
	case *ast.TextType:
		return true
	}
	return false
}

// mysqlGenIsIntegerPG reports whether a PG type is one a MySQL integer
// maps to when it is not a boolean (TINYINT(1)).
func mysqlGenIsIntegerPG(pg string) bool {
	switch pg {
	case "SMALLINT", "INTEGER", "BIGINT", "NUMERIC(20,0)":
		return true
	}
	return false
}

// mysqlGenCharset returns the lower-cased effective charset of a string
// column: its CHARACTER SET, else its type's, else the charset its
// collation belongs to, else the table default charset, else the charset
// of the table default collation. "" means the server default.
func mysqlGenCharset(c *ast.ColumnDef, opts ast.TableOptions) string {
	cs, coll := c.Charset, c.Collation
	switch t := c.Type.(type) {
	case *ast.CharType:
		if cs == "" {
			cs = t.Charset
		}
		if coll == "" {
			coll = t.Collation
		}
	case *ast.TextType:
		if cs == "" {
			cs = t.Charset
		}
		if coll == "" {
			coll = t.Collation
		}
	}
	if cs == "" {
		cs = mysqlGenCollationCharset(coll)
	}
	if cs == "" {
		cs = opts.Charset
	}
	if cs == "" {
		cs = mysqlGenCollationCharset(opts.Collate)
	}
	return strings.ToLower(cs)
}

// mysqlGenCollationCharset returns the charset part of a collation name
// (a single identifier): what precedes its first '_' (utf8mb4_bin →
// utf8mb4, latin1_swedish_ci → latin1, binary → binary). An unknown
// prefix never equals a real charset name, so it can only match the
// same collation family: the comparison stays conservative.
func mysqlGenCollationCharset(coll string) string {
	for i := 0; i < len(coll); i++ {
		if coll[i] == '_' {
			return coll[:i]
		}
	}
	return coll
}

func mysqlGenCharsetLabel(cs string) string {
	if cs == "" {
		return "(server default)"
	}
	return cs
}

// mysqlGenIsASCII reports whether a string literal value only holds
// ASCII characters.
func mysqlGenIsASCII(v string) bool {
	for i := 0; i < len(v); i++ {
		if v[i] >= 0x80 {
			return false
		}
	}
	return true
}

// mysqlGenIsPlainInteger reports whether a number literal token (a single
// token, never SQL) is a canonical unsigned integer of at most 18 digits.
func mysqlGenIsPlainInteger(tok string) bool {
	if tok == "" || len(tok) > 18 || (len(tok) > 1 && tok[0] == '0') {
		return false
	}
	for i := 0; i < len(tok); i++ {
		if tok[i] < '0' || tok[i] > '9' {
			return false
		}
	}
	return true
}

func mysqlGenCastText(e ast.Expr) ast.Expr {
	return &ast.CastExpr{Expr: e, Type: &ast.PGType{Name: "text"}, P: e.Pos()}
}

// mysqlGenTypeLabel names a MySQL column type for a refusal message,
// with the attributes that matter to its text form.
func mysqlGenTypeLabel(dt ast.DataType) string {
	switch t := dt.(type) {
	case *ast.IntType:
		s := strings.ToUpper(t.Name)
		if t.HasWidth {
			s += "(" + strconv.Itoa(t.Width) + ")"
		}
		if t.Unsigned {
			s += " UNSIGNED"
		}
		if t.Zerofill {
			s += " ZEROFILL"
		}
		return s
	case *ast.FloatType:
		if t.HasPS {
			return strings.ToUpper(t.Name) + "(" + strconv.Itoa(t.Precision) + "," + strconv.Itoa(t.Scale) + ")"
		}
		return strings.ToUpper(t.Name)
	case *ast.DecimalType:
		if t.HasPrec {
			return strings.ToUpper(t.Name) + "(" + strconv.Itoa(t.Precision) + "," + strconv.Itoa(t.Scale) + ")"
		}
		return strings.ToUpper(t.Name)
	case *ast.CharType:
		if t.HasLength {
			return strings.ToUpper(t.Name) + "(" + strconv.Itoa(t.Length) + ")"
		}
		return strings.ToUpper(t.Name)
	case *ast.TextType:
		return strings.ToUpper(t.Name)
	}
	if dt == nil {
		return "untyped"
	}
	return dt.TypeName()
}

// textConversionHazards returns why a generation expression without
// CONCAT may store a different text on PG than on MySQL, or nil. It is a
// small conservative rule on the typed AST (false positives are
// acceptable, silent differences are not):
//
//   - a hex or bit literal anywhere (X'41' / 0x41 / b'1'): MySQL reads it
//     as a binary string (X'41' is 'A', X'41' + 0 is 65), the emitted DDL
//     holds its digits (41);
//   - a CAST / CONVERT of a boolean value (a column whose final PG type is
//     BOOLEAN — TINYINT(1), BOOL, BIT(1), or a key retyped by
//     harmonizeFKTypes — a comparison, NOT, TRUE / FALSE…): MySQL
//     converts the integer 1 / 0, PG prints t / f (CAST AS CHAR) or
//     true / false;
//   - a boolean value as the whole value of a CHAR / VARCHAR / TEXT
//     generated column: MySQL stores '1' / '0', PG's assignment cast
//     stores 'true' / 'false'.
func (g *mysqlGenConcat) textConversionHazards(e ast.Expr) []string {
	var hazards []string
	ast.Rewrite(e, func(n ast.Node) ast.Node {
		switch x := n.(type) {
		case *ast.Literal:
			switch x.Kind {
			case "hex":
				hazards = append(hazards, "hex literal "+rawExpr(x)+" (MySQL reads X'…' / 0x… as a binary string — X'41' is 'A' — while the DDL holds its digits)")
			case "bit":
				hazards = append(hazards, "bit literal "+rawExpr(x)+" (MySQL reads b'…' as a binary string)")
			}
		case *ast.CastExpr:
			if g.isBoolean(x.Expr) {
				hazards = append(hazards, rawExpr(x)+" converts a boolean value (MySQL converts the integer 1 / 0, PostgreSQL prints t / f or true / false)")
			}
		case *ast.FuncCall:
			if strings.EqualFold(x.Name, "CONVERT") && len(x.Args) > 0 && g.isBoolean(x.Args[0]) {
				hazards = append(hazards, rawExpr(x)+" converts a boolean value (MySQL converts the integer 1 / 0, PostgreSQL prints t / f or true / false)")
			}
		}
		return n
	})
	if mysqlGenIsStringType(g.p.col.Type) && g.isBoolean(e) {
		hazards = append(hazards, "the value of the "+g.typeLabel(g.p.col)+" generated column is a boolean (MySQL stores '1' / '0', PostgreSQL's assignment cast stores 'true' / 'false')")
	}
	return hazards
}

// isBoolean reports whether an expression is boolean-typed on PG: a
// column whose final PG type is BOOLEAN, TRUE / FALSE, a comparison or
// logical operator, NOT, IN, BETWEEN, EXISTS, or a CASE / IF / IFNULL /
// COALESCE / NULLIF / GREATEST / LEAST one of whose values is.
func (g *mysqlGenConcat) isBoolean(e ast.Expr) bool {
	switch x := unwrapParens(e).(type) {
	case *ast.Literal:
		return x.Kind == "bool"
	case *ast.Ident:
		if len(x.Parts) == 0 {
			return false
		}
		col := g.column(x.Parts[len(x.Parts)-1])
		return col != nil && g.pgType(col) == "BOOLEAN"
	case *ast.BinaryExpr:
		return mysqlGenBooleanOps[strings.ToUpper(x.Op)]
	case *ast.UnaryExpr:
		return strings.EqualFold(x.Op, "NOT") || x.Op == "!"
	case *ast.InExpr, *ast.BetweenExpr, *ast.ExistsExpr:
		return true
	case *ast.CaseExpr:
		for _, w := range x.Whens {
			if g.isBoolean(w.Then) {
				return true
			}
		}
		return x.Else != nil && g.isBoolean(x.Else)
	case *ast.FuncCall:
		args := x.Args
		switch strings.ToUpper(x.Name) {
		case "IF":
			if len(args) > 0 {
				args = args[1:]
			}
		case "IFNULL", "COALESCE", "NULLIF", "GREATEST", "LEAST":
		default:
			return false
		}
		for _, a := range args {
			if g.isBoolean(a) {
				return true
			}
		}
	}
	return false
}

// mysqlGenBooleanOps are the binary operators (single operator tokens as
// the MySQL parser names them, upper-cased) whose result is a boolean.
// `||` is MySQL's OR (without PIPES_AS_CONCAT).
var mysqlGenBooleanOps = map[string]bool{
	"=": true, "<>": true, "!=": true, "<": true, "<=": true, ">": true, ">=": true, "<=>": true,
	"AND": true, "&&": true, "OR": true, "||": true, "XOR": true,
	"IS": true, "IS NOT": true, "LIKE": true, "NOT LIKE": true, "IN": true, "NOT IN": true,
	"REGEXP": true, "NOT REGEXP": true, "RLIKE": true, "NOT RLIKE": true, "SOUNDS LIKE": true,
}

// mysqlGenIsStringType reports whether a source column type is a
// character string type (CHAR / VARCHAR / NCHAR… or the TEXT family).
func mysqlGenIsStringType(dt ast.DataType) bool {
	switch dt.(type) {
	case *ast.CharType, *ast.TextType:
		return true
	}
	return false
}
