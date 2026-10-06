# Spec: SLA / SLI / SLO (func-sla-sli)

## Purpose

Computation and presentation of availability indicators: SLI, target SLOs, error budget; exclusion
of maintenance windows.

## Model (implemented in iter-0006)

- **SLI** = uptime% = up / total over the monitor's/project's heartbeats within a window.
- **Windows**: rolling **24h / 7d / 30d / 90d** (`internal/sla.StandardWindows`).
- **SLO** — `sla_targets` (per monitor + window; project-level is a groundwork); objective in the OPEN interval (0,100), canonical at four decimals — maximum 99.9999 (amended by D-0165/iter-0142: a zero error budget is not a supported configuration, because the shared burn math would answer a total outage with 0×).
- **Error budget** = `1 − SLO`; we show allowed/actual/remaining ratio, burned%, met
  (`sla.ErrorBudget`).
- **Maintenance windows** (`maintenance_windows`, monitor- or project-scoped) — heartbeats
  inside a window are **excluded** from both the numerator and the denominator of the SLI (not a "pause").

Code: `internal/sla/sla.go` (pure functions), `internal/store/sla.go`
(`MonitorSLI`/`ProjectSLI` with `NOT EXISTS` over maintenance), migration `00005_sla.sql`.

## Storage and computation

- SLI is computed by **direct SQL aggregates over `heartbeats`** (`count`, `FILTER (WHERE up)`,
  `avg(latency)` and **`percentile_cont(0.95)` — p95** `FILTER (WHERE up)`, D-0046), works
  on any Postgres → tests are hermetic.
- Long windows are cheaper to compute via rollup: **native daily RANGE partitions** of
  `heartbeats` + the `heartbeats_daily` table are implemented (the leader-scheduler recomputes the window; retention
  drops old partitions, frozen daily rows remain) — D-0037/D-0033.
  **TimescaleDB is not used** (no code dependency).

### Windows longer than raw retention (*Amendment D-0268 / iter-0199*)

Until iter-0199 every SLI window was a raw aggregate, so any window longer than
`heartbeats.retention_days` (default 30, minimum 2) silently covered only the retained days — the
default 90d window reported about 30 days as if they were 90 (BUG-0198-7). The frozen daily rows
above were never read for it. From iter-0199 the **availability** part of every window is computed
as follows, for each monitor `m` in the scope and window start `S = now − duration`:

- Availability is every raw heartbeat in `[S, now)` — as before — **plus**, from `heartbeats_daily`,
  the `up`/`total` of the whole UTC days `D` of the window that hold no raw heartbeat of `m` at all:
  `ceil_to_utc_day(S) ≤ D < utc_day(raw_start(m))`, where `raw_start(m)` is the monitor's earliest
  retained heartbeat (`D < start of today` when it has none).
- `raw_start(m)` is NOT treated as a retention boundary: raw days the rollup has not reached yet
  (yesterday, read before the next rollup pass) are raw and count as raw. The two parts cannot
  overlap, and the day of `raw_start(m)` is whole whenever older data was purged, because purging is
  day-aligned in both storage modes (one-day hypertable chunks; daily partitions and a
  DEFAULT-partition delete at a midnight cutoff).
- If `raw_start(m) ≤ S` the rollup contributes nothing and the window is read exactly as before.
- When the rollup does contribute, the partial first UTC day `[S, ceil_to_utc_day(S))` is not
  counted (its rollup row covers the whole day), so such a window is up to 24 hours shorter than its
  name.
- Maintenance is excluded in both parts: the rollup excludes it when a day is rolled up, raw
  heartbeats exclude it at read time.
- A project's availability is the sum over its monitors.

**Coverage.** A window is *complete* when some monitor in its scope has data older than the window:
`raw_start(m) ≤ S`, or a `heartbeats_daily` row for a UTC day **strictly before** `utc_day(S)`. A
rollup row for the start day itself proves nothing — it may hold only data after `S` — so a monitor
first seen later on `S`'s own day is incomplete. A monitor created on that day before `S`, whose raw
data was later purged, is therefore also reported incomplete: the error is on the side of claiming
less history, never more. An incomplete window states `data_from`: the UTC day of the earliest data
**counted** — in that conservative case the partial start day is not counted, so `data_from` is the
next day, or a later one when the monitor has no data on the next day. The number
is still returned — a young monitor gets its real uptime, never labelled as the full window.

**Latency.** avg and p95 cannot be rebuilt from a daily rollup (it keeps only `up`/`total`), so they
stay raw aggregates over `[S, now)`. When the earliest raw heartbeat in the window falls on a later
UTC day than the first day availability covers, the response states `latency_from` (that day).

**Readers.** The same computation serves `GET /monitors/{id}/sla`, `GET /projects/{id}/sla`, the
90-day uptime of a monitor-backed status-page component, the weekly SLA report windows, and the
**long window of monitor burn-rate rules** (up to 7 days, against a raw retention that may be 2 days).
The report's payload carries the corrected counts but no coverage field. Every burn window goes
through the same computation, so a burn window that raw heartbeats cover reads exactly as before,
while one longer than raw retention — the long window, or a short window that is itself longer
(e.g. 7d/4d against a 2-day retention) — now also counts the rollup days. The service reliability
surfaces have their own materialized facts and are not affected.

**One statement, one snapshot.** Each reader computes its raw aggregate, the earliest raw
heartbeat and the rollup part in a single SQL statement. Read separately, a purge landing between
them would let the raw aggregate count a day that the rollup then adds again.

**Limitations, stated rather than hidden.**
- A day the rollup never produced (the leader was down while that day's raw heartbeats existed, and
  they were then purged) is indistinguishable from a day with no checks: it lowers the totals and
  does not make the window incomplete.
- Days older than the rollup's recompute range (`[today − retention_days, today)`) are frozen: a
  maintenance window created or changed later does not reach them. Their raw heartbeats are gone, so
  no read could apply it either. Inside the range, a maintenance change reaches a rolled-up day at
  the next rollup pass; until then a rollup-backed window can disagree with raw by that day.
- The no-overlap and whole-first-day guarantees assume the shipped day-aligned purge (migration
  00043's one-day chunks, or daily partitions). An operator who changes the hypertable's
  `chunk_time_interval` to something not day-aligned can leave a partially purged first raw day,
  which then counts only its retained part.
- Counts stay heartbeat-weighted, as before.

## API (implemented)

- `GET /api/v1/monitors/{id}/sla` — SLI per window + objective/error budget (if a target is set).
- `PUT /api/v1/monitors/{id}/sla-target` — set the SLO (objective, window).
- `GET /api/v1/projects/{id}/sla` — project SLI per window.
- *Amendment D-0268 / iter-0199:* a window in either response may carry `data_from` (incomplete
  window: the UTC day its data starts) and `latency_from` (the UTC day latency starts, when later than
  availability's). Both are absent when not applicable. A monitor-backed status-page component carries
  the PUBLIC `uptime_since` (the same `data_from`) beside `uptime_90d`, because a number that names a
  window must not claim more history than it has. The UI marks such a window "since <date> UTC"
  ([`mock-sla-long-windows.html`](../design/mock-sla-long-windows.html)).
- `GET|POST /api/v1/projects/{id}/maintenance`, `DELETE /api/v1/maintenance/{id}` — maintenance
  windows. All with authz (`ProjectRead`/`ProjectWrite`) and isolation.

## Requirements

- FR-009 (heartbeat storage + SLA/SLI per window + maintenance windows) — DONE.
- NFR: the computation does not mutate data; maintenance exclusion is guaranteed at the SQL level.

## Open questions / next

- The project-objective card's PRESENTATION is specified by
  [`func-truthful-rendering.md`](func-truthful-rendering.md) §7 (FR-031, D-0235): read-only
  stored state with an explicit Edit, a draft cleared on success, and the two P0 tenant-context
  fixes — the context reset covering the card's own draft/error/busy refs, and a load-generation
  guard on both writers. The model and the API in this document are unchanged by it.

- TimescaleDB hypertable + CAGG for 90d at large volumes (D-0017).
- ~~Project-level SLO targets + an aggregated error budget per project.~~ DELIVERED: AC-0155-3,
  migration 00083, `GET`/`PUT`/`DELETE /api/v1/projects/{projectID}/sla-target`. A project target
  cannot page, by CHECK.
- Burn-rate alerting and displaying the error budget on the status page (status pages phase).
