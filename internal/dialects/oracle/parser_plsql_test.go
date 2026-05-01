package oracle

import (
	"strings"
	"testing"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// astCounts tallies the AST node kinds produced by ParseRoutineBody.
// Used by the regression tests below to assert exact node counts so a
// future "phantom IfStmt / phantom LoopStmt" regression surfaces as a
// count mismatch rather than as a downstream PG syntax error.
type astCounts struct {
	ifs       int
	loops     int
	whiles    int
	blocks    int
	cases     int
	forRanges int
	forCurs   int
}

// countAST walks a slice of PLStmt and any nested bodies, summing
// per-kind counts.
func countAST(stmts []ast.PLStmt, c *astCounts) {
	for _, s := range stmts {
		switch x := s.(type) {
		case *ast.Block:
			c.blocks++
			countAST(x.Stmts, c)
		case *ast.IfStmt:
			c.ifs++
			for _, br := range x.Branches {
				countAST(br.Body, c)
			}
			countAST(x.Else, c)
		case *ast.LoopStmt:
			c.loops++
			countAST(x.Body, c)
		case *ast.WhileStmt:
			c.whiles++
			countAST(x.Body, c)
		case *ast.CaseStmt:
			c.cases++
			for _, w := range x.When {
				countAST(w.Body, c)
			}
			countAST(x.Else, c)
		case *ast.NumericForStmt:
			c.forRanges++
			countAST(x.Body, c)
		case *ast.CursorForStmt:
			c.forCurs++
			countAST(x.Body, c)
		}
	}
}

// TestRepro_LoopWithOpenForFetchEndIfTail is the minimal reproducer for
// the PRC_MAJ_JRN bug pattern: OPEN cur FOR <select>; LOOP FETCH...; EXIT
// WHEN...; assign; END LOOP; followed by IF/CLOSE/...; then further
// statements (e.g. EXECUTE IMMEDIATE + tail assignment). Pre-fix, the
// EXECUTE IMMEDIATE expression capture didn't stop at `;`, so it
// swallowed every subsequent statement up to EOF, dropping the tail
// assignment from the AST and causing parsePLBlock's outer END to be
// "expected END (got "")".
func TestRepro_LoopWithOpenForFetchEndIfTail(t *testing.T) {
	src := `DECLARE
  l_rc_curs SYS_REFCURSOR;
  v VARCHAR2(100);
  l_vc_stmt VARCHAR2(4000);
BEGIN
  OPEN l_rc_curs FOR
    SELECT col FROM all_tab_columns WHERE owner = 'X' ORDER BY column_id;
  LOOP
    FETCH l_rc_curs INTO v;
    EXIT WHEN l_rc_curs%NOTFOUND;
    l_vc_stmt := l_vc_stmt || v || 'END;';
  END LOOP;
  IF (l_rc_curs%ISOPEN) THEN
    CLOSE l_rc_curs;
  END IF;
  EXECUTE IMMEDIATE l_vc_stmt;
  l_vc_stmt := 'tail';
END;`

	stmts, errs := ParseRoutineBody(src)
	if len(errs) != 0 {
		for i, e := range errs {
			t.Logf("  parse error #%d: %v", i, e)
		}
		t.Fatalf("unexpected parse errors: %d", len(errs))
	}
	if len(stmts) != 1 {
		t.Fatalf("expected single Block, got %d top-level stmts", len(stmts))
	}
	blk, ok := stmts[0].(*ast.Block)
	if !ok {
		t.Fatalf("expected Block, got %T", stmts[0])
	}
	var c astCounts
	countAST(blk.Stmts, &c)
	if c.loops != 1 {
		t.Errorf("expected 1 LoopStmt, got %d (suggests phantom or missing loop)", c.loops)
	}
	if c.ifs != 1 {
		t.Errorf("expected 1 IfStmt, got %d (suggests phantom or missing if)", c.ifs)
	}
	// Tail assignment must be present: pre-fix, EXECUTE IMMEDIATE
	// swallowed it as part of its SQL expression. After the Phase 1.2
	// migration AssignStmt.Expr is a typed *ast.Literal (not RawExpr),
	// so accept either form — what matters is the literal "tail" reaches
	// the AST.
	hasTailAssign := false
	for _, s := range blk.Stmts {
		a, ok := s.(*ast.AssignStmt)
		if !ok {
			continue
		}
		switch x := a.Expr.(type) {
		case *ast.RawExpr:
			if strings.Contains(strings.ToLower(x.Text), "tail") {
				hasTailAssign = true
			}
		case *ast.Literal:
			if x.Kind == "string" && strings.Contains(strings.ToLower(x.Text), "tail") {
				hasTailAssign = true
			}
		}
	}
	if !hasTailAssign {
		t.Errorf("tail assignment 'l_vc_stmt := ''tail'';' missing from Block.Stmts (truncation)")
	}
}

// TestRepro_SevenNestedIfsInLoopBody mirrors the PRC_MAJ_TRC pattern:
// seven IFs nested inside a LOOP body (with the unusual
// `((arr(i) IS NOT NULL) = FALSE)` boolean-equality cond), plus one IF
// after the LOOP. Asserts the body parser produces exactly 8 IfStmt
// nodes (no phantoms) so a future regression on `parseStmtsUntilAny`
// END-greediness surfaces here rather than as an extra `END IF;` in the
// generated PG DDL.
func TestRepro_SevenNestedIfsInLoopBody(t *testing.T) {
	src := `DECLARE
  l_i_nb_col NUMBER := 0;
  l_i_nb_tab NUMBER := 0;
  l_bo_add_comment BOOLEAN := FALSE;
  l_vc_comments VARCHAR2(400);
  l_i_indice_index NUMBER;
  l_i_indice_comment NUMBER;
  l_vc_stmt_add VARCHAR2(4000);
  l_vc_stmt_modify VARCHAR2(4000);
  l_sqlcmd VARCHAR2(4000);
  l_vc_lst_cp_col VARCHAR2(400);
  l_rc_curs SYS_REFCURSOR;
  l_vc_colonne_req VARCHAR2(400);
  l_vc_col_old VARCHAR2(400);
  l_vc_col_new VARCHAR2(400);
  l_vc_col VARCHAR2(400);
  TYPE t_string_array IS TABLE OF VARCHAR2(4000) INDEX BY BINARY_INTEGER;
  l_tab_index t_string_array;
  l_tab_commentaire t_string_array;
BEGIN
  LOOP
    FETCH l_rc_curs INTO l_vc_colonne_req, l_vc_col_old, l_vc_col_new, l_vc_col, l_vc_comments;
    EXIT WHEN l_rc_curs%NOTFOUND;
    l_vc_lst_cp_col := l_vc_colonne_req;
    SELECT count(*) INTO l_i_nb_col FROM all_tab_columns WHERE owner = 'X';

    IF (l_i_nb_col = 0 and l_i_nb_tab != 0) THEN
      l_vc_stmt_add := 'add';
    END IF;

    IF (l_bo_add_comment = TRUE or l_i_nb_col = 0) THEN
      l_i_indice_index := 1;

      IF ((l_tab_index(l_i_indice_index) IS NOT NULL) = FALSE) THEN
        l_tab_index(1) := 'foo';
      END IF;

      l_tab_index(l_i_indice_index) := 'CREATE INDEX foo';

      IF (length(l_vc_comments) > 0) THEN
        l_i_indice_comment := 1;

        IF ((l_tab_commentaire(l_i_indice_comment) IS NOT NULL) = FALSE) THEN
          l_tab_commentaire(1) := 'bar';
        END IF;

        l_tab_commentaire(l_i_indice_comment) := 'COMMENT ON COLUMN x';
      END IF;
    END IF;

    IF (l_i_nb_col != 0 and l_i_nb_tab != 0) THEN
      SELECT count(*) INTO l_i_nb_col FROM all_tab_columns WHERE owner = 'Y';
      IF (l_i_nb_col != 0 and l_i_nb_tab != 0) THEN
        l_vc_stmt_modify := 'modify';
      END IF;
    END IF;
  END LOOP;

  IF (l_i_nb_tab = 0) THEN
    l_sqlcmd := 'cmd';
  END IF;
END;`

	stmts, errs := ParseRoutineBody(src)
	if len(errs) != 0 {
		for i, e := range errs {
			t.Logf("  parse error #%d: %v", i, e)
		}
		t.Fatalf("unexpected parse errors: %d", len(errs))
	}
	if len(stmts) != 1 {
		t.Fatalf("expected single Block, got %d top-level stmts", len(stmts))
	}
	blk, ok := stmts[0].(*ast.Block)
	if !ok {
		t.Fatalf("expected Block, got %T", stmts[0])
	}
	var c astCounts
	countAST(blk.Stmts, &c)
	if c.loops != 1 {
		t.Errorf("expected 1 LoopStmt, got %d (phantom/missing loop)", c.loops)
	}
	// 7 IFs inside LOOP body + 1 IF after LOOP = 8 IfStmt nodes total.
	if c.ifs != 8 {
		t.Errorf("expected 8 IfStmt nodes (7 in LOOP body + 1 after), got %d", c.ifs)
	}
}

// TestPLSQL_WhileCondTyped asserts WhileStmt.Cond is a structured
// expression (BinaryExpr / UnaryExpr / etc.) rather than the legacy
// *ast.RawExpr fallback. Phase 1.1 of the AST-only refactor migrates
// parseWhileStmt from parseExprUntilKeyword to parseExprUntil so the
// translator can match on Op/Lhs/Rhs without reparsing text.
func TestPLSQL_WhileCondTyped(t *testing.T) {
	src := `BEGIN
		WHILE i < 10 AND active LOOP
			i := i + 1;
		END LOOP;
	END;`
	stmts, errs := ParseRoutineBody(src)
	if len(errs) != 0 {
		t.Fatalf("unexpected parse errors: %v", errs)
	}
	blk, ok := stmts[0].(*ast.Block)
	if !ok {
		t.Fatalf("want Block, got %T", stmts[0])
	}
	w, ok := blk.Stmts[0].(*ast.WhileStmt)
	if !ok {
		t.Fatalf("want WhileStmt, got %T", blk.Stmts[0])
	}
	if _, raw := w.Cond.(*ast.RawExpr); raw {
		t.Fatalf("WhileStmt.Cond is *ast.RawExpr — expected typed Expr (BinaryExpr at the AND root)")
	}
	bin, ok := w.Cond.(*ast.BinaryExpr)
	if !ok {
		t.Fatalf("WhileStmt.Cond want *ast.BinaryExpr, got %T", w.Cond)
	}
	if !strings.EqualFold(bin.Op, "AND") {
		t.Errorf("top-level op want AND, got %q", bin.Op)
	}
	// Left side is `i < 10` — also a BinaryExpr with Op `<`.
	lhs, ok := bin.Lhs.(*ast.BinaryExpr)
	if !ok {
		t.Fatalf("WhileStmt.Cond.Lhs want *ast.BinaryExpr, got %T", bin.Lhs)
	}
	if lhs.Op != "<" {
		t.Errorf("Lhs op want '<', got %q", lhs.Op)
	}
}

// TestPLSQL_ExceptionBlockTyped asserts Block.Except is populated with
// a typed ExceptionBlock (handler names + parsed body PLStmts) in
// parallel with the legacy /*EXCEPTION*/ RawSQL marker that the
// text-pipeline translator still consumes.
func TestPLSQL_ExceptionBlockTyped(t *testing.T) {
	src := `BEGIN
		NULL;
	EXCEPTION
		WHEN no_data_found THEN
			NULL;
		WHEN OTHERS THEN
			RAISE;
	END;`
	stmts, errs := ParseRoutineBody(src)
	if len(errs) != 0 {
		t.Fatalf("unexpected parse errors: %v", errs)
	}
	blk, ok := stmts[0].(*ast.Block)
	if !ok {
		t.Fatalf("want Block, got %T", stmts[0])
	}
	if blk.Except == nil {
		t.Fatalf("Block.Except must be populated by Phase 1.10")
	}
	if len(blk.Except.Handlers) != 2 {
		t.Fatalf("Except.Handlers want 2, got %d", len(blk.Except.Handlers))
	}
	h0 := blk.Except.Handlers[0]
	if len(h0.Names) != 1 || !strings.EqualFold(h0.Names[0], "no_data_found") {
		t.Errorf("handler[0].Names want [no_data_found], got %v", h0.Names)
	}
	if len(h0.Body) == 0 {
		t.Errorf("handler[0].Body must not be empty (NULL;)")
	}
	h1 := blk.Except.Handlers[1]
	if len(h1.Names) != 1 || !strings.EqualFold(h1.Names[0], "OTHERS") {
		t.Errorf("handler[1].Names want [OTHERS], got %v", h1.Names)
	}
	if len(h1.Body) == 0 {
		t.Errorf("handler[1].Body must not be empty (RAISE;)")
	}
}

// TestPLSQL_SelectIntoStmtTyped asserts that SelectInto now populates
// the typed Stmt field via the DML parser, in addition to the legacy
// RawQuery text. Both must point at the same SELECT body so consumers
// can opportunistically prefer Stmt while RawQuery stays available
// during the transition window.
func TestPLSQL_SelectIntoStmtTyped(t *testing.T) {
	src := `BEGIN
		SELECT id, name INTO v_id, v_name FROM users WHERE id = 1;
	END;`
	stmts, errs := ParseRoutineBody(src)
	if len(errs) != 0 {
		t.Fatalf("unexpected parse errors: %v", errs)
	}
	blk, ok := stmts[0].(*ast.Block)
	if !ok {
		t.Fatalf("want Block, got %T", stmts[0])
	}
	si, ok := blk.Stmts[0].(*ast.SelectInto)
	if !ok {
		t.Fatalf("want *SelectInto, got %T", blk.Stmts[0])
	}
	if len(si.Vars) != 2 ||
		!strings.EqualFold(si.Vars[0], "v_id") ||
		!strings.EqualFold(si.Vars[1], "v_name") {
		t.Errorf("Vars want [v_id v_name] (case-insensitive), got %v", si.Vars)
	}
	if si.RawQuery == "" {
		t.Errorf("RawQuery must remain populated for legacy consumers")
	}
	if si.Stmt == nil {
		t.Fatalf("Stmt must be populated by Phase 1.9 typed path")
	}
	if len(si.Stmt.Cols) != 2 {
		t.Errorf("Stmt.Cols want 2, got %d", len(si.Stmt.Cols))
	}
}

// TestPLSQL_ExecuteImmediateConcatTree asserts that the SQL operand of
// EXECUTE IMMEDIATE is now a typed concat tree (BinaryExpr "||" of
// Literal/Ident operands) rather than *ast.RawExpr. This is the
// foundation Phase 4's dynamic-DDL rewriter walks: previously the raw
// source had to be re-tokenised on every `||` boundary by hand.
func TestPLSQL_ExecuteImmediateConcatTree(t *testing.T) {
	src := `BEGIN
		EXECUTE IMMEDIATE 'CREATE TABLE ' || v_name || ' (id NUMBER)';
	END;`
	stmts, errs := ParseRoutineBody(src)
	if len(errs) != 0 {
		t.Fatalf("unexpected parse errors: %v", errs)
	}
	blk, ok := stmts[0].(*ast.Block)
	if !ok {
		t.Fatalf("want Block, got %T", stmts[0])
	}
	ei, ok := blk.Stmts[0].(*ast.ExecuteImmediateStmt)
	if !ok {
		t.Fatalf("want ExecuteImmediateStmt, got %T", blk.Stmts[0])
	}
	if _, raw := ei.SQL.(*ast.RawExpr); raw {
		t.Fatalf("ExecuteImmediateStmt.SQL is *ast.RawExpr — expected typed concat tree")
	}
	bin, ok := ei.SQL.(*ast.BinaryExpr)
	if !ok {
		t.Fatalf("ExecuteImmediateStmt.SQL want *ast.BinaryExpr, got %T", ei.SQL)
	}
	if bin.Op != "||" {
		t.Errorf("top-level op want ||, got %q", bin.Op)
	}
	// Walk the concat tree and confirm we see at least two string
	// literals plus one ident — Phase 4 needs all three to round-trip
	// the dynamic statement.
	var literals, idents int
	var walk func(ast.Expr)
	walk = func(e ast.Expr) {
		switch x := e.(type) {
		case *ast.BinaryExpr:
			walk(x.Lhs)
			walk(x.Rhs)
		case *ast.Literal:
			literals++
		case *ast.Ident:
			idents++
		}
	}
	walk(ei.SQL)
	if literals < 2 {
		t.Errorf("want >=2 string literals in the concat tree, got %d", literals)
	}
	if idents < 1 {
		t.Errorf("want >=1 ident in the concat tree, got %d", idents)
	}
}

// TestPLSQL_SchemaQualifiedFuncCall pins the Phase 6.3 parser fix:
// `pkg.fn(args)` and `schema.pkg.fn(args)` at statement position now
// parse as a typed *ast.FuncCall rather than leaving the `(` dangling
// for the next stmt loop iteration (which previously surfaced as
// "unexpected token in PL/SQL body (got "(", expected stmt)" 56+
// times in notice.drse).
func TestPLSQL_SchemaQualifiedFuncCall(t *testing.T) {
	src := `BEGIN
		fFile := UTL_FILE.FOPEN(rep, 'log.txt', 'w');
	END;`
	stmts, errs := ParseRoutineBody(src)
	if len(errs) != 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	blk := stmts[0].(*ast.Block)
	a, ok := blk.Stmts[0].(*ast.AssignStmt)
	if !ok {
		t.Fatalf("want AssignStmt, got %T", blk.Stmts[0])
	}
	fc, ok := a.Expr.(*ast.FuncCall)
	if !ok {
		t.Fatalf("AssignStmt.Expr want *ast.FuncCall, got %T", a.Expr)
	}
	if !strings.EqualFold(fc.Name, "UTL_FILE.FOPEN") {
		t.Errorf("FuncCall.Name want UTL_FILE.FOPEN, got %q", fc.Name)
	}
	if len(fc.Args) != 3 {
		t.Errorf("FuncCall.Args want 3, got %d", len(fc.Args))
	}
}

// TestPLSQL_SequenceRefTyped asserts the parser emits *ast.SequenceRef
// for seq.NEXTVAL / seq.CURRVAL (and the schema-qualified shape)
// inside expression context.
func TestPLSQL_SequenceRefTyped(t *testing.T) {
	src := `BEGIN
		x := my_seq.NEXTVAL;
		y := hr.emp_seq.CURRVAL;
	END;`
	stmts, errs := ParseRoutineBody(src)
	if len(errs) != 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	blk, ok := stmts[0].(*ast.Block)
	if !ok {
		t.Fatalf("want Block, got %T", stmts[0])
	}

	// First assignment — bare seq.NEXTVAL.
	a0 := blk.Stmts[0].(*ast.AssignStmt)
	ref, ok := a0.Expr.(*ast.SequenceRef)
	if !ok {
		t.Fatalf("a0.Expr want *SequenceRef, got %T", a0.Expr)
	}
	if !strings.EqualFold(ref.Name, "my_seq") {
		t.Errorf("Name want my_seq, got %q", ref.Name)
	}
	if ref.Schema != "" {
		t.Errorf("Schema must be empty for bare seq, got %q", ref.Schema)
	}
	if ref.Op != "NEXTVAL" {
		t.Errorf("Op want NEXTVAL, got %q", ref.Op)
	}

	// Second assignment — schema-qualified seq.CURRVAL.
	a1 := blk.Stmts[1].(*ast.AssignStmt)
	ref2, ok := a1.Expr.(*ast.SequenceRef)
	if !ok {
		t.Fatalf("a1.Expr want *SequenceRef, got %T", a1.Expr)
	}
	if !strings.EqualFold(ref2.Schema, "hr") {
		t.Errorf("Schema want hr, got %q", ref2.Schema)
	}
	if !strings.EqualFold(ref2.Name, "emp_seq") {
		t.Errorf("Name want emp_seq, got %q", ref2.Name)
	}
	if ref2.Op != "CURRVAL" {
		t.Errorf("Op want CURRVAL, got %q", ref2.Op)
	}
}

// TestPLSQL_CursorAttrTyped asserts that Oracle cursor attribute
// references (cur%FOUND / %NOTFOUND / %ISOPEN / %ROWCOUNT) parse to
// *ast.CursorAttr inside expressions. This unblocks the typed
// EXIT WHEN cond migration completed in the same commit.
func TestPLSQL_CursorAttrTyped(t *testing.T) {
	src := `BEGIN
		LOOP
		  EXIT WHEN c1%NOTFOUND;
		  CONTINUE WHEN c1%FOUND;
		END LOOP;
	END;`
	stmts, errs := ParseRoutineBody(src)
	if len(errs) != 0 {
		t.Fatalf("unexpected parse errors: %v", errs)
	}
	blk, ok := stmts[0].(*ast.Block)
	if !ok {
		t.Fatalf("want Block, got %T", stmts[0])
	}
	loop, ok := blk.Stmts[0].(*ast.LoopStmt)
	if !ok {
		t.Fatalf("want LoopStmt, got %T", blk.Stmts[0])
	}
	exit := loop.Body[0].(*ast.LeaveStmt)
	ca, ok := exit.WhenCond.(*ast.CursorAttr)
	if !ok {
		t.Fatalf("LeaveStmt.WhenCond want *CursorAttr, got %T", exit.WhenCond)
	}
	if strings.ToLower(ca.Cursor) != "c1" {
		t.Errorf("CursorAttr.Cursor want c1, got %q", ca.Cursor)
	}
	if ca.Attr != "NOTFOUND" {
		t.Errorf("CursorAttr.Attr want NOTFOUND, got %q", ca.Attr)
	}

	cont := loop.Body[1].(*ast.IterateStmt)
	ca2, ok := cont.WhenCond.(*ast.CursorAttr)
	if !ok {
		t.Fatalf("IterateStmt.WhenCond want *CursorAttr, got %T", cont.WhenCond)
	}
	if ca2.Attr != "FOUND" {
		t.Errorf("CursorAttr.Attr want FOUND, got %q", ca2.Attr)
	}
}

// TestPLSQL_NumericForBoundsTyped asserts NumericForStmt.Low and High
// are typed Expr nodes after Phase 1.4. Pre-migration both came back
// as *ast.RawExpr from parseExprUntilAnyKeywordOrRange.
func TestPLSQL_NumericForBoundsTyped(t *testing.T) {
	src := `BEGIN
		FOR i IN 1 .. n + 5 LOOP
		  NULL;
		END LOOP;
	END;`
	stmts, errs := ParseRoutineBody(src)
	if len(errs) != 0 {
		t.Fatalf("unexpected parse errors: %v", errs)
	}
	blk, ok := stmts[0].(*ast.Block)
	if !ok {
		t.Fatalf("want Block, got %T", stmts[0])
	}
	nf, ok := blk.Stmts[0].(*ast.NumericForStmt)
	if !ok {
		t.Fatalf("want NumericForStmt, got %T", blk.Stmts[0])
	}
	if _, raw := nf.Low.(*ast.RawExpr); raw {
		t.Errorf("NumericForStmt.Low is *ast.RawExpr — expected typed Literal")
	}
	if lit, ok := nf.Low.(*ast.Literal); !ok || lit.Text != "1" {
		t.Errorf("NumericForStmt.Low want Literal{1}, got %T %v", nf.Low, nf.Low)
	}
	if _, raw := nf.High.(*ast.RawExpr); raw {
		t.Errorf("NumericForStmt.High is *ast.RawExpr — expected typed BinaryExpr")
	}
	if bin, ok := nf.High.(*ast.BinaryExpr); !ok || bin.Op != "+" {
		t.Errorf("NumericForStmt.High want BinaryExpr{+}, got %T %v", nf.High, nf.High)
	}
}

// TestPLSQL_ExitContinueWhenTyped asserts EXIT WHEN cond and CONTINUE
// WHEN cond now produce typed *ast.LeaveStmt / *ast.IterateStmt with
// the cond stored on the WhenCond field. Pre-Phase 1.3 the parser
// emitted *ast.RawSQL with the entire `EXIT label WHEN <cond_text>`
// reassembled, forcing the translator to re-parse the cond text via
// rewriteMySQLBody.
func TestPLSQL_ExitContinueWhenTyped(t *testing.T) {
	src := `BEGIN
		LOOP
		  EXIT WHEN i > 10;
		  CONTINUE WHEN i < 0;
		END LOOP;
	END;`
	stmts, errs := ParseRoutineBody(src)
	if len(errs) != 0 {
		t.Fatalf("unexpected parse errors: %v", errs)
	}
	blk, ok := stmts[0].(*ast.Block)
	if !ok {
		t.Fatalf("want Block, got %T", stmts[0])
	}
	loop, ok := blk.Stmts[0].(*ast.LoopStmt)
	if !ok {
		t.Fatalf("want LoopStmt, got %T", blk.Stmts[0])
	}
	if len(loop.Body) < 2 {
		t.Fatalf("loop body want >=2 stmts, got %d", len(loop.Body))
	}

	// EXIT WHEN i > 10 → *LeaveStmt{WhenCond: <expr>}
	exit, ok := loop.Body[0].(*ast.LeaveStmt)
	if !ok {
		t.Fatalf("loop body[0] want *LeaveStmt, got %T", loop.Body[0])
	}
	if exit.WhenCond == nil {
		t.Errorf("LeaveStmt.WhenCond must be non-nil for EXIT WHEN form")
	}

	// CONTINUE WHEN i < 0 → *IterateStmt{WhenCond: <expr>}
	cont, ok := loop.Body[1].(*ast.IterateStmt)
	if !ok {
		t.Fatalf("loop body[1] want *IterateStmt, got %T", loop.Body[1])
	}
	if cont.WhenCond == nil {
		t.Errorf("IterateStmt.WhenCond must be non-nil for CONTINUE WHEN form")
	}
}

// TestPLSQL_AssignReturnDefaultTyped asserts AssignStmt.Expr,
// ReturnStmt.Expr, and DeclareVar.Default are typed Expr nodes after
// Phase 1.2's parseExprUntil migration. Each was previously a
// *ast.RawExpr capturing source text verbatim.
func TestPLSQL_AssignReturnDefaultTyped(t *testing.T) {
	src := `DECLARE
		x NUMBER := 10 + 5;
		y VARCHAR2(20) DEFAULT 'hello' || ' world';
	BEGIN
		x := y || x * 2;
		RETURN x + 1;
	END;`
	stmts, errs := ParseRoutineBody(src)
	if len(errs) != 0 {
		t.Fatalf("unexpected parse errors: %v", errs)
	}
	blk, ok := stmts[0].(*ast.Block)
	if !ok {
		t.Fatalf("want Block, got %T", stmts[0])
	}
	if len(blk.Decls) != 2 {
		t.Fatalf("want 2 decls, got %d", len(blk.Decls))
	}

	// DeclareVar.Default — `10 + 5` BinaryExpr.
	d0, ok := blk.Decls[0].(*ast.DeclareVar)
	if !ok {
		t.Fatalf("decl[0] want *ast.DeclareVar, got %T", blk.Decls[0])
	}
	if _, raw := d0.Default.(*ast.RawExpr); raw {
		t.Errorf("decl[0].Default is *ast.RawExpr — expected typed BinaryExpr")
	}
	if bin, ok := d0.Default.(*ast.BinaryExpr); !ok || bin.Op != "+" {
		t.Errorf("decl[0].Default want BinaryExpr{+}, got %T %v", d0.Default, d0.Default)
	}

	// DeclareVar.Default — `'hello' || ' world'` BinaryExpr (||).
	d1, ok := blk.Decls[1].(*ast.DeclareVar)
	if !ok {
		t.Fatalf("decl[1] want *ast.DeclareVar, got %T", blk.Decls[1])
	}
	if bin, ok := d1.Default.(*ast.BinaryExpr); !ok || bin.Op != "||" {
		t.Errorf("decl[1].Default want BinaryExpr{||}, got %T %v", d1.Default, d1.Default)
	}

	// AssignStmt.Expr — `y || x * 2` ; precedence: `||` is concat,
	// `*` is multiplication.  Expected tree: ((y || (x*2))).
	a, ok := blk.Stmts[0].(*ast.AssignStmt)
	if !ok {
		t.Fatalf("stmt[0] want *ast.AssignStmt, got %T", blk.Stmts[0])
	}
	if _, raw := a.Expr.(*ast.RawExpr); raw {
		t.Errorf("AssignStmt.Expr is *ast.RawExpr — expected typed BinaryExpr")
	}
	if bin, ok := a.Expr.(*ast.BinaryExpr); !ok || bin.Op != "||" {
		t.Errorf("AssignStmt.Expr want BinaryExpr{||}, got %T %v", a.Expr, a.Expr)
	}

	// ReturnStmt.Expr — `x + 1` BinaryExpr.
	r, ok := blk.Stmts[1].(*ast.ReturnStmt)
	if !ok {
		t.Fatalf("stmt[1] want *ast.ReturnStmt, got %T", blk.Stmts[1])
	}
	if r.Expr == nil {
		t.Fatal("ReturnStmt.Expr must not be nil")
	}
	if _, raw := r.Expr.(*ast.RawExpr); raw {
		t.Errorf("ReturnStmt.Expr is *ast.RawExpr — expected typed BinaryExpr")
	}
	if bin, ok := r.Expr.(*ast.BinaryExpr); !ok || bin.Op != "+" {
		t.Errorf("ReturnStmt.Expr want BinaryExpr{+}, got %T %v", r.Expr, r.Expr)
	}
}

// TestPLSQL_CaseStmtMatchTyped asserts simple-CASE Expr and searched-
// CASE WHEN Match are both typed Expr nodes after the parseExprUntil
// migration.
func TestPLSQL_CaseStmtMatchTyped(t *testing.T) {
	src := `BEGIN
		CASE i
		  WHEN 1 THEN x := 'a';
		  WHEN 2 THEN x := 'b';
		  ELSE x := 'z';
		END CASE;
		CASE
		  WHEN i > 0 THEN y := 1;
		  WHEN i < 0 THEN y := -1;
		  ELSE y := 0;
		END CASE;
	END;`
	stmts, errs := ParseRoutineBody(src)
	if len(errs) != 0 {
		t.Fatalf("unexpected parse errors: %v", errs)
	}
	blk, ok := stmts[0].(*ast.Block)
	if !ok {
		t.Fatalf("want Block, got %T", stmts[0])
	}
	if len(blk.Stmts) < 2 {
		t.Fatalf("want >=2 statements, got %d", len(blk.Stmts))
	}

	// Simple CASE: Expr is the operand `i`, Match values are 1/2.
	simple, ok := blk.Stmts[0].(*ast.CaseStmt)
	if !ok {
		t.Fatalf("simple-CASE want *ast.CaseStmt, got %T", blk.Stmts[0])
	}
	if simple.Expr == nil {
		t.Fatal("simple-CASE Expr must not be nil")
	}
	if _, raw := simple.Expr.(*ast.RawExpr); raw {
		t.Errorf("simple-CASE Expr is *ast.RawExpr — expected typed Ident")
	}
	if id, ok := simple.Expr.(*ast.Ident); !ok || strings.ToLower(id.Parts[0]) != "i" {
		t.Errorf("simple-CASE Expr want Ident{i}, got %T %v", simple.Expr, simple.Expr)
	}
	for i, w := range simple.When {
		if _, raw := w.Match.(*ast.RawExpr); raw {
			t.Errorf("simple-CASE When[%d].Match is *ast.RawExpr — expected typed Literal", i)
		}
	}

	// Searched CASE: Expr is nil, Match values are BinaryExpr (i > 0, i < 0).
	searched, ok := blk.Stmts[1].(*ast.CaseStmt)
	if !ok {
		t.Fatalf("searched-CASE want *ast.CaseStmt, got %T", blk.Stmts[1])
	}
	if searched.Expr != nil {
		t.Errorf("searched-CASE Expr should be nil (no operand), got %T", searched.Expr)
	}
	for i, w := range searched.When {
		if _, raw := w.Match.(*ast.RawExpr); raw {
			t.Errorf("searched-CASE When[%d].Match is *ast.RawExpr — expected typed BinaryExpr", i)
			continue
		}
		bin, ok := w.Match.(*ast.BinaryExpr)
		if !ok {
			t.Errorf("searched-CASE When[%d].Match want *ast.BinaryExpr, got %T", i, w.Match)
			continue
		}
		if bin.Op != ">" && bin.Op != "<" {
			t.Errorf("searched-CASE When[%d].Match.Op want '>' or '<', got %q", i, bin.Op)
		}
	}
}
