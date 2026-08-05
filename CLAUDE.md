# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

cerbix — self-hosted uptime & SLA monitoring. One static Go binary embeds the Vue 3 SPA, REST API, and goose migrations. Monorepo: `backend/` (Go 1.25), `frontend/` (Vue 3 + Vite + TS), `docs/`, `deploy/`, `openapi.yaml` (source of truth for the API). Module path is `github.com/teamlead-com/cerbix` (matches the GitHub repo; the repo is private, so external consumers need `GOPRIVATE=github.com/teamlead-com`).

## Commands

### Backend (from `backend/`)

```bash
go build -buildvcs=false ./...        # -buildvcs=false: repo may be a non-git checkout
go vet ./...

# Store/integration tests are DB-gated: they SKIP without this env var.
export CERBIX_TEST_DATABASE_DSN="postgres://cerbix:cerbix@localhost:5432/cerbix_test?sslmode=disable"
go test -race -count=1 ./...                          # full suite (30 packages)
go test ./internal/store/ -run TestName -count=1 -v   # single test
```

Test DB discipline:
- **Never point tests at the dev `cerbix` DB** — tests run `TruncateAll`. Use `cerbix_test`.
- Migrations auto-apply in test helpers (`Migrate → Open → TruncateAll`). After **changing** an already-applied migration file, drop and recreate `cerbix_test`.
- The dev compose Postgres is `timescale/timescaledb` and its extension lives in `template1`, so a fresh `cerbix_test` runs the suite in **hypertable mode**. CI (`.ci/`) uses plain `postgres:16-alpine` → **declarative-partition mode**. Storage is adaptive (migration 00043); changes touching `heartbeats`/retention must pass in **both modes** — spin a throwaway `postgres:16-alpine` on another port for the second run.

### Frontend (from `frontend/`) — no local node; use docker

```bash
docker run --rm -v "$PWD":/app -w /app node:22-alpine sh -c "npm run build"   # vue-tsc + vite (run-p type-check build-only)
# After ANY openapi.yaml change — regenerate the TS schema:
docker run --rm -v "$PWD":/app -v "$PWD/../openapi.yaml":/openapi.yaml -w /app node:22-alpine npm run gen:api
```

### Image & stacks (from repo root)

```bash
docker compose -f deploy/docker-compose.yml build            # multi-stage: node builds SPA → embedded → distroless
# Single-geo file is profile-driven; `single` and `distributed` are mutually exclusive:
docker compose -f deploy/docker-compose.yml --profile single up -d          # one process --role all, :8080 (static IPs 10.5.0.x)
docker compose -f deploy/docker-compose.yml --profile distributed run --rm api migrate --config /etc/cerbix/config.yaml  # migrate ONCE first — roles racing a new migration fail with "relation already exists"
docker compose -f deploy/docker-compose.yml --profile distributed up -d     # scheduler + api (:8082) + worker
# add --profile sso to either for Keycloak (:8081) + MariaDB
# deploy/docker-compose.geo.yml  — multi-geo: central always, remote sites via --profile geo1/geo2 (3 isolated subnets)
# deploy/docker-compose.prod.yml — prod role=all, secrets from deploy/.env (see .env.prod.example)
```

Dev login: `admin@cerbix.local` / `devpassword123` (local auth; Keycloak on :8081 optional).

## Architecture (the parts that span multiple files)

**One binary, five roles** (`--role` flag, wired in `internal/cli/cli.go`): `all` (inproc, dev), `api` (REST/SSE/SPA + ingest + outbox), `scheduler` (leader via Postgres advisory lock), `worker` (DB-less AMQP prober pool), `agent` (DB-less **and** broker-less HTTP-pull prober). Transport is the `Dispatcher` abstraction (`internal/dispatch`): `inproc` for dev, AMQP per-region queues `checks.jobs.<region>` for prod.

**Region affinity is strict by design**: a monitor belongs to ONE region; private targets are only reachable from their region's prober. No fallback to core anywhere (test-connection included — no worker/agent in region → 502). Regions without a broker use the **pull transport**: scheduler enqueues into `pull_jobs` (DB queue, `FOR UPDATE SKIP LOCKED` claim), agents poll over HTTPS with long-poll via `pg_notify`/`store.PullNotifier`. Test-connection mirrors the job transport per region (AMQP direct-reply-to vs `pull_tests` table).

**Correctness spine — transactional outbox** (`internal/outbox` + `outbox_events`): heartbeat insert, status flip (`RecordCheckStatus`, confirmation counter + maintenance suppression), and the notification event commit in ONE transaction; the worker delivers with retry/backoff/dead-letter. All alert-suppression logic lives at **delivery time** in the outbox worker (escalation flat-down, dependency-graph `DownAncestors` — fail-open, instance-wide silence). Facts always keep recording; only delivery is muted.

**Scheduler leader loop** (`internal/scheduler`): in-memory snapshot (refresh 15s) + `nextRun` map; sub-tick cadences for rollup/retention/renotify/burn-eval/SLA-reports/region-worker alerts/escalations. Confirm-phase acceleration is push-based: `RecordCheckStatus` NOTIFYs `monitor_confirm`, `store.ConfirmNotifier` wakes the leader (snapshot refresh is the fallback).

**Storage is adaptive** (`store.detectTimescale` at Open): timescaledb present → `heartbeats` is a hypertable (chunks on demand, compression, retention via `drop_chunks`); absent → declarative daily partitions + DEFAULT partition (inserts never lose data) managed by `EnsureHeartbeatPartitions`/`PurgeOldHeartbeats`. Do not assume one mode in store code — branch on `s.timescale`.

**Settings resolve DB → config → defaults** (`internal/settings`, singleton `instance_settings`): OIDC, SMTP, branding, auth policy, alerting silence, monitor defaults are all live-reconfigurable from the UI and **override** the YAML after first save. The YAML is a bootstrap seed; `config.Load` expands `${ENV_VARS}` (prod secrets come from compose `.env`).

**Secrets at rest**: monitor config keys in `domain.SecretMonitorConfigKeys`, channel credentials, SMTP password are AES-256-GCM encrypted via `internal/secret` keyring; API responses go through `Redacted()`.

### Change-pattern gotchas

- `store/monitors.go` `monitorColumns` is a const shared by every monitor SELECT and includes a correlated `depends_on` aggregate; new columns append at the end **and** to `scanMonitor` in exactly that order, plus Create/Update SQL.
- `outbox_events.topic` has a whitelist CHECK — a new topic needs a migration (see 00042) + a case in the outbox worker + its fakes (`internal/outbox/outbox_test.go`, `internal/api/api_test.go` fakeStore, `internal/scheduler/scheduler_test.go` fakeStore all implement store interfaces and break compilation on interface growth — that's intentional).
- Migrations: goose, embedded (`//go:embed migrations/*.sql`), auto-applied on startup by every role. `-- +goose NO TRANSACTION` + guarded idempotent DO-blocks is the pattern for conditional/DDL-heavy migrations (see 00043). Never attempt `CREATE EXTENSION timescaledb` in a migration (FATAL without preload).
- API errors: `writeError(w, status, msg)` → `{"error": msg}`; handlers are mounted behind session-auth middleware except `PublicRouter` (status pages) and `AgentRouter` (agent-token auth, region-scoped).
- Idempotency markers: system incident_updates use unique prefixes (`⚡ Context:`, `⏸ Suppressed:`) as insert guards — reuse the pattern for new system notes.

## Documentation process (spec-before-code, enforced by convention)

Docs are in **English**. Every feature follows: a spec in `docs/specs/func-*.md` → (if the feature has any SPA surface: an approved UI mock **before** writing frontend code) → implementation → `-race` + E2E on a live stack → iteration report `docs/iterations/iter-NNNN.md` + decision record `docs/decisions.md` (`D-NNNN`, next free number) + a row in `docs/traceability.md` (+ `docs/overview.md` when behavior/stack changes).
