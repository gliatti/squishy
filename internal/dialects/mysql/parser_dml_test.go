package mysql

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// parseSelectFor wraps ParseSelect and asserts no parse errors.
func parseSelectFor(t *testing.T, src string) *ast.SelectStmt {
	t.Helper()
	stmt, errs := ParseSelect(src)
	require.Empty(t, errs, "parse errors: %v", errs)
	require.NotNil(t, stmt)
	return stmt
}

func parseInsertFor(t *testing.T, src string) *ast.InsertStmt {
	t.Helper()
	stmt, errs := ParseInsert(src)
	require.Empty(t, errs, "parse errors: %v", errs)
	require.NotNil(t, stmt)
	return stmt
}

func parseUpdateFor(t *testing.T, src string) *ast.UpdateStmt {
	t.Helper()
	stmt, errs := ParseUpdate(src)
	require.Empty(t, errs, "parse errors: %v", errs)
	require.NotNil(t, stmt)
	return stmt
}

func parseDeleteFor(t *testing.T, src string) *ast.DeleteStmt {
	t.Helper()
	stmt, errs := ParseDelete(src)
	require.Empty(t, errs, "parse errors: %v", errs)
	require.NotNil(t, stmt)
	return stmt
}

// ---------------------------------------------------------------------------
// SELECT
// ---------------------------------------------------------------------------

func TestParseSelect_Basic(t *testing.T) {
	s := parseSelectFor(t, "SELECT id, name FROM users")
	require.Len(t, s.Cols, 2)
	require.Len(t, s.From, 1)
	ft, ok := s.From[0].(*ast.FromTable)
	require.True(t, ok)
	require.Equal(t, "users", ft.Name)
}

func TestParseSelect_Star(t *testing.T) {
	s := parseSelectFor(t, "SELECT * FROM users")
	require.Len(t, s.Cols, 1)
	require.True(t, s.Cols[0].Star)
	require.Equal(t, "", s.Cols[0].Qualifier)
}

func TestParseSelect_QualifiedStar(t *testing.T) {
	s := parseSelectFor(t, "SELECT u.* FROM users u")
	require.Len(t, s.Cols, 1)
	require.True(t, s.Cols[0].Star)
	require.Equal(t, "u", s.Cols[0].Qualifier)
}

func TestParseSelect_Distinct(t *testing.T) {
	s := parseSelectFor(t, "SELECT DISTINCT id FROM t")
	require.True(t, s.Distinct)
}

func TestParseSelect_Where(t *testing.T) {
	s := parseSelectFor(t, "SELECT id FROM t WHERE id > 10 AND name LIKE 'a%'")
	require.NotNil(t, s.Where)
	be, ok := s.Where.(*ast.BinaryExpr)
	require.True(t, ok)
	require.Equal(t, "AND", be.Op)
}

func TestParseSelect_GroupHavingOrderLimit(t *testing.T) {
	s := parseSelectFor(t,
		"SELECT dept, COUNT(*) FROM emp GROUP BY dept HAVING COUNT(*) > 5 ORDER BY dept DESC LIMIT 10")
	require.Len(t, s.GroupBy, 1)
	require.NotNil(t, s.Having)
	require.Len(t, s.OrderBy, 1)
	require.True(t, s.OrderBy[0].Desc)
	require.NotNil(t, s.Limit)
}

func TestParseSelect_LimitOffset(t *testing.T) {
	s := parseSelectFor(t, "SELECT * FROM t LIMIT 10 OFFSET 20")
	require.NotNil(t, s.Limit)
	require.NotNil(t, s.Offset)
}

func TestParseSelect_LimitCommaForm(t *testing.T) {
	s := parseSelectFor(t, "SELECT * FROM t LIMIT 5, 10")
	// MySQL `LIMIT offset, count` → offset=5, limit=10
	require.NotNil(t, s.Limit)
	require.NotNil(t, s.Offset)
}

func TestParseSelect_Union(t *testing.T) {
	s := parseSelectFor(t, "SELECT 1 UNION SELECT 2 UNION ALL SELECT 3")
	require.Len(t, s.SetOps, 2)
	require.Equal(t, "UNION", s.SetOps[0].Op)
	require.Equal(t, "UNION ALL", s.SetOps[1].Op)
}

func TestParseSelect_With(t *testing.T) {
	s := parseSelectFor(t, "WITH x AS (SELECT 1) SELECT * FROM x")
	require.NotNil(t, s.With)
	require.Len(t, s.With.CTEs, 1)
	require.Equal(t, "x", s.With.CTEs[0].Name)
}

func TestParseSelect_WithRecursive(t *testing.T) {
	s := parseSelectFor(t, "WITH RECURSIVE n AS (SELECT 1 UNION SELECT n+1 FROM n) SELECT * FROM n")
	require.NotNil(t, s.With)
	require.True(t, s.With.Recursive)
}

func TestParseSelect_InnerJoin(t *testing.T) {
	s := parseSelectFor(t, "SELECT * FROM a JOIN b ON a.id = b.aid")
	require.Len(t, s.From, 1)
	j, ok := s.From[0].(*ast.FromJoin)
	require.True(t, ok)
	require.Equal(t, ast.InnerJoin, j.Kind)
	require.NotNil(t, j.On)
}

func TestParseSelect_LeftOuterJoin(t *testing.T) {
	s := parseSelectFor(t, "SELECT * FROM a LEFT OUTER JOIN b ON a.id = b.aid")
	j, ok := s.From[0].(*ast.FromJoin)
	require.True(t, ok)
	require.Equal(t, ast.LeftJoin, j.Kind)
}

func TestParseSelect_NaturalJoin(t *testing.T) {
	s := parseSelectFor(t, "SELECT * FROM a NATURAL LEFT JOIN b")
	j, ok := s.From[0].(*ast.FromJoin)
	require.True(t, ok)
	require.True(t, j.Natural)
	require.Equal(t, ast.LeftJoin, j.Kind)
}

func TestParseSelect_JoinUsing(t *testing.T) {
	s := parseSelectFor(t, "SELECT * FROM a JOIN b USING (id)")
	j, ok := s.From[0].(*ast.FromJoin)
	require.True(t, ok)
	require.Equal(t, []string{"id"}, j.Using)
}

func TestParseSelect_FromSubquery(t *testing.T) {
	s := parseSelectFor(t, "SELECT * FROM (SELECT id FROM t) AS sub")
	sub, ok := s.From[0].(*ast.FromSubquery)
	require.True(t, ok)
	require.NotNil(t, sub.Stmt)
	require.Equal(t, "sub", sub.Alias)
}

func TestParseSelect_ExistsExpr(t *testing.T) {
	s := parseSelectFor(t, "SELECT 1 WHERE EXISTS (SELECT 1 FROM t)")
	ex, ok := s.Where.(*ast.ExistsExpr)
	require.True(t, ok, "got %T", s.Where)
	require.NotNil(t, ex.Subquery)
}

func TestParseSelect_ScalarSubquery(t *testing.T) {
	s := parseSelectFor(t, "SELECT (SELECT MAX(id) FROM t) AS m")
	require.Len(t, s.Cols, 1)
	_, ok := s.Cols[0].Expr.(*ast.SubqueryExpr)
	require.True(t, ok, "got %T", s.Cols[0].Expr)
	require.Equal(t, "m", s.Cols[0].Alias)
}

func TestParseSelect_InSubquery(t *testing.T) {
	s := parseSelectFor(t, "SELECT 1 WHERE id IN (SELECT id FROM t)")
	in, ok := s.Where.(*ast.InExpr)
	require.True(t, ok)
	require.NotNil(t, in.Subquery)
}

func TestParseSelect_ForUpdate(t *testing.T) {
	s := parseSelectFor(t, "SELECT * FROM t WHERE id = 1 FOR UPDATE")
	require.Equal(t, "FOR UPDATE", s.ForUpdate)
}

func TestParseSelect_LockInShareMode(t *testing.T) {
	s := parseSelectFor(t, "SELECT * FROM t LOCK IN SHARE MODE")
	require.True(t, strings.Contains(s.ForUpdate, "LOCK"))
}

// ---------------------------------------------------------------------------
// INSERT (via routine body)
// ---------------------------------------------------------------------------

func TestParseInsert_Values(t *testing.T) {
	ins := parseInsertFor(t, "INSERT INTO t (a, b) VALUES (1, 'x'), (2, 'y')")
	require.Equal(t, []string{"a", "b"}, ins.Cols)
	require.Len(t, ins.Values, 2)
	require.Len(t, ins.Values[0], 2)
}

func TestParseInsert_Select(t *testing.T) {
	ins := parseInsertFor(t, "INSERT INTO t (a, b) SELECT x, y FROM src")
	require.NotNil(t, ins.Select)
}

func TestParseInsert_OnDuplicateKeyUpdate(t *testing.T) {
	ins := parseInsertFor(t, "INSERT INTO t (a, b) VALUES (1, 2) ON DUPLICATE KEY UPDATE b = 99")
	require.NotNil(t, ins.OnConflict)
	require.Len(t, ins.OnConflict.Sets, 1)
	require.Equal(t, "b", ins.OnConflict.Sets[0].Col)
}

func TestParseInsert_SetForm(t *testing.T) {
	ins := parseInsertFor(t, "INSERT INTO t SET a = 1, b = 'x'")
	require.Equal(t, []string{"a", "b"}, ins.Cols)
	require.Len(t, ins.Values, 1)
	require.Len(t, ins.Values[0], 2)
}

// ---------------------------------------------------------------------------
// UPDATE
// ---------------------------------------------------------------------------

func TestParseUpdate_Single(t *testing.T) {
	upd := parseUpdateFor(t, "UPDATE t SET a = 1, b = 2 WHERE id = 5")
	require.Equal(t, "t", upd.Table.Name)
	require.Len(t, upd.Sets, 2)
	require.NotNil(t, upd.Where)
}

func TestParseUpdate_Multi(t *testing.T) {
	upd := parseUpdateFor(t, "UPDATE t1 JOIN t2 ON t1.id = t2.t1_id SET t1.a = t2.b WHERE t2.flag = 1")
	require.NotEmpty(t, upd.From)
	require.Len(t, upd.Sets, 1)
}

// ---------------------------------------------------------------------------
// DELETE
// ---------------------------------------------------------------------------

func TestParseDelete_Single(t *testing.T) {
	del := parseDeleteFor(t, "DELETE FROM t WHERE id = 5")
	require.Equal(t, "t", del.Table.Name)
	require.NotNil(t, del.Where)
}

func TestParseDelete_MultiTargetUsing(t *testing.T) {
	del := parseDeleteFor(t, "DELETE FROM t1, t2 USING t1 INNER JOIN t2 ON t1.id = t2.t1_id WHERE t2.flag = 1")
	require.NotEmpty(t, del.Using)
	require.NotNil(t, del.Where)
}

func TestParseDelete_MultiTargetCommaFrom(t *testing.T) {
	del := parseDeleteFor(t, "DELETE t1, t2 FROM t1 INNER JOIN t2 ON t1.id = t2.t1_id")
	require.Equal(t, "", del.Table.Name) // multi-target shape
	require.NotEmpty(t, del.Using)
}
