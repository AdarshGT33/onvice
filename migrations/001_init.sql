
-- +goose Up

CREATE TABLE apps (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    repo_url    TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE deployments (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    app_id      BIGINT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    status      TEXT NOT NULL
                CHECK (status IN (
                    'queued',
                    'building',
                    'deploying',
                    'healthy',
                    'unhealthy',
                    'failed',
                    'cancelled'
                )),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE jobs (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    deployment_id   BIGINT NOT NULL
                    REFERENCES deployments(id) ON DELETE CASCADE,
    status          TEXT NOT NULL
                    CHECK (status IN (
                        'queued',
                        'running',
                        'succeeded',
                        'failed',
                        'cancelled'
                    )),
    run_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_jobs_status_run_at
    ON jobs (status, run_at);


-- +goose Down

DROP TABLE IF EXISTS jobs;
DROP TABLE IF EXISTS deployments;
DROP TABLE IF EXISTS apps;
