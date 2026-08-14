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

`docker/config.dev.yaml` contains a fixed, public development-only at-rest key so the persistent
local database remains readable after E2E creates encrypted bearer values. Treat it like the other
well-known local credentials: never reuse it or this file outside the disposable development stack.
Production must inject its own random key and follow the rotation procedure below.

```bash
cp docker/.env.dev.example docker/.env.dev  # once; keep the broker image pinned thereafter
docker compose --env-file docker/.env.dev -f docker/docker-compose.yml \
  --profile single --profile sso --profile mail up --build
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

- `all` — every role in one process (dev; inproc transport, no broker needed).
- `api` — REST + SSE, serves the SPA, consumes results, delivers the outbox.
- `scheduler` — leader-elected job scheduler (Postgres advisory lock).
- `worker` — stateless AMQP prober pool (DB-less; region-scoped via `--region`).
- `agent` — DB-less **and** broker-less HTTP-pull prober for broker-less geos (outbound HTTPS only).

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
- **worker pool** — executes the probe (http/tcp/icmp/dns/tls/grpc/websocket/ssh, plus the
  DB/PromQL/RabbitMQ/composite/synthetic probers) with per-check timeout + retries.
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

### RabbitMQ baseline and upgrade

Compose requires `CERBIX_RABBITMQ_IMAGE` on every invocation: no default can safely describe both
an old 3.12 data volume and one already upgraded to 4.3. Fresh installs copy an env template that
pins 4.3 explicitly:

```bash
cp docker/.env.dev.example docker/.env.dev
docker compose --env-file docker/.env.dev -f docker/docker-compose.yml --profile single up -d
```

For retained queues/messages, first set the env file to the image that already owns the volume.
Before every hop, quiesce all Cerbix publishers/consumers, record and accept/drain the remaining
queue depth per the maintenance plan, cleanly stop RabbitMQ, and take a storage-consistent volume
snapshot (or prepare a Rabbit-supported blue/green old-cluster switchback). Never snapshot a live,
mutating broker and call it a rollback point. RabbitMQ does **not** support downgrade or a direct
3.12→4.3 jump.
See the vendor [upgrade guide](https://www.rabbitmq.com/docs/upgrade) and
[release support table](https://www.rabbitmq.com/release-information).

First migrate the Cerbix test-queue shape while the broker is still 3.12:

1. Stop new Test Connection requests and drain/stop every old worker in the region.
2. Run `docker compose --env-file <deployment.env> -f <compose.yml> exec rabbitmq rabbitmqctl
   list_queues name durable auto_delete exclusive consumers messages` and wait until
   `checks.tests.<region>` / `checks.tests.v2.<region>` have no consumers and auto-delete.
   If an empty stale queue remains, delete only that named empty test queue; never delete a queue
   with messages.
3. Deploy the new worker binary against 3.12. Worker readiness now waits for both enabled test
   consumers. Verify each test queue reports `durable=true auto_delete=true exclusive=false`.
4. Run the live v1/v2 gate:

```bash
CERBIX_TEST_RABBITMQ_URL='amqp://user:pass@broker:5672/' \
  go test -race ./internal/dispatch -run TestAMQPRoundTrip -count=1 -v
```

Then advance the broker one supported hop at a time. The commands below use `docker/.env.dev`;
production uses its filled `docker/.env`. Before each next image, enable all stable feature flags,
stop every Cerbix role, cleanly stop the broker, take the offline snapshot, persist the next image
in the env file, and start it. After start, repeat health/feature/queue checks before starting roles:

```bash
DC='docker compose --env-file docker/.env.dev -f docker/docker-compose.yml'
$DC exec rabbitmq rabbitmqctl enable_feature_flag all
$DC exec rabbitmq rabbitmq-diagnostics -q ping
$DC exec rabbitmq rabbitmqctl list_feature_flags
$DC exec rabbitmq rabbitmqctl list_queues name messages consumers durable auto_delete
$DC stop cerbix api scheduler worker
$DC exec rabbitmq rabbitmqctl stop
# take an offline/storage-consistent snapshot now

# Persist the next supported hop in docker/.env.dev, then:
$DC up -d rabbitmq  # 3.12→3.13, then repeat for 3.13→4.2 and 4.2→4.3
```

The env-file update is part of the commit point: every later Compose command must use that same
file. A one-shot shell override is forbidden because the next unqualified command could attempt an
unsupported downgrade against the upgraded volume.

Do not roll an upgraded data directory back by starting an older image. On a failed hop, stop the
new node and restore that hop's volume snapshot with its old image, or switch clients back to the
untouched blue/green cluster. To roll the *application* back across the durable test-queue change,
stop new workers, confirm both test queues are empty, delete those two named queues, then start the
old workers. A disposable dev broker may be recreated only after explicitly accepting loss of its
queued messages.

On 4.3, `checks.tests.<region>` and `checks.tests.v2.<region>` must report `durable=true`,
`auto_delete=true`, `exclusive=false`. Do not enable the deprecated
`transient_nonexcl_queues` compatibility switch: Cerbix no longer needs it. The queue remains
shared by workers in the region and disappears after its last consumer leaves. Its definition,
not an in-flight RPC message, is durable. A repeating
`INTERNAL_ERROR - Feature transient_nonexcl_queues is deprecated` means an old worker binary is
still declaring the former queue shape; finish the worker rollout before treating the region as
ready.

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
organization only (no cross-tenant path/error). Collection keys are stable: an empty result is
`[]`, never `null` or an omitted key.

**Startup wiring:** every role that owns file providers (`api`/`all`) must mount every configured
provider root at the exact static-config path and read-only. The shipped single and distributed
Compose profiles mount `./monitoring.d` at `/etc/cerbix/monitoring.d:ro`. A missing or unreadable
root is a fail-fast `file_provider_startup_failed`: the process cancels and drains started
background work before closing the dispatcher/database and exits non-zero. Fix the mount or
permissions; do not create the directory at runtime or downgrade the provider silently.

**Smoke:** `e2e/mac-smoke.sh` proves the full live lifecycle on a throwaway DB with the process
never restarting: create → scheduler executes the file-managed monitor → in-place semantic
update (generation bump, same DB id) → last-known-good on invalid input → orphan-disable (no
hard delete) → restore (same DB id and push token).

## Project secret inventory and credential dispatch (FR-020)

Secret values are write-only project data. Core materializing roles (`all`, `api`,
`scheduler`) hold the at-rest master and dispatch public configuration; executor roles
(`worker`, `agent`) must hold only their own region's dispatch keyring. Startup rejects an
executor config containing `security.encryption_key` or `previous_keys`.

### Enable or roll out

Use an expand-then-enable rollout; never temporarily send credentialed jobs through v1:

1. Configure `security.dispatch` on every process and set
   `secrets.dispatch_envelope: enforced`, while leaving `secrets.enabled: false`. Core roles
   also need `security.encryption_key`; executor configs must not contain it.
2. Roll all workers/agents. Confirm worker v2 consumers or pull-agent capabilities are live
   for every credentialed region and `/readyz` is 200 on executors.
3. Roll core roles, then set `secrets.enabled: true`. The Secrets API and `*_ref` writes are
   unavailable until this final switch; there is no accepted-but-undispatchable legacy mode.

`security.dispatch.default` is a single trust-domain fallback. Combining it with explicit
regional keyrings requires `shared_trust_acknowledged: true`; this deliberately sets
`cerbix_dispatch_shared_trust 1` and fires the informational posture alert.

### Rotate the at-rest master

1. Put the new key in `security.encryption_key` and retain the old key in
   `security.previous_keys` on every core role. Do not place either on executors.
2. Roll core roles so all readers can decrypt old rows while writers use the new primary.
3. Run `cerbix reencrypt --config <core-config>`. A zero exit is a bounded fixed-point proof
   that no `project_secrets` row remains under an old key; an exhausted convergence budget
   exits non-zero. Exact-ciphertext CAS prevents a concurrent secret rotation from being
   overwritten. The command also re-encrypts the older secret-bearing tables.
4. Remove the old key only after the command succeeds and every core replica has the new
   config. Run the command again after concurrent rotations if it reports non-convergence.

### Rotate a regional dispatch key

At-rest `reencrypt` does not touch broker or pull payloads. For one region:

1. Deploy `{primary: new, previous: [old]}` to materializers and executors. Executors must
   receive the overlap before materializers begin sealing with the new primary.
2. Verify executor readiness and that
   `cerbix_executor_probe_error_total{reason=~"unknown_key_id|decrypt_auth_failed"}` is not
   increasing.
3. Retain the old key for at least the maximum job/test TTL and until pull leases have
   drained. Also inspect and explicitly purge `checks.dead` (for example
   `rabbitmqctl purge_queue checks.dead`) or record that any retained old-key poison payload
   is intentionally unrecoverable.
4. Only then remove the old entry from `previous` everywhere.

A leaked regional key opens all retained payloads for that region until ACK/TTL/dead-queue
purge. Rotating a Cerbix inventory value fences results through `execution_revision`, but it
cannot recall a job already materialized; immediate revocation requires changing the
credential at the monitored target.

### Diagnose failures

| Symptom | Meaning / action |
| --- | --- |
| `CerbixCredentialDispatchUnavailable` or rising `cerbix_secret_resolution_failed_total{reason="no_capable_executor"}` | The scheduler withheld a credentialed job because no v2 credential-ready executor exists in its authoritative region. Check region routing, worker v2 queue consumers, pull-agent heartbeat capability and keyring presence. This is not DOWN. |
| `CerbixCredentialEnvelopeFailures`, executor `/readyz` 503 | A job could not be opened. Compare region and key ids across core/executor configs; retain both rotation keys. Values and ciphertext must never be copied into tickets or logs. A successful decrypt restores readiness. |
| Monitor detail shows `last_probe_error` | Typed execution diagnostic only: no heartbeat/status/SLA mutation occurred. A revision-valid live UP or DOWN result clears it; stale or SLA-only results do not. |
| Secret ref bundle is rejected/frozen | Create the named secret in the bundle's project, or correct `password_ref`. The file provider preserves last-known-good and never resolves across projects. |
| PostgreSQL `sslmode=require` | Transport is encrypted but server identity is not verified. Use `verify-ca`/`verify-full` for identity verification. MySQL/Redis ref monitors default to verified TLS; disabling TLS or `tls_skip_verify` is an explicit audited posture. |

Prometheus rules are in `docker/alerts/secret-inventory.rules.yml`. The live smoke
`e2e/secret-inventory-smoke.sh` covers inventory → MaC ref → wrong-key pull-agent degradation
→ correctly keyed recovery/JIT decrypt → real PostgreSQL UP → rotation fence and guards.
