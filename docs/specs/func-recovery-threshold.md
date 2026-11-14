# func-recovery-threshold — a monitor leaves DOWN only after a confirmed run of successes (FR-043 / NFR-036)

> **STATUS: revision 2 — DRAFT, not approved.** Revision 1 (2026-11-14) was written from an owner
> discussion and an independent code check of two design questions. Revision 2 (2026-11-14) answers
> the first external review of that draft — CHANGES REQUIRED, 1 P0 / 6 P1 / 3 P2 / 1 P3; every finding
> was checked against the code and its disposition is in §11. Three points carry an explicit owner
> answer of 2026-11-14 and are marked **OWNER**. Everything else the author decided is marked
> **AUTHOR'S CALL**; the six that needed the owner were confirmed on 2026-11-14 (§10).
> The SPA surfaces in §8 need an approved mock before any frontend code.
>
> Invariants are numbered **within this document** (I1, I2, …). Code is cited by file and function,
> never by line: this document is written before the change that will move the lines.

## 1. Why

A monitor has hysteresis in one direction only. `failure_threshold` makes it wait for N consecutive
failed checks before it is declared `down`; the way back has no counterpart. In
`internal/store/monitors.go` `recordCheckStatusTx` a single successful check sets `status = 'up'`,
zeroes `consecutive_failures` and clears `last_notified_at`.

For a target that is really back, that is right. For a target that answers every other request —
a pod in a restart loop, a pool that is still warming, one healthy replica out of three behind a
balancer — it produces three defects from one outage:

| What happens | Why it is wrong |
| --- | --- |
| A "recovered" notification goes out on the first lucky answer | It is false, and the on-call person stops reading the green ones |
| `consecutive_failures` is back at 0 | The next `down` needs the full `failure_threshold` again, so the monitor is `up` for most of an outage it is in the middle of |
| The auto-incident is resolved and a new one is opened minutes later | One outage becomes several short incidents, each with its own page and its own duration |

A service softens the third defect — at most one open auto-incident per service
([`func-service-incidents.md`](func-service-incidents.md)) — but a monitor with a notification
channel of its own has no protection at all.

## 2. Requirements

- **FR-043**: A monitor declares `recovery_threshold` — the number of **consecutive successful
  checks** it must see while `down` before it is declared `up`. Until the run is complete the monitor
  stays `down`, shows how far the run has got, sends no recovery notification, keeps its
  auto-incident open, sends no "still down" reminder and fires no escalation step. A failed check
  ends the run. The default is 1, which is today's behaviour exactly.
- **NFR-036**: The threshold is **alerting hysteresis, not measurement**. For a monitor whose
  configuration does not change during the period compared, it changes no probe cadence and
  therefore no row in `heartbeats`; no monitor SLA, project SLA, burn rate, service SLI, error
  budget, coverage, gate decision or status-page availability bar differs, for the same sequence of
  check results, between any two values of the threshold (proven by regression).

  The qualifier is not a loophole, it is D-0142: **every** monitor update — one that changes only
  this field included — runs `revisionFenceSetSQL`, and a result issued before the update is refused
  as stale and never reaches `heartbeats`. That is true of every field today and is not changed here.

## 3. The decisions

### Owner decisions (2026-11-14)

**D1 — no "still DOWN" reminder while a recovery run is in progress. OWNER.**
`EnqueueRenotifyReminders` (`internal/store/monitors.go`) skips a monitor whose run has started.
A reminder that says "still down" between success 2 and success 3 is the same false statement as a
premature "recovered", in the other direction. When the run breaks, reminders continue on the
schedule they already had.

**D2 — no escalation step fires while a recovery run is in progress. OWNER.**
`AdvanceEscalations` (`internal/store/escalation.go`) does not page the next line for a monitor
whose run has started. Waking the second line while the first line's fix is being confirmed is the
page nobody wants. When the run breaks, the ladder carries on (the exact clock rule is D9).

**D3 — the value is an integer in 1..10. OWNER.**
Outside the range is a 400 on every write surface, not a silent clamp.

### Author's calls

**D4 — checks are NOT accelerated during a recovery run. AUTHOR'S CALL, confirmed by the OWNER 2026-11-14.**
`confirm_interval_seconds` ([`func-confirm-phase.md`](func-confirm-phase.md)) accelerates the way
*down*; nothing accelerates the way *up*. The run is evaluated at the monitor's ordinary interval.
Four reasons, in order of weight:

1. **Acceleration defeats the purpose.** A restart loop with a 35–45 s period answers three checks
   5 s apart and fails on the fourth. A run sampled at the ordinary interval tests stability over the
   window the monitor was configured to care about.
2. **It is the pressure the confirm phase forbids.** Its principle is that a degrading service is
   not probed harder; a service that has just produced one success is the same service.
3. **The regional cap is shared.** `confirmCapPerRegion` (`internal/scheduler/scheduler.go`) bounds
   accelerated monitors per region. After a network blip hundreds of monitors recover at once; if
   they took the accelerated slots, a monitor that is *failing* at that moment would lose its fast
   confirmation to make green notifications arrive sooner.
4. **It would break NFR-036.** Monitor SLA is the share of successful checks (`internal/store/sla.go`,
   `internal/store/rollup.go`), so extra checks change the number.

The cost is stated plainly: with a 60 s interval and a threshold of 3, "recovered" arrives about two
minutes after the first good answer. The time to detect an outage is blind time; the time to confirm
a recovery is soak time with a person already looking. The form warns when
`interval_seconds × (recovery_threshold − 1)` reaches 600 s (§8).

**D5 — a recovery is dated with the instant it is CONFIRMED, not with its first success. AUTHOR'S CALL.**
The start of an outage is already dated with its verdict: `Reconciler.Reconcile`
(`internal/ingest/reconciler.go`) opens the auto-incident only on the transition, and `started_at`
takes the database clock at that moment. The end mirrors it: `Reconciler.resolveAutoIncident` calls
`AddIncidentUpdateBySystem` (`internal/store/incidents.go`), which stamps `resolved_at` with the
`statement_timestamp()` of its own transaction. Dating the end with the first success while the
start stays at the verdict would shorten every incident from both ends — a three-minute outage with
thresholds 3/3 at 60 s would be recorded as one minute — and could resolve an incident *before* the
last update in its own timeline.

The first success is not lost: it is carried as information (D6).

**D6 — the first success of the run is recorded and shown, never used as a system timestamp. AUTHOR'S CALL.**
`monitors.recovery_started_at` is stamped — with the database clock of the transaction that records
the check, the same clock as every incident timestamp — when the run goes 0 → 1, and cleared when
the run breaks or completes. The check's own timestamp is not used: a pull agent's clock may run up
to `result.allowed_skew` behind, which could print a "first OK" earlier than the incident's own
`started_at`.

The transaction that confirms the recovery clears the column, and both consumers of the sentence run
**after** that commit (`Reconciler.Reconcile` and the outbox worker each call `GetMonitor`). So the
sentence is never rendered from the `monitors` row. The confirming transaction captures a
**recovery snapshot** — run length, threshold in force, first-OK instant — before it clears them,
and that snapshot travels (§4.6).

**D7 — the threshold applies only to `down` → `up`. AUTHOR'S CALL.**
A new monitor and a re-enabled one are `pending`; their first success makes them `up` at once.
Applying the threshold there would leave a fresh monitor in `pending` — `no_data` on a status page —
for M intervals with nothing wrong.

**D8 — `push` and `composite` monitors must have a threshold of 1. AUTHOR'S CALL, confirmed by the OWNER 2026-11-14.**
A `push` monitor has no active check to repeat: its first heartbeat *is* the recovery. A `composite`
monitor's children already carry their own hysteresis, and a second threshold on top multiplies the
delay. A value above 1 on either type is a 400, not a silent reset. This is deliberately **stricter**
than `failure_threshold`, which is silently forced to 1 for `push` in `Monitor.Normalize` and is
allowed on `composite`; the new field does not copy that.

**D9 — during a run the escalation ladder skips its steps but its clock is not shifted. AUTHOR'S CALL, confirmed by the OWNER 2026-11-14.**
The ladder is not a countdown. `AdvanceEscalations` computes each step's due time as an absolute
offset from the incident's base (`started_at + after_seconds`), and the repeat of the last step from
`last_escalated_at`. D9 adds a condition to the query that selects monitor incidents; no timestamp
is rewritten. Three consequences, all intended:

- a step that became due during a run that then broke fires on the first pass after the break;
- if several steps became due during a long run, **all of them fire in that one pass** — the loop
  already fires every elapsed step at once;
- the `repeat_last` repetition pauses during a run in the same way and resumes after a break.

The alternative — pushing every later step back by the length of the run — needs a stored offset per
incident and delays the second line of an outage that has just been confirmed as continuing.

**D10 — changing the threshold never changes a monitor's status by itself. AUTHOR'S CALL.**
Lowering the threshold below the run's current length does not flip the monitor inside the update
transaction: the next successful check is evaluated against the new value. No configuration write in
cerbix enqueues a transition, resolves an incident or publishes a live event, and a Monitoring-as-Code
apply of many monitors must stay one atomic configuration change.

**D11 — a manual resolve during a run changes nothing in this feature. AUTHOR'S CALL.**
Today, a person who resolves an auto-incident while the monitor is `down` has taken that episode on:
no new incident opens until the monitor has been `up` and gone `down` again. A broken run after a
manual resolve is the same situation and keeps the same behaviour.

**D12 — no instance-wide default in this iteration. AUTHOR'S CALL, confirmed by the OWNER 2026-11-14.**
The per-monitor field ships with the default 1. The monitor-defaults settings carry neither
`failure_threshold` nor `confirm_interval_seconds` today, and an instance default of 2 would collide
with D8 for every `push` and `composite` monitor created without the field.

**D13 — the "recovering N/M" label is NOT live in this iteration. AUTHOR'S CALL, confirmed by the OWNER 2026-11-14.**
Revision 1 claimed the monitor list already consumes a live update. It does not: `MonitorsView.vue`
has no live subscription, `events.Event` carries a status and nothing else, and ingest publishes only
when the status changes — which, during a run, it does not. Today's "confirming N/M" label has the
same limit and is correct as of the last load. The new label follows it. Making both labels live
means a new event kind published on every counter change; that is a separate change (§9).

## 4. Behaviour

### 4.1 State machine

`status` keeps its three values (`pending`, `up`, `down`); no new status is added. The counter is
`consecutive_successes`, and it is non-zero only while the monitor is `down` — enforced by the
database (§5).

Transitions caused by a **check result** (`recordCheckStatusTx`, reached from
`RecordScheduledResult`, `RecordPushResult` and `RecordDeadmanResult`):

| Old status | Check | New status | `consecutive_successes` | `recovery_started_at` | `last_notified_at` |
| --- | --- | --- | --- | --- | --- |
| `pending` | success | `up` | 0 | NULL | cleared (as today) |
| `up` | success | `up` | 0 | NULL | cleared (as today) |
| `down` | success, run + 1 < threshold | `down` | run + 1 | kept, or stamped if it was NULL | **kept** |
| `down` | success, run + 1 ≥ threshold | `up` | 0 | NULL | cleared |
| `down` | failure | `down` | 0 | NULL | kept (as today) |
| `pending`, `up` | failure | as today | 0 | NULL | as today |

In row 4 the transaction returns the recovery snapshot — `run + 1`, the threshold it compared
against and the pre-clear `recovery_started_at` — before the columns are reset (§4.6).

Everything about the way down — `consecutive_failures`, `failure_threshold`, the `monitor_confirm`
notification, the stamping of `last_notified_at` on a fresh `down` — is unchanged. With
`recovery_threshold = 1` row 3 can never match and the table reduces to today's code.

**What does not touch the run.** These paths record something without a verdict and must neither
advance nor reset `consecutive_successes`:

- `RecordProbeError` — an executor diagnostic, not a check;
- a duplicate or out-of-order result (`ts <= last_result_ts` in `RecordScheduledResult`) and a result
  refused as stale or from the future — none of them reaches `recordCheckStatusTx`;
- the expected-run ledger marking missed windows after scheduler downtime.

**What resets the run without a check.** Every write that sets `status` outside
`recordCheckStatusTx` sets `consecutive_successes = 0` and `recovery_started_at = NULL` in the same
statement:

| Path | Today | With this change |
| --- | --- | --- |
| Re-enable in `updateMonitorTxPrepared` (API update and Monitoring-as-Code restore) | `status = 'pending'`, `consecutive_failures = 0` | also resets the run |
| `RetireMonitor`, `ReactivateMonitor` (`internal/store/monitorretire.go`) | the same | also resets the run |
| `SetMonitorStatus` (no production caller; a test helper) | sets `status` only | resets the run when the new status is not `down` |

Disabling a monitor does not change its status today and does not reset the run; the run is reset
when the monitor is enabled again.

### 4.2 What waits for the confirmed recovery

- **The transition event.** `recordCheckStatusTx` enqueues `MonitorTransition` only when the status
  changes, so a run in progress produces no event and a broken run produces none either.
- **The recovery notification** and **the auto-incident's resolution** — both hang on that event and
  on `Reconciler.Reconcile`, which ingest calls only on a status change.
- **`last_notified_at`.** Today it is cleared on any success. It keeps being cleared on every
  transition **into `up`** (`pending` → `up` and the confirmed `down` → `up`) and on a success while
  already `up`; it is **not** cleared by a success that leaves the monitor `down`. Otherwise a
  success that starts a run clears it, the run breaks, the monitor is still `down`, the stamp is
  never set again (it is set only on a *fresh* `down`), and `EnqueueRenotifyReminders` — which
  requires it to be non-NULL — is silent for the rest of the outage.

### 4.3 What pauses during a run (D1, D2)

- `EnqueueRenotifyReminders` gains the condition `consecutive_successes = 0`.
- `AdvanceEscalations` gains the same condition on the **monitor** incident query only. The service
  incident query is not changed: a service's ladder is driven by the service, not by one member's run.

Neither rewrites a timestamp. The consequences for overdue steps and `repeat_last` are in D9.

### 4.4 What a run looks like to everything that reads `status`

These readers are **not changed**; the table states the consequence so that none of it is a surprise.

| Reader | During a run (monitor still `down`) |
| --- | --- |
| Composite parent — `CompositeProber.Probe` via `MonitorStatuses` | The child counts as not-up for `all`, `any` and `quorum` |
| Dependency suppression — `DownAncestors` (`internal/store/dependencies.go`) | Descendants stay suppressed |
| Outbox staleness gate (`StateSequence > Seq`) | Unaffected: a success inside a run does not move `state_sequence` |
| Status page, **monitor** component | Stays `major_outage`; its 90-day bar, which is computed from `heartbeats`, already shows the successes |
| Status page, **service** component | Computed from `heartbeats` by `internal/reliability`; `operational` on the first success |
| Service alerting and the service auto-incident | Also computed from `heartbeats`, but closed by `DecideServiceAlert` only after `confirm_evaluations` healthy evaluations in a row — the service has its own hysteresis and this feature does not interact with it |
| `ServiceHealthNow` diagnostics | The member is listed as failing while the service SLI is healthy |
| Confirm phase — `Monitor.InConfirmPhase` | Never true: it requires `status = up` |

Two consequences are deliberate and documented in the runbook rather than engineered away:

- **A service can be green while its member monitor is still red.** The service measures; the
  monitor alerts. That split is the design (`internal/reliability` states it: alert hysteresis does
  not belong in measurement).
- **A parent can recover before its child.** With a parent at threshold 1 and a child at 3, the
  parent's `down` — and with it the suppression of the child — ends first. The form says so next to
  the field when the monitor has dependencies (§8).

### 4.5 Editing a monitor

- **One rule on every write surface** — API create, API update, Monitoring-as-Code decode: integer,
  1..10, and exactly 1 for `push` and `composite`.
- **Absent is not zero.** Every input shape carries the field as an optional integer (`*int` in the
  API request bodies and in the file provider's raw monitor). Absent means 1 on create and "keep the
  stored value" on update. An explicit `0` is refused by `Monitor.Validate`; `Monitor.Normalize` does
  **not** clamp this field, unlike `failure_threshold`.
- **Test-connection** runs `Monitor.Validate`, so it refuses an out-of-range value for the types it
  can test. `push` and `composite` are refused earlier with the existing "this monitor type cannot be
  tested"; that order is not changed.
- **Monitoring-as-Code**: `recovery_threshold` is accepted in every bundle format version that
  accepts `failure_threshold`, joins `commonMonitorFields` and the canonical monitor form, and so
  takes part in drift detection.
- **Execution semantics**: `RecoveryThreshold` is classified `SemanticDelivery`,
  `ConsecutiveSuccesses` and `RecoveryStartedAt` `SemanticRuntime` in
  `internal/domain/execsemantics.go`.
- Changing the threshold does not touch `consecutive_successes` or the status (D10).

### 4.6 Text

The **recovery snapshot** of a confirmed recovery is three values: `N` — the length of the run that
completed, `M` — the threshold it was compared against, and the first-OK instant.

- `recordCheckStatusTx` returns it; `ResultOutcome` carries it to `Reconciler.Reconcile`.
- `domain.MonitorTransition` gains three optional fields carrying the same values; the outbox worker
  renders from the event, never from the monitor row (which is already reset, and whose threshold may
  have been edited before delivery). An event without them — every event already in the queue at
  upgrade — renders today's text.

The sentence is `Recovered: N/M consecutive checks passed; first OK at <RFC 3339, UTC>`, and it is
present exactly when `N > 1`. `N` and `M` differ only when the threshold was lowered during the run
(D10): three successes at threshold 4, lowered to 2, confirmed on the fourth, reads `4/2`.

| Surface | With `N > 1` |
| --- | --- |
| Auto-incident resolving update | `Monitor recovered — automatically resolved. Recovered: N/M consecutive checks passed; first OK at …` |
| Notification, every channel that renders text | today's recovery text, then the sentence |
| Email | the **Subject stays today's string**; the sentence is added to the body only |

With `N = 1` every text is byte-for-byte what it is today.

## 5. Schema (sketch — the builder owns the exact DDL)

One migration, additive, no backfill beyond the defaults:

- `monitors.recovery_threshold integer NOT NULL DEFAULT 1`, with a CHECK for 1..10;
- `monitors.consecutive_successes integer NOT NULL DEFAULT 0`;
- `monitors.recovery_started_at timestamptz NULL`;
- a CHECK tying the three states together:
  `(consecutive_successes = 0 AND recovery_started_at IS NULL) OR
  (status = 'down' AND consecutive_successes > 0 AND recovery_started_at IS NOT NULL)`.

The three columns are appended to `monitorColumns` and to `scanMonitor` in the same order, and to
the create and update SQL (`recovery_threshold` only — the other two are server-owned). `heartbeats`
is not touched, so the storage mode does not matter to this change; the migration is still run in
both.

`openapi.yaml`: `recovery_threshold` on the monitor read model and on the create, update and
test-connection bodies; `consecutive_successes` and `recovery_started_at` (nullable) on the read
model, read-only. The TypeScript schema is regenerated.

## 6. Acceptance invariants

- **I1** — With `recovery_threshold = 1`, every status transition, outbox event, notification text,
  incident timestamp and `last_notified_at` value is identical to the behaviour before this change,
  for the same sequence of check results.
- **I2** — A `pending` monitor becomes `up` on its first success, whatever its threshold.
- **I3** — With the threshold unchanged at M, a `down` monitor becomes `up` on exactly the M-th
  consecutive success; a failure anywhere in the run resets the count to 0 and the monitor needs M
  again. After the threshold is lowered during a run, the next success is compared against the new
  value.
- **I4** — One confirmed recovery produces exactly one `MonitorTransition`. If an auto-incident is
  open for the monitor it is resolved once; the recovery notification is delivered under the
  ordinary outbox rules (instance silence and a superseding transition still apply). A run that
  breaks produces no event, no resolution and no notification.
- **I5** — For a monitor whose configuration does not change, the set of checks scheduled and the
  rows written to `heartbeats` are identical for every value of the threshold: the scheduler's
  cadence, the confirm phase, the pull-job TTL and the expected-run ledger do not read the new
  columns (NFR-036).
- **I6** — For the same `heartbeats`, monitor SLA, project SLA, burn rate, service SLI and budget,
  gate reports and status-page availability bars are identical for every value of the threshold
  (NFR-036).
- **I7** — `last_notified_at` is not cleared by a success that leaves the monitor `down`. After a
  run that breaks, reminders are sent again without any new `down` transition.
- **I8** — While `consecutive_successes > 0`, no reminder is enqueued and no escalation step or
  `repeat_last` repetition fires for the monitor's incident. After the run breaks, every step that
  became due during it fires on the next pass. Service incidents are unaffected.
- **I9** — An auto-incident resolved by a confirmed recovery has `resolved_at` equal to the database
  clock of the transaction that resolves the incident, never earlier than `started_at` or than any
  of its updates.
- **I10** — A value outside 1..10 is refused with 400 on API create, API update, test-connection and
  Monitoring-as-Code decode; a value above 1 on a `push` or `composite` monitor is refused on create,
  update and decode. None of them clamps, and an explicit `0` is refused, not defaulted.
- **I11** — An update that lowers the threshold leaves `status`, `consecutive_successes` and
  `state_sequence` untouched and enqueues nothing.
- **I12** — After a re-enable, a retire or a reactivate, `consecutive_successes = 0` and
  `recovery_started_at IS NULL`.
- **I13** — `recovery_started_at` is non-NULL exactly when `consecutive_successes > 0`, and then the
  monitor is `down` (database CHECK).
- **I14** — The D6 sentence is rendered from the recovery snapshot of the confirming transaction:
  it is correct when read after the commit that cleared the columns, and after the threshold has
  been edited between the recovery and the delivery.

## 7. Test matrix (written before the code)

| Layer | What it proves |
| --- | --- |
| Store, status recording | The six rows of §4.1 as a table test, `last_notified_at` included; I2, I3, I13; the same table with threshold 1 against the pre-change expectations (I1) |
| Store, non-verdict paths | A probe error, a duplicate result and an out-of-order result in the middle of a run leave the counter where it was |
| Store, status writers | Retire, reactivate and re-enable in the middle of a run (I12); the CHECK refuses an inconsistent row (I13) |
| Store, reminders | Reminder due, run starts → not enqueued; run breaks → enqueued without a new `down` (I7, I8) |
| Store, escalations | Step due during a run → not fired; run breaks → fired on the next pass, two overdue steps in the same pass; `repeat_last` paused and resumed; run completes → never fired; a service incident's ladder is not paused (I8) |
| Store, incidents | `resolved_at` at the resolving transaction; ordering against updates written during the run (I9) |
| Store, update | Lowering the threshold mid-run, then the next success confirms and the sentence reads `N/M` with `N > M` (I3, I11) |
| Store, migration | Existing rows get 1 / 0 / NULL; both storage modes |
| Domain | Range and type rules (I10); explicit 0 refused; field classification |
| API | Create and update refuse the same inputs with a message that names the rule; update of an existing `push`/`composite` to a value above 1 refused; an update that omits the field keeps the stored value; test-connection refuses an out-of-range value (I10) |
| File provider | A bundle with the field, without it, out of range, `0`, and above 1 on `push`/`composite`; the field takes part in drift |
| **Ingest + outbox, real database** | The full path `RecordScheduledResult` → `Reconciler.Reconcile` → outbox worker for a threshold-3 recovery: one event, one resolved incident, one notification, and the sentence with the right `N/M` and first-OK instant in both the incident update and the notification — read **after** the columns are cleared, and once more with the threshold edited before delivery (I4, I14). A unit test with a hand-built monitor cannot catch this |
| Outbox | A legacy event without the snapshot renders today's text; a run that breaks delivers nothing (I4); email Subject unchanged, sentence in the body |
| Manual resolve | An auto-incident resolved by a person mid-run: the run breaks → no new incident; the run completes → one transition, no second resolution (D11) |
| Scheduler | For a scripted fail/success timeline the scheduled check times are the same at threshold 1 and at threshold 3 (I5) |
| Regression (NFR-036) | Two monitors, thresholds 1 and 3, fed the same scripted results **through the scheduler and `RecordScheduledResult`**; then `heartbeats`, the expected-run ledger, SLA, burn rate, service reliability and gate report compared field by field (I5, I6). A hand-inserted `heartbeats` fixture read at two thresholds would only prove that the read queries ignore the column |
| Frontend unit | The field, its validation, the long-soak warning, the dependency hint; the "recovering N/M" label |
| E2E | One monitor at threshold 2 against a target that fails, succeeds once, fails, then succeeds twice: one recovery notification, one incident |

## 8. SPA (mock required before code)

- **Monitor form**, reliability block, next to `failure_threshold`: "Recovery threshold" with the
  hint that the monitor stays down until this many checks in a row succeed. Hidden for `push` and
  `composite`.
- **Long-soak warning**: when `interval_seconds × (recovery_threshold − 1)` is 600 s or more, the
  field states how long a recovery will take to be announced.
- **Dependency hint**: when the monitor has a parent, the field explains that a parent with a lower
  threshold recovers — and stops suppressing this monitor — first.
- **Monitor list and detail**: a `down` monitor with a run in progress shows "recovering N/M", the
  counterpart of today's "confirming N/M" (`confirmingLabel` in `frontend/src/views/MonitorsView.vue`).
  The pill stays the `down` pill; the mock decides the exact wording and placement. The detail view
  does not show "confirming N/M" today; whether it gains both labels is the mock's decision.
- The label is computed from `consecutive_successes` and `recovery_threshold` in the monitor read
  model and is as fresh as the last load of the view (D13).

## 9. Non-goals

- Accelerated checks during a run (D4), in any form — including a separate setting.
- An instance-wide or project-wide default (D12).
- A fourth monitor status.
- Live updates of the "recovering N/M" and "confirming N/M" labels (D13).
- Backdating an incident's end, or re-dating its start to the first failure. Whether incident
  duration should be measured from first failure to first success is a separate question about
  **both** ends and about the schema of `incidents`; this document does not open it.
- Flap detection as such — counting transitions per window, automatic thresholds, a "flapping" state.
- Any change to how a service is measured.
- New Prometheus metrics.

Noticed while checking, **not** part of this change: `EnqueueRenotifyReminders` has no `enabled`
condition, so a monitor disabled while `down` is still selected for reminders. Whether delivery
drops them was not verified. It is today's behaviour and is recorded here as a separate finding.

## 10. Questions put to the owner — all six confirmed as recommended (2026-11-14)

Questions 2, 5 and 8 of revision 1 are closed by the code check (D5, D6, D10; §11). The six below
were put to the owner with a recommendation each; the owner confirmed every one on 2026-11-14. No
question is open. The text is kept as the record of what was asked.

1. **D9 — the ladder's clock.** During the owner discussion the pause was described as "the timer
   carries on from the same place". The code has no timer: each step is due at a fixed offset from
   the incident's start. Under D9 a step whose time passed during a run fires on the first pass after
   the run breaks, and several overdue steps fire together. The alternative delays every later step
   by the length of each broken run and needs a stored offset per incident. **Recommended: D9** — an
   outage that has just been confirmed as continuing should not wait longer for the second line.
   What would overturn it: a team whose first line restarts the service on purpose, producing a
   short green run, and must not have the second line paged the moment it ends.
2. **D4 — no acceleration.** Recommended: confirm. The case that hurts is a long interval (300 s and
   above) with a threshold of 3 or more; the form warns about it.
3. **D8 — `composite`.** Recommended: confirm the refusal. What would overturn it: a composite in
   `any` or `quorum` mode over children that flap in turn, where no child is steadily `up` and the
   aggregate jumps. Allowing 1..10 there is technically trivial.
4. **§4.4 — the status page.** A monitor component stays `major_outage` for the length of the run
   while a service component over the same target is already `operational`. Recommended: as stated,
   no intermediate public state.
5. **D12 — defaults.** Recommended: not in this iteration.
6. **D13 — the label is not live.** Recommended: accept for this iteration. The alternative is a new
   live event on every counter change, for this label and for "confirming N/M" together.

## 11. Review history

**External review 1 of revision 1 (2026-11-14): CHANGES REQUIRED, P0 1 / P1 6 / P2 3 / P3 1.**
Every finding was checked against the code on `main`.

| Finding | Disposition |
| --- | --- |
| P0-1 — `recovery_started_at` is cleared by the confirming commit before either consumer of the D6 sentence reads the monitor | **Confirmed.** `Reconciler.Reconcile` and the outbox worker both call `GetMonitor` after the commit; `MonitorTransition` carried no such data. Recovery snapshot added: D6, §4.1, §4.6, I14, and an ingest + outbox test on a real database |
| P1-1 — status is written outside `recordCheckStatusTx` | **Confirmed** for re-enable, `RetireMonitor`, `ReactivateMonitor`; `SetMonitorStatus` has no production caller. §4.1 lists the paths, §5 adds the CHECK, I12 widened. The I7 wording contradicted I1 for `pending` → `up` — rewritten (§4.2, I7). The missing `enabled` condition in the reminder query is today's behaviour — recorded in §9, not fixed here |
| P1-2 — I3, I4, I9 and §4.6 contradicted D10, D11 and the code | **Confirmed**, all four parts. I3 qualified; I4 restated under the ordinary delivery rules; I9 names the resolving transaction; §4.6 gives the literal strings, `N/M`, and keeps the email Subject |
| P1-3 — NFR-036 / I5 were unconditional although every update runs the revision fence | **Confirmed.** NFR-036 and I5 qualified; field classification added to §4.5 |
| P1-4 — a zero-value integer would be clamped to 1; test-connection refuses `push`/`composite` before `Validate` | **Confirmed.** Optional integer on every input, no clamp (§4.5, I10); the test-connection order is kept and I10 narrowed to match |
| P1-5 — §8 claimed the list already consumes a live update | **Confirmed.** The claim is removed. The reviewer's remedy — a new live event — is **not** adopted: it is scope beyond the feature and today's "confirming N/M" has the same limit. Recorded as D13 and as an owner question |
| P1-6 — two function names that do not exist; service alerting does not "go green on the first success" | **Confirmed.** Names corrected (D5, §4.2); the service row of §4.4 split in two |
| P2-1 — `openapi.yaml`, JSON visibility, Monitoring-as-Code surfaces missing | **Accepted.** §4.5, §5 |
| P2-2 — several overdue steps fire in one pass; `repeat_last`; the service query | **Confirmed.** D9, §4.3, I8 |
| P2-3 — gaps in the test matrix, and a regression test that could not fail | **Accepted.** §7: non-verdict paths, status writers, manual resolve, the end-to-end path, and the regression rebuilt on a real run |
| P3-1 — documents to update; metrics | **Accepted.** §9 (no new metrics), §12 |

The reviewer's answers to the eight open questions of revision 1 are reflected in §3 and §10.

## 12. Documents the implementation updates

`openapi.yaml` and the generated TypeScript schema; `docs/overview.md`; `docs/runbook.md` (the two
deliberate consequences of §4.4, the long-soak cost of D4, the overdue-step behaviour of D9);
`docs/traceability.md` (FR-043, NFR-036); `docs/decisions.md` (next free `D-NNNN`);
`docs/iterations/iter-NNNN.md`; `CHANGELOG.md`; this document's status line and the index in
[`README.md`](README.md).
