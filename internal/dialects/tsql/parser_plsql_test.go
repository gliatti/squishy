package tsql

import (
	"testing"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

func TestBody_BlockAndDeclare(t *testing.T) {
	blk := mustBodyOne[*ast.Block](t, `BEGIN
    DECLARE @a int = 1, @b varchar(10), @t TABLE (id int PRIMARY KEY, name nvarchar(50) NULL);
    DECLARE c CURSOR LOCAL FAST_FORWARD FOR SELECT id FROM dbo.t WHERE x = @a;
    DECLARE c2 CURSOR FOR SELECT id FROM t FOR UPDATE OF id
    SET @a += 2
    SET @b = N'x'
END`)
	if len(blk.Stmts) != 5 || len(blk.Decls) != 0 {
		t.Fatalf("block: %d stmts, %d decls", len(blk.Stmts), len(blk.Decls))
	}
	ds := as[*ast.DeclareStmt](t, blk.Stmts[0])
	if len(ds.Decls) != 3 {
		t.Fatalf("multi DECLARE: %+v", ds.Decls)
	}
	a := as[*ast.DeclareVar](t, ds.Decls[0])
	if a.Name != "@a" || as[*ast.Literal](t, a.Default).Text != "1" {
		t.Errorf("@a: %+v", a)
	}
	if b := as[*ast.DeclareVar](t, ds.Decls[1]); b.Default != nil || as[*ast.CharType](t, b.Type).Length != 10 {
		t.Errorf("@b: %+v", b)
	}
	tv := as[*ast.TSQLTableType](t, as[*ast.DeclareVar](t, ds.Decls[2]).Type)
	if len(tv.Columns) != 2 || len(tv.Constraints) != 1 {
		t.Errorf("table variable: %+v", tv)
	}
	dc := as[*ast.DeclareCursor](t, as[*ast.DeclareStmt](t, blk.Stmts[1]).Decls[0])
	if dc.Name != "c" || dc.Stmt == nil || len(dc.Options) != 2 || dc.Options[1] != "FAST_FORWARD" || dc.SelectBody != "SELECT id FROM dbo.t WHERE x = @a" {
		t.Errorf("cursor: %+v", dc)
	}
	dc2 := as[*ast.DeclareCursor](t, as[*ast.DeclareStmt](t, blk.Stmts[2]).Decls[0])
	if dc2.ForUpdate != "FOR UPDATE OF id" {
		t.Errorf("cursor FOR UPDATE: %+v", dc2)
	}
	inc := as[*ast.AssignStmt](t, blk.Stmts[3])
	if be := as[*ast.BinaryExpr](t, inc.Expr); inc.Target != "@a" || be.Op != "+" || identText(t, be.Lhs) != "@a" {
		t.Errorf("SET @a += 2: %+v", inc)
	}
	as[*ast.AssignStmt](t, blk.Stmts[4])
}

func TestBody_SetOptions(t *testing.T) {
	stmts := mustBody(t, `SET NOCOUNT ON;
SET XACT_ABORT ON
SET XACT_ABORT OFF
SET ANSI_WARNINGS OFF
SET ROWCOUNT 10
SET TRANSACTION ISOLATION LEVEL SERIALIZABLE`)
	wantWarn := []bool{false, false, true, true, true, true}
	if len(stmts) != len(wantWarn) {
		t.Fatalf("want %d statements, got %d", len(wantWarn), len(stmts))
	}
	for i, w := range wantWarn {
		ns := as[*ast.NullStmt](t, stmts[i])
		if ns.Warn != w {
			t.Errorf("%q: want Warn %v", ns.Source, w)
		}
	}
	want(t, as[*ast.NullStmt](t, stmts[0]).Source, "SET NOCOUNT ON")
}

func TestBody_SelectAssignment(t *testing.T) {
	stmts := mustBody(t, `SELECT @DealerPrice = plph.[ListPrice] * @DealerDiscount, @n += 1
FROM [Production].[Product] p WHERE p.id = @ProductID
SELECT a, b FROM t`)
	if len(stmts) != 2 {
		t.Fatalf("want 2 statements, got %d", len(stmts))
	}
	si := as[*ast.SelectInto](t, stmts[0])
	if len(si.Vars) != 2 || si.Vars[0] != "@DealerPrice" || si.Vars[1] != "@n" || si.Stmt == nil || len(si.Stmt.Cols) != 2 {
		t.Errorf("SELECT @v = …: %+v", si)
	}
	if be := as[*ast.BinaryExpr](t, si.Stmt.Cols[1].Expr); be.Op != "+" {
		t.Errorf("compound select assignment: %+v", be)
	}
	as[*ast.SelectStmt](t, stmts[1])
	if _, errs := ParseRoutineBody("SELECT @a = 1, b FROM t"); len(errs) == 0 {
		t.Errorf("mixing assignment and data retrieval must be an error")
	}
}

func TestBody_ControlFlow(t *testing.T) {
	stmts := mustBody(t, `IF @x = 1 PRINT 'one'; ELSE IF @x = 2 PRINT 'two' ELSE BEGIN PRINT 'many' END
IF EXISTS (SELECT 1 FROM t) RETURN
WHILE @i < 10
BEGIN
    SET @i = @i + 1
    IF @i = 5 CONTINUE
    IF @i = 8 BREAK
END
RETURN @i * 2
lbl:
GOTO lbl`)
	if len(stmts) != 6 {
		t.Fatalf("want 6 statements, got %d", len(stmts))
	}
	ifs := as[*ast.IfStmt](t, stmts[0])
	if len(ifs.Branches) != 2 || len(ifs.Else) != 1 {
		t.Errorf("ELSE IF chain flattened: %+v", ifs)
	}
	as[*ast.Block](t, ifs.Else[0])
	ex := as[*ast.IfStmt](t, stmts[1])
	as[*ast.ExistsExpr](t, ex.Branches[0].Cond)
	if r := as[*ast.ReturnStmt](t, ex.Branches[0].Body[0]); r.Expr != nil {
		t.Errorf("bare RETURN must have no expression: %+v", r)
	}
	w := as[*ast.WhileStmt](t, stmts[2])
	body := as[*ast.Block](t, w.Body[0])
	as[*ast.IterateStmt](t, as[*ast.IfStmt](t, body.Stmts[1]).Branches[0].Body[0])
	as[*ast.LeaveStmt](t, as[*ast.IfStmt](t, body.Stmts[2]).Branches[0].Body[0])
	if r := as[*ast.ReturnStmt](t, stmts[3]); r.Expr == nil {
		t.Errorf("RETURN expr: %+v", r)
	}
	want(t, as[*ast.LabelStmt](t, stmts[4]).Label, "lbl")
	want(t, as[*ast.GotoStmt](t, stmts[5]).Label, "lbl")
}

func TestBody_TryCatchAndErrors(t *testing.T) {
	blk := mustBodyOne[*ast.Block](t, `BEGIN TRY
    BEGIN TRANSACTION;
    UPDATE t SET a = 1;
    COMMIT TRANSACTION;
END TRY
BEGIN CATCH
    IF @@TRANCOUNT > 0 ROLLBACK TRANSACTION;
    THROW;
END CATCH;`)
	if !blk.TryCatch || len(blk.Stmts) != 3 || blk.Except == nil || len(blk.Except.Handlers) != 1 {
		t.Fatalf("TRY/CATCH: %+v", blk)
	}
	h := blk.Except.Handlers[0]
	if len(h.Names) != 1 || h.Names[0] != "OTHERS" || len(h.Body) != 2 {
		t.Errorf("CATCH handler: %+v", h)
	}
	if ts := as[*ast.TransactionStmt](t, blk.Stmts[0]); ts.Kind != "BEGIN" {
		t.Errorf("BEGIN TRANSACTION: %+v", ts)
	}
	as[*ast.UpdateStmt](t, blk.Stmts[1])
	want(t, as[*ast.TransactionStmt](t, blk.Stmts[2]).Kind, "COMMIT")
	th := as[*ast.RaiseStmt](t, h.Body[1])
	if th.Kind != "THROW" || th.Level != "ERROR" || th.ErrNumber != nil {
		t.Errorf("bare THROW: %+v", th)
	}

	stmts := mustBody(t, `THROW 50001, N'bad', 1;
RAISERROR (N'Employees cannot be deleted.', 16, 1)
RAISERROR ('Beginning at %s ....', 10, 1, @dttm) WITH NOWAIT, LOG
RAISERROR (@msg, @sev, 1)
RAISERROR (50005, 11, 2)
PRINT 'x' + @y`)
	if len(stmts) != 6 {
		t.Fatalf("want 6 statements, got %d", len(stmts))
	}
	thr := as[*ast.RaiseStmt](t, stmts[0])
	if thr.Kind != "THROW" || thr.ErrNumber == nil || thr.Message == nil || thr.State == nil {
		t.Errorf("THROW n, msg, state: %+v", thr)
	}
	levels := []string{"ERROR", "NOTICE", "ERROR", "ERROR"}
	for i, lv := range levels {
		r := as[*ast.RaiseStmt](t, stmts[1+i])
		if r.Kind != "RAISERROR" || r.Level != lv {
			t.Errorf("RAISERROR %d: want level %s, got %+v", i, lv, r)
		}
	}
	notice := as[*ast.RaiseStmt](t, stmts[2])
	if len(notice.Args) != 1 || len(notice.Options) != 2 || notice.Options[0] != "NOWAIT" {
		t.Errorf("RAISERROR args / options: %+v", notice)
	}
	if num := as[*ast.RaiseStmt](t, stmts[4]); num.ErrNumber == nil || num.Message != nil {
		t.Errorf("RAISERROR with a message number: %+v", num)
	}
	pr := as[*ast.RaiseStmt](t, stmts[5])
	if pr.Kind != "PRINT" || pr.Level != "NOTICE" || pr.Message == nil {
		t.Errorf("PRINT: %+v", pr)
	}
}

func TestBody_Exec(t *testing.T) {
	stmts := mustBody(t, `EXECUTE [dbo].[uspLogError];
EXEC @rc = dbo.usp_create_order @customer_id = 1, @order_id = @id OUTPUT, 42, DEFAULT, @x OUT
EXEC (@sql + N' WHERE 1 = 1')
EXEC sp_executesql @stmt, N'@p int, @q int OUTPUT', @p = 1, @q = @r OUTPUT
EXEC sys.sp_executesql @stmt = N'SELECT 1'
EXECUTE AS CALLER`)
	if len(stmts) != 6 {
		t.Fatalf("want 6 statements, got %d", len(stmts))
	}
	c0 := as[*ast.CallStmt](t, stmts[0])
	if c0.Schema != "dbo" || c0.Name != "uspLogError" || len(c0.Args) != 0 {
		t.Errorf("EXECUTE proc: %+v", c0)
	}
	c1 := as[*ast.CallStmt](t, stmts[1])
	if c1.ReturnVar != "@rc" || c1.Name != "usp_create_order" || len(c1.Args) != 5 {
		t.Fatalf("EXEC with arguments: %+v", c1)
	}
	if na := as[*ast.NamedArg](t, c1.Args[0]); na.Name != "@customer_id" || na.Output {
		t.Errorf("named argument: %+v", na)
	}
	if na := as[*ast.NamedArg](t, c1.Args[1]); na.Name != "@order_id" || !na.Output {
		t.Errorf("named OUTPUT argument: %+v", na)
	}
	as[*ast.Literal](t, c1.Args[2])
	if d := as[*ast.Literal](t, c1.Args[3]); d.Kind != "default" {
		t.Errorf("DEFAULT argument: %+v", d)
	}
	if na := as[*ast.NamedArg](t, c1.Args[4]); na.Name != "" || !na.Output {
		t.Errorf("positional OUTPUT argument: %+v", na)
	}
	dyn := as[*ast.ExecuteImmediateStmt](t, stmts[2])
	if dyn.SPExecuteSQL || dyn.SQL == nil {
		t.Errorf("EXEC (expr): %+v", dyn)
	}
	sp := as[*ast.ExecuteImmediateStmt](t, stmts[3])
	if !sp.SPExecuteSQL || identText(t, sp.SQL) != "@stmt" || sp.ParamDecl == nil || len(sp.Using) != 2 {
		t.Errorf("sp_executesql: %+v", sp)
	}
	sp2 := as[*ast.ExecuteImmediateStmt](t, stmts[4])
	if !sp2.SPExecuteSQL || as[*ast.Literal](t, sp2.SQL).Text != "SELECT 1" {
		t.Errorf("sp_executesql @stmt =: %+v", sp2)
	}
	if ns := as[*ast.NullStmt](t, stmts[5]); !ns.Warn {
		t.Errorf("EXECUTE AS: %+v", ns)
	}
}

func TestBody_Cursors(t *testing.T) {
	stmts := mustBody(t, `OPEN c
FETCH NEXT FROM c INTO @a, @b
WHILE @@FETCH_STATUS = 0
    FETCH c INTO @a, @b
FETCH ABSOLUTE 3 FROM GLOBAL c
CLOSE c
DEALLOCATE c`)
	if len(stmts) != 6 {
		t.Fatalf("want 6 statements, got %d", len(stmts))
	}
	want(t, as[*ast.OpenStmt](t, stmts[0]).Cursor, "c")
	f := as[*ast.FetchStmt](t, stmts[1])
	if f.Direction != "NEXT" || f.Cursor != "c" || len(f.Into) != 2 {
		t.Errorf("FETCH NEXT: %+v", f)
	}
	w := as[*ast.WhileStmt](t, stmts[2])
	if wf := as[*ast.FetchStmt](t, w.Body[0]); wf.Direction != "" || wf.Cursor != "c" {
		t.Errorf("bare FETCH: %+v", wf)
	}
	abs := as[*ast.FetchStmt](t, stmts[3])
	if abs.Direction != "ABSOLUTE" || abs.Offset == nil || abs.Cursor != "c" {
		t.Errorf("FETCH ABSOLUTE: %+v", abs)
	}
	if c := as[*ast.CloseStmt](t, stmts[4]); c.Deallocate {
		t.Errorf("CLOSE: %+v", c)
	}
	if d := as[*ast.CloseStmt](t, stmts[5]); !d.Deallocate {
		t.Errorf("DEALLOCATE: %+v", d)
	}
}

func TestBody_Transactions(t *testing.T) {
	stmts := mustBody(t, `BEGIN TRAN t1
SAVE TRANSACTION sp1
ROLLBACK TRANSACTION sp1
COMMIT WORK
ROLLBACK
BEGIN DISTRIBUTED TRANSACTION`)
	wantTx := []ast.TransactionStmt{
		{Kind: "BEGIN", Name: "t1"}, {Kind: "SAVE", Name: "sp1"}, {Kind: "ROLLBACK", Name: "sp1"},
		{Kind: "COMMIT"}, {Kind: "ROLLBACK"}, {Kind: "BEGIN", Distributed: true},
	}
	if len(stmts) != len(wantTx) {
		t.Fatalf("want %d statements, got %d", len(wantTx), len(stmts))
	}
	for i, w := range wantTx {
		ts := as[*ast.TransactionStmt](t, stmts[i])
		if ts.Kind != w.Kind || ts.Name != w.Name || ts.Distributed != w.Distributed {
			t.Errorf("stmt %d: want %+v, got %+v", i, w, ts)
		}
	}
}

func TestBody_MiscStatements(t *testing.T) {
	stmts := mustBody(t, `WAITFOR DELAY '00:00:05'
CREATE TABLE #t (id int)
DROP TABLE #t
TRUNCATE TABLE dbo.log
INSERT INTO @t (a) VALUES (1)
DELETE FROM @t
UPDATE STATISTICS dbo.t
SET @a = 1 SELECT @a`)
	if len(stmts) != 9 {
		t.Fatalf("want 9 statements, got %d", len(stmts))
	}
	if w := as[*ast.NullStmt](t, stmts[0]); !w.Warn || w.Source != "WAITFOR DELAY '00:00:05'" {
		t.Errorf("WAITFOR: %+v", w)
	}
	as[*ast.CreateTable](t, as[*ast.EmbeddedDDL](t, stmts[1]).Stmt)
	as[*ast.DropTable](t, as[*ast.EmbeddedDDL](t, stmts[2]).Stmt)
	as[*ast.TruncateTable](t, stmts[3])
	if ins := as[*ast.InsertStmt](t, stmts[4]); ins.Table.Name != "@t" {
		t.Errorf("INSERT into a table variable: %+v", ins)
	}
	as[*ast.DeleteStmt](t, stmts[5])
	as[*ast.NullStmt](t, stmts[6])
	as[*ast.AssignStmt](t, stmts[7])
	as[*ast.SelectStmt](t, stmts[8])
}

func TestBody_NoTerminators(t *testing.T) {
	// T-SQL statements need no ';': the grammar alone delimits them.
	stmts := mustBody(t, `declare @min_lvl tinyint, @max_lvl tinyint
select @min_lvl = min_lvl, @max_lvl = max_lvl from employee e, jobs j, inserted i where e.emp_id = i.emp_id
IF (@job_id = 1) and (@emp_lvl <> 10)
begin
   raiserror ('Job id 1 expects the default level of 10.',16,1)
   ROLLBACK TRANSACTION
end
ELSE
IF NOT (@emp_lvl BETWEEN @min_lvl AND @max_lvl)
begin
   raiserror ('The level for job_id:%d should be between %d and %d.', 16, 1, @job_id, @min_lvl, @max_lvl)
   ROLLBACK TRANSACTION
end`)
	if len(stmts) != 3 {
		t.Fatalf("want 3 statements, got %d", len(stmts))
	}
	as[*ast.DeclareStmt](t, stmts[0])
	as[*ast.SelectInto](t, stmts[1])
	ifs := as[*ast.IfStmt](t, stmts[2])
	if len(ifs.Branches) != 2 {
		t.Errorf("IF … ELSE IF: %+v", ifs)
	}
}
