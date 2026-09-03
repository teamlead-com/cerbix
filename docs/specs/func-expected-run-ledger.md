# Spec: The fact that a run was expected (func-expected-run-ledger)

> **Lifecycle. NOT DESIGNED.** Opened by `D-0235` at iter-0174 as the requirement that must exist
> before any surface may draw a value across an interval it did not observe. This document states
> the problem, the evidence that the fact is missing, and the facts a solution must carry. It does
> **not** contain a design: no schema, no API, no retention rule, no volume analysis. Those need
> their own design review and a data-model migration plan.

## 1. The problem, in one sentence

cerbix records what happened. It does not record what was **supposed** to happen — so it cannot
tell the difference between "no check was due here" and "a check was due here and never ran".

## 2. Why this is a requirement and not a chart detail

FR-031 wanted a connected line on the monitor's Response time panel: a stroke between two
recorded checks, broken wherever a check was due and missing. Two designs for deriving that from
existing facts were proposed and both were rejected, the second at party [166] on three readings
of the tree that were verified before acceptance:

- `internal/prober/prober.go` runs `Retries + 1` attempts, each under its own
  `context.WithTimeout(m.Timeout())`, so one run may occupy up to `(retries+1) × timeout`.
- `result.allowed_skew` is only step 4b's clock test in `internal/store/monitors.go` —
  `ts.Before(hb.JobIssuedAt.Add(-skew))`, a bound on how far *before* its job's issue an
  observation may claim to be. It bounds neither queue delay nor delivery.
- `job_issued_at` is **not a column of `heartbeats`**. It rides the wire for correlation and is
  gone after ingest.

So two received heartbeats bound **observed spacing** and nothing more. Under broker or worker
trouble the queue wait is unbounded; leader absence leaves no trace at all; and cerbix cannot
witness its own absence. No allowance closes that, because the subject of the allowance — an
expected run — is not in the data.

The consequence for FR-031 is recorded in `func-truthful-rendering.md` §6.2: the panel draws
points, no stroke, no fill, and renders absence positively through an observation ruler. That is
honest with today's facts. **This requirement is what would make a stroke defensible.**

## 3. The facts a solution must carry

Specified by the reviewer at party [166] and [169] as the minimum, and recorded here so a later
design cannot quietly ship less:

1. **Materialized due windows** — the intervals in which a run was expected, as a durable fact
   rather than a recomputation from current configuration.
2. **Issue, claim and terminal-outcome timestamps** per expected run, so a run that was issued and
   never completed is distinguishable from one that was never issued.
3. **Historical configuration binding** — the interval, retry count and timeout in force for
   *that* run. Current monitor fields may not stand in for history: a monitor whose interval
   changed last week says nothing true about the week before.
4. **Retention semantics** — how long the ledger is kept, and what a surface may claim about a
   span whose ledger rows have been dropped.

Only acceptance criteria over these facts may permit a `covered` verdict, and only a `covered`
verdict may permit a stroke between adjacent windows.

## 4. Requirement

- **FR-032** — cerbix records that a run was expected, so a surface can distinguish an interval
  nothing was due in from an interval whose due run never happened. Status `TODO`.

## 5. Open questions, stated as open

- **Volume.** One row per due run is heartbeat-order volume. Whether the ledger is a separate
  table, a widened `heartbeats`, or a rollup that keeps only the windows where expectation and
  outcome *disagree*, is the first design question and it is not answered here.
- **Ownership.** The scheduler decides what is due; the store owns durability; the transport owns
  claim and delivery. Which role writes which fact, and in which transaction, is undesigned.
- **Distributed roles.** `worker` and `agent` are DB-less by design. How a claim or a terminal
  outcome from a broker-less pull agent reaches the ledger without giving those roles a database
  is undesigned.
- **Scope beyond the line.** A durable record of expected-versus-actual runs plausibly serves more
  than one surface — coverage reporting and the reliability gate come to mind — but this document
  does not claim that, because nothing has been analysed. It is named as a possibility, not a
  benefit.

## 6. Process

Its own design review, its own mock if it grows any UI surface, and a migration plan, before any
code. It is deliberately **not** carried by iter-0174: that iteration ships the honest panel, and
this one changes the reliability data model.
