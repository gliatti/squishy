// Package queue implements the Postgres-backed job queue: atomic claim via
// FOR UPDATE SKIP LOCKED, retry with exponential backoff, and a supervisor
// loop that reclaims orphaned running jobs.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusDead      Status = "dead"
)

type Job struct {
	ID          uuid.UUID
	RunID       uuid.UUID
	StepID      uuid.UUID
	BatchID     *uuid.UUID
	Kind        string
	Payload     json.RawMessage
	Priority    int16
	Status      Status
	Attempts    int
	MaxAttempts int
	LockedBy    string
}

var ErrNoJob = errors.New("queue: no job available")

type Store struct {
	Pool *pgxpool.Pool
}

func New(p *pgxpool.Pool) *Store { return &Store{Pool: p} }

// Enqueue inserts a new job.
func (s *Store) Enqueue(ctx context.Context, tx pgx.Tx, j Job) (uuid.UUID, error) {
	if j.ID == uuid.Nil {
		j.ID = uuid.New()
	}
	if j.Status == "" {
		j.Status = StatusPending
	}
	if j.MaxAttempts == 0 {
		j.MaxAttempts = 3
	}
	var payload []byte = j.Payload
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	q := `INSERT INTO squishy.jobs (id, run_id, step_id, batch_id, kind, payload, priority, status, max_attempts)
	      VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`
	exec := func(ctx context.Context) error {
		if tx != nil {
			_, err := tx.Exec(ctx, q, j.ID, j.RunID, j.StepID, j.BatchID, j.Kind, payload, j.Priority, j.Status, j.MaxAttempts)
			return err
		}
		_, err := s.Pool.Exec(ctx, q, j.ID, j.RunID, j.StepID, j.BatchID, j.Kind, payload, j.Priority, j.Status, j.MaxAttempts)
		return err
	}
	if err := exec(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("enqueue: %w", err)
	}
	return j.ID, nil
}

// Claim atomically picks the next pending job, marks it running, and returns
// it. Returns ErrNoJob when none available.
func (s *Store) Claim(ctx context.Context, lockedBy string) (*Job, error) {
	// Note: we exclude jobs whose `attempts` has already hit max_attempts —
	// they cannot be retried, and the UPDATE below would otherwise bump
	// attempts past max+1 and violate the jobs_attempts_chk constraint,
	// taking the whole Claim query into a tight error loop. A bad-data
	// cleanup (e.g. after an earlier runaway retry) is thus absorbed here.
	const q = `
UPDATE squishy.jobs
   SET status       = 'running',
       attempts     = attempts + 1,
       locked_at    = now(),
       locked_by    = $1,
       updated_at   = now()
 WHERE id = (
   SELECT j.id FROM squishy.jobs j
    WHERE j.status = 'pending'
      AND j.available_at <= now()
      AND j.attempts < j.max_attempts
    ORDER BY j.priority ASC, j.available_at ASC, j.id
    FOR UPDATE SKIP LOCKED
    LIMIT 1
 )
RETURNING id, run_id, step_id, batch_id, kind, payload, priority, attempts, max_attempts;`

	row := s.Pool.QueryRow(ctx, q, lockedBy)
	j := Job{LockedBy: lockedBy, Status: StatusRunning}
	var payload []byte
	err := row.Scan(&j.ID, &j.RunID, &j.StepID, &j.BatchID, &j.Kind, &payload, &j.Priority, &j.Attempts, &j.MaxAttempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoJob
	}
	if err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	j.Payload = payload
	return &j, nil
}

// Succeed marks a job as succeeded.
func (s *Store) Succeed(ctx context.Context, id uuid.UUID) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE squishy.jobs SET status='succeeded', locked_at=NULL, locked_by=NULL, updated_at=now()
		 WHERE id=$1`, id)
	return err
}

// Fail marks a job as failed (will retry if attempts < max_attempts) or dead.
// Backoff: min(60s, 2^attempts * 1s) with ±20% jitter.
func (s *Store) Fail(ctx context.Context, id uuid.UUID, reason string) error {
	const q = `
UPDATE squishy.jobs
   SET status = (CASE WHEN attempts >= max_attempts THEN 'dead' ELSE 'pending' END)::job_status,
       available_at = CASE
         WHEN attempts >= max_attempts THEN available_at
         ELSE now() + (LEAST(60, power(2, attempts)) * (0.8 + random()*0.4) || ' seconds')::interval
       END,
       locked_at = NULL, locked_by = NULL,
       last_error = $2,
       updated_at = now()
 WHERE id=$1
RETURNING status;`
	var status string
	if err := s.Pool.QueryRow(ctx, q, id, reason).Scan(&status); err != nil {
		return err
	}
	return nil
}

// RequeueOrphans returns pending any job that has been running past the
// heartbeat TTL. Called by the supervisor tick. Jobs whose attempts already
// hit max_attempts land in 'dead' instead — otherwise they'd sit as pending
// forever (Claim excludes them) and the owning step would never flip failed.
func (s *Store) RequeueOrphans(ctx context.Context, ttl time.Duration) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `
UPDATE squishy.jobs
   SET status       = (CASE WHEN attempts >= max_attempts THEN 'dead' ELSE 'pending' END)::job_status,
       locked_at    = NULL,
       locked_by    = NULL,
       available_at = CASE
         WHEN attempts >= max_attempts THEN available_at
         ELSE now() + (LEAST(60, power(2, attempts)) || ' seconds')::interval
       END,
       last_error   = COALESCE(last_error,'') || E'\n[requeue] lock expired',
       updated_at   = now()
 WHERE status = 'running'
   AND locked_at < now() - $1::interval`, fmt.Sprintf("%d seconds", int(ttl.Seconds())))
	if err != nil {
		return 0, err
	}
	// Mark owning steps failed when any non-batched job just landed in 'dead'.
	// Batched steps (copy_table) finalize via the regular step-rollup path.
	_, _ = s.Pool.Exec(ctx, `
UPDATE squishy.steps s
   SET status='failed', error=COALESCE(j.last_error,'lock expired after max attempts'),
       finished_at=now()
  FROM squishy.jobs j
 WHERE j.step_id = s.id
   AND j.batch_id IS NULL
   AND j.status = 'dead'
   AND s.status NOT IN ('succeeded','failed')`)
	return tag.RowsAffected(), nil
}

// Retry resets non-succeeded jobs + step_batches + steps in a run so
// everything pending or broken is picked up again.
func (s *Store) Retry(ctx context.Context, runID uuid.UUID) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `
UPDATE squishy.jobs
   SET status='pending',
       attempts=0,
       available_at=now(),
       locked_at=NULL, locked_by=NULL, updated_at=now()
 WHERE run_id=$1 AND status IN ('failed','dead','running')`, runID)
	if err != nil {
		return 0, err
	}
	// Reset batches that may have been recorded as failed so the copier reprocesses them.
	_, _ = s.Pool.Exec(ctx, `
UPDATE squishy.step_batches b
   SET status='pending', attempts=0, error=NULL, updated_at=now()
  FROM squishy.steps s
 WHERE b.step_id=s.id AND s.run_id=$1
   AND b.status IN ('failed','running')`, runID)
	return tag.RowsAffected(), nil
}
