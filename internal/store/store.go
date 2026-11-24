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
	ImageTag     *string    `json:"image_tag"` // NEW: set when the build succeeds
	Port         *int       `json:"port"`      // NEW: detected from the image
	HostPort     *int       `json:"host_port"` // NEW: where the live container answers
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
	AppID        string // NEW: used in the image name
	RepoURL      string
	Branch       string // CHANGED: empty means the repository's default branch
	CommitSHA    string
}

// CreateAppWithDeployment atomically creates an app,
// its initial deployment, and its build job.
// CHANGED: takes the owner; the app belongs to whoever created it.
func (s *Store) CreateAppWithDeployment(
	ctx context.Context,
	ownerID string,
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
		INSERT INTO apps (name, repo_url, owner_id)
		VALUES ($1, $2, $3)
		RETURNING id, name, repo_url, created_at, updated_at
	`, name, repoURL, ownerID).Scan(
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

// AppSummary is one row of the project list: the app, whether it is
// serving traffic, and what its newest deployment is doing. Both are
// needed: a failed redeploy leaves the previous version live.
type AppSummary struct {
	ID                 string
	Name               string
	RepoURL            string
	CreatedAt          time.Time
	Live               bool
	LatestDeploymentID *string // pointers: NULL if the app has no deployment
	LatestStatus       *string
	LatestErrorMessage *string
	LatestCreatedAt    *time.Time
}

// ListApps returns the owner's apps, newest first.
// ponytail: no pagination; add LIMIT/cursor when someone has hundreds of apps.
func (s *Store) ListApps(ctx context.Context, ownerID string) ([]AppSummary, error) {
	// LATERAL runs the "newest deployment" subquery once per app, using
	// deployments_app_id_created_at; EXISTS uses deployments_one_live_per_app.
	rows, err := s.pool.Query(ctx, `
		SELECT a.id::text, a.name, a.repo_url, a.created_at,
		       EXISTS (SELECT 1 FROM deployments h
		               WHERE h.app_id = a.id AND h.status = 'healthy'),
		       l.id::text, l.status, l.error_message, l.created_at
		FROM apps a
		LEFT JOIN LATERAL (
			SELECT id, status, error_message, created_at
			FROM deployments
			WHERE app_id = a.id
			ORDER BY created_at DESC
			LIMIT 1
		) l ON true
		WHERE a.owner_id = $1
		ORDER BY a.created_at DESC
	`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("list apps: %w", err)
	}
	apps, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (AppSummary, error) {
		var a AppSummary
		err := r.Scan(&a.ID, &a.Name, &a.RepoURL, &a.CreatedAt, &a.Live,
			&a.LatestDeploymentID, &a.LatestStatus, &a.LatestErrorMessage, &a.LatestCreatedAt)
		return a, err
	})
	if err != nil {
		return nil, fmt.Errorf("list apps: %w", err)
	}
	return apps, nil
}

var ErrInvalidToken = errors.New("invalid API key")

// UserIDByTokenHash finds the user an API key belongs to. Runs on every
// API request; token_hash is UNIQUE, so it's an index lookup.
func (s *Store) UserIDByTokenHash(ctx context.Context, hash []byte) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id::text FROM users WHERE token_hash = $1`, hash).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrInvalidToken
	}
	if err != nil {
		return "", fmt.Errorf("look up API key: %w", err)
	}
	return id, nil
}

// IssueToken creates the user if new, and gives them this key either way.
// For an existing user that is a rotation: the old key stops working at
// once, which is what you want for a lost or leaked key.
func (s *Store) IssueToken(ctx context.Context, email string, hash []byte) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO users (email, token_hash) VALUES ($1, $2)
		ON CONFLICT (email) DO UPDATE
		SET token_hash = EXCLUDED.token_hash, updated_at = now()
	`, email, hash)
	if err != nil {
		return fmt.Errorf("issue API key: %w", err)
	}
	return nil
}

// ErrDeploymentInProgress: the app already has a deployment in the pipeline.
var ErrDeploymentInProgress = errors.New("a deployment of this app is already in progress")

// CreateDeployment queues a new deployment of an existing app: a fresh
// build of the latest commit on its branch, then a zero-downtime swap.
// On ErrDeploymentInProgress it returns the deployment in progress, so
// the caller can tell the user which one to wait for.
// CHANGED: only the app's owner can redeploy it. Someone else's app looks
// exactly like a missing one (ErrAppNotFound), so this endpoint can't be
// used to probe which apps exist or who owns them.
func (s *Store) CreateDeployment(ctx context.Context, ownerID, appName string) (Deployment, error) {
	var d Deployment
	// One statement: the deployment and its build job exist together or
	// not at all, the same guarantee CreateAppWithDeployment gives.
	err := s.pool.QueryRow(ctx, `
		WITH d AS (
			INSERT INTO deployments (app_id, status)
			SELECT id, 'queued' FROM apps WHERE name = $1 AND owner_id = $2
			RETURNING id, app_id, status, created_at, updated_at
		), j AS (
			INSERT INTO jobs (deployment_id, kind, status)
			SELECT id, 'build', 'pending' FROM d
		)
		SELECT id, app_id, status, created_at, updated_at FROM d
	`, appName, ownerID).Scan(&d.ID, &d.AppID, &d.Status, &d.CreatedAt, &d.UpdatedAt)

	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Deployment{}, ErrAppNotFound
	case errors.As(err, &pgErr) && pgErr.ConstraintName == "deployments_one_in_progress_per_app":
		err = s.pool.QueryRow(ctx, `
			SELECT d.id, d.app_id, d.status, d.created_at, d.updated_at
			FROM deployments d JOIN apps a ON a.id = d.app_id
			WHERE a.name = $1 AND a.owner_id = $2
			  AND d.status IN ('queued', 'building', 'built', 'deploying')
		`, appName, ownerID).Scan(&d.ID, &d.AppID, &d.Status, &d.CreatedAt, &d.UpdatedAt)
		if err != nil {
			// It finished between the two queries; the client can just retry.
			return Deployment{}, ErrDeploymentInProgress
		}
		return d, ErrDeploymentInProgress
	case err != nil:
		return Deployment{}, fmt.Errorf("create deployment: %w", err)
	}
	return d, nil
}

// GetDeploymentByID retrieves a deployment by its ID, if ownerID owns its app.
// CHANGED: scoped to the owner. Another user's deployment gives ErrNoRows,
// the same as a missing one: guessing IDs reveals nothing.
func (s *Store) GetDeploymentByID(
	ctx context.Context,
	ownerID string,
	id string,
) (Deployment, error) {
	var deployment Deployment

	err := s.pool.QueryRow(ctx, `
		SELECT d.id, d.app_id, d.status,
		       d.commit_sha, d.started_at, d.failure_step, d.error_message,
		       d.image_tag, d.port, d.host_port,
		       d.created_at, d.updated_at
		FROM deployments d
		JOIN apps a ON a.id = d.app_id
		WHERE d.id = $1 AND a.owner_id = $2
	`, id, ownerID).Scan(
		&deployment.ID,
		&deployment.AppID,
		&deployment.Status,
		&deployment.CommitSHA,
		&deployment.StartedAt,
		&deployment.FailureStep,
		&deployment.ErrorMessage,
		&deployment.ImageTag,
		&deployment.Port,
		&deployment.HostPort,
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
	// CHANGED: when the last attempt's lease runs out, the job fails here
	// and never reaches MarkJobFailed. The second UPDATE fails its
	// deployment too, otherwise it would show "building" forever.
	// One statement, so both rows change together or not at all.
	_, err := s.pool.Exec(ctx, `
		WITH expired AS (
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
		RETURNING kind, deployment_id, status
		)
		UPDATE deployments AS d
		SET status = CASE e.kind WHEN 'build' THEN 'build_failed' ELSE 'failed' END,
			failure_step = 'worker_timeout',
			error_message = 'the ' || e.kind || ' stopped responding and all attempts were used',
			updated_at = now()
		FROM expired AS e
		WHERE e.status = 'failed'
		  AND d.id = e.deployment_id
		  -- Same per-kind guard as MarkJobFailed.
		  AND ((e.kind = 'build' AND d.status IN ('queued', 'building'))
		    OR (e.kind = 'deploy' AND d.status IN ('built', 'deploying')))
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

	// CHANGED: each job kind may only fail the stages it owns. A stray build
	// failure must never overwrite a deployment that is built or live, and a
	// deploy failure must never touch one that is still building.
	_, err = tx.Exec(ctx, `
		UPDATE deployments
		SET status = CASE $2::text WHEN 'build' THEN 'build_failed' ELSE 'failed' END,
			updated_at = now()
		WHERE id = $1
		  AND (($2::text = 'build' AND status IN ('queued', 'building'))
		    OR ($2::text = 'deploy' AND status IN ('built', 'deploying')))
	`, job.DeploymentID, job.Kind)
	if err != nil {
		return fmt.Errorf("mark deployment failed: %w", err)
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
			a.id::text,
			a.repo_url,
			COALESCE(a.branch, ''),
			COALESCE(d.commit_sha, '')
		FROM deployments d
		JOIN apps a ON a.id = d.app_id
		WHERE d.id = $1
	`, deploymentID).Scan(
		&input.DeploymentID,
		&input.AppID,
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

// MarkDeploymentBuilt records a successful build: the image, its port and
// the build log, and moves the deployment from building to built.
// NEW: replaces SaveBuildResult.
func (s *Store) MarkDeploymentBuilt(
	ctx context.Context,
	deploymentID string,
	imageTag string,
	port int,
	logOutput string,
) error {
	// Also clears failure_step and error_message: if an earlier attempt
	// failed and this retry succeeded, the old failure must not stay visible.
	// CHANGED: the same statement queues the deploy job, so a crash can't
	// leave a built deployment that nothing will ever deploy.
	tag, err := s.pool.Exec(ctx, `
		WITH built AS (
		UPDATE deployments
		SET
			status = 'built',
			image_tag = $2,
			port = $3,
			-- CHANGED: concat_ws skips the NULL from an empty log, so no
			-- leading newline. ::text because Postgres can't infer a
			-- parameter's type inside concat (SQLSTATE 42P18).
			build_log = concat_ws(E'\n', NULLIF(build_log, ''), $4::text),
			failure_step = NULL,
			error_message = NULL,
			updated_at = NOW()
		WHERE id = $1
		  AND status = 'building'
		RETURNING id
		)
		INSERT INTO jobs (deployment_id, kind, status)
		SELECT id, 'deploy', 'pending' FROM built
	`, deploymentID, imageTag, port, logOutput)

	if err != nil {
		return fmt.Errorf("mark deployment built: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf(
			"deployment %s not found or no longer building",
			deploymentID,
		)
	}

	return nil
}

// SaveFailure records why a build or deploy attempt failed. The status is
// left alone: the worker decides between a retry and giving up.
// CHANGED: was SaveBuildFailure; now also used while deploying.
func (s *Store) SaveFailure(
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
			-- CHANGED: concat_ws skips the NULL from an empty log, so no
			-- leading newline. ::text because Postgres can't infer a
			-- parameter's type inside concat (SQLSTATE 42P18).
			build_log = concat_ws(E'\n', NULLIF(build_log, ''), $4::text),
			updated_at = NOW()
		WHERE id = $1
		  AND status IN ('building', 'deploying')
	`,
		deploymentID,
		failureStep,
		errorMessage,
		logOutput,
	)

	if err != nil {
		return fmt.Errorf("save failure: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf(
			"deployment %s not found or not building or deploying",
			deploymentID,
		)
	}

	return nil
}

// ErrNotDeployable: the deployment isn't waiting to be deployed (missing,
// still building, already live, or failed). Retrying can't change that.
var ErrNotDeployable = errors.New("deployment does not exist or cannot enter deploying state")

type DeployInput struct {
	AppID    string
	ImageTag string
	Port     int // the port the app listens on inside the container
}

// StartDeploymentDeploy moves a built deployment to deploying and returns
// what the deployer needs. 'deploying' is accepted too, so a retried or
// recovered job can pick up where the last attempt stopped.
func (s *Store) StartDeploymentDeploy(ctx context.Context, deploymentID string) (DeployInput, error) {
	var in DeployInput
	err := s.pool.QueryRow(ctx, `
		UPDATE deployments
		SET status = 'deploying',
			updated_at = now()
		WHERE id = $1
		  AND status IN ('built', 'deploying')
		  AND image_tag IS NOT NULL
		  AND port IS NOT NULL
		RETURNING app_id::text, image_tag, port
	`, deploymentID).Scan(&in.AppID, &in.ImageTag, &in.Port)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeployInput{}, fmt.Errorf("deployment %s: %w", deploymentID, ErrNotDeployable)
	}
	if err != nil {
		return DeployInput{}, fmt.Errorf("start deployment deploy: %w", err)
	}
	return in, nil
}

// MarkDeploymentHealthy makes this deployment the app's live one and marks
// the app's previous live deployment superseded, in one transaction, so the
// database never shows two live deployments or none.
func (s *Store) MarkDeploymentHealthy(
	ctx context.Context,
	deploymentID string,
	hostPort int,
	logOutput string,
) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin healthy transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// CHANGED: old one out first, then new one in. The unique index
	// deployments_one_live_per_app is checked after every statement, not at
	// commit, so the reverse order would briefly have two live rows and fail.
	// Other readers (the router) still see the switch as one instant.
	_, err = tx.Exec(ctx, `
		UPDATE deployments
		SET status = 'superseded',
			host_port = NULL, -- its container is about to be stopped
			updated_at = now()
		WHERE app_id = (SELECT app_id FROM deployments WHERE id = $1)
		  AND id <> $1
		  AND status = 'healthy'
	`, deploymentID)
	if err != nil {
		return fmt.Errorf("supersede previous deployment: %w", err)
	}

	tag, err := tx.Exec(ctx, `
		UPDATE deployments
		SET status = 'healthy',
			host_port = $2,
			build_log = concat_ws(E'\n', NULLIF(build_log, ''), $3::text),
			failure_step = NULL,
			error_message = NULL,
			updated_at = now()
		WHERE id = $1
		  AND status = 'deploying'
	`, deploymentID, hostPort, logOutput)
	if err != nil {
		return fmt.Errorf("mark deployment healthy: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Returning here rolls back the supersede above too.
		return fmt.Errorf("deployment %s not found or no longer deploying", deploymentID)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit healthy deployment: %w", err)
	}
	return nil
}

var (
	ErrAppNotFound = errors.New("no app with this name")
	ErrAppNotLive  = errors.New("app has no live deployment")
)

// LiveHostPort returns the host port of the app's live container. The
// router calls it on every request, so a swap takes effect on the very
// next request. LEFT JOIN tells "no such app" (404) apart from "app
// exists but isn't running" (503) in one query.
func (s *Store) LiveHostPort(ctx context.Context, appName string) (int, error) {
	var port *int
	err := s.pool.QueryRow(ctx, `
		SELECT d.host_port
		FROM apps a
		LEFT JOIN deployments d
		       ON d.app_id = a.id
		      AND d.status = 'healthy'
		WHERE a.name = $1
	`, appName).Scan(&port)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, ErrAppNotFound
	case err != nil:
		return 0, fmt.Errorf("look up live deployment: %w", err)
	case port == nil:
		return 0, ErrAppNotLive
	}
	return *port, nil
}
