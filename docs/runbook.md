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

Supporting 14 was considered and **declined** (D-0183, owner, 2026-08-27): the compatibility path means
emulating column-list `ON DELETE SET NULL` with triggers across five migrations and maintaining them
indefinitely, inside the code path whose correctness the composite foreign keys exist to guarantee. 15
is the floor and there is no plan to lower it, so an operator reading this should budget the server
upgrade rather than wait for a release that removes the requirement.

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

## Notification channels: editing and credential rotation

A channel is edited in place — Settings → Notification channels → **Edit** on the row, or
`PATCH /api/v1/notification-channels/{id}` (editor+) with any of `name`, `config`, `enabled`.
A body naming none of the three is a 400 rather than a silent no-op.

Rotating a credential does NOT mean deleting and recreating the channel any more, so the
monitors, escalation steps and service alert routes that point at it keep pointing at it:

```bash
curl -sS -X PATCH "$CERBIX_URL/api/v1/notification-channels/$ID" \
  -H 'Content-Type: application/json' -b "$COOKIE" \
  -d '{"config":{"bot_token":"NEW-TOKEN","chat_id":"1000"}}'
```

Two rules explain every surprise here:

- **A blank secret keeps the stored one.** `url`, `bot_token` and `smtp_password` are never
  returned by the API (`internal/domain/notification.go`, `SecretChannelConfigKeys`), so an
  edit form is not given them; sending one empty — or omitting it — leaves the stored value
  alone, and only a non-empty value replaces it. Every other key is stored exactly as sent,
  which is how an optional field such as `smtp_username` is cleared.
- **The merged config still has to satisfy the type.** Clearing `chat_id` on a Telegram
  channel is a 400 naming the required keys, and the stored row is untouched — an edit
  cannot leave a channel undeliverable.

The channel TYPE is not editable: another type requires another config, which is a new
channel. A body carrying `type` is refused as an unknown field. Pausing is unchanged —
`{"enabled":false}` keeps the config and delivery skips the channel.

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

### Incident rows the 00090 repair reported but did not fix (D-0182)

Migration 00090 repairs two classes of durable damage and REPORTS two it will not guess at. Both
warnings appear once, in the migration output of the upgrade that crosses it. If you missed them, the
queries are below and are safe to run at any time.

**Member snapshots that MIGHT name a revision that was not yet governing.** The migration prints an
upper bound, not a defect count: the snapshot table stores the member list and no revision id, so a
wrong one cannot be told from a right one by inspection, and most of the reported set is correct.

```sql
SELECT i.id, i.title, i.started_at, s.slug
  FROM incident_member_snapshots ms
  JOIN incidents i ON i.id = ms.incident_id
  JOIN services  s ON s.id = i.service_id
 WHERE EXISTS (SELECT 1 FROM service_definition_revisions r
                WHERE r.service_id = i.service_id AND r.effective_at > i.started_at)
 ORDER BY i.started_at DESC;
```

What to do: nothing, for an incident that is closed and read. The snapshot is a record of who was on
the hook, and a wrong one over-states or under-states the membership by whatever the revision changed
— compare it against `service_definition_members` for the revision you believe governed, and correct
it BY HAND only where you know the answer. Do not derive the revision from `started_at`: that column
is the transaction clock and the evaluator used `statement_timestamp()`, so the two can disagree
across a boundary. That is the whole reason the migration does not do this for you.

**Anchorless open auto-incidents.** Both the monitor and the service are gone, so nothing says what
they were about.

```sql
SELECT id, title, started_at, project_id
  FROM incidents
 WHERE source = 'auto' AND status <> 'resolved'
   AND service_id IS NULL AND monitor_id IS NULL
 ORDER BY started_at;
```

These block nothing — the per-subject uniqueness indexes key on an anchor these rows do not have — so
the only cost is a stale row on the incident list. Resolve them by hand once you are satisfied they
are historical:

```sql
UPDATE incidents SET status = 'resolved', resolved_at = now(), updated_at = now()
 WHERE id = '<id>';
```

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

## Reliability gate operations (FR-024)

The gate (`docs/specs/func-reliability-gate.md`, revision 13; implementation iter-0163) answers one
question for a pipeline: does the error budget of THIS service, for the SLO window its policy names,
allow a release right now. It reads facts cerbix has already sealed and evaluated — the reliability
report, the burn latches, the open auto-incident, the coverage clauses — in ONE `REPEATABLE READ`
transaction, records the decision in a bounded ledger, and computes nothing of its own. The routes
(`internal/api/handlers_gate.go`) and the ledger store (`internal/store/gatedecision.go`, `gateledger.go`,
`gatemaintenance.go`) landed in iter-0163 (FR-024); the metric surface
(`internal/metrics/gate.go`), the CLI (`internal/cli/gate.go`), the `gate.*` keys
(`internal/config/config.go`, `docker/config.example.yaml`), the domain vocabulary
(`internal/domain/gate.go`) and the schema (`internal/store/migrations/00093_reliability_gate.sql`)
are in the tree. Route shapes below are the spec's (D13a, §5).

Two words carry the whole contract. `state` is what was OBSERVED: `ALLOW`, `WARN`, `BLOCK`, `UNKNOWN`,
or `NOT_CONFIGURED` (the service has no policy). `action` is what the pipeline should DO: `ALLOW`,
`WARN` or `BLOCK`; `NOT_CONFIGURED` has none. A known `BLOCK` is never softened by an unknown
neighbour, and an override changes `action` and nothing else.

### The gate says UNKNOWN

`UNKNOWN` is about the FACTS, not about the pipeline or the gate: a clause the policy assigns `block`
or `warn` could not be answered. The token, the network and cerbix itself are fine — cerbix is saying
it does not currently KNOW whether the budget allows the release, and `reasons[]` names every clause
that could not be answered, each with its reason code. A clause assigned `ignore` never produces
`UNKNOWN`, whatever its facts look like.

| Reason code | What it means | What to do |
|---|---|---|
| `seal_stale` | `evaluated_at − sealed_through` exceeds the policy's `max_seal_lag_seconds`: the materializer is behind, and the budget has not been measured since `sealed_through`. Every budget clause is unavailable (D8a). | Open the service page: it shows the same `sealed_through` and `seal_lag` the gate acted on — the report path states both and the gate derives nothing, so the two cannot disagree. Then read `cerbix_service_watermark_lag_seconds` and the FR-021 section above (a stalled leader, a wedged repair range, a stuck fact partition). Do not "fix" it by loosening the policy. The floor of `max_seal_lag_seconds` is **300 s** because a HEALTHY materializer sits at a lag of `[2m, 3m)` — `LateArrivalGrace` 120 s + `CanonicalBucket` 60 s, before queueing and commit — so the floor is that bound plus two buckets of headroom (`domain.MinSealLag`, D8a); the default is 900 s (15 min). A policy at the floor on a healthy stack is NOT `seal_stale`; a lag past the bound is a real fault in the seal pipeline. |
| `facts_stale` | A burn clause's latch is stale: the rule's lease has expired, or no burn evaluation exists for that rule yet. | The burn arm of the alerting evaluator — `cerbix_service_alert_lag_seconds{signal="burn"}`, `cerbix_service_alert_backlog{signal="burn"}`, "Alerting evaluator: a stall" above. A NEW target's rules read `facts_stale` until their first evaluation. |
| `never_sealed` | No fact has been sealed for this service at all: a fresh declaration, or one whose SLI members have never reported. | Wait for the first seal (a new service backfills first); check the service has SLI members and a governing definition revision. |
| `window_target_missing` | The service has no SLA target for the policy's `window` (D2). A policy for a window without a target is refused at write time, so this means the target was deleted afterwards. | Recreate the target for that window, or rewrite the policy for a window that has one. |
| `no_objective` | The window's target exists but carries no objective, so there is no budget to derive. | Set the objective on the target. |
| `budget_withheld` | The report WITHHOLDS the number for this window — the same withholding the service page shows (for example a definition revision spanning the window). A withheld number is a withheld clause, never a zero. | The service page names the withholding reason; resolve that. The gate never quotes a number the page would withhold (invariant 1). |

What happens next is the operator's declared choice, not a default: every policy carries
`unknown_behavior: warn | block` and cannot be created without one (D5). `state` stays `UNKNOWN` — the
word is in the response and on the CLI's stdout line regardless — and `action` becomes `WARN` (exit 0)
or `BLOCK` (exit 2) accordingly. A fresh installation has no sealed facts for hours, which is why the
choice is explicit: a silent `block` breaks onboarding and a silent `warn` is a fail-open nobody chose.

Not `UNKNOWN`: `NOT_CONFIGURED` (no policy — `reason: not_configured` with a documentation link, no
`action`, CLI exit 4) and a 503 (`snapshot_conflict` after the one retry, or `ledger_unwritable`),
where NO decision was made and nothing was recorded — the CLI exits 1 and the pipeline asks again.

### Overrides: lifting a BLOCK for a bounded time

Who: a principal holding `gate:override` — `project_admin` and above in v1 (D12); an API token asks
with the role it already has. A pipeline cannot approve itself: the override is never a field of the
decision request, and any client-supplied actor field on either endpoint is refused as unknown.

Create one against the policy revision you are looking at:

```
POST /api/v1/projects/{p}/services/{s}/gate/override
{"policy_revision": <revision from GET …/gate/policy>, "reason": "<1..500 chars>", "expires_at": "<RFC3339>"}
→ 201 {id}
```

`expires_at` must be after the database's `now()` and at most **7 days** ahead — a hard maximum, not a
default; there is no default. The actor is server-derived and stored twice: the typed attribution the
audit log uses AND an immutable `actor_label` (`token:<name>` for an API token, the user for a person),
so the evidence names who did it for as long as the row exists, after the token is deleted too. The
mutation goes to the tenant audit log.

What it changes: **`action` only — never `state`, never `reasons[]`** (D9). A `BLOCK` under an active
override answers `state: BLOCK`, `action: ALLOW`, `unoverridden_action: BLOCK` and
`override: {id, actor_label, reason, expires_at}`; an `UNKNOWN` whose `unknown_behavior` is `block`
likewise. `WARN` and `ALLOW` are left alone; `NOT_CONFIGURED` is never overridden. The CLI prints
`override=<actor_label>` on its stdout line and exits 0.

How to see it: `cerbix_gate_decisions_total{state="BLOCK",action="ALLOW",overridden="true"}`. The
`state` label stays the truth, so this series is exactly "releases that went out over a blocking
budget" — review it after any incident, and alongside the audit log, which has the override's reason.

Lifecycle, and the 409s a stale screen will meet:

- at most ONE unrevoked override per service: a second `POST` is 409 `override_active`. An expired one
  releases its slot (closed as `expired` under the service lock before the insert);
- a `policy_revision` that is not the current one is 409 `revision_conflict` — read the policy again;
- a policy edit or a policy `DELETE` revokes the active override in the same transaction
  (`revoked_reason: policy_changed` / `policy_deleted`), so an override never outlives a tightening of
  the policy it was granted under;
- revocation is by the override's immutable id, never "the current one":
  `DELETE /api/v1/projects/{p}/services/{s}/gate/overrides/{override_id}` → 204. Revoking an expired,
  already-revoked or superseded override is 409 `override_not_active`, never a silent 204, so a UI that
  held override A while B was created cannot revoke B by accident;
- reads: `GET …/gate/override` is the ACTIVE override (404 `none_active` otherwise); `GET …/gate/overrides`
  is history, the newest 50; `GET …/gate/overrides/{override_id}` is any override with both attribution
  triples and its read-time `status: active | expired | revoked | inert` (`inert` = the policy has moved
  on since it was granted).

From the SPA (iter-0163, D-0207). A project admin creates and revokes overrides from the `Release gate`
card on the service page (`frontend/src/components/ServiceGate.vue`): revocation is by the override's id,
the add form takes the reason and an `until` of at most 7 days, and a stale screen meets the same 409s as
above (`override_active`, `revision_conflict`, `override_not_active`) as a banner with a Reload that
re-reads before any further mutation is allowed. An editor edits the policy from the same card — Save and
Delete send `expected_revision`, and 409 `revision_conflict` is surfaced the same way. The ledger is
browsed at `/gate/decisions` (an explicit range of at most 31 days per page, `?service=` pre-filter,
cursor paging), one record at `/gate/decisions/:id`, the per-service override history at
`/services/:id/gate/overrides`. Nothing else changes: the SPA reads the same routes the CLI does, never
`POST`s a decision, and the audit log holds the reason of every override and policy mutation whichever
door it came through.

### The decision ledger: retention, disk and the maintenance pass

Every decision is one immutable row in `service_gate_decisions` — the gate's own bounded store and the
only new thing FR-024 keeps. Decisions are NOT audit rows: a busy pipeline would bury the tenant audit
log under its own heartbeat. Policy and override mutations ARE audited. A row stores the service slug
and name, the policy snapshot and the full evidence, so it stays readable by id after the service is
renamed or deleted (`service_id` becomes null; the row is cascaded with its project).

The table is RANGE-partitioned by `evaluated_at`, one partition per UTC day, in BOTH storage modes (a
plain partitioned table, no hypertable, and no DEFAULT partition), and **`gate.decision_retention_days`**
(default 90, range 7..365) is enforced by removing WHOLE partitions — never a row `DELETE`. Two
boundaries, both stated (D10), under a healthy pass:

- a row stops being READABLE on the ledger routes when its partition is DETACHED:
  `retention + <1 day + ≤ decision_purge_every` after `evaluated_at`;
- its BYTES exist until the deferred `DROP` one pass later:
  `retention + <1 day + ≤ 2 × decision_purge_every`.

Beyond either boundary is BACKLOG — visible in `cerbix_gate_decisions_partitions_pending_drop`, alerted
on below, and not a bound the product promises while a lock refusal or a lost session can delay a pass.

The pass runs on the scheduler leader in its own goroutine, but its authority is its OWN session-level
advisory lock (slot 3 of the `"cerbix" + slot` namespace; slot 1 is scheduler leadership, slot 2
migrations), and every statement of a pass runs on the one pinned connection that holds it — so a
deposed node cannot detach or drop anything after losing the lock, by construction. Each DDL statement
runs under `lock_timeout = 2s` and `statement_timeout = 10s`; a refusal is logged, counted in
`cerbix_gate_maintenance_errors_total{kind="lock_timeout"|"statement_timeout"}` and retried next pass,
never escalated to a longer wait. Per pass: up to `decision_partition_create_max` days are created
ahead (create standalone + attach, nearest horizon first, keeping `decision_partition_lead_days` of
writable horizon), then up to `decision_purge_max_partitions` removal STAGE-OPS (finalize → drops whose
snapshot gate is open → detaches, oldest first). Steady state consumes two stage-ops a day, so a
retention shortened by many days converges only at the surplus above that: for a large cut, raise
`gate.decision_purge_max_partitions` (or shorten `gate.decision_purge_every`) temporarily rather than
wait, then put it back.

**Disk.** A row is bounded by CHECKs — evidence ≤ 4 KiB, reasons ≤ 1 KiB, policy snapshot ≤ 4 KiB — so
at most ≈ 10 KiB and typically ≈ 1.5 KiB of PAYLOAD. What the §5a bounds allow, per replica:

| | rows / day | rows / 90 d | payload / 90 d typical → bound | rows / 365 d | payload / 365 d typical → bound |
|---|---|---|---|---|---|
| a real installation (≈ 200 decisions/day) | 200 | 18 000 | 27 MB → 180 MB | 73 000 | 110 MB → 730 MB |
| defaults saturated (`60/min` process) | 86 400 | 7 776 000 | 11.7 GB → 78 GB | 31 536 000 | 47 GB → 315 GB |
| hard maximum saturated (`600/min` process) | 864 000 | 77 760 000 | 117 GB → 778 GB | 315 360 000 | 473 GB → 3.2 TB |

Payload is not disk: tuple headers, TOAST, the four indexes per partition (about 240 B of index entries
per 1.5 KiB row) and fill factor sit outside the three JSON CHECKs, so physical size is roughly 2–3×
payload. **Size disk from `cerbix_gate_decisions_bytes`** (the sum of `pg_total_relation_size` over
partitions not yet dropped) **after the first week of real traffic, and from 3× the bound column before
it.** One daily partition at saturated defaults holds 86 400 rows ≈ 130 MB typical and is removed in two
catalog operations. The bounds are process-local by contract: multiply by the `api`/`all` replica count
for a cluster. Raising **`gate.evaluate_rate_process_per_minute`** raises THIS table — read it first.

### The CLI's environment contract

`cerbix gate check --project <id> --service <id> [--json] [--timeout 10s]` performs exactly ONE
`POST …/gate` and exits by the answer's `action`. It is a security and operations surface, not a
convenience (D16, `internal/cli/gate.go`): it opens no database, reads no config file, follows no
redirect.

| Variable | Meaning |
|---|---|
| `CERBIX_URL` | The server base URL (`https://cerbix.example.com`; a reverse-proxy sub-path is tolerated). No credentials, query or fragment. A 3xx is NOT followed — exit 1 with "set `CERBIX_URL` to the final address" — because following it would carry the bearer token to a host the operator did not name, or turn the POST into a GET. |
| `CERBIX_TOKEN` | The API token. **Environment only, never a flag**: flags land in shell history and process lists, and a `--token` flag does not exist. Missing → exit 1, naming the variable. |
| `CERBIX_CA_FILE` | Optional PEM file appended to the system roots. TLS verifies by default (TLS ≥ 1.2); there is no skip-verify option. |

| Exit | Meaning |
|---|---|
| `0` | `action` is `ALLOW` or `WARN` — including `state: UNKNOWN` under `unknown_behavior: warn`; the word `UNKNOWN` is on stdout regardless. |
| `2` | `action` is `BLOCK` — including `UNKNOWN` under `unknown_behavior: block`. Usage errors (a bad flag, a missing `--project`/`--service`, a non-positive `--timeout`) also exit 2: a pipeline that cannot even ask the gate must not proceed either. |
| `4` | `state: NOT_CONFIGURED` — the service has no policy. What to do with that is the integration's visible choice; it is never rendered as `ALLOW` or `WARN`. |
| `1` | Transport, timeout (`--timeout`, default 10 s), TLS, auth, any other 4xx/5xx — including 503 `snapshot_conflict` / `ledger_unwritable`, where no decision was made — a malformed response, and **429**. The CLI does NOT retry a 429 (a pipeline that retries into a rate limit is the load the limit exists to shed): it prints the server's `Retry-After` (whole seconds, `ceil`ed, never below 1) on stderr and exits 1; back off in the pipeline. |

stdout is ONE line — `state=<STATE> [action=<ACTION>] [override=<actor_label>] decision=<decision_id>`
— or, with `--json`, the API response byte for byte (it carries `schema_version`). Every reason and
every diagnostic goes to stderr. Store `decision_id` with the deploy: it resolves later, by id, through
the project-scoped ledger route, after the service has been renamed or deleted, and a replayed id is
visibly old because it carries its own `evaluated_at`.

### Metrics and suggested alerts

Nine families, every label set CLOSED (`internal/metrics/gate.go`): no principal, service or token ever
becomes a label, because this surface is reachable from an authenticated pipeline on every decision,
and a recorder handed a value outside its set records nothing. The four ledger gauges are exported
ONLY by the process that currently holds the gate maintenance session and are cleared when it loses
it — so exactly one process describes the ledger, and a threshold alert on a gauge must be paired with
an `absent()` alert, or a vanished leader reads as "no problem".

| Metric (type) | Labels | Meaning |
|---|---|---|
| `cerbix_gate_decisions_total` (counter) | `state`, `action`, `overridden` | Decisions by observed state and effective action; a `NOT_CONFIGURED` decision has no action and carries `action="none"`. |
| `cerbix_gate_evaluate_rejected_total` (counter) | `reason`: `process_inflight` \| `principal_inflight` \| `process_rate` \| `principal_rate` | Evaluations refused by a process-local bound BEFORE any transaction (a 429: no report, no ledger row, no rate token burnt). |
| `cerbix_gate_evaluate_errors_total` (counter) | `kind`: `snapshot_conflict` \| `timeout` \| `ledger_unwritable` \| `error` | Admitted evaluations that failed. Evaluation errors ONLY — a maintenance failure never moves this family. |
| `cerbix_gate_maintenance_errors_total` (counter) | `kind`: `lock_timeout` \| `statement_timeout` \| `partition_identity` \| `error` | Ledger maintenance statements refused or failed; each retried next pass, never escalated to a longer wait. |
| `cerbix_gate_decision_duration_seconds` (histogram) | — | Wall time of one admitted evaluation, request to decision, fixed buckets 0.05 … 30 s. |
| `cerbix_gate_decisions_partitions_pending_drop` (gauge) | — | Partitions attached past the retention cutoff plus detached but not yet dropped — the backlog. |
| `cerbix_gate_decisions_oldest_partition_age_seconds` (gauge) | — | Age of the oldest attached partition's upper bound; 0 when none is past the cutoff. |
| `cerbix_gate_decisions_writable_horizon_seconds` (gauge) | — | Seconds until the upper bound of the newest attached partition, from the registry and the catalog; the ledger stops accepting decisions at 0. |
| `cerbix_gate_decisions_bytes` (gauge) | — | `pg_total_relation_size` summed over partitions not yet dropped — the number to size disk from. |

| Alert | Severity | Meaning / what to do |
|---|---|---|
| `cerbix_gate_decisions_writable_horizon_seconds < 172800` (2 days) | ticket | The maintenance goroutine or the scheduler leader is gone: the lead of `decision_partition_lead_days` is being consumed and nobody is creating partitions. The gate stops RECORDING when it reaches 0 — and a leader absent that long has already pushed every policy past `max_seal_lag_seconds`, so the gate is saying `UNKNOWN` before it stops recording. Check the scheduler leader and its logs; pair with `absent(cerbix_gate_decisions_writable_horizon_seconds)`. |
| `cerbix_gate_decisions_partitions_pending_drop >= 2 for 6h` | ticket | Removal is being refused by `lock_timeout` pass after pass (see that row), or the pass is not running at all. Steady state is 0 or 1. |
| `cerbix_gate_evaluate_errors_total{kind="ledger_unwritable"} > 0` | **page** | The writable horizon was EXHAUSTED: a decision found no partition for its `evaluated_at`, the API answered 503, the pipeline got no decision. Restore the leader and its maintenance pass; the next pass creates the missing days nearest-horizon-first. |
| `cerbix_gate_maintenance_errors_total{kind="partition_identity"} > 0` | **page** | A relation under our partition name lacks our `cerbix:gate-ledger:<owner_token>` marker, or its shape is wrong, or the registry row is in a state no crash produces. That DAY is left alone until a human looks; the other days proceed, so one bad relation cannot exhaust the horizon by itself. Compare `service_gate_decision_partitions` with `pg_class` and `obj_description` for the named day; do not rename or drop by hand without reading D10. |
| `rate(cerbix_gate_maintenance_errors_total{kind="lock_timeout"}[1h]) > 0` | ticket | Something holds `ACCESS EXCLUSIVE` on the ledger or one of its partitions during maintenance — a manual DDL, a long-running dump, a stuck backup. Find it in `pg_locks` joined to `pg_stat_activity`. |
| `rate(cerbix_gate_evaluate_rejected_total[5m]) > 0` sustained for 15m | ticket | A pipeline is looping into the limit (the CLI never retries a 429 by itself, so this is a wrapper or a fleet). Read the `reason` label: `principal_*` names one token's loop, `process_*` means the replica's whole budget is spent — read the capacity table above before raising anything. |
| `cerbix_gate_evaluate_errors_total{kind="snapshot_conflict"}` rising (`increase(…[15m]) > 0`) | ticket | Serialization failures the single retry is not absorbing — contention on the service's rows (policy writes, overrides, alert latches) while decisions run. A steady rate means a pipeline and an editor are fighting over one service. |

### PostgreSQL ≥ 15.9 is recommended for the ledger (the floor stays 15.0)

Ledger retention is `ALTER TABLE … DETACH PARTITION … CONCURRENTLY` followed, later, by a `DROP TABLE`
of the standalone relation. PostgreSQL 15.9's release notes fix "possible crashes and 'could not open
relation' errors in queries on a partitioned table occurring concurrently with a DETACH CONCURRENTLY
and immediate drop of a partition". cerbix does not RELY on that fix: the drop is deferred to a later
pass and gated on snapshots — it runs only when `detached_at <= now() − decision_purge_every` on the
database clock AND no backend in the database has a transaction older than the detach
(`pg_stat_activity.xact_start`) — a sequence that is safe on every supported 15.x, which is why the
enforced floor (`minServerVersionNum`, 15.0, see "PostgreSQL 15 is a hard requirement" above) does not
move. Running ≥ 15.9 is belt to those braces: it removes the server-side defect class instead of only
sidestepping it. 16 is what every image and CI job here uses.

### The `gate.*` keys

All ten are validated at configuration load; a value outside its range refuses to start, naming the
key and the range (`internal/config/config.go`; the example block in `docker/config.example.yaml`).
Two cross-checks name both keys: `decision_partition_create_max × floor(86 400 / decision_purge_every)`
must be at least 2 (one day's partition plus catch-up) and
`decision_purge_max_partitions × floor(86 400 / decision_purge_every)` at least 4 (strictly more than
the two stage-ops a day of steady state).

| Key (`gate.*`) | Default | Range | Meaning |
|---|---|---|---|
| `evaluate_inflight_process` | 8 | 1..64 | decisions in flight per PROCESS; the (n+1)th is 429 `process_inflight` before any transaction |
| `evaluate_inflight_principal` | 2 | 1..16 | decisions in flight per principal (token id or user id); 429 `principal_inflight` |
| `evaluate_rate_principal_per_minute` | 10 | 1..600 | token bucket per principal: capacity = the value, refill value/60 per second; drained → 429 `principal_rate` |
| `evaluate_rate_process_per_minute` | 60 | 1..600 | token bucket for the process, same algorithm; drained → 429 `process_rate`. Raising it raises the capacity table above |
| `evaluate_tx_budget_ms` | 5000 | 500..30000 | begin-through-commit budget of the decision transaction, through the store's deadline wrapper |
| `decision_retention_days` | 90 | 7..365 | ledger retention by whole-partition removal — readable until detach, on disk until the deferred drop (the two boundaries above) |
| `decision_partition_lead_days` | 7 | 2..30 | days of partitions kept created ahead — the writable horizon the gauge measures against |
| `decision_partition_create_max` | 3 | 1..8 | days created (create + attach) per maintenance pass, nearest horizon first |
| `decision_purge_every` | 1h | 5m..24h | maintenance cadence on the gate's own fenced session; a pass's whole lifecycle fits 30 s (work ≤ 27 s + cleanup ≤ 3 s) |
| `decision_purge_max_partitions` | 8 | 1..48 | removal stage-ops (finalize, drop, detach) per pass; steady state is two a day |

## Change intelligence operations (FR-025)

Change intelligence (`docs/specs/func-change-intelligence.md`, revision 3 with the forward corrections
D-0211 and D-0212; implementation iter-0165) lets a pipeline RECORD that a release, rollback or flag flip
happened to a service, and lets the service's existing sealed facts answer what changed and when (the
timeline), which changes preceded an incident (the correlation) and how the SLI read before and after (the
comparison). It computes no new reliability fact, keeps no deployment catalog, takes no action on any
external system and never says "caused". The routes are `internal/api/handlers_change.go` (limiter
`internal/api/changelimiter.go`), the store `internal/store/change.go` (the comparison lives with the
series owner in `internal/store/servicereport.go`), the CLI `internal/cli/change.go`, the metrics
`internal/metrics/change.go`, the keys `internal/config/config.go` (example block in
`docker/config.example.yaml`), the schema `internal/store/migrations/00094_change_intelligence.sql`.

### The CI token: `role: editor, actions: [gate:evaluate, change:record]`

Create the pipeline's token with the editor role and the two-entry allow-list. What it CAN do: ask the
gate (`POST …/gate`, `cerbix gate check`) and record changes (`POST …/changes`, `cerbix change record`).
What it CANNOT do: anything else — `GET …/services`, the policy routes, the change timeline, the
comparison, the incident routes and every other action are 403 (never 404: the project stays visible,
because visibility is membership and the list narrows actions only). Rules of the list:

- `actions` omitted or `null` — the token's role decides, exactly as every token created before FR-025;
- an entry outside the action catalogue is 400 `action_unknown`; an entry the token's ROLE does not grant
  is 400 `action_not_granted` naming it (D-0212) — a viewer token cannot list `change:record`, and the
  mistake surfaces at the form, not at the pipeline's first 403;
- the list is immutable: `PATCH`/`PUT` of a token are 405. A pipeline that needs one more action gets a
  NEW token — tokens are cheap, audit is not;
- the list is in the token's read model and in the `token.create` audit row; recording a change is NOT an
  audit event (the row itself names `token:<name>` for as long as it exists).

A stolen CI token can record fake changes and ask the gate — nothing else. A fake change blocks nothing
and pages nobody: the timeline is a record, not a control; every row names the token; rate limits cap it.

### A pipeline reports out of order

`409 phase_order` ("succeeded already recorded", or `started` after a terminal) is the PIPELINE's bug, not
cerbix's: the domain owns the order — `started`, then exactly one of `succeeded`, `failed`, `cancelled`; a
terminal alone is accepted (many pipelines can only report the end). Typical causes: two jobs of one run
both reporting a terminal (the second is the 409 — and under a race the per-identity advisory lock
guarantees exactly one lands); a re-run reusing the previous run's `external-id` (use the new run id —
the external id is the change's identity at the source, case-sensitive by design); a retry with a CHANGED
body (`ref` edited between attempts → 409 `phase_exists` naming the field; an identical retry is a 200
replay and never an error). `400 occurred_at_before_start` means the terminal's `--at` precedes the
recorded `started`; `400 occurred_at_out_of_bounds` means `--at` is more than `change.max_past` behind or
`change.max_future` ahead of the server clock (defaults 24 h and 5 min) — check the runner's clock, or
drop `--at` and let it default to the invocation instant. The CLI prints every refusal verbatim on stderr
and exits 2; the pipeline should fail the step and fix itself, not retry.
`cerbix_change_record_rejected_total{reason="phase_order"}` rising is the fleet-level view of the same
thing.

### Reading a comparison: `pending` is not `withheld`

`GET …/changes/compare?source&external_id&horizon` states each side as exactly one of three shapes, and
the difference between the last two matters to an operator:

| Side shape | Meaning | What to do |
|---|---|---|
| a figure (`availability`, `good_seconds`, `bad_seconds`, `unknown_seconds`, `excluded_seconds`, `buckets`) | the sum of the SEALED canonical buckets of that side — the reliability page's own arithmetic for the same range and snapshot; `delta` (after − before, availability points) is present only when BOTH sides are figures | read it. Across time it follows the page's own corrections (repair, reconciliation, definition history) — the contract is parity with the series, not byte stability |
| `{pending: true, sealed_through}` | the side's end exceeds `sealed_through`: the facts are not yet SEALED — not undecidable. `after` is pending while `T + h > sealed_through`; `before` too while `T` itself is past the seal — a change reported minutes ago has exactly this shape (D-0211). Never a partial figure | wait for the seal (a healthy materializer sits 2–3 minutes behind) and ask again once `sealed_through` passes the side's end. If `sealed_through` is not moving, the FR-021 section above owns the fault (a stalled leader, a wedged range) — not this route |
| `{withheld: <reason>, detail?}` | the reliability page would NOT show a number for that range and neither does this: `definition_changed` (a revision or epoch boundary inside the side), `undecidable` (the page's own withholding, the same reason string in `detail`), `no_facts` (no sealed bucket in the range) | this resolves only if the underlying condition does — a different horizon may avoid a boundary; `no_facts` on a young service usually means the range predates its facts. Never read a withheld side as zero |

404 `no_terminal_phase` is a group with only `started` — no before/after until the pipeline reports the
end. `horizon` is one of `15m`, `1h`, `6h`, `24h` (400 `horizon_invalid` otherwise; default `1h`).

### The correlation note and the incident

At a service auto-incident's `opened` delivery the outbox worker links the changes whose latest phase
known at that instant lies within `change.correlation_window` (default 60 min) before the open — on the
incident's own service and on the services the impact graph marks `probable_root` — and appends ONE
system note beside `⚡ Context:`: `🚀 Changes: <n> preceded this incident — <kind ref by source, −<lag>>;
…`, naming at most `change.correlation_note_max` (default 5) and counting the rest. "Preceded" is the
whole claim: cerbix does not know that the deploy caused anything. A change recorded AFTER the open is not
back-linked and a later `resolved` does not recompute; a terminal phase reported after the open rewrites
neither the link nor the note (the group's live phases are shown beside the anchored one). If the
correlation fails or is slow, the incident opens and resolves exactly as before — the error is logged and
counted, and nothing else changes.

### Retention and the identity lock

`change.retention_days` (default 400 — a year and a month, so a quarterly review still has its history)
removes WHOLE change groups whose latest phase is older than the bound, on the scheduler leader's daily
cadence: each statement selects at most `change.retention_groups_per_batch` group keys (default 250) in
`(latest_occurred_at, service_id, source, external_id)` order, takes for each the SAME per-identity
advisory lock `RecordChangePhase` takes, re-evaluates the age under the lock and deletes every phase row
of the keys still old in one transaction — `incident_changes` rows cascade; a group whose `started` is old
but whose terminal is young is not selected. Two consequences worth knowing: the purge WAITS on a held
identity lock, bounded by the maintain pass's context (a cancelled batch rolls back and deletes nothing);
and it holds up to `retention_groups_per_batch` advisory locks per transaction — one lock-table slot each,
out of the shared table sized `max_locks_per_transaction × (max_connections + max_prepared_transactions)`
— which is why the key's ceiling stays at 2 500: size the lock table before raising it anywhere near
that. Capacity: a hundred services each deploying ten times a day with two phases is ~2 000 rows a day,
~7·10⁵ a year — a plain table with two indexes; `cerbix_changes_retained` is the row count. Deleting a
service cascades its changes and links; the incident's `🚀 Changes:` note remains as text.

### Metrics and suggested alerts

Six families (`internal/metrics/change.go`), every label set CLOSED — no service, source or external
identity is ever a label; a recorder handed a value outside its set records nothing.

| Metric (type) | Labels | Meaning |
|---|---|---|
| `cerbix_changes_recorded_total` (counter) | `kind`, `phase`, `outcome`: `recorded` \| `replayed` | Accepted records; an identical replay is counted apart. |
| `cerbix_change_record_rejected_total` (counter) | `reason`: the 400/409 codes \| `body_invalid` \| `process_inflight` \| `principal_inflight` \| `process_rate` \| `principal_rate` | Records refused, by the closed code they were refused with (D-0212: the 429s are counted, or the shed load would be invisible). |
| `cerbix_change_correlations_total` (counter) | `role`: `own_service` \| `upstream` | Links inserted at incident open. |
| `cerbix_change_correlation_errors_total` (counter) | — | Correlations that failed; the incident's delivery proceeded (fail-open). |
| `cerbix_change_compare_total` (counter) | `outcome`: `figure` \| `withheld` \| `pending` | Comparisons served, by the result's shape (`figure` = both sides figures; `pending` = a side not yet sealed; `withheld` = a side withheld for any other reason). |
| `cerbix_changes_retained` (gauge) | — | Rows of `service_changes`, sampled by the leader once per retention pass; cleared on leadership loss — pair any threshold with `absent()`. |

| Alert | Severity | Meaning / what to do |
|---|---|---|
| `cerbix_change_correlation_errors_total > 0 for 15m` (`increase(…[15m]) > 0`) | warn | Correlation is failing at incident open — incidents still open and resolve (fail-open), but their `🚀 Changes:` notes and links are missing. Read the outbox worker's log for the store error; the `⚡ Context:` note on the same incidents tells you whether the delivery itself is healthy. |
| `cerbix_change_record_rejected_total{reason="phase_order"}` rising (`increase(…[1h]) > 0`) | inform | A pipeline reports out of order (the section above). Not cerbix's fault and not an outage: find the pipeline from its own logs — the metric carries no source label by design. |
| `rate(cerbix_change_record_rejected_total{reason=~"process_.*\|principal_.*"}[5m]) > 0` sustained for 15m | ticket | A pipeline is looping into the limiter (the CLI never retries a 429 by itself, so this is a wrapper or a fleet). `principal_*` names one token's loop; `process_*` means the replica's whole budget is spent — read `change.record_rate_process_per_minute` and §5a's capacity note before raising anything. |

### The `change.*` keys

All ten are validated at configuration load; a value outside its range refuses to start, naming the key
and the range (`internal/config/config.go`; the example block in `docker/config.example.yaml`).

| Key (`change.*`) | Default | Range | Meaning |
|---|---|---|---|
| `record_rate_process_per_minute` | 300 | 10..3000 | token bucket, process-wide, for `POST …/changes`; drained → 429 `process_rate` |
| `record_rate_principal_per_minute` | 30 | 1..600 | token bucket per principal (user id or token id); drained → 429 `principal_rate` |
| `record_inflight_process` | 32 | 1..256 | in-flight permits for record; the (n+1)th is 429 `process_inflight` before any bucket is debited |
| `read_inflight_process` | 64 | 1..512 | in-flight permits for the timeline, comparison and incident-changes reads (reads take no rate token) |
| `max_past` | 24h | 1h..168h | `occurred_at` may lag the server clock by at most this (400 `occurred_at_out_of_bounds`) |
| `max_future` | 5m | 0s..1h | `occurred_at` may lead the server clock by at most this |
| `correlation_window` | 60m | 5m..24h | preceding-change window at incident open |
| `correlation_note_max` | 5 | 1..20 | entries named in the `🚀 Changes:` note; the rest are counted |
| `retention_days` | 400 | 30..1460 | age bound on change groups, judged by the group's LATEST phase |
| `retention_groups_per_batch` | 250 | 10..2500 | group keys selected per retention statement (≤ 4 rows each; one advisory lock each) |
