# cerbix Runbook

Operational guide. Grows as capabilities land.

## Run locally (single process)

```bash
cd backend
make build
./bin/cerbix serve --config packaging/config.example.yaml --role all
```

Probes:

```bash
curl -s localhost:8080/healthz   # {"status":"ok"}
curl -s localhost:8080/readyz    # {"status":"ready"}
curl -s localhost:8080/metrics   # cerbix_* series
```

## Run the dev stack

```bash
docker compose -f deploy/docker-compose.yml up --build
# Postgres :5432 · RabbitMQ :5672 (mgmt :15672) · Keycloak :8081 · cerbix :8080
```

## Endpoints

| Endpoint | Meaning |
| --- | --- |
| `/healthz` | Process liveness (always 200 while serving). |
| `/readyz` | Readiness; 503 with reason when not ready (e.g. during shutdown). |
| `/metrics` | Prometheus text (`cerbix_*`). |

## Config

Strict YAML (`packaging/config.example.yaml`). Unknown keys or invalid values cause a
fail-fast startup error logged at `CRITICAL` — the process exits non-zero. There is no
self-healing; fix the config and restart.

## Roles

- `all` — every role in one process (dev).
- `api` — REST + SSE, serves the SPA, consumes results.
- `scheduler` — leader-elected job scheduler.
- `worker` — stateless probe pool.

## Database & migrations

When `database.dsn` is set, `serve` runs migrations at startup, connects, and gates
readiness on live connectivity (a background ping every 10s updates `/readyz` and the
`cerbix_database_up` metric). When `database.dsn` is empty, cerbix runs in scaffold mode
(no DB, ready immediately).

Apply migrations explicitly (e.g. before a deploy):

```bash
./bin/cerbix migrate --config packaging/config.example.yaml   # requires database.dsn
```

Migrations are embedded (`internal/store/migrations/*.sql`, goose format) — no external
goose CLI is needed. On any migration/connection error at startup the process fails fast
(CRITICAL log, exit 1); there is no self-healing.

Run store integration tests against a throwaway Postgres:

```bash
docker run -d --name pg -e POSTGRES_PASSWORD=test -e POSTGRES_USER=test \
  -e POSTGRES_DB=cerbixtest -p 55432:5432 postgres:16-alpine
CERBIX_TEST_DATABASE_DSN="postgres://test:test@localhost:55432/cerbixtest?sslmode=disable" \
  go test ./internal/store/...
```

Storage is adaptive (migration 00043): on a TimescaleDB-enabled database the
suite runs in hypertable mode (declarative-partition tests skip, hypertable and
compression tests run); on plain Postgres — the reverse. To cover the hypertable
side locally, point `CERBIX_TEST_DATABASE_DSN` at a database created on the
`timescale/timescaledb` image (the dev compose Postgres qualifies — the image
pre-creates the extension in `template1`, so fresh databases inherit it).

## Authentication (Keycloak OIDC)

Auth activates when `oidc.issuer` is set (config validation then also requires
`database.dsn`). At `serve` startup cerbix performs OIDC discovery against the issuer and
fails fast if it is unreachable (`oidc_init_failed`, exit 1). When enabled, these routes
are served:

| Route | Purpose |
| --- | --- |
| `GET /auth/login?redirect=/path` | Start Authorization Code + PKCE; redirects to Keycloak. |
| `GET /auth/callback` | Verify ID token, JIT-provision user, create session, set cookie. |
| `GET\|POST /auth/logout` | Revoke session, clear cookie, redirect to post-logout URL. |
| `/api/v1/*` | Requires a valid session cookie (401 otherwise). |

### Local login (no Keycloak)

cerbix also supports a built-in username/password login, enabled with `local.enabled: true`
(requires `database.dsn`). It can run alone or alongside OIDC. Routes registered when
enabled: `POST /auth/local/login` (`{"username","password"}` → session cookie),
`POST /api/v1/me/password` (change own password). Passwords are argon2id-hashed; only the
hash is stored; failures return a uniform 401.

Bootstrap the first admin without Keycloak: set `security.admin_email` and
`security.admin_password`; on an empty system a global admin is created at startup
(`bootstrap_admin_created`). The password is taken from config and **never generated or
logged** — if it is unset, no admin is created (`bootstrap_admin_skipped`). Rotate it via
`/api/v1/me/password` after first login. (Local-login rate limiting is not yet implemented.)

### Keycloak bootstrap

Bootstrap the first global admin by listing their email in `oidc.admin_emails`;
they are promoted on their next login. Sessions live in Postgres (`sessions`, hashed
tokens); expired sessions and `auth_flows` can be swept via
`DeleteExpiredSessions`/`DeleteExpiredAuthFlows` (wired to a scheduled sweep in a later
iteration). Without OIDC, cerbix runs in scaffold mode and the `/api` and `/auth` routes
are not mounted.

## Monitoring pipeline

When a database is configured and `--role=all`, `serve` starts the checking pipeline:

- **scheduler** — contends for leadership via a Postgres advisory lock; the leader scans
  enabled monitors every second and publishes a job when one is due (`scheduler_leader_acquired`).
- **worker pool** — executes probes (HTTP/TCP) with per-check timeout + retries.
- **ingestion** — writes each result as a heartbeat, updates the monitor's `status`, and
  increments `cerbix_checks_total{result="up|down"}`.

Monitors are managed via the API (requires auth): `POST /api/v1/projects/{id}/monitors`,
`GET /api/v1/monitors/{id}`, `GET /api/v1/monitors/{id}/heartbeats`, `DELETE`. Condition
syntax (declarative): `[STATUS] == 200`, `[RESPONSE_TIME] < 500`, `[BODY] contains "UP"`,
`[CONNECTED] == 1`.

Distributed roles (`--role=scheduler` / `--role=worker` in separate processes) require the
RabbitMQ dispatcher, which is not yet wired; use `--role=all` for now. ICMP and push
(dead-man's-switch) monitor types are also not yet implemented.

## SLA / SLI

Availability is computed from heartbeats over rolling windows (24h / 7d / 30d / 90d):

- `GET /api/v1/monitors/{id}/sla` — per-window uptime %, avg latency, and (if an SLO target
  is set) the error budget.
- `PUT /api/v1/monitors/{id}/sla-target` — `{"objective":99.9,"window":"30d"}` sets the SLO.
- `GET /api/v1/projects/{id}/sla` — project-level SLI across its monitors.
- `POST /api/v1/projects/{id}/maintenance` — `{"monitor_id"?,"starts_at","ends_at","reason"}`
  schedules a window; `GET` lists; `DELETE /api/v1/maintenance/{id}` removes.

Heartbeats inside a maintenance window (monitor- or project-scoped) are **excluded** from
SLI numerator and denominator. SLI is computed by direct SQL over `heartbeats`; a
TimescaleDB hypertable + continuous aggregates is a future optimization for large volumes.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Exit code 1 on start, `config_load_failed` log | Config path/keys/values; see `packaging/config.example.yaml`. |
| Exit code 1, `db_migrate_failed` / `db_connect_failed` | DSN, DB reachability, credentials, migration SQL. |
| Exit code 1, `oidc_init_failed` | Keycloak issuer URL reachable? discovery endpoint correct? |
| `/api/*` returns 401 | Missing/expired session cookie; log in via `/auth/login`. |
| `/auth/callback` 400 `invalid or expired login state` | Flow expired (>10m) or reused; retry login. |
| `/readyz` returns 503 with `database unreachable` | DB down or network; check `cerbix_database_up`. |
| `/readyz` returns 503 during stop | Expected during graceful shutdown. |
| Port already in use | Change `server.listen` in config. |

> Sections for broker outages, scheduler failover, and SLA backfill are added as those
> subsystems land.
