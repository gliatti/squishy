package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.com/dalibo/squishy/internal/dataxfer"
	oracle "gitlab.com/dalibo/squishy/internal/dialects/oracle"
	"gitlab.com/dalibo/squishy/internal/events"
	"gitlab.com/dalibo/squishy/internal/queue"
)

// Deps is the set of resources that handlers need access to. Built at startup
// and passed in when wiring the handler registry.
type Deps struct {
	AppDB         *pgxpool.Pool          // squishy metadata DB
	SourceDB      *sql.DB                // source (MySQL / Oracle / …)
	SourceDialect dataxfer.SourceDialect // quoting + placeholders + catalog queries for the source
	TargetPool    *pgxpool.Pool          // PG target
	AdminPool     *pgxpool.Pool          // PG admin (no database override) — used by create_target_db
	Bus           *events.Bus
	BatchSize     int
}

// Handlers returns the default handler registry.
func (d *Deps) Handlers() HandlerRegistry {
	return HandlerRegistry{
		"inspect":          d.hInspect,
		"create_target_db": d.hCreateTargetDB,
		"create_ddl":       d.hCreateDDL,
		"copy_table":       d.hCopyTable,
		"copy_batch":       d.hCopyBatch,
		"create_index":     d.hCreateIndex,
		"create_fk":        d.hCreateFK,
		"create_routine":   d.hCreateRoutine,
		"validate":         d.hValidate,
	}
}

// hCreateTargetDB issues an idempotent CREATE DATABASE on the admin pool.
// The Postgres CREATE DATABASE statement does not accept bind parameters,
// so the database name is double-quote-escaped and interpolated directly;
// we still pre-check existence with a parameterised SELECT to keep the
// happy-path quiet (no "database already exists" error noise in logs).
func (d *Deps) hCreateTargetDB(ctx context.Context, j *queue.Job) error {
	var p struct {
		DBName string `json:"db_name"`
	}
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		return err
	}
	if p.DBName == "" {
		return fmt.Errorf("create_target_db: empty db_name in payload")
	}
	if d.AdminPool == nil {
		return fmt.Errorf("create_target_db: admin pool not configured")
	}

	var exists int
	err := d.AdminPool.QueryRow(ctx,
		`SELECT 1 FROM pg_database WHERE datname=$1`, p.DBName).Scan(&exists)
	if err == nil {
		// Already there — idempotent success.
		d.Bus.Publish(ctx, events.Event{
			RunID: j.RunID, StepID: &j.StepID,
			Kind: "log", Level: "info",
			Message: fmt.Sprintf("database %q already exists, skipping CREATE", p.DBName),
		})
		return nil
	}
	// Any error other than "no rows" is a real failure.
	// pgx surfaces no-rows as pgx.ErrNoRows but we don't import it here;
	// fall through and let CREATE DATABASE attempt to run, which will
	// itself fail loudly on connectivity / permission issues.

	quoted := `"` + strings.ReplaceAll(p.DBName, `"`, `""`) + `"`
	if _, err := d.AdminPool.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		return fmt.Errorf("create database %s: %w", quoted, err)
	}
	d.Bus.Publish(ctx, events.Event{
		RunID: j.RunID, StepID: &j.StepID,
		Kind: "log", Level: "info",
		Message: fmt.Sprintf("created database %q", p.DBName),
	})
	return nil
}

func (d *Deps) hInspect(ctx context.Context, j *queue.Job) error {
	// v1: the inspect step just re-pings the source to confirm it is reachable.
	if d.SourceDB == nil {
		return fmt.Errorf("source DB not configured")
	}
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := d.SourceDB.PingContext(ctx2); err != nil {
		return fmt.Errorf("ping source: %w", err)
	}
	return d.markStepRows(ctx, j.StepID, 1, 1)
}

func (d *Deps) hCreateDDL(ctx context.Context, j *queue.Job) error {
	var p struct {
		Script string `json:"script"`
	}
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		return err
	}
	if p.Script == "" {
		return nil
	}
	_, err := d.TargetPool.Exec(ctx, p.Script)
	return err
}

// copyTablePayload is the payload of a copy_table step (planner.Build).
type copyTablePayload struct {
	Schema       string `json:"schema"`
	Table        string `json:"table"`
	TargetSchema string `json:"target_schema"`
	// TargetTable overrides the PG table the rows land in (default:
	// the source table name).
	TargetTable string `json:"target_table"`
	// SourceVariant selects a non-default row source; RowEndCol is
	// its parameter. See copySourceDialect.
	SourceVariant string `json:"source_variant"`
	RowEndCol     string `json:"row_end_col"`
	// Columns, when set, is the exact column list to copy (source
	// names, which are also the PG names for MySQL/MariaDB). The
	// planner sets it from the translated table when the default —
	// the source catalog's copyable columns — would not match it
	// (MariaDB system-versioning emulation). Empty: catalog default.
	Columns []string `json:"columns,omitempty"`
}

// validate rejects a payload the copy cannot honour faithfully.
func (p copyTablePayload) validate() error {
	if p.SourceVariant == copySourceVariantSystemTimeHistory && len(p.Columns) == 0 {
		// The catalog default skips generated columns, which the
		// history table stores as plain columns: they would be copied
		// as NULL without a word.
		return fmt.Errorf("copy source variant %q needs an explicit column list", p.SourceVariant)
	}
	return nil
}

// copyBatchPayload is the payload of a copy_batch job (hCopyTable).
type copyBatchPayload struct {
	SrcSchema string         `json:"src_schema"`
	SrcTable  string         `json:"src_table"`
	DstSchema string         `json:"dst_schema"`
	DstTable  string         `json:"dst_table"`
	RangeKind string         `json:"range_kind"`
	Low       map[string]any `json:"low"`
	High      map[string]any `json:"high"`

	SourceVariant string   `json:"source_variant"`
	RowEndCol     string   `json:"row_end_col"`
	Columns       []string `json:"columns,omitempty"`
}

// newCopyBatchPayload builds the copy_batch payload of one range of a
// copy_table step. dstTable is the PG table the rows land in.
func newCopyBatchPayload(p copyTablePayload, dstTable, rangeKind string, r dataxfer.Range) copyBatchPayload {
	return copyBatchPayload{
		SrcSchema: p.Schema, SrcTable: p.Table,
		DstSchema: p.TargetSchema, DstTable: dstTable,
		RangeKind: rangeKind, Low: r.Low, High: r.High,
		SourceVariant: p.SourceVariant, RowEndCol: p.RowEndCol,
		Columns: p.Columns,
	}
}

// hCopyTable expands a copy_table step into N step_batches + N copy_batch jobs.
// Idempotent on re-run: existing batches are kept; missing ones are created.
func (d *Deps) hCopyTable(ctx context.Context, j *queue.Job) error {
	var p copyTablePayload
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		return err
	}
	if err := p.validate(); err != nil {
		return err
	}
	if p.TargetSchema == "" {
		p.TargetSchema = "public"
	}
	dial, err := d.copySourceDialect(p.SourceVariant, p.RowEndCol)
	if err != nil {
		return err
	}
	// Translator applies pgNormalize() to Oracle identifiers (all-caps
	// → lowercase) so the PG side ends up with `customers` rather than
	// `"CUSTOMERS"`. Mirror that normalization here so the copy targets
	// the table PG actually created.
	dstTable := pgNormalizeIdent(d.sourceDialect().Kind(), p.Table)
	if p.TargetTable != "" {
		dstTable = p.TargetTable
	}
	plan, err := dataxfer.BuildPartitionPlan(ctx, d.SourceDB, dial, p.Schema, p.Table, d.BatchSize)
	if err != nil {
		return fmt.Errorf("partition %s: %w", p.Table, err)
	}

	tx, err := d.AppDB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`UPDATE squishy.steps SET rows_total=$1 WHERE id=$2`, plan.TotalRows, j.StepID); err != nil {
		return err
	}

	q := queue.Store{Pool: d.AppDB}
	for _, r := range plan.Ranges {
		batchID := uuid.New()
		low, _ := json.Marshal(r.Low)
		high, _ := json.Marshal(r.High)
		var pkCols []string
		for k := range r.Low {
			pkCols = append(pkCols, k)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO squishy.step_batches (id, step_id, seq, pk_columns, range_low, range_high, range_kind, row_count_est)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			ON CONFLICT (step_id, seq) DO NOTHING`,
			batchID, j.StepID, r.Seq, pkCols, low, high, plan.RangeKind, r.RowCount); err != nil {
			return err
		}
		payload, _ := json.Marshal(newCopyBatchPayload(p, dstTable, plan.RangeKind, r))
		if _, err := q.Enqueue(ctx, tx, queue.Job{
			RunID: j.RunID, StepID: j.StepID, BatchID: &batchID,
			Kind: "copy_batch", Payload: payload, Priority: 100,
		}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (d *Deps) hCopyBatch(ctx context.Context, j *queue.Job) error {
	var p copyBatchPayload
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		return err
	}
	dial, err := d.copySourceDialect(p.SourceVariant, p.RowEndCol)
	if err != nil {
		return err
	}
	// JSON unmarshals all numbers as float64; coerce back to int64 so the
	// MySQL driver accepts them for LIMIT/OFFSET and PK ranges.
	for k, v := range p.Low {
		if f, ok := v.(float64); ok && f == float64(int64(f)) {
			p.Low[k] = int64(f)
		}
	}
	for k, v := range p.High {
		if f, ok := v.(float64); ok && f == float64(int64(f)) {
			p.High[k] = int64(f)
		}
	}
	n, err := dataxfer.CopyBatch(ctx, dataxfer.CopyOpts{
		SourceDB: d.SourceDB, SourceDialect: dial, TargetPool: d.TargetPool,
		SrcSchema: p.SrcSchema, SrcTable: p.SrcTable,
		DstSchema: p.DstSchema, DstTable: p.DstTable,
		RangeKind: p.RangeKind, Low: p.Low, High: p.High,
		Columns: p.Columns,
	})
	if err != nil {
		return err
	}
	if j.BatchID != nil {
		_, _ = d.AppDB.Exec(ctx,
			`UPDATE squishy.step_batches SET row_count=$1, status='succeeded', finished_at=now() WHERE id=$2`,
			n, *j.BatchID)
		d.Bus.Publish(ctx, events.Event{
			RunID: j.RunID, StepID: &j.StepID, BatchID: j.BatchID,
			Kind: "batch.progress", Level: "info",
			Message: fmt.Sprintf("copied %d rows into %s.%s", n, p.DstSchema, p.DstTable),
			Data:    map[string]any{"rows": n},
		})
	}
	// increment rows_done on the step
	_, _ = d.AppDB.Exec(ctx,
		`UPDATE squishy.steps SET rows_done = rows_done + $1 WHERE id=$2`, n, j.StepID)
	return nil
}

func (d *Deps) hCreateIndex(ctx context.Context, j *queue.Job) error {
	// Indexes are emitted in the ddl_post_script, so this is a no-op in v1:
	// the create_fk step executes the whole post-script once. We keep the step
	// for granular progress reporting.
	return nil
}

func (d *Deps) hCreateFK(ctx context.Context, j *queue.Job) error {
	var p struct {
		Script string `json:"script"`
	}
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		return err
	}
	if p.Script == "" {
		return nil
	}
	_, err := d.TargetPool.Exec(ctx, p.Script)
	return err
}

func (d *Deps) hCreateRoutine(ctx context.Context, j *queue.Job) error {
	var p struct {
		Name         string `json:"name"`
		Kind         string `json:"kind"`
		DDL          string `json:"ddl"`
		Schema       string `json:"schema"`        // legacy: per-translator schema
		TargetSchema string `json:"target_schema"` // canonical: per-migration target schema
	}
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		return err
	}
	if p.DDL == "" {
		return nil
	}
	schema := p.TargetSchema
	if schema == "" {
		schema = p.Schema
	}
	if schema == "" {
		schema = "public"
	}

	// Execute each routine inside a dedicated transaction with search_path
	// scoped to the migration schema (then public for extension types like
	// pgcrypto / uuid-ossp). This lets PG resolve unqualified table
	// references in view bodies exactly as they resolved on the source side.
	// A future "perfect" translation would qualify every ident via a real
	// MySQL DML parser — tracked as a v2 roadmap item.
	tx, err := d.TargetPool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		fmt.Sprintf(`SET LOCAL search_path TO %q, public`, schema)); err != nil {
		// non-fatal — fall through and attempt the DDL, PG will throw its
		// own error if search_path is the blocker.
		d.Bus.Publish(ctx, events.Event{
			RunID: j.RunID, StepID: &j.StepID,
			Kind: "log", Level: "warn",
			Message: fmt.Sprintf("search_path for %s: %v", p.Name, err),
		})
	}
	if _, err := tx.Exec(ctx, p.DDL); err != nil {
		// Retry path: when the DDL fails because a `<table>.<col>%TYPE`
		// reference points at a relation outside this migration's
		// scope (cross-schema dependency, common in dumps where one
		// schema's triggers reference tables in a sibling Oracle
		// schema not yet migrated), substitute that one column with
		// `text` and re-execute. Without this, otherwise-correct
		// routines fail to apply and the user has to wait for the
		// other schema's migration before re-running ours. The
		// fallback loses the original column type — a reviewer can
		// tighten it post-apply once the parent table exists.
		retryDDL, retried := retryWithTypeFallback(p.DDL, err)
		if retried {
			_ = tx.Rollback(ctx)
			tx2, err2 := d.TargetPool.Begin(ctx)
			if err2 == nil {
				defer tx2.Rollback(ctx)
				_, _ = tx2.Exec(ctx, fmt.Sprintf(`SET LOCAL search_path TO %q, public`, schema))
				if _, err3 := tx2.Exec(ctx, retryDDL); err3 == nil {
					d.Bus.Publish(ctx, events.Event{
						RunID: j.RunID, StepID: &j.StepID,
						Kind: "log", Level: "warn",
						Message: fmt.Sprintf("routine %s: cross-schema %%TYPE fallback to text (parent table missing)", p.Name),
					})
					return tx2.Commit(ctx)
				}
			}
		}
		_ = tx.Rollback(ctx)
		// No silent skip: a routine failure is a real failure the user must
		// address. The wizard checklist already surfaced the prerequisite
		// ("Translate MySQL routine body"), so reaching this point means the
		// user either ack'd without remediating or squishy hit an edge case
		// the checklist didn't cover.
		d.Bus.Publish(ctx, events.Event{
			RunID: j.RunID, StepID: &j.StepID,
			Kind: "log", Level: "error",
			Message: fmt.Sprintf("routine %s (%s) failed: %v", p.Name, p.Kind, err),
		})
		return fmt.Errorf("routine %s (%s): %w", p.Name, p.Kind, err)
	}
	return tx.Commit(ctx)
}

// retryWithTypeFallback inspects a PG error message for the
// `relation "<name>" does not exist` pattern that PG emits when a
// `<table>.<col>%TYPE` reference can't be resolved. When matched, it
// rewrites every `<name>.<col>%TYPE` (case-insensitively) in the DDL to
// the literal `text` so the routine compiles. Returns the rewritten DDL
// and `true` when at least one substitution was made; otherwise the
// original DDL with `false`. Applies repeatedly only until the err
// string offers an actionable target — no full SQL parser involved.
func retryWithTypeFallback(ddl string, execErr error) (string, bool) {
	if execErr == nil {
		return ddl, false
	}
	msg := execErr.Error()
	// Extract the relation name from PG's standard message:
	//   `ERROR: relation "foo" does not exist (SQLSTATE 42P01)`
	const marker = `relation "`
	i := strings.Index(msg, marker)
	if i < 0 {
		return ddl, false
	}
	j := strings.Index(msg[i+len(marker):], `"`)
	if j <= 0 {
		return ddl, false
	}
	relName := msg[i+len(marker) : i+len(marker)+j]
	if relName == "" {
		return ddl, false
	}
	// Substitute `[<schema>.]<rel>.<col>%TYPE` (case-insensitive on the
	// relation name; leave the rest of the DDL untouched) via a token
	// walk — no text matching on SQL.
	return replaceAnchoredTypeRefs(ddl, relName)
}

// replaceAnchoredTypeRefs lexes ddl with the Oracle lexer (its lexical
// rules — "quoted idents", 'strings', comments — match PG-emitted DDL)
// and replaces every token sequence
//
//	[<schema> .] <rel> . <col> % TYPE
//
// whose <rel> matches relName (case-insensitively; relName itself may be
// schema-qualified, e.g. `mig.emp`) with the literal `text`. The optional
// leading schema qualifier is part of the replaced span so
// `mig.emp.sal%TYPE` becomes `text`, not `mig.text`. Occurrences inside
// string literals or comments are separate tokens and never match.
//
// Token offsets are RUNE offsets (the lexer scans a []rune), so the
// splice works on []rune(ddl), never on bytes. Spans are patched from the
// back so earlier offsets stay valid. Returns (rewritten, true) only when
// at least one span was replaced.
func replaceAnchoredTypeRefs(ddl, relName string) (string, bool) {
	if relName == "" {
		return ddl, false
	}
	relParts := identParts(relName)
	if len(relParts) == 0 {
		// Not an `ident(.ident)*` spelling (e.g. a name PG printed that
		// needs quoting in SQL): compare it whole against single tokens,
		// which matches the Lit of a "Quoted Ident".
		relParts = []string{relName}
	}
	toks := significantTokens(ddl)

	type span struct{ start, end int }
	var spans []span
	floor := 0 // first token index not yet covered by a recorded span
	for i := 0; i < len(toks); i++ {
		end, ok := matchTypeRef(toks, i, relParts)
		if !ok {
			continue
		}
		// Absorb the `<qualifier> .` chain in front of the relation
		// (schema, or catalog.schema) so the whole anchored reference
		// is replaced — never reaching into a previously replaced span.
		start := i
		for start-2 >= floor && isPunct(toks[start-1], ".") && isIdentLike(toks[start-2]) {
			start -= 2
		}
		floor = end + 1
		spans = append(spans, span{
			start: toks[start].Pos.Offset,
			end:   toks[end].Pos.Offset + tokenRuneLen(toks[end]),
		})
		i = end
	}
	if len(spans) == 0 {
		return ddl, false
	}
	src := []rune(ddl)
	repl := []rune("text")
	for k := len(spans) - 1; k >= 0; k-- {
		s := spans[k]
		if s.start < 0 || s.end > len(src) || s.start > s.end {
			continue
		}
		out := make([]rune, 0, len(src)-(s.end-s.start)+len(repl))
		out = append(out, src[:s.start]...)
		out = append(out, repl...)
		out = append(out, src[s.end:]...)
		src = out
	}
	return string(src), true
}

// matchTypeRef tries to match `<relParts…> . <col> % TYPE` starting at
// toks[i] and returns the index of the TYPE token on success.
func matchTypeRef(toks []oracle.Token, i int, relParts []string) (int, bool) {
	j := i
	for k, part := range relParts {
		if k > 0 {
			if j >= len(toks) || !isPunct(toks[j], ".") {
				return 0, false
			}
			j++
		}
		if j >= len(toks) || !isIdentLike(toks[j]) || !strings.EqualFold(toks[j].Lit, part) {
			return 0, false
		}
		j++
	}
	if j+3 >= len(toks) {
		return 0, false
	}
	if !isPunct(toks[j], ".") || !isIdentLike(toks[j+1]) || !isPunct(toks[j+2], "%") {
		return 0, false
	}
	typ := toks[j+3]
	if (typ.Kind != oracle.TOK_IDENT && typ.Kind != oracle.TOK_KEYWORD) || !strings.EqualFold(typ.Lit, "TYPE") {
		return 0, false
	}
	return j + 3, true
}

// identParts lexes a (possibly dotted) relation name taken from a PG
// error message into its identifier parts. Returns nil when the name is
// not a clean `ident(.ident)*` sequence.
func identParts(name string) []string {
	var parts []string
	wantIdent := true
	for _, t := range significantTokens(name) {
		switch {
		case t.Kind == oracle.TOK_EOF:
			if wantIdent {
				return nil
			}
			return parts
		case wantIdent && isIdentLike(t):
			parts = append(parts, t.Lit)
			wantIdent = false
		case !wantIdent && isPunct(t, "."):
			wantIdent = true
		default:
			return nil
		}
	}
	return nil
}

// significantTokens returns every non-comment token of src, EOF included.
func significantTokens(src string) []oracle.Token {
	l := oracle.NewLexer(src)
	var out []oracle.Token
	for {
		t := l.Next()
		if t.Kind == oracle.TOK_COMMENT {
			continue
		}
		out = append(out, t)
		if t.Kind == oracle.TOK_EOF {
			return out
		}
	}
}

func isIdentLike(t oracle.Token) bool {
	return t.Kind == oracle.TOK_IDENT || t.Kind == oracle.TOK_QUOTED_IDENT || t.Kind == oracle.TOK_KEYWORD
}

func isPunct(t oracle.Token, lit string) bool {
	return t.Kind == oracle.TOK_PUNCT && t.Lit == lit
}

// tokenRuneLen is the token's length in source runes: Raw when the lexer
// records it (words, quoted idents, strings), else Lit (punctuation).
func tokenRuneLen(t oracle.Token) int {
	if t.Raw != "" {
		return len([]rune(t.Raw))
	}
	return len([]rune(t.Lit))
}

// validatePayload is the validate step payload (planner.Build).
type validatePayload struct {
	Tables []string `json:"tables"`
	// HistoryTables are the emulated MariaDB system-versioned tables'
	// history copies (planner.ValidateHistoryTable).
	HistoryTables []struct {
		SourceTable string `json:"source_table"`
		TargetTable string `json:"target_table"`
		RowEndCol   string `json:"row_end_col"`
	} `json:"history_tables"`
	SourceSchema string `json:"source_schema"`
	TargetSchema string `json:"target_schema"`
}

// countCheck is one row-count comparison of the validate step: the rows
// src reads from SourceTable against the rows of TargetTable.
type countCheck struct {
	src         dataxfer.SourceDialect
	SourceTable string
	TargetTable string
}

// validateChecks lists the row-count comparisons of a validate payload:
// one per copied base table, read through the connection's dialect, and
// one per history copy, read through the same MariaDB system-time
// history source as the copy itself (so a short or empty history copy
// is caught like a short table copy).
func (d *Deps) validateChecks(p validatePayload) ([]countCheck, error) {
	dial := d.sourceDialect()
	out := make([]countCheck, 0, len(p.Tables)+len(p.HistoryTables))
	for _, t := range p.Tables {
		// PG-side: apply the same ident normalization the translator +
		// copier used (all-caps Oracle names folded to lowercase,
		// mixed-case verbatim).
		out = append(out, countCheck{src: dial, SourceTable: t, TargetTable: pgNormalizeIdent(dial.Kind(), t)})
	}
	for _, h := range p.HistoryTables {
		if h.SourceTable == "" || h.TargetTable == "" {
			return nil, fmt.Errorf("validate: history table entry needs source_table and target_table, got %+v", h)
		}
		hist, err := d.copySourceDialect(copySourceVariantSystemTimeHistory, h.RowEndCol)
		if err != nil {
			return nil, fmt.Errorf("validate history %s: %w", h.TargetTable, err)
		}
		out = append(out, countCheck{src: hist, SourceTable: h.SourceTable, TargetTable: h.TargetTable})
	}
	return out, nil
}

func (d *Deps) hValidate(ctx context.Context, j *queue.Job) error {
	var p validatePayload
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		return err
	}
	if p.TargetSchema == "" {
		p.TargetSchema = "public"
	}
	checks, err := d.validateChecks(p)
	if err != nil {
		return err
	}
	for _, c := range checks {
		var srcN int64
		if err := d.SourceDB.QueryRowContext(ctx, c.src.CountQuery(p.SourceSchema, c.SourceTable)).Scan(&srcN); err != nil {
			return fmt.Errorf("count source %s (for %s): %w", c.SourceTable, c.TargetTable, err)
		}
		var dstN int64
		if err := d.TargetPool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %q.%q`, p.TargetSchema, c.TargetTable)).Scan(&dstN); err != nil {
			return fmt.Errorf("count pg %s: %w", c.TargetTable, err)
		}
		if srcN != dstN {
			return fmt.Errorf("row count mismatch on %s: source=%d target=%d", c.TargetTable, srcN, dstN)
		}
		d.Bus.Publish(ctx, events.Event{
			RunID: j.RunID, StepID: &j.StepID,
			Kind: "log", Level: "info",
			Message: fmt.Sprintf("%s count OK (%d rows)", c.TargetTable, srcN),
		})
	}
	return nil
}

func (d *Deps) markStepRows(ctx context.Context, stepID uuid.UUID, total, done int64) error {
	_, err := d.AppDB.Exec(ctx,
		`UPDATE squishy.steps SET rows_total=$1, rows_done=$2 WHERE id=$3`, total, done, stepID)
	return err
}

// sourceDialect returns the dialect wired at connection-resolution time,
// falling back to MySQL for legacy callers that haven't populated the field.
func (d *Deps) sourceDialect() dataxfer.SourceDialect {
	if d.SourceDialect != nil {
		return d.SourceDialect
	}
	return dataxfer.MySQLSource()
}

// copySourceVariantSystemTimeHistory is the copy_table source_variant
// that reads the closed versions of a MariaDB system-versioned table
// (planner.Build emits it for the emulated history table).
const copySourceVariantSystemTimeHistory = "system_time_history"

// copySourceDialect returns the source dialect a copy step reads through:
// the connection's dialect by default, or the MariaDB system-time
// history variant.
func (d *Deps) copySourceDialect(variant, rowEndCol string) (dataxfer.SourceDialect, error) {
	switch variant {
	case "":
		return d.sourceDialect(), nil
	case copySourceVariantSystemTimeHistory:
		if d.sourceDialect().Kind() != "mysql" {
			return nil, fmt.Errorf("copy source variant %q needs a MySQL/MariaDB source, got %q", variant, d.sourceDialect().Kind())
		}
		if rowEndCol == "" {
			return nil, fmt.Errorf("copy source variant %q needs row_end_col", variant)
		}
		return dataxfer.MySQLSystemTimeHistorySource(rowEndCol), nil
	}
	return nil, fmt.Errorf("unknown copy source variant %q", variant)
}

// pgNormalizeIdent mirrors translate.normalizeOracleIdent: all-caps Oracle
// identifiers get lowercased so they match the PG side emitted unquoted,
// while mixed-case ("t_Case_sensitive") round-trips verbatim. Applied to
// destination table + column names before COPY FROM.
func pgNormalizeIdent(kind, s string) string {
	if kind != "oracle" || s == "" {
		return s
	}
	hasLower := false
	hasUpper := false
	for _, r := range s {
		if r >= 'a' && r <= 'z' {
			hasLower = true
		}
		if r >= 'A' && r <= 'Z' {
			hasUpper = true
		}
	}
	if hasUpper && !hasLower {
		out := make([]byte, 0, len(s))
		for _, r := range s {
			if r >= 'A' && r <= 'Z' {
				r += 'a' - 'A'
			}
			out = append(out, byte(r))
		}
		return string(out)
	}
	return s
}
