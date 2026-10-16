package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNameTaken = errors.New("app name already exists")

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

type App struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	RepoURL   string    `json:"repo_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Deployment struct {
	ID        string    `json:"id"`
	AppID     string    `json:"app_id"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Job struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	DeploymentID string `json:"deployment_id"`
	Attempts     int    `json:"attempts"`
	MaxAttempts  int    `json:"max_attempts"`
}

// CreateAppWithDeployment atomically creates an app,
// its initial deployment, and its build job.
func (s *Store) CreateAppWithDeployment(
	ctx context.Context,
	name string,
	repoURL string,
) (App, Deployment, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return App{}, Deployment{}, fmt.Errorf(
			"begin transaction: %w", err,
		)
	}
	defer tx.Rollback(ctx)

	var app App
	err = tx.QueryRow(ctx, `
		INSERT INTO apps (name, repo_url)
		VALUES ($1, $2)
		RETURNING id, name, repo_url, created_at, updated_at
	`, name, repoURL).Scan(
		&app.ID,
		&app.Name,
		&app.RepoURL,
		&app.CreatedAt,
		&app.UpdatedAt,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) &&
			pgErr.Code == "23505" {
			return App{}, Deployment{}, ErrNameTaken
		}

		return App{}, Deployment{}, fmt.Errorf(
			"insert app: %w", err,
		)
	}

	var deployment Deployment
	err = tx.QueryRow(ctx, `
		INSERT INTO deployments (app_id, status)
		VALUES ($1, 'queued')
		RETURNING id, app_id, status, created_at, updated_at
	`, app.ID).Scan(
		&deployment.ID,
		&deployment.AppID,
		&deployment.Status,
		&deployment.CreatedAt,
		&deployment.UpdatedAt,
	)
	if err != nil {
		return App{}, Deployment{}, fmt.Errorf(
			"insert deployment: %w", err,
		)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO jobs (deployment_id, status)
		VALUES ($1, 'queued')
	`, deployment.ID)
	if err != nil {
		return App{}, Deployment{}, fmt.Errorf(
			"insert build job: %w", err,
		)
	}

	if err := tx.Commit(ctx); err != nil {
		return App{}, Deployment{}, fmt.Errorf(
			"commit transaction: %w", err,
		)
	}

	return app, deployment, nil
}

// GetDeploymentByID retrieves a deployment by its ID.
func (s *Store) GetDeploymentByID(
	ctx context.Context,
	id string,
) (Deployment, error) {
	var deployment Deployment

	err := s.pool.QueryRow(ctx, `
		SELECT id, app_id, status, created_at, updated_at
		FROM deployments
		WHERE id = $1
	`, id).Scan(
		&deployment.ID,
		&deployment.AppID,
		&deployment.Status,
		&deployment.CreatedAt,
		&deployment.UpdatedAt,
	)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Deployment{}, pgx.ErrNoRows
		}
		return Deployment{}, fmt.Errorf(
			"get deployment: %w", err,
		)
	}

	return deployment, nil
}

func (s *Store) ClaimNextJob(ctx context.Context) (Job, error) {
	var job Job

	err := s.pool.QueryRow(ctx, `
        WITH next_job AS (
            SELECT id
            FROM jobs
            WHERE (
                status = 'queued'
                AND run_at <= now()
            ) OR (
                status = 'running'
                AND locked_until < now()
            )
            ORDER BY run_at
            LIMIT 1
            FOR UPDATE SKIP LOCKED
        )
        UPDATE jobs AS j
        SET status = 'running',
            attempts = j.attempts + 1,
            locked_until = now() + interval '15 minutes',
            updated_at = now()
        FROM next_job
        WHERE j.id = next_job.id
        RETURNING
            j.id,
            j.kind,
            j.deployment_id,
            j.attempts,
            j.max_attempts
    `).Scan(
		&job.ID,
		&job.Kind,
		&job.DeploymentID,
		&job.Attempts,
		&job.MaxAttempts,
	)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Job{}, pgx.ErrNoRows
		}
		return Job{}, fmt.Errorf("claim next job: %w", err)
	}

	return job, nil
}
