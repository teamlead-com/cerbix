# Changelog

All notable changes to **cerbix** will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

## [v0.1.6] - 2026-09-01

Two requirements that turn reliability facts into something a pipeline can act on: a **release gate**
that answers whether the error budget allows a deploy, and **change intelligence** that records the
deploy went and lets the service's own facts say what followed. Eighty-five commits since `v0.1.5`.

### ⚠️ Upgrade Notes

- **Two additive migrations, no data repair, nothing to stop.** `00093` creates the gate's policy,
  override and decision tables plus the partition registry; `00094` creates the change tables and adds
  `api_tokens.actions`. Run `cerbix migrate` with the new binary and start as usual — every role applies
  migrations on startup anyway. On a database where no service has a gate policy, nothing evaluates and
  nothing is written ([16eecaf](https://github.com/teamlead-com/cerbix/commit/16eecaf), [c6c74dd](https://github.com/teamlead-com/cerbix/commit/c6c74dd))
- **`00094` builds a UNIQUE index on `incidents`.** The constraint `(id, project_id)` is what lets an
  incident↔change link be tenant-safe by the schema rather than by a query. It is built
  non-concurrently and takes a brief exclusive lock on `incidents`; the guard skips the build entirely
  if an equivalent constraint already exists ([c6c74dd](https://github.com/teamlead-com/cerbix/commit/c6c74dd))
- **The scheduler leader gains one background pass.** Gate-ledger partition maintenance runs on its own
  fenced advisory session — daily partitions created a week ahead, dropped past
  `gate.decision_retention_days` (default 90). Watch `cerbix_gate_decisions_writable_horizon_seconds`
  and `cerbix_gate_decisions_partitions_pending_drop`; a healthy pass keeps the first well above zero
  ([a6a9915](https://github.com/teamlead-com/cerbix/commit/a6a9915))
- **Existing API tokens are unchanged.** The new `actions` allow-list is optional: omitted or `null`
  means the token's role decides, exactly as before. A token is only ever NARROWED by it — the list is
  intersected with the role, never added to it ([7260e68](https://github.com/teamlead-com/cerbix/commit/7260e68))
- **`gate.*` and `change.*` ship with defaults**, so no YAML edit is required. The ones worth knowing:
  gate decisions retained 90 days and purged hourly; change groups retained by whole identity; the
  record endpoint bounded at 300 requests/minute per process and 30 per principal ([16eecaf](https://github.com/teamlead-com/cerbix/commit/16eecaf),
  [c6c74dd](https://github.com/teamlead-com/cerbix/commit/c6c74dd))

### New Features

**Reliability Gate — a deploy asks whether the error budget allows it (FR-024)**

- **One call, one machine-readable answer.** `cerbix gate check --project <id> --service <id>` (or
  `POST /api/v1/projects/{p}/services/{s}/gate`) returns an observed `state` — `ALLOW`, `WARN`,
  `BLOCK`, `UNKNOWN`, `NOT_CONFIGURED` — an effective `action`, every matching reason and the evidence
  under it. The CLI's exit code follows the action (`0` allow/warn, `2` block, `4` not configured,
  `1` transport/auth), so a CI step is one command; credentials come from the environment only
  ([d926568](https://github.com/teamlead-com/cerbix/commit/d926568), [229b33e](https://github.com/teamlead-com/cerbix/commit/229b33e))
- **What blocks is declared, not guessed.** A per-service policy names ONE SLO window and assigns each
  clause of a closed vocabulary — budget exhausted, budget consumed over a threshold, page-burn firing,
  ticket-burn firing, an open service incident — to `block`, `warn` or `ignore`, with a mandatory
  `unknown_behavior` and a seal-lag bound past which the budget is UNAVAILABLE rather than quoted stale
  ([8cb12d6](https://github.com/teamlead-com/cerbix/commit/8cb12d6))
- **The gate derives nothing.** The service row, the policy, the active override, the report, the burn
  latches, the incident and the ledger write all happen in ONE `REPEATABLE READ` transaction whose
  first snapshot-bearing statement is the decision's `evaluated_at` — so a gate answer and the service
  page cannot disagree about the same instant ([8cb12d6](https://github.com/teamlead-com/cerbix/commit/8cb12d6), [b7518b9](https://github.com/teamlead-com/cerbix/commit/b7518b9))
- **An override changes the action and never the facts.** At most seven days, one active per service,
  bound to the policy revision, project-admin only; the decision still records the state that was
  observed and that somebody overrode it ([229b33e](https://github.com/teamlead-com/cerbix/commit/229b33e))
- **Every decision is an immutable ledger row**, in a daily-partitioned bounded table, readable by id
  after the service is renamed or deleted — the moment the evidence is actually wanted. Partition
  maintenance runs as a fenced pass with a 30-second lifecycle and ownership proved by marker rather
  than by OID ([a6a9915](https://github.com/teamlead-com/cerbix/commit/a6a9915), [24e901f](https://github.com/teamlead-com/cerbix/commit/24e901f))
- **In the SPA:** a `Release gate` card on the service page with the policy editor, the latest decision
  and the override panel; a `Gate decisions` browser with a server-side state filter and keyset paging;
  the by-id record; and the per-service override history. Opening a page never creates a decision — only
  a pipeline does ([9793758](https://github.com/teamlead-com/cerbix/commit/9793758), [84adac1](https://github.com/teamlead-com/cerbix/commit/84adac1), [3fc19e3](https://github.com/teamlead-com/cerbix/commit/3fc19e3))

**Change Intelligence — the pipeline says what it changed (FR-025)**

- **A change is a fact about time, not a catalog.** `cerbix change record` (or one `POST`) reports a
  `deploy`, `rollback` or `flag` in one of its phases — `started`, `succeeded`, `failed`, `cancelled` —
  under an external identity `(source, external_id)`, optionally naming the gate decision the release
  rested on. Phases are append-only and idempotent: an identical retry returns the original row, a
  contradictory one is refused by name, and two runners reporting different endings for one run cannot
  both pass ([c6c74dd](https://github.com/teamlead-com/cerbix/commit/c6c74dd), [74ce187](https://github.com/teamlead-com/cerbix/commit/74ce187))
- **The service timeline** is a bounded `[from, to)` read of change groups with an opaque cursor that
  never returns a group twice, each group carrying its live gate decision and the incidents it preceded
  ([7260e68](https://github.com/teamlead-com/cerbix/commit/7260e68))
- **Incident correlation says "preceded", never "caused".** When a service auto-incident opens, the
  changes within the correlation window on that service and on its `probable_root` upstreams are linked
  and named in one `🚀 Changes:` note. It is fail-open in both directions: the incident opens and
  resolves exactly as before whatever the correlation does ([7260e68](https://github.com/teamlead-com/cerbix/commit/7260e68), [3b43b70](https://github.com/teamlead-com/cerbix/commit/3b43b70),
  [f5654d7](https://github.com/teamlead-com/cerbix/commit/f5654d7))
- **Before/after SLI around a change**, computed from SEALED buckets through the same query the
  reliability page uses — never a second implementation. Each side is a figure, or withheld with the
  page's own word, or `pending` until the seal reaches it; a delta only when both sides are figures
  ([c6c74dd](https://github.com/teamlead-com/cerbix/commit/c6c74dd), [5cace76](https://github.com/teamlead-com/cerbix/commit/5cace76))
- **A CI token can be narrower than a role.** An optional `actions` allow-list on an API token is
  intersected with the role in `authz.Can`, so a pipeline token can be exactly "ask the gate, record a
  change" and nothing else ([7260e68](https://github.com/teamlead-com/cerbix/commit/7260e68), [a1cfe00](https://github.com/teamlead-com/cerbix/commit/a1cfe00))
- **In the SPA:** a `Changes` card beside the release gate with terminal-only marks on the facts strip,
  the timeline view, the comparison view, and `Preceded by` on the incident page ([d2e0f3c](https://github.com/teamlead-com/cerbix/commit/d2e0f3c),
  [42ae4ab](https://github.com/teamlead-com/cerbix/commit/42ae4ab))

**Notification channels are edited in place**

- **A channel's name and config are editable**, so rotating a bot token or a hook URL no longer means
  delete-and-recreate — which silently dropped every monitor link, escalation step and alert route
  pointing at the channel. A secret left blank keeps the stored value, because the API never sends one
  out; the merged config is validated, so an edit cannot leave a channel undeliverable ([3e76791](https://github.com/teamlead-com/cerbix/commit/3e76791))

### Improvements

**UI**

- **The alerting panel keeps an operator's unsaved edits** when a late prop arrives instead of
  discarding them ([dd83bfa](https://github.com/teamlead-com/cerbix/commit/dd83bfa))
- **The gate ledger's state filter is the server's**, so a page of results is a page of matches and the
  cursor continues the filtered set ([84adac1](https://github.com/teamlead-com/cerbix/commit/84adac1))

**Documentation & Gates**

- **Every surface now says what the product is.** D-0174's positioning — a service reliability platform,
  not "uptime & SLA monitoring" — reached the README at iter-0160 and stopped there; the CLI's help, the
  OpenAPI description, the overview, the onboarding doc, the systemd unit and the OIDC client all still
  carried the pre-FR-021 framing. Claims that the repository is private are gone with them; they told a
  reader to authenticate for things that need no authentication ([92edc5b](https://github.com/teamlead-com/cerbix/commit/92edc5b))
- **The PRD describes the product as it is now** — services, their incidents, their escalation ladder
  and change intelligence — and points at a roadmap that exists ([4066527](https://github.com/teamlead-com/cerbix/commit/4066527), [59d05c4](https://github.com/teamlead-com/cerbix/commit/59d05c4))
- **`make docs-check` compares the FR-025 acceptance map as a SET** and refuses the spellings the design
  retired, in the specification and in every living document ([056eff5](https://github.com/teamlead-com/cerbix/commit/056eff5), [b320290](https://github.com/teamlead-com/cerbix/commit/b320290))
- **The incident-audit gap has a requirement.** FR-026 / NFR-021 are specified and approved at revision
  four after three review rounds; no product code changes until the iteration opens ([9208171](https://github.com/teamlead-com/cerbix/commit/9208171))

### API · Metrics · Schema

- **API:** eleven gate routes (the decision, the policy CRUD with `expected_revision`, the override
  lifecycle and history, the project-scoped ledger read and listing); four change routes (record,
  timeline, compare, an incident's preceding changes); `ApiToken.actions`; `PATCH
  /api/v1/notification-channels/{id}` now accepts `name` and `config` as well as `enabled`; the
  service detail carries its `sla_targets` inventory.
- **Metrics:** the gate family — `cerbix_gate_decisions_total{state,action,overridden}`,
  `cerbix_gate_decision_duration_seconds` (this project's first histogram),
  `cerbix_gate_evaluate_rejected_total`, `cerbix_gate_evaluate_errors_total`,
  `cerbix_gate_maintenance_errors_total` and four ledger gauges; and the change family —
  `cerbix_change_correlations_total`, `cerbix_change_correlation_errors_total`,
  `cerbix_change_compare_total`, `cerbix_change_record_rejected_total` ([db16dfa](https://github.com/teamlead-com/cerbix/commit/db16dfa)).
- **Schema:** migrations `00093`–`00094` — gate policies, overrides, the daily-partitioned decision
  ledger and its ownership registry; `service_changes`, `incident_changes`, `api_tokens.actions`, and
  `UNIQUE (id, project_id)` on `incidents`.

<sub>85 commits · decisions D-0188…D-0214 · independent review: every FR-024 range approved, FR-025
approved as four effective slices plus the live-evidence correction · full account in
`docs/iterations/iter-0163.md`, `docs/iterations/iter-0164.md`, `docs/iterations/iter-0165.md`</sub>

---

## [v0.1.5] - 2026-08-28

### ⚠️ Upgrade Notes

- **Stop the outbox owners before migrating.** Roles `all`, `api` and `scheduler` run the outbox worker; `worker` and `agent` do not and can keep running. Run `cerbix migrate` once with the new binary, then start the owners. Migration `00088` hands ownership of a class of outbox rows to the database and cannot reach a delivery an old worker already has in flight. Skipping the stop loses nothing; it risks out-of-order delivery for at most one already-claimed batch (≤50 rows) per old owner ([fc6608d](https://github.com/teamlead-com/cerbix/commit/fc6608d), [88ec4ad](https://github.com/teamlead-com/cerbix/commit/88ec4ad))
- **Migration `00090` repairs incident data written by earlier versions.** Incidents resolved and then walked backwards, and service incidents stranded open after their alert ended, are resolved with a `🔧 Repaired:` timeline note. Two classes are reported in the migration output but deliberately left alone — member snapshots that may name a not-yet-governing revision, and auto-incidents with no monitor or service left — because fixing them would mean guessing at history. Queries and the manual procedure are in `docs/runbook.md`. On a database that never hit the races the migration is a no-op ([af7a7a2](https://github.com/teamlead-com/cerbix/commit/af7a7a2))
- **PostgreSQL 15 or newer is required.** 14 is not supported and will not be; `cerbix migrate` refuses before applying anything ([e53244b](https://github.com/teamlead-com/cerbix/commit/e53244b))
- **Armed services keep their coverage across the upgrade.** `00089` seeds delivery tracking as "delivered" for existing rows, so no member monitors start paging in the minute after the upgrade ([7a9e87d](https://github.com/teamlead-com/cerbix/commit/7a9e87d))

### New Features

**Service Alerting: Coverage Means Somebody Was Told**

- **A route must name a live channel.** A schedule pointing at a deleted or disabled channel no longer counts as a route; members keep paging instead of being silenced in favour of a replacement that can reach nobody ([fb9e94b](https://github.com/teamlead-com/cerbix/commit/fb9e94b), [08a8b31](https://github.com/teamlead-com/cerbix/commit/08a8b31))
- **An alert nobody can receive is withheld, not sent into the void.** Counted as `cerbix_service_alert_withheld_total{signal,reason}` with `unroutable` or `no_governing_revision`, and announced as soon as a route exists — on both the health and burn signals ([44dfdad](https://github.com/teamlead-com/cerbix/commit/44dfdad), [a3aecfc](https://github.com/teamlead-com/cerbix/commit/a3aecfc), [19d4943](https://github.com/teamlead-com/cerbix/commit/19d4943))
- **Coverage requires a delivered announcement.** A service suppresses its members only once its own alert *reached* at least one recipient — a channel row that exists is not a delivery, and a 500 from the only channel is not a delivery either ([7a9e87d](https://github.com/teamlead-com/cerbix/commit/7a9e87d), [5ec101b](https://github.com/teamlead-com/cerbix/commit/5ec101b), [35de54d](https://github.com/teamlead-com/cerbix/commit/35de54d))
- **Coverage follows the state the service is in.** A delivered DEGRADED announcement no longer covers a service that has since observed DOWN and is still confirming it ([6e6339f](https://github.com/teamlead-com/cerbix/commit/6e6339f), [46c50de](https://github.com/teamlead-com/cerbix/commit/46c50de))
- **An outage nobody heard about is announced again** once there is somebody to tell — channel deleted mid-flight, every send failed, or retries exhausted — with a fresh recipient list and a new episode. A partial delivery that reached some recipients is never re-sent to them ([23aa3cc](https://github.com/teamlead-com/cerbix/commit/23aa3cc), [425a59f](https://github.com/teamlead-com/cerbix/commit/425a59f), [2b2594c](https://github.com/teamlead-com/cerbix/commit/2b2594c))
- **One vocabulary for "why did my monitor page".** The service badge and `cerbix_alert_delegation_fail_open_total{reason}` are computed from the same clause evaluation. New values `no_owning_service`, `onset_pending`, `onset_undelivered`, `latch_inconsistent`; `stale_lease` on the burn arm now means an expired lease and nothing else ([18e3aef](https://github.com/teamlead-com/cerbix/commit/18e3aef), [3192ebb](https://github.com/teamlead-com/cerbix/commit/3192ebb), [6e0b6a4](https://github.com/teamlead-com/cerbix/commit/6e0b6a4), [4c4bae3](https://github.com/teamlead-com/cerbix/commit/4c4bae3))

**Service Escalation: Repeat Cadence**

- **Services can repeat the last escalation step.** `renotify_seconds` on the service — 0 is off and the default, otherwise 60..86400 — read live, so turning it down mid-incident takes effect immediately. Available in the UI, the API and monitoring-as-code bundles (`alerting.renotify_seconds`). Previously `repeat_last` on a service-attached policy did nothing ([90bf146](https://github.com/teamlead-com/cerbix/commit/90bf146), [425a59f](https://github.com/teamlead-com/cerbix/commit/425a59f))
- **An incident climbs the ladder it started with.** The escalation policy is snapshotted when the incident opens, so editing a policy mid-outage cannot re-time a page already in flight. Incidents open across the upgrade start their ladder at the upgrade instant instead of firing every overdue step at once ([0c6e5dd](https://github.com/teamlead-com/cerbix/commit/0c6e5dd), [d3f428f](https://github.com/teamlead-com/cerbix/commit/d3f428f), [118c55c](https://github.com/teamlead-com/cerbix/commit/118c55c))

**Outbox: Ordered and Bounded Delivery**

- **Incident webhooks are dispatched in order.** Every `incident.*` payload carries `seq`; the claim will not release an event while an earlier one for the same incident is undelivered, and a dead predecessor blocks too. Arrival order over the network is not promised — `(incident.id, seq)` is what lets a receiver dedupe and order, and the runbook describes two receiver strategies ([554e609](https://github.com/teamlead-com/cerbix/commit/554e609), [8ed191a](https://github.com/teamlead-com/cerbix/commit/8ed191a), [02bc005](https://github.com/teamlead-com/cerbix/commit/02bc005), [4762f1a](https://github.com/teamlead-com/cerbix/commit/4762f1a), [64349a0](https://github.com/teamlead-com/cerbix/commit/64349a0))
- **A delivery is bounded by the lease that authorised it.** A deposed worker can no longer keep an HTTP request or SMTP session open while the new owner sends the same event. The lease is measured in database time, the settle is never bounded, and a claim whose turn came after its lease is handed back with its attempt refunded ([b17e0e2](https://github.com/teamlead-com/cerbix/commit/b17e0e2), [425a59f](https://github.com/teamlead-com/cerbix/commit/425a59f), [2b2594c](https://github.com/teamlead-com/cerbix/commit/2b2594c))

### Improvements

**Incident Lifecycle**

- **`resolved` is terminal and status only moves forward**, enforced in the write rather than in a stale read. A plain comment keeps the current status instead of carrying whatever the client last saw ([b21b09f](https://github.com/teamlead-com/cerbix/commit/b21b09f), [183b9f8](https://github.com/teamlead-com/cerbix/commit/183b9f8), [451406b](https://github.com/teamlead-com/cerbix/commit/451406b))
- **Service incidents are a full lifecycle.** They announce open *and* resolve to webhooks and status-page subscribers, and resolve when their alert ends — including through disown and delete, which used to leave them open forever ([e405d85](https://github.com/teamlead-com/cerbix/commit/e405d85), [744980f](https://github.com/teamlead-com/cerbix/commit/744980f))
- **The postmortem names the declaration that governed the outage**, not the newest one; a foreign revision is refused ([0a8ba9b](https://github.com/teamlead-com/cerbix/commit/0a8ba9b), [335d4db](https://github.com/teamlead-com/cerbix/commit/335d4db))
- **Every incident write stamps its times after the row lock**, so a writer that waited cannot date its action before the wait ([3ad2a4b](https://github.com/teamlead-com/cerbix/commit/3ad2a4b), [5ec101b](https://github.com/teamlead-com/cerbix/commit/5ec101b))

**Status Pages**

- **A page, its feed and its subscriber mail agree on what the page reports.** A page made only of Service components used to show an incident and email nobody about it. One project axis now, with the subscriber query as its exact inverse ([c7ce059](https://github.com/teamlead-com/cerbix/commit/c7ce059))

**UI**

- **Search hits bring their workspace.** Opening a monitor or incident from another project switches to it, so edit controls are not hidden from a legitimate editor; detail views follow the URL when navigating between two of the same kind ([2dbc7ce](https://github.com/teamlead-com/cerbix/commit/2dbc7ce))
- **A partly unmeasured hour no longer renders green** on the Reliability card ([27c63f7](https://github.com/teamlead-com/cerbix/commit/27c63f7))
- **The service picker in status-page components works again**, and the escalation form says what "repeat last step" does on a service ([a26260d](https://github.com/teamlead-com/cerbix/commit/a26260d), [a331af0](https://github.com/teamlead-com/cerbix/commit/a331af0))

**Documentation & Gates**

- `make docs-check` compares the FR-021 invariant set in the spec against the discharge map exactly — missing, extra, duplicate or skipped numbers all fail; invariants 92–105 moved into the spec ([46c50de](https://github.com/teamlead-com/cerbix/commit/46c50de), [35de54d](https://github.com/teamlead-com/cerbix/commit/35de54d))
- Four documents that still described the product as it was before FR-022/FR-023 shipped are corrected, and a gate catches the class ([afc8cd1](https://github.com/teamlead-com/cerbix/commit/afc8cd1))

### API · Metrics · Schema

- **API:** `ServiceAlertPolicy.renotify_seconds`; alerting-state `reason` gains `onset_pending`, `onset_undelivered`, `latch_inconsistent`; `incident.*` webhook payloads carry `seq`.
- **Metrics:** new `cerbix_service_alert_withheld_total{signal,reason}`; `cerbix_alert_delegation_fail_open_total{reason}` also emits `error`, `record_failed`, `unspecified` for lookup-level failures.
- **Schema:** migrations `00085`–`00092` — incident escalation snapshots, `incidents.event_seq`, `CHECK ((status = 'resolved') = (resolved_at IS NOT NULL))`, `delivered_seq`/`undelivered_seq` on both service latch tables, `services.renotify_seconds`, `undelivered` as an episode close reason.

<sub>57 commits · decisions D-0175…D-0187 · independent review: product approved at `35de54d`, follow-on work at `eba2b69` · full account in `docs/iterations/iter-0161.md`</sub>

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
