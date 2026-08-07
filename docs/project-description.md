# cerbix — Project Description (PRD)

## Problem & Goal

Company teams run ~10 internal projects (each with its own developers and PM) and need
real-time visibility into whether their applications are up, plus historical SLA/SLI.
cerbix is an internal, self-hosted uptime monitoring service with SLA/SLI computation,
provider-agnostic OpenID Connect (OIDC) authentication, org→project multi-tenancy with
strict isolation, role-based access control (RBAC), and public status pages with
incident and postmortem communication.

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
- **Deploy:** out of scope for now; only local dev via `deploy/docker-compose.yml`.

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

`Organization` → `Project` → `Monitor` → `Heartbeat`, plus `Incident`/`IncidentUpdate`/
`Postmortem`, `StatusPage`/`Component`, `MaintenanceWindow`, `NotificationChannel`,
`Subscriber`, `ApiToken`. Organization is the isolation boundary; Project is the primary
unit of permissions; Monitor is the "service" being checked.

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

## Monitoring as Code (planned)

An optional file provider in the `api`/`all` control plane will hot-reconcile strict,
versioned, project-scoped YAML bundles into PostgreSQL without restarting Cerbix. Existing
organizations/projects are referenced by immutable slugs; the provider never provisions
tenants. Ownership is per monitor, so file-managed and UI/API-managed monitors coexist in
one project. Reconciliation is transactional per ProjectBundle, tenant-scoped,
last-known-good preserving, no-op idempotent, HA-elected through PostgreSQL, and never
hard-deletes history. The complete pre-implementation contract is
[`docs/specs/func-monitoring-as-code.md`](specs/func-monitoring-as-code.md) (FR-017,
NFR-014, D-0143).

## Delivery Method

Iteration-based per `AGENTS.md`. The full phased roadmap and rationale live in the
planning document; scoped specs are in `docs/specs/`; live status is `docs/status.md`.
