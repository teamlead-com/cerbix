# func-reliability-gate — a deploy asks whether the error budget allows it (FR-024 / NFR-019)

> **Lifecycle: DESIGN GATE, revision 2 — under independent review, no code.** Revision 1 (`46379fa`,
> `c65ea5d`) was rejected by the design review with 4 P0, 9 P1, 3 P2 (party [15]); every finding is
> addressed below and each is named where it changed the text. The owner closed D9–D13 on 2026-08-28
> (D-0188) and the four questions the review raised the same day (D-0189). Two gates remain before code:
> review round 2/2 of THIS revision, and — because the policy editor and the decision history are SPA
> surfaces — an approved UI mock before any frontend code. Change Intelligence is **FR-025** and is not
> part of this requirement.

## 1. What this is, in one paragraph

cerbix already computes, per service and per SLO window, everything a release decision needs: how much of
the error budget is consumed and whether it is burning (the reliability report and the burn evaluator,
FR-021 phase 2), whether a service incident is open (FR-022), and how fresh all of that is (the seal
watermark and the evaluators' leases). What it does not do is turn those facts into a **decision a
pipeline can act on**. The Reliability Gate is that: a deterministic, machine-readable answer — an
observed `state` and an effective `action` — derived from facts cerbix has already sealed and evaluated,
read in ONE database snapshot, with every reason and every timestamp attached, asked immediately before
a protected pipeline step.

It is a thin slice on purpose. It adds a policy table, an override table, a decision ledger, one
decision endpoint and one CLI verb. It adds **no new computation of reliability** and **no new store of
events**.

## 2. Requirements

- **FR-024 — Reliability Gate.** An operator declares, per service, a policy naming the SLO window it
  governs and which reliability clauses block a release, warn, or are ignored. A pipeline, authenticated
  with an API token, asks `POST /api/v1/projects/{p}/services/{s}/gate` (or `cerbix gate check`) and
  receives a decision with all matching reason codes and the evidence they rest on, recorded in a
  bounded, append-only decision ledger. A privileged operator may record a time-bounded override; the
  gate reports that it was used. Policy and override mutations are audited; decisions are not written to
  the general audit log — the ledger is their record.
- **NFR-019 — the gate never disagrees with the screen.** Budget, burn and coverage in a gate response are
  the values the service page and the alerting badge would show for the SAME snapshot instant, because
  they are produced by the same code paths (D1) inside one transaction (D6a). A gate that computes its
  own number, or composes numbers from different instants, is a second semantics owner — a defect class
  this project has already paid for (iter-0161 §34, §45).

## 3. The decisions

**D1 — the gate reads existing owners and computes nothing. DECIDED; corrected by review P0-1.**
Revision 1 named `decideServiceWindow` as the budget owner. It is not: it owns *withholding* (may this
number be quoted at all) and availability. The remaining budget is computed AFTER it, in the report path,
by `reportObjective` + `sla.ErrorBudget` for ONE window's service-scoped target, yielding
`BurnedPercent` and `RemainingRatio`. The owners the gate consumes, precisely:

- **budget, for the policy's window** — the report path (`serviceReliabilityReportTx`, which already
  takes a transaction and an `asOf`): `BurnedPercent` is the budget fact; `decideServiceWindow` inside
  it decides whether the number may be quoted, and a withheld number is a withheld clause, never a zero.
- **burn, for the policy's window** — the burn latch `service_burn_alert_state` rows of THAT window's
  target only (`sla_targets` is `UNIQUE(service_id, window_name)`): `last_verdict`, `firing`,
  `lease_until`, per rule.
- **open service incident** — `incidents` where `service_id`, `source = 'auto'`, `status <> 'resolved'`.
- **coverage** — `serviceCoverageClauses` in `internal/store/servicealertstate.go`, as EVIDENCE only
  (D11): it says who is paging right now, not how reliable the service is.

The gate has no formula of its own for any of these. If a clause needs a fact none of the owners
exposes, the owner is extended and the page gains it too. **Invariant 1.**

**D2 — the window is part of the policy. DECIDED (owner, 2026-08-28; review P0-1).**
A policy names exactly one `window` (a `sla.Window` name the service has a target for). Every budget and
burn clause is evaluated against that window's target and no other; a policy for a window the service
has no target for is refused at write time and, if the target is later deleted, evaluates to `UNKNOWN`
with `reason: window_target_missing`. Worst-of-all-windows was considered and declined: the answer would
depend on which windows somebody happened to add, and the evidence would be a list rather than a fact.

**D3 — the budget threshold is a consumed percentage. DECIDED (owner, 2026-08-28; review P0-1).**
`RemainingRatio` is an absolute share of time (a 99.9 objective has a whole budget of 0.001), so a
threshold of "0.10 remaining" was an error of units — every fresh budget would already be "below". The
clause is `budget_consumed_percent >= N`, `N` an integer in 1..100 per policy, default 90, and
`BurnedPercent` already owns that quantity. `budget_exhausted` is the same fact at `N = 100`, exposed as
its own clause because it deserves its own default (block) and its own reason code.

**D4 — the states, and the split between what was OBSERVED and what to DO. DECIDED; review P0-2.**
Revision 1 put `UNKNOWN` outside the `BLOCK > WARN > ALLOW` order and then let the CLI exit non-zero for
it even when the operator had chosen `warn` — a shell would have blocked against the operator's choice.
The response therefore carries two fields:

- `state` — what was observed: `ALLOW`, `WARN`, `BLOCK`, or `UNKNOWN` (some clause the policy depends
  on could not be answered);
- `action` — what the pipeline should do: `ALLOW`, `WARN`, or `BLOCK`.

The algebra, total and deterministic, evaluated over ALL clauses:

1. A clause assigned `ignore` is evaluated for evidence but contributes nothing — its unavailability
   does NOT produce `UNKNOWN`.
2. If any clause assigned `block` is KNOWN and matches → `state = BLOCK`, `action = BLOCK`, whatever
   else is unknown. A known block is never softened by an unknown neighbour.
3. Else, if any clause assigned `block` or `warn` is UNAVAILABLE → `state = UNKNOWN`,
   `action = unknown_behavior` (`warn` or `block`, from the policy, no default).
4. Else, if any `warn` clause matches → `state = WARN`, `action = WARN`.
5. Else → `state = ALLOW`, `action = ALLOW`.

`reasons[]` lists every clause that matched or was unavailable, each with its assignment, its value and
its source, so a `WARN` with a stale ticket clause reports both. `NOT_CONFIGURED` is a fifth `state`
with NO `action`: the service has no policy, the response says so with `reason: not_configured` and a
documentation link, and what to do with that is the integration's visible choice. It is never rendered
as `ALLOW` or `WARN`.

**D5 — `unknown_behavior` is mandatory and has no default. DECIDED.**
A policy is created with `unknown_behavior: warn | block`, explicitly, or it is refused. A silent
`block` breaks onboarding (a fresh installation has no sealed facts for hours); a silent `warn` is a
fail-open nobody chose.

**D6 — a decision is a point-in-time advisory input, never a reservation. DECIDED.**
The gate reserves no budget, holds no lock across the deploy, and does not know whether the deploy
happened. The pipeline asks **immediately before** the protected step and stores `decision_id` as
evidence. The TOCTOU window between answer and step is named, is the pipeline's, and v1 does not try to
close it — a gate that coordinates deployments has become a deployment system.

**D6a — one snapshot, one instant. DECIDED; review P0-3.**
Revision 1 would have composed a report (its own REPEATABLE READ snapshot), a coverage read (a separate
pool query), an incident read, a policy read and an override read — five instants presented as one, an
evidence set that never existed together. The gate has ONE linearization point: a single
`REPEATABLE READ` transaction, read-write because the decision row is inserted in it, whose first
statement is `SELECT statement_timestamp()` — that value is `evaluated_at`, the same instant the report
path already uses as its `as_of`. Inside it, in this order: the service row (tenant check), the policy,
the active override, `serviceReliabilityReportTx(tx, …, evaluated_at)` for the policy's window, the
window's burn latches, the open-incident predicate, the coverage clauses, then the ledger INSERT and
commit. Each owner is consumed through a transaction-taking variant; where one does not exist yet, it is
added to the owner — the formula is never copied. A serialization failure retries the whole transaction
once; a second failure is a transport-class error (`exit 1`, HTTP 503 `snapshot_conflict`), never a
decision.

**D7 — the answer carries its own evidence, with correct semantics. DECIDED; review P1-1.**
Every response includes:

- `decision_id`;
- `evaluated_at` — the snapshot instant (D6a). Revision 1 called this `as_of` "the seal watermark";
  in the code `as_of` is the statement timestamp and `sealed_through` is the end of the data, and the
  two are different facts;
- `sealed_through` — the end of the sealed data the budget rests on;
- `window`, `target_id`, `objective`, `objective_updated_at`;
- `governing_revision` — the service definition revision in force at `evaluated_at`, nullable when
  none is; and `fact_revision_ids[]` — the revisions the sealed facts in the window were computed under,
  which may be several (the report's own `spans_definition_revisions` withholding exists precisely
  because they can be);
- `coverage_lease_until`, and `burn_leases[]` — one per rule of the policy's target, because a target
  may hold several rules with different leases;
- `policy_revision`, `override` (id, actor, reason, expires_at — when applied), `original_state` and
  `original_action` when an override changed them;
- `coverage_state` per signal, as evidence;
- `reasons[]` — every matching or unavailable clause, total.

There is **no `valid_until`**. Revision 1 offered one derived from leases; the review is right that the
incident, the policy and the override can all change a microsecond later regardless of any lease, so
the field would have promised a validity the decision does not have. `facts_fresh_until` is present
instead — the minimum of the applicable leases — and the response documents it as "when the FACTS go
stale", explicitly not as a lifetime of the decision.

**D8 — the sealed contract has a price, and the price is stated. DECIDED.**
The SLO window ends at the seal watermark, never at `now`. A gate asked at 14:03 answers with the budget
as of `sealed_through`, and an outage that began after it is not yet visible. This is accepted. It is
**forbidden** to "fix" the gate by reading raw heartbeats from the decision path: that would make it the
one surface in the product that quotes an unsealed number. **Invariant 6.**

**D9 — override lifecycle. DECIDED; review P1-3.**
An override is created through `POST …/gate/override`, never as a field of the decision request. Its
actor is the request's authenticated principal, server-derived — a `reason` (bounded, 1..500 chars) and
an `expires_at` are mandatory; `expires_at` must be after the database's `now()` and at most **7 days**
ahead (a hard maximum, not a default — there is no default). At most ONE unrevoked override exists per
service at a time: creation locks the service row and refuses a second with 409 `override_active`;
revocation is its own audited mutation. An override changes `BLOCK → ALLOW` and `UNKNOWN → ALLOW`,
leaves `WARN` and `ALLOW` as they are, and never applies to `NOT_CONFIGURED`. It is **bound to the
policy revision it was created under**: a policy edit revokes any active override in the same
transaction (`revoked_reason: policy_changed`), because an override that outlives a tightening of the
policy would silently allow what the new policy forbids. The response names the applied override and
the state it displaced; `cerbix_gate_decisions_total{state,action,overridden}` counts it.

**D10 — decisions are a ledger, not audit rows. DECIDED (owner, 2026-08-28; review P0-4).**
Revision 1 promised "every decision audited", "decisions persisted" and "O(1) reads with no write" at
once; they are incompatible. The resolution: every decision is one row in `service_gate_decisions`, an
append-only ledger with a bounded retention (default 90 days, configurable, purged on a scheduler
cadence like heartbeat retention) and a read endpoint
`GET …/gate/decisions/{id}` authorised like the decision itself. Policy and override mutations go to the
tenant audit log; decisions do NOT — a busy pipeline would otherwise bury the audit log under its own
heartbeat. A decision row is an immutable snapshot: it stores the service slug and name, the policy
clauses and revision, and the full evidence at the time, so it remains readable after the service is
renamed and survives its deletion (`service_id` nullable, `ON DELETE SET NULL`, the pattern
`service_alert_episodes` already uses); it is cascaded with its project. A lost response is retried by
the pipeline and produces a second, equally honest row — no idempotency key, because a decision is
a fresh reading and two of them are two facts. (The review noted that a signed canonical response CAN
prove content; revision 1 said otherwise. It is still declined, because it brings a key lifecycle and
cannot be looked up later.)

**D11 — the clause vocabulary. DECIDED (owner, 2026-08-28); names per review P2-1.**
Closed, versioned (`schema_version: 1`), exhaustive: a policy write must assign every clause in the
version's set, or it is refused (D14). Each clause is `block`, `warn` or `ignore`:

| Clause | Fact and source (D1) | Shipped default |
|---|---|---|
| `budget_exhausted` | `BurnedPercent >= 100` for the policy's window | block |
| `budget_consumed` | `BurnedPercent >= budget_consumed_percent` (policy field, 1..100, default 90) | warn |
| `page_burn_firing` | a `page`-severity rule of the window's target is FIRING in the latch | block |
| `ticket_burn_firing` | a `ticket`-severity rule of the window's target is FIRING | warn |
| `service_incident_open` | an unresolved auto-incident anchored to this service | warn |

Three choices inside it. An open incident WARNS, not blocks: the deploy is very often the fix, and a
gate that blocks the fix because of the incident it fixes works against its purpose; a stricter
installation sets `block`. The burn clauses are named by SEVERITY, not `fast`/`slow`: the domain allows
arbitrary windows, `page` and `ticket` are urgencies, and the default 1h/5m–6h/30m pair is only a
template. **`coverage_not_armed` is not in the vocabulary** (owner; review P1-2 concurs): "not armed"
means the service is NOT the one paging for its members right now, its causes are incommensurable
(`not_owned`, `state_not_pageable`, `unroutable`, `stale_lease`…), and none of them is a release-risk
fact. `coverage_state` stays in the response as evidence. If "do not deploy while paging is broken" is
ever wanted, it arrives as precise clauses (`alert_route_unroutable`, `evaluator_stale`), never as
`armed = false`.

Clauses whose fact is UNAVAILABLE rather than false — `budget_withheld` (the report withholds the
number), `facts_stale` (a lease expired or no evaluation exists), `no_objective` / `window_target_missing`
— are not assignments; they are the conditions under which a `block`/`warn` clause is unavailable and D4
step 3 applies, each with its own reason code in `reasons[]`.

**D12 — authorisation. DECIDED (owner, 2026-08-28; review P1-5).**
Three central `authz.Action`s, checked where every other action is: `gate:evaluate` (also reads the
ledger), `gate:policy:write`, `gate:override`. In v1 they map to the existing project roles — `viewer`+,
`editor`+, `project_admin`+ — and an API token asks the gate with the role it already has. A
token-scoped capability ("this token may evaluate the gate and read nothing else") is **deliberately not
built here**: `domain.Role` is shared by memberships and tokens, so a narrower token model is a change to
RBAC rather than an addition to it, and it is recorded as a follow-up with its own requirement. No
ad-hoc role comparison anywhere in the gate handlers.

**D13 — policy scope and ownership. DECIDED (owner, 2026-08-28; review P2-2).**
Per service, no inheritance. And the policy is an **operational control, owned by the UI/API even for a
file-managed service**: it is not part of bundle format 2, it is excluded from the canonical hash, and it
is the one paging-adjacent field a file-managed service accepts through the API — an explicit, documented
exception to the 409 rule of §16.6a, because the team that deploys is not always the team that owns the
service's declaration. *The review recommended the opposite* — policy as a declarative part of format 2,
with file-managed services refusing API writes — and that position is recorded here as the alternative
the owner declined, so it can be revisited with evidence rather than rediscovered.

**D14 — policy evolution and concurrency. DECIDED; review P1-8.**
The policy document carries `schema_version`; the server accepts only versions it knows. A write must
assign EVERY clause of that version exactly once — unknown, missing or duplicate clauses are refused with
the offending name, so adding a clause in a later version cannot silently reinterpret stored policies:
they keep their version and are evaluated under it until rewritten. Thresholds must be finite integers
within bounds and are stored canonically. A write identical to the stored policy is a no-op: no revision
bump, no audit row, no override revocation (the `alertPolicyDiff` pattern). Concurrent writes are
serialized by `expected_revision` in the body, 409 `revision_conflict` on mismatch — the declaration
PUT's pattern.

**D15 — tenant error contract. DECIDED; review P1-9.**
Revision 1 said "a foreign, unknown or malformed id answers 404, as the alerting endpoints do", and the
alerting endpoints do not: `serviceIDParam` refuses a malformed UUID with **400** before the store is
asked, and a well-formed foreign or unknown id is **404**. The gate keeps that contract exactly, on every
gate route alike. **Invariant 10.**

**D16 — the CLI is a security and operations surface, not a verb. DECIDED; review P1-6.**
`cerbix gate check --project <p> --service <s> [--json] [--timeout 10s]`. The server is `CERBIX_URL`, the
credential is `CERBIX_TOKEN` — **environment only, never a flag**, because flags land in shell history
and process lists. TLS verifies by default; `CERBIX_CA_FILE` adds a CA; there is no skip-verify option in
v1. The decision goes to stdout — one line of text, or the response JSON with `--json` — and reasons and
diagnostics go to stderr. The JSON is the API response verbatim and carries `schema_version`. Exit
codes follow `action`: **0** `ALLOW` and `WARN`; **2** `BLOCK`; **4** `NOT_CONFIGURED`; **1** transport,
auth, timeout or snapshot conflict. `UNKNOWN` therefore exits 0 or 2 by the operator's declared
`unknown_behavior`, and the word `UNKNOWN` is in the output regardless — revision 1's exit 3 would have
had a plain shell block against an operator who chose `warn`.

**D17 — Change Intelligence is FR-025 and is not started here. DECIDED.**
Deployment/rollback/flag events, the service timeline, incident correlation and before/after SLI
comparison extend `IncidentContext` (§14) with append-only phases keyed
`UNIQUE(service_id, source, external_id, phase)`, a domain-owned phase order, serialization per external
identity, and closed enums with no free payload. None of it is needed for a decision.

## 4. What must move WITH the implementation, not after it

- `docs/overview.md` — the gate endpoints and CLI verb; the one-snapshot rule and the owners under the
  reliability section.
- `docs/runbook.md` — "the gate says UNKNOWN" (it is about the facts, not the pipeline); the override
  procedure and the metric; the decision-ledger retention knob; the CLI's environment contract.
- `func-service-reliability.md` §16.8 — invariant 106 (the gate consumes the owners and derives
  nothing) and 107 (one snapshot); rows in `docs/traceability.md` in the same change, since
  `make docs-check` compares the two exactly.
- `func-service-reliability.md` §16.6a — the documented exception: gate policy is writable on a
  file-managed service (D13).
- `openapi.yaml` — decision, policy, override and ledger schemas; regenerate `frontend/src/api/schema.d.ts`.
- `docs/specs/README.md` — this file is listed (review P2-3, done in revision 2).
- README — one sentence: cerbix decides, the pipeline acts.

## 5. Schema (decided shape; column detail belongs to the implementation)

```
service_gate_policies   (service_id PK, project_id, window text, schema_version int,
                         clauses jsonb -- exhaustive {clause: block|warn|ignore} for the version,
                         budget_consumed_percent int CHECK 1..100, unknown_behavior CHECK IN (warn, block),
                         revision bigint DB-owned, updated_at, updated_by)
service_gate_overrides  (id, service_id, project_id, policy_revision, actor_user_id NULL, via_token bool,
                         reason text CHECK length 1..500, created_at, expires_at NOT NULL,
                         revoked_at NULL, revoked_reason NULL, revoked_by NULL;
                         at most one row per service with revoked_at IS NULL — enforced under the service lock)
service_gate_decisions  (id, project_id NOT NULL → projects CASCADE, service_id NULL → services SET NULL,
                         service_slug, service_name, window, policy_revision, policy_snapshot jsonb,
                         state, action, reasons jsonb, evidence jsonb, override_id NULL,
                         evaluated_at, sealed_through NULL; retained `gate.decision_retention_days`, default 90)
```

Tenant-composite FKs to `(services.id, project_id)` where the service reference is NOT NULL, as every
service-scoped table since 00085.

## 6. Acceptance invariants (FR-024) — draft, numbered on acceptance

1. the gate computes NO reliability fact: budget comes from the report path for the policy's window,
   withholding from `decideServiceWindow` inside it, burn from that window's latches, coverage from
   `serviceCoverageClauses`; a response never quotes a number the service page would withhold;
2. `NOT_CONFIGURED` is distinct from every policy outcome, has no `action`, carries its own reason and
   exit code, and is never rendered as `ALLOW` or `WARN`;
3. `UNKNOWN` is produced only under a declared policy, only by an unavailable `block`/`warn` clause
   (never by an `ignore`d one), and resolves to the policy's explicit `unknown_behavior`; a policy
   without one cannot be created; a KNOWN block is never softened by an unknown neighbour;
4. `reasons[]` is total — every matching or unavailable clause — and `state`/`action` follow the
   algebra of D4 exactly;
5. the response carries `evaluated_at`, `sealed_through`, `window`/`target_id`/`objective`,
   `governing_revision` and `fact_revision_ids[]`, `coverage_lease_until` and `burn_leases[]`,
   `policy_revision`; `facts_fresh_until` is the minimum of applicable leases and there is no
   `valid_until`;
6. the decision path reads only SEALED facts and has no heartbeat access by construction;
7. all reads and the ledger INSERT happen in ONE `REPEATABLE READ` transaction whose first statement
   supplies `evaluated_at`; a serialization failure is retried once and then reported as a transport
   error, never as a decision;
8. an override is created only through its own endpoint by a principal holding `gate:override`, with
   server-derived actor, bounded reason and an expiry ≤ 7 days; at most one is active per service; it
   is never honoured from a decision request body; a policy edit revokes it;
9. an expired or revoked override is not applied; an applied one is named in the response with the
   displaced state and counted in the metric;
10. a malformed service id answers 400 before the store; a well-formed foreign or unknown id answers
    404; identically on every gate route;
11. policy writes are exhaustive over the version's clause set, refuse unknown/missing/duplicate
    clauses by name, bump a DB-owned revision only on real change, are CAS'd on `expected_revision`,
    and are audited with before/after and actor;
12. a decision row is immutable, survives service rename and deletion, is purged by retention, and is
    readable under `gate:evaluate`;
13. the gate policy of a file-managed service is writable through the API (D13) and is absent from the
    bundle hash;
14. a decision request is bounded: a statement timeout on the transaction and a per-token in-flight
    limit, with the limit's rejection a transport error and never a decision.

## 7. Required test matrix (written before the code)

*Decision algebra:* intact budget → `ALLOW` with every evidence field · `page` rule firing → `BLOCK`
with `page_burn_firing`, AND the matching `ticket` clause reported too · `budget_consumed` only → `WARN`
· warn + block → `BLOCK`, both reasons · known `BLOCK` + one stale `warn` clause → `BLOCK`, the stale
clause in `reasons[]` as unavailable · only an `ignore`d clause unavailable → `ALLOW`, not `UNKNOWN` ·
a `warn` clause unavailable under `unknown_behavior: warn` → `state UNKNOWN`, `action WARN`, exit 0 ·
under `block` → `action BLOCK`, exit 2 · no policy → `NOT_CONFIGURED`, no action, exit 4 · a service
with two targets: the policy's `window` selects one, and the OTHER window's firing rule changes nothing
· `BurnedPercent` at exactly the threshold matches (`>=`), one below does not.

*Owners and parity:* the badge says `unroutable` and `coverage_state` says `unroutable` — the same
string from the same snapshot · the report withholds availability and the gate reports
`budget_withheld`, quotes no number · a gate that recomputes `BurnedPercent` locally fails the parity
test (mutation).

*One snapshot:* an incident opened, a policy edited and a latch flipped BETWEEN what would be separate
reads all land on one side of `evaluated_at` (real Postgres, a blocker connection) · a forced
serialization failure retries once, then 503.

*Override:* created through its endpoint, `BLOCK → ALLOW` with `override.id`, `original_state` and
`original_action` set · the same field in a decision body is refused as unknown-field · expired changes
nothing · a second active override is 409 · two concurrent creates: exactly one wins · a policy edit
revokes the active override and the next decision is un-overridden · `expires_at` of 8 days refused.

*Policy writes:* missing clause 400 naming it · unknown clause 400 · duplicate 400 · no
`unknown_behavior` 400 · a window the service has no target for 400 · identical rewrite: no revision
bump, no audit row · stale `expected_revision` 409 · file-managed service: policy write 200 while a
paging write is still 409.

*Tenant:* malformed id 400, foreign well-formed id 404, on decide, policy, override and ledger alike.

*Ledger:* a decision is readable by id · after service rename the row keeps the old slug and name ·
after service delete the row survives with `service_id NULL` · rows older than retention are purged.

*Bounds:* a statement timeout on the gate transaction fires and is exit 1 · the per-token in-flight limit
rejects the (n+1)th concurrent request as 429, never as a decision.

*CLI:* `CERBIX_TOKEN` absent → exit 1 with a message naming the variable · a `--token` flag is not
accepted · transport timeout → exit 1 · TLS with an unknown CA fails; with `CERBIX_CA_FILE` succeeds ·
`--json` output is byte-identical to the API response.

## 8. Threat model (facts corrected per review P1-7)

| Threat | Mitigation |
|---|---|
| Pipeline approves itself | override is not a request parameter (D9); it needs `gate:override`, leaves an audit row, is bound to the policy revision, and is counted |
| Stolen CI token | can evaluate the gate and read the ledger — facts the token could already read on the service page — and nothing more unless it also holds `gate:override`. A narrower token capability is a recorded follow-up, not a v1 promise |
| Policy deleted or loosened mid-pipeline | deletion → `NOT_CONFIGURED`, which has no action and its own exit; every policy write is audited and bumps a revision the decision cites |
| Override outlives a policy tightening | a policy edit revokes the active override in the same transaction (D9) |
| Time skew between pipeline and cerbix | every timestamp in the response is DATABASE time; the pipeline compares nothing against its own clock |
| Replaying an old `ALLOW` | a decision is not a capability and carries no authorisation; the pipeline asks immediately before the step (D6); a replayed id resolves in the ledger to its own `evaluated_at` and is visibly old |
| Reading unsealed facts to "improve" freshness | forbidden by D8, invariant 6; no heartbeat access from the decision path |
| Cross-tenant probing | 400 malformed / 404 foreign (D15), the existing transport contract |
| Load amplification from busy pipelines | **there is no API-token rate limiter today** (only the login IP limiter), and a decision is NOT one row: the report path runs bounded but real scans over sealed buckets and segments. Mitigations: a statement timeout on the gate transaction; a per-token in-flight bound in the handler (429, never a decision); the window is fixed by the policy so the scan is bounded by it; **no cache**, which would be a second semantics owner. Both bounds are tested |
| `UNKNOWN` silently read as green | `unknown_behavior` is the operator's declared choice (D5); the CLI exit follows `action`; the word `UNKNOWN` is in the output regardless (D16) |

## 9. Non-goals of FR-024

Deployment events and their timeline, incident–change correlation, before/after SLI comparison (all
FR-025); budget reservation or any lease across a deploy (D6); automatic rollback or any action by
cerbix on an external system (an invariant, not a deferral); a project-level inherited policy (D13,
second step); a token-scoped gate capability (D12, follow-up requirement); worst-of-all-windows
evaluation (D2); vendor-specific CI integrations — one generic endpoint and one CLI verb are the whole
surface; reading raw heartbeats from the decision path (D8); a decision cache (§8).
