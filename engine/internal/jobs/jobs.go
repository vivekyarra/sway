// Package jobs is a Postgres-backed job queue (transactional outbox): enqueue inside the same transaction
// as the state change that caused it, claim with FOR UPDATE SKIP LOCKED so any number of workers can run,
// retry with exponential backoff, and recover jobs whose worker died.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/vivekyarra/sway/engine/internal/db"
)

// Job types.
const (
	TypeAttribution  = "attribution"
	TypeWebhook      = "webhook"
	TypePayoutSubmit = "payout_submit"
	TypePayoutStatus = "payout_status"
	TypePayoutCancel = "payout_cancel"
	TypeReconcile    = "reconcile"
)

type Job struct {
	ID       int64
	Type     string
	Payload  json.RawMessage
	Attempts int
}

// Handler processes one job. Returning Retry(d) re-queues without counting as a failure.
type Handler func(ctx context.Context, j Job) error

// RetryError asks the queue to run the job again after a delay (e.g. a payout still PENDING at PayPal).
type RetryError struct{ After time.Duration }

func (e RetryError) Error() string { return fmt.Sprintf("retry after %s", e.After) }

func Retry(after time.Duration) error { return RetryError{After: after} }

// Enqueue adds a job. A non-empty dedupeKey makes it idempotent: the same key is enqueued at most once
// while an earlier job with that key is still queued or running.
func Enqueue(ctx context.Context, q db.Q, typ string, payload any, dedupeKey string, runAfter time.Duration) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var key any
	if dedupeKey != "" {
		key = dedupeKey
		// A finished job with the same key frees the key for a new run.
		if _, err := q.Exec(ctx, `UPDATE jobs SET dedupe_key=NULL WHERE dedupe_key=$1 AND status IN ('DONE','FAILED')`, dedupeKey); err != nil {
			return err
		}
	}
	_, err = q.Exec(ctx, `INSERT INTO jobs (type, payload, dedupe_key, run_after) VALUES ($1,$2,$3, now() + make_interval(secs => $4))
		ON CONFLICT (dedupe_key) DO NOTHING`, typ, raw, key, runAfter.Seconds())
	return err
}

// Worker runs handlers for queued jobs.
type Worker struct {
	DB          *db.DB
	Handlers    map[string]Handler
	Concurrency int
	Poll        time.Duration
	MaxAttempts int
	Log         *slog.Logger
	wake        chan struct{}
	once        sync.Once
}

func (w *Worker) init() {
	w.once.Do(func() {
		w.wake = make(chan struct{}, 1)
		if w.Concurrency <= 0 {
			w.Concurrency = 4
		}
		if w.Poll <= 0 {
			w.Poll = 500 * time.Millisecond
		}
		if w.MaxAttempts <= 0 {
			w.MaxAttempts = 8
		}
		if w.Log == nil {
			w.Log = slog.Default()
		}
	})
}

// Wake nudges idle workers (call after enqueueing for low latency).
func (w *Worker) Wake() {
	w.init()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run blocks until ctx is cancelled, then waits for in-flight jobs to finish (graceful shutdown).
func (w *Worker) Run(ctx context.Context) {
	w.init()
	var wg sync.WaitGroup
	for i := 0; i < w.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.loop(ctx)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				w.recoverStale(ctx)
			}
		}
	}()
	wg.Wait()
}

func (w *Worker) loop(ctx context.Context) {
	for ctx.Err() == nil {
		ran, err := w.RunOne(ctx)
		if err != nil && ctx.Err() == nil {
			w.Log.Error("job loop", "err", err)
		}
		if ran {
			continue
		}
		select {
		case <-ctx.Done():
		case <-w.wake:
		case <-time.After(w.Poll):
		}
	}
}

// RunOne claims and runs a single ready job. It reports whether a job ran.
func (w *Worker) RunOne(ctx context.Context) (bool, error) {
	w.init()
	var j Job
	err := w.DB.Pool.QueryRow(ctx, `UPDATE jobs SET status='RUNNING', attempts=attempts+1, locked_at=now(), updated_at=now()
		WHERE id = (SELECT id FROM jobs WHERE status='QUEUED' AND run_after <= now() ORDER BY run_after, id FOR UPDATE SKIP LOCKED LIMIT 1)
		RETURNING id, type, payload, attempts`).Scan(&j.ID, &j.Type, &j.Payload, &j.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	h, ok := w.Handlers[j.Type]
	if !ok {
		_, err := w.DB.Pool.Exec(ctx, `UPDATE jobs SET status='FAILED', last_error='no handler', updated_at=now() WHERE id=$1`, j.ID)
		return true, err
	}
	jobCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	start := time.Now()
	herr := safeRun(jobCtx, h, j)
	cancel()
	bg := context.WithoutCancel(ctx)
	var retry RetryError
	switch {
	case herr == nil:
		_, err = w.DB.Pool.Exec(bg, `UPDATE jobs SET status='DONE', last_error='', updated_at=now() WHERE id=$1`, j.ID)
		w.Log.Debug("job done", "id", j.ID, "type", j.Type, "ms", time.Since(start).Milliseconds())
	case errors.As(herr, &retry):
		_, err = w.DB.Pool.Exec(bg, `UPDATE jobs SET status='QUEUED', attempts=attempts-1, run_after=now()+make_interval(secs=>$2), updated_at=now() WHERE id=$1`,
			j.ID, retry.After.Seconds())
	case j.Attempts >= w.MaxAttempts:
		_, err = w.DB.Pool.Exec(bg, `UPDATE jobs SET status='FAILED', last_error=$2, updated_at=now() WHERE id=$1`, j.ID, herr.Error())
		w.Log.Error("job failed permanently", "id", j.ID, "type", j.Type, "attempts", j.Attempts, "err", herr)
	default:
		backoff := time.Duration(math.Min(300, math.Pow(2, float64(j.Attempts)))) * time.Second
		_, err = w.DB.Pool.Exec(bg, `UPDATE jobs SET status='QUEUED', last_error=$2, run_after=now()+make_interval(secs=>$3), updated_at=now() WHERE id=$1`,
			j.ID, herr.Error(), backoff.Seconds())
		w.Log.Warn("job failed, will retry", "id", j.ID, "type", j.Type, "attempt", j.Attempts, "backoff", backoff, "err", herr)
	}
	return true, err
}

func safeRun(ctx context.Context, h Handler, j Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return h(ctx, j)
}

// recoverStale requeues jobs whose worker died mid-run.
func (w *Worker) recoverStale(ctx context.Context) {
	tag, err := w.DB.Pool.Exec(ctx, `UPDATE jobs SET status='QUEUED', updated_at=now() WHERE status='RUNNING' AND locked_at < now() - interval '15 minutes'`)
	if err == nil && tag.RowsAffected() > 0 {
		w.Log.Warn("recovered stale jobs", "count", tag.RowsAffected())
	}
}

// Drain runs ready jobs until none remain (tests and the demo fast-forward use it).
func (w *Worker) Drain(ctx context.Context, max int) error {
	for i := 0; i < max; i++ {
		ran, err := w.RunOne(ctx)
		if err != nil {
			return err
		}
		if !ran {
			return nil
		}
	}
	return nil
}
