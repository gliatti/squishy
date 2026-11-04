package postgres

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// FROM items and clauses the T-SQL parser produces: APPLY joins,
// table-valued function calls, table value constructors and TOP … WITH
// TIES (kept as a flag on the Limit the T-SQL visitors set).

func TestWriteSelect_TSQLFromItems(t *testing.T) {
	col := func(parts ...string) ast.Expr { return &ast.Ident{Parts: parts} }
	sub := &ast.FromSubquery{
		Stmt:  &ast.SelectStmt{Cols: []ast.SelectItem{{Expr: col("o", "order_date")}}, From: []ast.FromItem{&ast.FromTable{Schema: "sales", Name: "orders", Alias: "o"}}},
		Alias: "last_o",
	}
	fn := &ast.FromFunc{Func: &ast.FuncCall{Name: "sales.fn_orders_of", Args: []ast.Expr{col("c", "customer_id")}}, Alias: "f"}
	cases := []struct {
		name string
		from ast.FromItem
		want string
	}{
		{
			name: "cross apply subquery",
			from: &ast.FromJoin{Kind: ast.CrossApply, Left: &ast.FromTable{Schema: "dbo", Name: "customers", Alias: "c"}, Right: sub},
			want: `SELECT * FROM "dbo"."customers" "c" CROSS JOIN LATERAL (SELECT "o"."order_date" FROM "sales"."orders" "o") "last_o"`,
		},
		{
			name: "outer apply function",
			from: &ast.FromJoin{Kind: ast.OuterApply, Left: &ast.FromTable{Schema: "dbo", Name: "customers", Alias: "c"}, Right: fn},
			want: `SELECT * FROM "dbo"."customers" "c" LEFT JOIN LATERAL sales.fn_orders_of("c"."customer_id") "f" ON true`,
		},
		{
			name: "function with column aliases",
			from: &ast.FromFunc{Func: &ast.FuncCall{Name: "unnest", Args: []ast.Expr{col("p", "lines")}}, Alias: "l", Cols: []string{"product_id", "quantity"}},
			want: `SELECT * FROM unnest("p"."lines") "l" ("product_id", "quantity")`,
		},
		{
			name: "values",
			from: &ast.FromValues{
				Rows:  [][]ast.Expr{{ast.BuildIntLit(1), ast.BuildStringLit("a")}, {ast.BuildIntLit(2), ast.BuildStringLit("b")}},
				Alias: "v", Cols: []string{"id", "name"},
			},
			want: `SELECT * FROM (VALUES (1, 'a'), (2, 'b')) "v" ("id", "name")`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &ast.SelectStmt{Cols: []ast.SelectItem{{Star: true}}, From: []ast.FromItem{tc.from}}
			require.Equal(t, tc.want, writeSelect(s))
		})
	}
}

func TestWriteSelect_WithTies(t *testing.T) {
	s := &ast.SelectStmt{
		Cols:        []ast.SelectItem{{Expr: &ast.Ident{Parts: []string{"name"}}}},
		From:        []ast.FromItem{&ast.FromTable{Name: "t"}},
		OrderBy:     []ast.OrderItem{{Expr: &ast.Ident{Parts: []string{"name"}}}},
		Limit:       ast.BuildIntLit(10),
		TopWithTies: true,
	}
	require.Equal(t, `SELECT "name" FROM "t" ORDER BY "name" FETCH FIRST (10) ROWS WITH TIES`, writeSelect(s))
	// Without ORDER BY WITH TIES means nothing: plain LIMIT.
	s.OrderBy = nil
	require.Equal(t, `SELECT "name" FROM "t" LIMIT 10`, writeSelect(s))
}

// MERGE … WHEN NOT MATCHED BY SOURCE (PG 17) and INSERT … DEFAULT VALUES
// are T-SQL statement shapes PG spells the same way: the writer must
// emit them, never drop them.
func TestWriteMerge_NotMatchedBySource(t *testing.T) {
	col := func(parts ...string) ast.Expr { return &ast.Ident{Parts: parts} }
	m := &ast.MergeStmt{
		Target:      ast.TableRef{Schema: "dbo", Name: "t"},
		TargetAlias: "tgt",
		Source:      &ast.FromTable{Schema: "dbo", Name: "s", Alias: "src"},
		On:          &ast.BinaryExpr{Op: "=", Lhs: col("tgt", "id"), Rhs: col("src", "id")},
		WhenMatched: []ast.MergeAction{{Kind: "UPDATE", Sets: []ast.Assign{{Col: "v", Expr: col("src", "v")}}}},
		WhenNotMatched: []ast.MergeAction{{
			Kind: "INSERT", InsertCols: []string{"id", "v"}, InsertValues: []ast.Expr{col("src", "id"), col("src", "v")},
		}},
		WhenNotMatchedBySource: []ast.MergeAction{
			{Kind: "UPDATE", Cond: &ast.BinaryExpr{Op: ">", Lhs: col("tgt", "v"), Rhs: ast.BuildIntLit(0)}, Sets: []ast.Assign{{Col: "v", Expr: ast.BuildIntLit(0)}}},
			{Kind: "DELETE"},
		},
	}
	require.Equal(t, `MERGE INTO "dbo"."t" "tgt" USING "dbo"."s" "src" ON "tgt"."id" = "src"."id"`+
		` WHEN MATCHED THEN UPDATE SET "v" = "src"."v"`+
		` WHEN NOT MATCHED THEN INSERT ("id", "v") VALUES ("src"."id", "src"."v")`+
		` WHEN NOT MATCHED BY SOURCE AND "tgt"."v" > 0 THEN UPDATE SET "v" = 0`+
		` WHEN NOT MATCHED BY SOURCE THEN DELETE`, writeMerge(m))
}

func TestWriteInsert_DefaultValues(t *testing.T) {
	s := &ast.InsertStmt{Table: ast.TableRef{Schema: "dbo", Name: "t"}, DefaultValues: true}
	require.Equal(t, `INSERT INTO "dbo"."t" DEFAULT VALUES`, writeInsert(s))
}

// A T-SQL `WITH cte AS (…) UPDATE|DELETE|INSERT|MERGE` keeps its CTE
// prelude on the statement: the writer must put it back in front.
func TestWriteDML_WithPrelude(t *testing.T) {
	w := &ast.WithClause{CTEs: []ast.CTE{{Name: "c", Body: &ast.SelectStmt{
		Cols: []ast.SelectItem{{Expr: &ast.Ident{Parts: []string{"id"}}}},
		From: []ast.FromItem{&ast.FromTable{Name: "s"}},
	}}}}
	where := &ast.InExpr{Expr: &ast.Ident{Parts: []string{"id"}}, Subquery: &ast.SelectStmt{
		Cols: []ast.SelectItem{{Expr: &ast.Ident{Parts: []string{"id"}}}},
		From: []ast.FromItem{&ast.FromTable{Name: "c"}},
	}}
	require.Equal(t, `WITH "c" AS (SELECT "id" FROM "s") DELETE FROM "t" WHERE "id" IN (SELECT "id" FROM "c")`,
		writeDelete(&ast.DeleteStmt{With: w, Table: ast.TableRef{Name: "t"}, Where: where}))
	require.Equal(t, `WITH "c" AS (SELECT "id" FROM "s") INSERT INTO "t" SELECT "id" FROM "c"`,
		writeInsert(&ast.InsertStmt{With: w, Table: ast.TableRef{Name: "t"}, Select: where.Subquery}))
}
