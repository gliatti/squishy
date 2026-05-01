package postgres

import (
	"fmt"
	"strings"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// pl_writer.go — PG-side AST → PL/pgSQL renderer for the dialect-
// neutral *ast.PLStmt nodes.
//
// Phase 2.3 closes the writer trio (expr / stmt / pl). Together they
// give the Phase 3 AST visitors a complete output path that does not
// route through the legacy text pipeline:
//
//   Oracle source
//     ↓ ParseRoutineBody (Phase 1 typed nodes)
//   ast.PLStmt
//     ↓ ast.Rewrite + Phase 3 visitors
//   ast.PLStmt' (PG-shape)
//     ↓ WritePLStmt
//   PL/pgSQL text
//
// WritePLStmt currently covers the common shapes (Block, AssignStmt,
// IfStmt, CaseStmt, WhileStmt, LoopStmt, NumericForStmt, LeaveStmt,
// IterateStmt, ReturnStmt, NullStmt, ExecuteImmediateStmt,
// ExceptionBlock). Less-used constructs (CursorForStmt, ForallStmt,
// FetchStmt) currently fall through to "" — the translator's existing
// pgast → text path remains the authoritative renderer for those
// until Phase 3 visitors target them.

// WritePLStmt renders an *ast.PLStmt as PG PL/pgSQL text. The output
// includes the trailing semicolon and a newline so callers can
// concatenate fragments cleanly.
func WritePLStmt(s ast.PLStmt) string {
	if s == nil {
		return ""
	}
	var b strings.Builder
	writePL(&b, s, 0)
	return b.String()
}

// WriteBlock is a convenience over WritePLStmt for the most common
// caller shape — an entire routine body wrapped in a Block.
func WriteBlock(blk *ast.Block) string {
	if blk == nil {
		return ""
	}
	var b strings.Builder
	writeBlockNode(&b, blk, 0)
	return b.String()
}

func plIndent(n int) string { return strings.Repeat("  ", n) }

func writePL(b *strings.Builder, s ast.PLStmt, depth int) {
	pfx := plIndent(depth)
	switch x := s.(type) {

	case *ast.Block:
		writeBlockNode(b, x, depth)

	case *ast.AssignStmt:
		fmt.Fprintf(b, "%s%s := %s;\n", pfx, x.Target, WriteExpr(x.Expr))

	case *ast.IfStmt:
		for i, br := range x.Branches {
			kw := "IF"
			if i > 0 {
				kw = "ELSIF"
			}
			fmt.Fprintf(b, "%s%s %s THEN\n", pfx, kw, WriteExpr(br.Cond))
			for _, bs := range br.Body {
				writePL(b, bs, depth+1)
			}
		}
		if len(x.Else) > 0 {
			fmt.Fprintf(b, "%sELSE\n", pfx)
			for _, bs := range x.Else {
				writePL(b, bs, depth+1)
			}
		}
		fmt.Fprintf(b, "%sEND IF;\n", pfx)

	case *ast.CaseStmt:
		if x.Expr == nil {
			fmt.Fprintf(b, "%sCASE\n", pfx)
		} else {
			fmt.Fprintf(b, "%sCASE %s\n", pfx, WriteExpr(x.Expr))
		}
		for _, w := range x.When {
			fmt.Fprintf(b, "%s  WHEN %s THEN\n", pfx, WriteExpr(w.Match))
			for _, bs := range w.Body {
				writePL(b, bs, depth+2)
			}
		}
		if len(x.Else) > 0 {
			fmt.Fprintf(b, "%s  ELSE\n", pfx)
			for _, bs := range x.Else {
				writePL(b, bs, depth+2)
			}
		}
		fmt.Fprintf(b, "%sEND CASE;\n", pfx)

	case *ast.WhileStmt:
		if x.Label != "" {
			fmt.Fprintf(b, "%s<<%s>>\n", pfx, x.Label)
		}
		fmt.Fprintf(b, "%sWHILE %s LOOP\n", pfx, WriteExpr(x.Cond))
		for _, bs := range x.Body {
			writePL(b, bs, depth+1)
		}
		fmt.Fprintf(b, "%sEND LOOP;\n", pfx)

	case *ast.LoopStmt:
		if x.Label != "" {
			fmt.Fprintf(b, "%s<<%s>>\n", pfx, x.Label)
		}
		fmt.Fprintf(b, "%sLOOP\n", pfx)
		for _, bs := range x.Body {
			writePL(b, bs, depth+1)
		}
		fmt.Fprintf(b, "%sEND LOOP;\n", pfx)

	case *ast.NumericForStmt:
		if x.Label != "" {
			fmt.Fprintf(b, "%s<<%s>>\n", pfx, x.Label)
		}
		reverse := ""
		if x.Reverse {
			reverse = "REVERSE "
		}
		fmt.Fprintf(b, "%sFOR %s IN %s%s..%s LOOP\n",
			pfx, strings.ToLower(x.Var), reverse, WriteExpr(x.Low), WriteExpr(x.High))
		for _, bs := range x.Body {
			writePL(b, bs, depth+1)
		}
		fmt.Fprintf(b, "%sEND LOOP;\n", pfx)

	case *ast.LeaveStmt:
		switch {
		case x.WhenCond != nil && x.Label != "":
			fmt.Fprintf(b, "%sEXIT %s WHEN %s;\n", pfx, x.Label, WriteExpr(x.WhenCond))
		case x.WhenCond != nil:
			fmt.Fprintf(b, "%sEXIT WHEN %s;\n", pfx, WriteExpr(x.WhenCond))
		case x.Label != "":
			fmt.Fprintf(b, "%sEXIT %s;\n", pfx, x.Label)
		default:
			fmt.Fprintf(b, "%sEXIT;\n", pfx)
		}

	case *ast.IterateStmt:
		switch {
		case x.WhenCond != nil && x.Label != "":
			fmt.Fprintf(b, "%sCONTINUE %s WHEN %s;\n", pfx, x.Label, WriteExpr(x.WhenCond))
		case x.WhenCond != nil:
			fmt.Fprintf(b, "%sCONTINUE WHEN %s;\n", pfx, WriteExpr(x.WhenCond))
		case x.Label != "":
			fmt.Fprintf(b, "%sCONTINUE %s;\n", pfx, x.Label)
		default:
			fmt.Fprintf(b, "%sCONTINUE;\n", pfx)
		}

	case *ast.ReturnStmt:
		if x.Expr == nil {
			fmt.Fprintf(b, "%sRETURN;\n", pfx)
		} else {
			fmt.Fprintf(b, "%sRETURN %s;\n", pfx, WriteExpr(x.Expr))
		}

	case *ast.NullStmt:
		fmt.Fprintf(b, "%sNULL;\n", pfx)

	case *ast.ExecuteImmediateStmt:
		// PG: EXECUTE <sql expr> [INTO target [, target …]] [USING e [, e …]];
		fmt.Fprintf(b, "%sEXECUTE %s", pfx, WriteExpr(x.SQL))
		if len(x.Into) > 0 {
			b.WriteString(" INTO ")
			b.WriteString(strings.Join(x.Into, ", "))
		}
		if len(x.Using) > 0 {
			b.WriteString(" USING ")
			args := make([]string, len(x.Using))
			for i, e := range x.Using {
				args[i] = WriteExpr(e)
			}
			b.WriteString(strings.Join(args, ", "))
		}
		b.WriteString(";\n")

	case *ast.RaiseStmt:
		if x.Name == "" {
			fmt.Fprintf(b, "%sRAISE;\n", pfx)
		} else {
			fmt.Fprintf(b, "%sRAISE %s;\n", pfx, x.Name)
		}

	case *ast.RawSQL:
		// Legacy escape hatch — the translator's text pipeline still
		// emits these for constructs the AST doesn't model yet
		// (COMMIT/ROLLBACK/SAVEPOINT, oracle merge, …).
		txt := strings.TrimSuffix(strings.TrimSpace(x.Text), ";")
		if txt != "" {
			fmt.Fprintf(b, "%s%s;\n", pfx, txt)
		}

	default:
		// Unhandled types (CursorForStmt, ForallStmt, FetchStmt, …)
		// fall through to a parseable comment so the routine boots
		// even when a Phase 3 visitor hasn't covered the construct
		// yet. The translator's existing pgast emit owns these for
		// now — see plsql_xlate.go.
		fmt.Fprintf(b, "%s/* unsupported AST PLStmt: %T */\n", pfx, s)
	}
}

func writeBlockNode(b *strings.Builder, blk *ast.Block, depth int) {
	pfx := plIndent(depth)
	if blk.Label != "" {
		fmt.Fprintf(b, "%s<<%s>>\n", pfx, blk.Label)
	}
	if len(blk.Decls) > 0 {
		fmt.Fprintf(b, "%sDECLARE\n", pfx)
		for _, d := range blk.Decls {
			fmt.Fprintf(b, "%s  %s\n", pfx, writeDeclLine(d))
		}
	}
	fmt.Fprintf(b, "%sBEGIN\n", pfx)
	for _, s := range blk.Stmts {
		writePL(b, s, depth+1)
	}
	if blk.Except != nil && len(blk.Except.Handlers) > 0 {
		fmt.Fprintf(b, "%sEXCEPTION\n", pfx)
		for _, h := range blk.Except.Handlers {
			fmt.Fprintf(b, "%s  WHEN %s THEN\n", pfx, strings.Join(h.Names, " OR "))
			for _, s := range h.Body {
				writePL(b, s, depth+2)
			}
		}
	}
	fmt.Fprintf(b, "%sEND;\n", pfx)
}

// writeDeclLine emits a single DECLARE entry. The translator owns the
// type-mapping decision (Oracle NUMBER → PG numeric, etc.); the writer
// best-efforts the AST shape for cases the translator has already
// resolved. Unsupported decl kinds emit a placeholder so the outer
// block remains parseable.
func writeDeclLine(d ast.PLDecl) string {
	switch x := d.(type) {
	case *ast.DeclareVar:
		typ := "text"
		if udt, ok := x.Type.(*ast.UserDefinedType); ok && udt.Name != "" {
			typ = udt.Name
		}
		out := fmt.Sprintf("%s %s", x.Name, typ)
		if x.Default != nil {
			out += " := " + WriteExpr(x.Default)
		}
		return out + ";"
	}
	return "/* unsupported decl */"
}
