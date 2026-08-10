# Onboarding — cerbix

Internal uptime & SLA monitoring: monitors → heartbeats → SLA/incidents/status
pages, with OpenID Connect (any issuer) + local login, org→project multi-tenancy and role-based access control (Global/Org/Project roles). Go backend,
Postgres, RabbitMQ, Vue 3 SPA. Monorepo: the Go module at the root (`cmd/`, `internal/`), `frontend/`, `docs/`, `docker/`.

## 1. Read this first

**`docs/` is the source of truth — spec beats code.** Before touching anything:

- [`docs/project-description.md`](docs/project-description.md) — the PRD.
- [`AGENTS.md`](AGENTS.md) — the **docs-driven, iteration-based methodology** everyone follows.
- [`docs/status.md`](docs/status.md) — live FR/NFR/AC checklist (what's done, with links to code+tests).
- [`docs/decisions.md`](docs/decisions.md) — every non-obvious decision (`D-XXXX`), newest last.
- [`docs/iterations/iter-XXXX.md`](docs/iterations/) — immutable per-iteration reports.
- [`openapi.yaml`](openapi.yaml) — the REST contract; the TS client is generated from it.

## 2. Run it locally

```bash
# full dev stack (pg+timescale, rabbitmq, an OIDC IdP [Keycloak in dev], cerbix)
docker compose -f docker/docker-compose.yml up --build

# or just the backend against your own Postgres:
./bin/cerbix migrate --config docker/config.example.yaml
./bin/cerbix serve   --config docker/config.example.yaml --role all
# :8080/healthz  /readyz  /metrics ; SPA at /
```

One binary, `--role api|scheduler|worker|all`. `all` runs everything in one process (local dev); in
production they scale as separate deployments over RabbitMQ. With no `database.dsn` the binary runs in
scaffold mode (ready immediately, no persistence).

## 3. Architecture in one screen

```
OIDC IdP/local ─▶ api (REST + SSE, RBAC, serves SPA, consumes results)
                    │ pgx
              Postgres ◀── heartbeats (daily partitions) + heartbeats_daily rollup + outbox_events
                    ▲
   scheduler (advisory-lock leader) ──publish──▶ Dispatcher ──▶ worker pool (prober registry)
   · in-memory monitor snapshot (15s refresh)      (inproc dev / RabbitMQ prod)   · SSRF-guarded dials
   · batched push-liveness (1 SQL)                                                 · HTTP/TCP/ICMP/Push
   · daily partition maintenance + retention
   · availability rollup
```

- **Dispatcher** seam: `inproc` (dev/tests) or `rabbitmq` (prod), same interface.
- **Ingest** consumes results → writes heartbeats, updates status, opens/resolves auto-incidents.
- **Transactional outbox**: incident webhooks + monitor-transition notifications are enqueued *in the
  same DB transaction* as the state change, then delivered by the `outbox` worker with retry/backoff
  and a dead-letter state (`FOR UPDATE SKIP LOCKED`, multi-replica-safe).
- **SLA/SLO**: rolling 24h/7d/30d/90d windows with error budget; uptime %, **avg and p95 latency**
  (`percentile_cont(0.95)`), maintenance-window exclusion — computed per monitor and per project
  (`internal/sla`, `store/sla.go`). See D-0046.
- **Auth**: provider-agnostic **OpenID Connect** (any issuer — Keycloak, Auth0, Okta, Google, Entra ID;
  Keycloak is only the dev-stack IdP) + local (argon2id) + service-account bearer tokens + OIDC
  client-credentials JWT for machines (audience check relaxed, authz still via memberships). All resolve
  to an `authz.Principal` and go through `authz.Can(action, org, project)`. The SPA discovers what an
  instance offers via public `GET /auth/config` → `{local, oidc, oidc_button_label}` and renders a
  provider-neutral sign-in button (label from `oidc.button_label`, default "Continue with SSO"). The user
  identity column is `oidc_sub`, JIT-provisioned via `store.UpsertUserByOIDCSub`. See D-0043/D-0044/D-0045.

Package map (`internal/`): `config` `logging` `metrics` `httpsrv` · `auth` `authz` ·
`store` (pgx + goose migrations) `domain` · `dispatch` `scheduler` `worker` `prober` `ingest` ·
`sla` `notify` `webhook` `outbox` `secret` · `api` `web` `feed`.

## 4. Development workflow (the short version of AGENTS.md)

1. Read the canon (`status.md`, `decisions.md`, last 2–3 iteration reports).
2. Update `docs/status.md` with the FR/NFR you're moving; plan P0→P2.
3. Implement with single-owner validation (transport / domain / application / infra layers).
4. Tests green, `gofmt`/`vet` clean, **coverage gate 70%**.
5. Write an immutable `docs/iterations/iter-XXXX.md`; add `D-XXXX` + a traceability row for any
   contract/reliability change.
6. No self-healing / silent fallback; strict config, fail-fast before business logic.

## 5. Testing

- **Hermetic** — default `go test ./...`; no external services.
- **DB-gated** — set `CERBIX_TEST_DATABASE_DSN` to a throwaway Postgres; store/api/scheduler suites use it.
- **AMQP** — opt-in via `CERBIX_TEST_RABBITMQ_URL`.
- **Real-binary e2e** — build + run `cerbix serve --role=all` against a container Postgres; the pattern
  in recent iteration reports (login → act → assert DB/metrics) is the template.

Everything runs in Docker (Go 1.25). Frontend is **Docker-only** (no host Node); `docker build` needs
`--network=host` on this host.

## 6. Common tasks

- **Add a prober**: implement `prober.Prober` (`Probe(ctx, monitor) Result`), register it in
  `prober.go`'s registry by `domain.MonitorType`, widen the DB `monitors_type_check` via a new
  migration, add the type to the domain enum + `openapi.yaml`. (ICMP needs `CAP_NET_RAW` /
  `ping_group_range`.)
- **Add a migration**: `internal/store/migrations/000NN_*.sql` (goose Up/Down, embedded); additive +
  reversible; add new tables to `TruncateAll`.
- **Add an API endpoint**: handler in `internal/api/handlers_*.go`, route in `api.go` `Router()`, method
  on the `Store` interface (+ the test fake), authz via `p.Can(...)`, document in `openapi.yaml`.
- **Rotate the encryption key**: new key → `security.encryption_key`, old key → `security.previous_keys`,
  run `cerbix reencrypt --config …`, then drop the old key. (See README → Security & operations.)

Existing surfaces worth knowing before you add new ones:

- **Global search**: `GET /api/v1/search` is a tenant-scoped fan-out over projects/monitors/incidents
  (`store/search.go`, `handlers_search.go`), filtered by `p.VisibleProject(...)` and wired to the topbar
  search box (`SearchBox.vue`). See D-0047.
- **Frontend**: the Vue SPA covers login, dashboard, monitor list/detail, new/edit monitor, SLA, members,
  incidents (+detail/new), status pages (+public render), plus a **Settings page** (notification channels /
  API tokens / webhooks) and **org/project creation** dialogs (`CreateDialog.vue`,
  `workspace.createOrg/createProject`, gated by global/org admin). Charts are bespoke inline SVG
  (`Sparkline.vue`, the latency chart, availability strips) with plain `onMounted`/`watch` data loading —
  no TanStack Query/uPlot. See D-0048/D-0049.

## 7. Gotchas

- **Postgres runs UTC** — day partitions and rollups assume it; keep it that way.
- **`heartbeats` is partitioned** — a `DEFAULT` partition keeps inserts safe; the scheduler leader
  pre-creates upcoming daily partitions and drops expired ones. Don't `DELETE` for retention; drop
  partitions.
- **Retention ⇄ rollup coupling** — `heartbeats.retention_days` bounds *both* raw retention and the
  rollup recompute window; changing one without understanding the other can wipe long-range SLA history
  (see D-0037).
- **Secrets** — never log channel configs or webhook secrets; they're encrypted at rest when a key is
  set. API-token and password hashes are one-way already.
- **Outbox is at-least-once** — a flaky endpoint may see a duplicate on partial failure; losing an event
  is the thing we avoid. Dead events are inspectable/replayable by global admins.
