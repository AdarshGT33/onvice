-- +goose Up
ALTER TABLE deployments
    ADD COLUMN IF NOT EXISTS image_tag TEXT,
    ADD COLUMN IF NOT EXISTS port INTEGER CHECK (port BETWEEN 1 AND 65535);

-- +goose Down
ALTER TABLE deployments
    DROP COLUMN IF EXISTS port,
    DROP COLUMN IF EXISTS image_tag;