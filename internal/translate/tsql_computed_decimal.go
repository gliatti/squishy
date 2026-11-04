package translate

import (
	"strconv"
	"strings"
	"unicode/utf8"

	pgast "gitlab.com/dalibo/squishy/internal/dialects/postgres"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// tsql_computed_decimal.go — the exact-number half of the computed-column
// proof (tsqlComputedProof). PG numeric arithmetic is exact: a + b keeps
// max(s1, s2) decimals, a * b keeps s1 + s2. SQL Server types every
// decimal result as decimal(p, s) with the same scales, except that
//
//   - a precision above 38 is capped and the scale reduced (the value is
//     rounded to the reduced scale);
//   - a division has the fixed scale max(6, s1 + p2 + 1), where PG picks
//     its own;
//   - money * money and money / x are money, rounded to 4 decimals.
//
// So a decimal expression is proven identical only when no operation hits
// one of these cases; tsqlDecimalReason says which one does.

// tsqlDec is the T-SQL decimal(p, s) type of an exact-number expression.
type tsqlDec struct {
	p, s  int
	money bool // money / smallmoney (decimal(19, 4) / decimal(10, 4) in arithmetic)
	ok    bool // false: p and s are unknown
}

// tsqlDecOfType is the decimal type of an exact T-SQL data type.
func (t *translator) tsqlDecOfType(dt ast.DataType) tsqlDec {
	switch x := dt.(type) {
	case *ast.IntType:
		switch strings.ToUpper(x.Name) {
		case "TINYINT":
			return tsqlDec{p: 3, ok: true}
		case "SMALLINT":
			return tsqlDec{p: 5, ok: true}
		case "BIGINT":
			return tsqlDec{p: 19, ok: true}
		}
		return tsqlDec{p: 10, ok: true}
	case *ast.BitType:
		return tsqlDec{p: 1, ok: true}
	case *ast.DecimalType:
		if !x.HasPrec {
			return tsqlDec{p: 18, ok: true}
		}
		return tsqlDec{p: x.Precision, s: x.Scale, ok: true}
	case *ast.TSQLType:
		switch x.Name {
		case "MONEY":
			return tsqlDec{p: 19, s: 4, money: true, ok: true}
		case "SMALLMONEY":
			return tsqlDec{p: 10, s: 4, money: true, ok: true}
		case "BIT":
			return tsqlDec{p: 1, ok: true}
		}
	case *ast.UserDefinedType:
		if t.tsql != nil {
			if alias := t.tsql.aliasTypes[tsqlUserTypeKey(x.Schema, x.Name)]; alias != nil && alias.BaseType != nil {
				return t.tsqlDecOfType(alias.BaseType)
			}
		}
	}
	return tsqlDec{}
}

// tsqlDecOfLiteral is the type SQL Server gives a number literal: an
// integer is int (decimal(p, 0) past the int range), 1.50 is
// decimal(3, 2) and 0.05 decimal(2, 2) (leading zeros do not count).
func tsqlDecOfLiteral(tok string) tsqlDec {
	intDigits, frac, seenDot, lead := 0, 0, false, true
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		switch {
		case c == '.':
			seenDot = true
		case c >= '0' && c <= '9':
			if seenDot {
				frac++
				continue
			}
			if lead && c == '0' {
				continue
			}
			lead = false
			intDigits++
		default:
			// 1e3 is a float literal in T-SQL.
			return tsqlDec{}
		}
	}
	if !seenDot {
		if intDigits <= 9 {
			return tsqlDec{p: 10, ok: true}
		}
		return tsqlDec{p: intDigits, ok: true}
	}
	p := intDigits + frac
	if p == 0 {
		p = 1
	}
	return tsqlDec{p: p, s: frac, ok: true}
}

// tsqlDecimalReason walks an exact-number source expression and returns
// why its value may differ on PG ("" when it is proven identical). types
// maps a folded column name to its T-SQL data type.
func (p *tsqlComputedProof) tsqlDecimalReason(e ast.Expr) string {
	_, reason := p.decOf(e)
	return reason
}

func (p *tsqlComputedProof) decOf(e ast.Expr) (tsqlDec, string) {
	switch x := e.(type) {
	case *ast.Literal:
		switch x.Kind {
		case "number":
			return tsqlDecOfLiteral(x.Text), ""
		case "money":
			return tsqlDec{p: 19, s: 4, money: true, ok: true}, ""
		}
		return tsqlDec{}, ""
	case *ast.Ident:
		if len(x.Parts) == 1 && !x.Variable && p.types != nil {
			if dt := p.types[normalizeTSQLIdent(x.Parts[0])]; dt != nil {
				return p.t.tsqlDecOfType(dt), ""
			}
		}
		return tsqlDec{}, ""
	case *ast.ParenExpr:
		return p.decOf(x.Inner)
	case *ast.UnaryExpr:
		return p.decOf(x.Rhs)
	case *ast.CastExpr:
		if _, r := p.decOf(x.Expr); r != "" {
			return tsqlDec{}, r
		}
		// CAST rounds to the target scale on both sides.
		return p.t.tsqlDecOfType(x.Type), ""
	case *ast.FuncCall:
		decs := make([]tsqlDec, len(x.Args))
		for i, a := range x.Args {
			d, r := p.decOf(a)
			if r != "" {
				return tsqlDec{}, r
			}
			decs[i] = d
		}
		switch strings.ToUpper(x.Name) {
		case "ISNULL", "ABS":
			// ISNULL is typed like its first argument.
			if len(decs) > 0 {
				return decs[0], ""
			}
		case "COALESCE", "NULLIF":
			return tsqlDecUnion(decs), ""
		}
		return tsqlDec{}, ""
	case *ast.CaseExpr:
		var decs []tsqlDec
		for _, w := range x.Whens {
			if _, r := p.decOf(w.Match); r != "" {
				return tsqlDec{}, r
			}
			d, r := p.decOf(w.Then)
			if r != "" {
				return tsqlDec{}, r
			}
			decs = append(decs, d)
		}
		if x.Else != nil {
			d, r := p.decOf(x.Else)
			if r != "" {
				return tsqlDec{}, r
			}
			decs = append(decs, d)
		}
		return tsqlDecUnion(decs), ""
	case *ast.BinaryExpr:
		l, r1 := p.decOf(x.Lhs)
		if r1 != "" {
			return tsqlDec{}, r1
		}
		r, r2 := p.decOf(x.Rhs)
		if r2 != "" {
			return tsqlDec{}, r2
		}
		switch x.Op {
		case "+", "-", "*", "/", "%":
			d, why, roundTo := tsqlDecArith(x.Op, l, r)
			if roundTo >= 0 {
				if p.roundAt == nil {
					p.roundAt = map[*ast.BinaryExpr]int{}
				}
				p.roundAt[x] = roundTo
			}
			return d, why
		}
		return tsqlDec{}, ""
	}
	return tsqlDec{}, ""
}

// tsqlDecUnion is the type of a CASE / COALESCE over exact numbers:
// enough integer digits and decimals for every branch.
func tsqlDecUnion(decs []tsqlDec) tsqlDec {
	out := tsqlDec{ok: true}
	intDigits := 0
	for _, d := range decs {
		if !d.ok {
			if d.p == 0 && d.s == 0 && !d.money {
				// NULL or a non-number branch: no constraint.
				continue
			}
			return tsqlDec{}
		}
		if d.s > out.s {
			out.s = d.s
		}
		if d.p-d.s > intDigits {
			intDigits = d.p - d.s
		}
	}
	out.p = intDigits + out.s
	return out
}

// tsqlDecArith applies SQL Server's decimal result-type rules to l op r
// and reports the cases PG's exact arithmetic does not reproduce.
func tsqlDecArith(op string, l, r tsqlDec) (tsqlDec, string, int) {
	integer := func(d tsqlDec) bool { return d.ok && d.s == 0 && !d.money }
	if integer(l) && integer(r) {
		// Integer arithmetic (division truncates toward zero on both
		// sides).
		p := l.p
		if r.p > p {
			p = r.p
		}
		return tsqlDec{p: p, ok: true}, "", -1
	}
	if l.money && r.money && (op == "*" || op == "/") || (l.money || r.money) && op == "/" {
		return tsqlDec{}, "money " + op + " rounds the result to 4 decimals on SQL Server", -1
	}
	if op == "/" {
		return tsqlDec{}, "decimal division has a fixed result scale on SQL Server (max(6, s1 + p2 + 1)), PostgreSQL picks its own", -1
	}
	if !l.ok || !r.ok {
		if op == "*" {
			return tsqlDec{}, "decimal multiplication of operands of unknown precision (SQL Server reduces the scale past precision 38)", -1
		}
		return tsqlDec{}, "", -1
	}
	if (l.money && integer(r)) || (r.money && integer(l)) {
		// money with an integer stays money: exact, 4 decimals.
		return tsqlDec{p: 19, s: 4, money: true, ok: true}, "", -1
	}
	var p, s int
	switch op {
	case "+", "-":
		s = l.s
		if r.s > s {
			s = r.s
		}
		ip := l.p - l.s
		if r.p-r.s > ip {
			ip = r.p - r.s
		}
		p = s + ip + 1
	case "*":
		p, s = l.p+r.p+1, l.s+r.s
	case "%":
		s = l.s
		if r.s > s {
			s = r.s
		}
		ip := l.p - l.s
		if r.p-r.s < ip {
			ip = r.p - r.s
		}
		p = ip + s
	}
	if p > 38 {
		// SQL Server caps the precision and keeps
		// max(38 - integer digits, min(s, 6)) decimals: the value is
		// rounded when that is fewer than the exact result has.
		reduced := 38 - (p - s)
		if m := min(s, 6); m > reduced {
			reduced = m
		}
		if reduced < s && op == "*" {
			// A product is rounded half away from zero to the reduced
			// scale (measured on SQL Server 2022: 0.0005 * 0.0010 in
			// decimal(19,4) * decimal(20,4) gives 0.000001), which a CAST
			// to numeric(38, reduced) reproduces on PG (roundAt).
			return tsqlDec{p: 38, s: reduced, ok: true}, "", reduced
		}
		if reduced < s {
			return tsqlDec{}, "decimal " + op + " exceeds precision 38 on SQL Server, which then rounds the result to " + strconv.Itoa(reduced) + " decimals", -1
		}
		return tsqlDec{p: 38, s: s, ok: true}, "", -1
	}
	return tsqlDec{p: p, s: s, ok: true}, "", -1
}

// ---------------------------------------------------------------------------
// ISNULL replacement length
// ---------------------------------------------------------------------------

// tsqlTextLen is the declared length of a T-SQL string expression.
type tsqlTextLen struct {
	n         int
	unbounded bool // (MAX), text, ntext
	ok        bool
}

// isnullFits proves ISNULL(a, b) identical to PG's coalesce(a, b) for
// strings: SQL Server converts b to the type of a, truncating it to a's
// length. The rewriter casts the call back to a's type when a is a plain
// column (rw.isnullType); for any other a, b must provably fit.
func (p *tsqlComputedProof) isnullFits(a, b ast.Expr) string {
	la := p.textLen(a)
	if !la.ok && !p.isTextExpr(a) || la.unbounded {
		return ""
	}
	if id, ok := tsqlStripParens(a).(*ast.Ident); ok && len(id.Parts) == 1 && !id.Variable && la.ok {
		return ""
	}
	if lit, ok := tsqlStripParens(b).(*ast.Literal); ok && lit.Kind == "null" {
		return ""
	}
	lb := p.textLen(b)
	if la.ok && lb.ok && !lb.unbounded && lb.n <= la.n {
		return ""
	}
	return "ISNULL converts its replacement " + pgast.WriteExpr(b) + " to the type of its first argument (truncating it), PostgreSQL's coalesce does not"
}

// isTextExpr reports whether e is classified as a string by the proof.
func (p *tsqlComputedProof) isTextExpr(e ast.Expr) bool {
	c, r := p.source(e)
	return r == "" && (c == tsqlClassText || c == tsqlClassChar)
}

func (p *tsqlComputedProof) textLen(e ast.Expr) tsqlTextLen {
	switch x := e.(type) {
	case *ast.Literal:
		if x.Kind == "string" {
			return tsqlTextLen{n: utf8.RuneCountInString(x.Text), ok: true}
		}
	case *ast.ParenExpr:
		return p.textLen(x.Inner)
	case *ast.Ident:
		if len(x.Parts) == 1 && !x.Variable && p.types != nil {
			return p.t.tsqlTextLenOfType(p.types[normalizeTSQLIdent(x.Parts[0])], false)
		}
	case *ast.CastExpr:
		return p.t.tsqlTextLenOfType(x.Type, true)
	case *ast.BinaryExpr:
		if x.Op != "+" {
			return tsqlTextLen{}
		}
		l, r := p.textLen(x.Lhs), p.textLen(x.Rhs)
		switch {
		case !l.ok || !r.ok:
			return tsqlTextLen{}
		case l.unbounded || r.unbounded:
			return tsqlTextLen{unbounded: true, ok: true}
		}
		return tsqlTextLen{n: l.n + r.n, ok: true}
	case *ast.FuncCall:
		switch strings.ToUpper(x.Name) {
		case "UPPER", "LOWER", "LTRIM", "RTRIM", "ISNULL":
			if len(x.Args) > 0 {
				return p.textLen(x.Args[0])
			}
		}
		if dt := p.t.tsql.funcReturns[tsqlFuncKey(x.Name)]; dt != nil {
			return p.t.tsqlTextLenOfType(dt, false)
		}
	}
	return tsqlTextLen{}
}

// tsqlTextLenOfType is the length of a string data type; inCast selects
// the CAST / CONVERT default length (30) for a type without one.
func (t *translator) tsqlTextLenOfType(dt ast.DataType, inCast bool) tsqlTextLen {
	switch x := dt.(type) {
	case *ast.CharType:
		switch {
		case x.Max:
			return tsqlTextLen{unbounded: true, ok: true}
		case x.HasLength:
			return tsqlTextLen{n: x.Length, ok: true}
		case inCast:
			return tsqlTextLen{n: 30, ok: true}
		}
		return tsqlTextLen{n: 1, ok: true}
	case *ast.TextType:
		return tsqlTextLen{unbounded: true, ok: true}
	case *ast.TSQLType:
		switch x.Name {
		case "NTEXT":
			return tsqlTextLen{unbounded: true, ok: true}
		case "SYSNAME":
			return tsqlTextLen{n: 128, ok: true}
		}
	case *ast.UserDefinedType:
		if t.tsql != nil {
			if alias := t.tsql.aliasTypes[tsqlUserTypeKey(x.Schema, x.Name)]; alias != nil && alias.BaseType != nil {
				return t.tsqlTextLenOfType(alias.BaseType, inCast)
			}
		}
	}
	return tsqlTextLen{}
}

// applyRounding wraps, in place, every product tsqlDecArith found rounded
// by SQL Server's precision cap (roundAt) in CAST(… AS decimal(38, s)):
// the rewriter maps it to CAST(… AS numeric(38, s)), which rounds half
// away from zero exactly like SQL Server's reduced-scale product, so the
// generated column holds the stored value. It runs on the source tree,
// before the rewrite (the keys are its nodes).
func (p *tsqlComputedProof) applyRounding(e ast.Expr) ast.Expr {
	if len(p.roundAt) == 0 || e == nil {
		return e
	}
	switch x := e.(type) {
	case *ast.ParenExpr:
		x.Inner = p.applyRounding(x.Inner)
	case *ast.UnaryExpr:
		x.Rhs = p.applyRounding(x.Rhs)
	case *ast.CastExpr:
		x.Expr = p.applyRounding(x.Expr)
	case *ast.FuncCall:
		for i := range x.Args {
			x.Args[i] = p.applyRounding(x.Args[i])
		}
	case *ast.CaseExpr:
		x.Operand = p.applyRounding(x.Operand)
		for i := range x.Whens {
			x.Whens[i].Match = p.applyRounding(x.Whens[i].Match)
			x.Whens[i].Then = p.applyRounding(x.Whens[i].Then)
		}
		x.Else = p.applyRounding(x.Else)
	case *ast.BinaryExpr:
		x.Lhs = p.applyRounding(x.Lhs)
		x.Rhs = p.applyRounding(x.Rhs)
		if s, ok := p.roundAt[x]; ok {
			return &ast.CastExpr{
				Expr: &ast.ParenExpr{Inner: x, P: x.P},
				Type: &ast.DecimalType{Name: "DECIMAL", Precision: 38, Scale: s, HasPrec: true, P: x.P},
				P:    x.P,
			}
		}
	}
	return e
}
