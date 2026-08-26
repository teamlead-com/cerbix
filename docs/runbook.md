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
make dev-init  # once, and only when no retained base broker volume exists
make dev-up
# Postgres :5432 · RabbitMQ :5672 (mgmt :15672) · Keycloak :8081 · cerbix :8080
```

Available non-production lifecycle gates:

| Topology | Build | Start + ready | Browser/smoke gate | Stop |
| --- | --- | --- | --- | --- |
| Single (`all`, SSO, mail) | `make dev-build` | `make dev-up` | `make dev-test` | `make dev-down` |
| Distributed (`api`/`scheduler`/`worker`) | `make dev-build-distributed` | `make dev-up-distributed` | `make dev-test-distributed` | `make dev-down` |
| Geo central only | `make geo-build` | `make geo-up` | readiness only | `make geo-down` |
| Geo central + geo1/geo2 | `make geo-build` | `make geo-up-all` | `make geo-test` | `make geo-down` |

For a fresh geo broker volume, run `make geo-init` once. Base and geo use separate
`docker/.env.dev` and `docker/.env.geo` pins because their retained RabbitMQ volumes may be on
different upgrade checkpoints. Every `up` refuses a conflicting live topology; switch explicitly
with its `down` goal. `down` never passes `-v` and preserves Postgres, RabbitMQ, and MariaDB data.
The Make facade is not broker-upgrade tooling: use the raw staged procedure below for D-0157.

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

### Stop the outbox owners before migrating across 00088

**This is a step, not a precaution, and the rolling upgrade elsewhere in this document does not cover
it.** Migration 00088 makes the fenced class the database's rule so that a producer of any version
cannot write an incident's events where an old worker would claim them. It fixes ROWS. It cannot fix a
CALL already in flight: a worker that claimed a legacy row seconds before the migration ran is holding
that payload in memory, and nothing in the database recalls an HTTP request or an SMTP session it has
already started (D-0177). The migration replaces such a worker's claim token, so it can no longer
settle the row or release the event behind it — but the delivery it may already have made stands.

So, when upgrading a deployment across 00088:

1. **Stop every role that runs the outbox worker**: `all`, `api` and `scheduler`. A `worker` (prober)
   and an `agent` do not deliver outbox events and can keep running — probing continues, results
   queue, nothing is lost.
2. Wait for those processes to exit. In-flight deliveries finish or fail on their own; either is fine,
   because the rows they did not settle stay claimable.
3. `cerbix migrate` with the NEW binary.
4. Start the new `scheduler`, then `api` (or `all`).

Skipping the stop does not corrupt anything and loses no event. What it risks is one thing —
an incident's events delivered out of order to a webhook or a subscriber, the defect 00088 exists to
end — and the BOUND on it is worth stating plainly, because "once" would be wrong. Each old owner may
already hold a claimed batch of up to 50 rows, and it works through that batch even after losing the
claim-token CAS on a row (it has already sent; the log line is `outbox_cas_lost`). Several
`all`/`api`/`scheduler` replicas may each hold one, and a single `incident_event` fans out to every
webhook and every confirmed subscriber of the pages that surface it. So the honest bound is: one
already-claimed batch per old outbox owner at the moment of the barrier, after which the class fencing
stops those workers claiming any more.

If a deployment cannot take the pause, the honest expectation is that the ordering guarantee begins
after the last old owner exits, not when the migration commits.

### PostgreSQL 15 is a hard requirement (learned in production)

An upgrade to `v0.1.5-beta.1` on **PostgreSQL 14** applies migrations `00061`…`00069` and then dies on
`00070` with `syntax error at or near "("` — five migrations (`00070`, `00080`, `00081`, `00082`, `00084`)
use the column-list `ON DELETE SET NULL (col)` form that arrived in PostgreSQL 15. The plain form cannot
substitute: on a composite FK it nulls EVERY referencing column including the NOT NULL `project_id`,
which is the bug `00070` exists to fix.

Nothing is lost when this happens — `00070` is transactional and rolls back, leaving the database at
`00069` — but the state is a PARTIAL upgrade, and one applied migration matters for it: `00065` sets
`monitors.slug` NOT NULL with no default, and a pre-`00065` binary does not write that column. So a
rolled-back-but-partially-migrated system on an OLD binary **cannot create monitors** until either the
upgrade completes or the constraint is relaxed:

```sql
-- temporary, only while an old binary must keep creating monitors; 00065 re-applies it later
ALTER TABLE monitors ALTER COLUMN slug DROP NOT NULL;
```

`cerbix migrate` now reads `server_version_num` BEFORE the first file and refuses with the version, the
requirement and the fact that nothing was applied. Upgrade the server to 15+ (16 matches every image and
CI job here) rather than working around the syntax.

### RabbitMQ baseline and upgrade

Compose requires `CERBIX_RABBITMQ_IMAGE` on every invocation: no default can safely describe both
an old 3.12 data volume and one already upgraded to 4.3. A fresh base-dev install uses the guarded
initializer, which refuses to overwrite a pin or attach the template to an existing broker volume:

```bash
make dev-init
make dev-up
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

The geo stack has an independent volume and pin. Perform the same queue, feature-flag, clean-stop,
snapshot, and per-hop checks using its own explicit command surface; do not reuse `.env.dev`:

```bash
GEO_DC='docker compose --env-file docker/.env.geo -f docker/docker-compose.geo.yml --profile geo1 --profile geo2'
$GEO_DC exec rabbitmq rabbitmqctl enable_feature_flag all
$GEO_DC stop scheduler api worker-core worker-geo1 worker-geo2
$GEO_DC exec rabbitmq rabbitmqctl stop
# take an offline/storage-consistent snapshot, persist the next image in docker/.env.geo, then:
$GEO_DC up -d rabbitmq
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
2. Roll all workers/agents. Confirm that every credentialed region has a live consumer for
   the carrier generation core will emit into it — a worker consuming
   `checks.jobs.v2.<region>`, or a pull agent whose heartbeat declares
   `credential_envelope` — and that `/readyz` is 200 on executors. Core will not emit a
   generation a region has not proven it can open, so a region that stays silent here goes
   to the §4.4.4 operational rejection rather than to DOWN.
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

### Carrier generations: roll forward, and drain before support is dropped

A carrier generation is the pair of physically separate queues and claim endpoints that
carries one envelope generation: generation 2 (`checks.{jobs,tests}.v2.<region>`,
`/api/v1/agent/v2/*`) carries envelope v1, generation 3 (`…v3…`) carries envelope v2, which
is the one that binds the execution body. Envelope and carrier are numbered separately on
purpose: to every already-deployed executor, generation 2 already MEANS envelope v1, so a
new binding has to arrive on a new carrier rather than redefine an old one.

Three separate things, which this runbook previously ran together:

**1. Rolling forward — executors before core, no manual drain.** Upgrade executors first, as
in "Enable or roll out" above. Nothing else is required: a capable executor consumes and
claims EVERY generation at or below its capability, so a mixed fleet is a supported steady
state and ordinary monitors keep running throughout.

**2. Which generation core emits is an EXISTENTIAL check, not a fleet-wide one.** A region
moves to generation 3 as soon as ONE credential-ready capability-2 executor exists there —
a capability-1 straggler does not hold it back. The consequence to plan for is therefore the
opposite of "the rollout stalls": credentialed work in that region concentrates on the
capable executors, and if the last of them goes away, generation-3 rows sit unclaimed until
its heartbeat ages out (45s) and core falls back to generation 2. Size the capable set
accordingly rather than relying on one upgraded replica.

**3. Dropping support for an old generation is a RELEASE, not an operator action.** The
current binary always consumes every generation it can open; there is no switch that stops
the old consumer. Support is removed by a future version that no longer declares it. What an
operator must do is prove the old generation is drained BEFORE deploying such a version:

1. Compare the capable executor count with the inventory you expect, not with itself:
   `SELECT region, agent_id, capabilities->>'credential_envelope' FROM agent_heartbeats
   WHERE seen_at > now() - interval '2 min' ORDER BY region, agent_id` for pull, and the
   consumer count on `checks.jobs.v3.<region>` against the replica count you deployed for
   AMQP. A consumer count alone says how many are attached, not that every expected worker
   was upgraded.
2. Confirm core has stopped emitting the old generation into that region: no NEW rows appear
   at it over several scheduler ticks —
   `SELECT protocol_version, count(*) FILTER (WHERE expires_at > now()) AS live,
           count(*) FILTER (WHERE expires_at <= now()) AS expired
      FROM pull_jobs GROUP BY 1 ORDER BY 1` — and the same query against `pull_tests`. Both
   tables matter: a Test Connection leaves a `pull_tests` row exactly like a scheduled probe
   leaves a `pull_jobs` one.
3. Wait out the maximum job/test TTL so live rows expire and in-flight AMQP work drains,
   then inspect and explicitly purge `checks.dead` (for example
   `rabbitmqctl purge_queue checks.dead`), or record that any retained old-generation payload
   is intentionally unrecoverable.
4. Expired rows are removed by the leader's housekeeping tick, which runs **hourly** — so
   `live = 0` is not the same as "no rows". Either wait for that tick, or remove only rows
   that have ALREADY expired:
   `DELETE FROM pull_jobs WHERE protocol_version = 3 AND expires_at <= now();`
   `DELETE FROM pull_tests WHERE protocol_version = 3 AND expires_at <= now();`
   Never delete unexpired rows: those are work an executor may still legitimately claim.
5. Prove zero in BOTH tables before deploying the version that drops support, because the
   rollback fence counts them together:
   `SELECT (SELECT count(*) FROM pull_jobs WHERE protocol_version = 3)
         + (SELECT count(*) FROM pull_tests WHERE protocol_version = 3);`

Migration `00063` refuses to roll back while any generation-3 row exists in either table and
reports the total. That refusal is step 5 enforced where a rollback would otherwise discard
pending jobs and in-flight Test Connections.

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
| `CerbixCredentialDispatchUnavailable` or rising `cerbix_secret_resolution_failed_total{reason="no_capable_executor"}` | The scheduler withheld a credentialed job because its authoritative region has no credential-ready executor for the carrier generation core would emit. Check region routing, consumer counts on that region's `checks.jobs.v2`/`v3` queues, pull-agent heartbeat `credential_envelope` capability, and keyring presence. Capability is GENERATIONAL: an executor that only opens envelope v1 is not evidence of readiness for a region core is about to emit envelope v2 into. This is not DOWN. |
| `CerbixCredentialEnvelopeFailures`, executor `/readyz` 503 | A job could not be opened. Readiness degrades on a PERSISTENT mismatch, so a 503 means repeated failures; a single mismatch does not by itself degrade readiness but is still actionable, never expected. During a CORRECT rotation the old key is still in `previous`, so its envelopes open normally and produce no error at all — an `unknown_key_id` means the key was removed too early or a payload was left undrained, which is a real fault. Compare region and key ids across core/executor configs; retain both rotation keys. Values and ciphertext must never be copied into tickets or logs. A successful decrypt restores readiness. |
| Monitor detail shows `last_probe_error` | Typed execution diagnostic only: no heartbeat/status/SLA mutation occurred. A revision-valid live UP or DOWN result clears it; stale or SLA-only results do not. |
| Secret ref bundle is rejected/frozen | Create the named secret in the bundle's project, or correct `password_ref`. The file provider preserves last-known-good and never resolves across projects. |
| PostgreSQL `sslmode=require` | Transport is encrypted but server identity is not verified. Use `verify-ca`/`verify-full` for identity verification. MySQL/Redis ref monitors default to verified TLS; disabling TLS or `tls_skip_verify` is an explicit audited posture. |

Prometheus rules are in `docker/alerts/secret-inventory.rules.yml`. The live smoke
`e2e/secret-inventory-smoke.sh` covers inventory → MaC ref → wrong-key pull-agent degradation
→ correctly keyed recovery/JIT decrypt → real PostgreSQL UP → rotation fence and guards.

## Service reliability operations (FR-021)

The service-reliability subsystem (declared services, duration-weighted facts, durable repair
ranges) exports its operational surface only from the ACTIVE scheduler leader; a deposed
leader clears its gauges on step-down, so exactly one process describes the cluster.

### Metrics

| Metric (type) | Meaning | Action when abnormal |
| --- | --- | --- |
| `cerbix_service_repair_ranges{state}` (gauge) | Sampled queue depth for `pending`, `running` and the terminal `error` state, each an index-backed probe SATURATING at 1000 (a value of 1000 means "at least"). `complete`/`superseded` are never rescanned — they are outcome counters below. | `error` > 0 is TERMINAL parking (evidence gone / unrecomputable): inspect `last_error` in `service_repair_ranges`; the range will never retry itself. Growing `pending` with idle workers → check the leader's slice loop logs (`service_slice_failed`). |
| `cerbix_service_watermark_lag_seconds` (gauge, zero-clamped) | Worst sealed-watermark lag across declared services. Steady state ≈ late-arrival grace + one tick. A NEW service backfilling history legitimately shows a large, shrinking value. | Sustained growth (not a shrinking backfill) means the materializer is not progressing: check leader logs, DB locks, the repair queue. |
| `cerbix_service_slices_total{outcome}` (counter) | Leader slices by `worked`/`empty`/`error`. | A stream of `error` outcomes names its cause in `service_slice_failed` log lines. |
| `cerbix_service_repair_outcomes_total{outcome,reason}` (counter) | Range lifecycle outcomes, attributed by the range reason (`declaration`/`epoch`/`late_data`/`maintenance`/`admin`/`backfill`) — the reason is what tells a repair from a recompute. | Rising `failed` = a persistent fault under backoff; `unrecomputable` = evidence destroyed (see wedged). |
| `cerbix_service_unrecomputable_rejections_total` (counter) | The §21 rejection counter: MUTATIONS refused at preview/confirm because their range is unrecomputable (`ErrUnrecomputableRange`). Distinct from a repair range parking on evidence loss, which shows in the outcomes counter as `unrecomputable`. | Rising rejections mean operators keep asking for restatements retention already ate — check the raw retention window. |
| `cerbix_service_epoch_fanout_total` (counter) | Evaluation epochs CREATED (fan-out of execution-changing writes). | Unbounded growth without monitor edits indicates a writer re-declaring in a loop. |
| `cerbix_service_late_arrivals_total`, `cerbix_service_late_arrival_overflow_total` (counters) | Heartbeats that arrived behind the seal, and example-slot overflows, as events. | A burst after an agent outage is expected — the repairs enqueue themselves. Persistent growth means an agent's clock or route is wrong. |
| `cerbix_service_wedged` (gauge) | 1 while the subsystem is wedged. **Wedged fails the scheduler's `/readyz`, and `cerbix_ready` agrees** (§21). | See below. |

Epoch fan-out and late-arrival counters are persisted WITH their owning transaction (a
rollback takes the delta), then exported by the sampler — monotonic by construction.
Lifecycle-outcome and rejection counters record commit-independent events. The stats sampler
runs off the dispatch loop in its own goroutine, scans only index-backed bounded sets, and
FAILS CLOSED: until its first successful sample (and on any sample failure) the subsystem
reports wedged/unknown and /readyz is not healthy.

### Wedged: definition and recovery

Wedged is bounded and deliberately NARROW — an operator is REQUIRED, by definition:
- **a repair range in state `error`** — terminally parked (typically `ErrEvidenceGone`: raw
  evidence for a sealed bucket was deleted). Recovery: inspect
  `SELECT id, service_id, reason, last_error FROM service_repair_ranges WHERE state='error'`;
  if the range is genuinely unrecomputable, resolve it deliberately (delete the row or
  re-enqueue a narrower range) — the facts it could not restate remain the honest record.

Watermark LAG is deliberately NOT a wedge: a fresh 90-day adoption lags enormously while
progressing normally, and one absolute sample cannot tell that from a stuck materializer.
Alert on lag (below) and read its trend; a progress-across-samples wedge tracker is future
work, recorded in the iteration reports.

`/readyz` recovers by itself on the next stats cadence (≤15s) once the condition clears.

### Restating pre-fix history (iter-0139 carry-in defect)

Facts sealed BEFORE the iter-0139 carry-in fix may carry a wrong pre-first-observation slice
in any bucket whose member had two or more observations before the bucket start (the
evaluator's hold state was undefined and could pick an arbitrary prior row). If a window's
numbers matter enough to restate, enqueue an audited admin repair over the affected range
with the shipped command:

```
cerbix enqueue-service-repair --config /etc/cerbix/config.yaml \
  --project <project-id> --service <service-id> \
  --from 2026-06-01T00:00:00Z --to 2026-07-01T00:00:00Z
```

It goes through the store's own enqueue path — pending same-reason union coalescing, bucket
flooring/ceiling, the audited `admin` reason — and only ENQUEUES: the scheduler leader
executes the recompute under the normal repair machinery, with the fixed carry-in, recording
the before/after movement per §10.6. There is no automatic mass restatement: a correction of
sealed history is an operator decision, not a background job.

### Fact partitions

`service_reliability_buckets` is range-partitioned by month in BOTH storage modes. The leader
pre-creates upcoming months (`ensure_service_fact_partitions_failed` warns on failure); the
DEFAULT partition keeps inserts safe meanwhile. A month whose rows landed in DEFAULT is
adopted automatically on the maintenance cadence, with the PARENT COPY AUTHORITATIVE
throughout: the long phase only COPIES into a standalone staging table (facts never leave
the parent's view; a crash resumes via pg_inherits detection), and the short fenced cutover
(DELETE…RETURNING under the parent lock → distinct-filtered upsert → ATTACH) imposes the
final content under ONE transaction-wide 5s budget — queueing, sweep, attach and commit
together, via the same deadline mechanism as the leader slices. The fenced workload is inherently O(rows still
in DEFAULT for the month) — hole-free incremental shrinking does not exist under native
partitioning — so the supported bound is DECLARED (D-0161, spec §10.11): a month with more
than 100 000 remaining DEFAULT rows is refused, and the bound is enforced TWICE — a cheap
preflight before any parent lock (no doomed all-row DELETE repeats across cadences), and
again UNDER the parent's ACCESS EXCLUSIVE lock before the sweep, because the unlocked count
alone is a TOCTOU against concurrent writers. A refused month stays fully visible and
surfaces through `cerbix_service_fact_maintenance_failing` with the month named in the
repeating `ensure_service_fact_partitions_failed` WARN.

Recovery, precisely:
- **Hot month under the bound**: quiesce the writers keeping it hot (resolve error-state
  repair ranges, stop replaying historical batches into it) and WAIT — the copy resumes from
  the staging's own max key each cadence, monotonically, and the last cadence needs one fence
  window (one transaction-wide 5s budget of parent lock, commit included).
- **Everything else — oversize month, or a month that still fails the fence every cadence
  after quiescence** (the bound is a row count, not a wall-clock guarantee; slow storage can
  exhaust 5s under 100k rows): run the shipped operator command in a maintenance window:

  ```
  cerbix adopt-fact-month --config /etc/cerbix/config.yaml --month 2026-05 --timeout 10m
  ```

  It is the SAME adoption code path with an operator-chosen fence budget and the row gate
  off: month input is validated (`YYYY-MM`), the copy phase holds no parent lock, the fenced
  cutover (lock → authoritative sweep → attach → commit) runs inside `--timeout`, an error
  rolls back leaving the parent authoritative and the staging resumable, and a rerun of an
  already-attached month is a no-op success. The command is covered by an end-to-end test
  against a real migrated database; there is no manual psql procedure to reproduce.

Past months never roll out of reach: the recovery probe adopts the oldest stranded month, one
per cadence, before any current-month work.
Facts are never purged by retention — they are the sealed product; raw heartbeat retention is
governed separately.

### Alerting evaluator: a stall, and what it costs (FR-021 §16.6b)

The two alerting arms run as leader-only sub-cadences: the LIVE signal every 30s, the SEALED
burn signal every 60s. Both export the same fixed, low-cardinality families — labelled by
`signal` (`health`|`burn`) and nothing else. There is deliberately no tenant, service, target or
rule label: this surface is reachable by anyone who can create a service.

| Metric (type) | Meaning |
| --- | --- |
| `cerbix_service_alert_evaluations_total{signal,outcome}` (counter) | Units of work by outcome, `ok`\|`error`\|`skipped`. The unit is what gets a verdict: a SERVICE for `health`, a burn RULE for `burn`. `skipped` is where the burn arm's HOLDs land — a successful evaluation that cannot be quoted. A pass that failed wholesale counts one `error`. |
| `cerbix_alert_delegation_fail_open_total{reason}` (counter) | A monitor alert that PAGED because coverage could not be confirmed, by the clause that failed. `no_owning_service` is the ordinary case — nothing claims this monitor — and the interesting values are the ones saying a replacement exists and went quiet: `stale_lease`, `evaluation_error`, `unroutable`, `onset_pending`, `held`, `generation_changed`, `revision_changed`, `never_evaluated`, `state_not_pageable`, `policy_pages_nothing`, `not_owned`, `no_enabled_target`, `rule_unevaluated`, `onset_undelivered`, `latch_inconsistent`. Three more are OPERATIONAL and have no badge counterpart: `error` (the lookup itself failed — not the same as `evaluation_error`, which is a successful lookup finding a recorded failure), `record_failed` (coverage confirmed, the suppression record could not be written, so it fails open), and `unspecified` (a dis-armed verdict with no reason — a cerbix defect, report it). The clause values come from the same evaluation as the service badge; with SEVERAL candidate services the counter names the furthest miss, which is always one of their badges but not necessarily the one on screen. §16.6b has both tables, with the selection RANK in a column. `stale_lease` on the burn arm means an expired lease and nothing else: a withheld onset reports `unroutable` and then `onset_pending` (D-0178), so a burn `stale_lease` really is the scheduler. `onset_undelivered` is the one to watch after a channel is deleted mid-outage: the page WAS sent and resolved to nobody, so the service stops covering and its members page for themselves until an announcement is received again (D-0179). |
| `cerbix_service_alert_withheld_total{signal,reason}` (counter) | ONSETS a successful evaluation refused to announce (D-0176), by REASON: `unroutable` (nothing could receive it — somebody's paging configuration is broken NOW) or `no_governing_revision` (no declaration governs the service yet — the paging configuration is fine and the declaration has not taken effect). The two have different owners, which is why one number would have sent the wrong person to look. Its own family rather than a fourth `outcome`, because that label partitions the units of work and this is something that did not happen to one. A persistent non-zero rate means somebody's paging configuration is broken, not that the services are fine: the members are paging for themselves meanwhile, and each service announces as soon as a route exists. Distinct from `..._undeliverable_total`, which is an announcement that WAS made to a route that has gone since. |
| `cerbix_service_alert_emitted_total{signal,edge}` (counter) | Alert edges ENQUEUED (`onset`\|`close`) — not deliveries; the outbox owns those. |
| `cerbix_service_incidents_total{action}` (counter) | Incidents a MACHINE opened or resolved for a SERVICE (`opened`\|`resolved`, FR-022). Deliberately NOT folded into the edge counter: an onset for a service whose incident is already open announces WITHOUT opening one, so a persistent gap between `emitted{edge="onset"}` and `incidents{action="opened"}` is a real signal — the open is being refused by the per-service index because something older never resolved. |
| `cerbix_service_alert_active{signal}` (gauge) | Open (unclosed) alert episodes, sampled, saturating at 1000. |
| `cerbix_service_alert_backlog{signal}` (gauge) | Owning services (`health`) and enabled burn targets (`burn`) DUE for evaluation — DB-clock lease expired, or never evaluated. Saturates at 1000. |
| `cerbix_service_alert_last_success_seconds{signal}` (gauge) | Unix time of the last SUCCESSFUL pass. A failed pass leaves it aging on purpose. |
| `cerbix_service_alert_lag_seconds{signal}` (gauge) | How far behind the stalest verdict of that pass was. |

**Incident webhook ordering, and where it stops.** cerbix orders an incident's lifecycle events in
DISPATCH: the outbox will not release an event while an earlier event of the same incident is
undelivered, and the worker calls the deliverer in that order (D-0177). It does NOT promise the order
they ARRIVE in. Delivery is at-least-once and a receiver is a remote system: a request whose worker
lost its lease mid-flight can still land, and a retry can land twice. Every payload therefore carries
`incident.id` and `seq` — unique per event, monotonic per incident, stable across retries. Payloads
from before D-0177 have no `seq` and are delivered as they always were.

What a receiver can build on that is two different things, and they are not interchangeable:

- **Current state, last-write-wins.** Keep the highest `seq` applied per incident and discard
  anything lower. This is one integer and it cannot regress: a retry is idempotent, and a late
  event that arrives after a newer one is dropped rather than applied backwards. It does not
  reconstruct the history — the dropped event is GONE from the receiver's view, so a status page
  built this way is always right about NOW and may never have shown a step it skipped.
- **Exact causal history.** Track the next `seq` expected per incident, buffer anything ahead of
  it, and apply in order as the gaps fill. This preserves every step, and it needs a policy for a
  gap that never fills: an event can be dead-lettered here and will then never arrive, so a
  receiver that waits forever stalls that incident. Bound the wait, then fall forward.

If subscribers report an update for an outage they were never told began, the two candidates are a
receiver doing neither of the above, and an opening event that genuinely never arrived (its delivery
dead-lettered, or in flight from a worker that lost its lease). `seq` tells them apart: the opening
event's `seq` is in the payload of the update they DID get, so a gap is visible at the receiver, and
the outbox's own dead-letter state says which side it happened on.

**The stall.** Symptom: `cerbix_service_alert_lag_seconds` climbing (with backlog that does not
drain), the scheduler failing `/readyz`, and `cerbix_alert_delegation_fail_open_total` rising on
the delivery side. Readiness is the lag: a pass whose lag exceeds **3 × its cadence** — the same
multiplier the evaluator writes its freshness lease with — marks the SCHEDULER not-ready. It
never marks the API not-ready: reads and the public status page are unaffected, and taking the
API out of rotation for an alerting stall would turn a degradation into an outage.

Immediate consequence: coverage DIS-ARMS, so member monitors page for themselves — noisier, not
silent. Nothing is lost while it lasts.

Recovery: restart the leader (a standby takes over; readiness recovers on the next pass inside
the bound). There is nothing to replay — arming is DERIVED from the last verdict's freshness and
generations, never stored as a decision, so a recovered evaluator re-arms by evaluating.

### Suggested alerts

- `cerbix_service_wedged == 1 for 5m` — page: the subsystem needs an operator by definition.
- `cerbix_service_alert_lag_seconds{signal="health"} > 90` (or `{signal="burn"} > 180`) `for 10m` —
  page: the evaluator is stalled and members are paging for themselves.
- `time() - cerbix_service_alert_last_success_seconds > 300` — ticket: an arm that keeps failing
  never updates its lag, so the aging last-success is what catches it.
- `cerbix_service_alert_backlog > 0 for 30m` — ticket: work that never drains at a fixed slice
  cap of 50 means the installation outgrew one leader's cadence.
- `cerbix_service_repair_ranges{state="error"} > 0 for 15m` — ticket.
- `increase(cerbix_service_slices_total{outcome="error"}[15m]) > 10` — ticket.
- `cerbix_service_watermark_lag_seconds > 3600 AND its 1h trend is not decreasing` — ticket:
  a growing lag with no backfill in flight is a stuck materializer.
- `cerbix_service_fact_maintenance_failing == 1 AND time() - cerbix_service_fact_maintenance_last_success_timestamp_seconds > 1800` —
  ticket: a stuck month (see the recovery split above). Before any success, last-success is
  floored at tracking start, so a first-cadence blip cannot trip the 30-minute age.
