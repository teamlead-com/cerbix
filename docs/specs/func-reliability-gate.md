# func-reliability-gate — a deploy asks whether the error budget allows it (FR-024 / NFR-019)

> **Lifecycle: DESIGN GATE CLOSED (2026-08-28) — implementation may begin.** D1–D8 and D14 were settled
> in the design pass (owner, `cerbix-dev`, `cerbix-reviewer`); D9–D13 were closed by the owner the same
> day (D-0188). Two gates remain in front of code, per the project's process: the independent design
> review of this document, which is in progress, and — because the policy editor and the decision history
> are SPA surfaces — an approved UI mock before any frontend code. Backend work may start on the
> reviewed contract. Change Intelligence — deployment events, service timeline, incident correlation —
> is **FR-025**, a separate requirement that this one does not depend on and must not grow into.

## 1. What this is, in one paragraph

cerbix already computes, per service, everything a release decision needs: whether the error budget is
intact and how fast it is burning (the burn evaluator, FR-021 phase 2), whether the service is currently
covering its members or paging through them (the coverage conjunction, FR-021 §16.1), whether a service
incident is open (FR-022), and how fresh all of that is (the seal watermark and the evaluators' leases).
What it does not do is turn those facts into a **decision a pipeline can act on**. The Reliability Gate
is that: a deterministic, machine-readable answer — `ALLOW`, `WARN`, `BLOCK`, `UNKNOWN`, or
`NOT_CONFIGURED` — derived from facts cerbix has already sealed and evaluated, with every reason and
every timestamp attached, asked immediately before a protected pipeline step.

It is a thin slice on purpose. It adds a policy table, an override table, a decision endpoint and a CLI
verb. It adds **no new computation of reliability** and **no new store of events**.

## 2. Requirements

- **FR-024 — Reliability Gate.** An operator declares, per service, a policy naming which reliability
  facts block a release and which merely warn. A pipeline, authenticated with an API token, asks
  `POST /api/v1/projects/{p}/services/{s}/gate` (or `cerbix gate check`) and receives a decision with all
  matching reason codes and the evidence they rest on. A privileged operator may record a time-bounded
  override; the gate reports that it was used. Every decision, policy change and override is audited.
- **NFR-019 — the gate never disagrees with the screen.** `coverage_state`, budget and burn in a gate
  response are the SAME values the service page and the alerting badge show at that instant, because they
  come from the same code. A gate that computes its own number is a second semantics owner, and this
  project has already paid for that class of defect once (iter-0161 §34).

## 3. The decisions

**D1 — the gate reads two existing owners and computes nothing. DECIDED.**
Reliability facts have two owners in the code today and the gate consumes both as they are:

- **budget and its withholding** — `decideServiceWindow` in `internal/store/servicereport.go`, the ONE
  owner of "may this number be quoted", already shared by the authenticated report and the public status
  page (AC-0150-4). It yields availability against the objective, the remaining-budget ratio, and a
  `withheld_reason` when the number may not be stated;
- **burn and coverage** — the burn latch `service_burn_alert_state` (`last_verdict` fire/clear/hold per
  rule, `firing`, `lease_until`, computed over SEALED facts) and `serviceCoverageClauses` in
  `internal/store/servicealertstate.go`, which produces the per-signal `armed`/`reason` the badge shows.

The gate has no formula of its own for any of these. If a clause the policy needs is not exposed by one
of the two owners, the owner is extended and the badge gains it too — the gate is never the first place a
fact is derived. **Invariant, not guideline** (§6.1).

**D2 — five states, and two of them are not policy outcomes. DECIDED.**

| State | Meaning | CLI exit |
|---|---|---|
| `ALLOW` | a policy exists and no blocking or warning clause matched | 0 |
| `WARN` | a policy exists and at least one warning clause matched, no blocking one | 0, with the reasons on stderr (OPEN: see D10) |
| `BLOCK` | a policy exists and at least one blocking clause matched | 2 |
| `UNKNOWN` | a policy exists and an authoritative answer is NOT available now (no sealed facts, stale lease, evaluator error, no governing declaration) | per the policy's `unknown_behavior` |
| `NOT_CONFIGURED` | the service has no gate policy | 4 |

`NOT_CONFIGURED` is neither `ALLOW` nor `WARN`. It carries `reason: not_configured` and a documentation
link, and has its own exit code, because a pipeline that opted into the gate and reads "no policy" as
"green" has been told a lie one step removed. Which interpretation to apply is the integration's choice,
made visibly.

**D3 — `unknown_behavior` is mandatory and has no default. DECIDED.**
A policy is created with `unknown_behavior: warn | block`, explicitly, or it is refused. A silent default
of `block` breaks onboarding (a fresh installation has no sealed facts for hours); a silent default of
`warn` is a fail-open nobody chose. The operator states which failure they prefer, once, per service.

**D4 — a decision is a point-in-time advisory input, never a reservation. DECIDED.**
The gate does not reserve budget, does not hold a lock across the deploy, and does not know whether the
deploy happened. The pipeline asks **immediately before** the protected step and stores the returned
`decision_id` as evidence. There is a TOCTOU window between the answer and the step; it is named, it is
the pipeline's, and v1 does not try to close it — a gate that coordinates deployments has become a
deployment system.

**D5 — the answer carries its own evidence. DECIDED.**
Every response, whatever the state, includes: `decision_id`, `evaluated_at`, `as_of` (the seal watermark
the budget rests on), `sealed_through`, both freshness leases (`coverage_lease_until`,
`burn_lease_until`), `policy_revision`, `service_definition_revision`, `coverage_state` per signal, and
**every** matching reason code with the value that matched it — not the first one found. `valid_until`
is present only when derivable as the minimum of the applicable leases; an arbitrary TTL is forbidden.

**D6 — the sealed contract has a price, and the price is stated rather than hidden. DECIDED.**
cerbix's SLO window ends at the seal watermark, never at `now` (README, PRD). So a gate asked at 14:03
answers with the budget as of the last seal, and an outage that began between `sealed_through` and now is
not yet visible to it. This is the cost of quoting only sealed numbers and it is accepted. It is
**forbidden** to "fix" the gate by reading raw heartbeats: that would make it the one surface in the
product that quotes an unsealed number, and the whole seal discipline exists so there is none.

**D7 — an override is a durable privileged mutation, not a request parameter. DECIDED.**
A pipeline that can pass `override=true` can always approve itself. An override is created through its
own endpoint by a principal with the override role (D9), with `actor`, `reason` and `expires_at` — all
mandatory, no default expiry — and audited in the same transaction. The gate only READS an active
override, applies it, and returns its id under `evidence.override`. `cerbix_gate_decisions_total{state,
overridden}` makes overridden decisions visible as a rate.

**D8 — precedence is deterministic and the response is total. DECIDED.**
`BLOCK` > `WARN` > `ALLOW`, evaluated over ALL clauses; `UNKNOWN` is reached only when a clause the
policy depends on cannot be answered, and only a declared policy can produce it. "Open service incident"
and "fast burn firing" are separate clauses with separate reason codes and are never inferred from one
another.

**D9 — who may do what. DECIDED (owner, 2026-08-28).**
Any token with `viewer` or above on the project may ASK the gate — it is a read of facts the token could
already read on the service page. `editor` or above writes the policy. `project_admin` or above creates
and revokes an override. **No dedicated `gate` token role in v1.** It would be new surface for a scenario
we do not have; if CI secrets prove less trusted than operator ones, a narrower role can be added
additively without breaking anything, whereas taking reads away from existing tokens later cannot.

**D10 — CLI exit codes and shape. DECIDED (owner, 2026-08-28).**
`cerbix gate check --project <p> --service <s> [--json]`. Exit **0** for `ALLOW` and `WARN` — the
warning's reasons go to stderr and into the JSON; **2** `BLOCK`; **3** `UNKNOWN` (or 2 when the policy's
`unknown_behavior` is `block`); **4** `NOT_CONFIGURED`; **1** transport or auth error. `WARN` shares exit
0 by the shell convention that non-zero means "do not continue": a warning is by definition "continue,
but know this", and a distinct code would turn every warning into pipeline handling that people would
soon silence. This is fixed now because changing it later breaks every pipeline that reads it.

**D11 — what a policy may say. DECIDED (owner, 2026-08-28) — this is the one that shapes the product.**
A policy is a small closed vocabulary of clauses, each assigned `block`, `warn` or `ignore`. The shipped
defaults are the first column of assignments below:

| Clause | Source (D1) | Proposed default |
|---|---|---|
| `budget_exhausted` — remaining ratio ≤ 0 | `decideServiceWindow` | block |
| `budget_below` — remaining ratio below the policy's threshold (`budget_below_ratio`, default 0.10) | `decideServiceWindow` | warn |
| `fast_burn_firing` — a `page`-severity burn rule is FIRING | burn latch | block |
| `slow_burn_firing` — a `ticket`-severity burn rule is FIRING | burn latch | warn |
| `service_incident_open` — an unresolved auto-incident for this service | incidents | warn |
| `budget_withheld` — the number exists but may not be quoted (`withheld_reason`) | `decideServiceWindow` | → UNKNOWN |
| `facts_stale` — a lease expired or no evaluation exists | both leases | → UNKNOWN |
| `no_objective` — no service-scoped SLA target | `decideServiceWindow` | → UNKNOWN |

Three choices inside it, each with its reason:

- **An open incident WARNS, it does not block.** An open incident is very often the reason a deploy is
  happening — the fix. A gate that blocks the fix because of the incident it fixes works against its own
  purpose. An installation that wants the stricter reading sets `block` in its policy.
- **`budget_below` carries a per-policy threshold**, default 0.10. One fixed number cannot serve a 99.99
  service and a 99.5 one alike.
- **`coverage_not_armed` is NOT in the vocabulary.** It states who is paging right now, which is a fact
  about alert delivery and not about reliability; admitting it would let a policy confuse the two
  questions this product keeps apart. `coverage_state` stays in the response as EVIDENCE (D5), because
  an operator reading a decision wants to know it — it just cannot decide anything.

**D12 — policy scope. DECIDED (owner, 2026-08-28).**
Per service, no inheritance. A project-level default that services inherit is a reasonable second step
and a trap as a first one: it adds a three-level resolution and the question of which revision a gate
response cites when the inherited policy is later overridden. v1 answers with one row and one revision.

**D13 — decisions are persisted. DECIDED (owner, 2026-08-28).**
A `decision_id` that cannot be looked up is evidence that cannot be produced, which is not evidence.
Every decision is stored — state, reasons, evidence, policy revision, override id, `evaluated_at`,
`as_of` — with a bounded retention on the SLA-report precedent, and read back through
`GET /api/v1/projects/{p}/services/{s}/gate/decisions/{id}`. The row is small; the table grows with deploy
frequency, which is the thing it exists to account for.

**D14 — Change Intelligence is FR-025 and is not started by this requirement. DECIDED.**
Deployment/rollback/flag events, the service timeline, incident correlation and before/after SLI comparison
extend `IncidentContext` (§14) with append-only phases keyed `UNIQUE(service_id, source, external_id,
phase)`, a domain-owned phase order, serialization per external identity, and closed enums with no free
payload. None of that is needed for a decision and none of it ships here.

## 4. What must move WITH the implementation, not after it

- `docs/overview.md` — the gate endpoint and CLI verb in the API summary; the two-owner rule under the
  reliability section.
- `docs/runbook.md` — a "the gate says UNKNOWN" section (it means the facts, not the pipeline); the
  override procedure and how to see overridden decisions in the metric.
- `func-service-reliability.md` §16.8 — invariant 106: the gate is a consumer of the two owners and
  derives nothing (the invariant set is compared exactly by `make docs-check`, so the row lands in
  `docs/traceability.md` in the same change).
- `openapi.yaml` — the decision, policy and override schemas; regenerate `frontend/src/api/schema.d.ts`.
- README — one sentence under the reliability platform positioning: cerbix decides, the pipeline acts.

## 5. Schema (draft — decided against D11–D13; column-level detail belongs to the implementation)

```
service_gate_policies      (service_id PK → services, project_id, unknown_behavior CHECK IN (warn, block),
                            clauses jsonb CHECK closed-vocabulary, budget_below_ratio numeric NULL,
                            revision bigint DB-owned, updated_at, updated_by)
service_gate_overrides     (id, service_id, project_id, actor_user_id NULL, via_token bool, reason text NOT NULL,
                            created_at, expires_at NOT NULL, revoked_at NULL, revoked_by NULL)
service_gate_decisions     (id, service_id, project_id, policy_revision, state, reasons jsonb, evidence jsonb,
                            override_id NULL, evaluated_at, as_of)             -- retained, bounded (D13)
```

Tenant-composite FKs to `(services.id, project_id)` as every service-scoped table since 00085; a foreign
or malformed id answers `ErrNotFound` uniformly, as the alerting endpoints do.

## 6. Acceptance invariants (FR-024) — draft, numbered on acceptance

1. the gate computes NO reliability fact of its own: budget, withholding, burn and coverage in a response
   are produced by `decideServiceWindow`, the burn latch and `serviceCoverageClauses`, and a response
   never quotes a number the service page would withhold;
2. `NOT_CONFIGURED` is distinct from every policy outcome, has its own reason and exit code, and is never
   rendered as `ALLOW` or `WARN`;
3. `UNKNOWN` is produced only under a declared policy and resolves through that policy's explicit
   `unknown_behavior`; a policy without one cannot be created;
4. a response carries every matching reason, not the first; precedence is `BLOCK` > `WARN` > `ALLOW`;
5. a response carries `decision_id`, `evaluated_at`, `as_of`, `sealed_through`, both leases and both
   revisions; `valid_until` appears only as the minimum of applicable leases;
6. the gate reads only SEALED facts and this is enforced by construction (no heartbeat access from the
   decision path), not by review;
7. an override is created only through its own endpoint, by an authorised principal, with actor, reason
   and expiry, audited in-transaction; it is never honoured from a gate request body;
8. an expired or revoked override is not applied; an applied one is named in the response and counted;
9. a decision is point-in-time: nothing is reserved, nothing is locked, nothing is rolled back;
10. tenant scope: a foreign, unknown or malformed service id answers `ErrNotFound` from every gate
    endpoint alike, so existence never leaks;
11. policy and override writes bump a DB-owned revision and are audited with before/after, actor and
    token attribution, exactly as paging-config writes are.

## 7. Required test matrix (written before the code)

a service with a policy and an intact budget: `ALLOW`, all evidence fields present · the same service
with a `page` rule firing: `BLOCK` with `fast_burn_firing`, AND the `ticket` clause reported too if it
matches (total response) · budget below threshold only: `WARN` · both a warn and a block clause: `BLOCK`,
both reasons · no sealed facts yet, policy says `unknown_behavior: block`: `UNKNOWN` → exit 2; says
`warn`: `UNKNOWN` → exit 3 · no policy: `NOT_CONFIGURED`, exit 4, never `ALLOW` · the badge says
`unroutable` and the gate's `coverage_state` says `unroutable` — the SAME string from the same call ·
the report withholds the availability number and the gate quotes none · an override created through its
endpoint flips `BLOCK` to `ALLOW` with `evidence.override.id` set; the same override in a gate request
body is ignored and the request is refused as unknown-field · an expired override changes nothing · a
foreign service id: 404 on decide, policy and override alike · policy created without
`unknown_behavior`: 400 · MUTATIONS for each: a gate that recomputes budget locally is caught by the
parity test; a first-match `switch` in precedence is caught by the two-reasons case; an override read
from the body is caught by the unknown-field test.

## 8. Threat model

| Threat | Mitigation |
|---|---|
| Pipeline approves itself | override is not a request parameter (D7); it needs a privileged principal and leaves an audit row and a metric |
| Stolen CI token | can ask the gate (a read the token could already make on the service page) and nothing more unless it also holds the override role; D9 asks whether a narrower `gate` role is wanted |
| Policy deleted mid-pipeline to force a lenient answer | the answer becomes `NOT_CONFIGURED`, which is not `ALLOW` and has its own exit (D2); the deletion is audited |
| Time skew between pipeline and cerbix | `as_of`, `evaluated_at` and every lease are DATABASE time; the pipeline compares nothing against its own clock |
| Replaying an old `ALLOW` | a decision is not a capability: it carries no signature that authorises anything, and the pipeline is told to ask immediately before the step (D4). If D13 persists decisions, a replayed id resolves to its own timestamps and is visibly old |
| Reading unsealed facts to "improve" freshness | forbidden by D6 and invariant 6; the decision path has no heartbeat access by construction |
| Cross-tenant probing of service ids | uniform `ErrNotFound` (invariant 10), the existing pattern |
| Gate as a load amplifier from busy pipelines | a decision is O(1) reads of latches and one report row; no per-request evaluation is triggered. Per-token rate limiting is the existing API-token mechanism if it becomes necessary |
| UNKNOWN silently downgraded to ALLOW by an integration | `unknown_behavior` is the operator's declared choice (D3) and the CLI exit is distinct (D10); the response says `UNKNOWN` in words regardless |

## 9. Non-goals of FR-024

Deployment events and their timeline (FR-025); incident–change correlation (FR-025); before/after SLI
comparison (a report, FR-025); any budget reservation or lease across a deploy (D4); automatic rollback
or any action taken by cerbix on an external system (an invariant, not a deferral); a project-level
inherited policy (D12, second step); vendor-specific CI integrations — the CLI and one generic HTTP
endpoint are the whole integration surface; reading raw heartbeats from the decision path (D6).
