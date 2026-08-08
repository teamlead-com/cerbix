# Requirement Status

Statuses: `TODO`, `IN_PROGRESS`, `DONE`. Every `DONE` links to code, tests, and metrics.

## Functional Requirements

| ID | Requirement | Status | Evidence |
| --- | --- | --- | --- |
| FR-001 | Provide CLI `cerbix serve --role all\|api\|scheduler\|worker` and `cerbix version`. | DONE | [`backend/internal/cli/cli.go`](../backend/internal/cli/cli.go), [`cli_test.go`](../backend/internal/cli/cli_test.go) |
| FR-002 | Serve operational endpoints `/healthz`, `/readyz`, `/metrics`. | DONE | [`backend/internal/httpsrv/server.go`](../backend/internal/httpsrv/server.go), [`server_test.go`](../backend/internal/httpsrv/server_test.go) |
| FR-003 | Load and strictly validate a YAML config (reject unknown keys, fail fast). | DONE | [`backend/internal/config/config.go`](../backend/internal/config/config.go), [`config_test.go`](../backend/internal/config/config_test.go), [`deploy/config.example.yaml`](deploy/config.example.yaml) |
| FR-004 | OIDC login (any OpenID Connect issuer — Keycloak/Auth0/Okta/Google/Entra ID) + JIT user provisioning + sessions. | DONE | Provider-agnostic OIDC via discovery (`oidc.issuer`), Authorization Code + PKCE, JIT provision keyed by the OIDC identity `(issuer, subject)` — `UpsertUserByOIDCIdentity`, migration 00056 (D-0143); a legacy `oidc_sub`-only row adopts the current issuer once on next login; public `GET /auth/config` reports `{local, oidc, oidc_button_label}` so the SPA renders a provider-neutral sign-in button (D-0045). [`internal/auth/*`](../backend/internal/auth/), [`handlers.go`](../backend/internal/auth/handlers.go) (`ConfigHandler`), [`internal/store/sessions.go`](../backend/internal/store/sessions.go), [`authflows.go`](../backend/internal/store/authflows.go), mock-OIDC tests [`auth_test.go`](../backend/internal/auth/auth_test.go), [`config_handler_test.go`](../backend/internal/auth/config_handler_test.go) |
| FR-005 | Organization/Project model, membership, and role assignment. | DONE | [`internal/domain/domain.go`](../backend/internal/domain/domain.go), [`internal/store/*`](../backend/internal/store/), API [`internal/api/*`](../backend/internal/api/) |
| FR-006 | Tenant isolation: reads/writes constrained to authorized org/project. | DONE | [`internal/authz/authz.go`](../backend/internal/authz/authz.go), [`internal/api/handlers.go`](../backend/internal/api/handlers.go), tests [`authz_test.go`](../backend/internal/authz/authz_test.go), [`api_test.go`](../backend/internal/api/api_test.go), [`store_integration_test.go`](../backend/internal/store/store_integration_test.go) |
| FR-014 | Embedded schema migrations + `cerbix migrate` + auto-migrate on serve. | DONE | [`internal/store/migrate.go`](../backend/internal/store/migrate.go), [`migrations/00001_init.sql`](../backend/internal/store/migrations/00001_init.sql), [`internal/cli/cli.go`](../backend/internal/cli/cli.go) |
| FR-015 | Default local login (username/password) alongside OIDC, same roles + sessions; bootstrap admin. | DONE | [`internal/auth/{password,local}.go`](../backend/internal/auth/), [`internal/store/users_local.go`](../backend/internal/store/users_local.go), [`migrations/00004_local_users.sql`](../backend/internal/store/migrations/00004_local_users.sql), API `POST /api/v1/me/password` |
| FR-007 | Monitor CRUD + HTTP/TCP probers with condition engine (ICMP/Push later). | DONE | [`internal/domain/monitor.go`](../backend/internal/domain/monitor.go), [`internal/prober/*`](../backend/internal/prober/), [`internal/store/monitors.go`](../backend/internal/store/monitors.go), API [`internal/api/handlers_monitors.go`](../backend/internal/api/handlers_monitors.go) |
| FR-008 | Scheduler (advisory-lock leader) + Dispatcher (inproc; RabbitMQ later) + worker pool + ingestion. | DONE | [`internal/scheduler`](../backend/internal/scheduler/), [`internal/dispatch`](../backend/internal/dispatch/), [`internal/worker`](../backend/internal/worker/), [`internal/ingest`](../backend/internal/ingest/). **Efficiency (iter-0031, D-0038):** the leader schedules from an in-memory monitor snapshot refreshed every 15 s (no per-second table scan) and detects push-timeouts via one batched `StalePushMonitors` query (no per-monitor N+1). |
| FR-009 | Heartbeat storage + SLA/SLI over rolling windows + maintenance windows. | DONE | Rolling 24h/7d/30d/90d uptime%, avg **and p95** latency (`percentile_cont(0.95)` on `up` rows, maintenance-excluded — D-0046), SLO error budget. [`internal/sla/sla.go`](../backend/internal/sla/sla.go), [`internal/store/sla.go`](../backend/internal/store/sla.go), [`migrations/00005_sla.sql`](../backend/internal/store/migrations/00005_sla.sql), API [`internal/api/handlers_sla.go`](../backend/internal/api/handlers_sla.go) |
| FR-010 | Notifications (Telegram/Slack/Email/webhook) on status change. | DONE | Per-project notification channels (**webhook/slack/telegram/email**) linked to monitors; on a monitor's down/recover transition the ingest pipeline sends a formatted message to each linked enabled channel via an async best-effort dispatcher ([`internal/notify`](../backend/internal/notify/)). Webhook/Slack/Telegram POST; email via `net/smtp`. All four verified e2e (HTTP receiver + mailpit). **Reliability (iter-0029, D-0036):** transitions are enqueued to a transactional outbox in the same tx as the status change and delivered by a retry/backoff worker ([`internal/outbox`](../backend/internal/outbox/)), so notifications survive restarts and transient failures. |
| FR-011 | Vue SPA (dashboards, monitor detail, members, incidents), served via Docker. | DONE | Full designed surface wired to the API and e2e-verified through the frontend nginx proxy, all built/run in Docker (D-0019): typed `openapi-fetch` client, Pinia `session`+`workspace`+`ui` stores, auth guard + org/project switcher; **Login (provider-neutral, `/auth/config`-driven — D-0045), Dashboard, Monitors, New-monitor, Monitor-detail(+pause/delete), Members, SLA & SLO, Incidents (list/open/detail+timeline+update+resolve+postmortem), Status pages (mgmt + components), the public `/status/:slug` render + RSS/Atom/JSON feeds, a consolidated Settings page (channels/tokens/webhooks) and a self-service org/project create dialog (D-0048), plus a topbar global search box (D-0047)** — every view rebuilt 1:1 against the approved design artifacts with bespoke inline-SVG charts (D-0049) ([`frontend/src/`](../frontend/src/)). **Live SSE (D-0056):** the dashboard and monitor-detail update status in realtime from `GET /api/v1/events`. **Per-control RBAC hiding (D-0057):** action controls (New/Edit/Delete/Invite/Declare/Settings mutations) hide or disable by the caller's effective role, so viewers/editors don't see controls that would 403. The designed SPA surface is complete. |
| FR-012 | Status pages + incidents/postmortems + subscribers/webhooks/feeds. | DONE | **Phase 1 (iter-0011): incidents core** — domain (status `investigating→identified→monitoring→resolved`, impact, source; resolved-terminal; postmortem-requires-resolved), migration `00006`, store (tx: create-with-opening-update, add-update syncs status/resolved_at), REST (`/projects/{id}/incidents`, `/incidents/{id}[/updates][/postmortem]`, editor+ writes, viewer reads, tenant-isolated) — postable via GUI **and** API; `cerbix_incidents_opened_total`. **Phase 2a (iter-0012): status pages + components + rendering** — org-level (opt. project) pages with visibility public/internal/unlisted; components tied to a monitor (status derived) or manual; public UNAUTHENTICATED render (`/api/v1/public/status-pages/{slug}`: public served, unlisted needs `?token=`, internal hidden 404) + authed member render assembling summary + per-component status + 90d uptime + active incidents. **Phase 2b-i (iter-0013): API tokens (service accounts)** — org/project-scoped tokens with a role, hashed at rest, issued/listed/revoked by org admins; `Authorization: Bearer` auth builds a scoped principal so machines post incidents/postmortems per the token's role (`source=api`), tenant-isolated. **Phase 2b-ii (iter-0014): auto-incident** — the ingest pipeline opens an incident (`source=auto`, linked to the monitor) when a monitor transitions to down, and auto-resolves it on recovery; no duplicate while one is open; counted in `cerbix_incidents_opened_total`. **Phase 2b-iii (iter-0015): status-page incident feeds** — JSON Feed v1 + RSS 2.0 + Atom of a page's incidents, served both authed (member) and public (same visibility gate as render: public served, unlisted needs `?token=`, internal hidden), `?format=rss|atom|json`. **Phase 2b-iv (iter-0016): outbound webhooks** — org/project-scoped webhook subscriptions; on every incident lifecycle event (opened/updated/resolved, from both the API handlers and the auto-incident pipeline) a signed (HMAC-SHA256 `X-Cerbix-Signature`) JSON POST is delivered best-effort by an async dispatcher. **Phase 2b-v (iter-0027): Keycloak client-credentials** — machine OAuth2 clients now authenticate via a second bearer path (issuer-trusted JWT, membership-gated), completing FR-013 (D-0034); the incidents/status Vue views were wired in FR-011. **Phase 2b-vi (iter-0029, D-0036): reliable delivery** — incident webhooks are enqueued to a transactional outbox in the same tx as the incident create/update and delivered by a retry/backoff worker (`FOR UPDATE SKIP LOCKED`, multi-replica-safe, dead-letter after max attempts), replacing the best-effort in-memory queue. **Phase 2b-vii (iter-0032, D-0039): dead-letter ops** — global-admin endpoints list and replay dead outbox events (`GET/POST /api/v1/admin/outbox/dead[...]`), so events that exhausted retries are inspectable and recoverable ([`internal/api/handlers_outbox.go`](../backend/internal/api/handlers_outbox.go)). **Closed (iter-0035):** every status-page/incident view rebuilt 1:1 with the design artifacts (management + public render, timelines, postmortems); status pages are now editable and deletable from the UI (`PATCH`/`DELETE /status-pages/{id}`, org-admin, components cascade) and manual incidents can bind an optional affected monitor (D-0050). Backend follow-ups (public per-day availability buckets, public maintenance/incident-history, email subscribers, multi-component incident impact) are tracked as API-gap backlog, not FR-012 scope. |
| FR-013 | API tokens (service accounts) + OIDC client-credentials for API. | DONE | **Service-account tokens** (iter-0013): org/project-scoped, hashed at rest, `Authorization: Bearer cbx_…` → scoped principal ([`internal/store/apitokens.go`](../backend/internal/store/apitokens.go), [`internal/api/handlers_apitokens.go`](../backend/internal/api/handlers_apitokens.go)). **OIDC client-credentials** (iter-0027, D-0034; provider-agnostic per D-0043): a machine OAuth2 client's JWT is verified issuer+signature (audience relaxed, `ccVerifier`), JIT-provisions a user by `sub`, and resolves to a **membership-gated** principal ([`internal/auth/{auth,middleware}.go`](../backend/internal/auth/), [`clientcreds_test.go`](../backend/internal/auth/clientcreds_test.go)). Both converge on the same `authz.Can`. |
| FR-016 | Global tenant-scoped search across monitors, projects, and incidents. | DONE | `GET /api/v1/search` (min 2 chars) runs escaped-`ILIKE` lookups and filters every hit through `Principal.VisibleProject`, so users only see hits in orgs/projects they belong to; surfaced by a debounced topbar search box (D-0047). [`internal/store/search.go`](../backend/internal/store/search.go), [`internal/api/handlers_search.go`](../backend/internal/api/handlers_search.go), [`api_search_test.go`](../backend/internal/api/api_search_test.go), [`search_internal_test.go`](../backend/internal/store/search_internal_test.go), [`frontend/src/components/SearchBox.vue`](../frontend/src/components/SearchBox.vue) |
| FR-017 | Monitoring as Code: tenant-scoped file providers hot-reconcile versioned ProjectBundles into provider-owned monitors without service restart/reload; UI/API monitors coexist. | IN_PROGRESS | Contract-foundation slice (iter-0087): strict static `providers.file` config ([`internal/config/providers.go`](../internal/config/providers.go)) + pure ProjectBundle parser/canonicalizer/validator/planner ([`internal/fileprovider/`](../internal/fileprovider/)); no runtime DB apply yet. Ownership/persistence (iter-0088): migrations 00059 `file_provider_bundles` + 00060 `managed_monitors` (schema-enforced tenant safety via composite FKs; one owner per monitor); provenance model + atomic ownership guard ([`internal/store/managed.go`](../internal/store/managed.go)); typed write-path split (`updateMonitorTx` shared by API + future file apply); API `409 managed_by_file` ([`internal/api/handlers_monitors.go`](../internal/api/handlers_monitors.go)). Atomic bundle apply (iter-0089): `ApplyFileManagedBundle` ([`internal/store/fileapply.go`](../internal/store/fileapply.go)) — tenant resolve + `fileprovider.Plan` + create/update/dependency_update/noop/orphan/restore in ONE tx (monitors + deps + provenance + monotonic generation + `monitor_config_changed` NOTIFY, deterministic lock order, per-project atomic); D-0142 revision-safe (no-op/dep-only never bump), orphan-grace disable (never hard delete), restore reuses DB id + push token; canonical-hash/dep split per D-0146. Filesystem scan & grouping (iter-0090): bounded directory scan ([`internal/fileprovider/scan.go`](../internal/fileprovider/scan.go)) — eligible `.yaml`/`.yml` immediate children, symlink-escape rejection, `max_files`/`max_file_bytes`/`max_total_bytes` bounds; `GroupBundles` duplicate-project freeze + unbound→suspend-orphan (§9.1). Reconcile loop + HA + wiring (iter-0091): `internal/fileprovider/runtime` — per-provider advisory-lock leader (distinct key namespace, same-session liveness/stepdown), poll-watch with debounce + dirty-bit + mandatory resync + directory-replacement recovery, scan→group→`ApplyFileManagedBundle` with whole-project-disappearance orphan honoring `SuspendOrphan`; wired into `--role api`/`all` only ([`internal/cli/cli.go`](../internal/cli/cli.go) `startFileProviders`, fail-fast on missing DB/unreadable dir §4.1). Observability + provenance (iter-0092): `cerbix_file_provider_*` metrics ([`internal/metrics/metrics.go`](../internal/metrics/metrics.go)) wired into the reconcile loop; monitor responses carry the read-only `management` provenance block ([`internal/api/handlers_monitors.go`](../internal/api/handlers_monitors.go)); `orphan_grace_period: 0` now honored as immediate (spec §4.1 pointer fix); runbook HA-invariant + stale-success alert. **No-restart real-binary smoke** (`e2e/mac-smoke.sh`): live create → orphan-disable with the process never restarting (update/LKG/restore/id+token/scheduler-exec were checked interactively but are NOT in the committed script — pending iter-0095 §4). Diagnostics + failover (iter-0093): tenant-aware provider diagnostics API (`GET /api/v1/admin/file-providers` global-admin; `GET /api/v1/organizations/{orgID}/file-providers` org-admin) [`internal/api/handlers_fileproviders.go`](../internal/api/handlers_fileproviders.go); explicit 2-replica leadership-failover race test on real Postgres. UI (iter-0094): source filter + `Managed by file` badge + read-only controls; OpenAPI `management`/diagnostics + regen. Orphan-safety P0 fix (iter-0095): duplicate-project freeze + scan-error provider-wide suspend so invalid/rejected input NEVER orphans LKG (§9.1); TOCTOU byte re-check; `max_monitors_per_bundle` enforced. **Reverted to IN_PROGRESS after review — remaining before DONE:** (P0) leadership fencing of the apply tx (§17); (P0) `max_managed_monitors` + YAML depth/tag policy; (P1) transactional audit in apply (§9); (P1) persisted+runtime diagnostics (status/last_error/attempted_at/leadership/last-scan/empty-providers, §15) + repeated-error rate-limit; (P1) tests: event-storm, leadership-loss-during-apply, two-process failover, full real-binary update/LKG/restore/id+token/scheduler-exec. iter-0094's '§19 all met' / dirty-bit-loop-test claims were overstated — corrected in iter-0095. Spec [`func-monitoring-as-code.md`](specs/func-monitoring-as-code.md), D-0145/D-0146/D-0147. |

## Non-Functional Requirements

| ID | Requirement | Status | Evidence |
| --- | --- | --- | --- |
| NFR-001 | Config is fail-fast and strictly validated before runtime. | DONE | [`backend/internal/config/config.go`](../backend/internal/config/config.go), [`config_test.go`](../backend/internal/config/config_test.go) |
| NFR-002 | Runtime does not self-heal, downgrade, or silently ignore invalid config. | DONE | [`backend/internal/cli/cli.go`](../backend/internal/cli/cli.go), [`docs/decisions.md`](decisions.md) |
| NFR-003 | Structured logging via slog; level/format from config; no secret logging. | DONE | [`backend/internal/logging/logger.go`](../backend/internal/logging/logger.go), [`logger_test.go`](../backend/internal/logging/logger_test.go), [`.golangci.yml`](../backend/.golangci.yml) |
| NFR-004 | Metrics use `cerbix_` prefix and low-cardinality labels. | DONE | [`backend/internal/metrics/metrics.go`](../backend/internal/metrics/metrics.go), [`metrics_test.go`](../backend/internal/metrics/metrics_test.go) |
| NFR-005 | Graceful shutdown on SIGINT/SIGTERM. | DONE | [`backend/internal/cli/cli.go`](../backend/internal/cli/cli.go) |
| NFR-006 | Tenant isolation enforced (domain + HTTP) + regression-tested. | DONE | schema FK + `authz` + API handlers; tests in `authz_test.go`, `api_test.go`, `store_integration_test.go` |
| NFR-007 | API tokens stored as hashes only; never logged. | DONE | Service-account tokens are stored only as a SHA-256 hash (`HashToken`) and matched by hash on presentation; the plaintext `cbx_…` is shown once at issuance and never persisted or logged ([`internal/store/apitokens.go`](../backend/internal/store/apitokens.go), [`internal/auth/middleware.go`](../backend/internal/auth/middleware.go) `principalFromToken`). Client-credentials JWTs are bearer tokens verified but never stored. |
| NFR-008 | CI runs lint + `go test -race` + coverage gate on MR/main. | DONE | [`.gitlab-ci.yml`](../.gitlab-ci.yml), [`.ci/.gitlab-ci-backend.yml`](../.ci/.gitlab-ci-backend.yml) |
| NFR-009 | DB-gated readiness + `cerbix_database_up`; fail-fast on DB init error. | DONE | [`internal/cli/cli.go`](../backend/internal/cli/cli.go) (`pingDatabase`), [`internal/metrics/metrics.go`](../backend/internal/metrics/metrics.go) |
| NFR-010 | Local-login passwords hashed (argon2id), never logged, rate-limited. | DONE | argon2id + uniform 401 + no-secret-logging ([`internal/auth/password.go`](../backend/internal/auth/password.go), [`local.go`](../backend/internal/auth/local.go)); **per-IP sliding-window rate limit** ([`internal/auth/ratelimit.go`](../backend/internal/auth/ratelimit.go), `login_rate_limit_per_minute` default 10, 429 over limit) — D-0031. **Proxy trust (D-0143):** `server.trusted_proxy_cidrs` makes the client-IP key honor `X-Forwarded-For` only when the direct peer is inside a trusted network (supersedes the hop-count `trusted_proxy_count` when set), closing the dual-path XFF-spoof. |
| NFR-011 | Probers guarded against SSRF: validate the resolved connect IP; block cloud-metadata/link-local by default. | DONE | `prober.Guard` validates the **resolved** connect IP via a custom `Transport.DialContext` (covers HTTP redirects + DNS rebinding by pinning the checked IP), shared by the TCP prober; ICMP checks the resolved IP. `prober.allow_private_ips` default **true** (internal tool), `allow_metadata_ips` default **false** (block `169.254.0.0/16` + `fe80::/10` + explicit IMDS incl. IPv6 `fd00:ec2::254`) ([`internal/prober/guard.go`](../backend/internal/prober/guard.go), [`guard_test.go`](../backend/internal/prober/guard_test.go), [`internal/config/config.go`](../backend/internal/config/config.go)) — D-0035, D-0042. |
| NFR-012 | Raw heartbeats bounded by retention; long-range availability preserved. | DONE | `heartbeats` is RANGE-partitioned by `ts` into daily partitions + a DEFAULT (migration `00017`); the scheduler leader pre-creates upcoming partitions and DROPs those older than `heartbeats.retention_days` (default 30) ([`internal/store/retention.go`](../backend/internal/store/retention.go), [`internal/scheduler/scheduler.go`](../backend/internal/scheduler/scheduler.go)). The same `retention_days` bounds the rollup recompute window so long-range availability (frozen `heartbeats_daily` rows) is never wiped — D-0037. |
| NFR-013 | Secrets encrypted at rest (webhook secrets, channel credentials). | DONE | `secret.Cipher` (AES-256-GCM, `enc:v1:` tag, legacy-plaintext passthrough) encrypts `webhooks.secret` and `notification_channels.config` values at the store boundary; `security.encryption_key` (base64 32B, empty = off) ([`internal/secret/secret.go`](../backend/internal/secret/secret.go), [`internal/store/{webhooks,notifications}.go`](../backend/internal/store/), [`internal/config/config.go`](../backend/internal/config/config.go)) — D-0040. **Key rotation (iter-0034, D-0041):** the cipher is a keyring (encrypt primary, decrypt try-all); `security.previous_keys` + the `cerbix reencrypt` command migrate data to a new key with zero downtime — D-0041. |
| NFR-014 | Monitoring as Code reconciliation is tenant-scoped, atomic per project bundle, HA-safe, bounded, no-op idempotent, last-known-good preserving, and never auto-deletes history or accepts inline secrets. | IN_PROGRESS | Core delivered (iter-0087…0095): strict/bounded parsing, scope matrix, inline-secret rejection, canonical no-op, per-project atomic apply, advisory leadership, D-0142 revision-safe updates, LKG on invalid, orphan/disable with no history loss, orphan-safety §9.1 fix. **Not yet complete:** true leadership fencing of the in-flight apply (§17), full bounded-work enforcement (`max_managed_monitors`, YAML depth/tag policy, apply-tx deadline, global reconcile concurrency), transactional audit, honest persisted/runtime diagnostics + error rate-limit, and the storm/failover/real-binary test matrix. Spec §§2, 7–18; D-0145/D-0146/D-0147. |

## Acceptance Criteria (iter-0001)

| ID | Criterion | Status |
| --- | --- | --- |
| AC-0001-1 | `go build ./...` and `go test ./...` pass in `backend/`. | DONE |
| AC-0001-2 | `cerbix serve` responds 200 on `/healthz`, `/readyz`; `/metrics` emits `cerbix_*`. | DONE |
| AC-0001-3 | `cerbix version` prints injected version/commit as JSON. | DONE |
| AC-0001-4 | Invalid/unknown config keys cause fail-fast (non-zero exit). | DONE |
| AC-0001-5 | `docs/` foundation + `AGENTS.md` (cerbix-adapted) present. | DONE |
| AC-0001-6 | `docker-compose` dev stack defined (pg+timescale, rabbitmq, keycloak, cerbix). | DONE |

## Acceptance Criteria (iter-0002)

| ID | Criterion | Status |
| --- | --- | --- |
| AC-0002-1 | Migrations create organizations/projects/users/memberships with FK + role/scope constraints. | DONE |
| AC-0002-2 | `cerbix migrate --config` applies migrations; fails fast without a DSN or on unreachable DB. | DONE |
| AC-0002-3 | `serve` with a DSN migrates, connects, and gates readiness on live DB (`/readyz` 503 + `cerbix_database_up 0` when DB down). | DONE |
| AC-0002-4 | `serve` without a DSN keeps scaffold mode (ready, no `cerbix_database_up`). | DONE |
| AC-0002-5 | Tenant-isolation queries verified: org-level vs project-level members see only permitted orgs/projects. | DONE |
| AC-0002-6 | Cross-org project membership rejected by composite FK. | DONE |
| AC-0002-7 | Coverage ≥ 70% with the DB service; store integration tests run in CI, skip locally without DSN. | DONE |

## Acceptance Criteria (iter-0003)

| ID | Criterion | Status |
| --- | --- | --- |
| AC-0003-1 | OIDC login (`/auth/login`) starts Authorization Code + PKCE with state/nonce stored in `auth_flows`. | DONE |
| AC-0003-2 | `/auth/callback` verifies the ID token (sig/iss/aud/nonce), JIT-provisions the user, creates a session, sets the cookie. | DONE |
| AC-0003-3 | `admin_emails` promotes matching users to global admin on login. | DONE |
| AC-0003-4 | `RequireAuth` resolves the session cookie to a Principal or returns 401; `/auth/logout` revokes the session. | DONE |
| AC-0003-5 | authz matrix + `Can` unit-tested; global-admin bypass; scope rules enforced. | DONE |
| AC-0003-6 | API enforces isolation: non-members get 404 (hidden), insufficient in-org role gets 403; list endpoints filter by visibility. | DONE |
| AC-0003-7 | OIDC flow verified hermetically via a mock provider; store sessions/auth-flows integration-tested. | DONE |
| AC-0003-8 | Coverage ≥ 70% with the DB service; default `go test ./...` stays hermetic. | DONE (74.7%) |

## Acceptance Criteria (iter-0004)

| ID | Criterion | Status |
| --- | --- | --- |
| AC-0004-1 | Monitor CRUD API with authz (ProjectRead/Write) and cross-project isolation. | DONE |
| AC-0004-2 | HTTP + TCP probers with timeout/retries; declarative condition engine. | DONE |
| AC-0004-3 | Scheduler elects a single leader (advisory lock) and publishes due jobs; passive monitors skipped. | DONE |
| AC-0004-4 | Worker pool executes jobs; ingestion writes heartbeats + updates status + `cerbix_checks_total`. | DONE |
| AC-0004-5 | End-to-end: `serve --role=all` drives a real monitor to `up` with heartbeats recorded. | DONE |
| AC-0004-6 | Coverage ≥ 70% with the DB service; default `go test ./...` hermetic (mock-OIDC + inproc, DB tests skip). | DONE (75.7%) |

## Acceptance Criteria (iter-0005)

| ID | Criterion | Status |
| --- | --- | --- |
| AC-0005-1 | `users.oidc_sub` nullable + `password_hash`; OIDC and local users coexist. | DONE |
| AC-0005-2 | argon2id hashing with random salt + constant-time verify; salted (distinct hashes). | DONE |
| AC-0005-3 | `POST /auth/local/login` issues a session on valid credentials; uniform 401 otherwise. | DONE |
| AC-0005-4 | Bootstrap creates a global admin on an empty system from config; never generates/logs a password. | DONE |
| AC-0005-5 | `POST /api/v1/me/password` changes a local user's password (verifies current, enforces min length). | DONE |
| AC-0005-6 | `auth.New` builds OIDC only when configured; cerbix runs local-only (no OIDC issuer). | DONE |
| AC-0005-7 | Real-binary e2e: bootstrap admin → local login → authenticated `/api/v1/me`. | DONE |
| AC-0005-8 | Coverage ≥ 70% with the DB service; default `go test ./...` hermetic. | DONE (75.7%) |

## Acceptance Criteria (iter-0006)

| ID | Criterion | Status |
| --- | --- | --- |
| AC-0006-1 | SLI (uptime%) computed over rolling 24h/7d/30d/90d windows per monitor and per project. | DONE |
| AC-0006-2 | SLO targets (per monitor+window) with error budget (allowed/actual/remaining/burned/met). | DONE |
| AC-0006-3 | Maintenance windows (monitor- or project-scoped) exclude heartbeats from SLI math. | DONE |
| AC-0006-4 | SLA/target/maintenance endpoints enforce authz + tenant isolation. | DONE |
| AC-0006-5 | Real-binary e2e: heartbeats → SLA windows → SLO error budget → maintenance exclusion drops totals to 0. | DONE |
| AC-0006-6 | Coverage ≥ 70% with the DB service; default `go test ./...` hermetic. | DONE (76.0%) |

## Acceptance Criteria (iter-0007)

| ID | Criterion | Status |
| --- | --- | --- |
| AC-0007-1 | `openapi.yaml` (OpenAPI 3.0.3) documents the current API (paths + schemas) and parses. | DONE |
| AC-0007-2 | Embedded SPA served from `embed.FS`; real assets served, unknown routes fall back to index.html. | DONE |
| AC-0007-3 | Route precedence: `/healthz|/readyz|/metrics` and `/api`, `/auth` win over the SPA catch-all. | DONE |
| AC-0007-4 | Real-binary: `GET /` serves the shell, `/orgs/x` falls back, `/api/v1/me` still 401, `/healthz` ok. | DONE |
| AC-0007-5 | Default `go test ./...` stays hermetic; build embeds the placeholder dist. | DONE |

## Acceptance Criteria (iter-0035)

| ID | Criterion | Status |
| --- | --- | --- |
| AC-0035-1 | OIDC is provider-agnostic (any issuer via discovery); no Keycloak-specific code paths. Config/comments name Keycloak only as one example (D-0043). | DONE |
| AC-0035-2 | User identity column is `oidc_sub` (original migrations rewritten, no rename migration — D-0044); no `keycloak_sub` remains in code/migrations/API. | DONE |
| AC-0035-3 | Public `GET /auth/config` returns `{local, oidc, oidc_button_label}`; the SPA renders a provider-neutral sign-in button labeled by `oidc.button_label` (default "Continue with SSO") — D-0045. | DONE |
| AC-0035-4 | Monitor & project SLA expose `p95_latency_ms` (`percentile_cont(0.95)`, maintenance-excluded) alongside avg — D-0046. | DONE |
| AC-0035-5 | `GET /api/v1/search` returns only hits the caller can see (`VisibleProject`); <2-char queries return empty; LIKE wildcards escaped — D-0047. | DONE |
| AC-0035-6 | Settings page (channels/tokens/webhooks) and role-gated org/project create dialog are wired to existing APIs — D-0048. | DONE |
| AC-0035-7 | Every SPA view rebuilt 1:1 with the approved design artifacts; charts are bespoke inline SVG (no TanStack Query/uPlot) — D-0049. | DONE |
| AC-0035-8 | `go build ./...` + `go test ./internal/web` green; frontend type-checks and builds; embedded `dist` refreshed with the `placeholder.txt` fixture. | DONE |

## Definition of Done (per iteration)

| ID | Item | Status (iter-0001) |
| --- | --- | --- |
| DoD-1 | Target `status.md` items `DONE` with evidence links. | DONE |
| DoD-2 | Required tests green (`go test ./...`, `-race`). | DONE |
| DoD-3 | Docs synchronized (PRD/status/decisions/traceability/runbook/iteration). | DONE |
| DoD-4 | Config strictly validated before business logic. | DONE |
| DoD-5 | No self-healing/fallback for invalid config. | DONE |
| DoD-6 | Iteration report written. | DONE |

## iter-0036 consolidation (sync with code, D-0050…D-0086)

Delivered and `DONE` (details — `decisions.md`, tests — `traceability.md`, report — `iterations/iter-0036.md`):

| Area | Status | Main Ds / code |
| --- | --- | --- |
| Check catalog (15 types, SSRF guard, secret encryption) | DONE | `internal/prober/*` |
| Alert reliability: confirmations / maintenance-suppression / re-notify / tags | DONE | D-0076/0077/0078 |
| SLO error-budget + **burn-rate alerts** | DONE | D-0079 (`sla`, `store.EvaluateBurnAlerts`) |
| Scheduled weekly **SLA reports** | DONE | D-0080 (`store.EnqueueDueSLAReports`) |
| Incidents (auto/manual/API) + postmortems + feeds | DONE | status pages + `feed`/`subscribe` |
| Incoming **Alertmanager receiver** | DONE | D-0081 (`api/handlers_alertmanager.go`) |
| 2FA/TOTP + self-service password reset | DONE | D-0064 + reset |
| **OIDC from the UI** (live provider reconfig) | DONE | D-0082 (`auth`, `oidc_settings`) |
| **Instance settings** (branding/auth-policy/alerting/monitor-defaults) | DONE | D-0083 (`settings`, `instance_settings`) |
| **SMTP group** + live mailer | DONE | D-0084 |
| Secrets-at-rest AES-256-GCM + rotation; global silence | DONE | `secret`, D-0083 |
| **Geo probers** (region-aware pools, `--region`, per-region queues, TTL) | DONE | D-0085 (`dispatch/amqp`, migration 00033) |
| **Live region picker** (RabbitMQ mgmt) + **Test connection** | DONE | D-0086 (`mqadmin`, `api`) |
| **Region-aware Test connection** (RPC, strictly no core fallback) | DONE | D-0087 (`dispatch/amqp` RunTest/ServeTests) |
| **"Region without a worker" alert** (edge-triggered + grace 90s) | DONE | D-0088 (`store/regionalerts.go`, migration 00034) |
| **On-call / escalations** (policies+rotations+ack, phase A) | DONE | D-0089 (`domain/escalation.go`, `store/escalation.go`, migration 00035) |
| **Synthetic checks** (scripted multi-step HTTP, phase B) | DONE | D-0090 (`domain/synthetic.go`, `prober/synthetic.go`, migration 00037) |
| **HTTP pull agent** (geo without a broker, phase C) | DONE | D-0091 (`internal/agent`, `store/pulljobs.go`, `api/handlers_agent.go`, migration 00036) |
| Repo sanitization (module `git.example.com`, embedded SPA without nginx, CI) | DONE | D-0081..D-0086 period |

Period verification: full `-race` — 28 packages green; `vue-tsc`+`vite build`; OpenAPI↔`schema.d.ts`
in sync; the image is self-contained (the SPA is built and embedded); E2E geo-affinity/live-picker/test-connection/
region-RPC-test/region-worker-alert (fires after grace + recovery). **Deploy:** run `cerbix migrate`
as a one-off before starting the roles (roles auto-migrate and race for a new migration — see overview §2.2).

## iter-0085 result-protocol + hardening backlog (sync with code, D-0142…D-0144)

Delivered and `DONE` (contract — `specs/func-result-protocol.md`; details — `decisions.md` D-0143/D-0144;
tests — `traceability.md`; report — `iterations/iter-0085.md`). Clears the iter-0084 §5 deferred list.

| Area | Status | Main Ds / code |
| --- | --- | --- |
| **P0a** result timestamp hygiene + typed origins (scheduled/push/backfill) | DONE | D-0142/D-0143 (`RecordScheduledResult`/`RecordPushResult`/`RecordHistoricalResults`, `ingest.Reconciler`, migration 00054) |
| **P0b** `execution_revision` config-generation gate + typed dead-man | DONE | D-0142/D-0143 (`RecordDeadmanResult`, migration 00055, `result.revision_mode`) |
| **P1** readiness-gated push-token backfill + pull ACK/lease correctness | DONE | D-0143 (`store/pulljobs.go`, `internal/agent`) |
| **P2** resource caps / strict env-expand / TruncateAll guard / migrate lock / fail-closed RNG / public-DTO redaction / atomic reset / TOTP / outbox purge+CAS / dep-graph lock | DONE | D-0143 (#1/#3/#4/#5/#7/#8/#9/#10/#11/#13) |
| **#14** rate-limit `server.trusted_proxy_cidrs` (CIDR trust model) | DONE | D-0143 (`auth/ratelimit.go`, `config`) |
| **#12** OIDC identity keyed by `(issuer, subject)` | DONE | D-0143 (`UpsertUserByOIDCIdentity`, migration 00056) |
| **#2** outbox `state_sequence` delivery-staleness gate | DONE | D-0143 (`MonitorTransition.Seq`, migration 00057) |
| **Push-watermark** preserve liveness on edit + dead-man re-arm on re-enable | DONE | D-0144 (`monitors.push_armed_at`, migration 00058) |

Period verification: full `go test -race` — **30 packages** green in **both** storage modes (timescale +
throwaway `postgres:16-alpine`); migrations 00054–00058 apply on both; image rebuilt; **E2E 34/34** on the
live `single`+`sso` stack (goose → 58). `observe` remains a temporary migration mode with a hard removal
window (D-0142, ≤ iter-0089).
