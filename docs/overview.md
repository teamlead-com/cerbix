# cerbix — overview: binary, architecture, stack, deployment

Self-hosted, multi-tenant service reliability platform (D-0174): organization → project →
service, with monitors beside services as the checks that feed them. A Service declares what
its reliability MEANS in versioned definitions; cerbix measures that from its own checks and
drives the response. A single Go binary runs in different roles,
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
| `cerbix gate check` | Ask the reliability gate (FR-024) whether the error budget allows a release; the exit code follows `action` — `0` ALLOW/WARN, `2` BLOCK, `4` NOT_CONFIGURED, `1` transport/auth/429. | `--project <id>`, `--service <id>` (required), `--json`, `--timeout 10s`; the server is `CERBIX_URL` and the credential `CERBIX_TOKEN` (environment only, never a flag), `CERBIX_CA_FILE` adds a CA |
| `cerbix change record` | Record a change event for a service (FR-025) — a deploy, rollback or flag flip in one of its phases — so the service's facts can say what followed; exit `0` recorded or replayed, `2` refused by the contract (400/404/409, printed verbatim), `1` transport/auth/429. | `--project <id>`, `--service <id>`, `--kind deploy\|rollback\|flag`, `--phase started\|succeeded\|failed\|cancelled`, `--source <slug>`, `--external-id <id>` (required), `[--ref <label>] [--url <https url>] [--decision <id>] [--at <RFC3339>] [--json] [--timeout 10s]`; the same `CERBIX_URL` / `CERBIX_TOKEN` / `CERBIX_CA_FILE` environment contract as the gate verb, never a flag. |
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
| `scheduler` | Leader scheduler (Postgres advisory lock): publishes due jobs; rollup/retention; renotify; burn-eval; SLA reports; region-worker-alert; escalation-advance; gate-ledger partition maintenance (FR-024, on its own fenced advisory session); change-group retention (FR-025, daily, whole groups under the identity lock). | AMQP | yes | yes |
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
| `gate` | `evaluate_inflight_process`, `evaluate_inflight_principal`, `evaluate_rate_principal_per_minute`, `evaluate_rate_process_per_minute`, `evaluate_tx_budget_ms`, `decision_retention_days`, `decision_partition_lead_days`, `decision_partition_create_max`, `decision_purge_every`, `decision_purge_max_partitions` | Reliability-gate bounds (FR-024, spec `func-reliability-gate` §5a): process-local concurrency and rate caps on decisions, the decision transaction's budget, and the decision ledger's daily-partition retention and maintenance. All ten range-validated at load; the runbook has the table. |
| `change` | `record_rate_process_per_minute`, `record_rate_principal_per_minute`, `record_inflight_process`, `read_inflight_process`, `max_past`, `max_future`, `correlation_window`, `correlation_note_max`, `retention_days`, `retention_groups_per_batch` | Change-intelligence bounds (FR-025, spec `func-change-intelligence` §5a): process-local concurrency and rate caps on the record route and permits on its reads, the `occurred_at` clock window, the incident-correlation window and note size, and retention of change groups by age. Every key refused outside its range at boot, naming the key. |
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
| `scheduler` | Leader (advisory lock), min-heap `next_run`, job publishing; rollup, retention, renotify, burn-eval, SLA reports; the gate ledger's partition maintenance under its own session lock (FR-024); the daily purge of change groups by whole identity (FR-025). |
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
burn rate shipped in phase 2 (iter-0144) and are returned on `…/services/{id}/reliability`; a window without an objective is **absent** rather than returned as zeros.

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

**Reliability gate (FR-024, iter-0163).** A deploy pipeline asks whether the error budget allows a
release and gets a machine-readable answer: an observed `state` (`ALLOW`, `WARN`, `BLOCK`, `UNKNOWN`,
or `NOT_CONFIGURED` when the service has no policy) and an effective `action` (`ALLOW`, `WARN`, `BLOCK`;
none for `NOT_CONFIGURED`), with every matching or unavailable clause in `reasons[]` and the evidence
it rests on. A per-service POLICY names ONE SLO window and assigns each clause of a closed, versioned
vocabulary — `budget_exhausted`, `budget_consumed` (≥ N %), `page_burn_firing`, `ticket_burn_firing`,
`service_incident_open` — to `block`, `warn` or `ignore`, plus a mandatory `unknown_behavior` and a
`max_seal_lag_seconds` bound past which the budget is UNAVAILABLE rather than quoted stale. **The gate
derives nothing** (NFR-019): it consumes the owners that already exist — the report path
(`serviceReliabilityReportTx`: the budget for the policy's window and whether it may be quoted at all),
the burn latches of that window's target, the coverage clauses (as evidence only, never a clause) and
the open auto-incident predicate — and if a clause ever needs a fact none of them exposes, the owner is
extended and the service page gains it too. **One snapshot** (D6a): the service row, the policy, the
active override, the report, the latches, the incident, the coverage and the ledger INSERT all happen in
ONE `REPEATABLE READ` transaction whose first snapshot-bearing statement, `SELECT statement_timestamp()`,
is `evaluated_at` — so a gate response and the service page cannot disagree about the same instant.
Every decision is one immutable row in a daily-partitioned, bounded ledger
(`gate.decision_retention_days`), readable by id after the service is renamed or deleted; decisions are
not audit rows, policy and override mutations are. An override (`gate:override`, at most 7 days, one
active per service, bound to the policy revision) changes `action` only — `state` and `reasons[]` stay
the observed facts, and the metric sees `cerbix_gate_decisions_total{state="BLOCK",action="ALLOW",overridden="true"}`.
Decision requests are bounded in concurrency and in rate per process (`gate.*`, §5a); a 429 runs no
report and writes no row. Change intelligence — the record of what a pipeline changed and what followed — is FR-025,
described below.

Routes (spec D13a and §5; `{p}` project id, `{s}` service id — 400 malformed, 404 foreign or unknown;
every body decoded strictly, the server fills nothing in):

| Route | Action | Purpose |
|---|---|---|
| `POST /api/v1/projects/{p}/services/{s}/gate` | `gate:evaluate` (`viewer`+) | The decision. 200 with the D7 response; 429 with `Retry-After` from the §5a bounds; 503 `snapshot_conflict` / `ledger_unwritable` — no decision, nothing recorded. |
| `GET` / `PUT` / `DELETE /api/v1/projects/{p}/services/{s}/gate/policy` | read `gate:evaluate`; write `gate:policy:write` (`editor`+) | The policy. `PUT` carries the FULL document with `expected_revision` (`null` = "nothing configured"), 409 `revision_conflict` on mismatch; `DELETE ?expected_revision=N` tombstones it (later decisions are `NOT_CONFIGURED`, the ledger is untouched, the active override closes as `policy_deleted`). `revision` is a per-service generation a delete-and-recreate never reuses. Writable on a FILE-MANAGED service too (D13 — the documented exception to the 409 rule of `func-service-reliability.md` §16.6a). |
| `GET /api/v1/projects/{p}/services/{s}/gate/override` | `gate:evaluate` | The ACTIVE override, or 404 `none_active`. |
| `POST /api/v1/projects/{p}/services/{s}/gate/override` | `gate:override` (`project_admin`+) | Create: `{policy_revision, reason, expires_at}` → 201 `{id}`; 409 `override_active` / `revision_conflict`; no `action` field — D9 fixes what an override does. |
| `GET /api/v1/projects/{p}/services/{s}/gate/overrides` | `gate:evaluate` | History: the newest 50 by `created_at DESC, id DESC`, each with its read-time `status: active \| expired \| revoked \| inert`. |
| `GET` / `DELETE /api/v1/projects/{p}/services/{s}/gate/overrides/{override_id}` | `gate:evaluate` / `gate:override` | One override by immutable id, with both attribution triples; revoke → 204, or 409 `override_not_active` for an expired, revoked or superseded one — never a silent 204. |
| `GET /api/v1/projects/{p}/gate/decisions/{id}` | `gate:evaluate` | One ledger row, PROJECT-scoped: no service on the path, so it still answers after the service is deleted — the moment the evidence is wanted. |
| `GET /api/v1/projects/{p}/gate/decisions?from&to[&service_id][&state…][&cursor][&limit]` | `gate:evaluate` | The listing: `[from, to)` required and ≤ 31 days, `limit` ≤ 200 (default 50), live keyset cursor over `evaluated_at DESC, id DESC`; a foreign `service_id` is an empty page, not 404. `state` is repeatable — a SET of the five states, OR-ed, none = every state, any other value 400 `state_invalid` — applied in the WHERE clause, so a page is a page of matches and the cursor continues the filtered set (iter-0164). |

CLI: `cerbix gate check --project <id> --service <id> [--json] [--timeout 10s]` (`internal/cli/gate.go`)
— `CERBIX_URL`, `CERBIX_TOKEN` (environment only, never a flag), `CERBIX_CA_FILE`; exit `0` `ALLOW`/`WARN`,
`2` `BLOCK`, `4` `NOT_CONFIGURED`, `1` transport/auth/timeout/429 (no retry; `Retry-After` on stderr). The
word `UNKNOWN` is in the output whatever the exit code, because the exit follows the operator's declared
`unknown_behavior`. The operational side — the meaning of every `UNKNOWN` reason, the override
procedure, ledger sizing, the metric surface and its alerts — is in `runbook.md`.

**The gate in the SPA (D-0207, `9793758`).** The service detail carries a `Release gate` card between
Alerting and Dependencies (`frontend/src/components/ServiceGate.vue`: facts → who is woken → who may
deploy). Its header pill is the state of the newest ledger row for the service over the last 30 days —
`not configured` while the service has no policy — next to a `revision N` chip; without a policy the card
is an empty state naming the windows that have a target and, for editors, `Configure`. The policy is
edited inline — the window from the service's `sla_targets` inventory (`GET …/services/{s}` carries it in
canonical order since `3fc19e3`; `ListServiceSLATargets` in `internal/store/servicereport.go`),
`unknown_behavior`, the five clause assignments, the threshold, the seal lag in whole minutes — with
client validation mirroring the server's rules; Save, Discard and Delete send `expected_revision`, and
after a 409 the draft is kept and every mutation is blocked until an explicit Reload re-reads. The latest
decision is rendered with its `reasons[]` (only a MATCHED clause is drawn in its assignment's colour;
`seal_stale`/`facts_stale` are dashed), `sealed_through` with `seal_lag` beside it — marked stale once
the lag exceeds the policy's bound — and `facts_fresh_until`; the error-budget figure is the value the
decision itself quoted in a budget clause, never a fresher number fetched from the report path (NFR-019's
"same snapshot", read strictly). An override panel shows the active override with Revoke-by-id and the
add form (reason, `until` ≤ 7 days, one at a time); a compact CLI card prints `cerbix gate check` for this
project and service with the four exit codes.

Two ledger views complete the surface: `Gate decisions` (`/gate/decisions`,
`frontend/src/views/GateDecisionsView.vue` — `?service=` pre-filter from the card, an explicit `[from, to)`
range of at most 31 days frozen at Apply and refused client-side before the request, the server's
`range_too_wide` rendered the same way, a state picker that travels as the server's `state=` filter on
the first page and on every `Show 50 more` — the view filters nothing itself since iter-0164 — and pages
of 50 over the keyset cursor) and the by-id record
(`/gate/decisions/:id`, `frontend/src/views/GateDecisionView.vue` — every present field of the immutable
row plus the raw JSON, still readable after the service is deleted); the per-service override history is
`/services/:id/gate/overrides` (`frontend/src/views/GateOverridesView.vue`, read-only, the closure per
`revoked_reason`). RBAC follows the actions: `gate:evaluate` is viewer+, so the card and both histories
render for everyone who sees the service; the policy controls render for `session.canProjectWrite`
(editor+); the override controls only for `session.canProjectAdmin` (org/project admin or global admin,
`frontend/src/stores/session.ts`). A file-managed service does NOT make the gate read-only (D13). And the
SPA never asks the gate: opening a page reads the ledger (`limit=1` for the card) and creates no decision
— a decision is written only by `POST …/gate`, that is by the CLI or the pipeline.

**Change intelligence (FR-025, iter-0165 — closed 2026-09-01, D-0213: schema, store, API, `cerbix change
record` and the SPA per the owner-approved mock all landed in that iteration).** The gate says whether a release may go; change intelligence records that it WENT
and lets the service's existing facts say what followed. A pipeline records a **change event** for a
service: `kind ∈ {deploy, rollback, flag}`, `phase ∈ {started, succeeded, failed, cancelled}`, the instant
it `occurred_at`, an external identity `(source, external_id)` under which the phases of one change are
grouped, an optional bounded `ref` (a version or commit label), an optional `https://` `url`, and
optionally the gate `decision_id` the release rested on. Phases are append-only rows keyed
`UNIQUE (service_id, source, external_id, phase)`; the order is the domain's — `started`, then exactly one
terminal; a terminal alone is accepted because many pipelines can only report the end — and a violation is
refused by name (409 `phase_order`, `kind_mismatch`; 400 `occurred_at_before_start`). Replaying a phase
with an identical body is 200 with the ORIGINAL row (a retry is not an error; the original actor and
`recorded_at` stand); a differing replay is 409 `phase_exists` naming the field. Writes for one identity
are serialized by a transaction-scoped advisory lock hashed over the canonical uuid text, so two pipelines
reporting `succeeded` and `failed` for one run cannot both pass the order check. The actor is
server-derived and stored twice (`actor_label` — `token:<name>` for a token — plus the typed pair); a body
with an actor field is a 400 unknown field. **It is a fact about time, not a catalog** (D1): no
repository, owner, environment or artefact is stored, `ref` and `url` are opaque bounded labels nothing
joins or searches, and cerbix takes no action on any external system. Text has one canonical form — NFC,
trimmed, no control or format characters, lengths in code points — normalized by the handler, validated
by the domain (the single Unicode authority), written only through `RecordChangePhase`; the DB CHECK
enforces exactly length and the ASCII control class and claims no more. `external_id` is case-sensitive
after normalization.

Three reads need no new measurement. **The timeline** is a `[from, to)` read of at most 92 days over
change GROUPS (one per identity, phases nested, selected and ordered by `latest_occurred_at DESC, source,
external_id`), with an opaque keyset cursor that never returns a group twice, `limit` counted in groups,
`kind` as a repeatable OR-set and `source` as one slug; a group's gate decision is read back live
(`state`/`action`, or `aged_out` once the ledger partition is gone) and `incidents[]` names the incidents
the change PRECEDED. **The correlation** runs where `⚡ Context:` does — at a service auto-incident's
`opened` delivery in the outbox worker: every change whose latest phase known at that instant lies within
`change.correlation_window` before the open, on the incident's own service and on the services the impact
graph marks `probable_root`, becomes one `incident_changes` row anchoring that phase with its lag copied
and never updated, and ONE system note `🚀 Changes: <n> preceded this incident — <kind ref by source,
−<lag>>; …` is appended in the same transaction through the same marker guard. The word is "preceded",
never "caused" — cerbix does not know that the deploy caused anything. It is fail-open: an error is
counted (`cerbix_change_correlation_errors_total`) and the incident opens and resolves exactly as before;
a change recorded after the open is not back-linked. **The comparison** states the SLI before and after a
change's terminal phase over a horizon of `15m`, `1h`, `6h` or `24h` from SEALED canonical buckets only,
through the series owner's own query (`reliabilityStepRollupTx`, shared with the reliability page —
NFR-020: never a second implementation): each side is a figure, or `withheld` with the page's own word
(`definition_changed`, `undecidable`, `no_facts`), or `pending` with `sealed_through` stated when the
side's end exceeds the seal — `after` when `T + h` does, `before` when `T` itself does (D-0211) — never a
partial figure; `delta` only when both sides are figures; a started-only group has no comparison (404
`no_terminal_phase`). Nothing is stored or cached: two reads in one snapshot are equal, and across time
the figures follow the page's own corrections.

Routes (spec D6–D8 and D12; `{p}` project, `{s}` service, `{i}` incident — 404 for an invisible project,
403 for a missing action, every body decoded strictly):

| Route | Action | Purpose |
|---|---|---|
| `POST /api/v1/projects/{p}/services/{s}/changes` | `change:record` (`editor`+) | Record a phase. 201 recorded; 200 identical replay `{replayed, change}`; 400 `kind_invalid`, `phase_invalid`, `source_invalid`, `external_id_invalid`, `ref_invalid`, `url_invalid`, `occurred_at_out_of_bounds`, `occurred_at_before_start`, `decision_unknown`, or an unknown field by name; 409 `phase_order`, `phase_exists`, `kind_mismatch`; 429 `process_inflight` / `principal_inflight` / `process_rate` / `principal_rate` with `Retry-After ≥ 1` (the `change.record_*` bounds, §5a). |
| `GET /api/v1/projects/{p}/services/{s}/changes?from&to[&kind…][&source][&cursor][&limit]` | `project:read` (`viewer`+) | The timeline: `[from, to)` required and ≤ 92 days, `limit` 1..200 (default 50), `{items, next_cursor}`; 400 `range_required`, `range_invalid`, `range_too_wide`, `limit_invalid`, `cursor_invalid`, `kind_invalid`, `source_invalid`. |
| `GET /api/v1/projects/{p}/services/{s}/changes/compare?source&external_id[&horizon]` | `project:read` | Before/after at `horizon` (default `1h`; otherwise 400 `horizon_invalid`); 404 `not found` for an unknown identity, `no_terminal_phase` for a started-only group. |
| `GET /api/v1/projects/{p}/incidents/{i}/changes` | `project:read` | The incident's preceding changes — the anchored link rows with each group's live phases, `own_service` and `upstream`. |

The three reads take in-flight permits (`change.read_inflight_process`) and no rate token.

CLI: `cerbix change record --project <id> --service <id> --kind deploy|rollback|flag --phase started|succeeded|failed|cancelled --source <slug> --external-id <id> [--ref <label>] [--url <https url>] [--decision <id>] [--at <RFC3339>] [--json] [--timeout 10s]`
(`internal/cli/change.go`) — `CERBIX_URL`, `CERBIX_TOKEN` (environment only, never a flag),
`CERBIX_CA_FILE`; ONE stdout line `recorded change=<id> kind=<k> phase=<p>` (or `replayed …`); exit `0`
recorded or replayed, `2` refused by the contract (400/404/409 — the pipeline's own mistake, printed
verbatim on stderr; usage errors too), `1` transport, auth, timeout, 429 (no retry; `Retry-After` printed)
and every other status. `--at` defaults to the invocation instant; `--json` prints the response byte for
byte. The CLI holds no copy of the rules: what it is given travels verbatim and the server's refusal is
what it prints.

**Tokens with an `actions` allow-list (D12, D-0212).** `api_tokens.actions` is `NULL` for every token
that exists today — the role decides, nothing changes — or a list intersected with the role inside the ONE
central predicate, `authz.Can` and its query-scope mirror `VisibleScope`: `Can(action)` for a token is
`roleGrants[role] ∋ action AND (actions IS NULL OR action ∈ actions)`. A CI token is `role: editor,
actions: [gate:evaluate, change:record]`: it can ask the gate and record changes and can do NOTHING else —
its `GET …/services` is 403, not 404, because project VISIBILITY (`VisibleProject`, the 404-versus-403
predicate) is membership alone. The list is validated at creation against the action catalogue (400
`action_unknown`) and against the token's own role (400 `action_not_granted` naming the entry — an
operator's mistake surfaces at the form, not at the pipeline's first 403), is immutable after (a different
list is a new token; `PATCH`/`PUT` are 405), and appears in the token's read model (`null` or an array)
and in the `token.create` audit row. Recording a change is not an audit event — the row is the record.

Bounds and retention: the ten `change.*` keys (§5a) are refused outside their ranges at boot, naming the
key. Retention (`change.retention_days`, default 400) removes WHOLE change groups by the age of their
latest phase — at most `change.retention_groups_per_batch` group keys per statement in key order, under
the same per-identity lock the writer takes, `incident_changes` cascading — on the scheduler leader's
daily cadence; deleting a service cascades its changes and links while the incident note remains as text.
Metrics (`internal/metrics/change.go`; every label set closed; no service, source or identity label):
`cerbix_changes_recorded_total{kind,phase,outcome}`, `cerbix_change_record_rejected_total{reason}`,
`cerbix_change_correlations_total{role}`, `cerbix_change_correlation_errors_total`,
`cerbix_change_compare_total{outcome}`, `cerbix_changes_retained` (rows, sampled by the leader after each
retention pass, cleared on leadership loss). In the SPA, per the approved mock (D-0210) and landing in
iter-0165: change marks on the service's reliability strip (one per terminal phase, placed by
`occurred_at`, kind-shaped, never a state colour), a `Changes` card between `Release gate` and
`Dependencies`, a per-service timeline view, the comparison view, a `Preceded by` section on the incident
page and the `actions` list on the token form — no control writes a change; the record is the pipeline's.
The operational side — the CI-token recipe, "a pipeline reports out of order", the alert rows, the
retention knobs and what `pending` versus `withheld` means to a reader — is in `runbook.md`.

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

**Rollout order:** stop the outbox owners (`all`/`api`/`scheduler`) → `cerbix migrate` (one-off,
NEW binary) → scheduler → workers → api. Probers (`worker`) and pull agents do not deliver outbox
events and keep running throughout. Every role fails fast on an invalid config or unreachable DB.

Roles also auto-apply migrations at startup whenever `database.dsn` is set, and doing so concurrently
is safe from races: `store.Migrate` holds a session `pg_advisory_lock` for the whole goose run, so
roles starting together serialize. Safe from races is not the same as safe to rely on here. Two
reasons to keep the one-off step:

- it makes a schema change a deliberate act performed at a moment somebody chose, rather than
  something the first process to boot happens to do;
- for upgrades crossing migration **00088** it is REQUIRED, together with the stop above. That
  migration fences the rows an old producer writes so an old worker cannot claim an incident's events
  and deliver them out of order — but it cannot reach a delivery such a worker already has in flight.
  Letting a role auto-apply it while other old roles are still running is precisely the window it
  exists to close. The runbook carries the procedure and what skipping it actually risks.

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
