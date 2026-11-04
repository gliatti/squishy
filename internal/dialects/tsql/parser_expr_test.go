package tsql

import (
	"testing"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

func TestExpr_Casts(t *testing.T) {
	c := as[*ast.CastExpr](t, mustExpr(t, "CAST(x AS varchar(10))"))
	if c.IsConvert || c.Try || as[*ast.CharType](t, c.Type).Length != 10 {
		t.Errorf("CAST: %+v", c)
	}
	tc := as[*ast.CastExpr](t, mustExpr(t, "TRY_CAST(x AS int)"))
	if !tc.Try {
		t.Errorf("TRY_CAST: %+v", tc)
	}
	cv := as[*ast.CastExpr](t, mustExpr(t, "CONVERT(datetime, '20030701', 112)"))
	if !cv.IsConvert || as[*ast.Literal](t, cv.ConvertStyle).Text != "112" || as[*ast.TSQLType](t, cv.Type).Name != "DATETIME" {
		t.Errorf("CONVERT: %+v", cv)
	}
	if as[*ast.Literal](t, cv.Expr).Text != "20030701" {
		t.Errorf("CONVERT operand: %+v", cv.Expr)
	}
	tcv := as[*ast.CastExpr](t, mustExpr(t, "TRY_CONVERT(int, @s)"))
	if !tcv.IsConvert || !tcv.Try || tcv.ConvertStyle != nil {
		t.Errorf("TRY_CONVERT: %+v", tcv)
	}
}

func TestExpr_Operators(t *testing.T) {
	cases := []struct {
		src, op string
	}{
		{"a + b", "+"}, {"a % b", "%"}, {"a & 0xf", "&"}, {"a | b", "|"}, {"a ^ b", "^"},
		{"a <> b", "<>"}, {"a != b", "!="}, {"a !< b", ">="}, {"a !> b", "<="},
		{"a LIKE 'x%'", "LIKE"}, {"a NOT LIKE 'x%'", "NOT LIKE"}, {"a IS NULL", "IS"},
		{"a IS NOT NULL", "IS NOT"}, {"a = 1 AND b = 2", "AND"}, {"a = 1 OR b = 2", "OR"},
	}
	for _, c := range cases {
		be := as[*ast.BinaryExpr](t, mustExpr(t, c.src))
		if be.Op != c.op {
			t.Errorf("%s: want op %q, got %q", c.src, c.op, be.Op)
		}
	}
	// sysstat & 0xf = 4: bitwise AND binds tighter than the comparison.
	eq := as[*ast.BinaryExpr](t, mustExpr(t, "sysstat & 0xf = 4"))
	if eq.Op != "=" || as[*ast.BinaryExpr](t, eq.Lhs).Op != "&" {
		t.Errorf("precedence: %+v", eq)
	}
	esc := as[*ast.BinaryExpr](t, mustExpr(t, `a LIKE '50!%' ESCAPE '!'`))
	if as[*ast.Literal](t, esc.Escape).Text != "!" {
		t.Errorf("ESCAPE: %+v", esc)
	}
	u := as[*ast.UnaryExpr](t, mustExpr(t, "NOT (a BETWEEN 1 AND 2)"))
	if u.Op != "NOT" {
		t.Errorf("NOT: %+v", u)
	}
	nb := as[*ast.BetweenExpr](t, mustExpr(t, "@x NOT BETWEEN @lo AND @hi"))
	if !nb.Not {
		t.Errorf("NOT BETWEEN: %+v", nb)
	}
	in := as[*ast.InExpr](t, mustExpr(t, "UPPER([g]) IN ('M', 'F')"))
	if len(in.List) != 2 || in.Not {
		t.Errorf("IN list: %+v", in)
	}
	sub := as[*ast.InExpr](t, mustExpr(t, "id NOT IN (SELECT id FROM t)"))
	if sub.Subquery == nil || !sub.Not {
		t.Errorf("NOT IN subquery: %+v", sub)
	}
	ex := as[*ast.ExistsExpr](t, mustExpr(t, "EXISTS (SELECT * FROM t)"))
	if ex.Subquery == nil {
		t.Errorf("EXISTS: %+v", ex)
	}
	all := as[*ast.BinaryExpr](t, mustExpr(t, "x > ALL (SELECT y FROM t)"))
	if f := as[*ast.FuncCall](t, all.Rhs); f.Name != "ALL" {
		t.Errorf("ALL (subquery): %+v", f)
	}
	at := as[*ast.BinaryExpr](t, mustExpr(t, "d AT TIME ZONE 'UTC'"))
	if at.Op != "AT TIME ZONE" {
		t.Errorf("AT TIME ZONE: %+v", at)
	}
}

func TestExpr_Primaries(t *testing.T) {
	v := as[*ast.Ident](t, mustExpr(t, "@ErrorLogID"))
	if !v.Variable || v.Parts[0] != "@ErrorLogID" {
		t.Errorf("variable: %+v", v)
	}
	g := as[*ast.Ident](t, mustExpr(t, "@@ROWCOUNT"))
	if !g.Variable || g.Parts[0] != "@@ROWCOUNT" {
		t.Errorf("system variable: %+v", g)
	}
	q := as[*ast.Ident](t, mustExpr(t, "[Sales].[T].[c]"))
	if len(q.Parts) != 3 || !q.Backtick {
		t.Errorf("qualified column: %+v", q)
	}
	n := as[*ast.Literal](t, mustExpr(t, "N'abc'"))
	if !n.NString || n.Kind != "string" {
		t.Errorf("national string: %+v", n)
	}
	m := as[*ast.Literal](t, mustExpr(t, "$20.00"))
	if m.Kind != "money" || m.Text != "20.00" {
		t.Errorf("money: %+v", m)
	}
	h := as[*ast.Literal](t, mustExpr(t, "0xFF00"))
	if h.Kind != "hex" || h.Text != "FF00" {
		t.Errorf("hex: %+v", h)
	}
	seq := as[*ast.SequenceRef](t, mustExpr(t, "NEXT VALUE FOR dbo.invoice_seq"))
	if seq.Schema != "dbo" || seq.Name != "invoice_seq" || seq.Op != "NEXTVAL" {
		t.Errorf("NEXT VALUE FOR: %+v", seq)
	}
	co := as[*ast.CollateExpr](t, mustExpr(t, "name COLLATE Latin1_General_BIN"))
	if co.Collation != "Latin1_General_BIN" || identText(t, co.Expr) != "name" {
		t.Errorf("COLLATE: %+v", co)
	}
	ce := as[*ast.CaseExpr](t, mustExpr(t, "CASE @Status WHEN 1 THEN N'a' WHEN 2 THEN N'b' ELSE N'c' END"))
	if ce.Operand == nil || len(ce.Whens) != 2 || ce.Else == nil {
		t.Errorf("simple CASE: %+v", ce)
	}
	se := as[*ast.CaseExpr](t, mustExpr(t, "CASE WHEN grouping(pub_id) = 1 THEN 'ALL' ELSE pub_id END"))
	if se.Operand != nil {
		t.Errorf("searched CASE: %+v", se)
	}
	sq := as[*ast.SubqueryExpr](t, mustExpr(t, "(SELECT SUM(x) FROM t)"))
	if sq.Stmt == nil {
		t.Errorf("scalar subquery: %+v", sq)
	}
}

func TestExpr_Functions(t *testing.T) {
	cases := []struct{ src, name string }{
		{"ISNULL(a, 0)", "ISNULL"}, {"IIF(a > 1, 'x', 'y')", "IIF"}, {"COALESCE(a, b)", "COALESCE"},
		{"NULLIF(a, 0)", "NULLIF"}, {"LEFT(name, 3)", "LEFT"}, {"RIGHT(name, 3)", "RIGHT"},
		{"getdate()", "GETDATE"}, {"DATEADD(day, 1, GETDATE())", "DATEADD"},
		{"dbo.ufnGetStock(@id)", "dbo.ufnGetStock"}, {"[dbo].[ufnX](1)", "dbo.ufnX"},
		{"UPDATE([BusinessEntityID])", "UPDATE"}, {"CURRENT_TIMESTAMP", "CURRENT_TIMESTAMP"},
		{"CURRENT_USER", "CURRENT_USER"}, {"COUNT(*)", "COUNT"},
	}
	for _, c := range cases {
		fc := as[*ast.FuncCall](t, mustExpr(t, c.src))
		if fc.Name != c.name {
			t.Errorf("%s: want name %q, got %q", c.src, c.name, fc.Name)
		}
	}
	d := as[*ast.FuncCall](t, mustExpr(t, "COUNT(DISTINCT a)"))
	if !d.Distinct {
		t.Errorf("COUNT(DISTINCT): %+v", d)
	}
	w := as[*ast.WindowedAgg](t, mustExpr(t, "ROW_NUMBER() OVER (PARTITION BY a, b ORDER BY c DESC ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW)"))
	if len(w.Over.PartitionBy) != 2 || !w.Over.OrderBy[0].Desc || w.Over.Frame != "ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW" {
		t.Errorf("OVER: %+v", w.Over)
	}
	sa := as[*ast.WindowedAgg](t, mustExpr(t, "STRING_AGG(name, ',') WITHIN GROUP (ORDER BY name)"))
	if len(sa.Within) != 1 || sa.Over != nil {
		t.Errorf("WITHIN GROUP: %+v", sa)
	}
}

func TestExpr_Methods(t *testing.T) {
	x := as[*ast.RawExpr](t, mustExpr(t, "[ContactInfo].ref.value(N'(/a)[1]', 'nvarchar(50)')"))
	if x.Kind != "XML_METHOD" || x.Text != "[ContactInfo].ref.value(N'(/a)[1]', 'nvarchar(50)')" {
		t.Errorf("xml value(): %+v", x)
	}
	xv := as[*ast.RawExpr](t, mustExpr(t, "@data.value('(/E)[1]', 'sysname')"))
	if xv.Kind != "XML_METHOD" {
		t.Errorf("xml method on a variable: %+v", xv)
	}
	h := as[*ast.MethodCall](t, mustExpr(t, "e.[OrganizationNode].GetAncestor(1)"))
	if h.Method != "GetAncestor" || len(h.Args) != 1 || identText(t, h.Target) != "e.OrganizationNode" {
		t.Errorf("hierarchyid method: %+v", h)
	}
	st := as[*ast.MethodCall](t, mustExpr(t, "hierarchyid::GetRoot()"))
	if st.StaticType != "hierarchyid" || st.Method != "GetRoot" || st.Target != nil {
		t.Errorf("static method: %+v", st)
	}
	ch := as[*ast.MethodCall](t, mustExpr(t, "@h.GetAncestor(1).ToString()"))
	if ch.Method != "ToString" {
		t.Errorf("chained method: %+v", ch)
	}
	as[*ast.MethodCall](t, ch.Target)
	p := as[*ast.MethodCall](t, mustExpr(t, "@geo.Lat"))
	if p.Method != "Lat" || p.Args != nil {
		t.Errorf("property: %+v", p)
	}
}

func TestParseExprTrailingTokens(t *testing.T) {
	if _, errs := ParseExpr("a + b c d"); len(errs) == 0 {
		t.Errorf("trailing tokens must be reported")
	}
}
