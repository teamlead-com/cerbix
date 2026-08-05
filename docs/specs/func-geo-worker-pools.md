# Spec: Geo-distributed probers / region-aware worker pools (func-geo-worker-pools)

## Purpose

Allow checking targets reachable only from a specific geo/network segment **from inside
that geo**, without deploying a full cerbix there — only a stateless prober (`--role worker`).
The core (scheduler/api/Postgres/**one central RabbitMQ**) stays centralized;
only the **`worker` role** becomes geo-distributed.

## Problem (current state)

All workers concurrently pull from **one shared queue `checks.jobs`** (default exchange, no
routing key and no notion of "region"): `internal/dispatch/amqp.go` (`jobsQueue`, `publish`),
`dispatch.CheckJob{Monitor}`, the only publisher is `scheduler.go` (the publish loop over the snapshot).
Therefore a worker in geo-1 would also grab geo-2 jobs (failing on unreachable targets), and vice versa.
The `worker` role is meanwhile already **DB-less** (except composites, which read child statuses from the DB) — a "prober-only"
mode is architecturally supported as soon as jobs can be **addressed** to the right pool.

## Model

- New field **`domain.Monitor.Region string`** (json `region`), normalized to lower/trim,
  default — **`core`** (the central pool). Validation: slug `^[a-z0-9-]{1,40}$`.
- The worker process has a **consumption region** (flag `--region`, mirrors `--role`; default `core`).
- In AMQP — **a queue per region**: `checks.jobs.<region>`. `checks.results` remains a single queue.
  The scheduler publishes a job into the queue of the monitor's region; a worker listens only to its own.

## Topology

```
        geo-2 (center)                        geo-1 (edge)
  ┌───────────────────────────┐         ┌────────────────────────┐
  │ api · scheduler · Postgres│         │ cerbix --role worker    │
  │ RabbitMQ (THE ONLY ONE)    │◄────────│   --region geo1         │
  │  checks.jobs.core         │ network │  (DB-less, only rabbitmq│
  │  checks.jobs.geo1 ────────┼─────────┼─► + prober.allow_private│
  │  checks.results           │◄────────┼── publishes heartbeats  │
  └───────────────────────────┘         └────────────────────────┘
```

The network between geos (WireGuard / amqps / amqproxy) is **outside cerbix**, on the admins; the docs give recommendations.

## Requirements

### Functional (FR)

- **FR-GEO-1** — A monitor has a region (`region`, default `core`); set via API/UI, stored in the DB,
  returned in responses, editable.
- **FR-GEO-2** — The scheduler routes a monitor's job into the `checks.jobs.<region>` queue; a worker
  started with `--region R` executes **only** jobs from `checks.jobs.R`.
- **FR-GEO-3** — An empty/unset region ⇒ `core`; existing monitors (migration) = `core`
  and keep running on the central workers (backward compatibility).
- **FR-GEO-4** — **Composite monitors are always `core`** (they need DB access for child statuses):
  forbidden in `Validate()` outside `core` and forced to `core` on publish.
- **FR-GEO-5** — A remote worker runs **without a DB**: `rabbitmq.url` (+ probe policy) is enough.
  It brings up only the ops endpoints (`/healthz` `/readyz` `/metrics`).
- **FR-GEO-6** — Mode `--role=all` (inproc, dev/single-process): the region is ignored, everything
  executes locally (in-proc queue).

### Non-functional (NFR)

- **NFR-GEO-1** — Jobs are published with a TTL (`Expiration` ≈ interval / a reasonable cap) so that when
  a regional worker is unavailable the queue does **not grow indefinitely** and messages expire
  (the next scheduler tick re-publishes).
- **NFR-GEO-2** — One central RabbitMQ for all geos; nothing in cerbix for tunnels/TLS — only
  `rabbitmq.url` (accepts `amqp`/`amqps`). Network security is on the admins.
- **NFR-GEO-3** — Idempotent queue declaration (publishing into an undeclared queue via the default
  exchange is silently lost) with a cache of declared queues on the publisher side.
- **NFR-GEO-4** — A geo worker for internal targets: `prober.allow_private_ips: true` (already the default),
  SSRF guard — per-worker (unchanged).

### Acceptance criteria (AC)

- **AC-GEO-1** — Create/Update of a monitor with `region` passes a round-trip (store + api); default `core`;
  an invalid region → 400; composite outside `core` → 400.
- **AC-GEO-2** — Publishing a job of a monitor with `region=geo1` lands **only** in `checks.jobs.geo1`;
  a worker `--region core` does not see it (an integration AMQP test, opt-in) or via a pure
  `jobsQueueForRegion` + declare logic.
- **AC-GEO-3** — A worker starts with a single `rabbitmq.url` without `database.dsn` and executes jobs
  of its region (E2E docker-compose: scheduler + worker core + worker geo1 against one RabbitMQ).
- **AC-GEO-4** — `--role=all` (inproc): the region is ignored, full `-race` green.

### Definition of Done (DoD)

- FR/NFR/AC = DONE, linked to code+tests; full `-race` green; `gofmt`/`vet`; frontend
  `vue-tsc`+`vite build`; openapi + `schema.d.ts` synchronized; `decisions.md` (new D-) and
  `traceability.md` updated; `overview.md` gets a "Geo-distributed probers" section + network
  recommendations; no self-healing/fallback; strict region validation (the single owner is `domain`).

## Affected files

- `internal/domain/monitor.go` — `Region` field, `Normalize`, `Validate` (composite→core).
- `internal/store/monitors.go` + `migrations/00033_monitor_region.sql` — column/scan/insert/update.
- `internal/dispatch/amqp.go` — `jobsQueueForRegion`, publish (region + declare-once + TTL),
  region consume (`WithJobRegion`); `dispatch.go`/`inproc.go` — unchanged.
- `internal/cli/cli.go` — `--region` flag, passed through to worker/AMQP.
- `internal/api/handlers_monitors.go` — `region` in create/update.
- `openapi.yaml` (+ `frontend/src/api/schema.d.ts`), `frontend/src/views/NewMonitorView.vue`
  (+ opt. `MonitorsView.vue`).
- `docs/overview.md`, `docs/decisions.md`, `docs/traceability.md`.

## Explicitly out of the first pass

- WireGuard/TLS/firewall setup (for the admins; recommendations in the docs).
- HTTP pull agent (a transport without RabbitMQ across geos) and RabbitMQ federation — deferred; the routing
  is designed so they can be added later.
- Multi-region failover, auto-discovery of pools, multiple pools per region.
