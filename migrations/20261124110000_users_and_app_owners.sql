-- +goose Up

-- One API key per user. Only a SHA-256 hash of the key is stored: a
-- leaked database (or backup) doesn't hand out working keys. A plain
-- hash is enough because keys are 256-bit random values, not passwords,
-- so there is nothing to brute-force.
CREATE TABLE users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email TEXT NOT NULL UNIQUE,
    token_hash BYTEA NOT NULL UNIQUE, -- UNIQUE also indexes the per-request lookup
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Every app belongs to one user; only they can see or redeploy it.
-- NULL only for apps created before accounts existed: they keep running
-- and routing, but no one can manage them through the API.
-- No ON DELETE CASCADE: deleting a user must not silently drop apps whose
-- containers are still running.
ALTER TABLE apps
    ADD COLUMN owner_id UUID REFERENCES users(id);

CREATE INDEX apps_owner_id ON apps (owner_id);

-- +goose Down

ALTER TABLE apps DROP COLUMN IF EXISTS owner_id;
DROP TABLE IF EXISTS users;
