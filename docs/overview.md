# cerbix — overview: binary, architecture, stack, deployment

Internal availability and SLA monitoring service for a multi-tenant
environment: organization → project → monitor. A single Go binary runs in different roles,
scales horizontally via RabbitMQ, stores data in Postgres (+TimescaleDB),
authenticates via OIDC (Keycloak) and/or local passwords, and serves an SPA (Vue 3).

The document has two parts:
1. **Binary, architecture, stack, and rationale.**
2. **Map of the preferred deployment.**

---

# Part 1 — Binary, architecture, stack

## 1.1 Commands of the `cerbix` binary

A single binary; behavior is selected by subcommand and flags.

| Command | Purpose | Flags |
|---|---|---|
| `cerbix serve` | Start the operational server in the selected role. | `--config <path>` (required), `--role all\|api\|scheduler\|worker\|agent` (default `all`), `--region <name>` (worker/agent pool, default `core`) |
| `cerbix migrate` | Apply DB migrations (goose, embedded) and exit. | `--config <path>` (required; needs `database.dsn`) |
| `cerbix reencrypt` | Re-encrypt all at-rest secrets under the current primary key (after key rotation). | `--config <path>` (required; needs `security.encryption_key` and `database.dsn`) |
| `cerbix version` | Print build info (version, commit) as JSON and exit. | — |
| `cerbix help` / `-h` / `--help` | Usage. | — |

**Flags in detail:**

- `--config <path>` — path to the strict YAML config. The config is **fail-fast**: unknown keys
  and invalid values abort startup (no self-healing). Required by all commands
  except `version`/`help`.
- `--role` (only `serve`) — process role. The same binary executes different parts of
  the system; in production, roles are separate deployments scaled independently.

### `serve` roles

| Role | What it runs | Job transport | Needs DB | Needs RabbitMQ |
|---|---|---|---|---|
| `all` | Everything in one process: scheduler + worker + ingest + API/SPA + outbox. | `inproc` (channel) | yes | no |
| `api` | HTTP: REST + SSE + SPA serving; the check-result consumer (→ heartbeats/statuses/incidents); outbox delivery. | AMQP | yes | yes |
| `scheduler` | Leader scheduler (Postgres advisory lock): publishes due jobs; rollup/retention; renotify; burn-eval; SLA reports; region-worker-alert; escalation-advance. | AMQP | yes | yes |
| `worker` | Prober pool: pulls jobs, executes the probe with a timeout, publishes the result. Stateless. | AMQP | no | yes |
| `agent` | HTTP pull prober for geos without a broker: pulls its region's jobs over HTTPS, probes, posts results. DB-less, broker-less. | HTTP-pull | no | no |

Every role starts an operational HTTP server with `/healthz`, `/readyz`, `/metrics`. `all` is for local
development (`docker compose`); the broker-backed distributed roles (`api`/`scheduler`/`worker`)
require `rabbitmq.url`; `agent` needs neither a broker nor a database.

## 1.2 Configuration (strict YAML)

Top-level `Config` sections (`internal/config`):

| Section | Keys (main) | Purpose |
|---|---|---|
| `server` | `listen`, `healthz_path`, `readyz_path`, `metrics_path`, `trusted_proxy_count`, `trusted_proxy_cidrs` | Address/paths of the operational endpoints; reverse-proxy trust for the rate-limiter client IP (CIDRs supersede the hop count when set — D-0139/D-0143). |
| `log` | `level`, `format` | `log/slog`, JSON. |
| `database` | `dsn` | Postgres (pgx). Empty → scaffold mode without a DB. |
| `rabbitmq` | `url`, `management_url` | AMQP for distributed roles (+ management API for worker-liveness alerts). |
| `oidc` | `issuer`, `client_id`, `client_secret`, `redirect_url`, `scopes`, `button_label`, `post_logout_redirect_url`, `bootstrap_admin_emails` | Bootstrap OIDC (overridable from the UI, see instance-settings). |
| `local` | `enabled`, `min_password_length`, `login_rate_limit_per_minute` | Local login (password + TOTP), brute-force limit. |
| `session` | `cookie_name`, `ttl`, `secure` | Server-side sessions (cookie). |
| `prober` | `allow_private_ips`, `allow_metadata_ips` | SSRF guard: what workers are allowed to resolve. |
| `notification_egress` | `allow_private_ips`, `allow_metadata_ips` | SSRF guard for **alert delivery** (webhook/notify/SMTP), independent of `prober`; defaults **deny-private** (D-0141/iter-0084). |
| `result` | `allowed_skew`, `revision_mode` | Result-ingest contract: future-clock skew bound + `execution_revision` gate policy (`enforce`\|`observe`; default `enforce`) — D-0142 (`specs/func-result-protocol.md`). |
| `heartbeats` | `retention_days` | Retention period for raw heartbeats (partitions are dropped by the leader). |
| `security` | `encryption_key`, `previous_keys`, `admin_email`, `admin_password` | AES-256-GCM keyring for at-rest secrets + rotation; global-admin bootstrap on an empty system. |
| `mail` | `smtp_host`, `smtp_port`, `smtp_username`, `smtp_password`, `from`, `public_base_url` | Bootstrap SMTP (overridable from the UI). |
| `pull` | `regions`, `token`, `agents`, `server_url` | HTTP-pull transport: broker-less regions (server side) and agent credentials (agent side). |
| `providers` | `file.<name>.{directory, debounce, resync_interval, orphan_grace_period, scope, limits}` | Monitoring-as-Code file providers (FR-017, spec `func-monitoring-as-code`). Static definition; contents hot-reconciled by `api`/`all`. Scope `instance\|organization\|project` (no implicit scope). |

Many settings (OIDC, branding, auth-policy, alerting, monitor-defaults, mail), once first
saved in the UI, live in the DB (singleton `instance_settings` / `oidc_settings`) and **override**
the config file; the file remains a bootstrap seed. See `decisions.md` D-0082..D-0084.

## 1.3 Architecture

Probes do not block the API: the scheduler decides "what and when", workers execute, ingest writes
the result, the API serves it. The transport between them is the `Dispatcher` abstraction (`inproc` for dev,
`rabbitmq` for production).

```mermaid
flowchart LR
  subgraph edge[ ]
    SPA[Vue 3 SPA]
    KC[(Keycloak OIDC)]
  end
  SPA -->|REST + SSE| API
  KC -. login .-> API

  subgraph app[cerbix binary: roles]
    API[api: REST/SSE/SPA + ingest + outbox]
    SCHED[scheduler: leader, heap next_run]
    WRK[worker: prober pool]
  end

  SCHED -->|publish CheckJob| MQ[(RabbitMQ<br/>checks.jobs / checks.results)]
  MQ -->|consume job| WRK
  WRK -->|publish result| MQ
  MQ -->|consume result| API
  API --> PG[(Postgres + TimescaleDB<br/>heartbeats, rollups, state)]
  SCHED --> PG
  API -->|outbox| OUT[[notify / webhooks / email]]
```

**Components (`internal/` packages) and their role:**

| Package | Role |
|---|---|
| `cli` | Entry point, command/flag parsing, role wiring, graceful shutdown. |
| `config` | Strict YAML, `Validate()`, single snapshot. |
| `httpsrv` | Operational server `/healthz` `/readyz` `/metrics`. |
| `api` | REST handlers (orgs/projects/monitors/incidents/sla/status-pages/settings…), SSE, secret redaction, SPA serving; global-admin surface `/api/v1/admin/*` (users, outbox dead-letter). |
| `auth` | OIDC login (live-reconfigurable, atomic provider), local login + TOTP, sessions, JIT provisioning, middleware. |
| `authz` | Roles and `Can(user, action, scope)`; tenant isolation. |
| `store` | pgx + goose migrations; repositories; encryption of secret fields. |
| `domain` | Models and invariants (`Validate()`), rules without I/O. |
| `dispatch` | `Dispatcher` interface + `inproc`/`amqp` implementations. |
| `scheduler` | Leader (advisory lock), min-heap `next_run`, job publishing; rollup, retention, renotify, burn-eval, SLA reports. |
| `worker` | Goroutine pool, probe execution with `context.WithTimeout`. |
| `prober` | `Prober` registry by monitor type + conditions engine; SSRF guard. |
| `ingest` | The CHECK-RESULT consumer — heartbeats, status flip (atomic), auto-incidents, transitions into the outbox. The package name is historical: cerbix ingests its OWN probe results and no external telemetry, which is why the product is not an observability backend. |
| `sla` | SLI/SLO/error-budget/burn-rate for a MONITOR (pure computations). |
| `reliability` | Reliability of a **service** (FR-021): a piecewise reducer over breakpoints producing duration-weighted facts on two conserved axes. Pure — no I/O, no clock. |
| `incidents`↔`api`/`store` | Incidents, timeline updates, postmortems, external-key correlation (Alertmanager). |
| `statuspage`/`feed`/`subscribe` | Public status pages, RSS/Atom/JSON feeds, subscribers. A component renders from ONE of three sources under a discriminator (`monitor` / `service` / `manual`, FR-021 §15.0) with the replaced binding kept dormant for revert; the page summary reports worst-of-MEASURED plus an unmeasured count, and measurement ABSENT is the public status `no_data` — never `operational`. |
| `notify` | Delivery of transitions/alerts to channels (webhook/Slack/Telegram/email). |
| `webhook` | Outbound incident webhooks (signed). |
| `outbox` | Transactional outbox: durable event delivery with retry/backoff/dead-letter, `SKIP LOCKED`. |
| `settings` | Instance settings (branding/auth-policy/alerting/monitor-defaults/mail) — atomic snapshot, resolver DB→config→defaults. |
| `secret` | AES-256-GCM keyring (encrypt/decrypt, rotation). |
| `mailer` | SMTP sending (live, resolves settings per-send). |
| `events` | In-process broker for SSE realtime. |
| `totp` | RFC 6238 (2FA). |
| `metrics`/`logging`/`buildinfo` | Prometheus (`cerbix_*`), slog JSON, build info. |
| `web` | `embed.FS` with the built SPA. |

**Services (FR-021, phase 1).** A `Service` is the unit whose reliability is *explicitly
declared*, and it sits BESIDE monitors rather than above them: a monitor may contribute to
several services' reliability inputs, or to none. Two axes are stored separately — a
`definition_revision` is what a human declared availability to MEAN, an `evaluation_epoch` is
what the system was MEASURING — and a fact references the epoch alone. Facts are sealed
bucket by bucket, and the watermark `sealed_through` is defined by **contiguity**, so a gap
holds it back instead of being jumped over. Phase 1 ships the declaration, the epoch, the seal
and the file-provider format-2 surface; availability over a window, the error budget and the
burn rate are phase 2 and are **absent** from the API rather than returned as zeros.

**Alerting ownership (FR-021, phase 5).** A service can be the thing that pages, and its declared
SLI members can stop delivering their own alerts for the same failure — but `owns_paging = true`
silences nothing on its own. Suppression is per SIGNAL (a LIVE health transition, a SEALED burn
breach), per POLARITY (only onset-like events; a recovery or a burn CLEAR is never suppressed), and
only while a replacement for THAT signal is demonstrably ARMED: a policy that can page the current
state, a quotable last verdict for every declared rule, the current generations and effective
revision, a fresh DB-clock lease, and a recipient that resolves right now. Anything ambiguous FAILS
OPEN — the member pages — because a page that was not needed is noise and a page that was owed and
never sent is the failure the design exists to prevent. Facts, status flips and history are
untouched by all of this: suppression applies to DELIVERY only. (This sentence used to say
"incidents" too, and FR-022 made that half false — see below. What stays true is the part
suppression is about: a member's incident is neither opened, resolved nor annotated differently
because a service covered its page.) An announcement that ends
carries WHY it ended, and only a genuine return to health is called `recovered`; ownership turned
off, a policy that no longer covers the state, a target disabled, a rule removed and a service
deleted each say so in their own words, from a durable episode that outlives what fired.

**Escalation for services (FR-023).** A service with an escalation policy escalates its OWN
auto-opened incident: the policy's steps fire from the incident's start, progress latches on the
incident so a step fires once, acknowledgement or resolution ends it, and every step names the
SERVICE. Two rules are worth knowing before reading the code. The ladder **fails closed** where
delegation fails open — a missing, stale or unreadable live verdict does not advance a step, because
ambiguity at delivery time means a page EXISTS while ambiguity in a ladder would mean a page
MULTIPLIES on a state nobody can currently confirm. And the service GRAPH does **not** pause it,
against the obvious symmetry with the monitor dependency pause: §14's impact graph annotates and
links, never suppresses, and a graph sold as advisory does not become a suppression mechanism because
a second feature found it convenient. A service's own step is never suppressed by delegation either —
that step IS the page delegation exists to redirect.

**Service incidents (FR-022).** A `Service` can be the SUBJECT of an incident, not merely a thing
that pages. An incident has AT MOST ONE anchor — a monitor or a service, enforced by a CHECK so the
discriminator every read path branches on cannot be ambiguous, while a manual project-level incident
with neither keeps working. A live service alert OPENS one automatically, in the SAME transaction as
the announcement, under the same three gates that decide whether it pages at all (a LIVE onset,
ARMED coverage, `confirm_evaluations`); the close RESOLVES it in the transaction that ends the
announcement. A burn breach opens none, ever — a budget signal trails the seal and says nothing about
now. At most one open auto-incident per service, so a flapping service cannot accumulate them; a
service that recovers and fails again gets a second one, which is what the resolve exists to make
possible. `source = 'auto'` is in the resolve's WHERE, so a machine never overwrites a conclusion a
person drew. It carries its impact links through the same service graph and a snapshot of the
members the service had AT OPEN time, which keeps naming a member deleted since. Deleting the service
clears the anchor and NOT the tenant key: the incident survives as a project-level record with its
timeline intact. What does NOT change: a monitor incident's lifecycle, notes, escalation, rendering
and postmortem (NFR-017); the `⚡` and `⏸` system notes keep their one home, the MONITOR's incident;
and an open service incident moves no component STATUS on a status page — it is rendered as an
incident, next to a status the §15.0 precedence table still derives from health alone.

**Catalog of check types (`prober`):** `http`, `tcp`, `icmp`, `dns`, `tls`, `grpc`, `postgres`,
`mysql`, `redis`, `rabbitmq`, `promql`, `websocket`, `ssh`, `composite`, `push` (dead-man's-switch).
HTTP-like types use the declarative conditions engine
(`[STATUS] == 200`, `[RESPONSE_TIME] < 500`, `[BODY].status == "UP"`).

**HTTP surface:** `/healthz`, `/readyz`, `/metrics` (operational); `/api/v1/*` (behind auth middleware);
`/api/v1/public/*` (status pages, feeds, subscriptions, push, branding — no login); `/auth/*`
(login/callback/logout/local/config); `/` — SPA (`embed.FS`).

## 1.4 Stack and rationale

The reference point is company conventions (Go services `example-svc`), plus lessons from OSS analogs.

| Component | Why chosen | Role in cerbix |
|---|---|---|
| **Go 1.25** | Company convention; static binary, cheap concurrency (goroutines), simple deployment. | The entire backend; one binary for all roles. |
| **stdlib `net/http` (ServeMux 1.22+)** | Method+path routing landed in the stdlib — no external router needed, fewer dependencies. | HTTP routing for API/auth/public. |
| **Postgres + `pgx/v5`** | Reliable relational database; pgx is a fast native driver with pooling. | State, heartbeats, settings, sessions. |
| **TimescaleDB (adaptive)** | Hypertable+compression "for growth" without a hard dependency: RDS/plain PG work without the extension. | Extension present → `heartbeats` = **hypertable** (daily chunks on-demand, native compression segmentby=monitor: ~10–20× disk, retention via `drop_chunks`); absent → **regular RANGE partitions** (daily) with manual maintenance. The mode is detected at startup (migration 00043 guarded); the `heartbeats_daily` rollup is identical in both. Revisit trigger for a dedicated TSDB: >100k monitors / intervals <10s / raw retention >1 year. |
| **goose (embedded)** | Versioned SQL migrations inside the binary; `cerbix migrate` and auto-apply at startup. | DB schema. |
| **RabbitMQ (`amqp091-go`)** | Existing cluster in the company infrastructure; durable queues, prefetch = backpressure; horizontal worker scaling. | Regional/versioned `checks.jobs.*` plus shared `checks.results` transport between scheduler↔worker↔api. |
| **`Dispatcher` (inproc\|amqp)** | An abstraction so that dev/tests don't require a broker. | Dev — `inproc`; prod — RabbitMQ. |
| **Postgres advisory lock (leader election)** | No etcd in the stack; cheap, no new dependencies; survives leader failure. | The single active scheduler. |
| **OIDC: `coreos/go-oidc` + `x/oauth2`** | Keycloak already exists; standard OIDC (Authorization Code + PKCE), ID-token/JWKS validation. | AuthN; authorization lives in the DB (own role model Global/Org/Project). |
| **Transactional outbox** | Guaranteed delivery of notifications/webhooks without dual-write; `SKIP LOCKED` — safe on N replicas. | Reliable notifications and incident webhooks. |
| **AES-256-GCM keyring (`secret`)** | Channel/webhook/OIDC/SMTP secrets, monitor config secrets, TOTP secrets, and push tokens (blind-indexed for lookup) must not be stored in plaintext; rotation support (`cerbix reencrypt`). | At-rest secret encryption. |
| **SSRF guard (`prober`)** | Probes hit user-supplied URLs — protection from access to metadata/internal IPs. | Validation of the resolved connect IP. |
| **Prometheus + `slog`** | Company convention; observability. | `cerbix_*` metrics, structured logs. |
| **Vue 3 + Vite + TS + Pinia + Tailwind** | The company's first frontend — we set the precedent; fast DX, type safety (TS client from OpenAPI). | SPA: dashboards, monitors, incidents, SLA, settings. |
| **OpenAPI → `openapi-typescript`** | A single contract, a generated type-safe client. | Frontend/backend synchronization. |
| **nginx (unprivileged) / `embed.FS`** | Two ways to serve the SPA: an nginx layer or the same Go binary. | Static serving + proxying `/api`,`/auth`. |

---

# Part 2 — Map of the preferred deployment

## 2.1 Local development (`docker/docker-compose.yml`)

One `--role=all` process + infrastructure:

| Service | Image | Port | Role |
|---|---|---|---|
| `postgres` | `timescale/timescaledb:2.17.2-pg16` | 5432 | DB + TimescaleDB |
| `rabbitmq` | `${CERBIX_RABBITMQ_IMAGE:?...}` (fresh env template pins `rabbitmq:4.3-management`) | 5672 / 15672 | Broker (not used in dev with `all`) |
| `keycloak` | `quay.io/keycloak/keycloak:18.0.0` (`start-dev`) | 8081 | OIDC IdP |
| `cerbix` | build from the repository root | 8080 | `serve --role all` (API + SPA embed) |

The SPA is served by the binary itself from `embed.FS` on :8080 — no separate nginx layer is needed. **The image
is self-contained:** `docker/Dockerfile` is a root-context multi-stage build (node builds the SPA → the Go stage
embeds `dist` into the binary → distroless), so `make dev-build` builds both the frontend and the backend into
one image. `make dev-up`, `make dev-up-distributed`, and `make geo-up-all` are the fixed non-production topology
entrypoints; base and geo retain separate RabbitMQ image pins and cannot run concurrently on their fixed ports.
For local development with hot-reload —
`make -C frontend dev` (Vite server on :5173).

## 2.2 Preferred production (distributed)

The same binary in three independently scalable deployments behind a reverse proxy. Data and the broker —
in HA.

```mermaid
flowchart TB
  U[Users / CI bots] --> LB[Reverse proxy<br/>nginx / Traefik + TLS]
  LB --> FE[SPA static assets<br/>nginx or embed]
  LB -->|/api /auth| API1
  LB -->|/api /auth| API2

  subgraph apis[api-tier: N replicas, stateless behind LB]
    API1[cerbix --role api]
    API2[cerbix --role api]
  end

  subgraph sch[scheduler-tier: 2 replicas]
    S1[cerbix --role scheduler<br/>LEADER]
    S2[cerbix --role scheduler<br/>standby]
  end

  subgraph wrk[worker-tier: M replicas, grow with load]
    W1[cerbix --role worker]
    W2[cerbix --role worker]
    W3[cerbix --role worker]
  end

  API1 & API2 & S1 & W1 & W2 & W3 --- MQ[(RabbitMQ 4.3<br/>cluster)]
  API1 & API2 & S1 & S2 --- PG[(Postgres 16 + TimescaleDB<br/>primary + replica)]
  API1 & API2 -. OIDC .- KC[(Keycloak)]
  S1 <-. advisory-lock<br/>leader election .-> S2

  API1 --> OBS[(Prometheus /metrics<br/>+ slog logs)]
```

**Topology and scaling rules:**

- **api-tier** — N stateless replicas behind a load balancer (SSE status stream, consumption of
  `checks.results`, outbox delivery). Grows with RPS/number of viewers. As realtime grows —
  insert Redis pub/sub between replicas.
- **scheduler-tier** — run 2 instances for HA; one is active (advisory lock), the second
  takes over when the leader fails. Not scaled by count (there is one leader).
- **worker-tier** — M stateless replicas, **grow horizontally with load**; prefetch on the queue
  provides backpressure and even distribution. No DB required.
- **PostgreSQL 15+ required, 16 used everywhere here** — the schema uses the column-list `ON DELETE SET NULL (col)` form from PG15 in five migrations; `Migrate` refuses an older server before applying anything.
- **Postgres 16 (TimescaleDB image)** — primary + streaming replica; time series on regular
  RANGE partitions + the `heartbeats_daily` rollup, retention drops old partitions (leader).
- **RabbitMQ 4.3** — preferred cluster after the staged upgrade; durable regional
  `checks.jobs.<region>` / `checks.jobs.v2.<region>` queues plus shared
  `checks.results` / `checks.dead` and auto-delete regional test queues.
- **Keycloak** — OIDC IdP (realm/client `cerbix`). Local login remains as a lockout fallback.
- **Secrets** — `security.encryption_key` from a secret manager/environment variable; rotation via
  `cerbix reencrypt`.
- **Observability** — `/metrics` (`cerbix_*`) from all roles into Prometheus; `/healthz`/`/readyz` for
  probes.

**Rollout order:** `cerbix migrate` (one-off) → scheduler → workers → api. Every role
fails fast on an invalid config/unreachable DB. **Important for distributed deployment:** run
`cerbix migrate` **as a separate one-off step before** starting the roles. Roles auto-apply migrations at
startup, and on the first deploy of a new migration, simultaneously starting roles race for it (goose does not
take a cross-process lock) — one applies it, the others fail with `relation already exists`. After
the one-off `migrate`, roles see the current version and simply skip this step.

## 2.3 Traefik + embed (recommended default edge, no nginx)

A separate nginx for serving the SPA is **not needed**: the Go binary (role `api` or `all`) serves on a single
listener (`server.listen`, :8080) the API, `/auth`, the public pages, and the **SPA from
`embed.FS`** itself (catch-all `/` with SPA fallback to `index.html`). It is enough to put
Traefik in front of it for TLS termination and route all traffic to **one backend** — no path splitting.

```mermaid
flowchart LR
  U[Client] -->|HTTPS| T[Traefik<br/>TLS / ACME certificate]
  T -->|all traffic, one service| API[cerbix --role api :8080<br/>API + auth + public + SPA embed]
  API --> PG[(Postgres)]
  API --- MQ[(RabbitMQ)]
```

Minimal router (Traefik, docker labels):

```yaml
labels:
  - "traefik.enable=true"
  - "traefik.http.routers.cerbix.rule=Host(`status.example.com`)"
  - "traefik.http.routers.cerbix.entrypoints=websecure"
  - "traefik.http.routers.cerbix.tls.certresolver=le"
  - "traefik.http.services.cerbix.loadbalancer.server.port=8080"
```

**Nuances:**

- Only the **`api`** role (and `all`) serves the SPA/API. `scheduler`/`worker` have `app == nil` —
  there is no point publishing them on `/` via Traefik (they only have `/healthz` `/readyz` `/metrics`).
- `/metrics` and `/healthz` are **on the same port**. If Traefik routes `/*` externally, `/metrics`
  becomes public. Restrict it: a separate router with an IP allowlist/basic-auth middleware on
  `server.metrics_path`, or scrape Prometheus over the internal network, or don't publish that path.
- Multiple `api` replicas — a regular Traefik load balancer (a service with multiple backends);
  sessions are server-side in Postgres, so sticky sessions are not required.

**When nginx (or a separate static layer) is still warranted:** you need a CDN/aggressive static caching, serving
the SPA separately from the backend, or independent frontend scaling. Otherwise "Traefik + embed" is simpler
and drops the extra layer.

## 2.4 Geo-distributed probers (region-aware worker pools)

Use case: part of the services' infrastructure lives in one geo while cerbix (the core) lives in another; you need
to check internal targets of the "far" geo **from inside** it, without deploying a full cerbix there.

**Solution:** a monitor has a **region** (`region`, default `core`); the scheduler routes the job into the
`checks.jobs.<region>` queue; a worker started with **`--region <name>`** executes only jobs
of its region. RabbitMQ is **one, central**; only the **`worker`** role (DB-less) is distributed.

```bash
cerbix serve --role worker --region geo1 --config worker.geo1.yaml     # in geo-1, prober only
```

Minimal worker config (DB-less; everything else — defaults):

```yaml
log: { level: info, format: json }
rabbitmq:
  url: "amqp://cerbix:***@10.8.0.2:5672/"   # CENTRAL RabbitMQ (geo-2)
prober:
  allow_private_ips: true                    # internal geo-1 targets
```

**Rules and nuances:**

- Existing monitors = `core` (migration), keep running on the central workers as before.
- **Composite monitors are always `core`** (they need DB access for child statuses) — forced automatically.
- Jobs are published with TTL ≈ interval: if a region has no live worker, they expire instead of piling up
  (the next tick re-issues them). Keep at least one worker per assigned region.
- The SSRF guard is per-worker: a geo worker sets `prober.allow_private_ips: true` (already the default).
- **The region picker in the UI** (`GET /api/v1/regions`) shows regions in use **plus** those that
  have a **live worker** (a consumer on `checks.jobs.<region>`, via the RabbitMQ management API) —
  a region with the "● worker live" mark appears once its worker connects to the broker.
- **Test connection** in the monitor form (`POST …/monitors/test`) — a single probe run before creation
  **in the spec's region**, so that a geo target is tested **from its own region**, not from core. The test route
  **mirrors the transport of the region's production jobs**:
  - **AMQP region** — an RPC request (RabbitMQ direct reply-to) to a worker (`checks.tests.<region>`);
  - **pull region** (a geo without a broker, HTTP agent only) — the test travels via the **pull queue** `pull_tests`:
    the API enqueues a one-off test, the region's agent picks it up in a separate loop (`GET /agent/tests`), probes it with the same
    logic as a production job, and posts the heartbeat back (`POST /agent/test-results`); the API polls for the
    result until TTL. This way a pull region is tested by the same agent of the same region (otherwise — historically a 502,
    since there is no RPC subscriber in a pull region).

  No live worker/agent in the region → **502 "no worker/agent responded in region …"** (strict, no
  fallback to core — otherwise an internal geo target would yield a false DOWN; the UI highlights a non-live region in advance).
  Push/composite are not testable.
- **The "region without a worker" alert.** Since affinity is strict, the death of a region's worker would otherwise silently
  leave its monitors unchecked. Every 30s the scheduler leader compares regions that have enabled monitors
  against live consumers (management API) and, if a region has been without a worker for longer than **grace (90s)** —
  sends an alert to the channels of the affected projects (edge-triggered + latch, recovery when the worker returns).
  The grace suppresses false alarms during restarts; a management-lookup error is treated as "unknown" rather than
  "no workers" (the tick is skipped). Respects the global silence.

**The network to the central RabbitMQ is on the admins (good practice):** preferably a **WireGuard tunnel**
between geos (URL = `amqp://<wg-ip>`); alternatives — **`amqps://`** (RabbitMQ TLS listener) or your
**`amqproxy`**. cerbix does not interfere: `rabbitmq.url` is a plain string, accepts `amqp`/`amqps`.
Do not expose the broker to the public internet without TLS/segmentation.

**The broker-less alternative in a geo — the HTTP pull agent.** If exposing RabbitMQ to another geo is not an option, the region
is declared pull-served in the central config (`pull.regions`), and its jobs go into the DB queue
`pull_jobs` instead of AMQP. In the geo you run `cerbix serve --role agent --region <r> --config agent.yaml` (DB-less, broker-less): it
uses **only outbound HTTPS** to the center to fetch jobs (`GET /agent/jobs`, atomic claim `FOR UPDATE SKIP
LOCKED`), runs them through its prober, and posts heartbeats (`POST /agent/results`, the same ingest). Authentication — a bearer token:
a shared `pull.token` (catch-all), a per-region token (`pull.agents: [{region, token}]`, an agent sees only its own
region), **or** a DB token (issue/revoke without redeploy via `POST/DELETE /api/v1/agent-tokens`, global-admin). Liveness
of a pull region (for the picker and the "region without a worker" alert) — via agent heartbeat. Example: `docker/config.agent.yaml`.

**Production-grade properties of the pull transport:**
- **Long-poll (LISTEN/NOTIFY):** `GET /agent/jobs` holds the request until a job appears (or 20s max-hold) instead of
  frequent polling — near-instant delivery, Postgres load → nearly zero, the transport stays plain HTTP
  (gRPC/SSE are deliberately not pulled in: they are justified only at thousands of agents/sub-second delivery).
- **Edge buffer:** on a connectivity outage the agent keeps results in a bounded in-memory ring and on reconnect
  re-sends them as a **historical backfill** (`POST /agent/backfill`) — filling the SLA gap **without** running old
  events through alerting (no incident storm after the fact). Idempotent (unique `monitor_id, ts`).
- **Result scoping:** an agent cannot post another region's results (403).
- **Observability:** metrics `cerbix_pull_jobs_pending{region}` and `cerbix_pull_agent_lag_seconds{region}`. Page
  on **lag** (a stuck agent that heartbeats but doesn't drain the queue):

  ```yaml
  # Prometheus alert: a region has been accumulating jobs for more than 2 minutes
  - alert: CerbixPullRegionLagging
    expr: cerbix_pull_agent_lag_seconds > 120
    for: 2m
    labels: { severity: warning }
    annotations:
      summary: "Pull region {{ $labels.region }} is not draining its queue"
  ```

---

_Related documents:_ `project-description.md` (PRD), `decisions.md` (decision log D-0001…),
`traceability.md` (requirement→code/test), `runbook.md` (operations), `status.md` (checklist).
