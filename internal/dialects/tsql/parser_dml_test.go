package tsql

import (
	"testing"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

func TestSelect_TopOffsetFor(t *testing.T) {
	s := mustSelect(t, "SELECT DISTINCT TOP (10) PERCENT WITH TIES a FROM t ORDER BY a DESC")
	if !s.Distinct || s.Top == nil || !s.TopPercent || !s.TopWithTies || !s.OrderBy[0].Desc {
		t.Errorf("TOP: %+v", s)
	}
	as[*ast.ParenExpr](t, s.Top)
	legacy := mustSelect(t, "SELECT TOP 1 1 FROM sys.databases")
	if as[*ast.Literal](t, legacy.Top).Text != "1" || as[*ast.Literal](t, legacy.Cols[0].Expr).Text != "1" {
		t.Errorf("legacy TOP: %+v", legacy)
	}
	off := mustSelect(t, "SELECT a FROM t ORDER BY a OFFSET 10 ROWS FETCH NEXT 5 ROWS ONLY")
	if as[*ast.Literal](t, off.Offset).Text != "10" || as[*ast.Literal](t, off.Limit).Text != "5" {
		t.Errorf("OFFSET / FETCH: %+v", off)
	}
	x := mustSelect(t, "SELECT a FROM t FOR XML PATH(''), TYPE")
	if x.ForXMLKind != "XML" || x.ForXML != "FOR XML PATH(''), TYPE" {
		t.Errorf("FOR XML: %q %q", x.ForXMLKind, x.ForXML)
	}
	j := mustSelect(t, "SELECT a FROM t FOR JSON PATH, ROOT('r') OPTION (MAXRECURSION 25)")
	if j.ForXMLKind != "JSON" || j.OptionClause != "OPTION (MAXRECURSION 25)" {
		t.Errorf("FOR JSON / OPTION: %+v", j)
	}
	into := mustSelect(t, "SELECT a, b INTO #tmp FROM t")
	if into.IntoTable == nil || into.IntoTable.Name != "#tmp" {
		t.Errorf("SELECT INTO: %+v", into)
	}
}

func TestSelect_Items(t *testing.T) {
	s := mustSelect(t, `SELECT *, p.*, s.t.*, [StateProvinceName] = sp.[Name], Total=SUM(q), a AS x, b y, c 'z', d AS 'w' FROM t`)
	if len(s.Cols) != 9 {
		t.Fatalf("want 9 items, got %d", len(s.Cols))
	}
	if !s.Cols[0].Star || !s.Cols[1].Star || s.Cols[1].Qualifier != "p" || s.Cols[2].Qualifier != "s.t" {
		t.Errorf("stars: %+v", s.Cols[:3])
	}
	for i, alias := range []string{"StateProvinceName", "Total", "x", "y", "z", "w"} {
		if s.Cols[3+i].Alias != alias {
			t.Errorf("item %d: want alias %q, got %q", 3+i, alias, s.Cols[3+i].Alias)
		}
	}
	if identText(t, s.Cols[3].Expr) != "sp.Name" {
		t.Errorf("alias = expr: %+v", s.Cols[3].Expr)
	}
}

func TestSelect_CTE(t *testing.T) {
	s := mustSelect(t, `WITH [BOM_cte]([a], [lvl]) AS (
    SELECT a, 0 FROM b
    UNION ALL
    SELECT c.a, [lvl] + 1 FROM [BOM_cte] c INNER JOIN b ON b.x = c.a
), other AS (SELECT 1 AS one)
SELECT a FROM [BOM_cte] OPTION (MAXRECURSION 25)`)
	if s.With == nil || len(s.With.CTEs) != 2 || !s.With.Recursive {
		t.Fatalf("recursive CTE: %+v", s.With)
	}
	cte := s.With.CTEs[0]
	if cte.Name != "BOM_cte" || len(cte.Columns) != 2 || len(cte.Body.SetOps) != 1 || cte.Body.SetOps[0].Op != "UNION ALL" {
		t.Errorf("CTE: %+v", cte)
	}
	plain := mustSelect(t, "WITH c AS (SELECT 1 AS a) SELECT a FROM c")
	if plain.With.Recursive {
		t.Errorf("non-recursive CTE flagged recursive")
	}
}

func TestSelect_FromSources(t *testing.T) {
	s := mustSelect(t, `SELECT 1
FROM [Person].[Person] AS p WITH (NOLOCK)
    INNER LOOP JOIN Sales.Customer c (nolock) ON c.PersonID = p.BusinessEntityID
    LEFT OUTER JOIN (SELECT id FROM x) AS d (k) ON d.k = p.id
    CROSS APPLY dbo.ufnGetContactInformation(p.id) AS ci
    OUTER APPLY p.[Demographics].nodes(N'/a') AS T(ref)
    CROSS JOIN @tv tv
    FULL JOIN (VALUES (1, 'a'), (2, 'b')) AS v (n, s) ON v.n = tv.n`)
	if len(s.From) != 1 {
		t.Fatalf("want one FROM tree, got %d", len(s.From))
	}
	kinds := []ast.JoinKind{}
	var item ast.FromItem = s.From[0]
	for {
		j, ok := item.(*ast.FromJoin)
		if !ok {
			break
		}
		kinds = append([]ast.JoinKind{j.Kind}, kinds...)
		switch j.Kind {
		case ast.CrossApply:
			ff := as[*ast.FromFunc](t, j.Right)
			if as[*ast.FuncCall](t, ff.Func).Name != "dbo.ufnGetContactInformation" || ff.Alias != "ci" {
				t.Errorf("CROSS APPLY function: %+v", ff)
			}
		case ast.OuterApply:
			ff := as[*ast.FromFunc](t, j.Right)
			if as[*ast.RawExpr](t, ff.Func).Kind != "XML_METHOD" || ff.Alias != "T" || len(ff.Cols) != 1 {
				t.Errorf("OUTER APPLY nodes(): %+v", ff)
			}
			if j.On != nil {
				t.Errorf("APPLY has no ON")
			}
		case ast.FullJoin:
			fv := as[*ast.FromValues](t, j.Right)
			if len(fv.Rows) != 2 || fv.Alias != "v" || len(fv.Cols) != 2 {
				t.Errorf("VALUES source: %+v", fv)
			}
		case ast.LeftJoin:
			fs := as[*ast.FromSubquery](t, j.Right)
			if fs.Alias != "d" || len(fs.Cols) != 1 {
				t.Errorf("derived table: %+v", fs)
			}
		case ast.CrossJoin:
			if ft := as[*ast.FromTable](t, j.Right); ft.Name != "@tv" || ft.Alias != "tv" {
				t.Errorf("table variable: %+v", ft)
			}
		case ast.InnerJoin:
			if ft := as[*ast.FromTable](t, j.Right); ft.Name != "Customer" || ft.Alias != "c" {
				t.Errorf("hinted join table: %+v", ft)
			}
			if lt := as[*ast.FromTable](t, j.Left); lt.Alias != "p" {
				t.Errorf("hinted base table: %+v", lt)
			}
		}
		item = j.Left
	}
	wantKinds := []ast.JoinKind{ast.InnerJoin, ast.LeftJoin, ast.CrossApply, ast.OuterApply, ast.CrossJoin, ast.FullJoin}
	if len(kinds) != len(wantKinds) {
		t.Fatalf("join kinds: want %v, got %v", wantKinds, kinds)
	}
	for i := range kinds {
		if kinds[i] != wantKinds[i] {
			t.Errorf("join %d: want %v, got %v", i, wantKinds[i], kinds[i])
		}
	}
}

func TestSelect_NestedJoins(t *testing.T) {
	s := mustSelect(t, `SELECT 1 FROM Shippers INNER JOIN
	(Products INNER JOIN (Orders INNER JOIN "Order Details" ON Orders.OrderID = "Order Details".OrderID)
	ON Products.ProductID = "Order Details".ProductID)
	ON Shippers.ShipperID = Orders.ShipVia`)
	top := as[*ast.FromJoin](t, s.From[0])
	if as[*ast.FromTable](t, top.Left).Name != "Shippers" {
		t.Errorf("left: %+v", top.Left)
	}
	mid := as[*ast.FromJoin](t, top.Right)
	inner := as[*ast.FromJoin](t, mid.Right)
	if as[*ast.FromTable](t, inner.Right).Name != "Order Details" {
		t.Errorf("innermost: %+v", inner.Right)
	}
	ansi := mustSelect(t, "SELECT 1 FROM a JOIN b JOIN c ON c.x = b.x ON b.y = a.y")
	outer := as[*ast.FromJoin](t, ansi.From[0])
	as[*ast.FromJoin](t, outer.Right)
	if as[*ast.FromTable](t, outer.Left).Name != "a" {
		t.Errorf("right-nested join: %+v", outer)
	}
	comma := mustSelect(t, "SELECT ProductName FROM Products P, [Order Details] Od WHERE Od.ProductID = P.ProductID")
	if len(comma.From) != 2 || as[*ast.FromTable](t, comma.From[1]).Alias != "Od" {
		t.Errorf("comma join: %+v", comma.From)
	}
}

func TestSelect_PivotGroupBy(t *testing.T) {
	s := mustSelect(t, `SELECT pvt.[2002] FROM (SELECT y, v FROM t) AS soh
PIVOT (SUM([v]) FOR [y] IN ([2002], [2003])) AS pvt`)
	pv := as[*ast.FromPivot](t, s.From[0])
	if pv.Unpivot || pv.ForCol != "y" || len(pv.InCols) != 2 || pv.InCols[0] != "2002" || pv.Alias != "pvt" {
		t.Errorf("PIVOT: %+v", pv)
	}
	as[*ast.FuncCall](t, pv.Agg)
	as[*ast.FromSubquery](t, pv.Source)
	u := mustSelect(t, "SELECT * FROM t UNPIVOT (val FOR col IN (a, b)) AS u")
	up := as[*ast.FromPivot](t, u.From[0])
	if !up.Unpivot || up.ValueCol != "val" || up.ForCol != "col" {
		t.Errorf("UNPIVOT: %+v", up)
	}
	g := mustSelect(t, "select pub_id, avg(price) from titles group by pub_id, type with rollup")
	if len(g.GroupBy) != 1 {
		t.Fatalf("WITH ROLLUP: %+v", g.GroupBy)
	}
	r := as[*ast.FuncCall](t, g.GroupBy[0])
	if r.Name != "ROLLUP" || len(r.Args) != 2 {
		t.Errorf("WITH ROLLUP as ROLLUP(): %+v", r)
	}
}

func TestInsert(t *testing.T) {
	ins, errs := ParseInsert(`INSERT INTO [Production].[TransactionHistory] ([ProductID], [Qty])
OUTPUT inserted.id INTO @ids (id)
SELECT i.[ProductID], i.[Qty] FROM inserted i`)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if ins.Table.Schema != "Production" || len(ins.Cols) != 2 || ins.Select == nil || ins.Output != "OUTPUT inserted.id INTO @ids (id)" {
		t.Errorf("INSERT … SELECT: %+v", ins)
	}
	v, _ := ParseInsert("INSERT dbo.t VALUES (1, N'a'), (2, DEFAULT)")
	if len(v.Values) != 2 || as[*ast.Literal](t, v.Values[1][1]).Kind != "default" {
		t.Errorf("INSERT VALUES: %+v", v)
	}
	d, _ := ParseInsert("INSERT INTO t DEFAULT VALUES")
	if !d.DefaultValues {
		t.Errorf("DEFAULT VALUES: %+v", d)
	}
	e, errs := ParseInsert("INSERT INTO #t (a) EXEC dbo.p @x = 1")
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if c := as[*ast.CallStmt](t, e.Exec); c.Name != "p" || len(c.Args) != 1 {
		t.Errorf("INSERT … EXEC: %+v", e.Exec)
	}
}

func TestUpdate(t *testing.T) {
	u, errs := ParseUpdate(`UPDATE TOP (5) [Person].[Person]
SET [Person].[Person].[Demographics] = N'x', @n = col = col + 1, [Count] += 2,
    [Demographics].modify(N'insert <a/> into (/b)[1]')
OUTPUT deleted.id
FROM inserted
WHERE [Person].[Person].[BusinessEntityID] = inserted.[BusinessEntityID]`)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if u.Table.Name != "Person" || u.Top == nil || len(u.Sets) != 4 || len(u.From) != 1 || u.Where == nil || u.Output == "" {
		t.Fatalf("UPDATE: %+v", u)
	}
	s0 := u.Sets[0]
	if s0.Col != "Demographics" || len(s0.ColQualifier) != 2 || s0.ColQualifier[0] != "Person" {
		t.Errorf("qualified SET target: %+v", s0)
	}
	if u.Sets[1].Col != "@n" {
		t.Errorf("variable SET: %+v", u.Sets[1])
	}
	inc := as[*ast.BinaryExpr](t, u.Sets[2].Expr)
	if inc.Op != "+" || identText(t, inc.Lhs) != "Count" {
		t.Errorf("compound assignment: %+v", inc)
	}
	if m := as[*ast.RawExpr](t, u.Sets[3].Expr); m.Kind != "XML_METHOD" || u.Sets[3].Col != "Demographics" {
		t.Errorf("xml modify(): %+v %+v", u.Sets[3], m)
	}
	c, errs := ParseUpdate("UPDATE t SET a = 1 WHERE CURRENT OF c")
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if r := as[*ast.RawExpr](t, c.Where); r.Kind != "CURRENT_OF" || r.Text != "c" {
		t.Errorf("WHERE CURRENT OF: %+v", r)
	}
}

func TestDelete(t *testing.T) {
	d, errs := ParseDelete("DELETE TOP (10) FROM o FROM dbo.Orders o INNER JOIN dbo.C c ON c.id = o.cid WHERE c.x = 1 OPTION (MAXDOP 1)")
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if d.Table.Name != "o" || d.Top == nil || len(d.Using) != 1 || d.Where == nil || d.OptionClause != "OPTION (MAXDOP 1)" {
		t.Errorf("DELETE … FROM: %+v", d)
	}
	plain, _ := ParseDelete("DELETE dbo.t WHERE a = 1")
	if plain.Table.Schema != "dbo" || plain.Where == nil {
		t.Errorf("DELETE without FROM: %+v", plain)
	}
}

func TestMerge(t *testing.T) {
	m := mustBodyOne[*ast.MergeStmt](t, `MERGE INTO dbo.Products AS tgt
USING (SELECT @id AS id, @name AS name) AS src ON tgt.id = src.id
WHEN MATCHED AND tgt.name <> src.name THEN UPDATE SET tgt.name = src.name
WHEN NOT MATCHED BY TARGET THEN INSERT (id, name) VALUES (src.id, src.name)
WHEN NOT MATCHED BY SOURCE THEN DELETE
OUTPUT $action, inserted.id;`)
	if m.Target.Name != "Products" || m.TargetAlias != "tgt" || m.On == nil {
		t.Fatalf("MERGE header: %+v", m)
	}
	as[*ast.FromSubquery](t, m.Source)
	if len(m.WhenMatched) != 1 || m.WhenMatched[0].Kind != "UPDATE" || m.WhenMatched[0].Cond == nil || len(m.WhenMatched[0].Sets) != 1 {
		t.Errorf("WHEN MATCHED: %+v", m.WhenMatched)
	}
	if len(m.WhenNotMatched) != 1 || m.WhenNotMatched[0].Kind != "INSERT" || len(m.WhenNotMatched[0].InsertValues) != 2 {
		t.Errorf("WHEN NOT MATCHED: %+v", m.WhenNotMatched)
	}
	if len(m.WhenNotMatchedBySource) != 1 || m.WhenNotMatchedBySource[0].Kind != "DELETE" {
		t.Errorf("WHEN NOT MATCHED BY SOURCE: %+v", m.WhenNotMatchedBySource)
	}
	if m.Output != "OUTPUT $action, inserted.id" {
		t.Errorf("OUTPUT: %q", m.Output)
	}
}

func TestDMLWithCTE(t *testing.T) {
	stmts := mustBody(t, `WITH d AS (SELECT id FROM t) DELETE FROM d;
WITH u AS (SELECT id FROM t) UPDATE u SET id = 0;
WITH i AS (SELECT 1 AS a) INSERT INTO x (a) SELECT a FROM i;`)
	if len(stmts) != 3 {
		t.Fatalf("want 3 statements, got %d", len(stmts))
	}
	if as[*ast.DeleteStmt](t, stmts[0]).With == nil || as[*ast.UpdateStmt](t, stmts[1]).With == nil || as[*ast.InsertStmt](t, stmts[2]).With == nil {
		t.Errorf("CTE preludes lost: %+v", stmts)
	}
}

func TestParseSelectTrailingTokens(t *testing.T) {
	if _, errs := ParseSelect("SELECT 1 FROM t garbage garbage"); len(errs) == 0 {
		t.Errorf("trailing tokens must be reported")
	}
}
