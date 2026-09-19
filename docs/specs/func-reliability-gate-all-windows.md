# func-reliability-gate-all-windows — worst-of-all-windows evaluation (FR-035 / NFR-029)

> **Revision 1 — APPROVED for iter-0186 by owner instruction on 2026-09-19.** Backend/schema/API
> implementation is authorized. Depends on iter-0185's effective-policy source tuple. This
> intentionally supersedes FR-024 D2's decision to evaluate exactly one window. The canonical-theme
> artifact [`mock-reliability-gate-all-windows.html`](../design/mock-reliability-gate-all-windows.html)
> was approved by the owner on 2026-09-19; SPA implementation is authorized.

## 1. Problem

FR-024 binds a policy to exactly one SLO window. A service can be healthy over 30 days while exhausting
its 24-hour budget, or look safe over 24 hours while a long-window burn is already unacceptable. An
operator must currently choose which truth the gate is allowed to see.

Worst-of-all-windows means the policy may evaluate every SLO target the service has in the same snapshot
and apply the existing gate algebra over the expanded evidence. It is not an average and does not let a
healthy window cancel an unhealthy one.

## 2. Requirements

- **FR-035 — Worst-of-all-windows gate evaluation.** A service or inherited project policy may choose
  `window_mode: all`; the gate evaluates every configured standard-window target for the service and
  returns all matching/unavailable reasons grouped by window.
- **NFR-029 — Order-independent, snapshot-consistent aggregation.** Target inventory, budgets, burn
  latches, incident state, policy source, override, ledger insert, and freshness bounds come from one
  database snapshot. Reordering targets cannot change state or action.

## 3. Policy schema v2

Gate policy schema version 2 is a strict tagged union:

```json
{
  "schema_version": 2,
  "window_mode": "one",
  "window": "30d",
  "clauses": {},
  "budget_consumed_percent": 90,
  "max_seal_lag_seconds": 600,
  "unknown_behavior": "block"
}
```

or:

```json
{
  "schema_version": 2,
  "window_mode": "all",
  "clauses": {},
  "budget_consumed_percent": 90,
  "max_seal_lag_seconds": 600,
  "unknown_behavior": "block"
}
```

- `one` requires exactly one `window` and preserves existing behavior.
- `all` forbids `window`; it reads the service's configured standard-window targets.
- Unknown, absent, duplicate, or contradictory fields are named `400` refusals.
- Existing schema-v1 rows migrate semantically to v2 `window_mode=one` with the same window, document
  hash, effective result, and next revision. No policy revision is bumped merely by migration.
- Project and service policy tables use the same document schema and validator.

## 4. Window inventory

For `all`, the inventory is the service-scoped targets visible at `evaluated_at`, in canonical order
`24h`, `7d`, `30d`, `90d`. A target added or removed changes future decisions because target inventory is
now an explicit policy input, and every decision records the exact inventory it used.

- At least one target: evaluate all of them.
- No target: the constraining budget/burn clauses are unavailable with `reason: no_objective`; the
  existing unknown algebra decides the action.
- Project objectives are reporting-only and never enter a service gate.
- Archived/deleted targets are absent from future inventory but remain named in historical decision
  evidence.

## 5. Evaluation algebra

The existing FR-024 D4 order remains authoritative, applied to clause instances:

1. Expand every window-scoped clause (`budget_exhausted`, `budget_consumed`, page/ticket burn) once per
   target.
2. Evaluate service-scoped clauses (`service_incident_open`) once, not once per window.
3. Any known matching `block` instance yields `BLOCK`, even when another window is unavailable.
4. Otherwise any unavailable constraining instance yields `UNKNOWN` and the policy's
   `unknown_behavior` action.
5. Otherwise any matching `warn` instance yields `WARN`; else `ALLOW`.

There is no numeric ranking, averaging, majority, or early exit. Every instance is evaluated so the
response and ledger contain the complete reason set. Canonical ordering is clause order, then standard
window order; state/action are set algebra and therefore independent of row order.

## 6. Evidence and freshness

The decision adds:

```json
{
  "window_mode": "all",
  "evaluated_windows": [
    {"window":"24h","target_id":"…","objective":99.9,"reasons":[]},
    {"window":"30d","target_id":"…","objective":99.95,"reasons":[]}
  ]
}
```

- `evaluated_windows` is always an array in canonical order and includes healthy windows with empty
  reasons so the inventory is reproducible.
- Top-level `reasons[]` remains for compatibility and contains the flattened matching/unavailable set;
  every window-scoped reason now carries `window` and `target_id`.
- `sealed_through` is recorded per window if reports can differ; if the implementation proves one
  service watermark is shared, it may also expose a top-level copy but may not discard per-window
  provenance.
- `facts_fresh_until` is the earliest constraining horizon across every evaluated instance and the
  policy's seal-lag limit.
- Ledger history is self-contained: later target deletion cannot alter an old decision response.

## 7. Overrides, inheritance, and compatibility

- Overrides still change effective action only; they never remove a bad window or rewrite observed
  state.
- The override binds to iter-0185's effective policy source tuple. A `one ↔ all` change is a real policy
  revision and revokes the active override.
- Service policies and inherited project policies may independently use `one` or `all`; source
  precedence is unchanged.
- `cerbix gate check` exit behavior follows effective action exactly as today.
- Existing clients reading one-window fields continue to receive them for `window_mode=one`. For `all`,
  singular `window`, `target_id`, and `objective` are absent by contract; clients must read
  `evaluated_windows`.

## 8. API and UI

- OpenAPI exposes policy schema v2 and the decision union for one/all modes; generated TypeScript and
  Go DTOs are parity-gated.
- The policy editor offers `One window` and `Worst of all configured windows`. Choosing `all` hides the
  singular selector and previews the current target inventory without copying it into the policy.
- The decision screen leads with the worst effective outcome, then renders every evaluated window,
  including healthy windows, and identifies the reason(s) that determined the result.
- This is a material gate UI change and requires an owner-approved artifact mock before SPA work.
  Backend/schema implementation may begin after spec approval, but the frontend waits for the mock.

## 9. Observability and performance

- Decision metrics gain bounded `window_mode="one|all"`; they do not label individual windows beyond
  existing bounded standard names and never label target ids.
- One evaluation remains under the existing transaction budget. Queries load all standard targets and
  latches in bounded set form, not N independent round trips.
- The maximum inventory is four standard windows, enforced by the domain vocabulary and schema.
- Runbook diagnostics print source tuple, mode, inventory, determining reasons, per-window freshness,
  and transaction-budget failure.

## 10. Acceptance invariants

1. V1 policies migrate to v2 one-window semantics without a revision or result change.
2. `all` uses exactly the configured service target set from the decision snapshot, max four.
3. Known BLOCK beats an unavailable neighbor; unavailable constraining evidence beats WARN; healthy
   windows never cancel unhealthy ones.
4. Incident state is evaluated once; window-scoped clauses are exhaustive per target.
5. Every decision records complete canonical inventory and per-window evidence.
6. Result and reason ordering are deterministic under randomized SQL row order.
7. Inheritance, CAS, overrides, ledger retention, and CLI exit semantics remain intact.
8. Evaluation stays within the existing transaction budget without per-window query loops.

## 11. Required tests and gates

- Pure table tests for every D4 precedence combination across two to four windows, including ignored
  unavailable clauses and randomized input order.
- Migration compatibility tests proving v1→v2 no revision/result drift.
- PostgreSQL snapshot tests for concurrent target add/delete, per-window burn evidence, earliest
  freshness, no-target UNKNOWN, and inherited all-window policy.
- API/OpenAPI/generated-client parity and singular-field absence in all mode.
- CLI golden/exit-code tests and ledger replay/history tests after target deletion.
- SPA component tests plus live Playwright for mode switching, inventory preview, mixed outcomes,
  UNKNOWN, override, and inherited project policy — after mock approval.
- Performance assertion: bounded statement count and transaction-budget integration test.

## 12. Iter-0186 plan and role split

| Priority | Task | Role |
| --- | --- | --- |
| P0 | Approve D2 supersession and aggregation | Owner + Agent C |
| P0 | Add schema v2 and compatibility migration | Agent A |
| P0 | Implement set-based multi-window evaluation | Agent A |
| P0 | Kill precedence/order/snapshot mutants | Agent B |
| P1 | Produce and approve the gate UI mock | Owner + Agent C |
| P1 | Add API, CLI, ledger and SPA evidence | Agent A |
| P1 | Extend metrics, performance and runbook | Agent D |
| P2 | Synchronize docs and run all live gates | Agents B/C/D |

## 13. Non-goals

Custom window names, weighted averages, quorum/majority decisions, choosing a subset of windows,
cross-service/project aggregate gates, predictive burn, deployment reservations, and changing SLO
calculation itself.
