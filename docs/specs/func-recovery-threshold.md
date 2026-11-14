# func-recovery-threshold — a monitor leaves DOWN only after a confirmed run of successes (FR-043 / NFR-036)

> **STATUS: revision 5 — APPROVED by the owner on 2026-11-15; not implemented.** Implementation
> follows sec-hardening-3. Revision 1 (2026-11-14) was written from an owner
> discussion and an independent code check of two design questions. Revision 2 (2026-11-14) answers
> the first external review of that draft — CHANGES REQUIRED, 1 P0 / 6 P1 / 3 P2 / 1 P3. Revision 3
> (2026-11-14) answers the second review, made against `main` at `58b5a32` — CHANGES REQUIRED,
> 0 P0 / 3 P1 / 4 P2 / 1 P3. Revision 4 (2026-11-14) answers the third, made at `caf34f8` — CHANGES
> REQUIRED, 0 P0 / 1 P1 / 0 P2 / 2 P3. Revision 5 (2026-11-15) answers the confirming pass, made at
`6fcc576` — 0 P0 / 0 P1 / 1 P2 / 0 P3, a test-matrix wording only. Every finding of all four rounds
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
Outside the range is refused on every write surface — a 400 in the API, a bundle validation error
in Monitoring-as-Code — not silently clamped.

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
`monitors.recovery_started_at` is stamped — with the database clock, the same clock as every incident
timestamp — when the run goes 0 → 1, and cleared when the run breaks or completes. The check's own
timestamp is not used: a pull agent's clock may run up to `result.allowed_skew` behind, which could
print a "first OK" earlier than the incident's own `started_at`.

The clock is read **after the monitor's row lock is held**: `statement_timestamp()` of a statement
that starts once the row is locked — not `now()`, which is the transaction's start and is taken
before the wait, and not the statement that acquires the lock, whose own timestamp is taken before
the wait as well. `RecordScheduledResult`, `RecordPushResult` and `RecordDeadmanResult` already take
the row with a `SELECT … FOR UPDATE` of their own before they reach `recordCheckStatusTx` (the
latter two act only on push monitors, whose threshold is 1 under D8, and `RecordDeadmanResult`
records failures only, so neither ever stamps `recovery_started_at`);
`Store.RecordCheckStatus`, the exported wrapper, does not, and gains the same locking statement.
`AddIncidentUpdateBySystem` dates `resolved_at` this way for the same reason.

What that guarantees, and what it does not:

- **Guaranteed, always:** the first-OK instant is not earlier than the commit of the `down` verdict
  that precedes the run. Both are serialized by the monitor's row lock.
- **Guaranteed on one ingest consumer:** it is not earlier than the auto-incident's `started_at`
  either. `Consumer.handle` finishes `Reconciler.Reconcile` — which opens the incident — before it
  reads the next result.
- **Not claimed across concurrent consumers.** The auto-incident is opened *after* the verdict's
  commit, in a transaction of its own that does not hold the monitor's row, and `started_at` is that
  transaction's `now()`. With several `api` replicas consuming a backlog, a first success can be
  recorded in the gap, and its first-OK can precede `started_at` by the length of that gap —
  milliseconds, or the incident-open retry backoff. Closing the gap would mean dating the incident
  with the verdict's clock instead of its own, which changes `incidents.started_at` for every
  monitor at every threshold and breaks I1. It is not done here.

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
The per-monitor field ships with the default 1. `MonitorDefaults` (`internal/domain/settings.go`)
does carry `failure_threshold` today — it can, because `Monitor.Normalize` silently forces that field
to 1 for `push` and `composite` accepts any value. The new field has neither escape (D8): an
instance default of 2 would be refused for every `push` and `composite` monitor created without the
field. `confirm_interval_seconds` is absent from `MonitorDefaults` as well.

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
  1..10, and exactly 1 for `push` and `composite`. The rule lives in `Monitor.Validate` and nowhere
  else.
- **Absent is not zero, and the boundary has one owner.** Every input shape carries the field as an
  optional integer (`*int` in the API request bodies and in the file provider's raw monitor). The
  **transport decoders** — `createMonitor` and `updateMonitor` in `internal/api/handlers_monitors.go`,
  `applyCommonMonitorFields` in `internal/fileprovider/parse.go` — resolve an absent value to 1
  (create, decode) or to the stored value (update), and copy an explicit value, `0` included,
  verbatim, before `Normalize` and `Validate`. **Nothing downstream clamps**: not `Monitor.Normalize`
  (unlike `failure_threshold`), not `insertMonitorTx`, not `updateMonitorTxPrepared`. The store writes
  the value it is given, so the CHECK of §5 backs the rule instead of being hidden by it.

  The price is stated: a `domain.Monitor{}` built in process carries 0 and is now refused — by
  `Validate`, or by the CHECK when it is inserted without one. Production has one such builder,
  `testMonitor` (next bullet). Tests have many: about 140 `st.CreateMonitor(ctx, domain.Monitor{…})`
  fixtures in `internal/store`, the literals of `internal/domain/*_test.go`, and the seeded
  monitors of `internal/api/*_test.go` (`seededStore`, `probetargets_test.go`), which `updateMonitor`
  reads back on a PATCH that omits the field. They set
  `RecoveryThreshold: 1`, or go through a helper that lives in a test-only file; a default applied
  in production SQL or in `Normalize` to keep them green is not an acceptable fix — it is the clamp
  this bullet forbids.
- **Test-connection is not a write surface for this field.** `POST …/monitors/test` decodes
  `TestMonitorSpec`, the probe-only body that sec-hardening-2 made strict: a saved monitor's fields
  (`name`, `retries`, `failure_threshold`) are unknown fields and answer 400. `recovery_threshold`
  is one more of them — it is delivery hysteresis, not part of a probe — so `TestMonitorSpec` is
  **not** extended and the field is refused there as unknown. `testMonitor` sets
  `RecoveryThreshold: 1` on the monitor it builds, so that `Monitor.Validate` keeps accepting every
  request it accepts today.
- **Monitoring-as-Code**: `recovery_threshold` is accepted in every bundle format version that
  accepts `failure_threshold` and joins `commonMonitorFields`. In the canonical monitor form
  (`canonicalMonitor`, `internal/fileprovider/canonical.go`) it is serialized with `omitempty` and
  populated **only when it is above 1**. A bundle that omits the key, and one that declares
  `recovery_threshold: 1`, therefore keep the `spec_hash` they had before the upgrade, byte for
  byte, while any change between 1 and 2..10 moves the hash in either direction. Writing the value
  unconditionally would put `"recovery_threshold":1` into every canonical form: the first reconcile
  after the upgrade would plan an update for every file-managed monitor of every bundle — a revision
  bump, `last_result_ts` cleared, in-flight results refused as stale, new service epochs — without
  one line of YAML having changed. `composite` is not a file-supported type (`fileSupportedTypes`),
  so the D8 rule is reachable in a bundle for `push` only.
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

The instant is formatted `firstOK.UTC().Format(time.RFC3339)` — whole seconds, `Z` suffix. The
incident update and the notification are rendered by two different components from the same
snapshot, so the format is fixed here rather than left to each. The sentence follows today's text
after **one space**, on every surface.

| Surface | With `N > 1` |
| --- | --- |
| Auto-incident resolving update | `Monitor recovered — automatically resolved. Recovered: N/M consecutive checks passed; first OK at …` |
| Notification text (Slack, Telegram, the webhook channel's `text`, the email body) | `🟢 <Name> recovered — it is back UP. Recovered: N/M consecutive checks passed; first OK at …` |
| Webhook channel JSON | `"event": "monitor.transition"` and `"up": true` unchanged; no key added. The sentence is in `text` only |
| Email | the **Subject stays today's string**; the sentence is added to the body only |

The notification keeps going through `notify.Dispatcher.Deliver`. `DeliverText` is not a substitute:
it changes the webhook event to `monitor.alert`, drops `up`, and makes the email Subject equal to the
body.

Because the sentence is part of the resolving update's `body`, everything that already publishes
incident updates carries it without formatting of its own: the project webhooks' incident event,
status-page subscriber mail, the public incident timeline and the syndication feeds.

With `N = 1` every text is byte-for-byte what it is today.

## 5. Schema (sketch — the builder owns the exact DDL)

One migration, additive, no backfill beyond the defaults:

- `monitors.recovery_threshold integer NOT NULL DEFAULT 1`, with a CHECK for 1..10;
- `monitors.consecutive_successes integer NOT NULL DEFAULT 0`;
- `monitors.recovery_started_at timestamptz NULL`;
- a CHECK tying the three states together:
  `(consecutive_successes = 0 AND recovery_started_at IS NULL) OR
  (status = 'down' AND consecutive_successes > 0 AND recovery_started_at IS NOT NULL)`;
- a CHECK for D8: `type NOT IN ('push', 'composite') OR recovery_threshold = 1`. `type` is in the
  same row, so the rule costs nothing to enforce where it cannot be bypassed.

The three columns are appended to `monitorColumns` and to the destination list in `scanMonitorAs`
(shared by `scanMonitor`, `scanMonitorForWriter`, `scanMonitorForExecution` and
`scanMonitorNoSecrets`) in the same order, and to the create and update SQL (`recovery_threshold`
only — the other two are server-owned). `heartbeats` is not touched, so the storage mode does not
matter to this change; the migration is still run in both.

`openapi.yaml`: `recovery_threshold` on the monitor read model and on the create and update bodies;
`consecutive_successes` and `recovery_started_at` (nullable) on the read model, read-only.
`TestMonitorSpec` is not changed (§4.5). The TypeScript schema is regenerated.

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
- **I10** — A value outside 1..10 is refused with 400 on API create and API update, and with a
  bundle validation error on Monitoring-as-Code decode. A value above 1 is refused with 400 on a
  `push` or `composite` monitor in the API, and with a validation error on `push` in a bundle
  (`composite` is refused there as an unsupported type before any field is read). Nothing clamps,
  and an explicit `0` is refused, not defaulted. The database refuses both rules independently.
- **I11** — An update that lowers the threshold leaves `status`, `consecutive_successes` and
  `state_sequence` untouched and enqueues nothing.
- **I12** — After a re-enable, a retire or a reactivate, `consecutive_successes = 0` and
  `recovery_started_at IS NULL`.
- **I13** — `recovery_started_at` is non-NULL exactly when `consecutive_successes > 0`, and then the
  monitor is `down` (database CHECK).
- **I14** — The D6 sentence is rendered from the recovery snapshot of the confirming transaction:
  it is correct when read after the commit that cleared the columns, and after the threshold has
  been edited between the recovery and the delivery. The incident update and the notification carry
  the same string, character for character.
- **I15** — A bundle that omits `recovery_threshold`, or declares it as 1, has the same `spec_hash`
  as before this change: the first reconcile after the upgrade plans no update for a monitor whose
  YAML did not change. A change between 1 and any value above it is detected as drift.
- **I16** — `POST …/monitors/test` accepts every body it accepts today and refuses
  `recovery_threshold` as an unknown field (400); `TestMonitorSpec` is unchanged.
- **I17** — `recovery_started_at` is never earlier than the commit of the `down` verdict that
  precedes the run, however the two transactions were started. On a single ingest consumer it is
  also never earlier than the `started_at` of the auto-incident that verdict opened. Ordering
  against `incidents.started_at` across concurrent consumers is not claimed (D6).

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
| Store, clock | While another transaction holds the monitor's row, a first success on a monitor with a threshold above 1 is submitted through both entry points that can start a run — `RecordScheduledResult` and `Store.RecordCheckStatus` — and is observed waiting on the row lock in `pg_stat_activity`; only then the database clock is read; the row is released. The stamped first-OK is not earlier than that reading — with `now()`, or with the timestamp of the locking statement, it is (I17). On the sequential path `RecordScheduledResult` → `Reconciler.Reconcile` → `RecordScheduledResult` the first-OK is not earlier than the incident's `started_at` |
| Store, constraints | The CHECKs refuse 0, 11, and a value above 1 on `push` and `composite`, on insert and on update (I10) |
| API | Create and update refuse the same inputs with a message that names the rule; update of an existing `push`/`composite` to a value above 1 refused; an update that omits the field keeps the stored value (I10). Test-connection: every body accepted today is still accepted, and `recovery_threshold` is refused as an unknown field; `TestTestConnectionBodySchemaIsWhatTheServerDecodes` stays green with `TestMonitorSpec` untouched (I16) |
| File provider | A bundle with the field, without it, out of range, `0`, and above 1 on `push`; the hash of a bundle without the key and of one with `recovery_threshold: 1` equals the pre-change hash, and 1 ↔ 3 is drift in both directions (I15) |
| **Ingest + outbox, real database** | The full path `RecordScheduledResult` → `Reconciler.Reconcile` → outbox worker for a threshold-3 recovery: one event, one resolved incident, one notification, and the sentence with the right `N/M` and first-OK instant in both the incident update and the notification — read **after** the columns are cleared, and once more with the threshold edited before delivery (I4, I14). A unit test with a hand-built monitor cannot catch this |
| Outbox | A legacy event without the snapshot renders today's text; a run that breaks delivers nothing (I4); email Subject unchanged, sentence in the body |
| Manual resolve | An auto-incident resolved by a person mid-run: the run breaks → no new incident; the run completes → one transition, no second resolution (D11) |
| Scheduler | For a scripted fail/success timeline the scheduled check times are the same at threshold 1 and at threshold 3 (I5) |
| Regression (NFR-036) | Two monitors, thresholds 1 and 3, fed the same scripted results **through the scheduler and `RecordScheduledResult`**; then `heartbeats`, the expected-run ledger, SLA, burn rate, service reliability and gate report compared field by field (I5, I6). A hand-inserted `heartbeats` fixture read at two thresholds would only prove that the read queries ignore the column |
| Frontend unit | The field, its validation, the long-soak warning, the dependency hint; the "recovering N/M" label, including `3/2` after the threshold was lowered; a type switched to `push` or `composite` after the field was edited omits `recovery_threshold` from the request; the multi-region set gives the children the form's value and omits the field on the composite parent |
| E2E | One monitor at threshold 2 against a target that fails, succeeds once, fails, then succeeds twice: one recovery notification, one incident |

**Existing tests and callers this change moves.** They are named so that none is "fixed" by weakening
a rule:

- the in-process monitor literals — about 140 fixtures in `internal/store/*_test.go`, the tables of
  `internal/domain/*_test.go` and the seeded monitors of `internal/api/*_test.go` — set
  `RecoveryThreshold: 1` or use a test-only helper (§4.5). The value is set on the fixture, never
  defaulted inside a fake store's read;
- `TestEveryDeclaredTypeCarriesEveryCommonField` (`internal/fileprovider/commonfields_test.go`)
  feeds one common block to every file-supported type, `push` included; a value above 1 in that block
  is refused for `push` under D8, so the test branches by type;
- `Store.RecordCheckStatus`, the exported wrapper used by integration tests, gains the row-locking
  statement of D6; it keeps its signature and drops the snapshot, or its callers are updated — the
  builder's choice, stated in the report;
- direct callers of `Reconciler.Reconcile` and the notify fakes in `internal/outbox` change with the
  signatures that carry the snapshot.

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
  model and is as fresh as the last load of the view (D13). It is shown whenever
  `status === "down" && consecutive_successes > 0` — **not** gated on `< recovery_threshold`, as
  `confirmingLabel` is on its own threshold: after the threshold is lowered during a run (D10) the
  monitor is still `down` with a live run, and the label reads e.g. `3/2` until the next check.
- **Monitor detail, Configuration list**: "Recovery threshold" next to "Failure threshold", for the
  types that have the field.
- **What the form sends.** For `push` and `composite` the form sends no `recovery_threshold`,
  whatever the field held before the type was switched — a hidden 3 would be a 400 under D8. In the
  multi-region set (`createMultiRegionSet`, `frontend/src/views/NewMonitorView.vue`) the regional
  child monitors receive the form's value and the composite parent receives none: a value above 1 on
  the parent is refused, which would roll back the children already created.

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
condition, so a monitor disabled while `down` is still selected for reminders. The second review
read the delivery side as well: the outbox worker does not check `enabled` either, so those
reminders are delivered. It is today's behaviour and is recorded here as a separate finding.

Stated so that it is not mistaken for a breach of NFR-036: the **displayed duration** of an
auto-incident (`resolved_at − started_at`) grows by `(recovery_threshold − 1) × interval_seconds`.
That is D5, chosen on purpose; no measured number reads `incidents.resolved_at`.

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

**External review 2 of revision 2 (2026-11-14, against `main` at `58b5a32`): CHANGES REQUIRED,
P0 0 / P1 3 / P2 4 / P3 1.** The round found the snapshot design sound under concurrent results,
edits during a run, both storage modes and both transports, and confirmed NFR-036 by reading every
SLA, burn-rate, service, gate and status-page query. Every finding was checked against the code.

| Finding | Disposition |
| --- | --- |
| P1-1 — test-connection decodes the strict, probe-only `TestMonitorSpec`; adding the field breaks its contract test, and the unset field on the synthetic monitor would make `Validate` refuse every test request | **Confirmed.** `TestMonitorSpec` is not extended; `testMonitor` sets 1 (§4.5, §5, I10, I16, §7). Revision 2's disposition of review 1's P1-4 was wrong on this surface |
| P1-2 — the field in the canonical monitor form would change the `spec_hash` of every file-managed monitor at upgrade | **Confirmed**: `failure_threshold` is serialized unconditionally and the same shape here would add `"recovery_threshold":1` everywhere. `omitempty`, populated only above 1 (§4.5, I15, §7) |
| P1-3 — no clamp, a CHECK and unset in-process literals collide | **Confirmed.** The boundary has one owner, the transport decoders; nothing downstream clamps; the fixtures change, about 140 in `internal/store` by count (§4.5, §7) |
| P2-1 — the D8 rule cannot be tested for `composite` in a bundle; a bundle error is not a 400 | **Confirmed.** I10 and §7 narrowed to `push` |
| P2-2 — separator, timestamp precision and the channel contract were not fixed | **Accepted.** §4.6: one space, `time.RFC3339`, the webhook JSON and email Subject unchanged, `Deliver` kept; I14 widened |
| P2-3 — the multi-region set, a hidden value after a type switch, the label predicate under D10, the detail view | **Confirmed**, with one correction: the composite parent is created with `failure_threshold: 1`, not with the form's value. §8 and the frontend row of §7 |
| P2-4 — D12 said `MonitorDefaults` has no `failure_threshold`; it has | **Confirmed.** The premise is corrected; the decision stands on its second argument |
| P3-1 — `scanMonitorAs`; no database rule for D8; two documents missing from §12 | **Accepted**, all three (§5, §12) |
| Unnumbered, from the concurrency check — `now()` is taken before the wait for the row lock, so a first-OK could predate the incident it belongs to | **Confirmed** for the clock. D6 names `statement_timestamp()` after the lock. The guarantee revision 3 derived from it was too strong — see review 3 |
| Unnumbered — the reminder finding of §9 | The reviewer verified the delivery side; §9 updated |

**External review 3 of revision 3 (2026-11-14, against `main` at `caf34f8`): CHANGES REQUIRED,
P0 0 / P1 1 / P2 0 / P3 2.** The round confirmed every finding of review 2 closed but the clock, and
found no production path that builds a monitor in process and is now refused.

| Finding | Disposition |
| --- | --- |
| P1-1 — I17 promised a first-OK not earlier than `incidents.started_at`, but the auto-incident is opened after the verdict's commit, in its own transaction, without the monitor's row; and `Store.RecordCheckStatus` reads the clock in the statement that takes the lock | **Confirmed**, both parts. The reviewer's variant A is taken: D6 states what the row lock does and does not order, I17 and the clock test are rebuilt on the verdict's commit, the wrapper locks first. Variant B — dating the incident with the verdict's clock — is **not** adopted: it changes `incidents.started_at` at threshold 1 and breaks I1 |
| P3-1 — §7 expected `recovery_threshold: 1` on the composite parent where §8 says the key is omitted | **Accepted.** §7 follows §8: omitted |
| P3-2 — the seeded monitors of `internal/api/*_test.go` were missing from the fixture list; D3 still said "a 400 on every write surface" | **Confirmed.** §4.5, §7, D3 |

**Confirming pass on revision 4 (2026-11-15, against `main` at `6fcc576`): CHANGES REQUIRED,
P0 0 / P1 0 / P2 1 / P3 0.** The pass confirmed the three findings of review 3 closed and found no
contradiction between D6 and I1, I9, I14 or between §7 and §8; it asked for no further pass.

| Finding | Disposition |
| --- | --- |
| P2-1 — the clock test of §7 named four entry points, two of which cannot stamp a first-OK (`RecordDeadmanResult` records failures only; `RecordPushResult` acts on push monitors, threshold 1), and read the clock without waiting for the lock wait to be visible | **Confirmed.** §7 names the two reachable entry points and the `pg_stat_activity` barrier; D6 says why the other two never stamp |

## 12. Documents the implementation updates

`openapi.yaml` and the generated TypeScript schema; `docs/overview.md`; `docs/runbook.md` (the two
deliberate consequences of §4.4, the long-soak cost of D4, the overdue-step behaviour of D9);
`docs/traceability.md` (FR-043, NFR-036); `docs/project-description.md` and `docs/status.md`, where
FR-043 and NFR-036 are registered; `docs/decisions.md` (next free `D-NNNN`);
`docs/iterations/iter-NNNN.md`; `CHANGELOG.md`; this document's status line and the index in
[`README.md`](README.md).
