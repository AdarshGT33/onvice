-- +goose Up

-- At most one deployment per app moving through the pipeline at a time.
-- Two overlapping deploys can finish out of order: an older one retrying
-- after a newer one went live would silently roll the app back to old
-- code. The database refuses the second insert, so two simultaneous
-- redeploy requests can't both get in, which a check-then-insert in app
-- code could not guarantee.
CREATE UNIQUE INDEX IF NOT EXISTS deployments_one_in_progress_per_app
    ON deployments (app_id)
    WHERE status IN ('queued', 'building', 'built', 'deploying');

-- +goose Down

DROP INDEX IF EXISTS deployments_one_in_progress_per_app;
