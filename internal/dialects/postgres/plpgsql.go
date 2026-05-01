package postgres

import (
	"fmt"
	"strings"
)

// PL/pgSQL AST — the procedural layer emitted inside CreateFunction /
// CreateProcedure / CreateTrigger body strings. The translator builds a
// tree of these nodes from the MySQL PL/SQL AST and calls WritePLpgSQL to
// render PL/pgSQL text that plugs into CreateFunction.Body / CreateProcedure.Body.
//
// Grammar reference: PostgreSQL 17 "SQL Procedural Language" section in the
// canonical docs (plpgsql.sgml), which aligns with reference/PostgreSQLParser.g4
// productions `createfunctionstmt` / `dostmt` / `plpgsql_statement`.

type PLStmt interface {
	plStmt()
}

type PLBlock struct {
	Label     string
	Decls     []PLBlockDecl
	// Exception is a pre-rendered EXCEPTION section body (no wrapping
	// `EXCEPTION` keyword — just the `WHEN … THEN …;` handlers). When
	// non-empty, the writer emits `EXCEPTION <handlers>` between the block
	// body and the closing `END;`.
	Exception string
	Body  []PLStmt
}

func (*PLBlock) plStmt() {}

type PLBlockDecl struct {
	// `<name> <type> [DEFAULT <expr>];` or `<name> CURSOR FOR <select>;`
	Text string
}

type PLAssign struct {
	Target string
	Expr   string
}

func (*PLAssign) plStmt() {}

type PLIf struct {
	Branches []PLIfBranch
	Else     []PLStmt
}

type PLIfBranch struct {
	Cond string
	Body []PLStmt
}

func (*PLIf) plStmt() {}

type PLCase struct {
	Expr string // empty for searched CASE
	When []PLCaseWhen
	Else []PLStmt
}

type PLCaseWhen struct {
	Match string
	Body  []PLStmt
}

func (*PLCase) plStmt() {}

type PLWhile struct {
	Label string
	Cond  string
	Body  []PLStmt
}

func (*PLWhile) plStmt() {}

type PLLoop struct {
	Label string
	Body  []PLStmt
}

func (*PLLoop) plStmt() {}

// PLForQuery renders `FOR <vars> IN <select-query> LOOP ... END LOOP;` —
// the canonical PG replacement for MySQL cursor + FETCH + HANDLER NOT FOUND.
type PLForQuery struct {
	Label string
	Vars  []string // single element = shortcut form; multi = target columns
	Query string
	Body  []PLStmt
}

func (*PLForQuery) plStmt() {}

// PLForRange renders `FOR <var> IN [REVERSE] <low>..<high> LOOP ... END
// LOOP;` — the integer-range FOR loop. Maps 1:1 onto Oracle's
// `FOR i IN [REVERSE] lo..hi LOOP …` so re-emission is verbatim
// (with bounds expressions individually translated).
type PLForRange struct {
	Label   string
	Var     string
	Reverse bool
	Low     string
	High    string
	Body    []PLStmt
}

func (*PLForRange) plStmt() {}

type PLExit struct {
	Label string
}

func (*PLExit) plStmt() {}

type PLContinue struct {
	Label string
}

func (*PLContinue) plStmt() {}

type PLExitWhen struct {
	Label string
	Cond  string
}

func (*PLExitWhen) plStmt() {}

// PLContinueWhen — CONTINUE [label] WHEN <cond>;
type PLContinueWhen struct {
	Label string
	Cond  string
}

func (*PLContinueWhen) plStmt() {}

type PLReturn struct {
	Expr string // empty for procedure (no RETURN value)
}

func (*PLReturn) plStmt() {}

type PLCall struct {
	Schema string
	Name   string
	Args   []string // pre-rendered PG expressions
}

func (*PLCall) plStmt() {}

type PLPerform struct {
	Expr string
}

func (*PLPerform) plStmt() {}

type PLRaise struct {
	Level   string // "EXCEPTION" | "WARNING" | "NOTICE"
	Msg     string
	ErrCode string // SQLSTATE or condition name
}

func (*PLRaise) plStmt() {}

// PLRawSQL is pass-through SQL (INSERT/UPDATE/DELETE/SELECT) already
// rewritten by the body rewriter. The writer appends a trailing `;`.
type PLRawSQL struct {
	Text string
}

func (*PLRawSQL) plStmt() {}

// PLCursorOp models OPEN/FETCH/CLOSE. In PG, cursor ops are available but
// FOR loops are idiomatic — the translator prefers FOR where possible.
type PLCursorOp struct {
	Kind   string // "OPEN" | "FETCH" | "CLOSE"
	Cursor string
	Into   []string
	// Args is the parenthesised actuals for `OPEN cur(a1, a2)` against
	// a parameterised cursor declared in DECLARE. Empty for parameter-
	// less cursors. Re-emitted verbatim by the writer as part of the
	// OPEN statement.
	Args string
	// ForQuery, IsDynamic, UsingArgs cover Oracle's `OPEN cur FOR <query>`
	// shape. Empty ForQuery means a plain OPEN against a DECLARE-section
	// cursor. Dynamic=true emits `OPEN … FOR EXECUTE`, otherwise
	// `OPEN … FOR <SELECT>`. UsingArgs map to PG's `USING <args>`.
	ForQuery  string
	IsDynamic bool
	UsingArgs []string
}

func (*PLCursorOp) plStmt() {}

// WritePLpgSQL renders a PLBlock as PL/pgSQL text suitable for the body of
// CreateFunction / CreateProcedure (i.e. what goes between $body$ ... $body$).
func WritePLpgSQL(b *PLBlock) string {
	var buf strings.Builder
	writePLBlock(&buf, b, 0)
	return buf.String()
}

func indent(n int) string { return strings.Repeat("  ", n) }

func writePLBlock(b *strings.Builder, blk *PLBlock, depth int) {
	pfx := indent(depth)
	if blk.Label != "" {
		fmt.Fprintf(b, "%s<<%s>>\n", pfx, blk.Label)
	}
	if len(blk.Decls) > 0 {
		fmt.Fprintf(b, "%sDECLARE\n", pfx)
		for _, d := range blk.Decls {
			fmt.Fprintf(b, "%s  %s\n", pfx, d.Text)
		}
	}
	fmt.Fprintf(b, "%sBEGIN\n", pfx)
	for _, s := range blk.Body {
		writePLStmt(b, s, depth+1)
	}
	if strings.TrimSpace(blk.Exception) != "" {
		fmt.Fprintf(b, "%sEXCEPTION\n%s\n", pfx, blk.Exception)
	}
	fmt.Fprintf(b, "%sEND;\n", pfx)
}

func writePLStmt(b *strings.Builder, s PLStmt, depth int) {
	pfx := indent(depth)
	switch x := s.(type) {
	case *PLBlock:
		writePLBlock(b, x, depth)
	case *PLAssign:
		fmt.Fprintf(b, "%s%s := %s;\n", pfx, x.Target, x.Expr)
	case *PLIf:
		for i, br := range x.Branches {
			kw := "IF"
			if i > 0 {
				kw = "ELSIF"
			}
			fmt.Fprintf(b, "%s%s %s THEN\n", pfx, kw, br.Cond)
			for _, bs := range br.Body {
				writePLStmt(b, bs, depth+1)
			}
		}
		if len(x.Else) > 0 {
			fmt.Fprintf(b, "%sELSE\n", pfx)
			for _, bs := range x.Else {
				writePLStmt(b, bs, depth+1)
			}
		}
		fmt.Fprintf(b, "%sEND IF;\n", pfx)
	case *PLCase:
		if x.Expr == "" {
			fmt.Fprintf(b, "%sCASE\n", pfx)
		} else {
			fmt.Fprintf(b, "%sCASE %s\n", pfx, x.Expr)
		}
		for _, w := range x.When {
			fmt.Fprintf(b, "%s  WHEN %s THEN\n", pfx, w.Match)
			for _, bs := range w.Body {
				writePLStmt(b, bs, depth+2)
			}
		}
		if len(x.Else) > 0 {
			fmt.Fprintf(b, "%s  ELSE\n", pfx)
			for _, bs := range x.Else {
				writePLStmt(b, bs, depth+2)
			}
		}
		fmt.Fprintf(b, "%sEND CASE;\n", pfx)
	case *PLWhile:
		if x.Label != "" {
			fmt.Fprintf(b, "%s<<%s>>\n", pfx, x.Label)
		}
		fmt.Fprintf(b, "%sWHILE %s LOOP\n", pfx, x.Cond)
		for _, bs := range x.Body {
			writePLStmt(b, bs, depth+1)
		}
		fmt.Fprintf(b, "%sEND LOOP;\n", pfx)
	case *PLLoop:
		if x.Label != "" {
			fmt.Fprintf(b, "%s<<%s>>\n", pfx, x.Label)
		}
		fmt.Fprintf(b, "%sLOOP\n", pfx)
		for _, bs := range x.Body {
			writePLStmt(b, bs, depth+1)
		}
		fmt.Fprintf(b, "%sEND LOOP;\n", pfx)
	case *PLForQuery:
		if x.Label != "" {
			fmt.Fprintf(b, "%s<<%s>>\n", pfx, x.Label)
		}
		// PG requires the loop target to be a pre-declared record/row variable
		// or a list of scalar variables. When the target is a single
		// identifier (the Oracle `FOR rec IN (SELECT …)` idiom), wrap the
		// FOR in a local DECLARE block so `rec` is typed as RECORD without
		// requiring the caller to thread a DECLARE entry into the outer block.
		vars := strings.Join(x.Vars, ", ")
		lower := strings.ToLower(vars)
		if len(x.Vars) == 1 {
			fmt.Fprintf(b, "%sDECLARE\n%s  %s RECORD;\n%sBEGIN\n", pfx, pfx, lower, pfx)
			fmt.Fprintf(b, "%s  FOR %s IN %s LOOP\n", pfx, lower, x.Query)
			for _, bs := range x.Body {
				writePLStmt(b, bs, depth+2)
			}
			fmt.Fprintf(b, "%s  END LOOP;\n%sEND;\n", pfx, pfx)
		} else {
			fmt.Fprintf(b, "%sFOR %s IN %s LOOP\n", pfx, lower, x.Query)
			for _, bs := range x.Body {
				writePLStmt(b, bs, depth+1)
			}
			fmt.Fprintf(b, "%sEND LOOP;\n", pfx)
		}
	case *PLForRange:
		if x.Label != "" {
			fmt.Fprintf(b, "%s<<%s>>\n", pfx, x.Label)
		}
		reverse := ""
		if x.Reverse {
			reverse = "REVERSE "
		}
		fmt.Fprintf(b, "%sFOR %s IN %s%s..%s LOOP\n",
			pfx, strings.ToLower(x.Var), reverse, x.Low, x.High)
		for _, bs := range x.Body {
			writePLStmt(b, bs, depth+1)
		}
		fmt.Fprintf(b, "%sEND LOOP;\n", pfx)
	case *PLExit:
		if x.Label != "" {
			fmt.Fprintf(b, "%sEXIT %s;\n", pfx, x.Label)
		} else {
			fmt.Fprintf(b, "%sEXIT;\n", pfx)
		}
	case *PLContinue:
		if x.Label != "" {
			fmt.Fprintf(b, "%sCONTINUE %s;\n", pfx, x.Label)
		} else {
			fmt.Fprintf(b, "%sCONTINUE;\n", pfx)
		}
	case *PLExitWhen:
		if x.Label != "" {
			fmt.Fprintf(b, "%sEXIT %s WHEN %s;\n", pfx, x.Label, x.Cond)
		} else {
			fmt.Fprintf(b, "%sEXIT WHEN %s;\n", pfx, x.Cond)
		}
	case *PLContinueWhen:
		if x.Label != "" {
			fmt.Fprintf(b, "%sCONTINUE %s WHEN %s;\n", pfx, x.Label, x.Cond)
		} else {
			fmt.Fprintf(b, "%sCONTINUE WHEN %s;\n", pfx, x.Cond)
		}
	case *PLReturn:
		if x.Expr == "" {
			fmt.Fprintf(b, "%sRETURN;\n", pfx)
		} else {
			fmt.Fprintf(b, "%sRETURN %s;\n", pfx, x.Expr)
		}
	case *PLCall:
		// PG distinguishes procedures (called via CALL) from functions
		// (PERFORM in PL/pgSQL). Oracle PL/SQL doesn't surface the
		// distinction at the call site — `pkg.do_thing();` works for
		// both — so we have to pick. orafce ships its compatibility
		// shims (utl_file.*, dbms_lob.*, dbms_random.*, dbms_utility.*)
		// as FUNCTIONS, which PG plpgsql refuses to CALL. Emit PERFORM
		// for those schemas, plus the lowercased-Oracle schemas that
		// the squishy translator routes to orafce. Anything else is
		// assumed to be a stored procedure (via squishy's own
		// translation of an Oracle PROCEDURE) and stays on CALL.
		schemaLower := strings.ToLower(x.Schema)
		nameLower := strings.ToLower(x.Name)
		isFunctionLike := false
		switch schemaLower {
		case "utl_file", "dbms_lob", "dbms_random", "dbms_utility":
			isFunctionLike = true
		}
		// Explicit per-name overrides: even outside the known schemas,
		// the squishy textual rewrites emit PERFORM-friendly inline calls
		// (e.g. set_config from package-var assignments). Catch the
		// common idioms here so we don't ship a CALL for a function name
		// the rewrites just produced.
		if !isFunctionLike && schemaLower == "" {
			switch nameLower {
			case "set_config", "current_setting", "pg_file_write":
				isFunctionLike = true
			}
		}
		var name string
		switch {
		case isFunctionLike && x.Schema != "":
			// orafce's schema + the function are stored in lowercase;
			// quoting would make the identifier case-sensitive and
			// PG would fail to find e.g. `"UTL_FILE"."PUT_LINE"`.
			name = strings.ToLower(x.Schema) + "." + strings.ToLower(x.Name)
		case isFunctionLike:
			name = strings.ToLower(x.Name)
		case x.Schema != "":
			name = qIdent(x.Schema) + "." + qIdent(x.Name)
		default:
			name = qIdent(x.Name)
		}
		if isFunctionLike {
			fmt.Fprintf(b, "%sPERFORM %s(%s);\n", pfx, name, strings.Join(x.Args, ", "))
		} else {
			fmt.Fprintf(b, "%sCALL %s(%s);\n", pfx, name, strings.Join(x.Args, ", "))
		}
	case *PLPerform:
		fmt.Fprintf(b, "%sPERFORM %s;\n", pfx, x.Expr)
	case *PLRaise:
		lvl := x.Level
		if lvl == "" {
			lvl = "EXCEPTION"
		}
		if x.ErrCode != "" {
			fmt.Fprintf(b, "%sRAISE %s %s USING ERRCODE = %s;\n",
				pfx, lvl, sqlLit(x.Msg), sqlLit(x.ErrCode))
		} else {
			fmt.Fprintf(b, "%sRAISE %s %s;\n", pfx, lvl, sqlLit(x.Msg))
		}
	case *PLCursorOp:
		// Cursor / local variable names must match the DECLARE section, which
		// itself is rendered verbatim (unquoted). Quoting the references here
		// would make the identifiers case-sensitive ("CUR_ORDERS" vs the
		// PG-folded cur_orders) and raise "not a known variable" at runtime.
		// Emit them as PG unquoted identifiers — they'll be folded to lower-
		// case consistently with the DECLARE side.
		cursor := strings.ToLower(x.Cursor)
		switch x.Kind {
		case "OPEN":
			if x.ForQuery != "" {
				body := strings.TrimSuffix(strings.TrimSpace(x.ForQuery), ";")
				keyword := "FOR"
				if x.IsDynamic {
					keyword = "FOR EXECUTE"
				}
				if len(x.UsingArgs) > 0 {
					fmt.Fprintf(b, "%sOPEN %s %s %s USING %s;\n",
						pfx, cursor, keyword, body, strings.Join(x.UsingArgs, ", "))
				} else {
					fmt.Fprintf(b, "%sOPEN %s %s %s;\n", pfx, cursor, keyword, body)
				}
			} else if strings.TrimSpace(x.Args) != "" {
				// Parameterised cursor open: PG matches the cursor's
				// declared parameter list, so we re-emit `OPEN cur(a1,
				// a2)` verbatim. Without this clause an Oracle-side
				// `OPEN cur(args);` is rendered as `OPEN cur;` and PG
				// rejects with `cursor "cur" has arguments`.
				fmt.Fprintf(b, "%sOPEN %s(%s);\n", pfx, cursor, strings.TrimSpace(x.Args))
			} else {
				fmt.Fprintf(b, "%sOPEN %s;\n", pfx, cursor)
			}
		case "FETCH":
			targets := make([]string, len(x.Into))
			for i, n := range x.Into {
				targets[i] = strings.ToLower(n)
			}
			fmt.Fprintf(b, "%sFETCH %s INTO %s;\n", pfx, cursor, strings.Join(targets, ", "))
		case "CLOSE":
			fmt.Fprintf(b, "%sCLOSE %s;\n", pfx, cursor)
		}
	case *PLRawSQL:
		txt := strings.TrimSpace(strings.TrimSuffix(x.Text, ";"))
		fmt.Fprintf(b, "%s%s;\n", pfx, txt)
	}
}
