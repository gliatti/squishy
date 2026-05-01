package ast

// Build* helpers construct AST nodes for use inside Rewriters. They
// keep the visitor code readable: a Phase 3 visitor that turns
// `decode(x, 1, 'a', 'b')` into `oracle.decode(x, 1, 'a', 'b')` writes
//   return BuildFuncCall("oracle.decode", fc.Args...)
// rather than the equivalent struct-literal incantation.
//
// All helpers leave Position{} zero — visitors operate semantically and
// the source position of an artificial replacement node has no precise
// answer. Downstream warnings keyed on position should consult the
// parent node (which keeps its original position).

// BuildFuncCall constructs a *FuncCall with the given Name and Args.
func BuildFuncCall(name string, args ...Expr) *FuncCall {
	return &FuncCall{Name: name, Args: args}
}

// BuildIdent constructs a *Ident from a list of parts.
func BuildIdent(parts ...string) *Ident {
	return &Ident{Parts: parts}
}

// BuildStringLit constructs a string-kind *Literal. Text is the raw
// payload — quoting is added by the writer (rawExpr / sqlString).
func BuildStringLit(s string) *Literal {
	return &Literal{Kind: "string", Text: s}
}

// BuildIntLit constructs a numeric-kind *Literal. The text form is the
// canonical Go representation (FormatInt base 10).
func BuildIntLit(n int64) *Literal {
	return &Literal{Kind: "number", Text: formatInt(n)}
}

// BuildBinary constructs a *BinaryExpr.
func BuildBinary(op string, lhs, rhs Expr) *BinaryExpr {
	return &BinaryExpr{Op: op, Lhs: lhs, Rhs: rhs}
}

// BuildParen wraps inner in a *ParenExpr — useful when a substitution
// changes the operator-precedence level of an expression and the
// parent's render would otherwise mis-associate.
func BuildParen(inner Expr) *ParenExpr {
	return &ParenExpr{Inner: inner}
}

// BuildCase constructs a searched *CaseExpr (no operand). Each Whens
// pair maps a condition to its result; Else is the optional default
// branch. Use this when rewriting Oracle DECODE to ANSI CASE.
func BuildCase(whens []CaseExprWhen, elseExpr Expr) *CaseExpr {
	return &CaseExpr{Whens: whens, Else: elseExpr}
}

// formatInt is a tiny strconv-free int→string helper kept inside the
// package so build.go has no test-binary import surface.
func formatInt(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
