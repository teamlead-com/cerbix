# cerbix — Project Description (PRD)

## Problem & Goal

Company teams run ~10 internal projects (each with its own developers and PM) and need
real-time visibility into whether their applications are up, plus historical SLA/SLI.

cerbix is a **self-hosted, multi-tenant service reliability platform**: teams DEFINE what
reliable means for a service — which checks are its SLI, how regions aggregate, what counts
as pageable — in versioned definitions, cerbix measures that from its own checks, and it
drives the operational response. Concretely: SLI/SLO with error budget and burn rate,
GOOD/BAD/UNKNOWN reliability semantics with reasons, incidents anchored to a monitor or a
Service, dependency impact, public status pages with postmortems, on-call escalation,
provider-agnostic OIDC, org→project multi-tenancy with strict isolation and RBAC.

**What it is not**, stated here because the boundary shapes every decision below: not a
telemetry store and not an observability platform. No arbitrary time-series queries, no
query language, no metrics backend, no trace or log ingestion, no service catalog, no
automatic root-cause analysis. It reads PromQL as a check source and exports its own
metrics; your Prometheus/Grafana stack stays where it is. The original framing —
"uptime monitoring service with SLA/SLI computation" — described the product before FR-021
made the Service a first-class reliability domain, and is kept here only as history.

## Constraints

- **Backend:** Go (`github.com/teamlead-com/cerbix`, layout `cmd/`+`internal/`, no `pkg/`).
- **Database:** Postgres — heartbeats use native daily RANGE partitioning plus a
  daily-availability rollup table for cheap long-window SLA (no TimescaleDB dependency, D-0037).
- **Broker:** RabbitMQ (jobs/results) — designed in from the start for horizontal growth.
- **Frontend:** Vue 3 + TypeScript (Vite), embedded into the binary via `embed.FS`.
- **Auth:** provider-agnostic OpenID Connect = authentication only — any OIDC-compliant
  issuer works (Keycloak, Auth0, Okta, Google, Entra ID; Keycloak is just the dev-stack
  example, D-0043). Authorization (membership + roles) lives in the cerbix database
  User identity keys on the `oidc_sub` claim (D-0044).
- **Deploy:** out of scope for now; only local dev via `docker/docker-compose.yml`.

## Architecture (summary)

One binary `cerbix` with roles selected by `--role` (detailed diagrams and sequence flows live in [`docs/architecture.md`](architecture.md)):

- **api** — REST (stdlib `net/http` routing, D-0010), OIDC login, RBAC, consumes
  `checks.results`, writes heartbeats/incidents, serves the SPA. Push-based realtime is
  shipped: the ingest pipeline publishes status changes to an in-process bus and the api
  streams them to the browser over SSE (`GET /api/v1/events`, D-0056). Multi-replica realtime
  would front the bus with Redis pub/sub (not yet needed).
- **scheduler** — single active instance via Postgres advisory lock; min-heap by
  `next_run`; publishes jobs to `checks.jobs`.
- **worker** — stateless, N replicas; executes probes with timeouts; publishes
  `checks.results`.

Transport between scheduler and workers is the `Dispatcher` interface: `rabbitmq`
(production) or `inproc` (local dev/tests).

## Domain Model & Naming

`Organization` → `Project` → `Monitor` → `Heartbeat`, plus `Service` (with its definition
revisions, evaluation epochs and sealed facts), `Incident`/`IncidentUpdate`/
`Postmortem`, `StatusPage`/`Component`, `MaintenanceWindow`, `NotificationChannel`,
`Subscriber`, `ApiToken`. Organization is the isolation boundary; Project is the primary
unit of permissions; a Monitor is one check with its own history, and a Service is the
declared reliability domain that monitor may contribute to — the two sit beside each other
(see "Service as a reliability domain" below), which is why this line no longer calls a
monitor "the service being checked".

## RBAC

`Global Admin` → `Org Admin` → `Project Admin` (a.k.a. Maintainer) → `Editor` → `Viewer`,
enforced by a central `authz.Can(user, action, scope)`. Tenant isolation: every query is
constrained to the caller's authorized `org_id`/`project_id`; users without membership
never see another org's projects/monitors.

## Check Types

MVP: HTTP(S) (status/latency/keyword/regex/JSON-query/headers), TCP, ICMP ping,
Push/heartbeat. Later: DNS, TLS cert expiry, gRPC, WebSocket/SSH/mail, PostgreSQL/MySQL/
Redis/RabbitMQ/Docker, Prometheus/PromQL, composite/group. Each is a `Prober` plugin;
assertions use a declarative condition engine.

## SLA / SLI / SLO

SLI = successful/total over rolling windows (24h/7d/30d/90d); SLO is the internal target,
SLA the (looser) external promise; error budget = 1 − SLO. Each window reports uptime %,
average and **p95 latency** (`percentile_cont(0.95)` over up checks, D-0046). Maintenance
windows are a first-class state excluded from SLA math. Storage: raw heartbeats live in
native daily RANGE partitions (~30d retention, dropped by partition) feeding a
daily-availability rollup table that backs cheap long-window SLA (D-0037).

## Status Pages & Incidents

Primary status page granularity is org-level (projects/monitors as components); optional
project-level pages. Per-page visibility: `internal` (default), `public`, `unlisted`.
Incidents (auto/manual/API) have a status timeline (investigating→identified→monitoring→
resolved), impact levels, and affected components. Postmortems attach to incidents and
publish via GUI and API. Subscribers + outbound webhooks + RSS/Atom/JSON feeds are
integration points. API auth: cerbix service-account tokens **and** OIDC
client-credentials JWTs (any issuer, D-0043), both routed through `authz.Can`.

## Console & API Surface (cross-cutting)

- **Global search** — `GET /api/v1/search` returns tenant-scoped hits across monitors,
  projects and incidents; the topbar search box is wired to it (D-0047).
- **Login discovery** — `GET /auth/config` (public) reports which methods an instance
  offers (`local`, `oidc`) plus a configurable OIDC **button label** (default
  "Continue with SSO"), so the sign-in page renders provider-agnostically (D-0045).
- **Self-service tenancy UI** — org and project creation from the workspace switcher,
  role-gated (global admin creates orgs, org admins create projects, D-0048).
- **Settings page** — one place to manage notification channels (per project) and API
  tokens + outbound webhooks (per org), mirroring the service-account/notification model.

## Monitoring as Code

An optional file provider in the `api`/`all` control plane hot-reconciles strict, versioned,
project-scoped YAML bundles into PostgreSQL without restarting Cerbix ([`internal/fileprovider`](../internal/fileprovider/)
+ [`internal/store/fileapply.go`](../internal/store/fileapply.go)). Existing organizations/projects
are referenced by immutable slugs; the provider never provisions tenants. Ownership is per
monitor, so file-managed and UI/API-managed monitors coexist in one project (file-managed ones
are read-only in the UI/API — `409 managed_by_file`). Reconciliation is transactional per
ProjectBundle, tenant-scoped, last-known-good preserving, no-op idempotent, HA-elected through a
PostgreSQL advisory lock (leader-fenced apply), and never hard-deletes history — a removed file
orphans then disables after a grace period, and re-adding it restores the same DB id + push
token. A committed config change wakes the scheduler via `monitor_config_changed`. Operators get
a tenant-safe diagnostics API (`GET /api/v1/admin/file-providers` → `{bundles, providers}`), a
"Managed by file" badge + named-provider filter in the UI, and Prometheus alert rules
([`docker/alerts/monitoring-as-code.rules.yml`](../docker/alerts/monitoring-as-code.rules.yml)).
Full contract: [`docs/specs/func-monitoring-as-code.md`](specs/func-monitoring-as-code.md)
(FR-017, NFR-014, D-0145/0146/0147/0148) — **DONE**.

## Project secret inventory

Projects have a write-only named secret inventory for credentialed PostgreSQL, MySQL,
Redis and RabbitMQ monitors. UI/API monitors and Monitoring-as-Code bundles store only a
`password_ref`; normalized tenant-safe reference rows prevent cross-project resolution and
guard rename/delete. Values are AAD-bound under the core-only at-rest master, then read at
the dispatch linearization point and immediately re-wrapped under a per-region dispatch
key. Workers and pull agents never receive the master and v1 consumers can never claim an
envelope job. Wrong/missing keys produce a typed diagnostic without heartbeat/status/SLA
mutation and degrade executor readiness. Ref-based target transport is encrypted by default,
with insecure or skip-verify modes explicit. The Secrets panel, monitor reference selector,
rotation runbook, metrics and alerts ship in the single binary. Full contract:
[`docs/specs/func-secret-inventory.md`](specs/func-secret-inventory.md) (FR-020, NFR-015,
D-0155) — **DONE**.

## Service as a reliability domain

A `Service` is the unit whose reliability is **explicitly declared**, and it sits BESIDE monitors
rather than above them: one monitor may contribute to several services, or to none. A team declares
what availability MEANS for a service — which checks are its SLI, how regions aggregate, what counts
as pageable — in a versioned definition revision, while what the system actually MEASURED is a
separate evaluation epoch; every fact references the epoch, so any reported number resolves back to
exactly one declaration. Facts are duration-weighted and sealed bucket by bucket, and the seal
watermark advances only over CONTIGUOUS data — a gap holds it back instead of being jumped over —
which is what makes a number reproducible and not merely current. Availability per window, the error
budget and the burn rate are served from those sealed facts (`GET …/services/{id}/reliability`,
[`internal/reliability/reduce.go`](../internal/reliability/reduce.go)); a window with no objective is
ABSENT rather than reported as zeros, and missing inputs are never rendered as perfect reliability. A
service can also be the thing that PAGES: with declared alerting ownership its SLI members stop
delivering their own alert for the same failure — but only per signal, only for onset-like events, and
only while a replacement is demonstrably armed; anything ambiguous fails OPEN and the member pages,
because a page that was not needed is noise while a page that was owed and never sent is the failure
the design exists to prevent. All of that governs DELIVERY only — facts, status flips and history keep
recording either way. Services are declarable as code through the same file provider as monitors. Full
contract: [`docs/specs/func-service-reliability.md`](specs/func-service-reliability.md) (FR-021,
NFR-016, D-0159/0166/0167/0168) — **DONE** (iter-0153, D-0169; the §16.9 follow-ups were explicit
non-goals there and shipped as the two requirements below).

## Service incidents and escalation

A Service can be the SUBJECT of an incident, not merely a thing that pages. An incident has AT MOST
ONE anchor — a monitor or a service — so every read path branches on an unambiguous discriminator,
while a manual project-level incident with neither keeps working. A live service alert opens one
automatically, in the same transaction as the announcement and under the same gates that decide
whether it pages at all; the end of the announcement resolves it. A burn-rate breach opens none, ever
— a budget signal trails the seal and says nothing about now. At most one open auto-incident per
service, so a flapping service cannot accumulate them, and only automatic incidents are auto-resolved,
so a machine never overwrites a conclusion a person drew. The incident carries its impact links and a
snapshot of the members the service had AT OPEN time, so it keeps naming a member deleted since;
deleting the service clears the anchor and keeps the record, timeline intact. A service that has an
escalation policy escalates its OWN incident: steps fire on the policy's schedule from the incident's
start, progress latches so a step fires once, acknowledgement or resolution ends the ladder, and every
step names the SERVICE. The ladder fails CLOSED where alert delegation fails open — ambiguity at
delivery time means one page exists, ambiguity in a ladder would mean pages MULTIPLY on a state nobody
can confirm — and the service graph annotates and links but never pauses it. Nothing about a monitor's
incident changes: lifecycle, notes, escalation, status-page rendering and postmortem are the same
before and after (NFR-017, NFR-018), and an open service incident moves no component STATUS on a
status page — it is rendered as an incident next to a status still derived from health alone. Full
contract: [`docs/specs/func-service-incidents.md`](specs/func-service-incidents.md) (FR-022, NFR-017,
D-0171) and [`docs/specs/func-service-escalation.md`](specs/func-service-escalation.md) (FR-023,
NFR-018, D-0173) — **DONE**.

## Reliability gate

A deploy pipeline asks cerbix whether a release may go out and gets an answer it can branch on
instead of a dashboard someone has to read. One call — `cerbix gate check --project <id> --service
<id>` or `POST /api/v1/projects/{p}/services/{s}/gate` with an API token — returns the observed
`state` (`ALLOW`, `WARN`, `BLOCK`, `UNKNOWN`, or `NOT_CONFIGURED` when the service has no policy), the
effective `action`, every matching reason and the evidence each reason rests on; the CLI's exit code
follows the action (`0` allow/warn, `2` block, `4` not configured, `1` transport/auth), so a CI step
is one command. What blocks is declared per service in a POLICY naming ONE SLO window and assigning
each clause of a closed vocabulary — budget exhausted, budget consumed at or above N %, page-burn firing,
ticket-burn firing, an open service incident — to `block`, `warn` or `ignore`, plus what to do when a
fact is unavailable and how stale the facts may be before they are refused rather than quoted. The
gate computes **no new reliability number**: it reads the same sealed facts through the same code
paths as the service page, inside one database snapshot, so a gate answer and the screen cannot
disagree about the same instant. A release that must ship anyway gets a time-bounded, audited
override (at most 7 days, one active per service, project admin only) that changes the ACTION and
never the observed state — the decision still records that the budget was exhausted and that someone
overrode it. Every decision is an immutable row in a bounded, daily-partitioned ledger, readable by
id after the service is renamed or deleted; the service page carries a `Release gate` card
([`frontend/src/components/ServiceGate.vue`](../frontend/src/components/ServiceGate.vue)) with the
policy editor, the latest decision and the override panel, and opening a page never creates a
decision — only a pipeline does. Full contract:
[`docs/specs/func-reliability-gate.md`](specs/func-reliability-gate.md) (FR-024, NFR-019,
D-0201/D-0207/D-0208) — **DONE**.

## Change intelligence

The gate says whether a release may go; change intelligence records that it WENT, and lets the facts
the service already has say what followed. A pipeline reports a **change event** — `deploy`, `rollback`
or `flag`, in one of its phases (`started`, `succeeded`, `failed`, `cancelled`) under its own external
identity `(source, external_id)` — with `cerbix change record` or one `POST`, optionally naming the
gate decision the release rested on. Phases are append-only and idempotent: an identical retry returns
the ORIGINAL row, a contradictory one is refused by name, and two runners reporting different endings
for the same run cannot both pass. It is a fact about TIME, not a catalog — no repository, environment
or artefact is stored, the version label and link are opaque, and cerbix takes no action on any
external system. Three reads follow, none of which measures anything new: a service TIMELINE of change
groups over a bounded window; the CORRELATION written when a service incident opens automatically,
linking the changes that preceded it on that service and on the upstreams the impact graph marks as
probable root, with one `🚀 Changes:` note whose word is "preceded", never "caused" — cerbix does not
claim to know what caused an outage — and which fails open, so an incident never depends on it; and a
before/after COMPARISON of the SLI around the change, computed from SEALED buckets through the same
query the reliability page uses, so it is withheld exactly where the page would withhold and reads
`pending` until the facts are sealed rather than quoting a partial figure. A CI token can be narrowed
to exactly what a pipeline needs — record a change, ask the gate — instead of carrying a whole role.
In the UI the service page carries a `Changes` card
([`frontend/src/components/ServiceChanges.vue`](../frontend/src/components/ServiceChanges.vue)) beside
the release gate, with the timeline and the before/after view behind it. Full contract:
[`docs/specs/func-change-intelligence.md`](specs/func-change-intelligence.md) (FR-025, NFR-020,
D-0209/D-0211/D-0213) — **DONE**.

## Delivery Method

Iteration-based per `AGENTS.md`. The full phased roadmap and rationale live in the
planning document; scoped specs are in `docs/specs/`; live status is `docs/status.md`.
