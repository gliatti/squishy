// Package worker implements the in-process worker pool that claims jobs from
// the squishy.jobs queue and dispatches them to registered handlers.
package worker

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"gitlab.com/dalibo/squishy/internal/events"
	"gitlab.com/dalibo/squishy/internal/queue"
)

// Handler executes one job. Returning nil marks it succeeded; an error marks
// it failed (and triggers retry/backoff).
type Handler func(ctx context.Context, j *queue.Job) error

// HandlerRegistry maps job.Kind → Handler.
type HandlerRegistry map[string]Handler

type Pool struct {
	Store     *queue.Store
	Bus       *events.Bus
	Handlers  HandlerRegistry
	Workers   int
	LockedBy  string
	Log       zerolog.Logger

	// Intervals (overridable for tests).
	PollInterval       time.Duration
	OrphanTTL          time.Duration
	OrphanCheckEvery   time.Duration

	stopOnce sync.Once
	wg       sync.WaitGroup
}

// Run starts the pool. Blocks until ctx is cancelled.
func (p *Pool) Run(ctx context.Context) error {
	if p.PollInterval == 0 {
		p.PollInterval = 500 * time.Millisecond
	}
	if p.OrphanTTL == 0 {
		p.OrphanTTL = 2 * time.Minute
	}
	if p.OrphanCheckEvery == 0 {
		p.OrphanCheckEvery = 30 * time.Second
	}
	if p.Workers <= 0 {
		p.Workers = 4
	}

	// Supervisor: requeue orphans.
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		t := time.NewTicker(p.OrphanCheckEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				n, err := p.Store.RequeueOrphans(context.Background(), p.OrphanTTL)
				if err != nil {
					p.Log.Warn().Err(err).Msg("requeue orphans")
					continue
				}
				if n > 0 {
					p.Log.Info().Int64("n", n).Msg("requeued orphans")
				}
			}
		}
	}()

	for i := 0; i < p.Workers; i++ {
		wid := fmt.Sprintf("%s#w%d", p.LockedBy, i)
		p.wg.Add(1)
		go p.workerLoop(ctx, wid)
	}
	<-ctx.Done()
	p.wg.Wait()
	return nil
}

func (p *Pool) workerLoop(ctx context.Context, wid string) {
	defer p.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		j, err := p.Store.Claim(ctx, wid)
		if err != nil {
			if err == queue.ErrNoJob {
				select {
				case <-ctx.Done():
					return
				case <-time.After(p.PollInterval):
				}
				continue
			}
			p.Log.Warn().Err(err).Msg("claim failed")
			time.Sleep(p.PollInterval)
			continue
		}
		p.handle(ctx, j)
	}
}

func (p *Pool) handle(ctx context.Context, j *queue.Job) {
	handler, ok := p.Handlers[j.Kind]
	if !ok {
		_ = p.Store.Fail(ctx, j.ID, "no handler for kind "+j.Kind)
		p.Bus.Publish(ctx, events.Event{
			RunID: j.RunID, StepID: &j.StepID, BatchID: j.BatchID,
			Kind: "log", Level: "error",
			Message: "no handler for kind " + j.Kind,
		})
		return
	}
	start := time.Now()
	// Flip step to 'running' in DB so UI doesn't misleadingly show 'pending'
	// while the worker is actually executing (applies to single-job steps;
	// batched steps also transition via the first batch picking up).
	_, _ = p.Store.Pool.Exec(ctx, `
		UPDATE squishy.steps
		   SET status='running', started_at=COALESCE(started_at, now())
		 WHERE id=$1 AND status='pending'`, j.StepID)
	p.Bus.Publish(ctx, events.Event{
		RunID: j.RunID, StepID: &j.StepID, BatchID: j.BatchID,
		Kind: "step.status", Level: "info",
		Message: "running " + j.Kind,
		Data:    map[string]any{"attempt": j.Attempts, "worker": j.LockedBy},
	})
	err := handler(ctx, j)
	dur := time.Since(start)
	if err != nil {
		_ = p.Store.Fail(ctx, j.ID, err.Error())
		// If the job is now dead (attempts >= max), mark its step as failed so
		// the run-state watcher can terminate the run.
		_, _ = p.Store.Pool.Exec(ctx, `
			UPDATE squishy.steps SET status='failed', error=$2, finished_at=now()
			 WHERE id=$1
			   AND EXISTS (SELECT 1 FROM squishy.jobs j WHERE j.step_id=$1 AND j.status='dead')`,
			j.StepID, err.Error())
		p.Bus.Publish(ctx, events.Event{
			RunID: j.RunID, StepID: &j.StepID, BatchID: j.BatchID,
			Kind: "step.status", Level: "error",
			Message: "failed " + j.Kind + ": " + err.Error(),
			Data:    map[string]any{"attempt": j.Attempts, "duration_ms": dur.Milliseconds()},
		})
		return
	}
	if err := p.Store.Succeed(ctx, j.ID); err != nil {
		p.Log.Warn().Err(err).Msg("mark succeeded")
	}
	// Roll up job success onto the owning step when possible.
	if err := p.maybeFinalizeStep(ctx, j); err != nil {
		p.Log.Warn().Err(err).Msg("finalize step")
	}
	p.Bus.Publish(ctx, events.Event{
		RunID: j.RunID, StepID: &j.StepID, BatchID: j.BatchID,
		Kind: "step.status", Level: "info",
		Message: "succeeded " + j.Kind,
		Data:    map[string]any{"attempt": j.Attempts, "duration_ms": dur.Milliseconds()},
	})
}

// maybeFinalizeStep marks a step as succeeded once all its jobs terminate
// successfully. For non-batched steps (one job, batch_id NULL), the step is
// finalized immediately. For batched steps (copy_table expansion), the step
// is finalized once every step_batches row is succeeded.
func (p *Pool) maybeFinalizeStep(ctx context.Context, j *queue.Job) error {
	db := p.Store.Pool
	var hasBatches bool
	if err := db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM squishy.step_batches WHERE step_id=$1)`,
		j.StepID).Scan(&hasBatches); err != nil {
		return err
	}
	if !hasBatches {
		// single-job step — the job.kind match s.kind, finalize now.
		_, err := db.Exec(ctx,
			`UPDATE squishy.steps SET status='succeeded', finished_at=now() WHERE id=$1 AND status<>'succeeded'`,
			j.StepID)
		return err
	}
	// batched step: count remaining non-terminal batches + non-terminal jobs.
	var pending int
	err := db.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM squishy.step_batches WHERE step_id=$1 AND status NOT IN ('succeeded','skipped','cancelled')) +
		  (SELECT count(*) FROM squishy.jobs         WHERE step_id=$1 AND status IN ('pending','running'))
	`, j.StepID).Scan(&pending)
	if err != nil {
		return err
	}
	if pending > 0 {
		return nil
	}
	_, err = db.Exec(ctx,
		`UPDATE squishy.steps SET status='succeeded', finished_at=now() WHERE id=$1 AND status<>'succeeded'`,
		j.StepID)
	return err
}

// NewIDFrom is a tiny helper used by handlers that need to emit a new UUID
// in payload-embedded events without importing uuid elsewhere.
func NewIDFrom() uuid.UUID { return uuid.New() }
