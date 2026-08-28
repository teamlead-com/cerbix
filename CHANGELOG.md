# Changelog

All notable changes to **cerbix** will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

## [v0.1.5-beta.3] - 2026-08-28

An adversarial review of service incidents (FR-022) and service escalation (FR-023) that grew into a
correction arc across the whole alerting spine: incident lifecycle and its clocks, escalation ladders,
alert routability, outbox delivery order, and what it means for a service to *cover* its members. Two
independent review rounds under contract; product approved at `35de54d`, the follow-on work at `eba2b69`.
Decisions D-0175 through D-0187, invariants 92 through 105, migrations `00085`…`00092`. 57 commits.

**Read the upgrade notes before rolling this out.** One of the migrations needs the outbox owners stopped.

### Upgrade notes

- **Stop the outbox owners before migrating across `00088`.** Roles `all`, `api` and `scheduler` run the
  outbox worker; `worker` and `agent` do not and keep running. Run `cerbix migrate` once with the NEW
  binary, then start the new owners. The migration fences a class of outbox rows the database now owns;
  it cannot reach a delivery an old worker already has in flight. Skipping the stop loses no event and
  corrupts nothing — it risks the out-of-order delivery this migration exists to end, bounded by one
  already-claimed batch (≤50 rows) per old owner. `docs/runbook.md` carries the procedure.
- **Migration `00090` repairs durable incident data and reports what it will not touch.** Resurrected
  incidents (a resolution time with a status that walked back) and stranded service incidents are
  resolved, each with a `🔧 Repaired:` timeline note. Two classes are only *counted*, in the migration's
  output, because repairing them would mean guessing at history: member snapshots that may have named a
  not-yet-governing revision, and anchorless auto-incidents. The runbook has the queries and the manual
  procedure. On a database that never hit the races, the repair is a no-op.
- **Existing service latches keep their coverage across the upgrade.** `00089` seeds `delivered_seq =
  emitted_seq` for rows that predate delivery tracking, so no armed service is declared undelivered at
  once. The new rule governs from the first announcement after the upgrade.
- **PostgreSQL 14 is not supported, and that is now decided rather than pending** (D-0183). 15 is the
  floor; the version guard from beta.2 already enforces it. Budget the server upgrade rather than waiting
  for a release that removes the requirement.

### Fixed

*Incidents*

- **`resolved` is terminal in the write, not in a prior read.** Two overlapping writers could commit an
  incident backwards, and nothing at all stopped `identified → investigating` even in sequence. Status
  transitions are now `CanFollow`-checked inside the store's transaction; a plain comment sends no status
  and keeps the current one server-side. The clock moved with it: `statement_timestamp()` after the row
  lock, one instant across the incident, its timeline entry and its outbox row.
- **A service incident announces both ends** — webhooks and status-page subscribers, not only the
  operator's screen — and resolves when the health episode that owns it ends, including through disown
  and delete, which used to close the alert and leave the incident open forever.
- **The postmortem names the declaration that governed the outage**, not the newest one; snapshotting
  refuses a foreign revision fail-closed.
- **An incident climbs the ladder it started with.** The escalation policy is snapshotted at open
  (`00085`); editing a policy mid-outage cannot re-time a page already in flight. Incidents open across
  the upgrade start their ladder at the upgrade instant rather than firing every overdue step at once
  (D-0175, a named legacy exception).
- **Repeat cadence for a service's own ladder** (`services.renotify_seconds`, D-0185). FR-023's D8 had
  declined the knob with a claim that the policy's own repeat covered it — false, so `repeat_last` on a
  service-attached policy did nothing. Zero is off and the default; 60..86400 otherwise; read live rather
  than frozen into the ladder; part of the alerting generation so a change dis-arms until re-evaluated.
  Declarable from a bundle (`alerting.renotify_seconds`), because file-managed services cannot use the UI.

*Alerting and coverage*

- **A route must name a live channel.** Routability was decided by counting array entries, so a schedule
  naming a deleted or disabled channel armed coverage and a member's page was suppressed in favour of a
  replacement that could notify nobody. Both delegation and the recipient resolver now require a channel
  that exists, is enabled and belongs to the project.
- **An onset nobody can receive is withheld, not latched** (D-0176), counted as
  `cerbix_service_alert_withheld_total{signal,reason}` with `unroutable` or `no_governing_revision` —
  different owners, so one number would have sent the wrong person to look.
- **Coverage follows the state the service is IN, and requires an announcement that was DELIVERED**
  (D-0179). A delivered DEGRADED announcement no longer covers a service that has since observed DOWN.
  And an announcement that reached nobody — channel deleted between enqueue and delivery, or every
  send failed — no longer arms coverage: `delivered_seq` is credited only when a send *succeeded* for at
  least one recipient. A channel row that exists is not a delivery.
- **The outage nobody heard about is announced again once there is somebody to tell** (D-0187), on both
  signals. The worker condemns an announcement only on the terminal paths — empty snapshot, zero
  recipients resolved, or retries exhausted — never while a retry is owed, and never one that already
  reached somebody. The evaluator re-announces through the ordinary onset path with a fresh recipient
  snapshot; the episode nobody heard closes as `undelivered`, a new reason.
- **One vocabulary for why a member paged.** The service badge named the failing clause; the delivery
  metric said `no_active_owner` whatever had failed. Both now come from one clause evaluation. New values
  `no_owning_service`, `onset_pending`, `onset_undelivered`, `latch_inconsistent`; `stale_lease` on the
  burn arm means an expired lease and nothing else. §16.6b carries the vocabulary as a normative table
  with an explicit rank column and the fix for each value.

*Outbox*

- **An incident's lifecycle events cannot be dispatched out of order** (D-0177). `incidents.event_seq`
  is stamped into every payload; the claim will not release an event while an earlier event of the same
  incident is undelivered; a dead predecessor blocks too. The fenced class is enforced by the database
  (`00088`), not by whichever binary is inserting. Arrival order is *not* claimed — cerbix orders what it
  controls, and `(incident.id, seq)` in every webhook payload is what lets a receiver dedupe and order.
- **A delivery is bounded by the lease of the claim that authorised it** (D-0186). A deposed worker can
  no longer sit inside an HTTP request or an SMTP session while the row's new owner sends the same
  event. The lease is measured as a span in database time, not by comparing the database's clock with
  the worker's; the settling and recording writes are deliberately *not* bounded; a claim whose turn
  came after its lease is handed back with its attempt refunded.

*Status pages*

- **A page, its feed and its subscriber mail agree about which projects the page reports** (D-0180).
  Three surfaces decided it three ways; a page made only of Service components rendered an incident and
  emailed nobody about it. One axis now — `page.project_id` UNION every component's `source_project`,
  not filtered by source — with the subscriber query as its exact inverse. A manual component with a
  dormant binding keeps reporting its project (D-0184, chosen rather than inherited).

*UI*

- **A search hit brings its tenant, and a detail view follows the URL.** A monitor or incident hit from
  another workspace landed on a page whose reads and permission checks ran against the previous tenant,
  hiding acknowledge, resolve and postmortem from a legitimate editor. Navigating between two of the same
  kind changed the address bar and nothing else. Both detail views now watch the route and the workspace,
  guard against a slower response for the previous subject, and the `RouterView` is keyed by path.
- **A partly unmeasured hour no longer renders green** on the Reliability card.
- **The escalation form says what `repeat_last` does on a service** beside the control.

### Changed

- **API:** `ServiceAlertPolicy` gains `renotify_seconds`; the alerting-state `reason` enum gains
  `onset_pending`, `onset_undelivered`, `latch_inconsistent`; webhook `incident.*` payloads carry `seq`.
- **Metrics:** `cerbix_service_alert_withheld_total{signal,reason}` is new;
  `cerbix_alert_delegation_fail_open_total{reason}` uses the shared clause vocabulary plus three
  operational values (`error`, `record_failed`, `unspecified`) that have no badge counterpart.
- **Schema:** `incidents` gains `event_seq` and `CHECK ((status = 'resolved') = (resolved_at IS NOT
  NULL))`; both service latch tables gain `delivered_seq` and `undelivered_seq`; `services` gains
  `renotify_seconds`; `service_alert_episodes.close_reason` admits `undelivered`.
- **Documentation gate:** `check-docs-references` now derives the FR-021 invariant set from the spec and
  compares it exactly with the discharge map — missing rows, extra rows, holes and duplicate numbers all
  fail. Invariants 92–105 moved into `func-service-reliability.md` §16.8, where a requirement lives.

### Known limits

- Two classes of pre-existing incident damage are reported by `00090` and not repaired; see the upgrade
  notes and `docs/runbook.md`.
- The outbox bounds *dispatch*; it cannot bound *arrival*. A receiver that wants exact ordering needs
  the `seq` field (`docs/runbook.md` describes two receiver strategies and what each loses).

---

## [v0.1.5-beta.2] - 2026-08-19

Three defects found by running `v0.1.5-beta.1` in production rather than by a test. The first blocked the
upgrade outright.

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
