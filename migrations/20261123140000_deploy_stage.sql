-- +goose Up

-- host_port: where the running container answers on this machine, so the
-- platform (and later the router) can reach the app.
ALTER TABLE deployments
    ADD COLUMN IF NOT EXISTS host_port INTEGER CHECK (host_port BETWEEN 1 AND 65535);

-- 'superseded': was healthy, then a newer deployment of the same app went
-- live and replaced it. Keeps history; only one deployment per app is live.
ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_status_check;

ALTER TABLE deployments
    ADD CONSTRAINT deployments_status_check
    CHECK (
        status IN (
            'queued', 'building', 'built', 'deploying',
            'healthy', 'unhealthy', 'superseded', 'failed',
            'build_failed', 'cancelled'
        )
    );


-- +goose Down

ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_status_check;

-- The old vocabulary has no 'superseded'; those containers are gone anyway.
UPDATE deployments SET status = 'cancelled' WHERE status = 'superseded';

ALTER TABLE deployments
    ADD CONSTRAINT deployments_status_check
    CHECK (
        status IN (
            'queued', 'building', 'built', 'deploying',
            'healthy', 'unhealthy', 'failed',
            'build_failed', 'cancelled'
        )
    );

ALTER TABLE deployments
    DROP COLUMN IF EXISTS host_port;
