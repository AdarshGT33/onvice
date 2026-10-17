-- +goose Up

-- Remove existing status constraints so existing values can be migrated.
ALTER TABLE jobs
    DROP CONSTRAINT IF EXISTS jobs_status_check;

ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_status_check;

-- Translate existing job statuses to the worker's status vocabulary.
UPDATE jobs
SET status = CASE status
    WHEN 'queued' THEN 'pending'
    WHEN 'succeeded' THEN 'done'
    ELSE status
END;

-- Add worker and retry fields.
ALTER TABLE jobs
    ADD COLUMN kind TEXT NOT NULL DEFAULT 'build',
    ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN max_attempts INTEGER NOT NULL DEFAULT 3,
    ADD COLUMN locked_until TIMESTAMPTZ,
    ADD COLUMN last_error TEXT;

-- Preserve existing scheduled times by renaming the column.
ALTER TABLE jobs RENAME COLUMN run_at TO run_after;

-- Apply the new job status constraint.
ALTER TABLE jobs
    ADD CONSTRAINT jobs_status_check
    CHECK (status IN ('pending', 'running', 'done', 'failed', 'cancelled'));

-- Allow the worker to report a failed build on its deployment.
ALTER TABLE deployments
    ADD CONSTRAINT deployments_status_check
    CHECK (
        status IN (
            'queued', 'building', 'deploying',
            'healthy', 'unhealthy', 'failed',
            'build_failed', 'cancelled'
        )
    );

-- Replace the old index with one matching the new scheduling column.
DROP INDEX IF EXISTS idx_jobs_status_run_at;

CREATE INDEX idx_jobs_status_run_after
    ON jobs(status, run_after);


-- +goose Down

-- Restore the original deployment status vocabulary.
ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_status_check;

UPDATE deployments
SET status = 'failed'
WHERE status = 'build_failed';

ALTER TABLE deployments
    ADD CONSTRAINT deployments_status_check
    CHECK (
        status IN (
            'queued', 'building', 'deploying',
            'healthy', 'unhealthy', 'failed', 'cancelled'
        )
    );

-- Restore the original job status vocabulary.
ALTER TABLE jobs
    DROP CONSTRAINT IF EXISTS jobs_status_check;

UPDATE jobs
SET status = CASE status
    WHEN 'pending' THEN 'queued'
    WHEN 'done' THEN 'succeeded'
    ELSE status
END;

ALTER TABLE jobs
    RENAME COLUMN run_after TO run_at;

ALTER TABLE jobs
    DROP COLUMN IF EXISTS last_error,
    DROP COLUMN IF EXISTS locked_until,
    DROP COLUMN IF EXISTS max_attempts,
    DROP COLUMN IF EXISTS attempts,
    DROP COLUMN IF EXISTS kind;

ALTER TABLE jobs
    ADD CONSTRAINT jobs_status_check
    CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled'));

DROP INDEX IF EXISTS idx_jobs_status_run_after;

CREATE INDEX idx_jobs_status_run_at
    ON jobs(status, run_at);