-- +goose Up

-- 'built' = image exists, not yet running. Without it in the list,
-- MarkDeploymentBuilt violates the CHECK and every good build ends build_failed.
ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_status_check;

ALTER TABLE deployments
    ADD CONSTRAINT deployments_status_check
    CHECK (
        status IN (
            'queued', 'building', 'built', 'deploying',
            'healthy', 'unhealthy', 'failed',
            'build_failed', 'cancelled'
        )
    );

-- NULL branch = the repository's default branch (HEAD). The old
-- DEFAULT 'main' broke every repo whose default branch is not main.
ALTER TABLE apps
    ALTER COLUMN branch DROP NOT NULL,
    ALTER COLUMN branch DROP DEFAULT;

-- No API sets a branch yet, so every stored 'main' is the old default,
-- not a user's choice.
UPDATE apps SET branch = NULL WHERE branch = 'main';


-- +goose Down

UPDATE apps SET branch = 'main' WHERE branch IS NULL;

ALTER TABLE apps
    ALTER COLUMN branch SET DEFAULT 'main',
    ALTER COLUMN branch SET NOT NULL;

ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_status_check;

-- The old vocabulary has no 'built'; such deployments need a rebuild.
UPDATE deployments SET status = 'queued' WHERE status = 'built';

ALTER TABLE deployments
    ADD CONSTRAINT deployments_status_check
    CHECK (
        status IN (
            'queued', 'building', 'deploying',
            'healthy', 'unhealthy', 'failed',
            'build_failed', 'cancelled'
        )
    );
