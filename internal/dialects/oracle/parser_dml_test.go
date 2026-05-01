package oracle

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

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

func parseMergeFor(t *testing.T, src string) *ast.MergeStmt {
	t.Helper()
	stmt, errs := ParseMerge(src)
	require.Empty(t, errs, "parse errors: %v", errs)
	require.NotNil(t, stmt)
	return stmt
}

// ---------------------------------------------------------------------------
// SELECT
// ---------------------------------------------------------------------------

func TestOracleParseSelect_Basic(t *testing.T) {
	s := parseSelectFor(t, "SELECT id, name FROM users")
	require.Len(t, s.Cols, 2)
	require.Len(t, s.From, 1)
	ft, ok := s.From[0].(*ast.FromTable)
	require.True(t, ok)
	require.Equal(t, "USERS", ft.Name) // Oracle case-folds unquoted idents
}

func TestOracleParseSelect_Star(t *testing.T) {
	s := parseSelectFor(t, "SELECT * FROM users")
	require.Len(t, s.Cols, 1)
	require.True(t, s.Cols[0].Star)
}

func TestOracleParseSelect_Distinct(t *testing.T) {
	s := parseSelectFor(t, "SELECT DISTINCT id FROM t")
	require.True(t, s.Distinct)
}

func TestOracleParseSelect_Concat(t *testing.T) {
	s := parseSelectFor(t, "SELECT first_name || ' ' || last_name AS full_name FROM emp")
	require.Len(t, s.Cols, 1)
	be, ok := s.Cols[0].Expr.(*ast.BinaryExpr)
	require.True(t, ok, "got %T", s.Cols[0].Expr)
	require.Equal(t, "||", be.Op)
}

func TestOracleParseSelect_Where(t *testing.T) {
	s := parseSelectFor(t, "SELECT id FROM t WHERE id > 10 AND name LIKE 'A%'")
	require.NotNil(t, s.Where)
}

func TestOracleParseSelect_GroupHavingOrder(t *testing.T) {
	s := parseSelectFor(t, "SELECT dept, COUNT(*) FROM emp GROUP BY dept HAVING COUNT(*) > 5 ORDER BY dept DESC")
	require.Len(t, s.GroupBy, 1)
	require.NotNil(t, s.Having)
	require.Len(t, s.OrderBy, 1)
	require.True(t, s.OrderBy[0].Desc)
}

func TestOracleParseSelect_FetchFirst(t *testing.T) {
	s := parseSelectFor(t, "SELECT * FROM t ORDER BY id FETCH FIRST 10 ROWS ONLY")
	require.NotNil(t, s.Limit)
}

func TestOracleParseSelect_OffsetFetch(t *testing.T) {
	s := parseSelectFor(t, "SELECT * FROM t ORDER BY id OFFSET 20 ROWS FETCH NEXT 10 ROWS ONLY")
	require.NotNil(t, s.Offset)
	require.NotNil(t, s.Limit)
}

func TestOracleParseSelect_UnionMinus(t *testing.T) {
	s := parseSelectFor(t, "SELECT 1 FROM dual UNION SELECT 2 FROM dual MINUS SELECT 3 FROM dual")
	require.Len(t, s.SetOps, 2)
	require.Equal(t, "UNION", s.SetOps[0].Op)
	require.Equal(t, "MINUS", s.SetOps[1].Op)
}

func TestOracleParseSelect_With(t *testing.T) {
	s := parseSelectFor(t, "WITH x AS (SELECT 1 FROM dual) SELECT * FROM x")
	require.NotNil(t, s.With)
	require.Len(t, s.With.CTEs, 1)
}

func TestOracleParseSelect_InnerJoin(t *testing.T) {
	s := parseSelectFor(t, "SELECT * FROM a JOIN b ON a.id = b.aid")
	j, ok := s.From[0].(*ast.FromJoin)
	require.True(t, ok)
	require.Equal(t, ast.InnerJoin, j.Kind)
}

func TestOracleParseSelect_LeftOuterJoin(t *testing.T) {
	s := parseSelectFor(t, "SELECT * FROM a LEFT OUTER JOIN b ON a.id = b.aid")
	j, ok := s.From[0].(*ast.FromJoin)
	require.True(t, ok)
	require.Equal(t, ast.LeftJoin, j.Kind)
}

func TestOracleParseSelect_NaturalJoin(t *testing.T) {
	s := parseSelectFor(t, "SELECT * FROM a NATURAL LEFT JOIN b")
	j, ok := s.From[0].(*ast.FromJoin)
	require.True(t, ok)
	require.True(t, j.Natural)
}

func TestOracleParseSelect_FromSubquery(t *testing.T) {
	s := parseSelectFor(t, "SELECT * FROM (SELECT id FROM t) sub")
	sub, ok := s.From[0].(*ast.FromSubquery)
	require.True(t, ok)
	require.NotNil(t, sub.Stmt)
	require.Equal(t, "SUB", sub.Alias)
}

func TestOracleParseSelect_ExistsExpr(t *testing.T) {
	s := parseSelectFor(t, "SELECT 1 FROM dual WHERE EXISTS (SELECT 1 FROM t)")
	ex, ok := s.Where.(*ast.ExistsExpr)
	require.True(t, ok, "got %T", s.Where)
	require.NotNil(t, ex.Subquery)
}

func TestOracleParseSelect_InSubquery(t *testing.T) {
	s := parseSelectFor(t, "SELECT 1 FROM dual WHERE id IN (SELECT id FROM t)")
	in, ok := s.Where.(*ast.InExpr)
	require.True(t, ok)
	require.NotNil(t, in.Subquery)
}

func TestOracleParseSelect_BetweenIs(t *testing.T) {
	s := parseSelectFor(t, "SELECT 1 FROM dual WHERE x BETWEEN 1 AND 10 AND y IS NOT NULL")
	require.NotNil(t, s.Where)
}

func TestOracleParseSelect_CastExpr(t *testing.T) {
	s := parseSelectFor(t, "SELECT CAST(x AS NUMBER(10,2)) FROM t")
	c, ok := s.Cols[0].Expr.(*ast.CastExpr)
	require.True(t, ok, "got %T", s.Cols[0].Expr)
	require.NotNil(t, c.Type)
}

func TestOracleParseSelect_CaseExpr(t *testing.T) {
	s := parseSelectFor(t, "SELECT CASE WHEN x > 0 THEN 1 ELSE 0 END FROM t")
	c, ok := s.Cols[0].Expr.(*ast.CaseExpr)
	require.True(t, ok, "got %T", s.Cols[0].Expr)
	require.Len(t, c.Whens, 1)
	require.NotNil(t, c.Else)
}

func TestOracleParseSelect_OuterJoinPlus(t *testing.T) {
	// `(+)` marker on a column reference is wrapped in OuterJoinHint.
	s := parseSelectFor(t, "SELECT * FROM a, b WHERE a.id = b.aid(+)")
	require.NotNil(t, s.Where)
	be, ok := s.Where.(*ast.BinaryExpr)
	require.True(t, ok)
	_, ok = be.Rhs.(*ast.OuterJoinHint)
	require.True(t, ok, "rhs got %T", be.Rhs)
}

func TestOracleParseSelect_WindowedAgg(t *testing.T) {
	s := parseSelectFor(t,
		"SELECT LISTAGG(name, ',') WITHIN GROUP (ORDER BY name) FROM t")
	w, ok := s.Cols[0].Expr.(*ast.WindowedAgg)
	require.True(t, ok, "got %T", s.Cols[0].Expr)
	require.Equal(t, "LISTAGG", w.Func.Name)
	require.Len(t, w.Within, 1)
}

func TestOracleParseSelect_OverWindow(t *testing.T) {
	s := parseSelectFor(t,
		"SELECT ROW_NUMBER() OVER (PARTITION BY dept ORDER BY salary DESC) FROM emp")
	w, ok := s.Cols[0].Expr.(*ast.WindowedAgg)
	require.True(t, ok, "got %T", s.Cols[0].Expr)
	require.NotNil(t, w.Over)
	require.Len(t, w.Over.PartitionBy, 1)
	require.Len(t, w.Over.OrderBy, 1)
}

func TestOracleParseSelect_ForUpdate(t *testing.T) {
	s := parseSelectFor(t, "SELECT * FROM t WHERE id = 1 FOR UPDATE")
	require.True(t, strings.Contains(strings.ToUpper(s.ForUpdate), "FOR UPDATE"))
}

// ---------------------------------------------------------------------------
// INSERT
// ---------------------------------------------------------------------------

func TestOracleParseInsert_Values(t *testing.T) {
	ins := parseInsertFor(t, "INSERT INTO t (a, b) VALUES (1, 'x')")
	require.Equal(t, []string{"A", "B"}, ins.Cols)
	require.Len(t, ins.Values, 1)
	require.Len(t, ins.Values[0], 2)
}

func TestOracleParseInsert_Select(t *testing.T) {
	ins := parseInsertFor(t, "INSERT INTO t (a, b) SELECT x, y FROM src")
	require.NotNil(t, ins.Select)
}

func TestOracleParseInsert_Returning(t *testing.T) {
	ins := parseInsertFor(t, "INSERT INTO t (a) VALUES (1) RETURNING a INTO :v")
	require.Len(t, ins.Returning, 1)
}

// ---------------------------------------------------------------------------
// UPDATE
// ---------------------------------------------------------------------------

func TestOracleParseUpdate_Single(t *testing.T) {
	upd := parseUpdateFor(t, "UPDATE t SET a = 1, b = 2 WHERE id = 5")
	require.Equal(t, "T", upd.Table.Name)
	require.Len(t, upd.Sets, 2)
	require.NotNil(t, upd.Where)
}

// ---------------------------------------------------------------------------
// DELETE
// ---------------------------------------------------------------------------

func TestOracleParseDelete_Single(t *testing.T) {
	del := parseDeleteFor(t, "DELETE FROM t WHERE id = 5")
	require.Equal(t, "T", del.Table.Name)
	require.NotNil(t, del.Where)
}

func TestOracleParseDelete_NoFrom(t *testing.T) {
	// Oracle accepts `DELETE t WHERE …` (FROM is optional).
	del := parseDeleteFor(t, "DELETE t WHERE id = 5")
	require.Equal(t, "T", del.Table.Name)
}

// ---------------------------------------------------------------------------
// MERGE
// ---------------------------------------------------------------------------

func TestOracleParseMerge_Basic(t *testing.T) {
	src := `MERGE INTO target t USING source s ON (t.id = s.id)
		WHEN MATCHED THEN UPDATE SET t.val = s.val
		WHEN NOT MATCHED THEN INSERT (id, val) VALUES (s.id, s.val)`
	m := parseMergeFor(t, src)
	require.Equal(t, "TARGET", m.Target.Name)
	require.Equal(t, "T", m.TargetAlias)
	require.NotNil(t, m.Source)
	require.NotNil(t, m.On)
	require.Len(t, m.WhenMatched, 1)
	require.Equal(t, "UPDATE", m.WhenMatched[0].Kind)
	require.Len(t, m.WhenNotMatched, 1)
	require.Equal(t, "INSERT", m.WhenNotMatched[0].Kind)
}

func TestOracleParseMerge_LogErrorsTolerated(t *testing.T) {
	src := `MERGE INTO t USING (SELECT 1 id FROM dual) s ON (t.id = s.id)
		WHEN MATCHED THEN UPDATE SET t.id = s.id
		LOG ERRORS REJECT LIMIT UNLIMITED`
	m := parseMergeFor(t, src)
	require.Len(t, m.WhenMatched, 1)
}

func TestOracleParseMerge_DeleteWhereTolerated(t *testing.T) {
	src := `MERGE INTO t USING s ON (t.id = s.id)
		WHEN MATCHED THEN UPDATE SET t.val = s.val DELETE WHERE t.deleted = 'Y'`
	m := parseMergeFor(t, src)
	require.Len(t, m.WhenMatched, 1)
}
