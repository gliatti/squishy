package mysql

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// parseBodyFor parses a routine body and returns the statements of its
// outermost BEGIN … END block. Parse errors are returned for the caller to
// assert on (some tests expect them).
func parseBodyFor(t *testing.T, src string) ([]ast.PLDecl, []ast.PLStmt, ErrorList) {
	t.Helper()
	stmts, errs := ParseRoutineBody(src)
	require.Len(t, stmts, 1, "expected a single BEGIN … END block")
	blk, ok := stmts[0].(*ast.Block)
	require.True(t, ok, "got %T", stmts[0])
	return blk.Decls, blk.Stmts, errs
}

func TestParseRoutineBody_DeclareCursorTyped(t *testing.T) {
	decls, stmts, errs := parseBodyFor(t, "BEGIN DECLARE cur CURSOR FOR SELECT id FROM t WHERE x > 1; OPEN cur; END")
	require.Empty(t, errs, "parse errors: %v", errs)
	require.Len(t, decls, 1)
	dc, ok := decls[0].(*ast.DeclareCursor)
	require.True(t, ok, "got %T", decls[0])
	require.Equal(t, "cur", dc.Name)
	require.Equal(t, "SELECT id FROM t WHERE x > 1", dc.SelectBody)
	require.NotNil(t, dc.Stmt)
	require.Len(t, dc.Stmt.Cols, 1)
	require.NotNil(t, dc.Stmt.Where)
	require.Len(t, stmts, 1)
	_, ok = stmts[0].(*ast.OpenStmt)
	require.True(t, ok, "statement after the cursor got %T", stmts[0])
}

func TestParseRoutineBody_DeclareCursorUnparseableKeepsText(t *testing.T) {
	decls, stmts, errs := parseBodyFor(t,
		"BEGIN DECLARE cur CURSOR FOR SELECT a FROM t WHERE MATCH (a) AGAINST ('x'); SET n = 1; END")
	require.NotEmpty(t, errs, "the cursor query diagnostic must be kept")
	dc, ok := decls[0].(*ast.DeclareCursor)
	require.True(t, ok)
	require.Nil(t, dc.Stmt)
	require.Equal(t, "SELECT a FROM t WHERE MATCH (a) AGAINST ('x')", dc.SelectBody)
	// The statement boundary is unaffected by the failed typed parse.
	require.Len(t, stmts, 1)
	as, ok := stmts[0].(*ast.AssignStmt)
	require.True(t, ok, "got %T", stmts[0])
	require.Equal(t, "n", as.Target)
}

func TestParseRoutineBody_SelectInto(t *testing.T) {
	cases := []struct {
		name     string
		src      string
		vars     []string
		rawQuery string
	}{
		{"before FROM", "SELECT COUNT(*) INTO n FROM t WHERE id = 1", []string{"n"}, "SELECT COUNT(*) FROM t WHERE id = 1"},
		{"session and local vars", "SELECT a, b INTO @x, y FROM t", []string{"@x", "y"}, "SELECT a, b FROM t"},
		{"trailing INTO", "SELECT a FROM t WHERE id = 1 INTO v", []string{"v"}, "SELECT a FROM t WHERE id = 1"},
		{"after LIMIT", "SELECT a FROM t ORDER BY a LIMIT 1 INTO v", []string{"v"}, "SELECT a FROM t ORDER BY a LIMIT 1"},
		{"no FROM", "SELECT 1 + 1 INTO v", []string{"v"}, "SELECT 1 + 1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, stmts, errs := parseBodyFor(t, "BEGIN "+c.src+"; END")
			require.Empty(t, errs, "parse errors: %v", errs)
			require.Len(t, stmts, 1)
			si, ok := stmts[0].(*ast.SelectInto)
			require.True(t, ok, "got %T", stmts[0])
			require.Equal(t, c.vars, si.Vars)
			require.NotNil(t, si.Stmt)
			require.Equal(t, c.rawQuery, si.RawQuery)
		})
	}
}

func TestParseRoutineBody_SelectIntoStmtShape(t *testing.T) {
	_, stmts, errs := parseBodyFor(t, "BEGIN SELECT COUNT(*) INTO n FROM t WHERE id = 1; END")
	require.Empty(t, errs)
	si := stmts[0].(*ast.SelectInto)
	require.Len(t, si.Stmt.Cols, 1)
	fc, ok := si.Stmt.Cols[0].Expr.(*ast.FuncCall)
	require.True(t, ok, "got %T", si.Stmt.Cols[0].Expr)
	require.Equal(t, "COUNT", fc.Name)
	require.Len(t, si.Stmt.From, 1)
	require.NotNil(t, si.Stmt.Where)
}

func TestParseRoutineBody_SelectIntoUnparseableQuery(t *testing.T) {
	_, stmts, errs := parseBodyFor(t,
		"BEGIN SELECT a INTO v FROM t WHERE MATCH (a) AGAINST ('x'); SET n = 1; END")
	require.NotEmpty(t, errs)
	require.Len(t, stmts, 2)
	si, ok := stmts[0].(*ast.SelectInto)
	require.True(t, ok, "got %T", stmts[0])
	require.Equal(t, []string{"v"}, si.Vars)
	require.Nil(t, si.Stmt)
	require.Equal(t, "SELECT a FROM t WHERE MATCH (a) AGAINST ('x')", si.RawQuery)
	_, ok = stmts[1].(*ast.AssignStmt)
	require.True(t, ok, "got %T", stmts[1])
}

func TestParseRoutineBody_SelectIntoOutfileIsNotSelectInto(t *testing.T) {
	_, stmts, errs := parseBodyFor(t, "BEGIN SELECT a INTO OUTFILE '/x' FROM t; SET n = 1; END")
	require.NotEmpty(t, errs, "INTO OUTFILE must be reported")
	require.Len(t, stmts, 2)
	raw, ok := stmts[0].(*ast.RawSQL)
	require.True(t, ok, "got %T", stmts[0])
	require.False(t, raw.Verbatim)
	require.Equal(t, "SELECT a INTO OUTFILE '/x' FROM t", raw.Text)
}

func TestParseRoutineBody_PlainSelectStaysTyped(t *testing.T) {
	_, stmts, errs := parseBodyFor(t, "BEGIN SELECT a FROM t WHERE b <=> c; END")
	require.Empty(t, errs)
	require.Len(t, stmts, 1)
	sel, ok := stmts[0].(*ast.SelectStmt)
	require.True(t, ok, "got %T", stmts[0])
	be, ok := sel.Where.(*ast.BinaryExpr)
	require.True(t, ok)
	require.Equal(t, "<=>", be.Op)
}

func TestParseRoutineBody_Resignal(t *testing.T) {
	_, stmts, errs := parseBodyFor(t,
		"BEGIN RESIGNAL; RESIGNAL SQLSTATE '45000'; RESIGNAL SET MESSAGE_TEXT = 'boom'; SIGNAL SQLSTATE VALUE '45001' SET MESSAGE_TEXT = 'x'; END")
	require.Empty(t, errs, "parse errors: %v", errs)
	require.Len(t, stmts, 4)
	s0, ok := stmts[0].(*ast.SignalStmt)
	require.True(t, ok, "got %T", stmts[0])
	require.True(t, s0.Resignal)
	require.Empty(t, s0.SQLState)
	s1 := stmts[1].(*ast.SignalStmt)
	require.True(t, s1.Resignal)
	require.Equal(t, "45000", s1.SQLState)
	s2 := stmts[2].(*ast.SignalStmt)
	require.True(t, s2.Resignal)
	require.Equal(t, "boom", s2.Message)
	s3 := stmts[3].(*ast.SignalStmt)
	require.False(t, s3.Resignal)
	require.Equal(t, "45001", s3.SQLState)
	require.Equal(t, "x", s3.Message)
}

func TestParseRoutineBody_Truncate(t *testing.T) {
	_, stmts, errs := parseBodyFor(t, "BEGIN TRUNCATE TABLE db.t; TRUNCATE u; END")
	require.Empty(t, errs)
	require.Len(t, stmts, 2)
	tt, ok := stmts[0].(*ast.TruncateTable)
	require.True(t, ok, "got %T", stmts[0])
	require.Equal(t, "db", tt.Table.Schema)
	require.Equal(t, "t", tt.Table.Name)
	tt, ok = stmts[1].(*ast.TruncateTable)
	require.True(t, ok, "got %T", stmts[1])
	require.Equal(t, "u", tt.Table.Name)
}

func TestParseRoutineBody_RawSQLVerbatimClassification(t *testing.T) {
	cases := []struct {
		src      string
		verbatim bool
	}{
		{"COMMIT", true},
		{"ROLLBACK", true},
		{"COMMIT WORK", false},
		{"COMMIT AND CHAIN", false},
		{"ROLLBACK TO SAVEPOINT sp1", false},
		{"ROLLBACK TO sp1", false},
		{"SAVEPOINT sp1", false},
		{"RELEASE SAVEPOINT sp1", false},
		{"START TRANSACTION", false},
		{"GET DIAGNOSTICS n = ROW_COUNT", true},
		{"GET CURRENT DIAGNOSTICS n = ROW_COUNT, m = ROW_COUNT", true},
		{"GET DIAGNOSTICS CONDITION 1 v = MESSAGE_TEXT", false},
		{"GET DIAGNOSTICS CONDITION 1 @p = MESSAGE_TEXT", false},
		{"GET DIAGNOSTICS @n = ROW_COUNT", false},
		{"GET DIAGNOSTICS n = NUMBER", false},
		{"GET STACKED DIAGNOSTICS n = ROW_COUNT", false},
		{"PREPARE s FROM 'x'", false},
		{"EXECUTE s", false},
		{"DEALLOCATE PREPARE s", false},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			_, stmts, _ := parseBodyFor(t, "BEGIN "+c.src+"; END")
			require.Len(t, stmts, 1)
			raw, ok := stmts[0].(*ast.RawSQL)
			require.True(t, ok, "got %T", stmts[0])
			require.Equal(t, c.verbatim, raw.Verbatim)
			require.Equal(t, c.src, raw.Text)
		})
	}
}

// ---------------------------------------------------------------------------
// CREATE VIEW / CREATE EVENT typed slots
// ---------------------------------------------------------------------------

func TestParseCreateView_TypedBody(t *testing.T) {
	stmts, errs := Parse("CREATE VIEW v AS SELECT a, GROUP_CONCAT(b SEPARATOR ',') AS bs FROM t JOIN u GROUP BY a WITH CHECK OPTION;")
	require.Empty(t, errs, "parse errors: %v", errs)
	require.Len(t, stmts, 1)
	cv, ok := stmts[0].(*ast.CreateView)
	require.True(t, ok, "got %T", stmts[0])
	require.Equal(t, "SELECT a, GROUP_CONCAT(b SEPARATOR ',') AS bs FROM t JOIN u GROUP BY a", cv.SelectBody)
	require.NotNil(t, cv.Select)
	require.Empty(t, cv.SelectParseError)
	require.Len(t, cv.Select.Cols, 2)
	require.Equal(t, "CHECK OPTION", cv.CheckOption)
}

func TestParseCreateView_UnparseableBodyKeepsTextAndNoError(t *testing.T) {
	src := "CREATE VIEW v AS SELECT a FROM t WHERE MATCH (a) AGAINST ('x');\n" +
		"CREATE TABLE z (id INT);"
	stmts, errs := Parse(src)
	require.Empty(t, errs, "an unparseable view body must not surface as a Parse() error")
	require.Len(t, stmts, 2, "the following statement must still be parsed")
	cv, ok := stmts[0].(*ast.CreateView)
	require.True(t, ok, "got %T", stmts[0])
	require.Nil(t, cv.Select)
	require.NotEmpty(t, cv.SelectParseError)
	require.Equal(t, "SELECT a FROM t WHERE MATCH (a) AGAINST ('x')", cv.SelectBody)
	_, ok = stmts[1].(*ast.CreateTable)
	require.True(t, ok, "got %T", stmts[1])
}

func TestParseCreateView_TruncatedBodyDoesNotSwallowNextStatement(t *testing.T) {
	stmts, errs := Parse("CREATE VIEW v AS SELECT a FROM t WHERE;\nCREATE TABLE z (id INT);")
	require.Empty(t, errs)
	require.Len(t, stmts, 2)
	cv := stmts[0].(*ast.CreateView)
	require.Nil(t, cv.Select)
	require.NotEmpty(t, cv.SelectParseError)
	require.Equal(t, "SELECT a FROM t WHERE", cv.SelectBody)
}

func TestParseCreateEvent_AtExpr(t *testing.T) {
	stmts, errs := Parse("CREATE EVENT e ON SCHEDULE AT CURRENT_TIMESTAMP + INTERVAL 1 HOUR DO DELETE FROM t;")
	require.Empty(t, errs, "parse errors: %v", errs)
	require.Len(t, stmts, 1)
	ev, ok := stmts[0].(*ast.CreateEvent)
	require.True(t, ok, "got %T", stmts[0])
	require.Equal(t, "AT", ev.ScheduleKind)
	require.Equal(t, "CURRENT_TIMESTAMP + INTERVAL 1 HOUR", ev.At)
	be, ok := ev.AtExpr.(*ast.BinaryExpr)
	require.True(t, ok, "got %T", ev.AtExpr)
	require.Equal(t, "+", be.Op)
	fc, ok := be.Lhs.(*ast.FuncCall)
	require.True(t, ok, "lhs got %T", be.Lhs)
	require.Equal(t, "CURRENT_TIMESTAMP", fc.Name)
	iv, ok := be.Rhs.(*ast.IntervalLit)
	require.True(t, ok, "rhs got %T", be.Rhs)
	require.Equal(t, "HOUR", iv.Unit)
	require.Equal(t, "DELETE FROM t", ev.Body)
}

func TestParseCreateEvent_AtExprUnparseableKeepsRaw(t *testing.T) {
	stmts, errs := Parse("CREATE EVENT e ON SCHEDULE AT x y ON COMPLETION PRESERVE DO DELETE FROM t;")
	require.Empty(t, errs, "parse errors: %v", errs)
	ev := stmts[0].(*ast.CreateEvent)
	require.Nil(t, ev.AtExpr)
	require.Equal(t, "x y", ev.At)
	require.Equal(t, "PRESERVE", ev.OnCompletion)
}
