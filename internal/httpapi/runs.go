package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"gitlab.com/dalibo/squishy/internal/planner"
	"gitlab.com/dalibo/squishy/internal/project"
	"gitlab.com/dalibo/squishy/internal/queue"
)

func (d *Deps) getMigration(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "migrationID"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid migrationID")
		return
	}
	m, err := d.Repo.GetMigration(r.Context(), id)
	if errors.Is(err, project.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "migration not found")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	okJSON(w, m)
}

// listMigrationRuns returns the runs of a migration, most recent first, with
// the same progress/status fields the run detail page uses. Powers the
// Execution tab's run history.
func (d *Deps) listMigrationRuns(w http.ResponseWriter, r *http.Request) {
	migrationID, err := uuid.Parse(chi.URLParam(r, "migrationID"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid migrationID")
		return
	}
	rows, err := d.DB.Query(r.Context(), `
		SELECT r.id, r.status::text, r.dispatch_mode, r.triggered_by,
		       coalesce(r.started_at,'epoch'::timestamptz),
		       coalesce(r.finished_at,'epoch'::timestamptz),
		       coalesce(r.error,''),
		       r.created_at,
		       coalesce(v.steps_total,0), coalesce(v.steps_done,0),
		       coalesce(v.steps_failed,0), coalesce(v.steps_running,0),
		       coalesce(v.rows_total,0), coalesce(v.rows_done,0)
		  FROM squishy.runs r
		  LEFT JOIN squishy.v_run_progress v ON v.run_id = r.id
		 WHERE r.migration_id = $1
		 ORDER BY r.created_at DESC`, migrationID)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var (
			id                       uuid.UUID
			status, mode, triggeredBy string
			startedAt, finishedAt     any
			errMsg                   string
			createdAt                any
			stTot, stDone, stFail    int64
			stRun                    int64
			rTot, rDone              int64
		)
		if err := rows.Scan(&id, &status, &mode, &triggeredBy,
			&startedAt, &finishedAt, &errMsg, &createdAt,
			&stTot, &stDone, &stFail, &stRun, &rTot, &rDone); err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, map[string]any{
			"id":            id,
			"status":        status,
			"dispatch_mode": mode,
			"triggered_by":  triggeredBy,
			"started_at":    startedAt,
			"finished_at":   finishedAt,
			"error":         errMsg,
			"created_at":    createdAt,
			"steps_total":   stTot,
			"steps_done":    stDone,
			"steps_failed":  stFail,
			"steps_running": stRun,
			"rows_total":    rTot,
			"rows_done":     rDone,
		})
	}
	okJSON(w, map[string]any{"runs": out})
}

// startRun reads the migration, expands its data_plan into steps/batches/jobs,
// and returns the new run_id. Long-running execution is handled by the worker
// pool subscribed to the jobs table.
func (d *Deps) startRun(w http.ResponseWriter, r *http.Request) {
	migrationID, err := uuid.Parse(chi.URLParam(r, "migrationID"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid migrationID")
		return
	}
	m, err := d.Repo.GetMigration(r.Context(), migrationID)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Gate: all blocking prerequisites must be acknowledged.
	var prereqs []struct {
		ID       string `json:"id"`
		Severity string `json:"severity"`
		Title    string `json:"title"`
	}
	_ = json.Unmarshal(m.Prerequisites, &prereqs)
	var acked []string
	_ = json.Unmarshal(m.AckedPrereqs, &acked)
	ackSet := map[string]bool{}
	for _, id := range acked {
		ackSet[id] = true
	}
	var unresolved []string
	for _, p := range prereqs {
		if p.Severity == "blocking" && !ackSet[p.ID] {
			unresolved = append(unresolved, p.Title)
		}
	}
	if len(unresolved) > 0 {
		errJSON(w, http.StatusConflict,
			"unresolved blocking prerequisites — acknowledge them in the wizard checklist first: "+
				strings.Join(unresolved, "; "))
		return
	}

	// Re-materialize the stored plan.Steps from data_plan JSON.
	var dp struct {
		Steps []planner.Step `json:"steps"`
	}
	if err := json.Unmarshal(m.DataPlan, &dp); err != nil {
		errJSON(w, http.StatusInternalServerError, "corrupt data_plan: "+err.Error())
		return
	}
	if len(dp.Steps) == 0 {
		errJSON(w, http.StatusPreconditionFailed, "migration has no steps")
		return
	}

	// data_plan was minted at plan_migration time and carries fixed step UUIDs.
	// Reusing them across runs collides with the steps_pkey of the previous
	// run's still-persisted rows, so we mint fresh IDs here and rewrite each
	// step's depends_on to keep the DAG consistent.
	idMap := make(map[uuid.UUID]uuid.UUID, len(dp.Steps))
	for i := range dp.Steps {
		idMap[dp.Steps[i].ID] = uuid.New()
	}
	for i := range dp.Steps {
		s := &dp.Steps[i]
		s.ID = idMap[s.ID]
		newDeps := make([]uuid.UUID, 0, len(s.DependsOn))
		for _, d := range s.DependsOn {
			if mapped, ok := idMap[d]; ok {
				newDeps = append(newDeps, mapped)
			} else {
				// Should never happen — keep the original to surface the bug
				// rather than silently drop the dependency.
				newDeps = append(newDeps, d)
			}
		}
		s.DependsOn = newDeps
	}

	// Optional body: { "mode": "auto" | "manual", "skip_data": bool }.
	// Default mode "auto"; skip_data drops copy_table and validate steps so
	// the resulting target schema is structurally complete (DDL, indexes,
	// FKs, routines, triggers) but holds no rows — handy for handing off an
	// empty PG snapshot or iterating on routine translation without
	// re-fetching data. Validate is dropped because its row-count check
	// would always fail when the target is intentionally empty.
	mode := "auto"
	skipData := false
	if r.Body != nil {
		var req struct {
			Mode     string `json:"mode"`
			SkipData bool   `json:"skip_data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Mode == "manual" {
			mode = "manual"
		}
		skipData = req.SkipData
	}

	if skipData {
		dp.Steps = filterSkipDataSteps(dp.Steps)
	}

	ctx := r.Context()

	// Reject if a run is already pending/running for this migration. A partial
	// unique index on squishy.runs catches this at the DB level too, but a
	// handler-level check gives a crisp 409 Conflict instead of an opaque
	// unique-violation error.
	var activeID string
	err = d.DB.QueryRow(ctx, `
		SELECT id::text FROM squishy.runs
		 WHERE migration_id = $1
		   AND status IN ('pending','running')
		 LIMIT 1`, migrationID).Scan(&activeID)
	if err == nil {
		errJSON(w, http.StatusConflict,
			"a run is already active for this migration (id="+activeID+"); cancel it before starting a new one")
		return
	}

	tx, err := d.DB.Begin(ctx)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback(ctx)

	runID := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO squishy.runs (id, migration_id, status, dispatch_mode, started_at)
		VALUES ($1, $2, 'running', $3, now())`, runID, migrationID, mode); err != nil {
		// 23505 = unique_violation — the partial-index race guard fired.
		if strings.Contains(err.Error(), "uq_runs_one_active_per_migration") {
			errJSON(w, http.StatusConflict,
				"a run was started concurrently for this migration; refresh and retry")
			return
		}
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	// In manual mode, steps are locked by default — nothing dispatches until
	// the user presses "play" on a level.
	unlocked := mode == "auto"

	// Persist steps.
	for _, s := range dp.Steps {
		payload, _ := json.Marshal(s.Payload)
		deps := s.DependsOn
		if deps == nil {
			deps = []uuid.UUID{}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO squishy.steps
			  (id, run_id, seq, kind, target, payload, depends_on, priority, rows_total, level, unlocked)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			s.ID, runID, s.Seq, s.Kind, nullString(s.Target), payload,
			deps, s.Priority, s.RowsTotal, s.Level, unlocked); err != nil {
			errJSON(w, http.StatusInternalServerError, "steps: "+err.Error())
			return
		}
	}

	// Enqueue initial jobs: any step with no deps is ready — but only in auto mode.
	if mode == "auto" {
		q := queue.Store{Pool: d.DB}
		for _, s := range dp.Steps {
			if len(s.DependsOn) > 0 {
				continue
			}
			payload, _ := json.Marshal(s.Payload)
			if _, err := q.Enqueue(ctx, tx, queue.Job{
				RunID: runID, StepID: s.ID,
				Kind: s.Kind, Payload: payload, Priority: s.Priority,
			}); err != nil {
				errJSON(w, http.StatusInternalServerError, "enqueue: "+err.Error())
				return
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Start the step-dependency unlock loop: we launch a background worker that
	// watches for completed steps and enqueues dependent ones. For simplicity
	// and testability, this is handled by a tiny dispatcher goroutine in
	// cmd/squishy/main. Here we just return the run_id.
	createdJSON(w, map[string]any{"run_id": runID})
}

func (d *Deps) getRun(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid runID")
		return
	}
	row := d.DB.QueryRow(r.Context(), `
		SELECT v.run_id, v.run_status, v.steps_total, v.steps_done, v.steps_failed, v.steps_running,
		       v.rows_total, v.rows_done, r.dispatch_mode
		  FROM squishy.v_run_progress v
		  JOIN squishy.runs r ON r.id = v.run_id
		 WHERE v.run_id=$1`, id)
	var runID uuid.UUID
	var status, mode string
	var tot, done, fail, run, rowsT, rowsD int64
	if err := row.Scan(&runID, &status, &tot, &done, &fail, &run, &rowsT, &rowsD, &mode); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			errJSON(w, http.StatusNotFound, "run not found")
			return
		}
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	okJSON(w, map[string]any{
		"run_id":        runID,
		"status":        status,
		"steps_total":   tot,
		"steps_done":    done,
		"steps_failed":  fail,
		"steps_running": run,
		"rows_total":    rowsT,
		"rows_done":     rowsD,
		"dispatch_mode": mode,
	})
}

func (d *Deps) listSteps(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid runID")
		return
	}
	rows, err := d.DB.Query(r.Context(), `
		SELECT s.id, s.seq, s.kind, s.target, s.status, s.attempts, s.rows_total, s.rows_done,
		       coalesce(s.started_at,'epoch'::timestamptz), coalesce(s.finished_at,'epoch'::timestamptz), coalesce(s.error,''),
		       coalesce(s.depends_on, ARRAY[]::uuid[]), s.priority, s.level, s.unlocked,
		       coalesce((SELECT j.last_error FROM squishy.jobs j
		                  WHERE j.step_id = s.id AND j.last_error IS NOT NULL
		                  ORDER BY j.updated_at DESC LIMIT 1), ''),
		       coalesce((SELECT j.attempts FROM squishy.jobs j
		                  WHERE j.step_id = s.id AND j.batch_id IS NULL
		                  ORDER BY j.updated_at DESC LIMIT 1), 0),
		       coalesce((SELECT j.max_attempts FROM squishy.jobs j
		                  WHERE j.step_id = s.id AND j.batch_id IS NULL
		                  ORDER BY j.updated_at DESC LIMIT 1), 0)
		  FROM squishy.steps s WHERE s.run_id=$1 ORDER BY s.seq`, id)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id uuid.UUID
		var seq int
		var kind, status, errMsg, lastErr string
		var target *string
		var attempts int
		var rt, rd int64
		var sAt, fAt any
		var deps []uuid.UUID
		var priority int16
		var level int
		var unlocked bool
		var jobAttempts, jobMaxAttempts int
		if err := rows.Scan(&id, &seq, &kind, &target, &status, &attempts, &rt, &rd, &sAt, &fAt, &errMsg,
			&deps, &priority, &level, &unlocked, &lastErr, &jobAttempts, &jobMaxAttempts); err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, map[string]any{
			"id":               id,
			"seq":              seq,
			"kind":             kind,
			"target":           target,
			"status":           status,
			"attempts":         attempts,
			"rows_total":       rt,
			"rows_done":        rd,
			"started_at":       sAt,
			"finished_at":      fAt,
			"error":            errMsg,
			"depends_on":       deps,
			"priority":         priority,
			"level":            level,
			"unlocked":         unlocked,
			"last_error":       lastErr,
			"job_attempts":     jobAttempts,
			"job_max_attempts": jobMaxAttempts,
		})
	}
	okJSON(w, map[string]any{"steps": out})
}

func (d *Deps) listBatches(w http.ResponseWriter, r *http.Request) {
	if _, err := uuid.Parse(chi.URLParam(r, "runID")); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid runID")
		return
	}
	stepID, err := uuid.Parse(chi.URLParam(r, "stepID"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid stepID")
		return
	}
	rows, err := d.DB.Query(r.Context(), `
		SELECT id, seq, range_kind, range_low, range_high,
		       row_count_est, row_count, status, attempts,
		       coalesce(started_at,'epoch'::timestamptz),
		       coalesce(finished_at,'epoch'::timestamptz),
		       coalesce(error,'')
		  FROM squishy.step_batches
		 WHERE step_id=$1
		 ORDER BY seq`, stepID)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id uuid.UUID
		var seq int
		var rangeKind, status, errMsg string
		var rangeLow, rangeHigh []byte
		var rowEst, rowCount int64
		var attempts int
		var sAt, fAt any
		if err := rows.Scan(&id, &seq, &rangeKind, &rangeLow, &rangeHigh, &rowEst, &rowCount, &status, &attempts, &sAt, &fAt, &errMsg); err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, map[string]any{
			"id":            id,
			"seq":           seq,
			"range_kind":    rangeKind,
			"range_low":     json.RawMessage(rangeLow),
			"range_high":    json.RawMessage(rangeHigh),
			"row_count_est": rowEst,
			"row_count":     rowCount,
			"status":        status,
			"attempts":      attempts,
			"started_at":    sAt,
			"finished_at":   fAt,
			"error":         errMsg,
		})
	}
	okJSON(w, map[string]any{"batches": out})
}

func (d *Deps) streamEvents(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid runID")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	if err := d.Bus.ServeSSE(ctx, id, w); err != nil {
		d.Log.Warn().Err(err).Msg("sse")
	}
}

func (d *Deps) cancelRun(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid runID")
		return
	}
	if _, err := d.DB.Exec(r.Context(), `
		UPDATE squishy.runs SET status='cancelled', finished_at=now() WHERE id=$1`, id); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := d.DB.Exec(r.Context(), `
		UPDATE squishy.jobs SET status='dead', last_error='run cancelled'
		 WHERE run_id=$1 AND status IN ('pending','running')`, id); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	okJSON(w, map[string]string{"status": "cancelled"})
}

func (d *Deps) retryRun(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid runID")
		return
	}
	q := queue.Store{Pool: d.DB}
	n, err := q.Retry(r.Context(), id)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	// also reset steps to pending so the dispatcher re-unlocks them
	if _, err := d.DB.Exec(r.Context(), `
		UPDATE squishy.steps SET status='pending', error=NULL, finished_at=NULL
		 WHERE run_id=$1 AND status IN ('failed','running','cancelled')`, id); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := d.DB.Exec(r.Context(), `
		UPDATE squishy.runs SET status='running', finished_at=NULL, error=NULL WHERE id=$1`, id); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	okJSON(w, map[string]any{"requeued": n})
}

// playLevel unlocks all steps at (runID, level) and resets any failed/cancelled
// steps at that level to pending so the dispatcher can pick them up. Running
// jobs on those steps (stale retries, failed jobs) are reset to pending too.
func (d *Deps) playLevel(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid runID")
		return
	}
	level, err := parseIntParam(r, "level")
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid level")
		return
	}
	ctx := r.Context()
	tx, err := d.DB.Begin(ctx)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		UPDATE squishy.steps SET unlocked = true
		 WHERE run_id=$1 AND level=$2`, runID, level); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := tx.Exec(ctx, `
		UPDATE squishy.steps
		   SET status='pending', error=NULL, finished_at=NULL, attempts=0
		 WHERE run_id=$1 AND level=$2 AND status IN ('failed','cancelled')`, runID, level); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Reset every non-terminal job for the level: failed/dead/running get
	// re-enqueued, and pending jobs that exhausted their attempts (stuck after
	// orphan requeue loops) get their counter zeroed so Claim picks them up.
	if _, err := tx.Exec(ctx, `
		UPDATE squishy.jobs
		   SET status='pending', attempts=0, available_at=now(),
		       locked_at=NULL, locked_by=NULL, updated_at=now()
		 WHERE run_id=$1
		   AND status IN ('pending','failed','dead','running')
		   AND step_id IN (SELECT id FROM squishy.steps WHERE run_id=$1 AND level=$2)`,
		runID, level); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := tx.Exec(ctx, `
		UPDATE squishy.step_batches b
		   SET status='pending', attempts=0, error=NULL, updated_at=now()
		  FROM squishy.steps s
		 WHERE b.step_id=s.id AND s.run_id=$1 AND s.level=$2
		   AND b.status IN ('failed','running')`, runID, level); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := tx.Exec(ctx, `
		UPDATE squishy.runs SET status='running', finished_at=NULL, error=NULL
		 WHERE id=$1 AND status IN ('failed','cancelled','succeeded')`, runID); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := tx.Commit(ctx); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	okJSON(w, map[string]any{"ok": true, "level": level})
}

// replayLevel fully resets every step at level >= the given level — the target
// level and all its descendants (steps that depend, transitively, on the
// target) — back to pending, clears their jobs/batches, and unlocks. The
// dispatcher then re-runs the whole cascade.
func (d *Deps) replayLevel(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid runID")
		return
	}
	level, err := parseIntParam(r, "level")
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid level")
		return
	}
	ctx := r.Context()
	tx, err := d.DB.Begin(ctx)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback(ctx)

	// Drop every job for steps at level >= target — succeeded ones too, since
	// we're replaying. Deleting jobs cascades to nothing (jobs have no FK
	// children).
	if _, err := tx.Exec(ctx, `
		DELETE FROM squishy.jobs
		 WHERE run_id=$1
		   AND step_id IN (SELECT id FROM squishy.steps WHERE run_id=$1 AND level>=$2)`,
		runID, level); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Drop existing batches — copy_table will recompute them on replay.
	if _, err := tx.Exec(ctx, `
		DELETE FROM squishy.step_batches
		 WHERE step_id IN (SELECT id FROM squishy.steps WHERE run_id=$1 AND level>=$2)`,
		runID, level); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := tx.Exec(ctx, `
		UPDATE squishy.steps
		   SET status='pending', rows_done=0, error=NULL, finished_at=NULL, started_at=NULL,
		       attempts=0, unlocked=true
		 WHERE run_id=$1 AND level>=$2`, runID, level); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := tx.Exec(ctx, `
		UPDATE squishy.runs SET status='running', finished_at=NULL, error=NULL
		 WHERE id=$1 AND status IN ('failed','cancelled','succeeded')`, runID); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := tx.Commit(ctx); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	okJSON(w, map[string]any{"ok": true, "level": level})
}

// playStep is the single-step counterpart of playLevel: unlocks this step,
// resets failed/cancelled → pending, resets attempts on its jobs so Claim can
// pick them up, without touching downstream steps.
func (d *Deps) playStep(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid runID")
		return
	}
	stepID, err := uuid.Parse(chi.URLParam(r, "stepID"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid stepID")
		return
	}
	ctx := r.Context()
	tx, err := d.DB.Begin(ctx)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		UPDATE squishy.steps SET unlocked=true WHERE id=$1 AND run_id=$2`, stepID, runID); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := tx.Exec(ctx, `
		UPDATE squishy.steps
		   SET status='pending', error=NULL, finished_at=NULL, attempts=0
		 WHERE id=$1 AND run_id=$2 AND status IN ('failed','cancelled')`, stepID, runID); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := tx.Exec(ctx, `
		UPDATE squishy.jobs
		   SET status='pending'::job_status, attempts=0, available_at=now(),
		       locked_at=NULL, locked_by=NULL, updated_at=now()
		 WHERE step_id=$1 AND status IN ('pending','failed','dead','running')`, stepID); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := tx.Exec(ctx, `
		UPDATE squishy.step_batches
		   SET status='pending', attempts=0, error=NULL, updated_at=now()
		 WHERE step_id=$1 AND status IN ('failed','running')`, stepID); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := tx.Exec(ctx, `
		UPDATE squishy.runs SET status='running', finished_at=NULL, error=NULL
		 WHERE id=$1 AND status IN ('failed','cancelled','succeeded')`, runID); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := tx.Commit(ctx); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	okJSON(w, map[string]any{"ok": true, "step_id": stepID})
}

// replayStep resets a single step and every step that (transitively) depends
// on it — same effect as replayLevel but scoped to one DAG subtree. Useful to
// re-run just one copy_table + its create_index without redoing siblings.
func (d *Deps) replayStep(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid runID")
		return
	}
	stepID, err := uuid.Parse(chi.URLParam(r, "stepID"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid stepID")
		return
	}
	ctx := r.Context()
	tx, err := d.DB.Begin(ctx)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback(ctx)

	// Gather the target step + its transitive descendants via depends_on.
	const descendantsCTE = `
		WITH RECURSIVE deps(id) AS (
		  SELECT id FROM squishy.steps WHERE id=$2 AND run_id=$1
		  UNION
		  SELECT s.id FROM squishy.steps s
		    JOIN deps d ON d.id = ANY(s.depends_on)
		   WHERE s.run_id=$1
		)
		SELECT id FROM deps`

	if _, err := tx.Exec(ctx, `
		DELETE FROM squishy.jobs
		 WHERE run_id=$1 AND step_id IN (`+descendantsCTE+`)`, runID, stepID); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM squishy.step_batches
		 WHERE step_id IN (`+descendantsCTE+`)`, runID, stepID); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := tx.Exec(ctx, `
		UPDATE squishy.steps
		   SET status='pending', rows_done=0, error=NULL, finished_at=NULL, started_at=NULL,
		       attempts=0, unlocked=true
		 WHERE id IN (`+descendantsCTE+`)`, runID, stepID); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := tx.Exec(ctx, `
		UPDATE squishy.runs SET status='running', finished_at=NULL, error=NULL
		 WHERE id=$1 AND status IN ('failed','cancelled','succeeded')`, runID); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := tx.Commit(ctx); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	okJSON(w, map[string]any{"ok": true, "step_id": stepID})
}

func parseIntParam(r *http.Request, key string) (int, error) {
	raw := chi.URLParam(r, key)
	var n int
	_, err := fmt.Sscanf(raw, "%d", &n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// filterSkipDataSteps drops the data-copy steps (copy_table) and the
// row-count validation step (validate) so the
// resulting target schema is structurally complete — DDL, indexes, FKs,
// routines and triggers all run — but no rows are loaded. Useful for
// snapshotting an empty PG schema for handoff or for iterating on routine
// translation without re-fetching data. The DAG stays connected because
// non-copy steps don't depend on copy_table; we still re-route any
// orphaned dependencies to the first surviving step (typically inspect)
// for safety.
func filterSkipDataSteps(in []planner.Step) []planner.Step {
	dropped := map[uuid.UUID]bool{}
	var rootID uuid.UUID
	for _, s := range in {
		switch s.Kind {
		case "copy_table", "validate":
			// validate compares source vs target row counts; with no rows
			// loaded it would always fail, so drop it alongside copy_table.
			dropped[s.ID] = true
		}
	}
	for _, s := range in {
		if dropped[s.ID] {
			continue
		}
		// First non-dropped step (typically `inspect`) becomes the
		// rerouting target so create_routine has at least one dependency.
		rootID = s.ID
		break
	}
	out := make([]planner.Step, 0, len(in))
	for _, s := range in {
		if dropped[s.ID] {
			continue
		}
		newDeps := make([]uuid.UUID, 0, len(s.DependsOn))
		seen := map[uuid.UUID]bool{}
		for _, d := range s.DependsOn {
			if dropped[d] {
				if rootID != uuid.Nil && rootID != s.ID && !seen[rootID] {
					newDeps = append(newDeps, rootID)
					seen[rootID] = true
				}
				continue
			}
			if !seen[d] {
				newDeps = append(newDeps, d)
				seen[d] = true
			}
		}
		s.DependsOn = newDeps
		out = append(out, s)
	}
	return out
}
