package translate

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// withTSQLHooks removes every hook registered for SQL Server for the
// duration of the test (the dialect's own files register real ones in
// init), so a test sees the identity behaviour or only the hooks it
// installs itself.
func withTSQLHooks(t *testing.T) {
	t.Helper()
	k := dialects.KindSQLServer
	p, br, sr, er, rt, pa := routineBodyParsers[k], bodyRewriters[k], selectRewriters[k], exprRewriters[k], routineBodyTranslators[k], preActionsProviders[k]
	delete(routineBodyParsers, k)
	delete(bodyRewriters, k)
	delete(selectRewriters, k)
	delete(exprRewriters, k)
	delete(routineBodyTranslators, k)
	delete(preActionsProviders, k)
	t.Cleanup(func() {
		restore := func(set func(), del func(), present bool) {
			if present {
				set()
			} else {
				del()
			}
		}
		restore(func() { routineBodyParsers[k] = p }, func() { delete(routineBodyParsers, k) }, p != nil)
		restore(func() { bodyRewriters[k] = br }, func() { delete(bodyRewriters, k) }, br != nil)
		restore(func() { selectRewriters[k] = sr }, func() { delete(selectRewriters, k) }, sr != nil)
		restore(func() { exprRewriters[k] = er }, func() { delete(exprRewriters, k) }, er != nil)
		restore(func() { routineBodyTranslators[k] = rt }, func() { delete(routineBodyTranslators, k) }, rt != nil)
		restore(func() { preActionsProviders[k] = pa }, func() { delete(preActionsProviders, k) }, pa != nil)
	})
}

func TestHooks_IdentityWhenNothingRegistered(t *testing.T) {
	withTSQLHooks(t)
	k := dialects.KindSQLServer
	e := &ast.Ident{Parts: []string{"X"}}
	require.Same(t, e, rewriteHookedExpr(k, e, nil))
	require.Nil(t, rewriteHookedExpr(k, nil, nil))
	sel := &ast.SelectStmt{}
	out, labels := rewriteHookedSelect(k, sel, nil)
	require.Same(t, sel, out)
	require.Empty(t, labels)
	require.Nil(t, hookedRoutineBodyTranslator(k))
	require.Nil(t, hookedPreActions(k, &HookRun{}))
	require.True(t, usesHookedBodies(k))
	require.False(t, usesHookedBodies(dialects.KindMySQL))
}

// SQL Server routine bodies default to the T-SQL parser; MySQL, DB2 and
// Oracle keep their built-in parsers whatever is registered.
func TestHooks_RoutineBodyParserByKind(t *testing.T) {
	withTSQLHooks(t)
	stmts, errs := parseRoutineBodyByKind("BEGIN DECLARE @a int = 1; SET @a = @a + 1; END", dialects.KindSQLServer)
	require.Empty(t, errs)
	require.Len(t, stmts, 1)
	blk, ok := stmts[0].(*ast.Block)
	require.True(t, ok, "%T", stmts[0])
	_, isDeclare := blk.Stmts[0].(*ast.DeclareStmt)
	require.True(t, isDeclare, "%T", blk.Stmts[0])

	called := false
	RegisterRoutineBodyParser(dialects.KindSQLServer, func(body string) ([]ast.PLStmt, string) {
		called = true
		return []ast.PLStmt{&ast.NullStmt{}}, ""
	})
	stmts, _ = parseRoutineBodyByKind("anything", dialects.KindSQLServer)
	require.True(t, called)
	require.Len(t, stmts, 1)

	// MySQL path unchanged: a MySQL body parses with the MySQL parser.
	stmts, errs = parseRoutineBodyByKind("BEGIN SET @x = 1; END", dialects.KindMySQL)
	require.Empty(t, errs)
	require.NotEmpty(t, stmts)
}

// A registered body rewriter replaces the MySQL passes for its kind and
// receives the hook context.
func TestHooks_BodyRewriterRunsInsteadOfMySQLPasses(t *testing.T) {
	withTSQLHooks(t)
	var got *ExprCtx
	RegisterBodyRewriter(dialects.KindSQLServer, func(stmts []ast.PLStmt, ctx *ExprCtx) []ast.PLStmt {
		got = ctx
		return []ast.PLStmt{&ast.ReturnStmt{Expr: &ast.Literal{Kind: "number", Text: "42"}}}
	})
	ctx := &ExprCtx{Object: "function.dbo.f"}
	text, untranslated, _, _ := translateRoutineBodyCore("BEGIN RETURN 1; END", dialects.KindSQLServer, "", "", "dbo", nil, nil, ctx)
	require.Same(t, ctx, got)
	require.Contains(t, text, "RETURN 42;")
	require.Empty(t, untranslated)
}

func TestHookRun_UseAndResolve(t *testing.T) {
	tr := &translator{opt: Options{PreserveSourceSchemas: true, SchemaPrefix: "db_"}, res: &Result{}}
	tr.sourceObjects = map[string]bool{
		sourceObjectKey("Sales", "Orders"):  true,
		sourceObjectKey("dbo", "Customers"): true,
	}
	run := &HookRun{Kind: dialects.KindSQLServer, Schema: tr.schemaFor, t: tr}
	run.Use("tsql.like")
	run.Use("tsql.dateadd")
	require.True(t, run.Used("tsql.like"))
	require.False(t, run.Used("tsql.datediff"))
	require.Equal(t, []string{"tsql.dateadd", "tsql.like"}, run.Features())

	cases := []struct {
		schema, name, def string
		wantS, wantN      string
	}{
		{"Sales", "Orders", "dbo", "db_sales", "orders"},  // qualified
		{"", "Orders", "Sales", "db_sales", "orders"},     // module schema first
		{"", "Customers", "Sales", "db_dbo", "customers"}, // then dbo
		{"", "Unknown", "Sales", "db_sales", "unknown"},   // else the module schema
		{"", "Unknown", "", "db_dbo", "unknown"},          // no module schema: dbo
		{"HumanResources", "Employee", "", "db_humanresources", "employee"},
	}
	for _, c := range cases {
		s, n := run.ResolveRelation(c.schema, c.name, c.def)
		require.Equal(t, c.wantS, s, "%+v", c)
		require.Equal(t, c.wantN, n, "%+v", c)
	}
	require.Equal(t, "orders", run.Ident("Orders"))
}

func TestRoutineCtx_Param(t *testing.T) {
	c := &RoutineCtx{Params: []RoutineParam{{SourceName: "@CustomerID", Name: "customerid"}}}
	require.Equal(t, "customerid", c.Param("@customerid").Name)
	require.Nil(t, c.Param("@other"))
	require.Same(t, c, c.Expr().Routine)
}
