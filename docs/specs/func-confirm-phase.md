# Spec: Confirm phase — accelerated down confirmation

**Iteration:** iter-0039 · **Complexity:** M–L · **SPA:** yes — a field in the monitor form
→ **create an artifact mockup before implementation** (reliability-settings block of the form).

## Pain point
Time-to-alert = `interval × failure_threshold`: at 60s×3 the service is down ~3 minutes before
the alert. Reducing the base interval is expensive (load ×N permanently).

## Principle
We accelerate **only the confirmation phase** — from the first failure to the down verdict (or reset).
This is not "more frequent probing under flapping": constant pressure on a degrading service
amplifies the outage. After the verdict — back to the normal cadence.

## Mechanism
- **Monitor field** `confirm_interval_seconds` (json `confirm_interval_seconds`):
  default 10; 0 = disabled; clamp 5..interval; active only when `failure_threshold > 1`
  and the type is active (not push/composite).
- **Signal to the scheduler — pg_notify** (the PullNotifier pattern is already in the codebase):
  `reconcileTransition`, on incrementing `consecutive_failures` (status still up), sends
  NOTIFY `monitor_confirm` payload=monitor_id. The leader holds a single LISTEN connection
  (reuse/mirror `store.PullNotifier`) and on the signal sets
  `nextRun[id] = now + confirm_interval`.
- **Fallback without notify** (reconnect/miss): add `consecutive_failures` to the scheduler
  snapshot (the column already exists in the DB — add it to `monitorColumns`/scan);
  on refresh, monitors with `0 < consecutive_failures < failure_threshold` get the
  confirm interval instead of the normal one.
- **Exiting the phase:** a down verdict or a counter reset (up) → normal interval
  (the next refresh/notify restores the cadence; +1 extra fast probe is acceptable).
- **Pull transport:** TTL of a confirm job = confirm_interval (not the full interval),
  so that stale fast jobs do not pile up.
- **Cap (anti-thundering-herd):** no more than N monitors in the confirm phase simultaneously
  per region (a constant, e.g. 50); beyond the cap — normal interval + WARN log
  `confirm_phase_capped` (+ a gauge in metrics if desired).

## SPA (artifact before code)
Monitor form, reliability block: next to `failure_threshold` — a "Confirm interval, s" field
with the hint "during down confirmation probes run more frequently"; a badge/label
"confirming…" on the monitor card while in the confirmation phase (status up + fails > 0) —
if the data is already available to the frontend. Mockup — an artifact, to be approved before implementation.

## Tests
- domain: field clamp/validation, applicability by type.
- store: reconcile sends notify on increment (0→1, 1→2), does not send on an up reset and after
  a down verdict; `consecutive_failures` is returned in the monitor selection.
- scheduler (unit, fake store): notify → nearer nextRun; fallback via snapshot;
  the cap is respected; exiting the phase restores the interval.
- E2E: monitor interval=60 threshold=3 confirm=5 → time-to-down ≈10–15s (instead of ~180s);
  pull region: the agent receives confirm jobs with a short TTL.

## Out of scope
Adaptive interval outside the confirmation phase (flapping mode); automatic interval
selection; per-project defaults.

## Acceptance
Time-to-down in the E2E scenario shrinks multiple-fold and deterministically; under mass
failure the cap holds the load; `-race` green; both DB modes (hypertable/declarative)
are not affected by regression; UI matches the mockup; vue-tsc+build clean.
