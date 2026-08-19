# Changelog

All notable changes to **cerbix** will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

## [Unreleased]

### Fixed

- **PostgreSQL 15 is now enforced instead of assumed.** A production upgrade to `v0.1.5-beta.1` on
  PostgreSQL 14 applied `00061`…`00069` and died on `00070` with `syntax error at or near "("`: five
  migrations use the column-list `ON DELETE SET NULL (col)` form introduced in PG15. Every document, image
  and CI job already said 16, but nothing checked it and nothing said it out loud. `cerbix migrate` now
  reads `server_version_num` before the first file and refuses with the version, the requirement and the
  fact that nothing was applied. README, `runbook.md` and `overview.md` state the requirement; the runbook
  also carries the recovery note for a system left partially migrated (`00065` makes `monitors.slug` NOT
  NULL, which an older binary does not write).
- **A status-page component could not be created from a Service.** The service picker rendered a blank
  option AND carried no value, because the view read `sv.name`/`sv.id` from a list endpoint that answers
  `ServiceSummary` (the service wrapped with its rollup counts). An `as Service[]` cast on the load is what
  stopped the compiler from reporting it, and the test fixture repeated the same wrong shape, so six
  passing tests never saw it.
- **A stale claim on the service page.** The footer said availability, the error budget and the burn rate
  "arrive with the next iteration". They arrived in iter-0144 and the page already renders them; what was
  actually missing on a fresh service is sealed facts and a declared objective, which is what it says now.

---

## [v0.1.5-beta.1] - 2026-08-19

242 commits since `v0.1.0-beta.5`. This is the release where a **Service** becomes the object
reliability is defined on, measured for, and paged about — and where the product's own
positioning was corrected to match (**D-0174**).

### Added

- **Service reliability, phases 3–5 (FR-021 / NFR-016, D-0169)** — closed against an ENFORCED
  discharge map of 91 acceptance invariants and 24 required scenarios, each naming a test that
  exists; `make docs-check` fails if a number lacks a row or a row cites a test the tree lacks.
  - **dependency impact graph** — a same-project service DAG (schema-enforced tenancy, bounded,
    outside the declaration), symmetric open-time correlation into structured incident↔service
    links with 🕸 timeline notes. It annotates and links: it records **candidates**, never elects
    a culprit, and never suppresses or hides.
  - **status-page projection (§15.0)** — a component renders from ONE of three sources under a
    discriminator (`monitor`/`service`/`manual`) with the replaced binding kept dormant for
    revert. **Public-output change:** the page summary is worst-of-MEASURED plus an unmeasured
    count, and measurement ABSENT is the public status `no_data` — never `operational`.
  - **alerting ownership (§16)** — a Service can be the thing that pages, and its declared SLI
    members can stop delivering their own alerts for the same failure. Suppression is per
    SIGNAL (live health, sealed burn), per POLARITY (onset-like only; a recovery is never
    suppressed) and only while a replacement is demonstrably ARMED. Anything ambiguous **fails
    open** — the member pages.
  - **service burn alerting** — arbitrary long/short windows over one burn-math owner, with the
    hold matrix and the watermark every number was computed from.
- **Service incidents (FR-022 / NFR-017, D-0170, D-0171)** — an incident can be an incident OF a
  Service. At most one anchor, enforced by CHECK; opened in the SAME transaction as the
  announcement and resolved by its close; **never on a burn breach**; at most one open
  auto-incident per service; a member snapshot a postmortem can still name after the world moved;
  impact links through the service graph, with no link ever naming its own subject. Closed
  against 16 invariants + 16 scenarios.
- **Escalation for services (FR-023 / NFR-018, D-0172, D-0173)** — a Service with an escalation
  policy escalates its own auto-opened incident: steps from the incident's start, durable
  progress, acknowledgement or resolution ends it, every step names the SERVICE. The ladder
  **fails closed** where delegation fails open, and the service graph does **not** pause it.
  Closed against 16 invariants + 19 scenarios.
- **Project-level SLO objective** and an **instance-wide audit surface** for a global admin's own
  actions, which had been recorded for months and shown nowhere.
- **`job_id` correlation** end to end, and `observed_at` ordering that refuses a result older
  than the issue it answers.
- **A write path for a service's escalation policy** — the column existed since phase 5 and was
  reachable only at create time or from a file provider; a change to who gets woken is now
  audited with what moved, inside the mutating transaction.
- **`cerbix_service_incidents_total{action}`** and **`cerbix_escalation_steps_total{subject}`** —
  the on-call ladder had no metric at all before, only a log line.

### Changed

- **Positioning (D-0174)** — cerbix is a **service reliability platform**, not "uptime & SLA
  monitoring". The README states its NON-GOALS publicly, quoted from the specification: no
  arbitrary time-series queries, no generic telemetry, no query language, no metrics backend, no
  service catalog, no trace or log ingestion, and **no automatic root-cause analysis**.
- **`ServiceDetail.reliability`** stays `null` by design and now SAYS so: SLO, error budget and
  burn rate live on `GET …/services/{id}/reliability` with the honesty context a bare number
  would lack. The previous description claimed they were unbuilt.
- **Dependencies** — `golang.org/x/crypto` 0.55.0, `x/net` 0.58.0, `x/text` 0.41.0; build image
  golang 1.26.6; pinia 4.0.3; and four frontend majors (`@vitejs/plugin-vue` 6, `npm-run-all2` 9,
  `unplugin-auto-import` 21, `@vueuse/core` 14), each merged and verified one at a time.

### Fixed

- **A public leak found by reviewing our own invariant:** `Incident.PublicRedacted` cleared
  `project_id`, `monitor_id`, the external key and the ack actor — and not the `service_id` added
  days earlier, so every unauthenticated render of a page with a service incident shipped the
  service's internal UUID.
- **CI had never run on this line of work**: tests triggered only on `pull_request`, `docs-check`
  ran only by hand, one storage mode was covered, and readiness was awaited with `pg_isready`,
  which this project's own discipline calls a non-barrier. All four repaired.
- **Two flaky tests fixed at their cause**, not re-run: a fence test that bet a budget refusal on
  5ms of wall clock (2 failures in 12 runs under load), and scheduler telemetry waits that
  asserted more series than they waited for.

### Migrations

24 new migrations (`00061`…`00084`), forward-only and applied automatically by every role.

### Compatibility

FR-021 §17 makes backward compatibility an acceptance criterion, not a footnote: zero Services is
a valid installation state, every existing Monitor stays valid without a service, bundle format 1
stays valid, existing composites and monitor SLOs keep their semantics. **The one intentional
break is the public status-page output** described above — a consumer that read `operational` for
an unmeasured component will now read `no_data`.

---

## [v0.1.0-beta.1 … v0.1.0-beta.5] — released 2026-07-25 … 2026-08-12

> **Corrected on 2026-08-19.** This block sat under `[Unreleased]` while its contents were
> shipping across the five `v0.1.0-beta.*` tags — nobody moved it out. It is relabelled rather
> than rewritten, because it is a record of what happened, not a plan. The per-beta split is
> not reconstructed here: the tag messages carry it, and inventing a division after the fact
> would be a worse claim than admitting the block covers the whole beta train.

### Added
- **Geo-Distributed HTTP Pull Agent** (`--role agent`) with Long-Polling (`LISTEN/NOTIFY`), Edge Ring-Buffer (`bufferCap=10000`), and historical backfill (`POST /agent/backfill`).
- **Observability for Pull Transport**: Prometheus gauges `cerbix_pull_jobs_pending{region}` and `cerbix_pull_agent_lag_seconds{region}` with automatic lagging alerts.
- **Database Agent Tokens**: Table `agent_tokens` and Admin API (`POST/GET/DELETE /api/v1/agent-tokens`) for issuing, listing, and revoking agent tokens without redeploy.
- **Region Scoping**: Enforcement of `monitor.region == agent.region` on `/agent/results` and `/agent/backfill` (403 Forbidden on mismatch).
- **16 Prober Types**: Added probers for PostgreSQL, MySQL, Redis, RabbitMQ, PromQL, gRPC, WebSocket, SSH, DNS, TLS cert expiry, Composite, and Synthetic multi-step HTTP scenarios.
- **On-Call & Escalation Engine**: Escalation ladders, on-call schedules with vacation overrides, and acknowledge-to-stop incident handling.
- **Prometheus Alertmanager Receiver**: Inbound webhook receiver (`firing` -> auto-incident, `resolved` -> auto-close by fingerprint).
- **Instance Settings Framework**: Database singleton `instance_settings` for branding, auth policies, SMTP mailer, and global silence toggle.

### Changed
- **OIDC Provider Independence**: Identity provider is now any OpenID Connect issuer (Keycloak, Auth0, Okta, Google, Entra ID) discovered via `oidc.issuer`.
- **Database Schema**: Renamed `keycloak_sub` to `oidc_sub`.
- **Heartbeat Retention & Partitioning**: Switched `heartbeats` to native daily RANGE partitioning with automatic retention purging (`retention_days`).

### Security
- **SSRF Guard**: Prober target resolution validated by [`prober.Guard`](internal/prober/guard.go), blocking cloud metadata (`169.254.169.254`) and link-local ranges by default.
- **AES-256-GCM Secrets at Rest**: Keyring encryption for webhook secrets and channel credentials with zero-downtime key rotation (`cerbix reencrypt`).

---

> **Note on the two entries below.** `v0.35.0` and `v0.10.0` belong to an EARLIER numbering
> scheme and correspond to no git tag in this repository — the tag line is `v0.1.0-beta.*`.
> They are kept because they document real work; treat their version numbers as historical
> labels, not as releases anybody can check out.

## [v0.35.0] - 2026-08-01

### Added
- **Global Search**: Tenant-scoped search endpoint (`GET /api/v1/search`) across monitors, projects, and incidents.
- **p95 Latency**: SLA reporting enriched with `p95_latency_ms` via `percentile_cont(0.95)`.
- **UI 1:1 Design Sync**: Vue 3 SPA views rebuilt matching modern dark-theme design artifacts with bespoke inline-SVG charts.

---

## [v0.10.0] - 2026-07-25

### Added
- **Transactional Outbox**: Outbox delivery pipeline for notifications and incident webhooks with exponential backoff and dead-letter queue.
- **SLA & SLO**: Error budgets, maintenance window exclusion, and daily availability rollup aggregation.
- **Single Binary Multi-Role Execution**: `--role all|api|scheduler|worker` process execution model.
