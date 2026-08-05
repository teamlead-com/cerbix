# Spec: Monitor dependency graph + cascading alert suppression

**Iteration:** iter-0040 · **Complexity:** L (the hardest of the batch: schema + DAG +
suppression in 2 topics + escalations + UI) · **SPA:** yes — dependency multiselect and
badges → **create an artifact mockup before implementation** and get it approved.

## Pain point
Postgres goes down → alerts for all 50 dependent services. We need `depends_on` and silence for
the children while the parent is down — with honest data.

## Schema
- Migration: `monitor_dependencies (monitor_id uuid FK→monitors ON DELETE CASCADE,
  depends_on_id uuid FK→monitors ON DELETE CASCADE, PRIMARY KEY (monitor_id, depends_on_id),
  CHECK (monitor_id <> depends_on_id))` + an index on `depends_on_id`.
- M2M: a service can depend on both a DB and a broker. Parents — from the same project.

## Domain / Store
- `Monitor.DependsOn []string` (json `depends_on`).
- **DAG validation** on create/update: recursive CTE over monitor_dependencies,
  depth cap 10; a cycle (including self and transitive) → 400; a foreign project → 400.
- Store: `SetMonitorDependencies(ctx, id, parents)` (replace-set in a transaction inside
  Create/UpdateMonitor), `DownAncestors(ctx, id) ([]idName, error)` — recursive CTE
  upwards: ancestors with status='down' **or** an open auto-incident (depth cap 10).

## Suppression (existing pattern: flat-down suppression of escalations in the outbox)
- `TopicMonitorTransition` (down) and `TopicSLOBurnAlert` of a child: before delivery —
  `DownAncestors`; non-empty → suppressed: the event is delivered (no-op), log
  `alert_suppressed_by_dependency root=<id>`.
- **We suppress delivery, not facts:** the child's heartbeats, status, incidents, SLA are written
  honestly — otherwise we would distort the reports.
- **Escalations:** `AdvanceEscalations` skips incidents of monitors with a down ancestor
  (the ladder does not start/freezes; no ack required). Resumption — when the ancestor recovers
  while the child is still down.
- **Recovery (up) is never suppressed.**
- Child's timeline: a system incident_update "alerts suppressed: depends on <parent>
  which is down" (integration with iter-0037: the root-cause field of the context is filled in).
- The race "parent went down later than the child": v1 best-effort, no retrospective window —
  record the limitation in the overview.

## API / SPA (artifact before code)
- `create/update monitor`: `depends_on: [id]`; openapi + regen schema.d.ts.
- Mockup artifact: a "Depends on" multiselect in the monitor form (the project's monitors, except
  itself, searchable by name); a "⏸ suppressed by <parent>" badge in the monitor list/details;
  a "Dependencies" block in the details (parents/children with statuses).

## Tests
- domain/api: cycle validation (self, A→B→A, transitive, depth cap), a foreign project.
- store: SetMonitorDependencies round-trip + cascade on monitor deletion;
  DownAncestors (direct/transitive/cap; ancestor down vs an open auto-incident).
- outbox: child down while ancestor down → suppressed (the notifier was not called, delivered);
  a child's burn alert → suppressed; up → delivered; ancestor recovered → delivery resumed.
- scheduler/escalation: AdvanceEscalations skips suppressed incidents and
  resumes after the ancestor's recovery.
- E2E: parent + 2 children, take down the parent → one alert (the parent), the children are silent with badges
  and a timeline note; recovery → child notifications arrive.

## Out of scope
A retrospective suppression window; automatic graph building (dependency discovery);
cross-project dependencies; a visual graph editor (multiselect only in v1).

## Acceptance
The E2E cascade scenario yields exactly one alert; child data (SLA/heartbeats/incidents)
is not distorted; `-race` green; UI matches the mockup; vue-tsc+build clean;
D-doc + traceability.
