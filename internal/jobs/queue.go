// Package jobs is a Postgres-backed work queue (SELECT ... FOR UPDATE SKIP LOCKED).
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
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

func (q *Queue) Enqueue(ctx context.Context, kind string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = q.pool.Exec(ctx, `INSERT INTO jobs (kind, payload) VALUES ($1, $2)`, kind, b)
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

// RecoverStale requeues jobs left 'running' by a crashed worker.
func (q *Queue) RecoverStale(ctx context.Context) error {
	_, err := q.pool.Exec(ctx, `UPDATE jobs SET status='queued' WHERE status='running'`)
	return err
}

type Handler func(ctx context.Context, j *Job) error

// Run polls until ctx is cancelled, dispatching to handlers by job kind.
func (q *Queue) Run(ctx context.Context, handlers map[string]Handler, onErr func(error)) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		for {
			j, err := q.Claim(ctx)
			if err != nil {
				onErr(err)
				break
			}
			if j == nil {
				break
			}
			h, ok := handlers[j.Kind]
			if !ok {
				_ = q.Fail(ctx, &Job{ID: j.ID, Attempts: MaxAttempts}, errors.New("no handler for "+j.Kind))
				continue
			}
			if err := h(ctx, j); err != nil {
				onErr(err)
				_ = q.Fail(ctx, j, err)
				continue
			}
			_ = q.Done(ctx, j.ID)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
