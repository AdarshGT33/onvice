-- +goose Up

-- At most one live deployment per app, enforced by the database rather
-- than trusted to code: two live rows would make the router's choice
-- arbitrary. It's also the index the router uses on every request
-- (app_id -> its live deployment).
CREATE UNIQUE INDEX IF NOT EXISTS deployments_one_live_per_app
    ON deployments (app_id)
    WHERE status = 'healthy';

-- +goose Down

DROP INDEX IF EXISTS deployments_one_live_per_app;
