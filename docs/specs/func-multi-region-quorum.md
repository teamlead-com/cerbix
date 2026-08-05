# Spec: Multi-region quorum via composite (variant B)

**Iteration:** iter-0041 · **Complexity:** S–M · **SPA:** yes — quorum mode in the composite form
+ a "Multi-region set" wizard → **create an artifact mockup before implementation** and get it approved.

## Decision context (discussed)
Full per-monitor quorum (variant A: `regions[]` on the monitor, region in heartbeats,
N state machines) **rejected**: a migration of a hot hypertable that breaks the unique key
`(monitor_id, ts)`, a rework of SLA semantics, XL scope. The pain point of false positives from a single
vantage point is already closed by threshold+confirm-phase (D-0099). The remaining value — multi-perspective
for **publicly reachable** targets — is covered by composing existing primitives:
N single-region monitors + a composite with a new quorum mode. Heartbeats stay untouched,
per-region SLA comes for free (a child's SLI = the view from its region).

## Mechanism
- **New composite mode** `mode: "quorum"` + `config.quorum = M` (an int as a string in the
  config map, like children/mode).
- **Semantics (per the original formulation):** the composite becomes **DOWN only when
  ≥ M children are not-up** ("2 out of 3 regions confirmed the failure"). up = downVotes < M.
  Not-up includes pending/deleted (consistent with the current "missing = not-up");
  record this in the doc.
- **domain:** `CompositeQuorum()` (parsing config.quorum); `Validate()` for
  mode=quorum: 1 ≤ M ≤ len(children); mode ∈ {all, any, quorum}.
- **prober/composite.go:** quorum branch; Msg of the form
  `"2/3 children down (quorum 2) — composite down"` / symmetric for up.
- **Alerting:** the composite alerts (channels are attached to it); children are created without
  channels — that is already the default (channels are linked separately). Suppression/escalations/burn —
  unchanged (the composite is an ordinary monitor).

## "Multi-region set" wizard (SPA, pure frontend sugar)
In the active-monitor creation form — a "Multi-region set" toggle: select ≥2
regions (from live ones, the picker already exists) + quorum M (default: majority, ⌈(N+1)/2⌉).
Submit makes N+1 API calls: N children `name @ region` (the same spec, region=selected,
no channels) + a composite `name` (children=the children, mode=quorum, quorum=M). An error
midway — roll back what was created (delete). No backend changes required.

## Tests
- domain: Validate of the quorum mode (M<1, M>len, non-number → 400; valid ones pass);
  CompositeQuorum default.
- prober: table-driven quorum test (N=3: 0/1 down → up; 2/3 down → down; M=1 ≙ all;
  M=N ≙ any-inversion), Msg.
- api/frontend: create composite with mode=quorum round-trip; vue-tsc+build.
- **E2E** (live binary): 3 children (2 live targets, 1 dead) + composite quorum=2 →
  up; take down the second child → composite down (one alert transition); the wizard via the UI path
  (or equivalent API calls) assembles a correct structure.

## Out of scope
Variant A (per-monitor multi-region); grouping children in the list (the naming convention
`name @ region` is sufficient for v1); auto-expanding the set when a region is added.

## Acceptance
Composite-quorum computes per the "down at ≥M down votes" semantics; the wizard creates
a consistent set and rolls back on error; `-race` green; UI per the mockup; the E2E above
reproduces; iter-0041 + D-0101 + traceability.
