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

// CHANGED: Permanent marks an error as not worth retrying, without changing
// its message. Wrapping with fmt.Errorf("%w: ...", ErrPermanent) also works,
// but adds "permanent job failure:" to the text users see in error_message.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err: err}
}

type permanentError struct{ err error }

func (e permanentError) Error() string        { return e.err.Error() }
func (e permanentError) Unwrap() error        { return e.err }
func (e permanentError) Is(target error) bool { return target == ErrPermanent }

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

	// Use a fresh bounded context for every save below, so a cancelled
	// worker context doesn't stop the outcome from being recorded.
	saveCtx, saveCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer saveCancel()

	switch {
	case err == nil:
		// CHANGED: was the parent context. If shutdown began right after the
		// job finished, the save failed and the finished job ran again later.
		if saveErr := w.store.MarkJobDone(saveCtx, job); saveErr != nil {
			log.Printf("mark job %s done: %v", job.ID, saveErr)
		}

	case parent.Err() != nil:
		// CHANGED: shutting down. The job didn't fail, it was interrupted,
		// so hand it back right away (no delay) for the next start to pick up.
		if saveErr := w.store.RetryJob(saveCtx, job, err, 0); saveErr != nil {
			log.Printf("release job %s on shutdown: %v", job.ID, saveErr)
		}

	case errors.Is(err, ErrPermanent) || job.Attempts >= job.MaxAttempts:
		w.fail(job, err)

	default:
		delay := retryDelay(job.Attempts)
		if saveErr := w.store.RetryJob(saveCtx, job, err, delay); saveErr != nil {
			log.Printf("schedule retry for job %s: %v (original error: %v)",
				job.ID, saveErr, err)
		}
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
