-- +goose Up

-- "Newest deployment of each app" (the project list) reads this index
-- instead of scanning every deployment ever made, which only grows.
-- Also speeds up deleting an app: the ON DELETE CASCADE from apps has
-- to find its deployments by app_id.
CREATE INDEX IF NOT EXISTS deployments_app_id_created_at
    ON deployments (app_id, created_at DESC);

-- +goose Down

DROP INDEX IF EXISTS deployments_app_id_created_at;
