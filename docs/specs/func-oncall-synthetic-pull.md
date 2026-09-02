# Spec: On-call/escalations · Synthetic checks · HTTP pull agent (func-oncall-synthetic-pull)

A single spec for three product gaps. The features are independent (different seams), so they are split into sections
**A / B / C** with their own FR/NFR/AC and their own phases; at the end — a common DoD and an explicit "out of scope for this pass".
Implemented on top of the fixed architecture (roles `all/api/scheduler/worker`, `Dispatcher`
inproc/amqp, transactional outbox, strict config, secrets-at-rest). During implementation each section
produces its own `decisions.md` entries (approximately **D-0089+**) and traceability rows.

---

## A. On-call / escalations  ✅ IMPLEMENTED (D-0089)

> Status: delivered and verified (unit + `-race` 29 packages + E2E). Details — `decisions.md` D-0089,
> the traceability row, `overview.md` §3.1/§3.5. Remains deferred (per spec): multiple rotations/vacation
> overrides in on-call (MVP — one rotation).

### A.0 Problem (current state)

Notifications are "flat": monitor down → outbox (`TopicMonitorTransition`) → `notify.Dispatcher` sends to
**all** channels attached to the monitor; repeats — via a single `renotify_seconds` flag (the leader tick
`EnqueueRenotifyReminders`). There are no: **escalation tiers** (escalate if nobody reacted within N minutes),
**on-call schedules** (whom to page right now), **acknowledgement (ack)** (stop the escalation). Channels —
`webhook/slack/telegram/email` (`domain.ChannelType`, per-project).

Explicitly **out of** this pass (confirmed earlier): our own outgoing integrations into external on-call/chat systems.
Escalation **reuses** the existing channels as targets; outward — only via a
webhook channel, if the admin sets one up themselves.

### A.1 Model

- **Escalation policy** (per-project) — an ordered list of **steps**: `{after_seconds, targets[]}`.
  Step 0 is usually `after=0` (immediately). A step's target is an existing `notification_channel` **or** an on-call
  schedule (see below). A policy is attached to a monitor (field `escalation_policy_id`, nullable —
  without a policy the current flat delivery works, backward compatibility).
- **On-call schedule** (per-project) — a rotation: a list of participants (target channels) + a shift rule
  (`weekly`/`daily`/`custom` shift length, start anchor, TZ). Resolving "who is on now" is a pure function of
  time (deterministic, testable). MVP: one rotation without overrides/vacations.
- **Acknowledgement** — the auto-incident (there already is `FindOpenAutoIncidentByMonitor`) gets
  `acknowledged_at`/`acknowledged_by`. Ack stops further escalation steps. Ack sources:
  the UI (a button on the incident), an API token, and (reusing Tier-4 inbound) — an alert resolve.
- **Escalation state** — on an open auto-incident: `escalation_step`, `next_escalation_at`.
  A leader tick (following the pattern of `EvaluateBurnAlerts`/renotify) advances the steps: if the incident is open, not
  acked and `now >= next_escalation_at` — enqueue the selected target of the step and shift to the next.

### A.2 Requirements

**FR**
- **FR-ESC-1** — CRUD of escalation policies (per-project, editor+): steps `{after_seconds, targets[]}`,
  validation (non-decreasing after, non-empty targets, existing channels/schedules).
- **FR-ESC-2** — CRUD of on-call schedules (per-project): participants + rotation rule; an endpoint
  "who is on now / at time T".
- **FR-ESC-3** — A monitor references a policy (`escalation_policy_id`, nullable). Without a policy —
  the previous flat delivery (backward compatibility).
- **FR-ESC-4** — Escalation is advanced by the leader: an open unacked auto-incident, upon reaching
  `next_escalation_at`, sends the target of the current step and moves to the next; after the last one — stop
  (or repeat of the last per `renotify`, a policy config flag).
- **FR-ESC-5** — Incident ack (UI/API/inbound-resolve) sets `acknowledged_at/by` and
  **stops** the escalation; recovery (monitor up / incident closed) also stops it.
- **FR-ESC-6** — Resolving an on-call target into a concrete channel is a deterministic function of time.

**NFR**
- **NFR-ESC-1** — Step advancement is **edge-triggered + a latch** on the incident (no duplicates per tick),
  in a transaction with the outbox (like burn-alerts). Idempotent under a repeated leader.
- **NFR-ESC-2** — Respects **global silence** and maintenance suppression (like other alerts).
- **NFR-ESC-3** — The single owner of policy/schedule validation is `domain` (strict, no fallback).

**AC**
- **AC-ESC-1** — Policy/schedule round-trip (store+api), invalid ones → 400.
- **AC-ESC-2** — Simulation: an incident is open, step0 immediately, step1 after N — once N has passed the leader
  enqueues the second target exactly once; ack before N — the second step is **not** sent.
- **AC-ESC-3** — The on-call "who is on now" resolve is stable and tested on shift/TZ boundaries.
- **AC-ESC-4** — Without a policy the monitor's behavior does not change (a regression test of flat delivery).

### A.3 Affected files (guideline)
`domain/escalation.go` (Policy/Schedule + Validate + on-call resolve), `domain/incident.go`
(ack fields), `store/migrations/00035_escalation.sql` (policies, steps, schedules, incident ack +
escalation_step/next_escalation_at, monitors.escalation_policy_id), `store/escalation.go`
(CRUD + `AdvanceEscalations` edge-triggered), `store/incidents.go` (ack), `outbox/outbox.go`
(a new topic `escalation_step` → notify of the selected target), `domain/outbox.go` (payload),
`scheduler/scheduler.go` (the `AdvanceEscalations` tick), `api/handlers_escalation.go` +
`handlers_incidents.go` (ack), `openapi.yaml`(+schema.d.ts), frontend: Settings→Escalation/On-call,
an Ack button on the incident.

---

## B. Synthetic checks (scripted, no browser)  ✅ IMPLEMENTED (D-0090)

> Status: delivered and verified (unit + `-race` + E2E). Details — `decisions.md` D-0090.

### B.0 Problem

The catalog of 15 types consists of single-step probes (`prober.Prober.Probe`, registry `map[MonitorType]Prober`).
There are no **multi-step scenarios** (login→action→check): a single `GET /health` = 200 does not mean that
a user flow (login, a critical API path) actually works.

### B.1 Model

A new type `MonitorSynthetic`: a scenario of steps `{method, url, headers, body, extract[], assert[]}`;
between steps — extraction of values (JSON-path / regex / header) into a context and substitution into subsequent
steps; asserts on status/body/latency. Executed by **any** worker (including geo) on the existing
stack — a new `Prober` in the registry, the scenario in `monitors.config` (secrets encrypted as today). Does not break
a single cerbix invariant (static binary, DB-less worker, geo pools) — it is an enriched http prober, not a
new genre.

### B.2 Requirements

**FR**
- **FR-SYN-1** — Type `synthetic`: a scenario of steps with extract/assert; Normalize/Validate in `domain`;
  the scenario/secrets in `config` (encryption like the other types).
  **This sentence was FALSE OF THE CODE from the day it was written until 2026-09-02, and is now true
  (FR-028 stage 1, D-0216).** Only `config["password"]` was encrypted and rotation covered only that
  set, so a scenario's secrets were cleartext at rest and were returned to any principal who could read
  the monitor. `sec-synthetic-secrets.md` stage 1 made the scenario ciphertext at rest, covered by
  rotation and by an idempotent startup backfill, and withheld from a principal who cannot write the
  monitor; stage 2 made a DECLARED credential a named binding resolved from the project inventory. What
  the sentence still does not promise: an undeclared literal in a header nobody would call a credential
  header, or in a body, is not detectable and is not refused — it is protected by encryption and read
  scoping only (FR-028 D7).
- **FR-SYN-2** — A synthetic probe yields a single `Heartbeat` (up/down + the latency of the whole scenario +
  a msg with the failed step/assert). The monitor's conditions are applied on top (as today).
- **FR-SYN-3** — Synthetic executes on a regular worker, including geo (region-aware, like all Active).

**NFR**
- **NFR-SYN-1** — The time budget of the whole scenario is bounded by `timeout_seconds` (a hard deadline on the chain).
- **NFR-SYN-2** — The step-by-step result does not leak secrets into heartbeat.msg (redaction).
  **Held for the assert path and NOT for the transport path until FR-028 stage 0 (D-0216):** a failed
  connection returned `err.Error()`, and `net/http` embeds the request URL, so a step URL's query
  string reached the message. Now every probe result is composed from a bounded failure class plus a
  host, for every type — `internal/prober/failure.go`.

**AC**
- **AC-SYN-1** — Scenario round-trip; multi-step with extract→substitution passes against a stub server
  (up), an assert failure → down with the step indicated (unit + httptest).
- **AC-SYN-2** — A synthetic monitor in `region=geo1` is executed by a geo worker (reuses the geo E2E).
  **NOT DISCHARGED, recorded 2026-09-02; narrowed 2026-09-03:** `e2e/tests/synthetic-bindings.spec.ts`
  now creates and edits a synthetic monitor, so the type has browser coverage — but on the CORE region
  only. Nothing puts one on a geo worker, which is what this criterion actually asks. Region affinity holds by construction — `synthetic` is an ordinary prober
  registry entry with no region rule of its own — and that is an argument, not evidence. The row in
  `docs/status.md` for FR-SYN-3 says the same rather than implying coverage.

### B.3 Affected files (guideline)
`domain/monitor.go` (type `synthetic` + Active/NeedsTarget), `domain/synthetic.go` (step/extract/assert
structures + Validate), `prober/synthetic.go` (a new Prober, registration in
`NewRunnerWithGuard`), `openapi.yaml`(+schema), frontend: a scenario builder in the monitor form. The prober
registry is already extensible — minimal core touch.

---

## C. HTTP pull agent (an alternative geo transport without RabbitMQ)  ✅ IMPLEMENTED (D-0091)

> Status: delivered and verified (unit + `-race` + E2E). Details — `decisions.md` D-0091. Remains
> deferred (per spec): per-region scope of the agent token; RabbitMQ federation as yet another transport.

### C.0 Problem

A geo worker currently connects to the **central RabbitMQ** (`dispatch.AMQP`). If exposing the broker to
another geo is undesirable (network/policies), a transport is needed where the remote agent talks to the core
**only over HTTPS to the API** (outbound from the geo, pulls jobs and posts results). This is a third transport
alongside `Dispatcher` (inproc/amqp), declared "deferred" in the geo spec.

### C.1 Model

- **Region.transport** — a region has a logical transport: `amqp` (default) or `pull`. Determined
  by how the pool is brought up (a worker/agent flag), not by a monitor field. A monitor still carries only
  `region`.
- **Server-side pull queue** — for pull regions the scheduler puts a ready `CheckJob` (a snapshot
  of the monitor) into a **DB table** `pull_jobs(region, payload, expires_at, claimed_at, agent_id)` instead of/in
  addition to AMQP. TTL = `expires_at` (like AMQP-Expiration): expiry ⇒ the next tick re-publishes.
- **Agent API** (agent bearer token, per-region scope):
  - `GET /api/v1/agent/jobs?region=R&max=N` — long-poll: atomically claims up to N unexpired jobs
    of the region (`FOR UPDATE SKIP LOCKED`), marks `claimed_at/agent_id`, returns the snapshots.
  - `POST /api/v1/agent/results` — accepts `Heartbeat[]`, feeds them into the **same ingest path** as
    AMQP results (no separate evaluation logic).
  - `GET /api/v1/agent/regions/heartbeat` — the agent's "I'm alive" (feeds the region-worker alert analogously to
    consumer detection, see below).
- **Agent** — the same binary in a new mode `--role agent --region R --server https://core --token …`:
  DB-less, without RabbitMQ; loop: poll jobs → prober.Runner → post results. Reuses `prober` entirely.
- **Liveness for the alert** — a region in pull mode has no consumer in RabbitMQ, so D-0088 (the alert
  "region without a worker") gets a second liveness source: a fresh `agent heartbeat` in the DB. `LiveRegions`
  becomes the union of {RabbitMQ consumers} ∪ {agents with a recent heartbeat}.

### C.2 Requirements

**FR**
- **FR-PULL-1** — Mode `--role agent` (DB-less): poll `GET /agent/jobs`, execution via `prober`,
  `POST /agent/results`; authentication with a bearer token bound to a region.
- **FR-PULL-2** — For pull regions the scheduler puts jobs into `pull_jobs` with a TTL; claim —
  `FOR UPDATE SKIP LOCKED`, no re-issuing of expired ones (re-published on the next tick).
- **FR-PULL-3** — Agent results feed into the shared ingest (metrics/incidents/escalations — same as AMQP).
- **FR-PULL-4** — Agent tokens: issuance/revocation (reuse `api_tokens`, adding scope
  `agent:region`), storage — like other secrets.
- **FR-PULL-5** — Pull-region liveness via agent heartbeat is included in the `LiveRegions` source
  (feeds the region picker and the D-0088 alert).

**NFR**
- **NFR-PULL-1** — Only outbound agent connections (HTTPS to core); the broker is not exposed to the geo.
- **NFR-PULL-2** — The claim is atomic and concurrency-safe (multiple agents per region); one job —
  one agent.
- **NFR-PULL-3** — The transport is abstracted: the core (scheduler/ingest) does not distinguish amqp/pull behind the seam
  (publish into pull_jobs vs a queue; results — a single path).
- **NFR-PULL-4** — A geo worker/agent for internal targets: `prober.allow_private_ips: true` (as in the geo spec).

**AC**
- **AC-PULL-1** — An agent claims only its own region; foreign/expired jobs are not returned; a double claim
  of one job is impossible (a concurrent test of `SKIP LOCKED`).
- **AC-PULL-2** — An agent's result produces a heartbeat and opens/closes an incident the same way as AMQP.
- **AC-PULL-3** — E2E: `scheduler` (pull region) + `agent --region pull1` **without RabbitMQ access**
  from the geo — a monitor with region=pull1 is probed by the agent; the picker/alert see the region as live via heartbeat.
- **AC-PULL-4** — Token revocation immediately closes the agent's access (401).

### C.3 Affected files (guideline)
`store/migrations/00036_pull_agent.sql` (+ api_tokens scope), `store/pulljobs.go` (enqueue/claim/expire),
`domain/` (agent token scope, region transport), `scheduler/scheduler.go` (for pull regions —
enqueue into pull_jobs), `api/handlers_agent.go` (jobs/results/heartbeat + token-auth middleware),
`ingest` (shared result intake — reuse), `cli/cli.go` (`--role agent`, flags
`--server/--token`), `mqadmin`/`LiveRegions` (union with agent heartbeat), `deploy` (an example
`config.agent.yaml`), `openapi.yaml`(+schema), `docs/overview.md` (§2.4 — the pull transport variant).

---

## Phasing (recommended order)

1. **A. Escalations/on-call** — the greatest product value, clean seams (outbox/notify/incidents).
2. **B. Synthetic (scripted)** — cheap catalog extensibility, no new image.
3. **C. Pull agent** — removes the "RabbitMQ in every geo" requirement; medium complexity (a new transport+API).

Each phase is a self-contained pass with its own D-/traceability and a green `-race`; the phases are independent, the order
can be changed.

## Common Definition of Done (for each phase)

- FR/NFR/AC = DONE, linked to code+tests; full `-race` green; `gofmt`/`vet`; frontend
  `vue-tsc`+`vite build`; `openapi.yaml`↔`schema.d.ts` synchronized; `decisions.md`(a new D-) +
  `traceability.md` + `overview.md` updated; strict config/validation before business logic; **no
  self-healing/fallback**; secrets (scenarios, tokens) — via `secret.Cipher`, included in `reencrypt`
  and `TruncateAll`; new outbox topics — in the CHECK whitelist via a migration; new ticks respect global silence.

## Explicitly out of this pass

- Outgoing integrations into external on-call/chat systems as targets (only via a webhook channel, if the admin sets one up).
- Multi-region failover, auto-discovery of pools, multiple rotations/vacation overrides in on-call (MVP — one rotation).
- RabbitMQ federation as yet another geo transport (pull covers the "no broker in the geo" case).
- **Browser / headless checks (real rendering, JS, clicks)** — deliberately outside cerbix: they pull in Chromium
  (breaking the static distroless image and the light DB-less worker) and shift the product into the synthetic-platform genre.
  Synthetic (scripted HTTP) covers user-flow verification without that weight.

## Open decisions (to approve before starting the corresponding phase)

- **A:** repeating the last escalation step per `renotify` — yes/no (a policy flag). Proposed: yes, optional.
- **C:** a separate `--role agent` (the proposal) **or** a transport switch on the existing
  `--role worker`; the token scope `agent:<region>` in `api_tokens` vs a separate entity.
