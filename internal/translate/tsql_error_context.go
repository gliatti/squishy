package translate

import (
	pgast "gitlab.com/dalibo/squishy/internal/dialects/postgres"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// tsql_error_context.go — ERROR_MESSAGE() / ERROR_NUMBER() / … read by a
// routine its caller's CATCH block calls.
//
// T-SQL's error functions return the error of the innermost CATCH block
// in scope, including from a procedure called inside that block:
// AdventureWorks' uspLogError / uspPrintError log and print "the error
// that made the caller jump to its CATCH". A PL/pgSQL routine sees
// SQLERRM / SQLSTATE only in the EXCEPTION handler that caught the
// error, so the translation carries the error explicitly through the
// transaction-local setting the tsql shim keeps (tsql_shim/
// 80_error_context.sql):
//
//   - a translated CATCH block that calls a procedure starts with
//     `_error_context := tsql.error_enter(SQLSTATE, SQLERRM)` (saving the
//     context it replaces) and restores it with tsql.error_leave when it
//     ends or RETURNs;
//   - outside a CATCH block, ERROR_MESSAGE() is tsql.error_field('message'),
//     ERROR_NUMBER() tsql.error_number_of(tsql.error_field('state')), and
//     ERROR_SEVERITY() / ERROR_STATE() read 16 / 1 while an error is in
//     scope — NULL otherwise, as in SQL Server.
//
// PostgreSQL code calling such a routine from its own EXCEPTION handler
// must call tsql.error_enter(SQLSTATE, SQLERRM) first (reported in the
// routine's note).

// tsqlErrorNumberNote documents the ERROR_NUMBER() mapping.
const tsqlErrorNumberNote = "ERROR_NUMBER() maps the PostgreSQL SQLSTATE to the SQL Server error number of the same class (tsql.error_number_of: 2627 unique key, 547 foreign key / check, 515 NOT NULL, 8134 division by zero, 245 conversion, 1205 deadlock, …), 50000 for any other error"

// use records a shim function the rendered body calls (the expression
// rewriters record the calls they leave on the AST themselves).
func (b *tsqlBody) use(fn string) {
	if b.ctx.Run != nil {
		b.ctx.Run.Use(fn)
	}
}

// errorContextFunc translates an error function read outside any CATCH
// block of the routine.
func (b *tsqlBody) errorContextFunc(name string, p ast.Position) ast.Expr {
	b.note("ERROR_MESSAGE() / ERROR_NUMBER() / … outside a CATCH block read the error of the CATCH block that called this routine, published by the translated CATCH blocks (tsql.error_enter / tsql.error_field); PostgreSQL code calling it from its own EXCEPTION handler must call tsql.error_enter(SQLSTATE, SQLERRM) first. ERROR_LINE() and ERROR_PROCEDURE() read NULL")
	b.use("tsql.error_field")
	field := func(f string) ast.Expr {
		return &ast.FuncCall{Name: "tsql.error_field", Args: []ast.Expr{ast.BuildStringLit(f)}, P: p}
	}
	inScope := func(then ast.Expr) ast.Expr {
		return &ast.CaseExpr{Whens: []ast.CaseExprWhen{{
			Match: &ast.BinaryExpr{Op: "IS NOT", Lhs: field("state"), Rhs: &ast.Literal{Kind: "null", Text: "NULL"}, P: p},
			Then:  then,
		}}, P: p}
	}
	switch name {
	case "ERROR_MESSAGE":
		return field("message")
	case "ERROR_NUMBER":
		b.note(tsqlErrorNumberNote)
		b.use("tsql.error_number_of")
		return &ast.FuncCall{Name: "tsql.error_number_of", Args: []ast.Expr{field("state")}, P: p}
	case "ERROR_SEVERITY":
		return inScope(&ast.Literal{Kind: "number", Text: "16", P: p})
	case "ERROR_STATE":
		return inScope(&ast.Literal{Kind: "number", Text: "1", P: p})
	case "ERROR_LINE":
		return tsqlTypedNull("integer", p)
	}
	return tsqlTypedNull("text", p)
}

// catchHandler renders the body of a CATCH block. When it calls a
// procedure (which may read the error with ERROR_*()), the block
// publishes its error first and restores the previous context at its
// end.
func (b *tsqlBody) catchHandler(body []ast.PLStmt) []pgast.PLStmt {
	if !tsqlCallsProcedure(body) {
		return b.stmts(body)
	}
	saved := b.addLocal("_error_context", "text", "")
	b.use("tsql.error_enter")
	b.errCtx = append(b.errCtx, saved)
	inner := b.stmts(body)
	b.errCtx = b.errCtx[:len(b.errCtx)-1]
	out := []pgast.PLStmt{&pgast.PLAssign{Target: saved.target(), Expr: "tsql.error_enter(SQLSTATE, SQLERRM)"}}
	out = append(out, inner...)
	if !tsqlEndsFlow(inner) {
		out = append(out, &pgast.PLPerform{Expr: "tsql.error_leave(" + saved.target() + ")"})
	}
	return out
}

// leaveErrorContext restores, before a RETURN inside CATCH blocks, the
// error context the outermost of them replaced.
func (b *tsqlBody) leaveErrorContext() []pgast.PLStmt {
	if len(b.errCtx) == 0 {
		return nil
	}
	return []pgast.PLStmt{&pgast.PLPerform{Expr: "tsql.error_leave(" + b.errCtx[0].target() + ")"}}
}

// tsqlCallsProcedure reports whether stmts (nested ones included) call a
// procedure.
func tsqlCallsProcedure(stmts []ast.PLStmt) bool {
	found := false
	tsqlEachStmt(stmts, func(s ast.PLStmt) {
		if _, ok := s.(*ast.CallStmt); ok {
			found = true
		}
	})
	return found
}

// tsqlEndsFlow reports whether the last rendered statement leaves the
// block (RETURN / RAISE EXCEPTION), after which a statement is
// unreachable.
func tsqlEndsFlow(in []pgast.PLStmt) bool {
	switch x := tsqlLastPL(in).(type) {
	case *pgast.PLReturn:
		return true
	case *pgast.PLRaise:
		return x.Level == "" || x.Level == "EXCEPTION"
	}
	return false
}
