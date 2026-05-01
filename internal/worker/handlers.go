package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.com/dalibo/squishy/internal/dataxfer"
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

// hCopyTable expands a copy_table step into N step_batches + N copy_batch jobs.
// Idempotent on re-run: existing batches are kept; missing ones are created.
func (d *Deps) hCopyTable(ctx context.Context, j *queue.Job) error {
	var p struct {
		Schema       string `json:"schema"`
		Table        string `json:"table"`
		TargetSchema string `json:"target_schema"`
	}
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		return err
	}
	if p.TargetSchema == "" {
		p.TargetSchema = "public"
	}
	plan, err := dataxfer.BuildPartitionPlan(ctx, d.SourceDB, d.sourceDialect(), p.Schema, p.Table, d.BatchSize)
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
		// Translator applies pgNormalize() to Oracle identifiers (all-caps
		// → lowercase) so the PG side ends up with `customers` rather than
		// `"CUSTOMERS"`. Mirror that normalization here so the copy targets
		// the table PG actually created.
		payload, _ := json.Marshal(map[string]any{
			"src_schema": p.Schema,
			"src_table":  p.Table,
			"dst_schema": p.TargetSchema,
			"dst_table":  pgNormalizeIdent(d.sourceDialect().Kind(), p.Table),
			"range_kind": plan.RangeKind,
			"low":        r.Low,
			"high":       r.High,
		})
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
	var p struct {
		SrcSchema string         `json:"src_schema"`
		SrcTable  string         `json:"src_table"`
		DstSchema string         `json:"dst_schema"`
		DstTable  string         `json:"dst_table"`
		RangeKind string         `json:"range_kind"`
		Low       map[string]any `json:"low"`
		High      map[string]any `json:"high"`
	}
	if err := json.Unmarshal(j.Payload, &p); err != nil {
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
		SourceDB: d.SourceDB, SourceDialect: d.sourceDialect(), TargetPool: d.TargetPool,
		SrcSchema: p.SrcSchema, SrcTable: p.SrcTable,
		DstSchema: p.DstSchema, DstTable: p.DstTable,
		RangeKind: p.RangeKind, Low: p.Low, High: p.High,
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
	// Substitute `<rel>.<col>%TYPE` (case-insensitive on the relation
	// name; leave the column name and rest of the DDL untouched). Use
	// a regex anchored on identifier boundaries.
	re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(relName) + `\.[A-Za-z_][A-Za-z0-9_]*%TYPE`)
	out := re.ReplaceAllString(ddl, "text")
	if out == ddl {
		return ddl, false
	}
	return out, true
}

func (d *Deps) hValidate(ctx context.Context, j *queue.Job) error {
	var p struct {
		Tables       []string `json:"tables"`
		SourceSchema string   `json:"source_schema"`
		TargetSchema string   `json:"target_schema"`
	}
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		return err
	}
	if p.TargetSchema == "" {
		p.TargetSchema = "public"
	}
	dial := d.sourceDialect()
	for _, t := range p.Tables {
		var srcN int64
		if err := d.SourceDB.QueryRowContext(ctx, dial.CountQuery(p.SourceSchema, t)).Scan(&srcN); err != nil {
			return fmt.Errorf("count source %s: %w", t, err)
		}
		// PG-side: apply the same ident normalization the translator + copier
		// used (all-caps Oracle names folded to lowercase, mixed-case verbatim).
		dstTable := pgNormalizeIdent(dial.Kind(), t)
		var dstN int64
		if err := d.TargetPool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %q.%q`, p.TargetSchema, dstTable)).Scan(&dstN); err != nil {
			return fmt.Errorf("count pg %s: %w", dstTable, err)
		}
		if srcN != dstN {
			return fmt.Errorf("row count mismatch on %s: source=%d target=%d", t, srcN, dstN)
		}
		d.Bus.Publish(ctx, events.Event{
			RunID: j.RunID, StepID: &j.StepID,
			Kind: "log", Level: "info",
			Message: fmt.Sprintf("%s count OK (%d rows)", t, srcN),
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
