-- +goose Up

ALTER TABLE deployments
    ADD COLUMN started_at TIMESTAMPTZ,
    ADD COLUMN commit_sha TEXT,
    ADD COLUMN failure_step TEXT,
    ADD COLUMN error_message TEXT,
    ADD COLUMN build_log TEXT NOT NULL DEFAULT '';



ALTER TABLE apps
    ADD COLUMN branch TEXT NOT NULL DEFAULT 'main';



-- +goose Down

ALTER TABLE deployments
    DROP COLUMN IF EXISTS build_log,
    DROP COLUMN IF EXISTS error_message,
    DROP COLUMN IF EXISTS failure_step,
    DROP COLUMN IF EXISTS commit_sha,
    DROP COLUMN IF EXISTS started_at;


ALTER TABLE apps
    DROP COLUMN branch;