package tsql

import (
	"fmt"
	"strings"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// xquery.go — hand-rolled lexer and parser of the XQuery argument of the
// xml data-type methods (`x.value(N'…', 'type')`, `x.nodes(N'…')`,
// `x.exist(N'…')`, `x.query(N'…')`).
//
// SQL Server's XQuery is a large language; the methods are written with
// a small part of it, which is what this parser accepts: the prolog
// namespace declarations (`declare namespace p = "uri";`, `declare
// default element namespace "uri";`) and one expression made of path
// expressions (abbreviated and full axes, name / kind tests,
// predicates), filter expressions (`(/a/b)[1]`), function calls,
// literals and the comparison / boolean / arithmetic / union operators.
// FLWOR, constructors, sequences, sql:column() / sql:variable() and XML
// DML (modify()) are refused with an error, which the translator reports
// as an untranslated construct. The literal is lexed here character by
// character — like the T-SQL lexer reads SQL text — and nothing
// downstream ever looks at the XQuery text again.

// ParseXQuery parses the XQuery text of an xml method argument.
func ParseXQuery(src string) (*ast.XQuery, error) {
	toks, err := lexXQuery(src)
	if err != nil {
		return nil, err
	}
	p := &xqParser{toks: toks}
	q := &ast.XQuery{}
	if err := p.prolog(q); err != nil {
		return nil, err
	}
	body, err := p.expr()
	if err != nil {
		return nil, err
	}
	if !p.at(xqEOF, "") {
		return nil, p.errf("unexpected %q", p.cur().text)
	}
	q.Body = body
	return q, nil
}

// XQueryError is a lexical or syntax error of an XQuery literal.
type XQueryError struct {
	Offset int
	Msg    string
}

func (e *XQueryError) Error() string { return "XQuery: " + e.Msg }

type xqKind int

const (
	xqEOF    xqKind = iota
	xqName          // NCName or QName (prefix:local), text as written
	xqString        // string literal, text unescaped
	xqNumber        // numeric literal
	xqPunct         // operators and delimiters
)

type xqTok struct {
	kind xqKind
	text string
	// prefix / local split a QName token (prefix "" for an NCName).
	prefix, local string
	off           int
}

func xqNameStart(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r > 0x7f
}

func xqNameChar(r rune) bool {
	return xqNameStart(r) || (r >= '0' && r <= '9') || r == '.' || r == '-'
}

func xqDigit(r rune) bool { return r >= '0' && r <= '9' }

// lexXQuery splits src into tokens (comments `(: … :)` dropped).
func lexXQuery(src string) ([]xqTok, error) {
	rs := []rune(src)
	var out []xqTok
	i := 0
	errAt := func(at int, msg string) error { return &XQueryError{Offset: at, Msg: msg} }
	for i < len(rs) {
		r := rs[i]
		switch {
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			i++
			continue
		case r == '(' && i+1 < len(rs) && rs[i+1] == ':':
			depth := 0
			start := i
			for i < len(rs) {
				if rs[i] == '(' && i+1 < len(rs) && rs[i+1] == ':' {
					depth++
					i += 2
					continue
				}
				if rs[i] == ':' && i+1 < len(rs) && rs[i+1] == ')' {
					depth--
					i += 2
					if depth == 0 {
						break
					}
					continue
				}
				i++
			}
			if depth != 0 {
				return nil, errAt(start, "unterminated comment")
			}
			continue
		case r == '"' || r == '\'':
			q := r
			start := i
			i++
			var b strings.Builder
			closed := false
			for i < len(rs) {
				if rs[i] == q {
					if i+1 < len(rs) && rs[i+1] == q {
						b.WriteRune(q)
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				if rs[i] == '&' {
					// Predefined entity references of XQuery string
					// literals.
					ent, n := xqEntity(rs[i:])
					if n == 0 {
						return nil, errAt(i, "unsupported entity reference in a string literal")
					}
					b.WriteString(ent)
					i += n
					continue
				}
				b.WriteRune(rs[i])
				i++
			}
			if !closed {
				return nil, errAt(start, "unterminated string literal")
			}
			out = append(out, xqTok{kind: xqString, text: b.String(), off: start})
			continue
		case xqDigit(r) || (r == '.' && i+1 < len(rs) && xqDigit(rs[i+1])):
			start := i
			for i < len(rs) && (xqDigit(rs[i]) || rs[i] == '.') {
				i++
			}
			if i < len(rs) && (rs[i] == 'e' || rs[i] == 'E') {
				i++
				if i < len(rs) && (rs[i] == '+' || rs[i] == '-') {
					i++
				}
				for i < len(rs) && xqDigit(rs[i]) {
					i++
				}
			}
			out = append(out, xqTok{kind: xqNumber, text: string(rs[start:i]), off: start})
			continue
		case xqNameStart(r):
			start := i
			for i < len(rs) && xqNameChar(rs[i]) {
				i++
			}
			local := string(rs[start:i])
			prefix := ""
			// QName: prefix ':' local with no blank around the colon.
			// `::` is the axis separator, not a QName.
			if i+1 < len(rs) && rs[i] == ':' && rs[i+1] != ':' && (xqNameStart(rs[i+1]) || rs[i+1] == '*') {
				prefix = local
				i++
				if rs[i] == '*' {
					local = "*"
					i++
				} else {
					ls := i
					for i < len(rs) && xqNameChar(rs[i]) {
						i++
					}
					local = string(rs[ls:i])
				}
			}
			text := local
			if prefix != "" {
				text = prefix + ":" + local
			}
			out = append(out, xqTok{kind: xqName, text: text, prefix: prefix, local: local, off: start})
			continue
		}
		// Punctuation, longest match first.
		two := ""
		if i+1 < len(rs) {
			two = string(rs[i : i+2])
		}
		switch two {
		case "//", "::", "!=", "<=", ">=", "..":
			out = append(out, xqTok{kind: xqPunct, text: two, off: i})
			i += 2
			continue
		}
		switch r {
		case '/', '[', ']', '(', ')', '@', ',', ';', '=', '<', '>', '|', '+', '-', '*', '.', '$', '{', '}', ':':
			out = append(out, xqTok{kind: xqPunct, text: string(r), off: i})
			i++
			continue
		}
		return nil, errAt(i, "unexpected character "+string(r))
	}
	out = append(out, xqTok{kind: xqEOF, off: len(rs)})
	return out, nil
}

// xqEntity decodes a predefined entity or character reference at the
// start of rs; n is the number of runes consumed (0: none).
func xqEntity(rs []rune) (string, int) {
	end := -1
	for k := 1; k < len(rs) && k < 12; k++ {
		if rs[k] == ';' {
			end = k
			break
		}
	}
	if end < 0 {
		return "", 0
	}
	name := string(rs[1:end])
	switch name {
	case "lt":
		return "<", end + 1
	case "gt":
		return ">", end + 1
	case "amp":
		return "&", end + 1
	case "quot":
		return `"`, end + 1
	case "apos":
		return "'", end + 1
	}
	if len(name) > 1 && name[0] == '#' {
		v, base, digits := 0, 10, name[1:]
		if digits != "" && (digits[0] == 'x') {
			base, digits = 16, digits[1:]
		}
		if digits == "" {
			return "", 0
		}
		for _, c := range digits {
			d := -1
			switch {
			case c >= '0' && c <= '9':
				d = int(c - '0')
			case base == 16 && c >= 'a' && c <= 'f':
				d = int(c-'a') + 10
			case base == 16 && c >= 'A' && c <= 'F':
				d = int(c-'A') + 10
			}
			if d < 0 {
				return "", 0
			}
			v = v*base + d
		}
		return string(rune(v)), end + 1
	}
	return "", 0
}

type xqParser struct {
	toks []xqTok
	i    int
}

func (p *xqParser) cur() xqTok { return p.toks[p.i] }

func (p *xqParser) peek(n int) xqTok {
	if p.i+n < len(p.toks) {
		return p.toks[p.i+n]
	}
	return p.toks[len(p.toks)-1]
}

func (p *xqParser) advance() xqTok {
	t := p.toks[p.i]
	if p.i < len(p.toks)-1 {
		p.i++
	}
	return t
}

// at reports whether the current token has kind k (and text, when not
// empty).
func (p *xqParser) at(k xqKind, text string) bool {
	t := p.cur()
	return t.kind == k && (text == "" || t.text == text)
}

func (p *xqParser) isPunct(text string) bool { return p.at(xqPunct, text) }

// isWord reports whether the current token is the unprefixed name w (an
// operator / keyword in operator position).
func (p *xqParser) isWord(w string) bool {
	t := p.cur()
	return t.kind == xqName && t.prefix == "" && t.local == w
}

func (p *xqParser) errf(format string, args ...any) error {
	return &XQueryError{Offset: p.cur().off, Msg: fmt.Sprintf(format, args...)}
}

func (p *xqParser) expect(text string) error {
	if !p.isPunct(text) {
		return p.errf("expected %q", text)
	}
	p.advance()
	return nil
}

// prolog parses the namespace declarations.
func (p *xqParser) prolog(q *ast.XQuery) error {
	for p.isWord("declare") {
		p.advance()
		switch {
		case p.isWord("namespace"):
			p.advance()
			if !p.at(xqName, "") || p.cur().prefix != "" {
				return p.errf("expected a namespace prefix")
			}
			prefix := p.advance().local
			if err := p.expect("="); err != nil {
				return err
			}
			if !p.at(xqString, "") {
				return p.errf("expected a namespace URI")
			}
			q.Namespaces = append(q.Namespaces, ast.XQNamespace{Prefix: prefix, URI: p.advance().text})
		case p.isWord("default"):
			p.advance()
			if !p.isWord("element") {
				return p.errf("only `declare default element namespace` is supported")
			}
			p.advance()
			if !p.isWord("namespace") {
				return p.errf("expected namespace")
			}
			p.advance()
			if !p.at(xqString, "") {
				return p.errf("expected a namespace URI")
			}
			q.DefaultElementNS = p.advance().text
		default:
			return p.errf("unsupported prolog declaration %q", p.cur().text)
		}
		if err := p.expect(";"); err != nil {
			return err
		}
	}
	return nil
}

// xqKeywordStarts are XQuery constructs outside the supported subset.
var xqKeywordStarts = map[string]string{
	"for": "FLWOR expression", "let": "FLWOR expression", "some": "quantified expression",
	"every": "quantified expression", "if": "conditional expression",
	"insert": "XML DML", "delete": "XML DML", "replace": "XML DML",
	"element": "computed constructor", "attribute": "computed constructor", "text": "computed constructor",
}

func (p *xqParser) expr() (ast.XQExpr, error) {
	e, err := p.orExpr()
	if err != nil {
		return nil, err
	}
	if p.isPunct(",") {
		return nil, p.errf("sequence expressions are not supported")
	}
	return e, nil
}

func (p *xqParser) orExpr() (ast.XQExpr, error) {
	l, err := p.andExpr()
	if err != nil {
		return nil, err
	}
	for p.isWord("or") {
		p.advance()
		r, err := p.andExpr()
		if err != nil {
			return nil, err
		}
		l = &ast.XQBinary{Op: "or", Lhs: l, Rhs: r}
	}
	return l, nil
}

func (p *xqParser) andExpr() (ast.XQExpr, error) {
	l, err := p.compExpr()
	if err != nil {
		return nil, err
	}
	for p.isWord("and") {
		p.advance()
		r, err := p.compExpr()
		if err != nil {
			return nil, err
		}
		l = &ast.XQBinary{Op: "and", Lhs: l, Rhs: r}
	}
	return l, nil
}

var xqCompPuncts = map[string]bool{"=": true, "!=": true, "<": true, "<=": true, ">": true, ">=": true}
var xqCompWords = map[string]bool{"eq": true, "ne": true, "lt": true, "le": true, "gt": true, "ge": true}

func (p *xqParser) compExpr() (ast.XQExpr, error) {
	l, err := p.addExpr()
	if err != nil {
		return nil, err
	}
	t := p.cur()
	if (t.kind == xqPunct && xqCompPuncts[t.text]) || (t.kind == xqName && t.prefix == "" && xqCompWords[t.local]) {
		p.advance()
		r, err := p.addExpr()
		if err != nil {
			return nil, err
		}
		return &ast.XQBinary{Op: t.text, Lhs: l, Rhs: r}, nil
	}
	return l, nil
}

func (p *xqParser) addExpr() (ast.XQExpr, error) {
	l, err := p.mulExpr()
	if err != nil {
		return nil, err
	}
	for p.isPunct("+") || p.isPunct("-") {
		op := p.advance().text
		r, err := p.mulExpr()
		if err != nil {
			return nil, err
		}
		l = &ast.XQBinary{Op: op, Lhs: l, Rhs: r}
	}
	return l, nil
}

func (p *xqParser) mulExpr() (ast.XQExpr, error) {
	l, err := p.unaryExpr()
	if err != nil {
		return nil, err
	}
	for p.isPunct("*") || p.isWord("div") || p.isWord("mod") || p.isWord("idiv") {
		op := p.advance().text
		r, err := p.unaryExpr()
		if err != nil {
			return nil, err
		}
		l = &ast.XQBinary{Op: op, Lhs: l, Rhs: r}
	}
	return l, nil
}

func (p *xqParser) unaryExpr() (ast.XQExpr, error) {
	if p.isPunct("-") || p.isPunct("+") {
		op := p.advance().text
		e, err := p.unaryExpr()
		if err != nil {
			return nil, err
		}
		return &ast.XQUnary{Op: op, Expr: e}, nil
	}
	return p.unionExpr()
}

func (p *xqParser) unionExpr() (ast.XQExpr, error) {
	l, err := p.pathExpr()
	if err != nil {
		return nil, err
	}
	for p.isPunct("|") || p.isWord("union") {
		p.advance()
		r, err := p.pathExpr()
		if err != nil {
			return nil, err
		}
		l = &ast.XQBinary{Op: "|", Lhs: l, Rhs: r}
	}
	return l, nil
}

// pathExpr parses `/`, `/rel`, `//rel` or `rel`. A path made of one
// filter step without predicates is returned as its primary expression.
func (p *xqParser) pathExpr() (ast.XQExpr, error) {
	path := &ast.XQPath{}
	desc := false
	switch {
	case p.isPunct("/"):
		p.advance()
		path.Root = true
		if !p.stepStarts() {
			return path, nil
		}
	case p.isPunct("//"):
		p.advance()
		path.Root = true
		desc = true
	}
	for {
		st, err := p.step()
		if err != nil {
			return nil, err
		}
		st.Desc = desc
		path.Steps = append(path.Steps, st)
		switch {
		case p.isPunct("/"):
			p.advance()
			desc = false
		case p.isPunct("//"):
			p.advance()
			desc = true
		default:
			if !path.Root && len(path.Steps) == 1 && path.Steps[0].Primary != nil && len(path.Steps[0].Preds) == 0 {
				return path.Steps[0].Primary, nil
			}
			return path, nil
		}
	}
}

// stepStarts reports whether the current token can start a step (after
// a leading `/`: a bare `/` is the root).
func (p *xqParser) stepStarts() bool {
	t := p.cur()
	switch t.kind {
	case xqName, xqString, xqNumber:
		return true
	case xqPunct:
		switch t.text {
		case "@", "*", ".", "..", "(":
			return true
		}
	}
	return false
}

var xqAxes = map[string]bool{
	"child": true, "attribute": true, "self": true, "parent": true, "descendant": true,
	"descendant-or-self": true, "ancestor": true, "ancestor-or-self": true,
	"following-sibling": true, "preceding-sibling": true, "following": true, "preceding": true,
}

var xqKindTests = map[string]bool{"text": true, "node": true, "comment": true, "processing-instruction": true}

func (p *xqParser) step() (*ast.XQStep, error) {
	st := &ast.XQStep{}
	t := p.cur()
	switch {
	case t.kind == xqPunct && t.text == "..":
		p.advance()
		st.Axis, st.Test = "parent", &ast.XQNodeTest{Kind: "node"}
	case t.kind == xqPunct && t.text == "@":
		p.advance()
		test, err := p.nodeTest()
		if err != nil {
			return nil, err
		}
		st.Axis, st.Test = "attribute", test
	case t.kind == xqName && t.prefix == "" && xqAxes[t.local] && p.peek(1).kind == xqPunct && p.peek(1).text == "::":
		p.advance()
		p.advance()
		test, err := p.nodeTest()
		if err != nil {
			return nil, err
		}
		st.Axis, st.Test = t.local, test
	case t.kind == xqPunct && t.text == "*":
		p.advance()
		st.Axis, st.Test = "child", &ast.XQNodeTest{Kind: "name", Local: "*"}
	case t.kind == xqName && p.peek(1).kind == xqPunct && p.peek(1).text == "(" && !(t.prefix == "" && xqKindTests[t.local]):
		if t.prefix == "" && xqKeywordStarts[t.local] != "" {
			return nil, p.errf("%s is not supported", xqKeywordStarts[t.local])
		}
		if t.prefix == "sql" {
			return nil, p.errf("sql:column() / sql:variable() are not supported")
		}
		call, err := p.funcCall()
		if err != nil {
			return nil, err
		}
		st.Primary = call
	case t.kind == xqName:
		if nx := p.peek(1); t.prefix == "" && xqKeywordStarts[t.local] != "" &&
			(nx.kind == xqName || nx.kind == xqString || (nx.kind == xqPunct && (nx.text == "$" || nx.text == "<" || nx.text == "{"))) {
			return nil, p.errf("%s is not supported", xqKeywordStarts[t.local])
		}
		test, err := p.nodeTest()
		if err != nil {
			return nil, err
		}
		st.Axis, st.Test = "child", test
	default:
		prim, err := p.primary()
		if err != nil {
			return nil, err
		}
		st.Primary = prim
	}
	for p.isPunct("[") {
		p.advance()
		pred, err := p.expr()
		if err != nil {
			return nil, err
		}
		if err := p.expect("]"); err != nil {
			return nil, err
		}
		st.Preds = append(st.Preds, pred)
	}
	return st, nil
}

func (p *xqParser) nodeTest() (*ast.XQNodeTest, error) {
	t := p.cur()
	switch {
	case t.kind == xqPunct && t.text == "*":
		p.advance()
		return &ast.XQNodeTest{Kind: "name", Local: "*"}, nil
	case t.kind == xqName && t.prefix == "" && xqKindTests[t.local] && p.peek(1).kind == xqPunct && p.peek(1).text == "(":
		p.advance()
		p.advance()
		if err := p.expect(")"); err != nil {
			return nil, p.errf("kind tests with arguments are not supported")
		}
		return &ast.XQNodeTest{Kind: t.local}, nil
	case t.kind == xqName:
		p.advance()
		return &ast.XQNodeTest{Kind: "name", Prefix: t.prefix, Local: t.local}, nil
	}
	return nil, p.errf("expected a node test, got %q", t.text)
}

func (p *xqParser) funcCall() (ast.XQExpr, error) {
	name := p.advance()
	if err := p.expect("("); err != nil {
		return nil, err
	}
	call := &ast.XQFuncCall{Prefix: name.prefix, Name: name.local}
	if !p.isPunct(")") {
		for {
			a, err := p.orExpr()
			if err != nil {
				return nil, err
			}
			call.Args = append(call.Args, a)
			if !p.isPunct(",") {
				break
			}
			p.advance()
		}
	}
	if err := p.expect(")"); err != nil {
		return nil, err
	}
	return call, nil
}

func (p *xqParser) primary() (ast.XQExpr, error) {
	t := p.cur()
	switch {
	case t.kind == xqString:
		p.advance()
		return &ast.XQLiteral{String: true, Text: t.text}, nil
	case t.kind == xqNumber:
		p.advance()
		return &ast.XQLiteral{Text: t.text}, nil
	case t.kind == xqPunct && t.text == ".":
		p.advance()
		return &ast.XQContextItem{}, nil
	case t.kind == xqPunct && t.text == "(":
		p.advance()
		if p.isPunct(")") {
			return nil, p.errf("the empty sequence () is not supported")
		}
		inner, err := p.expr()
		if err != nil {
			return nil, err
		}
		if err := p.expect(")"); err != nil {
			return nil, err
		}
		return &ast.XQParen{Inner: inner}, nil
	case t.kind == xqPunct && t.text == "$":
		return nil, p.errf("XQuery variables are not supported")
	case t.kind == xqPunct && t.text == "<":
		return nil, p.errf("XML constructors are not supported")
	}
	return nil, p.errf("unexpected %q", t.text)
}
