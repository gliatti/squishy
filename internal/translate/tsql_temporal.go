package translate

// tsql_temporal.go — SQL Server system-versioned (temporal) tables →
// PostgreSQL.
//
// A T-SQL table `WITH (SYSTEM_VERSIONING = ON (HISTORY_TABLE = s.h))`
// with `PERIOD FOR SYSTEM_TIME (start, end)` over two `datetime2
// GENERATED ALWAYS AS ROW START | END` columns is emulated:
//
//   - the current table keeps both period columns (timestamp(n) NOT
//     NULL) and the copy transfers their source values;
//   - the history table, which SQL Server keeps as a real table holding
//     the closed versions, becomes a PG table of the same columns (plain
//     storage: no key, identity, default, generation or check, like a
//     SQL Server history table) in the history table's own schema, and
//     the planner copies the source history table into it (Plan
//     TSQLTemporal.CopyHistory);
//   - post-copy, a BEFORE INSERT OR UPDATE row trigger stamps
//     start = the transaction start time in UTC and end = the
//     open-ended maximum of the column type (9999-12-31 23:59:59.999…),
//     and an AFTER UPDATE OR DELETE row trigger archives OLD into the
//     history table closed at that time. This is SQL Server's rule
//     (checked against its documentation and behaviour): both stamps
//     are the begin time of the transaction (SYSUTCDATETIME at BEGIN
//     TRAN), every UPDATE writes a version even when it changes no
//     value, a row changed twice in one transaction leaves a zero-length
//     version, and a transaction that began before the version it
//     modifies fails (error 13535).
//
// The trigger bodies are typed PL nodes rendered by the PG writer;
// nothing here inspects SQL text.

import (
	"strconv"
	"strings"

	pgast "gitlab.com/dalibo/squishy/internal/dialects/postgres"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// tsqlTemporalEmulation returns the emulation descriptor of the T-SQL
// table s, or nil when it is not a
// system-versioned table the emulation covers: SYSTEM_VERSIONING ON, a
// SYSTEM_TIME period over a ROW START / ROW END column pair, and a named
// history table.
func (t *translator) tsqlTemporalEmulation(s *ast.CreateTable, srcSchema string) *PGTSQLTemporal {
	if !s.SystemVersioned || s.HistoryTable == nil || s.HistoryTable.Name == "" {
		return nil
	}
	var start, end string
	for _, pd := range s.Periods {
		if strings.EqualFold(pd.Name, "SYSTEM_TIME") {
			start, end = pd.StartCol, pd.EndCol
		}
	}
	var startCol, endCol *ast.ColumnDef
	for _, c := range s.Columns {
		if !c.SystemVersioning {
			continue
		}
		switch {
		case strings.EqualFold(c.Name, start) && strings.EqualFold(c.PeriodRow, "START"):
			startCol = c
		case strings.EqualFold(c.Name, end) && strings.EqualFold(c.PeriodRow, "END"):
			endCol = c
		}
	}
	if startCol == nil || endCol == nil {
		return nil
	}
	digits, ok := tsqlDatetime2Digits(endCol.Type)
	if !ok {
		return nil
	}
	if _, ok := tsqlDatetime2Digits(startCol.Type); !ok {
		return nil
	}
	histSrcSchema := s.HistoryTable.Schema
	if histSrcSchema == "" {
		histSrcSchema = srcSchema
	}
	hs, hn := t.tsql.run.ResolveRelation(histSrcSchema, s.HistoryTable.Name, srcSchema)
	t.useSchema(histSrcSchema)
	return &PGTSQLTemporal{
		RowStart:            normalizeTSQLIdent(startCol.Name),
		RowEnd:              normalizeTSQLIdent(endCol.Name),
		RowEndMax:           tsqlPeriodEndMax(digits),
		HistorySchema:       hs,
		HistoryTable:        hn,
		HistorySourceSchema: histSrcSchema,
		HistorySourceTable:  s.HistoryTable.Name,
		CopyHistory:         true,
	}
}

// tsqlDatetime2Digits returns the fractional-second digits a datetime2
// period column keeps on PostgreSQL (datetime2(n), n defaulting to 7,
// capped at PG's 6), false for any other type (SQL Server only accepts
// datetime2 period columns).
func tsqlDatetime2Digits(dt ast.DataType) (int, bool) {
	x, ok := dt.(*ast.TSQLType)
	if !ok || !strings.EqualFold(x.Name, "DATETIME2") {
		return 0, false
	}
	n := 7
	if x.HasPrecision {
		n = x.Precision
	}
	if n > 6 {
		n = 6
	}
	return n, true
}

// tsqlPeriodEndMax is the ROW END value of current rows as PG stores it:
// datetime2's maximum, 9999-12-31 23:59:59.9999999, truncated to the
// column's fractional digits like the copied values are.
func tsqlPeriodEndMax(digits int) string {
	v := "9999-12-31 23:59:59"
	if digits > 0 {
		v += "." + strings.Repeat("9", digits)
	}
	return v
}

// emitTSQLTemporal materialises the emulation of every SQL Server
// system-versioned table: its history table (appended to Plan.Tables
// unless the script creates it itself) and the post-copy versioning
// triggers.
func (t *translator) emitTSQLTemporal() {
	taken := t.sysverTakenNames()
	n := len(t.res.Plan.Tables)
	for i := 0; i < n; i++ {
		cur := &t.res.Plan.Tables[i]
		tv := cur.TSQLTemporal
		if tv == nil {
			continue
		}
		cols := make([]string, 0, len(cur.Columns))
		for _, c := range cur.Columns {
			cols = append(cols, c.Name)
		}
		if _, exists := t.tsql.tables[t.tableKey(tv.HistorySchema, tv.HistoryTable)]; exists {
			// A hand-written script creating the history table itself:
			// it is migrated (and copied) as a table of its own.
			tv.CopyHistory = false
		} else {
			h := PGTable{
				Schema:       tv.HistorySchema,
				Name:         tv.HistoryTable,
				SourceSchema: tv.HistorySourceSchema,
				SourceName:   tv.HistorySourceTable,
				Comment:      "Closed row versions of " + cur.Schema + "." + cur.Name + " (SQL Server system-versioning emulation).",
			}
			for _, c := range cur.Columns {
				// A SQL Server history table stores plain values: no
				// identity, default, computed column or constraint.
				h.Columns = append(h.Columns, PGColumn{Name: c.Name, Type: c.Type, NotNull: c.NotNull, Collation: c.Collation})
			}
			taken.relations[strings.ToLower(pgClipIdent(h.Name))] = true
			t.res.Plan.Tables = append(t.res.Plan.Tables, h)
			t.tsql.tables[t.tableKey(h.Schema, h.Name)] = len(t.res.Plan.Tables) - 1
			cur = &t.res.Plan.Tables[i]
		}
		trg := taken.triggersOn(cur.Name)
		names := sysverNames{
			stampFn:      claimIdent(cur.Name, "_period_stamp_fn", taken.functions),
			histFn:       claimIdent(cur.Name, "_period_history_fn", taken.functions),
			stampTrigger: claimIdent(cur.Name, "_period_stamp", trg),
			histTrigger:  claimIdent(cur.Name, "_period_history", trg),
		}
		t.res.Plan.PostActions = append(t.res.Plan.PostActions, tsqlTemporalDDL(cur.Schema, cur.Name, cols, tv, names))
		obj := cur.Schema + "." + cur.Name
		hist := tv.HistorySchema + "." + tv.HistoryTable
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: "table." + obj,
			Source: "WITH (SYSTEM_VERSIONING = ON (HISTORY_TABLE = " + tv.HistorySourceSchema + "." + tv.HistorySourceTable + ")), PERIOD FOR SYSTEM_TIME (" + tv.RowStart + ", " + tv.RowEnd + ")",
			Target: "current table + history table " + hist + " + versioning triggers",
			Reason: "PostgreSQL has no system versioning; it is emulated. The period columns are kept (NOT NULL) and copied with their source values, and the source history table is copied into " + hist + ". After the copy, a BEFORE INSERT OR UPDATE trigger stamps " + tv.RowStart + " = the transaction start time in UTC and " + tv.RowEnd + " = '" + tv.RowEndMax + "' (datetime2's maximum, as the copied current rows hold it), and an AFTER UPDATE OR DELETE trigger archives the previous version into " + hist + " closed at that time — SQL Server's rule: one version per updated row, even when no value changes, and an error when the transaction began before the version it modifies (SQL Server error 13535). Values written to the period columns are replaced by the stamps (SQL Server rejects them). `FOR SYSTEM_TIME AS OF / BETWEEN / ALL` is query syntax PostgreSQL lacks: such queries must read " + hist + " UNION ALL " + obj + " with the equivalent " + tv.RowStart + " / " + tv.RowEnd + " predicates.",
			Level:  "info",
		})
	}
}

// tsqlTemporalDDL renders the two trigger functions and triggers of the
// emulation of table schema.table (cols: every column, in order).
func tsqlTemporalDDL(schema, table string, cols []string, tv *PGTSQLTemporal, names sysverNames) string {
	txTime := func() ast.Expr {
		return &ast.FuncCall{Name: "timezone", Args: []ast.Expr{
			&ast.Literal{Kind: "string", Text: "UTC"},
			&ast.FuncCall{Name: "transaction_timestamp"},
		}}
	}
	rec := func(r, c string) ast.Expr { return &ast.Ident{Parts: []string{r, c}} }
	tgOp := func(op string) string {
		return pgast.WriteExpr(&ast.BinaryExpr{Op: "=", Lhs: &ast.Ident{Parts: []string{"TG_OP"}}, Rhs: &ast.Literal{Kind: "string", Text: op}})
	}

	stamp := &pgast.PLBlock{Body: []pgast.PLStmt{
		&pgast.PLIf{Branches: []pgast.PLIfBranch{{
			Cond: tgOp("UPDATE") + " AND " + pgast.WriteExpr(&ast.BinaryExpr{Op: ">", Lhs: rec("OLD", tv.RowStart), Rhs: txTime()}),
			Body: []pgast.PLStmt{&pgast.PLRaise{
				Level: "EXCEPTION",
				Msg:   "Data modification failed on system-versioned table " + schema + "." + table + " because transaction time was earlier than period start time for affected records (SQL Server error 13535)",
			}},
		}}},
		&pgast.PLAssign{Target: "NEW." + pgQuote(tv.RowStart), Expr: pgast.WriteExpr(txTime())},
		&pgast.PLAssign{Target: "NEW." + pgQuote(tv.RowEnd), Expr: pgast.WriteExpr(&ast.CastExpr{
			Expr: &ast.Literal{Kind: "string", Text: tv.RowEndMax},
			Type: &ast.PGType{Name: "timestamp"},
		})},
		&pgast.PLReturn{Expr: "NEW"},
	}}

	archive := func(closeAt ast.Expr) pgast.PLStmt {
		vals := make([]ast.Expr, 0, len(cols))
		for _, c := range cols {
			if c == tv.RowEnd {
				vals = append(vals, closeAt)
				continue
			}
			vals = append(vals, rec("OLD", c))
		}
		return &pgast.PLRawSQL{Text: pgast.WriteStmt(&ast.InsertStmt{
			Table:  ast.TableRef{Schema: tv.HistorySchema, Name: tv.HistoryTable},
			Cols:   cols,
			Values: [][]ast.Expr{vals},
		})}
	}
	history := &pgast.PLBlock{Body: []pgast.PLStmt{
		&pgast.PLIf{
			Branches: []pgast.PLIfBranch{{Cond: tgOp("UPDATE"), Body: []pgast.PLStmt{archive(rec("NEW", tv.RowStart))}}},
			Else:     []pgast.PLStmt{archive(txTime())},
		},
		&pgast.PLReturn{Expr: "NULL"},
	}}

	return pgast.Write([]pgast.Stmt{
		&pgast.CreateFunction{Schema: schema, Name: names.stampFn, Returns: "trigger", Body: pgast.WritePLpgSQL(stamp)},
		&pgast.CreateTrigger{
			Name: names.stampTrigger, Timing: "BEFORE", Events: []string{"INSERT", "UPDATE"},
			Schema: schema, Table: table, FnName: names.stampFn, ForEach: "ROW",
		},
		&pgast.CreateFunction{Schema: schema, Name: names.histFn, Returns: "trigger", Body: pgast.WritePLpgSQL(history)},
		&pgast.CreateTrigger{
			Name: names.histTrigger, Timing: "AFTER", Events: []string{"UPDATE", "DELETE"},
			Schema: schema, Table: table, FnName: names.histFn, ForEach: "ROW",
		},
	})
}

// tsqlTemporalHidden explains the HIDDEN period columns of an
// emulated table (PostgreSQL has no hidden columns).
func (t *translator) tsqlTemporalHidden(s *ast.CreateTable, obj string) {
	var hidden []string
	for _, c := range s.Columns {
		if c.SystemVersioning && c.Invisible {
			hidden = append(hidden, normalizeTSQLIdent(c.Name))
		}
	}
	if len(hidden) == 0 {
		return
	}
	t.res.Explanations = append(t.res.Explanations, Explanation{
		Object: "table." + obj,
		Source: "GENERATED ALWAYS AS ROW START | END HIDDEN",
		Target: "regular columns " + strings.Join(hidden, ", "),
		Reason: "SQL Server leaves HIDDEN period columns out of SELECT * and of an INSERT without a column list; PostgreSQL has no hidden columns, so they appear in SELECT * (" + strconv.Itoa(len(hidden)) + " column(s)). An INSERT without a column list that supplies only the other columns still works when the period columns are last: the omitted trailing columns are stamped by the versioning trigger.",
		Level:  "info",
	})
}
