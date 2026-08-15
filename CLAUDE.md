# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

cerbix — self-hosted uptime & SLA monitoring. One static Go binary embeds the Vue 3 SPA, REST API, and goose migrations. Monorepo: the Go module at the repo root (`cmd/`, `internal/`, Go 1.25), `frontend/` (Vue 3 + Vite + TS), `docs/`, `docker/`, `openapi.yaml` (source of truth for the API). Module path `github.com/teamlead-com/cerbix` matches the GitHub repo, so `go install .../cmd/cerbix@latest` works once the repo is public.

## Commands

### Backend (from the repo root)

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
- The dev compose Postgres is `timescale/timescaledb` and its extension lives in `template1`, so a fresh `cerbix_test` runs the suite in **hypertable mode**. Storage is adaptive (migration 00043); changes touching `heartbeats`/retention must pass in **both modes** — to cover **declarative-partition mode**, spin a throwaway plain `postgres:16-alpine` on another port for the second run.

### Frontend (from `frontend/`) — no local node; use docker

```bash
docker run --rm -v "$PWD":/app -w /app node:22-alpine sh -c "npm run build"   # vue-tsc + vite (run-p type-check build-only)
# After ANY openapi.yaml change — regenerate the TS schema:
docker run --rm -v "$PWD":/app -v "$PWD/../openapi.yaml":/openapi.yaml -w /app node:22-alpine npm run gen:api
```

### E2E (from repo root) — Playwright in docker, against a LIVE stack

```bash
# Canonical single-stack gate starts the required SSO and mail dependencies:
make dev-up
make dev-test                         # 37 pass; idle-provider MaC UI may skip

# Advanced targeted run against that same local stack:
CERBIX_TOPOLOGY=single CERBIX_URL=http://localhost:8080 ./e2e/run.sh tests/monitors.spec.ts
```

The suite signs in ONCE (local logins are rate-limited — never add per-test logins;
reuse the stored session, and never call `/auth/logout` from a spec that shares it).
Tests create `e2e-`prefixed entities and clean them up — dev stacks only.

### Image & stacks (from repo root)

```bash
make dev-init              # once, fresh base broker volume only
make dev-build             # multi-stage SPA + Go image
make dev-up                # role=all :8080 + SSO + mail
make dev-test              # browser suite; idle-provider MaC UI assertion may skip
make dev-down              # stop single before switching topology
make dev-up-distributed    # scheduler + api :8082 + worker; one explicit migration first
make dev-test-distributed  # targeted distributed-role/prober smoke
make dev-down              # no -v; base named volumes survive

make geo-init              # once, fresh geo broker volume only (separate image pin)
make geo-build
make geo-up-all            # central + geo1 AMQP worker + geo2 pull agent
make geo-test
make geo-down              # no -v; geo named volumes survive
# docker/docker-compose.prod.yml — prod role=all, secrets from docker/.env (see .env.prod.example)
```

Base and geo use different env files because they own different retained RabbitMQ volumes.
Make fails on conflicting live topologies and never performs broker upgrades.

Dev login: `admin@cerbix.local` / `devpassword123` (local auth; Keycloak on :8081 optional).

## Architecture (the parts that span multiple files)

**One binary, five roles** (`--role` flag, wired in `internal/cli/cli.go`): `all` (inproc, dev), `api` (REST/SSE/SPA + ingest + outbox), `scheduler` (leader via Postgres advisory lock), `worker` (DB-less AMQP prober pool), `agent` (DB-less **and** broker-less HTTP-pull prober). Transport is the `Dispatcher` abstraction (`internal/dispatch`): `inproc` for dev, AMQP per-region queues `checks.jobs.<region>` for prod.

**Region affinity is strict by design**: a monitor belongs to ONE region; private targets are only reachable from their region's prober. No fallback to core anywhere (test-connection included — no worker/agent in region → 502). Regions without a broker use the **pull transport**: scheduler enqueues into `pull_jobs` (DB queue, `FOR UPDATE SKIP LOCKED` claim), agents poll over HTTPS with long-poll via `pg_notify`/`store.PullNotifier`. Test-connection mirrors the job transport per region (AMQP direct-reply-to vs `pull_tests` table).

**Correctness spine — transactional outbox** (`internal/outbox` + `outbox_events`): heartbeat insert, status flip (`RecordCheckStatus`, confirmation counter + maintenance suppression), and the notification event commit in ONE transaction; the worker delivers with retry/backoff/dead-letter. All alert-suppression logic lives at **delivery time** in the outbox worker (escalation flat-down, dependency-graph `DownAncestors` — fail-open, instance-wide silence). Facts always keep recording; only delivery is muted.

**Scheduler leader loop** (`internal/scheduler`): in-memory snapshot (refresh 15s) + `nextRun` map; sub-tick cadences for rollup/retention/renotify/burn-eval/SLA-reports/region-worker alerts/escalations. Confirm-phase acceleration is push-based: `RecordCheckStatus` NOTIFYs `monitor_confirm`, `store.ConfirmNotifier` wakes the leader (snapshot refresh is the fallback).

**Storage is adaptive** (`store.detectTimescale` at Open): timescaledb present → `heartbeats` is a hypertable (chunks on demand, compression, retention via `drop_chunks`); absent → declarative daily partitions + DEFAULT partition (inserts never lose data) managed by `EnsureHeartbeatPartitions`/`PurgeOldHeartbeats`. Do not assume one mode in store code — branch on `s.timescale`.

**Settings resolve DB → config → defaults** (`internal/settings`, singleton `instance_settings`): OIDC, SMTP, branding, auth policy, alerting silence, monitor defaults are all live-reconfigurable from the UI and **override** the YAML after first save. The YAML is a bootstrap seed; `config.Load` expands `${ENV_VARS}` (prod secrets come from compose `.env`).

**Secrets at rest and in dispatch** (FR-020, D-0155/D-0160): project secrets live in
`project_secrets`, AEAD-bound to `(project_id, secret_id)`; monitors reference them by NAME
via `*_ref`, normalized into `monitor_secret_refs`. **Two keyrings, never shared**:
`security.encryption_key` is the at-rest master and never leaves core; `security.dispatch`
holds per-region keys that executors use to open a `credential_envelope`. Reads are
decrypt-free by SCHEMA (`scanMonitorNoSecrets`), not by post-hoc redaction. Every executor
path crosses ONE gate, `dispatch.ValidateAndMaterialize`, which takes the carrier generation
out of band and refuses a missing, stripped, empty or schema-forbidden credential before any
connection to the target. Legacy inline credentials still ride generation-1 carriers
unchanged, which is what keeps `secrets.enabled: false` meaning "nothing else changes".

### Change-pattern gotchas

- `store/monitors.go` `monitorColumns` is a const shared by every monitor SELECT and includes a correlated `depends_on` aggregate; new columns append at the end **and** to `scanMonitor` in exactly that order, plus Create/Update SQL.
- `outbox_events.topic` has a whitelist CHECK — a new topic needs a migration (see 00042) + a case in the outbox worker + its fakes (`internal/outbox/outbox_test.go`, `internal/api/api_test.go` fakeStore, `internal/scheduler/scheduler_test.go` fakeStore all implement store interfaces and break compilation on interface growth — that's intentional).
- Migrations: goose, embedded (`//go:embed migrations/*.sql`), auto-applied on startup by every role. `-- +goose NO TRANSACTION` + guarded idempotent DO-blocks is the pattern for conditional/DDL-heavy migrations (see 00043). Never attempt `CREATE EXTENSION timescaledb` in a migration (FATAL without preload).
- API errors: `writeError(w, status, msg)` → `{"error": msg}`; handlers are mounted behind session-auth middleware except `PublicRouter` (status pages) and `AgentRouter` (agent-token auth, region-scoped).
- Idempotency markers: system incident_updates use unique prefixes (`⚡ Context:`, `⏸ Suppressed:`) as insert guards — reuse the pattern for new system notes.

## Documentation process (spec-before-code, enforced by convention)

Docs are in **English**. Every feature follows: a spec in `docs/specs/func-*.md` → (if the feature has any SPA surface: an approved UI mock **before** writing frontend code) → implementation → `-race` + E2E on a live stack → iteration report `docs/iterations/iter-NNNN.md` + decision record `docs/decisions.md` (`D-NNNN`, next free number) + a row in `docs/traceability.md` (+ `docs/overview.md` when behavior/stack changes).
