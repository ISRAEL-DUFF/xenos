// Package jobs is a Postgres-backed work queue (SELECT ... FOR UPDATE SKIP LOCKED).
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const MaxAttempts = 3

type Job struct {
	ID       int64
	Kind     string
	Payload  json.RawMessage
	Attempts int
}

type Queue struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Queue { return &Queue{pool} }

// Execer is satisfied by *pgxpool.Pool and pgx.Tx.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func (q *Queue) Enqueue(ctx context.Context, kind string, payload any) error {
	return EnqueueTx(ctx, q.pool, kind, payload)
}

// EnqueueTx adds a job inside the caller's transaction, so the job exists if and only if the change that needs it commits.
func EnqueueTx(ctx context.Context, e Execer, kind string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = e.Exec(ctx, `INSERT INTO jobs (kind, payload) VALUES ($1, $2)`, kind, b)
	return err
}

// Claim takes the next ready job, or returns (nil, nil) when none is ready.
func (q *Queue) Claim(ctx context.Context) (*Job, error) {
	var j Job
	err := q.pool.QueryRow(ctx, `
		UPDATE jobs SET status='running', attempts = attempts + 1
		WHERE id = (
			SELECT id FROM jobs WHERE status='queued' AND run_after <= now()
			ORDER BY run_after, id FOR UPDATE SKIP LOCKED LIMIT 1)
		RETURNING id, kind, payload, attempts`).Scan(&j.ID, &j.Kind, &j.Payload, &j.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &j, err
}

func (q *Queue) Done(ctx context.Context, id int64) error {
	_, err := q.pool.Exec(ctx, `UPDATE jobs SET status='done', last_error=NULL WHERE id=$1`, id)
	return err
}

// Fail requeues with exponential backoff until MaxAttempts, then marks failed.
func (q *Queue) Fail(ctx context.Context, j *Job, cause error) error {
	if j.Attempts >= MaxAttempts {
		_, err := q.pool.Exec(ctx, `UPDATE jobs SET status='failed', last_error=$2 WHERE id=$1`, j.ID, cause.Error())
		return err
	}
	delay := time.Duration(1<<j.Attempts) * 10 * time.Second
	_, err := q.pool.Exec(ctx,
		`UPDATE jobs SET status='queued', last_error=$2, run_after = now() + $3::interval WHERE id=$1`,
		j.ID, cause.Error(), delay.String())
	return err
}

// Requeue puts an interrupted job back without counting the attempt.
func (q *Queue) Requeue(ctx context.Context, j *Job) error {
	_, err := q.pool.Exec(ctx, `UPDATE jobs SET status='queued', attempts = GREATEST(attempts - 1, 0) WHERE id=$1`, j.ID)
	return err
}

// RecoverStale requeues jobs left 'running' by a crashed worker.
func (q *Queue) RecoverStale(ctx context.Context) error {
	_, err := q.pool.Exec(ctx, `UPDATE jobs SET status='queued' WHERE status='running'`)
	return err
}

type Handler func(ctx context.Context, j *Job) error

// Run starts `workers` goroutines that poll for jobs until ctx is cancelled
// and dispatch to handlers by kind. Handlers for the same VM must serialise
// themselves (see internal/vm). Completion is recorded even after shutdown
// begins, so an interrupted job is requeued rather than left 'running'.
func (q *Queue) Run(ctx context.Context, workers int, handlers map[string]Handler, onErr func(error)) {
	if workers < 1 {
		workers = 1
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q.loop(ctx, handlers, onErr)
		}()
	}
	wg.Wait()
}

func (q *Queue) loop(ctx context.Context, handlers map[string]Handler, onErr func(error)) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		for ctx.Err() == nil {
			j, err := q.Claim(ctx)
			if err != nil {
				if ctx.Err() == nil {
					onErr(err)
				}
				break
			}
			if j == nil {
				break
			}
			done := context.WithoutCancel(ctx)
			h, ok := handlers[j.Kind]
			if !ok {
				_ = q.Fail(done, &Job{ID: j.ID, Attempts: MaxAttempts}, errors.New("no handler for "+j.Kind))
				continue
			}
			if err := h(ctx, j); err != nil {
				if ctx.Err() != nil {
					// Interrupted by shutdown, not a real failure: retry without penalty.
					_ = q.Requeue(done, j)
					return
				}
				onErr(err)
				_ = q.Fail(done, j, err)
				continue
			}
			_ = q.Done(done, j.ID)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
