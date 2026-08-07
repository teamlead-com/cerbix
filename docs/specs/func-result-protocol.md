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
| `RecordPushResult` | the push HTTP handler ONLY, via a dedicated `PushResultRecorder` (never the shared `ResultSink`) | server `received_at` (ingress) | exempt (applied to current config) |
| `RecordDeadmanResult` | the scheduler leader, directly (no dispatcher) | server `statement_timestamp()` | CAS incl. atomic staleness re-check |
| `RecordHistoricalResults` | agent backfill replay | per-row `observed_at` | n/a — SLA-only, never live state (§4) |

`statement_timestamp()` (the start of the current SQL statement) is the single
authoritative clock for the skew bound and `received_at`. It is preferred over
`now()`/`transaction_timestamp()` (start of the whole transaction): a clock read in a
statement that runs AFTER a `SELECT ... FOR UPDATE` reflects that statement's start,
whereas `now()` still returns the transaction's start — stale relative to wall-clock.
(Neither value changes *during* the lock wait itself; the difference is statement- vs
transaction-scope.)

Push is applied by a dedicated server-side `PushResultRecorder` that calls
`RecordPushResult` **directly** (the api/all role has the store) — it does NOT go through
the shared `ResultSink`/dispatcher/ingest queue. Routing push through the common result
queue would force the origin to travel as a data field again (the anti-pattern §2 forbids)
and would reintroduce ack-before-durable. Direct application also makes each push HTTP
request durable within the request and removes any transport redelivery.

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
- **The revision gate runs BEFORE the heartbeat insert, not just before the status flip.**
  A result that measured a different configuration is invalid for SLA too, so a revision
  failure inserts NO heartbeat. Ordering within the ingest tx:
  1. `SELECT execution_revision, last_result_ts, type, enabled FROM monitors WHERE id=$1 FOR UPDATE`
     — the row lock (held for the whole tx) serialises against `UpdateMonitor`, so reading
     the revision and then acting is atomic. (This is the CAS; the earlier "no
     SELECT-then-UPDATE" caution was about an *unlocked* read — a `FOR UPDATE` read is not a
     TOCTOU window, and it must gate the insert, which a `WHERE`-predicate on the later
     status UPDATE could not.)
  2. Evaluate revision by mode: scheduled present-mismatch → **reject** (`stale_revision`);
     scheduled missing → **reject** in `enforce` (`missing_revision`), apply + count in
     `observe`; push is exempt (its recorder does not pass a revision). A reject returns
     `applied=false` with **no heartbeat insert** and no state/counter/incident/watermark
     mutation.
  3. Only on revision pass does the timestamp outcome (§4) decide insert + apply.
- **On `UpdateMonitor` bump, reset `last_result_ts = NULL`** in the same statement: a new
  generation starts a fresh watermark (a new job cannot predate the new revision).
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

### Wire compatibility (do NOT rename the field)

The result wire format keeps its existing `ts` JSON field; `observed_at` is a DB concept.
For scheduled results the wire `ts` (the worker/agent probe clock) maps to DB
`observed_at`. There is NO new `observed_at` wire field, so an old worker (which sends
`ts` but no revision) stays compatible: `observe` mode tolerates its missing revision, and
its `ts` populates `observed_at` — it is NOT seen as `missing_timestamp`. `missing_timestamp`
means the wire `ts` is genuinely zero (a malformed/broken producer), not "old binary".

### Scheduled — outcomes
A scheduled result MUST carry a timestamp (the prober always sets `ts`; `observed_at`
nullable exists only for push/legacy). Precedence: **duplicate → missing → revision (§3)
→ timestamp**.

- **duplicate** (heartbeat `(monitor_id, ts)` already present — an AMQP/pull redelivery):
  `INSERT ... ON CONFLICT DO NOTHING` affects 0 rows → `applied=false`, no state/counter
  mutation (the fact is already recorded). This is the redelivery-safety case.
- **missing timestamp** (`ts` zero → `observed_at` would be NULL): **reject**, no insert.
  `result_rejected_total{reason="missing_timestamp"}`. Never silently treated as "now".

Then, on a fresh non-duplicate result that passed the revision gate, by `observed_at`:
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
`processed_at` under queue delay). `ts = received_at`, `observed_at = client Ts` (or NULL).
Push advances `last_result_ts = received_at` so the dead-man CAS sees fresh pings.

**Current-state re-check under the row lock.** The monitor can be disabled (or retyped)
between the token lookup and the apply. `RecordPushResult` re-reads `type='push' AND enabled`
under `FOR UPDATE` and drops the result if it no longer holds — a ping accepted just before
a disable must not mutate a now-disabled monitor.

**Client timestamp is optional and, today, absent.** The push endpoint does not currently
accept a client timestamp, so `observed_at` is normally NULL — this is the expected case,
NOT an error: no `missing` metric, no log. `result_clock_skew_total{origin="push",
reason="future|past"}` is emitted ONLY when a client timestamp is actually provided and
sits outside `received_at ± skew` (adoption telemetry for a future optional field); it is
never a reject.

**Post-commit flow (shared, all origins).** Applying push must run the SAME post-commit
reconciliation as scheduled ingest and dead-man — SSE status event, auto-incident
open/resolve, operational check metric (the logic currently inline in `ingest.handle`).
The direct `PushResultRecorder` must invoke that shared helper after commit; otherwise
direct push would silently stop opening/resolving incidents.

### Dead-man — atomic staleness re-check, through the outbox
The scheduler applies it directly via `RecordDeadmanResult(monitorID, revision, cutoff)`
(no synthetic heartbeat through the dispatcher). In ONE transaction:
1. `SELECT ... FROM monitors WHERE id=$1 FOR UPDATE` and evaluate the staleness predicate
   `type='push' AND enabled AND execution_revision=$revision
    AND COALESCE(last_result_ts, created_at) < $evaluated_cutoff`. If it fails (a fresh
   ping, config change, or disable landed since the staleness snapshot) → drop the
   synthetic DOWN, `applied=false`, commit nothing.
2. If it passes, in the SAME transaction:
   a. **Insert a DOWN heartbeat** (`up=false, ts=statement_timestamp(), observed_at=NULL,
      msg="push timeout…"`). This is required: SLA is sample-based (`count(up)/count(*)`),
      so a status flip WITHOUT a heartbeat would leave the monitor DOWN but SLA at 100% on
      stale UP samples. The insert provides the down sample.
   b. Apply the DOWN through the SAME `recordCheckStatusTx` a real result uses — it must NOT
      bypass confirmation/maintenance handling or the transition-outbox enqueue.
   Staleness predicate + heartbeat insert + status transition + outbox event = one tx.
3. Incident/event reconciliation runs AFTER commit, via the shared post-commit helper.

`created_at` in the predicate is the monitor's, not the result's; `evaluated_cutoff` is
the staleness threshold the scheduler used (`now - interval - grace`), passed in.

**Dead-man does NOT advance `last_result_ts`** (only a real observation from the monitored
service does). That is deliberate: while an outage persists, each scheduler tick (throttled
by `nextRun ≈ interval`) re-fires the dead-man — its CAS still passes (`last_result_ts`
unchanged, still `< cutoff`) — inserting a periodic DOWN sample so SLA reflects the ongoing
outage; `recordCheckStatusTx` transitions only on the first (its own `prev != cur` guard),
so no incident churn. The instant a real ping returns, `last_result_ts` advances and the
CAS stops firing.

> **Open point — `status <> 'down'` in the predicate (I diverge).** The review suggested
> adding it to stop a concurrent explicit push-DOWN from being re-counted. I'd NOT add it:
> an explicit push-DOWN goes through `RecordPushResult`, which advances
> `last_result_ts=received_at`, so the dead-man CAS (`last_result_ts < cutoff`) already
> fails — the double-count is prevented, and both paths serialise on the same `FOR UPDATE`
> lock. Adding `status <> 'down'` would instead convert dead-man from periodic
> down-sampling to edge-only, REGRESSING SLA continuity during a sustained push outage
> (the current behavior samples each tick). Flagging for your call before I lock it.

### Historical backfill — a fourth, SLA-only path
An agent buffers results during a central outage and replays them as historical backfill;
today `agentBackfill` calls `InsertHeartbeatsBulk` directly, bypassing every gate — so it
can insert a `2099` row, and SLA (`WHERE h.ts >= since`, lower-bound only) counts it
immediately. Backfill gets its own contract, `RecordHistoricalResults`:

- **Never touches live state** (no status/counter/incident/outbox/`last_result_ts`) — it is
  SLA/audit-only by definition (replaying old down→up must not re-alert).
- **Per-row timestamp validation, identical bounds to scheduled:** `missing` → skip;
  `future beyond skew` → skip; `outside retention` → skip; a valid historical row → insert
  SLA-only. Each row of the batch is evaluated independently.
- **Idempotent:** `ON CONFLICT (monitor_id, ts) DO NOTHING`.
- **Revision:** not applicable — the gate exists to protect live state, which backfill never
  mutates; consistency with D-0142 is "backfill never applies to live state, regardless of
  revision", not a CAS.
- Skipped rows increment the same `result_quarantined_total` / `result_ignored_total`
  reasons; the handler returns inserted/received/skipped counts.

## 5. Metrics & logs

Prometheus (low-cardinality, NO monitor_id/job_id in labels):
`result_quarantined_total{reason}`, `result_ignored_total{reason}`,
`result_clock_skew_total{origin,reason}`,
`result_rejected_total{reason}` (reasons: `stale_revision` | `missing_revision` |
`missing_timestamp`), `result_missing_revision_total` (the observe-mode counter).
Operator diagnostics for quarantine/skew/reject go to a rate-limited structured **log**
carrying monitor_id / region / job_id.

## 6. Config

New strict-validated `result:` block (loader defaults + bounds, like every other config):

```yaml
result:
  allowed_skew: 5m        # bound on how far ahead of statement_timestamp() an observed_at
                          # may be before a scheduled result is quarantined
  revision_mode: enforce  # enforce | observe  (observe = temporary migration mode)
```

- `result.allowed_skew` (`Duration`): default `5m`; validation `> 0` and `<= 1h` (a larger
  window defeats the future-clock guard; reject at load).
- `result.revision_mode` (enum): default `enforce`; only `enforce` | `observe` accepted
  (anything else → fail-fast, no silent downgrade). `observe` is the rolling-upgrade escape
  hatch; its removal is fixed in the decision record (§10 / D-0142).

## 7. Schema summary

- `monitors.execution_revision BIGINT NOT NULL DEFAULT 1` (+ bump in `UpdateMonitor`, reset
  `last_result_ts` on bump).
- `heartbeats.observed_at timestamptz NULL`.
- (`monitors.last_result_ts` already exists, from migration 00051.)

## 8. Test matrix (DB-gated + unit)

- Revision: bump on `UpdateMonitor`; NO bump on `SetMonitorStatus`/`ReencryptSecrets`;
  CAS rejects a mismatched-revision result (state untouched); `last_result_ts` reset on
  bump; observe vs enforce for missing revision; present-mismatch rejected in both modes.
- Scheduled: duplicate (redelivery)→applied=false no mutation; missing `ts`→reject no
  insert; then the four timestamp outcomes; future→quarantine (no heartbeat row);
  1970→outside-retention (no row); out-of-order→SLA-only.
- Push: bad-clock ping applied (not rejected), `ts=received_at`, `observed_at=client`,
  skew metric bumped. Idempotency precision — a client-level HTTP **retry** gets a fresh
  `received_at` and IS a new heartbeat (no dedup, by design; the test must NOT assert
  dedup). With the direct `PushResultRecorder` there is no transport envelope and thus no
  redelivery to dedup in the first place.
- Push: current-state gate — a ping accepted just before a disable does NOT mutate the
  now-disabled monitor (re-check under the row lock); direct-recorder path still opens/
  resolves an auto-incident (post-commit helper runs).
- Dead-man: synthetic DOWN dropped when a fresh ping raced in (CAS zero-rows); applied
  when genuinely stale; INSERTS a down heartbeat so SLA drops (not just status); periodic
  re-fire during a sustained outage samples SLA; not applied across a revision bump / after
  disable.
- Backfill: mixed valid/invalid batch — future/1970/missing rows skipped, valid rows
  inserted SLA-only, live state untouched; idempotent replay (ON CONFLICT); a future row
  never reaches an SLA `ts >= since` query.
- received_at captured at ingress (not processed_at) — handler-level test.

## 9. Non-goals / deferred

- `job_id` end-to-end correlation and a strict `observed_at >= job_issued_at` check — with
  P0b/job correlation, not here.
- `state_sequence` (per-applied-transition monotonic) for outbox delivery ordering (#4) —
  a **separate** axis from `execution_revision`; introduced with the outbox-ordering work
  (P2), not here.

## 10. Rollout

Two independent constraints:

**Config vs strict YAML.** The loader rejects unknown keys, so a `result:` block cannot be
pushed to an old binary that predates it — the old process would fail to start. Therefore
the `result:` block must be **optional with safe defaults**, and binaries are upgraded
before the block is added to their configs. To make an in-place upgrade safe without a
config edit, **when the `result:` block is ABSENT the binary defaults `revision_mode` to
`observe`** (an upgrade of an existing install is the likely context); a fresh install
gets `enforce` via the shipped `config.example.yaml` template (which includes the block).
`allowed_skew` defaults to 5m in both cases. (This is the one behavioral default I want
confirmed — see the reply.)

**Producer-before-consumer.** Revision only appears once producers (worker/scheduler/agent)
emit it. Roll producers to the new binary first, then the API/ingest consumer; `observe`
covers imperfect ordering during the window.

`observe` mode is the rolling-upgrade escape hatch (separate role processes may run mixed
versions mid-rollout). **Decision D-0142** fixes the `observe`-removal window: observe ships
with P0b; it is removed **no later than iter-0089** (≈ three iterations after it lands), or
one release after a confirmed prod `enforce` cutover, whichever comes first — so it can
never silently become a permanent bypass.
