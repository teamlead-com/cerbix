# cerbix Runbook

Operational guide. Grows as capabilities land.

## Run locally (single process)

```bash
make build
./bin/cerbix serve --config docker/config.example.yaml --role all
```

Probes:

```bash
curl -s localhost:8080/healthz   # {"status":"ok"}
curl -s localhost:8080/readyz    # {"status":"ready"}
curl -s localhost:8080/metrics   # cerbix_* series
```

## Run the dev stack

```bash
docker compose -f docker/docker-compose.yml up --build
# Postgres :5432 · RabbitMQ :5672 (mgmt :15672) · Keycloak :8081 · cerbix :8080
```

## Endpoints

| Endpoint | Meaning |
| --- | --- |
| `/healthz` | Process liveness (always 200 while serving). |
| `/readyz` | Readiness; 503 with reason when not ready (e.g. during shutdown). |
| `/metrics` | Prometheus text (`cerbix_*`). |

## Config

Strict YAML (`docker/config.example.yaml`). Unknown keys or invalid values cause a
fail-fast startup error logged at `CRITICAL` — the process exits non-zero. There is no
self-healing; fix the config and restart.

> **⚠ Breaking upgrade — notification egress (D-0141).** Outbound alert delivery
> (webhooks, Slack/notify HTTP, SMTP) is now governed by a separate `notification_egress`
> policy that **denies private IPs by default** (previously it inherited the allow-private
> prober policy). If any webhook or `mail.smtp_host` points at an INTERNAL address
> (`mail.internal`, a `10.x`/`192.168.x` proxy, a private Alertmanager), delivery will
> start failing after upgrade. Opt back in explicitly:
> ```yaml
> notification_egress:
>   allow_private_ips: true
> ```
> OIDC (`oidc.issuer`) is unaffected — an internal Keycloak stays supported; identity
> egress is operator-trusted and not routed through this guard.

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
./bin/cerbix migrate --config docker/config.example.yaml   # requires database.dsn
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
`/api/v1/me/password` after first login. Local-login brute-force protection is a per-client-IP
sliding-window limiter (`login_rate_limit_per_minute`, default 10; 0 disables) — D-0031.

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

Distributed roles run as separate processes over the RabbitMQ dispatcher (`--role=scheduler` /
`--role=worker`, per-region `checks.jobs.<region>`); broker-less regions use the HTTP-pull
`--role=agent` transport. `--role=all` remains the single-process dev mode. All MVP monitor
types are implemented, including ICMP (`internal/prober/icmp.go`, D-0032; unprivileged-first
socket) and push (dead-man's-switch, D-0028).

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
| Exit code 1 on start, `config_load_failed` log | Config path/keys/values; see `docker/config.example.yaml`. |
| Exit code 1, `db_migrate_failed` / `db_connect_failed` | DSN, DB reachability, credentials, migration SQL. |
| Exit code 1, `oidc_init_failed` | Keycloak issuer URL reachable? discovery endpoint correct? |
| `/api/*` returns 401 | Missing/expired session cookie; log in via `/auth/login`. |
| `/auth/callback` 400 `invalid or expired login state` | Flow expired (>10m) or reused; retry login. |
| `/readyz` returns 503 with `database unreachable` | DB down or network; check `cerbix_database_up`. |
| `/readyz` returns 503 during stop | Expected during graceful shutdown. |
| Port already in use | Change `server.listen` in config. |

> Sections for broker outages, scheduler failover, and SLA backfill are added as those
> subsystems land.

## Monitoring as Code file providers (FR-017)

Static `providers.file.<name>` blocks reconcile ProjectBundle YAML into provider-owned
monitors without a restart/reload. Owned by `--role api`/`all` only (scheduler/worker/agent
parse the config but never watch the directory).

**Deployment invariant (HA):** every `api`/`all` replica MUST see identical provider-directory
content (shared read-only volume, identical ConfigMap, or identical git-sync checkout). One
replica per provider applies at a time (a PostgreSQL advisory lock, distinct per provider);
the lock prevents concurrent apply but cannot tell a stale local directory from a fresh one,
so divergent content across replicas is an operator error. Alert if replicas disagree.

**Alerting (fleet-level):** local follower readiness cannot prove an active provider leader.
Alert on `time() - cerbix_file_provider_last_success_timestamp_seconds{provider=...}` exceeding
an operator window (e.g. 3× `resync_interval`), and on a rising
`cerbix_file_provider_bundle_errors{provider=...}` — a persistently invalid or half-written
bundle keeps last-known-good running while the desired generation is rejected (degraded).

**Degraded vs down:** an invalid dynamic bundle, a duplicate-project file, or a temporarily
unreadable directory rejects the desired generation and marks the provider degraded — it never
restarts the process or mutates the committed runtime. A directory that disappears is NOT a
desired deletion (last-known-good is kept, orphaning is suspended). A truly-absent valid bundle
orphans then disables after `orphan_grace_period` (0 = immediate); history is never hard-deleted.

**Diagnostics API:** `GET /api/v1/admin/file-providers` (global admin) returns
`{bundles, providers}` — persisted per-bundle status/last-error/generation AND this process's
live runtime view of every configured provider (leadership, last scan, last success, counts),
including configured-but-idle providers. Leadership/scan times are process-local: query each
`api`/`all` replica to see which one currently leads. `?provider=<name>` narrows to one provider.
`GET /api/v1/organizations/{orgID}/file-providers` (org admin) returns `{bundles}` scoped to that
organization only (no cross-tenant path/error).

**Smoke:** `e2e/mac-smoke.sh` proves the full live lifecycle on a throwaway DB with the process
never restarting: create → scheduler executes the file-managed monitor → in-place semantic
update (generation bump, same DB id) → last-known-good on invalid input → orphan-disable (no
hard delete) → restore (same DB id and push token).
