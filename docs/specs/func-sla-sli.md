# Spec: SLA / SLI / SLO (func-sla-sli)

## Purpose

Computation and presentation of availability indicators: SLI, target SLOs, error budget; exclusion
of maintenance windows.

## Model (implemented in iter-0006)

- **SLI** = uptime% = up / total over the monitor's/project's heartbeats within a window.
- **Windows**: rolling **24h / 7d / 30d / 90d** (`internal/sla.StandardWindows`).
- **SLO** — `sla_targets` (per monitor + window; project-level is a groundwork); objective 0..100.
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

## API (implemented)

- `GET /api/v1/monitors/{id}/sla` — SLI per window + objective/error budget (if a target is set).
- `PUT /api/v1/monitors/{id}/sla-target` — set the SLO (objective, window).
- `GET /api/v1/projects/{id}/sla` — project SLI per window.
- `GET|POST /api/v1/projects/{id}/maintenance`, `DELETE /api/v1/maintenance/{id}` — maintenance
  windows. All with authz (`ProjectRead`/`ProjectWrite`) and isolation.

## Requirements

- FR-009 (heartbeat storage + SLA/SLI per window + maintenance windows) — DONE.
- NFR: the computation does not mutate data; maintenance exclusion is guaranteed at the SQL level.

## Open questions / next

- TimescaleDB hypertable + CAGG for 90d at large volumes (D-0017).
- Project-level SLO targets + an aggregated error budget per project.
- Burn-rate alerting and displaying the error budget on the status page (status pages phase).
