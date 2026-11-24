# Onvice

A small, self-hosted platform for deploying web apps from GitHub, in the
spirit of Heroku or Render. Point it at a public repository; Onvice builds
it into a Docker image, runs it, checks it is healthy, and serves it at a
stable URL such as `https://myapp.apps.example.com`. Redeploys swap
versions with no downtime, and a bad release can be rolled back in seconds.

- **No Dockerfile needed.** Onvice detects Node, Python, Go and static sites
  and writes the Dockerfile itself. A repository that ships its own
  Dockerfile is built with it unchanged.
- **Zero-downtime deploys.** The new version must pass a health check
  before it receives traffic; the old one keeps serving until then.
- **Rollback.** The previous successful version stays on disk and goes back
  live in seconds, without a rebuild.
- **Isolated users.** Every API call needs a key, and each user sees only
  their own apps.

## How it works

```
POST /projects ─► queue a build job
                     │
worker ─► clone the exact commit ─► detect app / write Dockerfile ─► docker build
       ─► docker run (pinned port, 512 MB, 1 CPU) ─► HTTP health check
       ─► mark healthy, retire the previous version, delete images no longer needed

visitor ─► https://<name>.<APPS_DOMAIN> ─► router ─► the app's live container
```

One Go process runs three things: the **API** (`:8080`), the **router**
that sends app traffic to containers (`:8000`), and a **worker** that runs
build and deploy jobs from a queue in PostgreSQL. Jobs survive restarts:
an interrupted job is picked up again, and a failed job gets up to three
attempts (retried after 30 s, then 60 s) unless retrying cannot help, such
as a repository with nothing Onvice can build.

A deployment moves through these states:

```
queued → building → built → deploying → healthy → superseded (a newer version went live)
            ↘ build_failed         ↘ failed
```

## Requirements

| Tool | Why |
|------|-----|
| Go 1.26+ | builds Onvice |
| PostgreSQL 13+ | stores apps, deployments, users and the job queue |
| Docker | builds and runs the apps |
| git | clones the repositories |
| [goose](https://github.com/pressly/goose) | runs database migrations: `go install github.com/pressly/goose/v3/cmd/goose@latest` |

## Run it locally

1. Create a database, then a `.env` file in the repository root (it is
   git-ignored, so it never gets committed):

   ```
   DATABASE_URL=postgres://postgres:<password>@localhost:5432/onvice_db

   GOOSE_DRIVER=postgres
   GOOSE_DBSTRING=postgres://postgres:<password>@localhost:5432/onvice_db
   GOOSE_MIGRATION_DIR=migrations
   ```

2. Create the tables, then start Docker and the server:

   ```
   goose up
   go run ./cmd/server
   ```

   For automatic rebuilds while editing code, use `go tool air` instead.

3. Issue yourself an API key (shown once; keep it):

   ```
   go run ./cmd/admin issue-key you@example.com
   ```

4. Deploy something:

   ```
   curl -X POST http://localhost:8080/api/v1/projects \
     -H "Authorization: Bearer $ONVICE_KEY" \
     -H "Content-Type: application/json" \
     -d '{"name": "hello", "github_url": "https://github.com/heroku/node-js-getting-started"}'
   ```

   A minute or two later the app is live at `http://hello.localhost:8000`.
   Browsers and curl resolve `*.localhost` to your machine, so no DNS setup
   is needed.

## Configuration

Set as environment variables (or in `.env` for local development).

| Variable | Default | Purpose |
|----------|---------|---------|
| `DATABASE_URL` | *(required)* | PostgreSQL connection string. |
| `APPS_DOMAIN` | `localhost` | Apps are served at `<name>.<APPS_DOMAIN>`. In production, e.g. `apps.example.com`, with wildcard DNS `*.apps.example.com` pointing at the server. |
| `ROUTER_ADDR` | `:8000` | Where app traffic is served. `:443` in production. |
| `TLS_CERT_FILE`, `TLS_KEY_FILE` | *(unset)* | Set both to serve the API and all apps over HTTPS only. The certificate must cover the API's host and `*.<APPS_DOMAIN>`. Renewed files are picked up without a restart. |
| `HTTP_REDIRECT_ADDR` | *(unset)* | Optional plain-HTTP listener (normally `:80`) that redirects app visitors to HTTPS. Requires TLS. |
| `APP_URL_TEMPLATE` | derived | The app URL shown to users; must contain `{name}`. Defaults to what the router serves, e.g. `https://{name}.apps.example.com`. Set it when a load balancer sits in front. |
| `GITHUB_TOKEN` | *(unset)* | Optional. Raises GitHub's API rate limit for repository checks (60 → 5,000 requests per hour). |
| `GIN_MODE` | `debug` | Set to `release` in production. |

The API always listens on `:8080`.

## Supported apps

Onvice looks at the repository root, in this order:

| Found | Built as | Started with |
|-------|----------|--------------|
| `Dockerfile` | the repository's own Dockerfile | its `CMD`; port from its `EXPOSE` |
| `package.json` | Node 22 (`npm ci` with a lockfile, else `npm install`; then `npm run build` if present) | `npm start` |
| `requirements.txt` | Python 3.12 | `python main.py` or `python app.py` |
| `go.mod` | Go 1.26, main package at the root | the compiled binary |
| `index.html` | static site on nginx | nginx |

- A `Procfile` with a `web:` line overrides the start command for Node and
  Python, e.g. `web: gunicorn app:app --bind 0.0.0.0:$PORT`.
- **Apps must listen on `$PORT`** (8080 for generated Dockerfiles), the
  same contract as Heroku and Render.
- An app is healthy once `GET /` answers with any status below 500
  (a 404 counts) within 60 seconds of starting.
- Each app gets 512 MB of memory and 1 CPU, and is restarted if it crashes.
- The whole build and deploy must finish within 10 minutes.
- Only **public** GitHub repositories are supported. Onvice builds the
  latest commit on the repository's default branch.

## API

Every request needs the header `Authorization: Bearer <your key>`.
Errors are JSON: `{"error": "..."}`. Another user's apps and deployments
answer 404, exactly like ones that don't exist.

| Method and path | What it does |
|-----------------|--------------|
| `POST /api/v1/projects` | Create an app and deploy it. |
| `GET /api/v1/projects` | List your apps. |
| `POST /api/v1/projects/:name/deployments` | Redeploy: build the latest commit and swap it in. |
| `POST /api/v1/projects/:name/rollback` | Put the previous successful version back live. |
| `GET /api/v1/deployments/:id` | One deployment's status. |
| `GET /api/v1/deployments/:id/logs` | Its build and deploy log (plain text). |
| `GET /api/v1/projects/:name/logs?lines=200` | The running app's own output (plain text). |

### Create an app

```
POST /api/v1/projects
{"name": "hello", "github_url": "https://github.com/owner/repo"}
```

The name becomes the subdomain: 1–63 lowercase letters, digits or hyphens,
starting and ending with a letter or digit (uppercase is lowercased).
Names are unique across the whole platform.

`202 Accepted`:

```json
{
  "message": "Deployment queued",
  "app_id": "cabcac59-…",
  "name": "hello",
  "github_url": "https://github.com/owner/repo",
  "url": "http://hello.localhost:8000",
  "deployment_id": "27639f67-…",
  "status": "queued"
}
```

Errors: `400` invalid name or URL, `409` name taken, `422` repository
missing or private, `503` GitHub rate limit (with `Retry-After`),
`502` GitHub unreachable.

### List your apps

`GET /api/v1/projects` returns `200`:

```json
{
  "projects": [
    {
      "app_id": "cabcac59-…",
      "name": "hello",
      "github_url": "https://github.com/owner/repo",
      "url": "http://hello.localhost:8000",
      "live": true,
      "created_at": "2026-11-24T10:25:52+05:30",
      "latest_deployment": {
        "deployment_id": "27639f67-…",
        "status": "healthy",
        "error_message": null,
        "created_at": "2026-11-24T10:25:52+05:30"
      }
    }
  ]
}
```

`live` and `latest_deployment` answer different questions: a failed
redeploy leaves the previous version live (`live: true` with a latest
status of `build_failed` or `failed`).

### Redeploy and roll back

`POST /api/v1/projects/:name/deployments` builds the latest commit.
`POST /api/v1/projects/:name/rollback` reuses the previous version's image,
so it is live in seconds; calling it again rolls forward. Both answer
`202` with the new `deployment_id`, and both go through the same health
check and zero-downtime swap.

Only one deployment per app can be in progress. While one is, both answer
`409` with the `deployment_id` to wait for. Rollback also answers `409`
when there is no previous version.

### Follow a deployment

`GET /api/v1/deployments/:id` returns its `status`, `commit_sha`,
`image_tag`, `port`, `host_port` and, on failure, `failure_step` and
`error_message`. Poll it until the status is `healthy`, `failed` or
`build_failed`.

`GET /api/v1/deployments/:id/logs` returns every git, docker and
health-check step with its output; a failed deployment's log ends with
`FAILED at <step>: <reason>`. The log is saved when a step finishes, so it
is empty while a build is still running.

### App logs

`GET /api/v1/projects/:name/logs?lines=200` returns the last lines
(1–5000, default 200) the live container printed, with timestamps.
`409` if the app has no live version.

## Operating Onvice

### API keys

```
go run ./cmd/admin issue-key you@example.com
```

Creates the user if new and prints a key (`onv_` + 64 hex characters).
Running it again for the same email **replaces** the key: the old one stops
working immediately. Use that when a key is lost or leaked. Only a SHA-256
hash of each key is stored. There is no self-service sign-up yet.

### Production checklist

1. Point wildcard DNS `*.apps.example.com` (and the API's host) at the server.
2. Get one certificate covering both, e.g. with certbot and a DNS challenge:

   ```
   certbot certonly --manual --preferred-challenges dns \
     -d api.example.com -d "*.apps.example.com"
   ```

3. Set `TLS_CERT_FILE`, `TLS_KEY_FILE`, `APPS_DOMAIN=apps.example.com`,
   `ROUTER_ADDR=:443`, `HTTP_REDIRECT_ADDR=:80` and `GIN_MODE=release`.
4. Run `goose up`, then start the server. It refuses to start on
   half-configured TLS, and warns loudly when TLS is off.

The API deliberately has no HTTP-to-HTTPS redirect: by the time a plain
HTTP request arrives, its API key has already crossed the network
unencrypted. Plain HTTP requests to the API are rejected instead.

### Disk usage

After every successful deploy, Onvice deletes an app's images except the
live one and the previous one (the rollback target). Docker's build cache
is not pruned automatically; run `docker builder prune` periodically.

## Project layout

```
cmd/server       the platform: API, router and worker in one process; TLS setup
cmd/admin        operator tool: issue API keys
internal/api     HTTP handlers for /api/v1
internal/auth    API keys and the authentication middleware
internal/builder build (clone, Dockerfile, docker build) and deploy (run, health check, swap, cleanup)
internal/queue   the job worker: claiming, retries, shutdown
internal/router  routes <name>.<APPS_DOMAIN> to the app's live container
internal/store   all database access
migrations       goose migrations
```

```
go test ./...
```

## Known limitations

- Public GitHub repositories only; no private repositories or other hosts.
- One worker: builds and deploys run one at a time across all apps.
- One machine: apps run on the same Docker host as Onvice.
- No self-service sign-up; keys are issued by the operator.
- Logs are snapshots, not live streams, and build logs appear only when a
  step finishes.
- One rollback target (the previous version).
- Rollback restores the code, not data: a database change made by the
  newer version is not undone.
