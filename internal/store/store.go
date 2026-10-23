package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNameTaken = errors.New("app name already exists")

// CHANGED: a named error, so the builder can tell "this deployment has moved on"
// (never worth retrying) apart from a database hiccup (worth retrying).
var ErrNotBuildable = errors.New("deployment does not exist or cannot enter building state")

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
	ID     string `json:"id"`
	AppID  string `json:"app_id"`
	Status string `json:"status"`
	// CHANGED: the build fields, so GET /deployments/:id shows what happened.
	// Pointers because these columns can be NULL; they appear as null in JSON.
	CommitSHA    *string    `json:"commit_sha"`
	StartedAt    *time.Time `json:"started_at"`
	FailureStep  *string    `json:"failure_step"`
	ErrorMessage *string    `json:"error_message"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

type Job struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	DeploymentID string `json:"deployment_id"`
	Attempts     int    `json:"attempts"`
	MaxAttempts  int    `json:"max_attempts"`
}

type BuildInput struct {
	DeploymentID string
	RepoURL      string
	Branch       string // CHANGED: empty means the repository's default branch
	CommitSHA    string
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
		INSERT INTO jobs (deployment_id, kind, status)
		VALUES ($1, 'build', 'pending')
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

	// CHANGED: also selects the build fields.
	err := s.pool.QueryRow(ctx, `
		SELECT id, app_id, status,
		       commit_sha, started_at, failure_step, error_message,
		       created_at, updated_at
		FROM deployments
		WHERE id = $1
	`, id).Scan(
		&deployment.ID,
		&deployment.AppID,
		&deployment.Status,
		&deployment.CommitSHA,
		&deployment.StartedAt,
		&deployment.FailureStep,
		&deployment.ErrorMessage,
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
				status = 'pending'
				AND run_after <= NOW()
				AND attempts < max_attempts
			) OR (
				status = 'running'
				AND locked_until <= NOW()
				AND attempts < max_attempts
			)
			ORDER BY run_after, created_at
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE jobs AS j
		SET status = 'running',
			attempts = j.attempts + 1,
			locked_until = NOW() + INTERVAL '15 minutes',
			updated_at = NOW()
		FROM next_job
		WHERE j.id = next_job.id
		RETURNING
			j.id::text,
			j.kind,
			j.deployment_id::text,
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

func (s *Store) RecoverExpiredJobs(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE jobs
		SET status = CASE
				WHEN attempts >= max_attempts THEN 'failed'
				ELSE 'pending'
			END,
			run_after = CASE
				WHEN attempts >= max_attempts THEN run_after
				ELSE now()
			END,
			locked_until = NULL,
			last_error = CASE
				WHEN attempts >= max_attempts
					THEN 'worker lease expired; attempts exhausted'
				ELSE 'worker lease expired'
			END,
			updated_at = now()
		WHERE status = 'running'
		  AND locked_until < now()
	`)
	if err != nil {
		return fmt.Errorf("recover expired jobs: %w", err)
	}
	return nil
}

func (s *Store) MarkJobDone(ctx context.Context, job Job) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE jobs
		SET status = 'done',
			locked_until = NULL,
			last_error = NULL,
			updated_at = now()
		WHERE id = $1 AND status = 'running'
	`, job.ID)
	if err != nil {
		return fmt.Errorf("mark job done: %w", err)
	}
	return nil
}

func (s *Store) RetryJob(
	ctx context.Context,
	job Job,
	cause error,
	delay time.Duration,
) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE jobs
		SET status = 'pending',
			run_after = now() + ($2 * interval '1 second'),
			locked_until = NULL,
			last_error = $3,
			updated_at = now()
		WHERE id = $1 AND status = 'running'
	`, job.ID, delay.Seconds(), cause.Error())

	if err != nil {
		return fmt.Errorf("schedule job retry: %w", err)
	}
	return nil
}

func (s *Store) MarkJobFailed(
	ctx context.Context,
	job Job,
	cause error,
) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin failure transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `
		UPDATE jobs
		SET status = 'failed',
			locked_until = NULL,
			last_error = $2,
			updated_at = now()
		WHERE id = $1 AND status = 'running'
	`, job.ID, cause.Error())
	if err != nil {
		return fmt.Errorf("mark job failed: %w", err)
	}

	if job.Kind == "build" {
		_, err = tx.Exec(ctx, `
			UPDATE deployments
			SET status = 'build_failed',
				updated_at = now()
			WHERE id = $1
		`, job.DeploymentID)
		if err != nil {
			return fmt.Errorf("mark deployment failed: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit job failure: %w", err)
	}
	return nil
}

func (s *Store) StartDeploymentBuild(
	ctx context.Context,
	deploymentID string,
) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE deployments
		SET
			status = 'building',
			started_at = COALESCE(started_at, NOW()),
			updated_at = NOW()
		WHERE id = $1
		  AND status IN ('queued', 'building')
	`, deploymentID)
	if err != nil {
		return fmt.Errorf("start deployment build: %w", err)
	}

	if tag.RowsAffected() == 0 {
		// CHANGED: wraps ErrNotBuildable so callers can check it with errors.Is.
		return fmt.Errorf("deployment %s: %w", deploymentID, ErrNotBuildable)
	}

	return nil
}

func (s *Store) GetBuildInput(
	ctx context.Context,
	deploymentID string,
) (BuildInput, error) {
	var input BuildInput

	// CHANGED: also loads the app's branch (empty if not set).
	err := s.pool.QueryRow(ctx, `
		SELECT
			d.id::text,
			a.repo_url,
			COALESCE(a.branch, ''),
			COALESCE(d.commit_sha, '')
		FROM deployments d
		JOIN apps a ON a.id = d.app_id
		WHERE d.id = $1
	`, deploymentID).Scan(
		&input.DeploymentID,
		&input.RepoURL,
		&input.Branch,
		&input.CommitSHA,
	)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return BuildInput{}, fmt.Errorf(
				"deployment %s not found",
				deploymentID,
			)
		}

		return BuildInput{}, fmt.Errorf(
			"get build input: %w",
			err,
		)
	}

	return input, nil
}

func (s *Store) PinCommitSHA(
	ctx context.Context,
	deploymentID string,
	sha string,
) (string, error) {
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return "", errors.New("commit SHA cannot be empty")
	}

	var savedSHA string

	err := s.pool.QueryRow(ctx, `
		UPDATE deployments
		SET
			commit_sha = COALESCE(NULLIF(commit_sha, ''), $2),
			updated_at = NOW()
		WHERE id = $1
		  AND status = 'building'
		RETURNING commit_sha
	`, deploymentID, sha).Scan(&savedSHA)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf(
				"deployment %s is missing or not in building state",
				deploymentID,
			)
		}

		return "", fmt.Errorf("pin commit SHA: %w", err)
	}

	return savedSHA, nil
}

func (s *Store) SaveBuildResult(
	ctx context.Context,
	deploymentID string,
	logOutput string,
) error {
	// CHANGED: clears failure_step and error_message. If an earlier attempt
	// failed and this retry succeeded, the old failure must not stay visible.
	tag, err := s.pool.Exec(ctx, `
		UPDATE deployments
		SET
			build_log = CONCAT(
				NULLIF(build_log, ''),
				CASE
					WHEN build_log = '' THEN ''
					ELSE E'\n'
				END,
				$2
			),
			failure_step = NULL,
			error_message = NULL,
			updated_at = NOW()
		WHERE id = $1
		  AND status = 'building'
	`, deploymentID, logOutput)

	if err != nil {
		return fmt.Errorf("save build result: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf(
			"deployment %s not found or no longer building",
			deploymentID,
		)
	}

	return nil
}

func (s *Store) SaveBuildFailure(
	ctx context.Context,
	deploymentID string,
	failureStep string,
	errorMessage string,
	logOutput string,
) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE deployments
		SET
			failure_step = $2,
			error_message = $3,
			build_log = CONCAT(
				NULLIF(build_log, ''),
				CASE
					WHEN build_log = '' THEN ''
					ELSE E'\n'
				END,
				$4
			),
			updated_at = NOW()
		WHERE id = $1
		  AND status = 'building'
	`,
		deploymentID,
		failureStep,
		errorMessage,
		logOutput,
	)

	if err != nil {
		return fmt.Errorf("save build failure: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf(
			"deployment %s not found or no longer building",
			deploymentID,
		)
	}

	return nil
}
