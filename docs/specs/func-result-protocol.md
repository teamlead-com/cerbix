# func-result-protocol — Result ingest: origin, timestamp & config-generation contract

Status: SPEC (spec-before-code). Scope: P0a (timestamp hygiene + typed origins) and
P0b (execution_revision gating). Supersedes the ad-hoc freshness gate shipped in
iter-0083 (`RecordResult` + `last_result_ts` keyed on the worker-clock `hb.Ts`).

## 1. Motivation

The iter-0083 freshness gate compared a worker/agent-clock `Ts` against `last_result_ts`
and advanced the watermark unconditionally. A follow-up review found three independent
ways to corrupt live state, none closed by a simple skew clamp:

- **future clock** — an agent with a clock in 2099 sets `last_result_ts=2099`; every
  subsequent correct result is then "stale" and never applied — live state frozen.
- **config staleness** — a result produced against an OLD monitor config (target/probe
  changed by a PATCH) has a *newer* timestamp and wins, applying an obsolete measurement
  to the new configuration. Timestamps cannot detect this.
- **dead-man race** — a synthetic push-timeout DOWN, decided from a staleness snapshot,
  is delivered after a real heartbeat has already arrived; its server timestamp is newer,
  so it re-trips DOWN.

Plus: push heartbeats carried no timestamp (server substituted `now()` per attempt, so a
retry bypassed dedup), and a 1970 timestamp polluted the raw time series.

## 2. Trusted origins (server-side, never inferred from payload)

Origin is established by which **typed store entrypoint** is called, not by any field in
the result body (an external client must not be able to claim an origin). Three:

| Entrypoint | Caller | Ordering clock | Revision gate |
| --- | --- | --- | --- |
| `RecordScheduledResult` | worker/agent results (AMQP + pull) | result `observed_at` (worker clock, bounded) | required, CAS |
| `RecordPushResult` | the push HTTP handler ONLY | server `received_at` (ingress) | exempt (applied to current config) |
| `RecordDeadmanResult` | the scheduler leader, directly (no dispatcher) | server `statement_timestamp()` | CAS incl. atomic staleness re-check |

`statement_timestamp()` (not `now()`, which is transaction-start and goes stale after a
`FOR UPDATE` wait) is the single authoritative clock for the skew bound and `received_at`.

## 3. execution_revision (config generation) — P0b

- `monitors.execution_revision BIGINT NOT NULL DEFAULT 1`. A **config generation**, not a
  row version — distinct from `updated_at`, which churns on every status flip.
- Incremented **only** by the semantic config write path `UpdateMonitor`
  (`execution_revision = execution_revision + 1`, same statement), which covers
  target/config/type/method/conditions/timeout/retries/region/escalation/**enabled**.
  Bump-on-any-`UpdateMonitor` is deliberate fail-safe: an over-bump costs one re-probe, a
  missed bump reintroduces config-staleness. NOT bumped by: `SetMonitorStatus`,
  `recordCheckStatus`, `ReencryptSecrets` (it rewrites the `config` column but changes no
  semantics — binding the bump to the *method*, not the column, is essential), dependency
  edits (they affect delivery-time suppression, not probe validity).
- The scheduler snapshots `execution_revision` into the job; the prober stamps it into the
  result (the job already carries the whole `Monitor`, so no scheduler/dispatch change).
- Applied as a **CAS inside the status UPDATE**: `... WHERE id=$1 AND execution_revision=$rev`.
  Zero rows updated → `applied=false, reason=stale_revision`; no state/counter/incident/
  watermark mutation. No SELECT-then-check (TOCTOU-free).
- **On bump, reset `last_result_ts = NULL`** in the same statement: a new generation starts
  a fresh watermark (a new job cannot predate the new revision).
- **Mode enum `result_revision_mode: observe | enforce`:**
  - `observe` (migration only): a *missing* revision on a scheduled result is applied +
    counted (`result_missing_revision_total`). A *present-but-mismatched* revision is
    **rejected in both modes**. Push is a separate trusted entrypoint, unrelated to this.
  - `enforce` (default for new installs): missing revision on a scheduled result → reject.
  - Switch observe→enforce only after: fleet upgraded, `result_missing_revision_total`
    flat, `max(job timeout + lease + retry)` elapsed, old queues/DLQ drained. `observe`
    carries an explicit removal date (see Decision) — it is a migration mode, not a
    permanent compatibility bypass.

## 4. Timestamp contract — P0a

Columns on `heartbeats`: `ts` (effective/ordering timestamp) and new nullable
`observed_at` (raw client/probe observation; NULL when there is no genuine client
observation — legacy rows, push without a timestamp). Never write a synthetic zero.

Skew bound: `allowed_skew` (config, default 5m), evaluated in SQL against
`statement_timestamp()`.

### Scheduled — four outcomes
1. **fresh** (`observed_at` within `[now-retention, now+skew]`, newer than watermark):
   apply to state + SLA; `ts = observed_at`; watermark advances.
2. **out-of-order within retention** (older than watermark, but `>= now - retention`):
   SLA-only, not applied to live state. `result_ignored_total{reason="out_of_order"}`.
3. **future beyond skew** (`observed_at > now + skew`): **quarantine** — no state, no
   counters, no incident, no outbox, AND no heartbeat insert (a future-dated row would
   pollute the rollup/partition). `result_quarantined_total{reason="future_timestamp"}` +
   a rate-limited structured log with monitor_id / region / job_id.
4. **outside retention** (`observed_at < now - retention`): ignore, no insert (already
   outside the raw-retention/recompute window). `result_ignored_total{reason="outside_retention"}`.

### Push — never quarantined on client clock
The heartbeat is the liveness signal; rejecting it on a bad client clock would trip a
false dead-man DOWN. Ordering uses server `received_at`, captured at **ingress**:
`GetMonitorByPushToken` returns `(monitor, statement_timestamp())` from the same query;
the handler carries that trusted `received_at` in a server-side envelope to
`RecordPushResult` (external JSON cannot set it — avoids `received_at` degrading into
`processed_at` under queue delay). The result is applied to the current config;
`ts = received_at`, `observed_at = client Ts` (diagnostic). Client-clock anomaly →
`result_clock_skew_total{origin="push", reason="future|past|missing"}` (comparing raw
`observed_at` to `received_at ± skew`), never a reject. Push advances
`last_result_ts = received_at` so the dead-man CAS can see fresh pings.

### Dead-man — atomic staleness re-check
The scheduler applies it directly via `RecordDeadmanResult(monitorID, revision, cutoff)`
(no synthetic heartbeat through the dispatcher). CAS:
```
UPDATE monitors SET status='down', ...
 WHERE id=$1 AND type='push' AND enabled
   AND execution_revision=$revision
   AND COALESCE(last_result_ts, created_at) < $evaluated_cutoff
```
Zero rows → a fresh ping (or a config change, or disable) landed since the staleness
snapshot → the synthetic DOWN is dropped. Incident/event reconciliation runs via a shared
helper (the logic currently in `ingest.handle`, extracted so both ingest and the dead-man
path reuse it).

## 5. Metrics & logs

Prometheus (low-cardinality, NO monitor_id/job_id in labels):
`result_quarantined_total{reason}`, `result_ignored_total{reason}`,
`result_clock_skew_total{origin,reason}`, `result_rejected_total{reason}`,
`result_missing_revision_total`. Operator diagnostics for quarantine/skew go to a
rate-limited structured **log** carrying monitor_id / region / job_id.

## 6. Schema summary

- `monitors.execution_revision BIGINT NOT NULL DEFAULT 1` (+ bump in `UpdateMonitor`, reset
  `last_result_ts` on bump).
- `heartbeats.observed_at timestamptz NULL`.
- (`monitors.last_result_ts` already exists, from migration 00051.)

## 7. Test matrix (DB-gated + unit)

- Revision: bump on `UpdateMonitor`; NO bump on `SetMonitorStatus`/`ReencryptSecrets`;
  CAS rejects a mismatched-revision result (state untouched); `last_result_ts` reset on
  bump; observe vs enforce for missing revision; present-mismatch rejected in both modes.
- Timestamps: the four scheduled outcomes; future→quarantine (no heartbeat row);
  1970→outside-retention (no row); out-of-order→SLA-only.
- Push: bad-clock ping applied (not rejected), `ts=received_at`, `observed_at=client`,
  skew metric bumped; retry idempotency by received_at ordering.
- Dead-man: synthetic DOWN dropped when a fresh ping raced in (CAS zero-rows); applied
  when genuinely stale; not applied across a revision bump / after disable.
- received_at captured at ingress (not processed_at) — handler-level test.

## 8. Non-goals / deferred

- `job_id` end-to-end correlation and a strict `observed_at >= job_issued_at` check — with
  P0b/job correlation, not here.
- `state_sequence` (per-applied-transition monotonic) for outbox delivery ordering (#4) —
  a **separate** axis from `execution_revision`; introduced with the outbox-ordering work
  (P2), not here.

## 9. Rollout

`observe` mode is the rolling-upgrade escape hatch (separate role processes may run mixed
versions mid-rollout). New installs default to `enforce`. A decision record fixes the
`observe`-removal window.
