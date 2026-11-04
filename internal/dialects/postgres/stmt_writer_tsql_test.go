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
