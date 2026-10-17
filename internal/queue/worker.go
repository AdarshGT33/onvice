package queue

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/AdarshGT33/onvice/internal/store"
	"github.com/jackc/pgx/v5"
)

var ErrPermanent = errors.New("permanent job failure")

type Store interface {
	ClaimNextJob(context.Context) (store.Job, error)
	RecoverExpiredJobs(context.Context) error
	MarkJobDone(context.Context, store.Job) error
	RetryJob(context.Context, store.Job, error, time.Duration) error
	MarkJobFailed(context.Context, store.Job, error) error
}

type Handler func(context.Context, store.Job) error

type Worker struct {
	store    Store
	handlers map[string]Handler
}

func NewWorker(s Store, handlers map[string]Handler) *Worker {
	return &Worker{
		store:    s,
		handlers: handlers,
	}
}

func (w *Worker) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := w.store.RecoverExpiredJobs(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("recover expired jobs: %v", err)
			if !wait(ctx, 2*time.Second) {
				return
			}
			continue
		}

		job, err := w.store.ClaimNextJob(ctx)
		if errors.Is(err, pgx.ErrNoRows) {
			if !wait(ctx, 2*time.Second) {
				return
			}
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("claim job: %v", err)
			if !wait(ctx, 2*time.Second) {
				return
			}
			continue
		}

		w.process(ctx, job)
	}
}

func (w *Worker) process(parent context.Context, job store.Job) {
	handler, ok := w.handlers[job.Kind]
	if !ok {
		w.fail(job, fmt.Errorf("%w: unknown job kind %q", ErrPermanent, job.Kind))
		return
	}

	// Must be shorter than the job lease unless you implement lease renewal.
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	err := handler(ctx, job)
	cancel()

	if err == nil {
		if saveErr := w.store.MarkJobDone(parent, job); saveErr != nil {
			log.Printf("mark job %s done: %v", job.ID, saveErr)
		}
		return
	}

	if errors.Is(err, ErrPermanent) || job.Attempts >= job.MaxAttempts {
		w.fail(job, err)
		return
	}

	delay := retryDelay(job.Attempts)

	// Use a fresh bounded context so a canceled worker context does not
	// prevent saving the retry decision during shutdown.
	saveCtx, saveCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer saveCancel()

	if saveErr := w.store.RetryJob(saveCtx, job, err, delay); saveErr != nil {
		log.Printf("schedule retry for job %s: %v (original error: %v)",
			job.ID, saveErr, err)
	}
}

func (w *Worker) fail(job store.Job, cause error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := w.store.MarkJobFailed(ctx, job, cause); err != nil {
		log.Printf("mark job %s failed: %v (original error: %v)",
			job.ID, err, cause)
	}
}

func retryDelay(attempt int) time.Duration {
	// Attempt 1 -> 30s, attempt 2 -> 60s, attempt 3 -> 120s.
	delay := 30 * time.Second
	for i := 1; i < attempt && i < 10; i++ {
		delay *= 2
	}
	return delay
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
