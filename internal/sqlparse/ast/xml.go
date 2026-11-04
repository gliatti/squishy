package ast

// ---------------------------------------------------------------------------
// XML: XQuery expressions (source side) and XMLTABLE (PostgreSQL side).
//
// T-SQL xml data-type methods take an XQuery string literal:
//
//	Resume.value(N'declare default element namespace "urn:r";
//	             (/Resume/Name/Name.First)[1]', 'nvarchar(30)')
//
// The T-SQL parser parses that literal once into the XQuery nodes below
// (MethodCall.XQuery) — the subset SQL Server's XML methods are written
// with: the namespace prolog, path expressions (axes, name / kind tests,
// predicates), filter expressions `(…)[n]`, function calls, literals,
// comparison / boolean / arithmetic operators. The translator rewrites
// that tree (default element namespace → explicit prefix, context of a
// nodes() row) and the PostgreSQL writer prints it as the XPath 1.0
// expression of an XMLTABLE / xpath() call. No XQuery text is ever
// inspected outside the parser.
// ---------------------------------------------------------------------------

// XQuery is a parsed XQuery main module: its prolog namespace
// declarations and its body expression.
type XQuery struct {
	// Namespaces are the `declare namespace p = "uri";` declarations,
	// in source order.
	Namespaces []XQNamespace
	// DefaultElementNS is the URI of `declare default element namespace
	// "uri";` ("" when absent): it qualifies every unprefixed element
	// name test of the body.
	DefaultElementNS string
	Body             XQExpr
}

// XQNamespace is one namespace binding (prefix → URI).
type XQNamespace struct {
	Prefix string
	URI    string
}

// XQExpr is an XQuery expression node.
type XQExpr interface{ xqExpr() }

// XQPath — a path expression: `/a/b`, `a//b[@x = 1]`, `(…)[1]/c`, `/`.
// Root is a leading `/` (the document node of the context); each step
// records whether `//` precedes it. A bare `/` is Root with no steps.
type XQPath struct {
	Root  bool
	Steps []*XQStep
}

// XQStep — one step of a path: an axis step (Test set) or a filter
// step (Primary set: `(expr)`, a function call, `.`), with predicates.
type XQStep struct {
	// Desc marks a step preceded by `//` (descendant-or-self::node()/).
	Desc bool
	// Axis is the axis of an axis step ("child", "attribute", "self",
	// "parent", "descendant", "descendant-or-self", "ancestor",
	// "ancestor-or-self", "following-sibling", "preceding-sibling",
	// "following", "preceding"); `@x` is the attribute axis and `..`
	// is parent::node().
	Axis    string
	Test    *XQNodeTest
	Primary XQExpr
	Preds   []XQExpr
}

// XQNodeTest — a name test (Kind "name": Prefix / Local, Local "*" for a
// wildcard) or a kind test (Kind "text", "node", "comment",
// "processing-instruction").
type XQNodeTest struct {
	Kind   string
	Prefix string
	Local  string
}

// XQContextItem — `.`.
type XQContextItem struct{}

// XQFuncCall — `string(.)`, `fn:data(x)`, `count(a/b)`.
type XQFuncCall struct {
	Prefix string
	Name   string
	Args   []XQExpr
}

// XQLiteral — a string literal (String true; Text is the unescaped
// value) or a numeric literal (Text as written).
type XQLiteral struct {
	String bool
	Text   string
}

// XQBinary — `a = b`, `a eq b`, `a and b`, `a + b`, `a div b`, `a | b`.
type XQBinary struct {
	Op       string
	Lhs, Rhs XQExpr
}

// XQUnary — `-x`.
type XQUnary struct {
	Op   string
	Expr XQExpr
}

// XQParen — `( expr )`.
type XQParen struct {
	Inner XQExpr
}

func (*XQPath) xqExpr()        {}
func (*XQContextItem) xqExpr() {}
func (*XQFuncCall) xqExpr()    {}
func (*XQLiteral) xqExpr()     {}
func (*XQBinary) xqExpr()      {}
func (*XQUnary) xqExpr()       {}
func (*XQParen) xqExpr()       {}

// FromXMLTable — PostgreSQL `XMLTABLE(XMLNAMESPACES('uri' AS p, …),
// 'row path' PASSING expr COLUMNS c type PATH 'path', …) AS alias`. It
// is output-only: no parser produces it, the T-SQL rewriter builds it
// for xml.nodes() (a FROM / APPLY source) and inside the scalar
// subqueries of xml.value() / exist() / query(). RowPath and the column
// paths are XPath 1.0 text the rewriter rendered from typed XQuery
// nodes.
type FromXMLTable struct {
	Namespaces []XQNamespace
	RowPath    string
	Passing    Expr
	Columns    []XMLTableColumn
	Alias      string
	P          Position
}

func (f *FromXMLTable) Pos() Position { return f.P }
func (f *FromXMLTable) fromItemNode() {}

// XMLTableColumn is one COLUMNS entry: `name type PATH 'path'`, or
// `name FOR ORDINALITY` (Ordinality set, Type and Path empty). Type is
// PostgreSQL type text.
type XMLTableColumn struct {
	Name       string
	Type       string
	Path       string
	Ordinality bool
}
