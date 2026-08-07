# Architectural & Contract Decisions

Chronological log of decisions that affect contracts, reliability, or architecture.
New decisions append here; do not rewrite history.

## D-0001 — Monorepo (backend + frontend)

**Context:** cerbix ships a Go backend and a Vue SPA.
**Decision:** single repo `github.com/teamlead-com/cerbix` with `backend/` and
`frontend/`; the SPA is embedded into the binary via `embed.FS`.
**Consequence:** atomic API+UI changes, one release; CI jobs are path-scoped.

## D-0002 — Keycloak = authN only; authorization in cerbix DB

**Context:** deep Keycloak integration with per-project isolation and role-based access.
**Decision:** Keycloak authenticates (OIDC); membership and roles live in cerbix
(`memberships`), managed via the cerbix UI. Roles are not read from token claims.
**Consequence:** Familiar RBAC UX; onboarding without Keycloak edits. Optional group→role
sync deferred.

## D-0003 — Distributed from the start (RabbitMQ) behind a `Dispatcher`

**Context:** ~10 projects now, expected to grow significantly.
**Decision:** scheduler (leader-elected) → RabbitMQ → stateless workers, abstracted by a
`Dispatcher` interface with `rabbitmq` (prod) and `inproc` (dev) implementations.
**Consequence:** horizontal scale of workers; local dev/tests run without a broker.

## D-0004 — Scheduler leader election via Postgres advisory lock

**Context:** no etcd in the stack; need a single active scheduler.
**Decision:** use `pg_try_advisory_lock` for leadership.
**Consequence:** no new dependency; failover when the leader dies.

## D-0005 — Strict-only configuration

**Context:** company invariant (from reference AGENTS.md).
**Decision:** `internal/config` rejects unknown keys (`KnownFields(true)`) and validates
fail-fast; defaults only in the central loader for optional fields; runtime uses a
validated snapshot, no inline env reads. No self-healing/fallback.
**Consequence:** misconfiguration stops startup with a clear error.

## D-0006 — Go module rooted at `backend/`

**Context:** monorepo with mixed toolchains.
**Decision:** `go.mod` module path `github.com/teamlead-com/cerbix` lives in `backend/`;
imports are `github.com/teamlead-com/cerbix/internal/...`.
**Consequence:** clean import paths; frontend tooling stays out of the Go module.

## D-0007 — Status page default visibility is `internal`

**Context:** cerbix monitors internal apps; public pages risk leaking topology.
**Decision:** per-page visibility `internal` (default) | `public` | `unlisted`; `public`
must be an explicit opt-in and expose only configured information.
**Consequence:** safe-by-default; public exposure is a deliberate action.

## D-0008 — Hand-written pgx repositories instead of sqlc (for now)

**Context:** the plan named `pgx + sqlc`, but `sqlc` is not installed in the build
environment and its codegen would add an out-of-band step not runnable here or
guaranteed in CI.
**Decision:** implement the store as hand-written, typed pgx v5 repositories
(`internal/store`); defer sqlc. Queries stay explicit and reviewable; the build needs no
extra codegen tool.
**Consequence:** no generated code to commit/regenerate; SQL lives inline in Go. Revisit
sqlc later if query volume warrants it (would be an additive change behind the same
repository methods).

## D-0009 — goose as a library with embedded migrations

**Context:** no goose CLI in the environment; migrations must run reproducibly.
**Decision:** embed `internal/store/migrations/*.sql` via `embed.FS` and apply them with
`github.com/pressly/goose/v3` used as a library (`goose.UpContext`), through the pgx
`database/sql` stdlib driver. Exposed as `cerbix migrate --config` and run automatically
at `serve` startup when a DSN is configured.
**Consequence:** one migration path for dev, CI, and ops; no external tool required.

### Schema isolation constraints (part of D-0009)

`memberships` carries a composite FK `(project_id, org_id) → projects(id, org_id)` so a
project-scoped grant can only reference a project inside the named org (MATCH SIMPLE skips
the FK for org-level grants where `project_id IS NULL`). A `CHECK` mirrors
`domain.Role.ValidForScope`. These make tenant-scope integrity a schema invariant, not
only an application check.

## D-0010 — stdlib net/http routing instead of chi

**Context:** the plan named chi, but Go 1.22+ `http.ServeMux` supports method + path
patterns (`GET /api/v1/organizations/{orgID}`, `r.PathValue`).
**Decision:** use stdlib routing for the API; no chi dependency.
**Consequence:** fewer dependencies, closer to the company's stdlib-net/http convention.
Revisit only if routing needs (middleware groups, regex params) outgrow stdlib.

## D-0011 — Server-side sessions; hashed opaque tokens; single-use auth flows

**Context:** Keycloak is authN only; cerbix owns sessions (D-0002).
**Decision:** opaque random session tokens in an HttpOnly/SameSite=Lax cookie; only the
SHA-256 hash is stored (`sessions.token_hash`). OIDC login state/nonce/PKCE verifier live
in `auth_flows`, consumed atomically via `DELETE ... RETURNING` (single use), both with
short TTLs. OIDC uses Authorization Code + PKCE (S256) with nonce verification.
**Consequence:** stealing the DB does not reveal usable session tokens; replay of a login
callback is prevented; logout is server-side revocable.

## D-0012 — OIDC verified with a hermetic mock provider in tests

**Context:** exercising the callback needs an OIDC provider; a live Keycloak is heavy and
its realm/client naming is still open.
**Decision:** unit/integration tests stand up an in-process mock OIDC provider (discovery
+ JWKS + token, RS256-signed ID tokens) so the real login/callback/verify path is tested
without Keycloak. Live-Keycloak e2e remains opt-in (docker compose), per the AGENTS.md
live-dependency rule.
**Consequence:** auth logic is covered in default CI; no external dependency to run tests.

## D-0013 — Default local login alongside Keycloak

**Context:** OIDC-only auth means cerbix cannot be used or administered without Keycloak
(dev stands, bootstrap, isolated environments). Requested: a built-in login with the same
role model.
**Decision:** add an optional local authentication path — `users` gains a nullable
`keycloak_sub` and a nullable `password_hash` (argon2id); a user is OIDC-backed or local.
`POST /auth/local/login` issues the same server-side session; roles/authz are unchanged. A
bootstrap admin is created from config when local login is enabled and no users exist. OIDC
and local login can coexist; local login is config-gated.
**Consequence:** cerbix is usable without Keycloak. Scheduled for iter-0005 (spec now in
`sec-authn-authz.md`, FR-015/NFR-010). Requires a migration to relax `users.keycloak_sub`
to NULLABLE and add `password_hash`.

## D-0014 — Check jobs carry a monitor snapshot; inproc pipeline is single-process

**Context:** the scheduler publishes checks that workers execute (D-0003).
**Decision:** a `CheckJob` embeds a full `domain.Monitor` snapshot taken at publish time, so
workers need no database access and see a consistent config for the run. The `inproc`
Dispatcher (channels) is valid only within one process, so the full pipeline currently runs
under `--role=all`; splitting scheduler/worker into separate processes awaits the RabbitMQ
Dispatcher implementation (the seam already exists).
**Consequence:** simple, DB-free workers and a hermetic in-process pipeline for dev/tests;
horizontal split is an additive change behind `Dispatcher`.

## D-0015 — Scheduler leadership via a held advisory-lock connection

**Context:** exactly one scheduler must publish (avoid duplicate checks).
**Decision:** `store.TryBecomeLeader(key)` acquires a dedicated pooled connection and holds
it for the lock's lifetime (`pg_try_advisory_lock`), returning a `release()` that unlocks and
returns the connection. Non-leaders retry on an interval.
**Consequence:** session-scoped lock stays valid for the leader's lifetime; failover happens
when the leader's process (and connection) goes away. Verified by
`TestLeadershipMutualExclusion`.

## D-0016 — Local-login rate limiting deferred; bootstrap never logs a password

**Context:** NFR-010 asks for password hashing, no secret logging, and rate limiting on
local login.
**Decision:** iter-0005 ships argon2id hashing, constant-time verify, a uniform 401 (no
user enumeration), and no secret logging. **Rate limiting is deferred** to a later
iteration (belongs with a shared HTTP middleware / reverse-proxy policy). Bootstrap admin
creation requires an explicitly configured password and never auto-generates or logs one —
if no password is set, no admin is created.
**Consequence:** NFR-010 is partially satisfied (rate-limit outstanding, tracked in
`status.md`); no password value ever reaches the logs.

## D-0017 — SLI computed by direct SQL aggregation; TimescaleDB deferred

**Context:** the plan calls for raw heartbeats + TimescaleDB continuous-aggregate rollups
for cheap long-window SLA.
**Decision:** compute SLI with direct SQL aggregation over the `heartbeats` table
(`count`, `FILTER (WHERE up)`, `avg` + a `NOT EXISTS` maintenance-window exclusion). This
works on any Postgres, so tests and CI stay hermetic on `postgres:16-alpine`. Converting
`heartbeats` to a TimescaleDB hypertable with continuous aggregates (minute→hour→day) is a
**deferred optimization** for large volumes, not required for correctness; the dev
`docker-compose` already runs the TimescaleDB image for production use.
**Consequence:** correct SLA now with no extension dependency; a future migration can add
hypertable + CAGG rollups behind the same store methods.

## D-0018 — SPA embedded via embed.FS with index fallback; OpenAPI is the contract

**Context:** the Vue SPA must ship inside the single binary and coexist with the API.
**Decision:** `internal/web` embeds `dist/` via `embed.FS` and serves it as the catch-all
`/` route inside the app mux (after `/auth` and `/api`), returning real assets and falling
back to `index.html` for client-side routes. The build copies the Vue output
(`frontend/dist`) into `backend/internal/web/dist` before `go build`; a placeholder shell is
committed so the module always builds. `openapi.yaml` (repo root) is the source-of-truth API
contract, used to generate the TypeScript client in iter-0008.
**Consequence:** one artifact serves UI + API; the SPA is mounted only when auth is enabled
(it needs a session); operational and API routes keep precedence over the SPA.

## D-0019 — Frontend built and served in Docker (globex convention); nginx container

**Context:** the user requires the frontend to be built and run entirely in Docker (no local
Node), following the neighbouring `example/globex-frontend` project's structure.
**Decision:** `frontend/` is a Vite + Vue 3 + TS + Tailwind app with a globex-style Docker
layout — `docker/Dockerfile` (multi-stage: `node:22-alpine` build → `nginx-unprivileged`
serve `dist` on :8000), `docker/nginx/{nginx.conf,default.conf.template}`, plus
`docker-compose.yml` (deploy), `docker-compose.yml.dist` (local image), and
`docker-compose.dev.yml` (Vite hot-reload container). nginx serves the SPA and **proxies
`/api` and `/auth` to the cerbix backend** (`${BACKEND_URL}`). All Node tooling runs via the
`Makefile` in containers. This becomes the primary frontend delivery, relaxing D-0018: the
Go `embed.FS` serving from iter-0007 stays available as an optional single-binary mode but
is not the default path.
**Consequence:** no host Node; frontend and backend are separate deployables (like globex).
Build-time npm needs `--network=host` on hosts whose Docker build sandbox lacks DNS (baked
into the Makefile and documented); base-image pulls and `docker run` are unaffected.
Design tokens live in `src/style.css` + `tailwind.config.js`, derived 1:1 from
`docs/design/notes.md`.

## D-0020 — Incidents core reuses project-write authz; status is server-owned; postmortem gated on resolved

**Context:** FR-012 is large (incidents, status pages, components, subscribers, feeds,
webhooks, api-tokens). iter-0011 lands **phase 1: incidents + timeline updates + postmortems**
(manual + API), deferring the rest to iter-0012.
**Decision:**
- **Authorization reuses existing project actions** — reads require `ActionProjectRead`
  (viewer+), all writes (create incident, add update, publish postmortem) require
  `ActionProjectWrite` (editor+). No new authz actions; the plan's "an Editor-level caller
  may post incidents" falls out directly. Tenant isolation via the same `incidentAccess`→
  `projectAccess` path as monitors (non-members get 404, in-org insufficient role 403).
- **Lifecycle is forward-flowing; Resolved is terminal.** An incident carries a status
  (`investigating→identified→monitoring→resolved`); each timeline update sets the incident's
  status. Once resolved, further updates are rejected (400). `resolved_at` is stamped in SQL
  the first time an incident reaches Resolved (`AddIncidentUpdate` runs insert + status sync
  in one transaction). `CreateIncident` writes the incident and its opening update in one
  transaction, so every incident has a timeline from t0.
- **Source is server-determined**, not client-supplied: a cookie session yields
  `source=manual`. `api` (service-account tokens) and `auto` (monitor-down) sources arrive
  with phase 2, so the enum exists now but only `manual` is produced.
- **A postmortem requires a resolved incident** (400 otherwise); it is an upsert (one per
  incident), publishable identically via GUI and direct API `PUT`.
- **Observability:** `cerbix_incidents_opened_total`, wired via an optional, nil-safe
  `api.Metrics` recorder (`Handler.WithMetrics`) so hermetic tests and embed-only mode omit it.
**Consequence:** incidents are postable via GUI and API today. Status pages/components,
subscribers/webhooks/feeds, api-tokens, and auto-incident-from-monitor are explicitly
phase 2 (iter-0012). Migration `00006_incidents.sql` adds `incidents`, `incident_updates`,
`postmortems` with CHECK-constrained enums; `TruncateAll` includes them.

## D-0021 — Status pages: org-scoped, visibility-gated; public render is a separate unauthenticated route; component status derived from monitors

**Context:** FR-012 phase 2a — status pages + components + rendering. A status page's value is
its public read view, which must work without a session for `public`/`unlisted` pages, while
all existing `/api/v1/*` sit behind auth middleware.
**Decision:**
- **Status pages are organization-scoped** (`status_pages.org_id`, optional `project_id` for a
  narrower page), matching the one-page-per-product model (projects as
  components). Management authz reuses org actions: create/manage components require
  `ActionOrgManage` (org admin); reading config/render requires org membership (`InOrg`).
- **Rendering is split by trust boundary.** An **authenticated** `GET /status-pages/{id}/render`
  serves members at any visibility. A **separate unauthenticated** `GET /api/v1/public/status-pages/{slug}`
  is mounted via `Handler.PublicRouter()` on the app mux **outside** `RequireAuth`; ServeMux
  longest-match makes its more-specific prefix win over the auth-gated `/api/`. It enforces
  visibility itself: `public` served to anyone, `unlisted` requires the matching `?token=`
  (128-bit hex, generated on create), `internal` returns 404 (existence hidden). No optional-auth
  plumbing in the public path.
- **Component status is derived, not stored duplicative.** A component bound to a monitor
  derives its status from the monitor's last state (`ComponentStatusFromMonitor`) and its 90-day
  uptime from the existing SLA aggregation; a `manual_status` (when set) overrides. The page
  summary is the worst-of component severities (maintenance ranks above operational but below any
  outage). Active incidents on the render are the unresolved incidents of the projects the
  components draw from (`ListOpenIncidentsByProject`). Render tolerates a monitor deleted out from
  under a component (FK `ON DELETE SET NULL`) but propagates real DB errors (no self-healing).
- A monitor-backed component must reference a monitor **within the page's org** (`monitorInOrg`
  guard, 400 otherwise), preventing cross-tenant leakage onto a page.
**Consequence:** public status pages are live with derived component health, 90d uptime, and
active incidents, at three visibilities. Migration `00007_status_pages.sql` (status_pages +
components, CHECK-constrained visibility/manual_status, unique slug, cascade FKs) is additive;
`TruncateAll` includes them. Deferred to phase 2b (iter-0013): subscribers + outbound webhooks +
RSS/Atom/JSON feeds, `api_tokens` + Keycloak client-credentials for API posting, auto-incident on
monitor-down, then wiring the incidents & status-page Vue views.

## D-0022 — Service-account API tokens: hashed at rest, bearer auth builds a scoped principal, authorization stays role-driven

**Context:** FR-012's API-first requirement — machines (CI, bots) must post incidents/updates/
postmortems without a Keycloak session, scoped and revocable.
**Decision:**
- **Tokens are org-scoped (optionally project-scoped) with a role**, validated at their scope via
  the same `Role.ValidForScope` used for memberships (org token → org_admin/editor/viewer; project
  token → project_admin/editor/viewer). Issued/listed/revoked by org admins only.
- **Only a hash is stored.** Secrets are `cbx_` + 24 random bytes (hex); the store keeps the hex
  SHA-256 (reusing `store.HashToken`, shared with session tokens — both are high-entropy secrets,
  not passwords, so a fast hash is correct, not a KDF). The plaintext is returned exactly once, in
  the create response.
- **Bearer auth is a second path in the same `RequireAuth` middleware.** After the session cookie,
  an `Authorization: Bearer <token>` is hashed, looked up, and turned into a **synthetic
  `authz.Principal`** carrying one membership `{org, project?, role}`, `UserID = "apitoken:<id>"`,
  and a new `ViaToken` flag. Authorization is unchanged — the same `Can(...)` role matrix and tenant
  isolation apply — so a viewer token gets 403 on writes and a token can only act within its
  org/project. `last_used_at` is stamped best-effort (a touch failure never denies a valid call).
- **`ViaToken` drives attribution, not authorization:** an incident opened by a token gets
  `source = api` (vs `manual` for a session); nothing else branches on it.
**Consequence:** incidents/updates/postmortems (and any role-appropriate write) are fully
API-drivable with issued tokens, tenant-isolated and revocable. Migration `00008_api_tokens.sql`
(hash unique, role CHECK, cascade FKs) is additive; `TruncateAll` includes it. The Keycloak
client-credentials alternative (the plan's second machine-auth option) remains for a later
iteration; the token path covers the CI/bot use case now.

## D-0023 — Auto-incidents from monitor transitions (pipeline-driven, deduped, monitor-linked)

**Context:** FR-012 — the watchdog should open an incident on its own when a monitor goes down and
close it on recovery, without a human or an API caller.
**Decision:**
- **The ingest pipeline owns auto-incidents.** `SetMonitorStatus` now returns the *previous* status
  (via a CTE), so the consumer detects up/down transitions per heartbeat. On `!down → down` it opens
  an incident; on `down → up` it resolves the open one. A pending→up first check opens nothing.
- **Incidents link to their monitor** (`incidents.monitor_id`, `ON DELETE SET NULL`). Manual/API
  incidents leave it null. `FindOpenAutoIncidentByMonitor` (indexed on `monitor_id WHERE source='auto'
  AND status<>'resolved'`) makes the open-incident lookup cheap.
- **Dedup while open:** open is a no-op if an auto-incident is already active for the monitor, so a
  monitor that stays down produces exactly one incident, not one per failed check.
- **Auto-incidents are `source=auto`, impact major, authored `"auto"`**, titled `"<monitor> is down"`,
  with the failing check's message as the opening timeline entry; recovery adds a `resolved` update.
- **Best-effort, never fatal to ingestion:** every auto-incident step logs and returns on error; a
  nil recorder (tests) and a monitor lookup failure don't stop heartbeat processing. Auto-opens
  increment `cerbix_incidents_opened_total` alongside API-opened ones (the ingest `Recorder` gained
  `RecordIncidentOpened`, satisfied by the metrics registry).
**Consequence:** monitors now self-report outages as incidents that appear on status pages (active
incidents) and resolve automatically. Migration `00009_incident_monitor.sql` is additive/reversible.
Auto-resolve only closes incidents it owns (`source=auto`); manually/API-opened incidents are never
touched by the pipeline.

## D-0024 — Status-page incident feeds: three formats from one model, stdlib-only, visibility-gated

**Context:** FR-012 calls for RSS/Atom/JSON feeds on status pages (standard syndication points).
**Decision:**
- **One `internal/feed` model, three serializers.** A `feed.Feed`/`feed.Item` model renders to
  JSON Feed v1, RSS 2.0, and Atom, using only `encoding/xml` + `encoding/json` — no third-party
  feed library (the plan floated `gorilla/feeds`; hand-rolling keeps the dependency set minimal and
  the package hermetically testable). Each serializer returns `(bytes, content-type)`; RSS uses
  RFC1123Z dates, Atom/JSON use RFC3339; zero times render empty.
- **Two endpoints reuse the status-page trust boundaries.** An authenticated
  `GET /status-pages/{id}/feed` serves members; an unauthenticated
  `GET /api/v1/public/status-pages/{slug}/feed` (on the same `PublicRouter` mounted outside
  `RequireAuth`) applies the identical visibility gate as the public render — public served,
  unlisted requires `?token=`, internal returns 404. Format via `?format=rss|atom|json` (default RSS).
- **Feed items are the page's incidents.** The handler gathers incidents from the projects the
  page draws from (its own `project_id` plus the projects of its monitor-backed components — the
  same set logic the render uses, factored into `statusPageProjectIDs`), newest first, capped at 20.
  Each item summarizes `status · impact`; links are `#<incident-id>` anchors on the page URL, whose
  base is reconstructed from the request (`X-Forwarded-Proto` + Host) so no public-base-URL config
  is required yet.
**Consequence:** status pages expose standard RSS/Atom/JSON incident feeds, honoring visibility.
Read-only, no outbound HTTP, no new dependencies or schema. Outbound webhooks/subscribers and
Keycloak client-credentials remain for a later iteration.

## D-0025 — Outbound webhooks: async best-effort delivery, HMAC-signed, fired from one incident-event sink

**Context:** FR-012 — external systems should be notified when incidents change, driven by both
API-posted and auto-opened incidents.
**Decision:**
- **One event, one sink, two producers.** A `domain.IncidentEvent` (`incident.opened|updated|
  resolved` + the incident + the causing update) is emitted through a nil-safe `IncidentSink`
  interface that both the API handlers (`Handler.WithIncidentSink`) and the auto-incident pipeline
  (`Consumer.SetIncidentSink`) call. There is a single event shape and a single delivery path,
  regardless of whether a human, an API token, or the monitor pipeline caused the change.
- **Delivery is asynchronous and best-effort.** `internal/webhook.Dispatcher` enqueues events on a
  buffered channel (non-blocking: a full queue drops with a log, so the request/ingest path is never
  slowed) and a worker goroutine (`Run`) delivers them. Per delivery: load the applicable webhooks
  (`ListEnabledWebhooksForProject` — the project's own hooks plus org-wide hooks), POST the JSON with
  a per-request timeout; failures and non-2xx are logged, never propagated.
- **Deliveries are signed.** `X-Cerbix-Signature: sha256=<hmac>` over the exact body using the
  webhook's secret, plus `X-Cerbix-Event`. The secret is stored in **plaintext** (the server must
  reproduce the HMAC to sign — unlike API tokens, which are only ever verified) and returned **once**
  at creation so the subscriber can verify; listings strip it. Acceptable for an internal tool; a
  later iteration may encrypt secrets at rest.
- **Scope & management:** webhooks are org-scoped (optionally project-scoped), created/listed/deleted
  by org admins; a project-scoped hook must reference a project in the org.
**Consequence:** every incident lifecycle change fans out to matching webhooks with a verifiable
signature, from both manual/API and automatic incidents. Migration `00010_webhooks.sql` is
additive/reversible; `TruncateAll` includes it. The dispatcher shares no state with the request path
beyond the store, and its failure never affects incident writes. Keycloak client-credentials (the
second machine-auth path) and subscriber email channels remain for later.

## D-0026 — RabbitMQ dispatcher: distributed roles over durable queues, lazy role-natural consumers

**Context:** the founding requirement was a distributed pipeline (scheduler → broker → N workers →
broker → ingest) scalable beyond `--role=all`. iter-0004 shipped the `dispatch.Dispatcher` seam with
only the in-process implementation; iter-0019 adds the RabbitMQ one and wires the roles.
**Decision:**
- **`dispatch.AMQP` implements the existing `Dispatcher` interface** (no interface change). Two
  durable queues — `checks.jobs`, `checks.results`. `PublishJob`/`PublishResult` serialize JSON on a
  single mutex-guarded publish channel (persistent delivery). `Jobs()`/`Results()` **lazily** start a
  manual-ack consumer (with `Qos` prefetch = backpressure) on first call, forwarding onto a buffered
  Go channel. Laziness makes consumption role-natural: a scheduler only calls `PublishJob` (never
  consumes jobs), a worker calls `Jobs()`+`PublishResult`, the API calls `Results()` — so jobs are
  not stolen by non-worker processes.
- **Ack-on-enqueue.** A delivery is acked once handed to the in-process channel; losing a single
  check on a hard crash is acceptable (the scheduler re-emits next interval) and keeps the transport
  seam free of an ack concept the interface doesn't expose. Prefetch bounds in-flight jobs per worker.
- **Role-aware wiring in `cli`:** `--role=all` keeps the in-process dispatcher (dev/tests); `api`,
  `scheduler`, `worker` use the AMQP dispatcher and each run only their part (scheduler publishes
  jobs — needs a DB for leader-election + monitor reads; worker consumes+probes+publishes — stateless,
  no DB; api ingests results + serves HTTP). Distributed roles **fail fast** when `rabbitmq.url` is
  unset. Broker connect uses a **bounded startup retry** (dial up to 30×1s) — connection resilience
  for a lagging/booting broker, distinct from config self-healing.
**Consequence:** the scheduler (leader) + N workers + API can run as separate processes coordinating
through RabbitMQ, validated e2e (a monitor driven `up` across three processes; a second worker joins
and throughput continues). `--role=all` is unchanged. New dependency `github.com/rabbitmq/amqp091-go`.
The AMQP round-trip test is opt-in via `CERBIX_TEST_RABBITMQ_URL` (CI stays hermetic on the in-process
dispatcher). Ops note: the RabbitMQ container needs a writable `HOME` for its Erlang cookie in some
sandboxes (`-e HOME=/tmp`).

## D-0027 — Notifications (FR-010): per-monitor channels, fired on transition from ingest, HTTP types

**Context:** FR-010 — alert on a monitor's status change through Telegram/Slack/Email/webhook.
**Decision:**
- **Channels are per-project, linked to monitors** (`notification_channels` + a
  `monitor_notifications` join). A channel has a `type` (`webhook`/`slack`/`telegram`) and a JSONB
  `config` (a URL, or `bot_token`+`chat_id`); validation is type-specific in `domain`.
- **Delivery is fired from the ingest pipeline on a transition**, the same seam as auto-incidents:
  `reconcileTransition` fetches the monitor once and, on `→down`, opens the auto-incident **and**
  notifies (down); on `down→up`, resolves **and** notifies (recovery). A `pending→up` first check
  does not notify. The `notify.Dispatcher` is async best-effort (non-blocking enqueue, worker loads
  the monitor's *enabled* linked channels, renders per type, POSTs with a timeout; failures logged).
- **Type rendering is one pure `render` function:** webhook → structured JSON
  (`monitor.transition` + fields), Slack → `{"text": …}` to the incoming-webhook URL, Telegram →
  `POST {api}/bot<token>/sendMessage {chat_id,text}` (the Telegram base is a var so tests stub it).
  The `Dispatcher` and `render` are transport-only; the transition semantics stay in `ingest`.
- **Email (SMTP) is deferred** — the three HTTP channel types cover the common cases and stay
  hermetically testable (httptest); SMTP needs a mail server and is a follow-up.
- Management authz: channels are project-scoped (create/delete = editor+, read = viewer);
  monitor↔channel linking is editor+, and a linked channel must be in the monitor's project.
**Consequence:** monitors alert their linked channels on down/recovery, validated e2e (a monitor to a
toggleable target fires `🔴 DOWN` then `🟢 recovered` to a webhook receiver). This is distinct from
the FR-012 webhooks (which fire on *incident* events, org/project-scoped, HMAC-signed): notifications
are *monitor*-scoped, per-channel-type formatted, and unsigned. Migration `00011_notifications.sql` is
additive/reversible; `TruncateAll` includes it. `internal/notify` reuses the async-best-effort shape
of `internal/webhook` without sharing code (different payloads/targets).

## D-0028 — Push monitors: token heartbeat endpoint + scheduler liveness; ICMP deferred

**Context:** the MVP prober set was HTTP/TCP/ICMP/Push; HTTP+TCP shipped in iter-0004. This adds
**Push** (the passive dead-man's-switch for cron/batch/backups). **ICMP is deferred** — it needs raw
ICMP sockets (CAP_NET_RAW or the `ping_group_range` sysctl), which is unreliable to run and test in
the container/CI sandbox; it will land with a privileged, env-gated integration test.
**Decision:**
- **Push monitors are passive.** They carry a secret `push_token` (generated on create; unique
  index) and expose an **unauthenticated** endpoint `POST /api/v1/public/push/{token}` (on the same
  `PublicRouter` as status pages). The watched service POSTs there every interval; a call records an
  up heartbeat (or down with `?status=down`).
- **Heartbeats flow through the pipeline, not a direct write.** The push handler publishes the
  heartbeat via an optional `ResultSink` (the dispatcher's `PublishResult`), so a push goes through
  the exact same ingest path as an active check — status update, notifications (D-0027), and
  auto-incidents (D-0023) all apply uniformly. The API handler gets the sink wired in `cli` (api/all
  roles).
- **Liveness is the scheduler's job.** The leader, on its tick, evaluates each enabled push monitor:
  if it hasn't reported within its interval (`interval_seconds` = the expected heartbeat window) and
  isn't already down, it **publishes a down result** (ingest then marks it down and fires
  notifications/incidents). A `nextRun` throttle avoids re-publishing while silent; a fresh push
  brings it back up. Push monitors are never turned into `CheckJob`s (no active probe).
- `interval_seconds` is now required for push monitors (the liveness deadline).
**Consequence:** cron/batch/backup jobs can be watched with a heartbeat URL; silence past the
interval alerts through the normal channels. Validated e2e (push → up → stop → scheduler-down →
push → up → unknown-token 404). Migration `00012_push_token.sql` is additive/reversible. The push
endpoint being unauthenticated is by design (the token is the credential); an unknown token is 404.
Optional follow-up: surface the push URL in the monitor UI, and add the ICMP prober.

## D-0029 — Email (SMTP) notification channel completes FR-010

**Context:** FR-010's remaining channel type after iter-0020 (webhook/slack/telegram) was email.
**Decision:**
- **A `email` channel type** carries its SMTP settings in the channel `config` (`smtp_host`,
  `smtp_port` default 587, `from`, `to` — comma-separated; optional `smtp_username`/`smtp_password`),
  so it is self-contained like the HTTP channel types (no global mail config). Validation requires
  `to`, `smtp_host`, `from`. Migration `00013` widens the type CHECK to include `email`.
- **Delivery uses `net/smtp`** via a package `sendMailFunc` var (defaulting to `smtp.SendMail`, a var
  so tests capture the message without a mail server). The dispatcher's `deliver` branches: email →
  `sendEmail` (builds an RFC822 text/plain message with the 🔴/🟢 subject, `PlainAuth` when a username
  is set), other types → the existing HTTP `render`+`post`. Best-effort, logged on failure — same as
  the rest of `notify`.
**Consequence:** FR-010 is complete — a monitor transition alerts webhook/Slack/Telegram/**email**
channels. Validated e2e against a real SMTP catcher (mailpit): a down transition delivers a
`🔴 … DOWN` email to multiple recipients, recovery a `🟢 … recovered` email. No global config, no new
runtime dependency (`net/smtp` is stdlib).

## D-0030 — Monitor edit: PATCH with partial semantics; type/push_token immutable; create/edit share the form

**Context:** since iter-0010 the UI could create but not edit a monitor (a noted backend gap). This
adds monitor update end-to-end.
**Decision:**
- **`PATCH /api/v1/monitors/{id}` (editor+)** applies a *partial* update: the body's fields are
  pointers, and only the ones present overwrite the loaded monitor; omitted fields are unchanged.
  The merged monitor is `Validate`d before `store.UpdateMonitor`.
- **Type and `push_token` are immutable** — `UpdateMonitor` only writes name/target/schedule/
  conditions/enabled (changing a monitor's type or rotating its push secret is a create/delete, not
  an edit). Isolation/authz reuse `monitorAccess` (ProjectWrite).
- **The frontend `NewMonitorView` is now create *and* edit.** Route `/monitors/:id/edit` reuses the
  same form; on mount it loads the monitor, prefills (guarding the type-change watcher so it doesn't
  reset conditions), disables the type selector, and submits a PATCH instead of a POST. The monitor
  detail view gained an **Edit** action.
**Consequence:** monitors are editable through the API and the UI, closing the iter-0010 gap.
Validated e2e (rename + conditions + interval + disable reflected and persisted; partial
enabled-only update preserves other fields; invalid interval 400; the edit SPA route serves). No
schema migration (uses existing columns). `openapi.yaml` → 0.15.0 with the PATCH path + `UpdateMonitor`.

## D-0031 — Add member by email; local-login rate limit (NFR-010)

**Context:** two hardening gaps — org members could only be added by raw `user_id` (UX friction,
noted in iter-0010), and local login had no brute-force protection (NFR-010 rate-limit, deferred in
D-0016).
**Decision:**
- **Add member by email OR user_id.** `POST …/members` accepts `email` as an alternative to
  `user_id`; the handler resolves it via `store.GetUserByEmail` to an already-provisioned user. If no
  user has that email yet (they haven't signed in), it returns 400 "they must sign in once first" —
  cerbix provisions users JIT on first login, so a true pre-signup invite (placeholder user linked on
  first login) is a separate, larger feature; this covers adding existing colleagues without copying a
  UUID. Authz unchanged (org admin).
- **Local-login rate limit.** A per-client-IP sliding-window limiter (`internal/auth/ratelimit.go`,
  `login_rate_limit_per_minute`, default 10; 0 disables) is checked at the top of
  `LocalLoginHandler` — over the limit returns 429 before any credential work. The key is the
  client IP (X-Forwarded-For first hop, else RemoteAddr), so distinct callers are independent. In
  memory, best-effort (a single-process concern; a distributed limiter would need shared state, noted).
**Consequence:** members are addable by email; local login resists brute force (NFR-010 complete —
argon2id + uniform-401 + no-secret-logging were already in place). Validated e2e (add-by-email 201,
unknown-email 400; 10 attempts then 429 for one IP, a fresh IP unaffected). No migration; config gains
one optional field with a default.

## D-0032 — ICMP prober: unprivileged-first socket, migration widens the type CHECK; needs NET_RAW/ping_group_range

**Context:** the MVP prober set (HTTP/TCP/ICMP/Push) — ICMP was deferred in D-0028 as privileged;
this lands it.
**Decision:**
- **`icmp` monitor type** (active). The `icmpProber` (`golang.org/x/net/icmp` + `ipv4`) resolves the
  target, opens an ICMP socket **unprivileged-first** (`udp4` datagram, works where
  `net.ipv4.ping_group_range` is set) and **falls back to raw** (`ip4:icmp`, needs CAP_NET_RAW),
  sends one echo, and reports the first echo reply as reachable with the round-trip latency (else
  down with the error). Reachability maps to `Result.Connected`, so the existing condition engine
  (`[CONNECTED]`, `[RESPONSE_TIME]`) and the no-conditions default (connected → up) work unchanged.
- **Migration `00014` widens `monitors_type_check`** to include `icmp` — the type is enforced by a
  DB CHECK, so the domain enum alone is insufficient (this bit us in the e2e: create returned an
  `internal error` until the constraint was widened). `openapi`/frontend add `icmp` to the type
  selector.
- **Dependency**: `golang.org/x/net`.
**Consequence:** host reachability is monitorable. Validated e2e (an `icmp` monitor to a reachable
container → up with RTT; to a TEST-NET address → down) with the backend granted **CAP_NET_RAW**.
**Ops note:** an ICMP-capable deployment needs the worker to hold CAP_NET_RAW (`--cap-add=NET_RAW` /
`securityContext.capabilities.add: [NET_RAW]`) or the host `net.ipv4.ping_group_range` sysctl to
cover the process GID; the prober logs and reports down if neither is available. The hermetic ICMP
prober test skips where no ICMP socket can be opened.

## D-0033 — Daily-availability rollup: recompute-range job on plain PG; endpoint reads rollup + today-live

**Context:** the dashboard's 90-day timeline had no API to render from (noted since iter-0009), and
D-0017 deferred TimescaleDB continuous aggregates. This realizes the rollup portably.
**Decision:**
- **`heartbeats_daily` (monitor_id, day, up, total)** is a plain table (works on any Postgres). A
  **recompute-range job** (`RollupDailyAvailability(from, to)`) DELETEs and re-INSERTs the `[from,to)`
  range in one transaction, aggregating raw heartbeats per (monitor, UTC day) with the same
  maintenance-window exclusion as the SLI. Delete-then-insert (not upsert) so a day that drops to
  zero — e.g. a **retroactive maintenance window** — is removed, not left stale (an upsert-only
  version failed exactly this case in test).
- **The scheduler leader runs the job** on a ~1-minute cadence over a trailing 95-day window (single
  active via the existing advisory lock; no new leader election).
- **The availability endpoints read rollup + today-live:** `GET /monitors/{id}/availability?days=N`
  and `/projects/{id}/availability?days=N` return per-day `{day, up, total, uptime_percent}` —
  completed days from `heartbeats_daily` (cheap) plus **today computed from raw** (always current).
  The frontend dashboard renders a 90-day project timeline strip from the project endpoint.
- On TimescaleDB, `heartbeats_daily` can later be swapped for a continuous aggregate without changing
  the read path (D-0017 stands). Days are UTC (Postgres runs UTC).
**Consequence:** long-range availability is cheap and the dashboard 90-day signal strip is live.
Validated e2e (a seeded yesterday → rolled up 83.3%; today live 100%; project spans both days;
rollup fired within the tick). Migration `00015` additive/reversible; `TruncateAll` includes the table.

## D-0034 — Keycloak client-credentials: a second machine-auth path; issuer-trusted JWT, membership-gated

**Context:** FR-013 and the PRD's "API auth — both ways" require Keycloak machine clients
(OAuth2 client-credentials) to call the API alongside cerbix service-account tokens (iter-0013). A
service-account access token from Keycloak is a signed JWT whose audience is the client, not cerbix.
**Decision:**
- **A second Authenticator verifier**, `ccVerifier = provider.Verifier(&oidc.Config{SkipClientIDCheck:
  true})`, is built only when OIDC is enabled. It validates the issuer and JWKS signature but
  **relaxes the audience check** — machine tokens carry a per-client `aud`, and cerbix trusts any
  token minted by its configured realm issuer. This does **not** widen authorization: access stays
  membership-gated, so a service-account client with no cerbix memberships gets a principal with no
  access (empty `Memberships`, no `is_global_admin`).
- **The bearer path in `RequireAuth` tries the service-account token first**, and only on
  `store.ErrNotFound` (the presented string isn't a known cerbix token) falls through to
  `principalFromJWT`: verify via `ccVerifier`, **JIT-provision a user keyed by the token `sub`**
  (`UpsertUserByKeycloakSub`, the same path as interactive OIDC login; email defaults to
  `<sub>@clients` when the token carries none), load memberships, and flag `ViaToken=true`. A token
  that is neither a known service token nor a valid realm JWT yields **401** (not 500).
- **Grants for a machine client** are assigned exactly like a human user's: an org/project admin adds
  the JIT-provisioned identity to a project with a role. This keeps one authorization model
  (`authz.Can`) for cookies, service tokens, and client-credentials JWTs.
**Consequence:** FR-013 is complete — both API-auth methods (cerbix service tokens **and** Keycloak
client-credentials) are live and converge on the same RBAC. Verified with a hermetic mock-OIDC test
(a minted client-credentials JWT authenticates, JIT-provisions the subject, principal is `ViaToken`
with no memberships until granted; an invalid bearer → 401). **Out of scope (unchanged from the
plan):** a live-Keycloak realm/client e2e — the realm/client naming is an open deployment question;
the mock-OIDC verifier exercises the same `go-oidc` verification path. No new dependency.

## D-0035 — SSRF guard in the prober: validate the resolved connect IP; allow private, block metadata by default

**Context:** workers probe operator-supplied `target`s (HTTP/TCP/ICMP) with no IP validation — the
HTTP prober used a default `http.Client` that follows redirects, and TCP/ICMP dialed the target
directly. That is an SSRF surface: a project Editor could point a monitor at cloud instance metadata
(`169.254.169.254`) or another tenant's internal service. But cerbix's **purpose** is monitoring
internal apps, so a blanket "block private IPs" (as a naive SSRF fix would do) breaks the product.
**Decision:**
- **A `Guard` validates the resolved connect IP, not the hostname.** The HTTP prober dials through a
  custom `Transport.DialContext` that resolves the host, rejects it unless a candidate IP passes
  policy, and **dials that checked IP directly (pinned)**. Because every redirect hop re-dials through
  the same transport, the guard covers redirects; because the dialed IP is the checked IP, it defeats
  **DNS rebinding** (no re-resolution between check and connect). `Transport.Proxy` is disabled so a
  proxy can't bypass the check. The TCP prober shares the same guarded dialer; ICMP checks the
  resolved IP before sending (no dial to hook).
- **Policy (corrected default):** `prober.allow_private_ips` **defaults true** — RFC1918 + loopback +
  ULA are the product's job. `prober.allow_metadata_ips` **defaults false** — link-local
  `169.254.0.0/16` + `fe80::/10` (cloud instance metadata) are blocked unless explicitly enabled.
  Unspecified/multicast/other non-global-unicast are **always** blocked. A blocked target fails the
  probe with a clear `blocked target IP <ip> (<reason>)` message and **makes no network request**.
- Threat model: monitor creation is already gated to project Editor+, so this hardens an
  **authenticated** surface (cross-tenant / metadata reach), not anonymous SSRF. The default keeps
  every existing internal monitor working while closing the metadata hole.
**Consequence:** NFR-011 satisfied. Verified hermetically through the real `Runner.Run` path (a
metadata HTTP target and a locked-down loopback TCP target both report down with a blocked message and
never dial) plus a `checkIP` matrix and the hostname-resolution branch. No new dependency; the guard
is fully in-process (worker), so no live-stack e2e was required. `NewRunner()` keeps the safe default;
`NewRunnerWithGuard` takes the configured policy (wired in `cli`).

## D-0036 — Transactional outbox for webhook/notification delivery: enqueue in-tx, deliver with retry/backoff

**Context:** incident webhooks and monitor-transition notifications were delivered from **in-memory
best-effort queues** (a full queue dropped events; a restart lost every queued event) — a dual-write
that loses outbound events on any failure or deploy. The reviewer's P1 asked for guaranteed delivery.
**Decision:**
- **A single `outbox_events` table** (`topic, payload jsonb, status pending|delivered|dead, attempts,
  next_attempt_at, last_error`) is written **in the same transaction as the state change** that
  produced the event, so an event is durable iff its cause committed (no dual-write):
  - `CreateIncident` / `AddIncidentUpdate` enqueue an `incident_event` (webhook payload) in their
    existing transaction.
  - `SetMonitorStatus` becomes transactional and enqueues a `monitor_transition` (the raw
    `{monitor_id, prev, cur}` fact) only when the status actually changes.
- **The notification policy moved to the delivery worker** (application layer): the store records the
  raw transition; the outbox worker decides whether it warrants a notification (`ShouldNotify`: any →
  down, or up only from down). This keeps the store free of notification policy and makes the enqueue
  atomic (the old policy lived in ingest, after the status write).
- **A delivery worker** (`internal/outbox`) claims due events with
  `UPDATE … WHERE id IN (SELECT … FOR UPDATE SKIP LOCKED LIMIT n)`, which (a) lets **multiple replicas
  and roles** run the worker without double-claiming and (b) **pushes `next_attempt_at` forward by an
  exponential backoff that doubles as a lease** — a worker that dies mid-delivery leaves the row to
  become due again on its own. Delivery happens outside the row lock; success → `delivered`, failure →
  stays `pending` until `attempts >= maxAttempts` (10), then `dead` for operator inspection.
  `cerbix_outbox_delivered_total` / `cerbix_outbox_dead_total` are exported.
- **Delivery is at-least-once.** One outbox row fans out to all of a project's webhooks / a monitor's
  channels; a partial failure retries the whole event, so a flaky endpoint may see a duplicate. For
  alerts, a duplicate is acceptable and losing the event is not — the deliberate trade.
- The in-memory queues and the `IncidentSink`/`Notifier` seams in `ingest` and the API handler are
  **removed**; `notify`/`webhook` keep their rendering/HMAC/SMTP logic behind a `Deliver(...) error`
  method the worker calls. The worker runs wherever a database is configured (as the old dispatchers
  did).
**Consequence:** webhooks and notifications survive restarts and transient endpoint failures (FR-010 /
FR-012 hardened). Verified with a DB-gated store test (in-tx enqueue on incident + transition;
claim/lease pushes `next_attempt_at`; delivered/dead terminal states), a hermetic worker test
(dispatch by topic, policy skip, deleted-monitor drop, retry-then-dead, nil-deliverer/unknown-topic
failure), and a **real-binary e2e** (migration `00016` applied; the wired worker drained a seeded
event pending→delivered in ~3 s and incremented `cerbix_outbox_delivered_total`). Migration additive;
`TruncateAll` includes the table.

## D-0037 — Heartbeat retention via daily RANGE partitions; retention_days also bounds the rollup window

**Context:** `heartbeats` grew unbounded (raw time-series), degrading the hottest table over time. The
reviewer's P2 asked for native partitioning + a retention policy. The subtlety: the daily-availability
rollup (D-0033) recomputes from raw over a trailing window and **DELETE+INSERTs** that range — if raw
were dropped underneath a still-active recompute window, the rollup would recompute empty and **wipe
the frozen rows that carry long-range availability**.
**Decision:**
- **`heartbeats` is RANGE-partitioned by `ts` into daily, UTC-aligned partitions** (`heartbeats_pYYYYMMDD`)
  plus a **DEFAULT partition** (migration `00017`, which converts in place and copies existing rows into
  dated partitions). The default guarantees inserts never fail even if maintenance falls behind.
- **The scheduler leader maintains partitions** (hourly, and once on acquiring leadership):
  `EnsureHeartbeatPartitions` pre-creates `[today, today+2]`; `PurgeOldHeartbeats` **DROPs** dated
  partitions whose whole day is older than the cutoff (a cheap DDL, not a large DELETE) and clears any
  straggler rows from the default. Retention is a cheap partition drop; reads prune by `ts`.
- **`heartbeats.retention_days` (config, default 30, min 2) drives BOTH** the retention cutoff **and**
  the rollup recompute window: the leader now recomputes only `[today - retention_days, today)`, so the
  recompute never touches a day whose raw partition has been dropped. Long-range availability lives in
  the frozen rollup rows (never recomputed, never wiped); the 90-day dashboard reads rollup as before.
**Consequence:** raw storage is bounded and reclaimed cheaply, without losing long-range availability
(NFR-012). Verified with a DB-gated store test (partition auto-created + inserts routed off the
default; old partition dropped while the recent one and its data are kept), a scheduler test (the
leader runs `Ensure`+`Purge` with the coupled window), a config test (default/validation), and a
real-binary e2e (migration `00017` applied; the wired leader pre-created `today..+2` partitions on
boot). Migration is additive/reversible; `TruncateAll` still targets `heartbeats` (cascades to
partitions). A TimescaleDB hypertable + native retention policy can later replace this without
changing the read path (D-0017 stands).

## D-0038 — Scheduler efficiency: in-memory monitor snapshot + batched push-liveness query

**Context:** the leader's hot loop ran every second and, each tick, executed a **full
`ListEnabledMonitors` scan** and — for push monitors — a **per-monitor `LatestHeartbeat` lookup**
(N+1). At 10k+ monitors this is parasitic, steady load on Postgres for work whose inputs rarely change.
**Decision:**
- **Decouple the DB reload from the scheduling tick.** The leader reloads enabled monitors into an
  **in-memory snapshot every `refreshEvery` (15 s)**; the 1 s tick publishes due active checks from that
  snapshot with the existing `nextRun` map — no per-tick table scan. A new or edited monitor is picked
  up within the refresh window (documented latency, fine for interval-based checks).
- **Batch push-liveness into one query.** `StalePushMonitors` returns, in a single statement, the
  enabled, not-already-down push monitors whose dead-man's switch has tripped
  (`COALESCE(max(heartbeat.ts), created_at) < now() - interval`), using the `(monitor_id, ts DESC)`
  index for the correlated max. The leader runs it every `pushCheckEvery` (5 s) and publishes a down
  result per stale monitor, still throttled by `nextRun` to absorb the async status-update lag. This
  removes the per-push round-trip and moves the selection policy into indexed SQL.
- `LatestHeartbeat` is dropped from the scheduler's Store interface; the per-monitor `evaluatePush` is
  replaced by `checkStalePush`.
**Consequence:** Postgres load from scheduling is now ~one snapshot query per 15 s plus one batched
push query per 5 s, independent of monitor count per tick — the parasitic per-second full scan and the
push N+1 are gone, while behavior (which checks fire, which push monitors go down) is unchanged.
Verified hermetically (the leader schedules active checks from the snapshot and publishes downs for the
batched stale set), with a DB-gated `StalePushMonitors` test (stale/never-reported returned;
fresh/down/non-push excluded), and a real-binary smoke (leader healthy across both cadences, no
errors). No schema or config change; guideline-level scale target, not a hard limit.

## D-0039 — Outbox dead-letter visibility + replay (global-admin API)

**Context:** iter-0029's outbox parks an event as `dead` after `maxAttempts`, incrementing
`cerbix_outbox_dead_total` — but a dead event was otherwise invisible and unrecoverable (no way to see
why it failed or to retry it once the downstream endpoint is fixed). That's an operational gap in the
delivery guarantee.
**Decision:**
- **Three global-admin endpoints** (the outbox is a system-wide operational surface, not org/project
  scoped — guarded by `authz.ActionGlobalManage`):
  `GET /api/v1/admin/outbox/dead` (list dead events with topic, attempts, `last_error`, payload,
  timestamps), `POST /api/v1/admin/outbox/dead/{eventID}/replay` (requeue one → 204, 404 if not dead),
  `POST /api/v1/admin/outbox/dead/replay-all` (requeue all → `{replayed: n}`).
- **Replay resets the row to a fresh pending state** (`status='pending', attempts=0,
  next_attempt_at=now(), last_error=''`) so the existing worker redelivers it on its next poll — no new
  delivery path. `ReplayDeadOutbox` only matches `status='dead'` (replaying a pending/delivered id is a
  404, not a silent no-op). Replays are logged for audit.
**Consequence:** dead-lettered notifications/webhooks are now inspectable and recoverable by an
operator after fixing the downstream, closing the outbox loop. Verified with a DB-gated store test
(list returns only dead; replay resets to due-pending; replaying a non-dead id → ErrNotFound;
replay-all count), an API authz test (global-admin only; 403 for org-admin; replay 204/404;
replay-all count), and a **real-binary e2e** (admin logs in, lists a seeded dead event, replays it,
the wired worker delivers it pending→delivered, the list empties; unauthenticated → 401). No schema or
config change; no UI (ops endpoint). `openapi` → 0.19.0.

## D-0040 — Encrypt secrets at rest: AES-256-GCM with legacy-plaintext passthrough

**Context:** notification-channel credentials (SMTP passwords, Telegram bot tokens, incoming hook URLs)
and webhook HMAC signing secrets were stored **plaintext** in Postgres. A database dump or read access
leaked live secrets — a real hardening gap for the security-track spec.
**Decision:**
- **A `secret.Cipher` (internal/secret) encrypts/decrypts strings with AES-256-GCM** (authenticated),
  tagging ciphertext with an `enc:v1:` prefix and a random nonce per value. **Decrypt passes
  un-prefixed values through unchanged**, so encryption can be enabled on an existing database without
  migrating legacy plaintext rows; a nil cipher (encryption off) is the identity, and an encrypted
  value presented with no key is a hard error (never silently returned as ciphertext).
- **The store applies it at the column boundary**: `webhooks.secret` and every `notification_channels.config`
  value are encrypted on write and decrypted on read (the scan/collect helpers became `*Store` methods
  so decryption lives in one place per entity). Callers and the delivery workers always see plaintext;
  the deliverer signs/authenticates with the decrypted secret, unchanged.
- **`security.encryption_key`** is base64 of 32 bytes, validated at config load (fail-fast on
  malformed/wrong-length); **empty disables encryption** (the prior plaintext behavior — this is an
  explicit opt-in capability, not a silent downgrade). `cli` builds the cipher and attaches it via
  `store.WithCipher`.
**Consequence:** secrets at rest are protected once a key is configured (NFR-013), with a zero-migration
rollout and forward compatibility for existing rows. Verified with `secret` unit tests (round-trip,
legacy passthrough, wrong-key GCM failure, nil identity, key-length validation), a DB-gated store test
(the raw `secret`/`config` columns are `enc:v1:`-prefixed and never contain the plaintext, while reads
return plaintext; a legacy plaintext row still reads), a config test (key validation), and a real-binary
e2e (encryption on → a webhook created via the API stores `enc:v1:…` in the column while the API
round-trips the plaintext secret). No schema change; config addition only.

## D-0041 — Encryption key rotation: keyring (try-all decrypt) + `cerbix reencrypt`

**Context:** iter-0033 encrypted secrets under a single key with no rotation path — a compromised or
aged key could never be replaced, an operational security gap.
**Decision:**
- **The `secret.Cipher` becomes a keyring.** `New(keys...)` takes the primary (first) plus any previous
  keys. **Encrypt always uses the primary**; **Decrypt tries each key in turn** — AES-GCM's
  authentication tag distinguishes the right key from a wrong one, so no key identifier needs to be
  embedded and the iter-0033 `enc:v1:` format is unchanged. A value no key can open is a hard error.
- **Config**: `security.previous_keys` ([]base64) alongside `encryption_key`; `Keys()` returns the
  ordered, validated keyring (previous_keys without a primary is a config error). `serve` builds the
  keyring so the running service reads data under any listed key while writing under the primary.
- **`cerbix reencrypt`** (new one-shot subcommand) rewrites every webhook secret and channel-config
  value: read (decrypt via the keyring, incl. old keys) → write (encrypt under the primary). The
  rotation procedure: set the new key as `encryption_key`, move the old to `previous_keys`, run
  `reencrypt`, then drop the old key.
**Consequence:** encryption keys can be rotated with zero downtime and without data loss, completing the
at-rest-encryption story (NFR-013). Verified with `secret` keyring unit tests (a value under the old key
reads via `[new, old]`; new writes use the new primary; the old key alone can't read them; no-match →
error), a DB-gated `ReencryptSecrets` test (rotate `A`→`[B,A]`, reencrypt, then the columns read under
`B` alone and not under `A`), a config test (`previous_keys` validation), and a **real-binary rotation
e2e** (webhook created under key A → `reencrypt` under `[B,A]` changes the ciphertext → a B-only
`reencrypt` succeeds while an A-only `reencrypt` fails, proving the data moved off A). No schema change;
`enc:v1:` format and existing data untouched.

## D-0042 — SSRF guard: block IPv6 cloud metadata (fd00:ec2::254) explicitly

**Context:** a session code review found a gap in the SSRF guard (D-0035): `allow_metadata_ips=false`
blocked the IPv4 IMDS `169.254.169.254` (via the link-local rule) and `fe80::/10`, but **not** AWS's
**IPv6 IMDS `fd00:ec2::254`** — that address is in `fc00::/7` (ULA), which Go's `ip.IsPrivate()` reports
as "private", so with the default `allow_private_ips=true` it was waved through the private bucket.
**Decision:** the guard now checks an explicit `metadataIPs` list (`169.254.169.254`, `fd00:ec2::254`)
**before** the loopback/link-local/private buckets, gated on `allow_metadata_ips`. Metadata endpoints
are blocked by default regardless of which range they inhabit; ordinary ULA/RFC1918 addresses stay
allowed (they remain "private", not "metadata"), so internal monitoring is unaffected.
**Consequence:** the IPv6 IMDS bypass is closed (NFR-011 hardened). Verified by extending the `checkIP`
matrix: `fd00:ec2::254` is blocked by default and allowed only with `allow_metadata_ips=true`, while a
generic ULA (`fd12:3456::1`) is still treated as private. No config or behavior change for legitimate
targets. Found by an independent adversarial review of the session's work.

## D-0043 — Provider-agnostic OIDC (any issuer), not Keycloak-specific

**Context:** early decisions (D-0002/D-0011/D-0012/D-0013/D-0034) framed authentication around Keycloak
specifically — naming, comments, and even the `keycloak_sub` column. In practice cerbix only ever used
standard OpenID Connect discovery + Authorization Code + PKCE + ID-token verification, none of which is
Keycloak-specific. Hard-coding the vendor in docs/identifiers was misleading and would block deployments
against Auth0, Okta, Google, or Entra ID. **Decision:** treat the IdP as **any OpenID Connect-compliant
issuer**, selected purely by `oidc.issuer` (discovery). Keycloak is retained only as the dev-stack example
IdP and as one entry in "any issuer works (Keycloak, Auth0, Okta, Google, Entra ID)". No Keycloak-only code
paths exist; the client-credentials machine path (D-0034) is likewise issuer-agnostic. This supersedes the
Keycloak-specific framing of D-0002/D-0011/D-0012/D-0013/D-0034 (the mechanisms are unchanged; only the
vendor coupling is removed). **Consequence:** cerbix can authenticate against any conformant IdP with no
code change; docs and identifiers no longer imply a Keycloak dependency. Drives D-0044 (the `oidc_sub`
rename) and D-0045 (provider-neutral sign-in UI).

## D-0044 — Rename `keycloak_sub` → `oidc_sub` by rewriting the original migrations

**Context:** the users table stored the OIDC subject in a column literally named `keycloak_sub`, contradicting
the provider-agnostic model (D-0043). The obvious fix is an `ALTER TABLE ... RENAME COLUMN` migration.
**Decision:** since there is **no production instance yet**, rewrite the original migrations (`00001_init.sql`,
`00004_local_users.sql`) to define `oidc_sub` directly rather than adding a rename migration — the schema is
recreated from scratch in dev/CI, so a rename step would be permanent dead weight. The domain field
(`User.OIDCSub`), store functions (`UpsertUserByOIDCSub`, `GetUserByOIDCSub`), API field (`oidc_sub`), and the
generated frontend types were renamed to match. **Consequence:** no `keycloak_sub` identifier remains anywhere
(migrations, Go, OpenAPI, TS); the highest migration stays `00017` (no rename migration added). This is only
safe pre-production — once an instance ships, column renames MUST go through a forward migration.

## D-0045 — Public `GET /auth/config` + configurable login button label

**Context:** the SPA hard-coded a "Continue with Keycloak" button and a vendor badge, and had no way to know
whether local login or OIDC was even enabled on a given instance. With provider-agnostic OIDC (D-0043) the
button must not name a vendor, and the UI should adapt to whatever the server offers. **Decision:** add a
public (no-session) `GET /auth/config` endpoint returning `{local, oidc, oidc_button_label}`, and a
configurable `oidc.button_label` (default **"Continue with SSO"**). `LoginView` fetches it on mount and
renders the local form only if `local`, the OIDC button (with the configured label) only if `oidc`, and the
divider only when both — with an explicit hint when neither is configured. **Consequence:** operators brand
the sign-in button per their IdP ("Log in with Okta") without touching code; the login page truthfully
reflects each instance's enabled methods; no vendor name is baked into the SPA.

## D-0046 — p95 latency in SLA via `percentile_cont(0.95)`

**Context:** the SLA surface exposed only average latency, which hides tail behaviour that SLOs usually care
about. **Decision:** compute a 95th-percentile latency per window with
`percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms) FILTER (WHERE up)` — restricted to `up` heartbeats
and honouring maintenance exclusion exactly like the existing aggregates — and surface it as `p95_latency_ms`
on both monitor- and project-level `WindowSLA`. **Consequence:** dashboards and the SLA view show avg **and**
p95; the monitor-detail response-time chart draws a p95 reference line. Pure additive change to the SLA
schema; no new tables.

## D-0047 — Global tenant-scoped search endpoint

**Context:** navigation required drilling through org→project→monitor; there was no way to jump to a resource
by name. **Decision:** add `GET /api/v1/search?q=` (min 2 chars) that runs escaped-`ILIKE` lookups across
projects (name/slug), monitors (name), and incidents (title), returns typed `SearchHit`s, and — critically —
filters **every** hit through `Principal.VisibleProject(orgID, projectID)` in the handler so results never leak
across tenants. LIKE metacharacters are escaped (`\`, `%`, `_`) so a `%` query matches literally, not
everything. Surfaced by a debounced (220 ms) topbar `SearchBox` with keyboard navigation. **Consequence:**
users jump directly to monitors/projects/incidents they can see; tenant isolation is preserved at the API
layer (the store query is deliberately un-scoped, the handler is the single enforcement point), covered by
`api_search_test.go` (o1 viewer sees only o1) and `search_internal_test.go` (escaping).

## D-0048 — Self-service GUI: org/project creation dialog + consolidated Settings page

**Context:** organizations, projects, notification channels, API tokens, and webhooks all had working APIs but
no UI — they could only be created via curl. **Decision:** add (a) a `CreateDialog` modal driven by a small
`ui` store, with name→auto-slug, role-gated entry points in the org/project switcher (new-org = global admin,
new-project = org admin), calling `workspace.createOrg`/`createProject`; and (b) a consolidated **Settings**
page with tabs for notification channels (project-scoped), API tokens (org-scoped), and outbound webhooks
(org-scoped), each wired to its existing endpoints. **Consequence:** the whole tenancy + integrations surface
is operable from the UI with correct role gating; no new backend endpoints were needed (pure frontend over the
existing API).

## D-0049 — Frontend 1:1 with design artifacts; bespoke inline-SVG charts

**Context:** the original plan's stack sketch named TanStack Query and uPlot/ECharts, and the views had drifted
from the approved claude.ai/design artifacts. **Decision:** rebuild every SPA view **1:1** against its design
artifact, and standardize on **plain Pinia + `onMounted`/`watch` data-loading** and **hand-authored inline-SVG
charts** (`Sparkline`, the monitor-detail latency chart with avg/p95, availability strips) rather than adding
a data-fetching library or a charting dependency — the charts are simple, theme-token-driven, and lighter
without a library. **Consequence:** the shipped stack has no TanStack Query / uPlot / ECharts; docs that
implied them were corrected. Views match the artifacts; where an artifact element had no backend (per-day
public availability buckets, HTTP method selector, email subscribers, etc.) the UI was adapted honestly and
the gap logged as API-gap backlog rather than faked.

## D-0050 — Status-page edit/delete, incident→monitor link, dashboard KPI parity

**Context:** three 1:1/UX gaps surfaced in use: (1) status pages could be created but not **edited or
deleted** from the UI (no backend `PATCH`/`DELETE`); (2) manual incidents could not be tied to a specific
monitor (only auto-incidents set `monitor_id`), so the operator couldn't consciously bind an incident to the
affected service; (3) the dashboard diverged from the artifact — no **P95 latency** / **error-budget** KPIs
and no live pulse on Operational monitors. **Decision:** (1) add `PATCH /api/v1/status-pages/{pageID}`
(title/visibility; slug and org immutable; switching to unlisted mints a token, away clears it) and
`DELETE /api/v1/status-pages/{pageID}` (components cascade via FK), both org-admin gated, with an inline edit
form + confirm-delete in `StatusPagesView`. (2) `createIncident` accepts an optional `monitor_id` validated
to belong to the incident's project; `NewIncidentView` gains an explicit **Project** selector (no longer the
implicit workspace project) and an optional **Affected monitor** selector — so manual incidents surface on the
monitor-detail banner and status-page components like auto-incidents. (3) the dashboard KPI row becomes
Availability · Monitors-up (with down count) · **Error budget · 30d** (mean of monitors that have an SLO
target — there is no project-level target) · **P95 latency · 30d**; `MonitorCard` gains an error-budget meter;
`StatusPill` pulses (animate-ping, `motion-reduce` respected) on `up`. **Consequence:** status pages are fully
CRUD from the UI; incidents bind explicitly to a project (+ optional monitor); the dashboard matches the
artifact. Covered by `TestUpdateStatusPageAuthz`/`TestDeleteStatusPageAuthz` (viewer 403 / outsider 404 /
admin 200·204 / gone-after-delete); frontend type-checks + builds.

## D-0051 — Members: identity enrichment + role change / removal

**Context:** the members endpoint returned bare `Membership` rows (ids only), and there was no way to
change a member's role or remove a member from the UI — the first of the API-gap backlog from iter-0035.
**Decision:** (1) `GET /organizations/{orgID}/members` now returns an enriched `Member` (membership +
`email`, `display_name`, and `last_active_at` = the user's most recent session time). (2) add
`PATCH /organizations/{orgID}/members/{membershipID}` (change role) and `DELETE …/{membershipID}` (remove),
both org-admin gated via a shared `memberAccess` helper (404 hides cross-org / unknown, 403 for non-admins),
with role re-validated for the membership's scope (`ValidForScope`). (3) a **lockout guard**
(`CountOrgAdmins`, org-level `org_admin` with `project_id IS NULL`) forbids demoting or removing the last
org admin (400). The `MembersView` shows real name/email + initials avatar + last-active, a scope-aware role
`<select>`, and an inline confirm-remove; all gated behind `isOrgAdmin`. **Consequence:** member management
is fully operable from the UI without lockout risk. Covered by `TestUpdateMemberAuthz`,
`TestRemoveMemberAuthz`, `TestLastOrgAdminGuard` (api) and `TestOrgMembersEnrichmentAndMutation` (store, over
Postgres: email/display-name enrichment, last-active from a session, role update, delete, admin count).

## D-0052 — Public status page: per-day strips, maintenance & incident history

**Context:** the public render exposed only an aggregate `uptime_90d` per component (no per-day bars), and no
scheduled maintenance or resolved-incident history — the second API-gap item from iter-0035. **Decision:**
`writeStatusPageRender` now, for each monitor-backed component, also returns a **`daily`** 90-day series (via
`MonitorDailyAvailability`); and the render adds **`maintenance`** (active or upcoming windows, `ends_at` in
the future) and **`recent_incidents`** (resolved in the last 90 days, newest first) gathered across the
projects the components draw from. Manual components (no monitor) carry no daily data and fall back to the
aggregate meter. `PublicStatusView` renders a real per-day availability strip (green/amber/red/inset), a
Maintenance card (reason + window + Scheduled/In-progress tag), and a Past-incidents list. **Consequence:**
the public page matches the design artifact's history/maintenance sections truthfully. Covered by
`TestPublicRenderEnriched` (daily strip present, one active + one resolved incident split correctly, upcoming
maintenance surfaced). **Still open:** email subscribers (a separate feature — subscribers table + double
opt-in + per-incident email) remains unbuilt; the page offers RSS/Atom/JSON feeds instead.

## D-0053 — Monitor HTTP method, push grace period, and channel linking at create

**Context:** the New-monitor form couldn't set an HTTP request method (GET-only), couldn't give push
monitors extra tolerance before "down", and couldn't attach notification channels — the third API-gap item.
**Decision:** (1) monitors gain a `method` column (default `GET`) and a `grace_seconds` column (default 0),
added to the original `00003_monitors.sql` per the pre-production migration convention (D-0044).
`Monitor.Normalize()` upper-cases/defaults the method for HTTP, clears it for other types, and zeroes grace
for non-push; `Validate` rejects unknown methods and negative grace. The HTTP prober issues the configured
method; the push dead-man's-switch query (`StalePushMonitors`) widens the liveness window to
`interval_seconds + grace_seconds`. (2) create/update handlers accept `method`/`grace_seconds` and normalize
before validation. (3) `NewMonitorView` adds a method `<select>` (HTTP), a Grace-period input (push), and a
**Notifications** section of channel checkboxes that link/unlink via the existing
`/monitors/{id}/notifications` endpoints (create links the checked set; edit diffs against the currently
linked set). **Consequence:** monitors are fully configurable from the form; no separate step to wire alerts.
Covered by `TestMonitorMethodAndGrace` (domain normalize/validate), `TestHTTPProberUsesMethod` (prober issues
POST / defaults GET), `TestStalePushGrace` (grace extends liveness, store/Postgres), and
`TestCreateMonitorMethodAndGrace` (api normalization + reject TRACE + push grace/no-method).

## D-0054 — Structured postmortem over the markdown body (no schema change)

**Context:** the postmortem was a single free-text markdown `body`, so there was no consistent structure —
the last incident API-gap item. A backend change (structured columns) is unwarranted: the body is already
markdown, and the structure is presentational. **Decision:** keep the storage contract unchanged and make the
structure a **frontend concern**. A `lib/postmortem.ts` serializes fixed sections (**Summary / Root cause /
Resolution / Action items**) to `## Heading` markdown blocks and parses them back; a legacy or free-form body
with no recognized headings falls into Summary (so existing postmortems still render). `IncidentDetailView`
replaces the single textarea with the four section fields, renders a published postmortem as styled sections,
and adds an **Edit** affordance (the `PUT` is an upsert, so revising re-serializes and overwrites);
`IncidentsView`'s read-only panel renders the same sections. **Consequence:** postmortems have a consistent,
scannable structure with zero migration or API change; the body stays plain markdown, so feeds and the API
are unaffected. Frontend type-checks and builds (no frontend test harness exists; the lib is pure and simple).

## D-0055 — Status-page email subscribers (double opt-in + incident emails)

**Context:** the public status page could only be followed by RSS/Atom/JSON feed — the last API-gap item was
email subscribers. **Decision:** add a full subscription vertical. A new **`subscribers`** table (in
`00007_status_pages.sql`, pre-prod convention D-0044) holds `(status_page_id, email, confirm_token,
confirmed_at)` with a per-page-email unique key. A new optional **`mail`** config section (SMTP host/port/
user/pass, from, `public_base_url`) drives a small **`internal/mailer`** (SMTP, testable `sendMailFunc`);
when unset, subscription endpoints report 503. Public endpoints:
`POST /public/status-pages/{slug}/subscribers` (visibility-gated like the render; sends a **double opt-in**
confirmation email), `POST /public/subscriptions/{token}/confirm`, `DELETE /public/subscriptions/{token}`.
Incident emails ride the **existing transactional outbox**: an `internal/subscribe` notifier fans an
`IncidentEvent` out to the confirmed subscribers of every page that surfaces the incident's project
(`ConfirmedSubscriberEmailsForProject` joins subscribers→components→monitors), wired via a cli
`incidentFanout` composite so the webhook result governs retry and subscriber email is best-effort. The public
page gains a subscribe form and handles `?confirm=`/`?unsubscribe=` links. **Consequence:** end users self-serve
email subscriptions with confirmation; incident lifecycle changes notify them, reusing the reliable outbox and
SMTP. Covered by `mailer` (message build), `subscribe` (fan-out), `store` `TestSubscriberLifecycleAndFanout`
(confirm gating + join, Postgres), and api `TestSubscribeConfirmUnsubscribe` / `TestSubscribeDisabledWithoutMailer`.
**This closes the iter-0035 API-gap backlog (point 1).**

## D-0056 — Realtime status via SSE over an in-process event bus

**Context:** the SPA polled for status; live updates were the next backlog item (was marked
planned-not-shipped). **Decision:** add an in-process pub/sub **`internal/events.Broker`** (buffered
per-subscriber channels; a slow consumer drops events rather than stalling ingest). The ingest consumer
publishes a `status` event on every monitor status change (it already fetches the monitor there), and a new
authed **`GET /api/v1/events`** SSE endpoint streams them, filtered to the caller's visible projects
(`ListProjectsForUser`; global admins see all). The endpoint sets `text/event-stream` + `X-Accel-Buffering:
no` and pings every 25s; the server has no write timeout so streams stay open. The SPA has a `live` Pinia
store wrapping one `EventSource` singleton (kept open across navigation, closed on logout); Dashboard and
Monitor-detail patch status/latency live from it. **Consequence:** status changes reach the browser in
realtime without polling. Single-process only — multi-replica realtime would front the broker with Redis
pub/sub (noted, not yet needed). Covered by `events` (`TestBrokerPublishSubscribe`,
`TestBrokerDropsOnFullBuffer`) and api (`TestEventsDisabledWithoutSource`, `TestEventsStreamFiltersByVisibility`
— cross-tenant events filtered out). Supersedes the SSE-planned notes in the PRD/README/status.

## D-0057 — Per-control RBAC hiding in the SPA (defense-in-depth)

**Context:** the UI showed action controls to everyone; a viewer clicking New/Edit/Delete only learned they
lacked permission when the backend returned 403. **Decision:** hide or disable mutating controls by the
caller's effective role, computed on the client from `session.memberships`. A new
`session.canProjectWrite(orgID, projectID)` getter mirrors backend `ActionProjectWrite` (global admin,
org-level org_admin/editor, or project-level project_admin/editor); `session.isOrgAdmin(orgID)` gates
org-manage actions. Gated across nine views: New-monitor entry points (Dashboard/Monitors), monitor
Pause/Edit/Delete, SLA maintenance + SLO edit, Declare-incident, incident Resolve/update/postmortem, Settings
channels (project-write) vs tokens/webhooks (org-manage), all status-page mutations (org-manage), and the
create forms disable submit for deep-links. **This is presentation only — the backend authz layer remains the
sole enforcement point (every gated action is still 403/404-checked server-side);** the UI change just avoids
showing controls that would fail. Read-only content (lists, tables, timelines, published postmortems) stays
visible to everyone who can see the resource. Frontend type-checks and builds. **This completes point 3 and
the entire designed SPA surface.**

## D-0058 — Phase 6 probers, batch 1: DNS resolution + TLS cert expiry

**Context:** the catalog had HTTP/TCP/ICMP/Push; Phase 6 extends it. First batch = the two cleanest
network probers with no external dependencies. **Decision:** add monitor types **`dns`** and **`tls`**
(type CHECK extended in `00014`, pre-prod convention; both are `Active`). **DNS** (`dnsProber`) does
`LookupHost` — up = at least one address resolves; the resolved addresses are exposed as `[BODY]` (so
`[BODY] contains "10.0."` asserts an expected value) and the resolve time as `[RESPONSE_TIME]`. **TLS**
(`tlsProber`) dials host:port (default 443) **through the SSRF guard**, handshakes with
`InsecureSkipVerify` (so internal/self-signed certs are monitorable — expiry is the signal), and reports the
leaf cert's days-to-expiry via a new **`[CERT_EXPIRY]`** conditions placeholder; default (no conditions) = up
when the handshake succeeds and the cert is currently valid, so `[CERT_EXPIRY] > 14` alerts ahead of expiry.
No scheduler change (both are ordinary interval-scheduled active monitors); `NewMonitorView` gains the two
type chips, labels, icons, and condition defaults. **Consequence:** DNS-resolution and cert-expiry
monitoring ship. Covered by `TestDNSProber` (resolves localhost, body assertion, `.invalid` down),
`TestTLSProber` (httptest TLS server up, `[CERT_EXPIRY]` threshold pass/fail, closed port down), and
`TestDNSAndTLSMonitorTypesActive`. Remaining Phase-6 probers (gRPC health, PostgreSQL/MySQL/Redis/RabbitMQ,
PromQL, composite) are follow-on batches — several need per-type config (credentials/queries), a larger
schema step deferred to their own iteration.

## D-0059 — Phase 6 probers, batch 2: gRPC health

**Context:** continuing the catalog with another credential-free network prober. **Decision:** add monitor
type **`grpc`** (type CHECK extended in `00014`; `Active`). `grpcProber` dials host:port (default 50051)
**through the SSRF guard** via `grpc.WithContextDialer`, over a plaintext (`insecure`) transport, and calls
the standard **`grpc.health.v1` Health/Check** for the overall server (the `""` service); up = `SERVING`.
This adds a direct dependency on `google.golang.org/grpc`. `NewMonitorView` gains the gRPC type chip/label/
icon/placeholder (default check = SERVING; `[RESPONSE_TIME]` conditions still apply). **Consequence:** gRPC
health monitoring ships. Covered by `TestGRPCProber` (real `health.NewServer`: SERVING up, NOT_SERVING down,
closed port down). Remaining Phase-6 probers (PostgreSQL/MySQL/Redis/RabbitMQ, PromQL, composite) need
per-type config — several carry credentials, so they depend on an encrypted `monitors.config` step (their own
iteration). TLS/mTLS for gRPC is a follow-up (v1 is plaintext).

## D-0060 — Audit log of access changes (Phase 7)

**Context:** there was no record of who changed access — the first Phase-7 item. Chosen over the
infra-prober config foundation because it is self-contained (no new dependency, no credential handling).
**Decision:** add an **`audit_logs`** table (migration `00018`; org FK cascades, actor is a soft FK so
history survives user deletion) and record access-relevant actions: `member.add`, `member.role_change`,
`member.remove`, `token.create`, `token.delete`. A best-effort `Handler.audit` helper writes an entry after
each such mutation (a failure is logged, never fails the request). `GET /api/v1/organizations/{orgID}/audit`
(org admin) lists recent entries, newest first, joined with the actor's current identity (email/display, or
"a service token" when `via_token`). The Members view shows a read-only Audit-log section (org-admin gated).
**Consequence:** org admins can see who granted/revoked access and when. Scope is deliberately the
access-control surface (members + tokens); broader mutation auditing (monitors, status pages, incidents) can
extend the same helper later. Covered by `store` `TestAuditRecordAndList` (order, actor join, NULL actor, org
isolation, limit) and api `TestAuditRecordedAndListed`/`TestAuditListAuthz` (recording + viewer-403/outsider-404).

## D-0061 — Composite (group) monitor + the non-secret `monitors.config`

**Context:** the last credential-free Phase-6 prober — a composite that aggregates other monitors — needs
per-type config (child ids + mode) but no secrets. **Decision:** add a plain **`config jsonb`** column to
`monitors` (in `00003`, pre-prod convention; **no encryption** — this is the non-secret half of the deferred
config foundation, so infra probers can later add encrypted secret fields on top). Add monitor type
**`composite`**: it is `Active` (so the scheduler schedules it on its interval) but **target-less** (new
`NeedsTarget()` splits "scheduled" from "needs a network target"). `Config["children"]` is a CSV of child
monitor ids and `Config["mode"]` is `all` (default — every child up) or `any` (≥1 up). Because a composite
derives status rather than probing, the `compositeProber` reads child statuses via an **injected
`ChildStatusFunc`** (`runner.WithChildStatus(store.MonitorStatuses)` wired in cli) — the prober package keeps
no store dependency. The create/update handlers validate children exist **in the same project** (tenant
isolation). `NewMonitorView` gains a Group type with a members picker + all/any toggle. **Consequence:**
composite monitors ship with heartbeat history/SLA like any monitor, re-evaluated each interval. Missing/
deleted children count as not-up. Covered by `prober` `TestCompositeProber`/`TestCompositeWithoutLookup`,
`domain` `TestCompositeMonitorValidation`, `store` `TestMonitorConfigAndStatuses` (config round-trip +
`MonitorStatuses`), api `TestCreateCompositeMonitor` (in-project child 201 / cross-project 400 / no children
400). **This closes Phase 6's credential-free probers; the remaining infra probers (PostgreSQL/Redis/PromQL/
etc.) need the encrypted-config extension of this column.**

## D-0062 — Encrypted `monitors.config` + PostgreSQL prober

**Context:** the remaining infra probers carry credentials; this adds the encrypted-config foundation and the
first such prober. **Decision:** designate secret config keys (`SecretMonitorConfigKeys` = {`password`}, a
single source in `domain`). The store encrypts those values at rest with the existing keyring cipher
(`marshalConfig`) and decrypts on read (`scanMonitor` — both converted to methods so they see `s.cipher`);
non-secret keys (database/username/query/children/mode) stay plaintext. Secrets are **write-only over the
API**: `Monitor.Redacted()` blanks them in every monitor response (get/list/create/update), and on update an
empty submitted secret **preserves the stored one** (the client never receives it, so can't resend it). The
**`postgres`** prober connects via **pgx** with a **guarded `DialFunc`** (SSRF policy applies to the DB dial)
and runs a query (default `SELECT 1`); config keys: database, username, password, sslmode (default `prefer`),
query. `NewMonitorView` gets a Connection section with a write-only password field. **Consequence:** DB
monitoring ships with credentials encrypted at rest and never leaked to clients; the same pattern unblocks
Redis/MySQL/RabbitMQ/PromQL. Covered by `domain` `TestPostgresTypeAndRedaction`, `store`
`TestMonitorConfigPasswordEncrypted` (column has `enc:v1:`, non-secret plaintext, round-trip), api
`TestPostgresMonitorRedactionAndPreserve` (redacted response + preserve-on-empty), and `prober`
`TestPostgresProber` (live connect + SELECT 1 up / bad query / wrong password down — gated on the test DSN).

## D-0063 — Phase 6 completed: MySQL, Redis, PromQL probers

**Context:** the last three infra probers, all reusing the encrypted `monitors.config` foundation (D-0062).
**Decision:** add three monitor types (CHECK in `00014`; all `Active`/target-needing). **`redis`** — a hand-
written RESP client (no dependency) dials through the SSRF guard, optionally `AUTH`s (password encrypted at
rest), and `PING`s; up = `PONG`. **`mysql`** — the `go-sql-driver/mysql` driver via `sql.OpenDB` with a
**mutex-guarded, process-global registered dialer** (`RegisterDialContext("cerbixguard", …)` set from the
runner's guard, so the DB dial still honours the SSRF policy) runs a query (default `SELECT 1`). **`promql`** —
a guarded HTTP GET to Prometheus `/api/v1/query`; the scalar/first-vector value is exposed via a new
**`[RESULT]`** condition placeholder (with float comparison), so `[RESULT] < 0.9` alerts on a threshold;
default up = the query returns a value. `NewMonitorView` shares the DB Connection form (postgres/mysql), adds
a Redis auth section and a PromQL query field, and lists `[RESULT]` in the conditions editor. **Consequence:**
the Phase-6 prober catalog is complete (HTTP/TCP/ICMP/DNS/TLS/gRPC/composite/PostgreSQL/MySQL/Redis/PromQL +
Push). Covered by `prober` `TestRedisProber` (fake RESP server: PING/AUTH up, closed down), `TestPromQLProber`
(fake Prometheus: scalar up, `[RESULT]` threshold pass/fail, empty/no-query down), `TestMySQLProberClosedPort`
(guarded-dial path, closed → down), and `domain` `TestMySQLRedisPromQLTypes`. MySQL against a live server is a
DSN-gated integration follow-up (no MySQL in the dev stack).

## D-0064 — Two-factor authentication (TOTP) for local login (Phase 7)

**Context:** local (password) accounts had a single factor; Phase 7 hardening adds 2FA. OIDC users get MFA
from their identity provider, so 2FA is scoped to local credentials only.
**Decision:** implement **TOTP (RFC 6238)** with a hand-rolled `internal/totp` package (HMAC-SHA1, 6 digits,
30s period, base32 no-padding secret, ±1 step skew, constant-time compare — no new dependency; verified
against the RFC test vector). Migration `00019` adds `users.totp_secret` (encrypted at rest with the same
keyring cipher as monitor secrets) + `users.totp_enabled`, and a `totp_recovery_codes(user_id, code_hash,
used_at)` table (codes stored only as `HashToken` hashes, single-use via `UPDATE … WHERE used_at IS NULL`).
**Enrollment is two-step and self-service** (`SettingsView` → Security tab): `POST /api/v1/me/totp/enroll`
generates a pending secret (2FA not yet active) + an `otpauth://` URI; `POST …/enable` verifies a live code,
flips `totp_enabled`, and returns **8 single-use recovery codes shown once**; `POST …/disable` re-verifies the
account password before clearing the secret + codes. `enroll` is refused for token principals and non-local
accounts. **Login** (`LocalLoginHandler`) gains an optional `totp` field: after the password checks out, an
enrolled user must present a valid TOTP **or an unused recovery code** (`secondFactorOK` → `totp.Validate`
else `ConsumeRecoveryCode`); a missing/invalid factor returns `401 {"totp_required":true}` (a wrong *password*
stays a uniform 401 with no hint, preventing enumeration). `/api/v1/me` now reports `totp_enabled` so the UI
reflects state; the LoginView reveals a code field on the `totp_required` signal. **Consequence:** local
accounts can require a second factor; secrets/recovery codes are encrypted or hashed at rest and never
returned after issuance. Covered by `totp` `TestTOTP*` (RFC vector, skew, recovery-vs-code), `store`
`TestTOTPSecretAndRecoveryCodes` (encrypt-at-rest round-trip, single-use consume, replace/disable wipe), `auth`
`TestLocalLoginWithTOTP` (missing→totp_required, valid code, recovery once-then-gone, wrong-password uniform),
and api `TestTOTPEnrollEnableDisable` + `TestTOTPEnrollRejectsTokenPrincipal`.

## D-0065 — RabbitMQ, WebSocket & SSH probers (extended catalog)

**Context:** three remaining "by demand" catalog types from the plan (Phase 6/7), all built on the
existing SSRF-guarded dial + encrypted `monitors.config` foundations (D-0062).
**Decision:** add three monitor types (CHECK in `00020`; all `Active`/target-needing). **`ssh`** — dial
through the guard and read the server's identification banner (RFC 4253 §4.2); success (no conditions) = a
line starting with `SSH-`, exposed as `[BODY]` so `[BODY] contains OpenSSH` can assert the software/version.
**`websocket`** — perform the RFC 6455 opening handshake over the guarded dial (TLS-wrapped for `wss://`),
sending `Sec-WebSocket-Key` and verifying the server returns **101** with a matching **`Sec-WebSocket-Accept`**
(SHA-1 of key+GUID); `[STATUS]` sees the HTTP status. Targets accept `ws://`/`wss://`/`http(s)://`.
**`rabbitmq`** — two modes via config `mode`: **`amqp`** (default) opens a TCP connection through the guard,
sends the AMQP 0-9-1 protocol header, and confirms the broker starts the handshake (a `0x01` METHOD frame or
the broker echoing its own header on a version mismatch — either proves a live broker), no credentials
needed; **`management`** does a guarded HTTP GET to the management API (default `/api/overview`, port 15672)
with basic auth (username + write-only `password` encrypted at rest), mirroring HTTP's 2xx-default and
exposing `[STATUS]`/`[BODY]` for richer JSON assertions (queue/consumer/memory checks). `NewMonitorView` adds
the three type chips, per-type target labels/hints, and a RabbitMQ mode selector with a management
username/password/path form (password write-only, preserve-on-empty). **Consequence:** the prober catalog now
covers HTTP/TCP/ICMP/DNS/TLS/gRPC/composite/PostgreSQL/MySQL/Redis/PromQL/**RabbitMQ/WebSocket/SSH** + Push.
Covered by `prober` `TestSSHProber` (banner up + `[BODY]` assert + non-SSH/closed down), `TestWebSocketProber`
(valid handshake up, bad-accept/non-upgrading/closed down), `TestRabbitMQProberAMQP` (METHOD-frame + header-echo
up, non-AMQP/closed down) and `TestRabbitMQProberManagement` (200 + basic auth + `[BODY]`, 404 down), plus
`domain` `TestRabbitMQWebSocketSSHTypes`.

## D-0066 — Auto-incidents are optional per monitor

**Context:** every monitor going down automatically opened an incident (`ingest.reconcileTransition`), which is
noisy for best-effort / dev / composite monitors that shouldn't page.
**Decision:** add a per-monitor boolean **`monitors.auto_incident`** (migration `00022`,
`NOT NULL DEFAULT true` — existing rows and new monitors keep opening incidents unless opted out). The ingest
pipeline gates **only the open path** (`if mon.AutoIncident { openAutoIncident() }`); the **resolve path stays
unconditional**, so an already-open auto-incident still closes on recovery even if the operator turned the flag
off in the meantime. The impact level stays `major` (configurable severity is a possible later extension).
API: `CreateMonitor`/`UpdateMonitor` accept `auto_incident` (create defaults to true when omitted, mirroring
`enabled`; PATCH leaves it unchanged when omitted); the `Monitor` response carries it. The store persists it
faithfully via the shared `monitorColumns` (like `enabled`) — the DB default only backfills existing rows.
Frontend: a toggle in the New/Edit-monitor form (default on) and an "Auto-incident on/off" row on the monitor
detail. **Consequence:** operators can silence auto-incidents on individual monitors without disabling the
monitor or its notifications. Covered by `ingest` `TestAutoIncidentDisabledSkipsOpen` (off → no open, but a
pre-existing incident still resolves) and the updated `TestAutoIncidentOpenAndResolve` (seed `AutoIncident:true`),
`store` `TestMonitorCRUDAndHeartbeats` (round-trip true + update→false), api `TestCreateMonitorAutoIncident`
(default true / explicit false / PATCH toggle / omit-unchanged).

## D-0067 — Wire up dormant backend features in the SPA (change-password, dead-letter admin)

**Context:** an audit of frontend `api.*` calls vs. backend routes found endpoints with no UI: `POST
/api/v1/me/password` (change password), the dead-letter admin trio (`GET /api/v1/admin/outbox/dead`,
`POST …/{eventID}/replay`, `POST …/replay-all`), and a dead `href="#"` "Forgot?" link in the login page for
a reset flow the backend never implemented.
**Decision:** front-end only (no backend/API changes — every endpoint already existed and is documented):
(1) **Change password** — a form in `Settings → Security` (local accounts only), current + new + confirm,
8-char minimum and match checked client-side, `POST /api/v1/me/password`, inline success/error. (2)
**Dead-letter admin** — a new global-admin view `AdminOutboxView` at `/admin/outbox` (router `globalAdmin`
meta gate bounces non-admins; nav link shown only to global admins), listing dead-lettered outbox events
(topic, attempts, last_error, failed-at, expandable payload) with per-row **Replay** and a **Replay all**
action over the existing admin endpoints. (3) The dead "Forgot?" link is replaced with an honest,
non-clickable hint ("Forgot? Ask an admin") — local passwords have no self-service reset endpoint, so
promising one was misleading. Also hardened SLO objective editing (`SlaView`): the inline editor now always
re-seeds its field from the stored 30d objective on open and surfaces validation/API errors, fixing a
"Save does nothing" caused by an empty draft behind the placeholder. **Consequence:** local users can rotate
their password from the UI; global admins can triage/replay failed webhook & notification deliveries without
hand-rolling API calls; no more dead links. Verified via `vue-tsc` type-check + `vite build` + embed + web
test; backend unchanged.

## D-0068 — Self-service password reset for local accounts

**Context:** local accounts could only change their password while logged in (`me/password`, D-0067);
a forgotten password had no recovery path (the login "Forgot?" link was dead). Reset requires emailing the
user, so it reuses the existing global `mail` (SMTP + `public_base_url`) that already powers status-page
subscribers.
**Decision:** add a token-based reset flow (migration `00023` `password_reset_tokens(user_id, token_hash,
expires_at, used_at)` — only the hash is stored, like sessions/api-tokens; TTL **1 hour**, **single-use** via
`UPDATE … SET used_at=now() WHERE token_hash=$1 AND used_at IS NULL AND expires_at > now() RETURNING
user_id`). Two **public** auth endpoints (registered only when local login is on): `POST
/auth/local/reset/request` {email} — **always `200 {ok:true}`** (no account enumeration), throttled by the
shared per-IP `loginLimiter`; when the email matches a local account and a mailer is attached, it stores a
token hash and emails `${public_base_url}/reset?token=<raw>`. `POST /auth/local/reset/confirm` {token,
new_password} — consumes the token, enforces `min_password_length`, and `SetPassword`; invalid/expired/used →
`400`. The `Authenticator` gains an optional `WithMailer` (wired in `cli.go` from the same `mail` config);
`/auth/config` now reports **`password_reset`** (= local && mailer present) so the SPA shows the "Forgot?"
link only when reset actually works — otherwise it falls back to the honest "Forgot? Ask an admin" hint.
Frontend: `ForgotPasswordView` (`/forgot`, request) and `ResetPasswordView` (`/reset?token=…`, confirm),
both public; confirm redirects to `/login?reset=1` which shows a success banner. **Consequence:** local users
recover access without an admin, with no account enumeration and short-lived single-use tokens; the feature
is inert (and the link hidden) when mail isn't configured. Covered by `auth`
`TestResetRequestCreatesToken`/`TestResetRequestDisabledWithoutMailer`/`TestResetConfirm` (valid→204,
reuse/expired/unknown/short→400, no-enumeration) and `store` `TestPasswordResetTokens` (round-trip, single-use,
expiry). Also (web.go): the SPA now sends `Cache-Control: no-cache` on index.html and `immutable` on
content-hashed `/assets/*`, so a redeploy is picked up without a hard refresh (fixes "changes don't appear
after rebuild").

## D-0069 — Fix: SLO objective Save silently failed (number vs string)

**Context:** on the SLA & SLO page, Save on an SLO objective did nothing — no row change, no error — even
though the backend `PUT …/sla-target` and the api client both worked when called directly.
**Root cause:** the objective field is `<input type="number" v-model="draft">`; Vue's v-model on a
`type="number"` input hands back a **number**, but `saveSlo` called `draft.value.trim()` — a string method —
**before** the try/catch. On a number that throws `TypeError: .trim is not a function` synchronously, so the
handler rejected before issuing the request; the rejection surfaced only in the browser console (invisible in
the UI). The `.trim()` had been introduced with the empty-field validation; the original code used
`Number(raw)` directly, which tolerates both types. **Decision:** normalize before string ops —
`String(draft.value ?? "").trim()`. Also, prior debugging surfaced a deployment fact worth recording: the SPA
is served to users by the **nginx `cerbix-frontend` image on :8000** (built from `frontend/docker/Dockerfile`
via `vite build`), **not** the Go embed on :8080 — so frontend changes reach users only when that image is
rebuilt; `backend/internal/web/dist` and the `web.go` cache headers (D-0068) apply to :8080 only.
**Consequence:** SLO objectives save from the UI. Guard against the same class of bug: never call string
methods on a `type="number"` v-model binding without coercing.

## D-0070 — Uptime bars: SVG instead of flex (fix uneven tick spacing)

**Context:** the 30-day uptime bar on monitor cards (`UptimeBar`) and the 90-day project timeline
(`DashboardView`) rendered ticks as flex items (`flex-1` + `gap-[2px]`). With many ticks the fractional
per-item width forced the browser to round each box to whole device pixels; the accumulated error made
roughly every 4th gap look wider — visibly uneven spacing.
**Decision:** render both bars as a single **SVG** with `viewBox="0 0 N 10"`, `preserveAspectRatio="none"`,
and one `<rect>` per tick (`x=i+0.13`, `width=0.74`). Because every rect scales by the same factor, the
tick+gap rhythm is perfectly uniform at any container width. Fills use the theme CSS vars (`var(--up)` etc.),
so light/dark still follow automatically; the dashboard timeline keeps its per-day tooltip via a `<title>`
child on each rect. **Consequence:** availability bars have even spacing regardless of tick count/width.
Note (deploy): these are served by the nginx `cerbix/frontend` image (:8000, D-0069), so the image must be
rebuilt for the change to appear. Addendum: the SVG `<rect>` ticks carry `rx=0.2 ry=0.7` so they render with the same rounded corners the original `rounded-[2px]` divs had (the SVG rewrite had briefly made them square).

## D-0071 — Dashboard KPI footers (trends) + rounder cards, to match the design artifact

**Context:** the dashboard hero KPIs diverged from the reference artifact — Availability had no footer and P95
showed a bare "avg N ms" instead of the artifact's trend line ("▲ stable across N checks"); the user also
wanted rounder card corners.
**Decision:** `Kpi` gains an optional `trend` ({dir: pos|neg|flat, label}) rendered as a colored
mono chip (▲ up=green / ▼ down=red / ▪ flat) before the grey sub-text, mirroring the artifact's `.trend`.
Real data drives the trends (no synthetic values): **Availability · 30d** shows `▲/▼ ±X.XX% vs prev 30d`
computed from the 90-day daily strip (mean of the last 30 days vs the 30 before; falls back to "rolling
30-day window" until ≥60 days of history exist). **P95 latency · 30d** shows `▲ stable` / `▪ variable`
(p95 ≤ 1.6×avg = stable) plus `across N checks` from the window's check count. **Card shape:** the KPI row was
one panel with square inner grid cells (so a radius bump was invisible there); reworked into **four individual
rounded cards** (`Kpi` is now a standalone `rounded border bg-surface shadow-card`) with the 90-day availability
as its own card below — matching the separate-card look of the public status page the user preferred. Global
`borderRadius.DEFAULT` stays **8px** (= the status page's `rounded-lg`) — the user's "round the boxes" turned
out to mean the green uptime ticks, not the card corners (see D-0070 addendum), so card radius is unchanged. **Consequence:** the dashboard reads like the artifact with
genuine metrics behind the trends and rounded status-page-style KPI cards. Served by the nginx `cerbix/frontend`
image (:8000) — rebuild it to see the change.

## D-0072 — Status page "Past incidents" accordion (timeline / postmortem)

**Context:** the public status page's past-incidents list showed only title + resolved-time + impact — too
thin. `recent_incidents` in the render was bare `[]domain.Incident` (no timeline, no postmortem), even though
both are stored.
**Decision:** enrich each resolved incident in the render and expand it into an accordion. Backend
(`writeStatusPageRender`): `recent_incidents` items become **`recentIncidentView`** = the incident **plus its
`updates` timeline and `postmortem`** (nil when none) — fetched per incident (few over 90 days, so N+1 is
fine); openapi gains `RecentIncident` (`allOf` Incident + updates + postmortem), TS regenerated. Frontend
(`PublicStatusView`): each past incident is a collapsed row (title · resolved-when · **duration** started→
resolved · impact · a "postmortem" hint when present) that **expands** to show — by priority — the
**published postmortem** (parsed into sections via the existing `renderSections`, matching the internal
incident view) **or**, when there's no postmortem, the **update timeline** (each entry: status pill + relative
time + body); "No further details were published." only if neither exists. Bodies render as `whitespace-pre-wrap`
(no markdown-to-HTML step exists; consistent with `IncidentDetailView`). **Consequence:** visitors get the full
communication history / postmortem per past incident, like Atlassian Statuspage. Both public and
member (`/render`) status pages share the enrichment. Covered by api `TestPublicRenderEnriched` (asserts the
recent incident carries its 2 updates + postmortem). Served by the backend (:8080, proxied) + nginx
`cerbix/frontend` (:8000) — both images rebuilt.

**Addendum — active incidents enriched too:** the view struct was generalized (`recentIncidentView` →
`incidentDetailView`, incident + updates + optional postmortem) and a shared `enrichIncidents(…, withPostmortem)`
helper now feeds **both** `active_incidents` (timeline only) and `recent_incidents` (timeline + postmortem);
openapi's `RecentIncident` was renamed to **`IncidentDetail`** and both arrays reference it. On the page each
active incident shows its **latest update inline** (current communicated state: status pill + time + body) with
a **"Show full timeline (N)"** toggle that expands the whole history (same accordion machinery). `TestPublicRenderEnriched`
also asserts the active incident carries its update.

## D-0073 — Incident status change via a one-click segmented picker

**Context:** on the incident detail page, changing status meant opening a `<select>` buried in the "Post an
update" composer — not obvious and several clicks.
**Decision:** replace the status dropdown with a **segmented status picker** (the four lifecycle statuses as
buttons: Investigating / Identified / Monitoring / Resolved). The incident's current status is highlighted;
clicking another selects it in one click. The message textarea is now explicitly optional, and the post
button's label reflects the action — `Change to <Status>` when a new status is selected, else `Post update`
(a plain comment keeping the current status). Frontend-only (`IncidentDetailView`), same
`POST /incidents/{id}/updates` endpoint and posting logic; the top-bar "Resolve" quick action stays.
**Consequence:** status transitions are visible and one-click; you can change status with or without a note.
Verified by `vue-tsc` + `vite build`; served by the nginx `cerbix/frontend` image (:8000).

## D-0074 — Incidents list: drop the redundant nav button, add inline Resolve

**Context:** in `IncidentsView`'s detail panel, both "Manage →" (header) and the postmortem "Write/Edit"
button were `RouterLink`s to the **same** incident page — redundant — and the postmortem "Write" showed even
on active incidents where a postmortem is disallowed (gated to resolved). There was also no way to resolve an
incident from the list (only from `IncidentDetailView`).
**Decision:** (1) the postmortem "Write/Edit postmortem" link now appears **only for resolved incidents**
(with a "can be written once resolved" hint otherwise), so it no longer duplicates "Manage" on active
incidents and never points to a blocked action. (2) Add an inline **Resolve** button to the panel header for
active incidents (`canProjectWrite`-gated) — posts a `resolved` update via `POST /incidents/{id}/updates`,
refreshes the list, and keeps the incident selected to show its resolved state; full editing still lives
behind "Manage →". Frontend-only (`IncidentsView`). **Consequence:** each action is distinct (Resolve =
quick close, Manage = full edit, Write postmortem = resolved-only), and incidents can be resolved without
leaving the list. Verified by `vue-tsc` + `vite build`; served by the nginx `cerbix/frontend` image (:8000).

## D-0075 — Incidents list: inline segmented status change

**Context:** follow-up to D-0074 — moving an incident through Investigating → Identified → Monitoring still
required opening "Manage". 
**Decision:** add a **"Set status" segmented control** to `IncidentsView`'s detail panel (active incidents,
`canProjectWrite`-gated) with the three working statuses; clicking one transitions in a single click
(`setStatus` posts an update via `POST /incidents/{id}/updates`, refreshes the list, keeps the incident
selected). The current status is highlighted and is a no-op. **Resolve stays a separate, deliberate button**
because it's terminal — it is not part of the one-click segmented control. `resolveSelected` is now just
`setStatus("resolved", "Resolved.")`. Frontend-only (`IncidentsView`). **Consequence:** the working-status
progression is one click from the list; resolving remains an explicit action. Verified by `vue-tsc` +
`vite build`; served by the nginx `cerbix/frontend` image (:8000).

## D-0076 — Alert reliability: confirmations + maintenance suppression (Tier 1, part 1)

**Context:** the status flip was immediate on a single failed check, and maintenance windows only affected
SLA math — a check going down during planned work still opened an incident and paged. First slice of the
"alert reliability" work.
**Decision:** replace the ingest status write with an atomic **`RecordCheckStatus(monitorID, up)`** that
applies two policies in one `FOR UPDATE` statement (migration `00024` adds `monitors.failure_threshold` (default
1) + the live `consecutive_failures` counter):
- **Confirmations:** a down flip happens only after `failure_threshold` consecutive failed checks; recovery
  (up) is immediate and resets the counter. Default 1 preserves today's behavior (and keeps push monitors,
  which already have grace, unchanged).
- **Maintenance suppression:** if the flip lands inside an active maintenance window (monitor- or
  project-scoped), the status still changes (accuracy + SLA) but the monitor-transition notification is **not**
  enqueued and `suppressed=true` tells ingest **not to open an incident**. (Known edge: an outage that begins
  during maintenance and continues after it ends won't re-page, since the flip already happened — acceptable
  and documented.)
Both active-prober and push (stale-timeout) results flow through the same method (both go via ingest).
Domain `Monitor.FailureThreshold` (Normalize floors it at 1); API create/update accept `failure_threshold`;
`NewMonitorView` gains a "Confirmations" field. **Consequence:** far fewer false alerts on brief blips, and no
paging during planned maintenance. Covered by `store` `TestRecordCheckStatusConfirmationsAndMaintenance`
(threshold flip, recovery reset, maintenance-suppressed down), `ingest` `TestConfirmationsBeforeDown` +
`TestMaintenanceSuppressesIncident`, api `TestCreateMonitorFailureThreshold`. **Remaining Tier-1 piece:**
re-notify while down + explicit recovery cadence (next).

## D-0077 — Re-notify while down + reminder events (Tier 1, part 2, completes alert reliability)

**Context:** completes the Tier-1 "alert reliability" work (D-0076). A monitor that stayed down was alerted
once; there was no reminder cadence, and an outage that began during maintenance (flip suppressed) never paged
afterward.
**Decision:** add per-monitor **`renotify_seconds`** (migration `00025`; 0 = off, default) plus an internal
**`last_notified_at`** timestamp. `RecordCheckStatus` now maintains `last_notified_at`: stamped on a fresh,
non-suppressed down flip, cleared on recovery, otherwise left for the reminder job. A new store method
**`EnqueueRenotifyReminders`** atomically claims down monitors whose `last_notified_at` is older than
`renotify_seconds`, enqueues one reminder per monitor, and bumps `last_notified_at` — all in one transaction.
Reminders reuse `TopicMonitorTransition` with a new **`Reminder`** flag (`MonitorTransition.ShouldNotify()`
returns true for reminders even though prev==cur==down); the outbox worker re-delivers the standard "down"
notification to the monitor's channels. The **scheduler leader** calls it on a coarse `renotifyEvery` (15s)
tick — the per-monitor interval gates the actual re-send. Domain `Monitor.RenotifySeconds`; API create/update
accept it; `NewMonitorView` gains a "Re-notify" field. **Consequence:** long outages page repeatedly at a
configurable cadence (and this also covers the D-0076 maintenance edge — a still-down monitor re-alerts once
`last_notified_at` was set post-maintenance on the next fresh flip). Covered by `store` `TestRenotifyReminders`
(due only after the interval, only re-notify-enabled monitors, bumped after send, cleared on recovery),
`domain` `TestMonitorTransitionShouldNotify` (reminder path), api `TestCreateMonitorFailureThreshold`
(renotify round-trip). **Tier 1 complete.**

## D-0078 — Monitor tags/labels + client-side filtering (Tier 2)
**Context:** with the catalog growing (HTTP/TCP/ICMP/push/postgres/redis/rabbitmq/ws/ssh/composite) a project can
hold dozens of monitors; the list was a flat table with no way to slice by environment, team, or tier.
**Decision:** add a free-form **`tags text[]`** column on `monitors` (migration `00026`, `DEFAULT '{}'` + GIN
index `monitors_tags_idx` for future server-side `@>` queries). Tags are normalized in the domain layer
(`normalizeTags`: trim, drop empties, case-insensitive de-dupe keeping first spelling, cap 20 tags × 40 chars,
never nil) so `Monitor.Normalize()` is the single owner — API create/update just pass the slice through.
`monitorColumns`/`scanMonitor`/insert/update carry `tags`; a nil slice is coerced to `[]string{}` before write
so pgx sends an empty array, not NULL. Filtering is **client-side** for now (`MonitorsView`: `activeTags` set,
`allTags` derived from the loaded page, AND-semantics `shown` computed) — cheap, no new endpoint, and the GIN
index is already in place if/when the monitor list needs server-side tag filters at scale. `NewMonitorView`
gains a chip input (Enter/comma to add, click to remove). **Consequence:** monitors are labelable and the list
filters by any conjunction of tags without a round-trip. Covered by `domain` `TestNormalizeTags`, `store`
round-trip in `TestMonitorCRUDAndHeartbeats`, api `TestCreateMonitorTags` (normalize on create + PATCH replaces
the set). **Tier 2 complete.**

## D-0079 — SLO burn-rate alerts (Tier 3)
**Context:** monitors could alert on a hard down/up transition and re-notify (D-0076/D-0077), but nothing warned
when a *flaky* service was quietly draining its SLO error budget — many brief failures that never trip a single
long outage. **Decision:** add opt-in per-target burn-rate alerting on `sla_targets` (migration `00027`:
`burn_alert_enabled`, `burn_window_seconds` default 3600, `burn_threshold` default 14.4, `burn_firing` latch,
`burn_notified_at`). `sla.BurnRate(objective, up, total)` is the single owner of the math: observed bad fraction
÷ the objective's allowed bad fraction (0 when there's no data or no budget). The **scheduler leader** runs
`store.EvaluateBurnAlerts` on a `burnEvery` (1m) tick: for each burn-enabled monitor target it counts
heartbeats over the burn window (maintenance excluded, same predicate as `MonitorSLI`) and compares the rate to
the threshold. `burn_firing` makes it **edge-triggered** — one alert when it crosses up (🔥) and one recovery
(✅) when it drops back, never one-per-tick — all inside a single `FOR UPDATE` tx that also enqueues the outbox
event. Delivery reuses the notification channels via a new `TopicSLOBurnAlert` topic + `SLOBurnAlert.Message`;
`notify.Dispatcher.DeliverText` fans arbitrary text to a monitor's channels (webhook event `monitor.alert`).
Enabled from the SLA view's inline objective editor; a 🔥 marks enabled/firing targets. **Consequence:** slow
budget burn pages before it becomes a full outage. Covered by `sla` `TestBurnRate`, `store`
`TestEvaluateBurnAlerts` (fires once, idempotent while firing, resolves on recovery, latch asserted), `outbox`
`TestDeliversBurnAlert`, api `TestMonitorSLAAndTarget` (burn_alert round-trip).

## D-0080 — Scheduled weekly SLA reports (Tier 3)
**Context:** SLA/SLI was pull-only (the SLA view); teams had no periodic push summary of how a project tracked
against its objectives. **Decision:** add an opt-in weekly SLA report per project (migration `00028`:
`projects.sla_report_weekly` + `sla_report_last_at` watermark). The **scheduler leader** runs
`store.EnqueueDueSLAReports` on a `reportEvery` (1h) tick; a project is due when the watermark is unset or older
than 7 days. Due projects are claimed `FOR UPDATE` and their 7d/30d availability computed via the existing
`ProjectSLI`, packed into a `TopicSLAReport` outbox event, and the watermark bumped — all in one tx, so a report
goes out once per period across replicas. `domain.SLAReport.Message` renders the multi-line summary;
`notify.Dispatcher.DeliverProjectText` fans it to **every enabled channel in the project** (new
`ListEnabledChannelsByProject`; webhook event `sla.report`). Toggled from the SLA view header
(`PUT /projects/{id}/sla-report`), surfaced in the project SLA read. **Consequence:** each project can receive a
recurring availability digest on its own channels. Covered by `store` `TestEnqueueDueSLAReports` (due once,
watermark blocks re-send, disabled skipped), `outbox` `TestDeliversSLAReport`, api `TestProjectSLAReportToggle`.
**Tier 3 complete.**

## D-0081 — Incoming Alertmanager webhook receiver (Tier 4)
**Context:** cerbix could push incidents outward (webhooks, D-0027) and open them from its own monitors
(auto-incidents) or the API, but had no way to *ingest* alerts from the existing Prometheus/Alertmanager stack —
so a Prometheus-detected problem never became a cerbix incident/status-page entry. **Decision:** add a
project-scoped receiver `POST /api/v1/projects/{id}/alerts/alertmanager` that consumes the Alertmanager webhook
payload (schema v4). It is **authed like any project write** — a service-account bearer token (D-0034) in
Alertmanager's `http_config` — rather than a new anonymous endpoint, so no unauthenticated incident creation.
Alerts correlate by **fingerprint**, persisted as `incidents.external_key` (migration `00029`); a partial unique
index `(project_id, external_key) WHERE external_key IS NOT NULL AND status <> 'resolved'` guarantees at most one
open incident per key, making the receiver idempotent. A *firing* alert opens an incident (source `api`, impact
mapped from the `severity` label: critical/page→critical, warning/major→major, else minor; title from the
`summary` annotation) unless one is already open for that fingerprint; a *resolved* alert closes the incident its
firing opened via the normal `AddIncidentUpdate` resolve path (which stamps `resolved_at` and fires the outbox
incident event); unknown fingerprints on resolve are ignored. The response reports `{opened, resolved, ignored}`.
`store.FindOpenIncidentByExternalKey` is the correlation lookup. **Consequence:** the Prometheus stack can drive
cerbix incidents and status pages without bespoke glue. Covered by `store` `TestFindOpenIncidentByExternalKey`,
api `TestAlertmanagerWebhook` (open → idempotent re-fire → resolve → unknown-resolve ignored → viewer 403).
**Tier 4 complete.**

## D-0082 — OIDC configurable from the Settings UI (DB override + live reload)
**Context:** OIDC was configurable only via the strict YAML `oidc:` block, applied once at boot with fail-fast
discovery (D-0043/D-0045). Operators wanted to configure/rotate the IdP from the UI without a redeploy.
**Decision (design agreed via artifact):** a global-admin, instance-wide OIDC override in the DB that, once
saved, **fully replaces** the config file (the file becomes a bootstrap seed used only until the first UI save).
- **Storage:** singleton `oidc_settings` row (migration `00030`, `id boolean PK CHECK(id)`); `client_secret`
  encrypted at rest via the existing `secret.Cipher` (mirrors `users_totp.go`); `ReencryptSecrets` extended so
  key rotation covers it; `TruncateAll` includes the table.
- **Live provider:** `auth.Authenticator` now holds provider/verifier/ccVerifier/oauth behind an
  `atomic.Pointer[oidcRuntime]`, nil = inactive. `SyncOIDC` resolves the effective settings (DB row if present,
  else config bootstrap), rebuilds via discovery, and swaps atomically. `/auth/login` + `/auth/callback` are
  always registered and return **503** while inactive; `/auth/config` and the login button reflect the live
  state. `auth.New` no longer discovers at boot.
- **Startup resilience (agreed):** `StartOIDC` does an initial sync then runs a background reloader
  (`oidcRetryEvery` 30s) that retries while intended-enabled but not yet active — an unreachable IdP at boot no
  longer crashes the process (local login stays up), it just 503s SSO until the provider builds.
- **API:** `GET/PUT /api/v1/settings/oidc` under `requireGlobalAdmin`. GET redacts the secret
  (`client_secret_set` only). PUT validates shape (issuer/client_id/redirect_url required when enabled),
  preserves the stored secret on blank submit, persists, then re-syncs the provider; a build failure is
  reported (`reload_error`, `active:false`) but the save still succeeds (the reloader keeps trying).
- **Lockout safety:** local password+TOTP login is independent, so a global-admin can always recover.
**Consequence:** SSO can be set up, rotated, enabled/disabled entirely from Settings → Authentication (a new
global-admin-only tab) with no redeploy. Covered by `store` `TestOIDCSettingsRoundTrip`, api
`TestOIDCSettingsReadWrite`/`TestOIDCSettingsSecretPreserveAndReloadError`, `auth` `TestConfigHandler`
(runtime-driven label) + existing OIDC flow tests (now synced explicitly in the test helper).

## D-0083 — Generalized instance settings (branding, auth-policy, alerting, monitor-defaults)
**Context:** after per-feature OIDC settings (D-0082), more instance-wide knobs were wanted in the UI without a
table-per-feature sprawl. **Decision (design agreed via artifact, "approach A"):** one singleton
`instance_settings` (migration `00031`) with a **JSONB column per group**; each group is a self-validating Go
type (`domain/settings.go`: `Branding`, `AuthPolicy`, `Alerting`, `MonitorDefaults`, each with `Validate()` and a
`Configured` flag). A shared `settings.Service` resolves the **effective** value per group — DB override if
`Configured`, else the config-file **bootstrap** (auth-policy/monitor-defaults), else code `Defaults()`
(branding/alerting) — and serves it from a lock-free `atomic.Pointer` snapshot refreshed write-through on save
and on a 30s timer (so cross-replica writes land). Precedence is **"UI fully replaces"**, consistent with OIDC.
Per-group API `GET/PUT /api/v1/settings/{branding,auth-policy,alerting,monitor-defaults}` under
`requireGlobalAdmin`; **branding is also public** (`GET /api/v1/public/branding`) so the login page and status
pages can theme without auth. **OIDC stays its own table** (`oidc_settings`) — it needs a live provider rebuild;
`instance_settings` covers groups whose read is just a snapshot. **Consumers:** auth reads the policy (session
TTL, allowed email domains gate for local + OIDC-JIT login, TOTP-required enforcement, min password length);
the outbox suppresses monitor-transition + burn-alert notifications under `global_silence` (incidents still
recorded, incident webhooks/SLA reports unaffected); `createMonitor` fills omitted fields from monitor-defaults;
the SPA applies branding at startup (product name, `--accent` CSS override, announcement banner). **Consequence:**
four instance-wide settings groups are now editable from Settings (global-admin), extensible by one column + one
Go type. Covered by `domain` `TestBrandingValidate`/`TestAuthPolicyValidateAndHelpers`/`TestAlertingSilenced`/
`TestMonitorDefaultsValidate`, `settings` `TestResolveBootstrapThenOverride`, `store` `TestInstanceSettingsRoundTrip`,
`outbox` `TestSilenceSuppressesAlerts`, api `TestBrandingSettings`/`TestAuthPolicyAndAlertingAndDefaults`.

## D-0084 — SMTP settings group + live mailer (instance settings CUT 2)
**Context:** D-0083 shipped four instance-settings groups but deferred SMTP because it carries a secret. **Decision:**
add a `mail` JSONB group to `instance_settings` (migration `00032`) following the same pattern, with the SMTP
password **encrypted at rest** inside the JSON (`store.UpsertMail` encrypts, `GetInstanceSettings` decrypts the
`mail.smtp_password` field via `secret.Cipher`; `ReencryptSecrets` extended with a `jsonb_set` rewrite so key
rotation covers it). The **mailer is now live**: `mailer.NewLive(resolve func() Settings)` resolves its SMTP
endpoint per send from `settings.Service.Mail()` (effective = DB override if configured, else the config-file
`mail:` bootstrap), so changing SMTP in the UI applies immediately with no redeploy and no provider rebuild.
`resetEnabled()` / `/auth/config` `password_reset` now reflect `mailer.Enabled()` dynamically. API `GET/PUT
/api/v1/settings/mail` under `requireGlobalAdmin`: GET redacts the password (`smtp_password_set` + a `deliverable`
flag), PUT preserves the stored password on a blank submit (same pattern as OIDC). A new Settings → Email tab
edits it. **Consequence:** outgoing email (reset links, subscription mail) is configurable from the UI, keyed by
the same encryption keyring as every other secret. Covered by `domain` `TestMailSettingsValidateAndHelpers`,
`store` `TestMailSettingsRoundTrip`, `mailer` `TestLiveMailerResolvesPerSend`, api `TestMailSettings` (redaction +
preserve-on-blank + validation).

## D-0085 — Region-aware worker pools (geo-distributed probers)
**Context:** the infrastructure of some services lives in one geo, the cerbix core in another; internal
targets of the "far" geo need to be checked from inside it, without standing up a full cerbix there.
But today all workers concurrently pull from one shared `checks.jobs` queue (default exchange, no
routing key) — a plain worker would grab jobs of a foreign geo and fail on unreachable targets.
**Decision:** introduce a **region** dimension.
- **Monitor**: a `monitors.region` column (migration `00033`, `NOT NULL DEFAULT 'core'` + index),
  `domain.Monitor.Region` + `Normalize` (lower/trim, empty→`core`, composite→`core`) + `Validate`
  (slug `^[a-z0-9-]{1,40}$`, a composite outside `core` — an error). `DefaultRegion = "core"`.
- **AMQP** (`dispatch/amqp.go`): a per-region queue `checks.jobs.<region>` (`jobsQueueForRegion`).
  `PublishJob` routes by `job.Monitor.Region` (composite→`core`), idempotently declares the queue
  (`declareJobQueue`, cached — a publish to an undeclared queue via the default exchange is lost) and
  publishes with a **TTL ≈ the interval** (`Expiration`), so jobs of a region with no live worker expire
  instead of piling up. A worker, via `WithJobRegion(region)`, listens only to `checks.jobs.<region>`.
  `checks.results` — a single one. `scheduler.go`/`worker.go` unchanged (routing is localized in the dispatcher).
- **CLI**: a `--region` flag on `serve` (mirrors `--role`); passed through to AMQP only for the worker
  (a no-op for scheduler/api). The worker is **DB-less** (except composites → they run on `core`).
- **API/UI**: `region` in monitor create/update (validated by the domain, default `core`); a "Region" field in
  the monitor form (composite → locked to `core`).
- **Topology**: RabbitMQ — one central instance; only the `worker` is distributed; the network to the broker
  (WireGuard/amqps/amqproxy) is outside cerbix, delegated to the admins, with recommendations in `overview.md`.
**Consequence:** "just a prober in another geo" = `cerbix --role worker --region geoX` against the
central RabbitMQ, without a separate full cerbix and without a DB in the remote geo. Deferred (the design
allows it): an HTTP pull agent and RabbitMQ federation as alternative transports. Covered by `domain`
`TestMonitorRegionNormalizeAndValidate`, `dispatch` `TestJobsQueueForRegion`, `store`
`TestMonitorCRUDAndHeartbeats` (region round-trip + core default), api `TestCreateMonitorRegion`
(default/normalize/bad-slug/composite-auto-pin). Spec: `docs/specs/func-geo-worker-pools.md`.

## D-0085a — E2E verification of geo pools + region picker (addendum to D-0085)
**Verified E2E** on a distributed stand (`deploy/docker-compose.distributed.yml` +
`deploy/config.worker.yaml`): one RabbitMQ, roles `scheduler`+`api`+`worker --region core`+
`worker --region geo1`. Confirmed: each worker declares and listens only to its own queue
(`checks.jobs.<region>`); a `region=geo1` monitor is probed only by the geo1 worker; when the
geo1 worker is stopped, its jobs expire by TTL and the core worker does **not** pick them up (core keeps
serving its own); when the worker returns — the monitor resumes. **Region picker:** `GET /api/v1/regions`
(`store.ListRegions` — DISTINCT over monitors + always `core`; any authenticated caller) feeds a
`<datalist>` in the monitor form — one can pick an existing pool or type a new one. Covered by
`store` `TestMonitorCRUDAndHeartbeats` (ListRegions), api `TestListRegions`.

## D-0086 — Live-worker region picker + pre-create "Test connection"
**Context:** after geo pools (D-0085) the region in the monitor form was free-text input (a native
`<datalist>` — off-style), and there was no visibility into which region actually has a worker up; plus
there was no way to test a probe before creating it. **Decision:**
- **Live picker.** `GET /api/v1/regions` now returns `[{name, live}]`: regions in use
  (from monitors, always `core`) ∪ regions with a **live worker** — a consumer on `checks.jobs.<region>`,
  visible via the RabbitMQ **management API** (`internal/mqadmin`: `New`/`FromAMQP` derives the mgmt URL from
  the amqp URL — port 5672→15672, amqps→https:15671; `LiveJobRegions` filters `checks.jobs.*` with
  `consumers>0`). The api handler merges and flags `live`; the source is optional (`WithLiveRegions`, best-
  effort, on unavailability — `live=false`). Config: optional `rabbitmq.management_url` (otherwise derived).
  Frontend: a **custom combobox** (floating list, a green "worker live" dot, free-form input)
  instead of the `<datalist>`.
- **Test connection.** `POST /api/v1/projects/{id}/monitors/test` (editor+) — runs **one** probe of the
  submitted spec before saving, through the SSRF-guarded `prober.Runner` (`api.WithProber`), returning
  `{up, latency_ms, code, msg}`. The probe runs from the **central** process (for a geo target, core
  reachability may differ — caveat noted). Push/composite — not testable (400). A "Test
  connection" button in the form shows the up/down+latency+msg result.
- **MonitorDetail**: **Region** and **Tags** added to the Configuration panel.
**Consequence:** the region is picked from actually available pools (a worker appears once it connects to
RabbitMQ), and a probe can be verified before creation. Verified live: `/regions` returned `core`/`geo1` `live:true`
with the workers up; `test` → up on a live target, down with a message on a dead one, 400 on push. Covered
by `mqadmin` `TestFromAMQPDerivesManagement`/`TestLiveJobRegions`, api `TestListRegions` (shape),
`TestTestMonitor` (up/push-400/composite-400/viewer-403).

## D-0087 — "Test connection" executes in the target's region (RPC), strictly no fallback to core
**Context:** in D-0086 the test probe ran in the **central** process (`api.WithProber`), i.e. it measured
reachability from core — for a geo target (geo1's internal network, unreachable from core) that gave a
**false** result and contradicted the very point of geo pools (D-0085). **Decision:**
- **Region-aware RPC.** The probe is routed to the **spec's region** over RabbitMQ (direct reply-to):
  the API publishes a `CheckJob` to `checks.tests.<region>` with a temporary **exclusive/auto-delete** reply
  queue and waits for the response; that region's worker (`ServeTests`, listening on `checks.tests.<region>`) runs
  the same `prober.Runner` and publishes the heartbeat back to `ReplyTo`. Thus a geo target is tested **from its own
  region**. Interface `api.ProbeRunner`→**`RegionTester`** (`RunTest(ctx,m) (Heartbeat, error)`);
  `WithProber`→`WithTester`. Inproc (`--role all`) — a local adapter `localTester` (the region is cosmetic).
- **Strictly no fallback to core** (decision confirmed by the user). No live worker in the region →
  the `checks.tests.<region>` queue is auto-deleted → the publish is unroutable → the API times out and returns
  **502 "no worker responded in region …"**. A silent fallback to core is forbidden: it would yield a false DOWN
  for internal targets and would hide the real problem (a dead worker). Consistent with the strict geo
  affinity of production jobs (D-0085): a monitor is not picked up by a foreign pool. Timeout = `timeout+5s`
  (else 20s); a TTL (`Expiration`) on the request — against sticking. The UI highlights a non-live region up front.
- **502 semantics:** the result is "unknown" (bring up a worker `--region <name>`), **not** "the target is down".
**Consequence:** the test reflects the target's real visibility from its assigned region. Verified live (compose,
scheduler+api+worker-core+worker-geo1 against one RabbitMQ): `region=geo1` with a live geo1 worker →
the probe executed by it (UP on example.com, DOWN+msg on a dead tcp); after `stop worker-geo1` →
`checks.tests.geo1` disappeared, `region=geo1` → **502 "no worker responded in region "geo1""**;
`region=core` with a live core worker → a result. Covered by api `TestTestMonitor` (up/push-400/
composite-400/viewer-403/**502 no-worker**).

## D-0088 — "Region without a worker" alert (edge-triggered, with a grace period)
**Context:** strict geo affinity (D-0085/D-0087) means that when a region's worker dies, its
monitors silently drift into "no data" — the `checks.jobs.<region>` jobs expire by TTL and nobody
picks them up. There was no "region geo1 lost its worker" signal — an operational risk introduced by the
affinity itself. **Decision:**
- **A detector in the scheduler (leader).** A new tick (`regionWorkerEvery=30s`): the leader takes the live regions from
  the RabbitMQ **management API** (`mqadmin.LiveJobRegions` — consumers on `checks.jobs.*`) and calls
  `store.EvaluateRegionWorkerAlerts(ctx, live, grace)`. A region is "needed" if it has **enabled,
  non-push** monitors. **A lookup failure ≠ "no workers"**: on a mgmt error the tick is skipped (otherwise
  every region would be stormed).
- **Edge-triggered + latch (like burn alerts, D-0079).** A state table `region_worker_alerts` (per region):
  the row exists while the region is observed without a worker; `notified_at` is set at the moment of an actual
  send. The alert fires **once** on the transition, recovery — once when the worker returns.
- **Grace period (`regionWorkerGraceSeconds=90`).** The region must be observed without a worker for ≥ grace before
  an alert — suppresses false alarms on startup/rolling restarts (a brief worker reconnect). The first
  observation only starts the clock (INSERT, no alert).
- **Delivery — per-project** (notification channels in cerbix are project-bound): a new outbox topic
  `region_worker_alert` fans out per affected project with its monitor count; the outbox worker
  sends `DeliverProjectText`. Respects **global silence**. The topics' CHECK whitelist widened (migration 34).
- **Wiring.** `scheduler.WithLiveRegions(mqadmin)` — only for the distributed `--role scheduler`
  (inproc `all` co-locates the worker, doesn't connect). The mgmt client is built once in cli and reused
  by the api picker and the scheduler.
**Consequence:** a dead region worker is no longer silent. Verified: unit — store fire/latch/recover +
grace suppression (`TestEvaluateRegionWorkerAlerts`), outbox delivery + silence (`TestDeliversRegionWorkerAlert`),
domain `TestRegionWorkerAlertMessage`; live (compose scheduler+api+worker-core, a geo1 monitor without
worker-geo1) — the tracking row appears within a tick, after the grace a `region_worker_alert` fires into the outbox,
recovery when the worker returns. **Deploy nuance (found on the run):** roles must not apply migrations
in parallel — on the first deploy of a new migration, roles starting simultaneously race (`relation already
exists`); run `cerbix migrate` once before starting the roles (see overview §2.4).

## D-0089 — On-call / escalations (phase A of func-oncall-synthetic-pull)
**Context:** notifications were "flat" — a monitor's down went to **all** channels, repeats only via
`renotify_seconds`. No duty rotations (who is responsible right now), no escalation (raise it higher if nobody
reacts) and no ack (taken — stand down) → alert fatigue and "the incident hung until morning". **Decision (spec section A):**
- **Escalation policy** (per-project, JSONB steps like instance-settings): an ordered ladder
  `{after_seconds, targets[]}`; a target = `channel` **or** `schedule`. `RepeatLast` — repeat the last
  step at the `renotify` cadence until ack (open decision approved: yes, optional). `domain.EscalationPolicy.Validate`
  (non-decreasing offsets, non-empty targets) — the single owner of the invariants.
- **On-call schedule** (per-project): a rotation of channels, `OnCall(t)` — a **pure function of time**
  (`floor((t-anchor)/shift) mod N`, normalized for t before anchor). MVP: a single rotation.
- **Ack on the auto-incident** (`incidents.acknowledged_at/by`, `AcknowledgeIncident` is idempotent): ack and
  recovery/resolve **stop** the ladder.
- **The engine — a leader tick `AdvanceEscalations`** (edge-triggered + a latch on the incident, like burn/region alerts,
  `escalationEvery=15s`): open non-acked auto-incidents of monitors with a policy; fires every step
  whose offset from `started_at` has elapsed, resolving targets (channel + on-call `OnCall(now)`) into **concrete
  channel_ids** at fire time; latches `escalation_step`/`last_escalated_at`. A new outbox topic
  `escalation_step` → `notify.DeliverChannels` (respects global silence). The CHECK whitelist widened (migration 35).
- **Integration without duplicates:** for a monitor with a policy (+auto_incident) the **flat down-notify is suppressed**
  in the outbox (`TopicMonitorTransition`, `Cur=down`) — escalation carries the down alerts; **recovery (up) is sent
  as usual**. The flat `EnqueueRenotifyReminders` excludes monitors with a policy (the repeat is carried by the policy).
- **API/UI:** CRUD `escalation-policies`/`oncall-schedules` (editor+), `POST incidents/{id}/acknowledge`;
  a monitor carries `escalation_policy_id` (FK ON DELETE SET NULL). Frontend: an **Escalation** view (ladder +
  rotations), a policy selector in the monitor form, an **Acknowledge** button on the incident.
**Consequence:** a down reaches the on-call person, escalates if unanswered, ack silences it. Verified: unit —
`domain` (policy Validate, OnCall rotation/boundaries/TZ, Message), `store` `TestAdvanceEscalations`
(multi-step/on-call/latch/ack-stop) + `TestAdvanceEscalationsRepeatLast`, `outbox`
`TestDeliversEscalationStep` + `TestEscalationPolicySuppressesFlatDown`, api CRUD/ack; full `-race` over
29 packages; E2E (a monitor with a policy against a dead target → auto-incident → `escalation_step` in the outbox → ack
stops it). External on-call/chat systems as separate outbound integrations — out of scope (only via a webhook channel).

## D-0090 — Synthetic checks (scripted multi-step HTTP) — phase B
**Context:** the catalog was single-step probes; a lone `GET /health`=200 doesn't mean the user
flow (login→action→verify) works. **Decision (spec section B):**
- A new type `synthetic` — an **enriched http prober**, not a new genre: it breaks no invariants (static
  binary, DB-less worker, geo pools). The scenario (`domain/synthetic.go`: Step/Extract/Assert + Validate)
  is stored in `monitors.config["scenario"]` as JSON; the type is whitelisted by migration 37.
- The prober (`prober/synthetic.go`, Runner registry) runs the steps **sequentially** in a shared variable
  context: `extract` (from json-path/header/status/body) → `{{var}}` substitution into the url/headers/body
  of subsequent steps; `assert` (status/body_contains/json/latency_ms, op eq/ne/lt/gt/contains). It goes through
  the same **SSRF-guarded** client. The whole scenario runs within the `timeout_seconds` budget.
- **Redaction (NFR-SYN-2):** heartbeat.msg names only the step + the failed check (`step 2
  (profile): assert status eq 200 (got 401)`), never echoes bodies/headers — scenario secrets don't leak.
- `NeedsTarget=false` (the URLs live in the steps); monitor-level Conditions do not apply (the scenario has its own asserts).
- Frontend: a **Synthetic** type + a JSON scenario editor (first pass; a visual builder — later).
**Consequence:** we verify business flows, not just "the port is alive". Verified: unit — `prober`
(multi-step extract→subst→assert success/fail/missing-extract, jsonPath+subst), `domain`
(ParseScenario/Validate + Monitor.Validate); E2E (a 2-step synthetic → up, both asserts passed).
**Browser/headless — deliberately out of scope** (breaks the static image; see the spec's "out of this pass").

## D-0091 — HTTP pull agent (an alternative geo transport without RabbitMQ) — phase C
**Context:** a geo worker requires access to the central RabbitMQ; when exposing the broker to the geo is not an option,
a transport using only outbound HTTPS to the API is needed. **Decision (spec section C):**
- **Pull regions** are defined by the central config `pull.regions` (not a monitor field). For such
  regions the scheduler puts the `CheckJob` into a DB table **`pull_jobs`** with a TTL (~interval) instead of an AMQP publish
  (`WithPullRegions`); the other regions — as before, via the dispatcher.
- **The claim — `DELETE … WHERE id IN (SELECT … FOR UPDATE SKIP LOCKED) RETURNING`**: atomically takes+deletes,
  a job is delivered to exactly one agent, concurrent agents are safe; expired ones are not handed out (+`PurgeExpiredPullJobs`
  in the maintain tick).
- **Agent API** (bearer token `pull.token`, outside session auth, `AgentRouter`): `GET /agent/jobs?region&max`
  (claim), `POST /agent/results` (heartbeats → the same **ResultSink/ingest** as AMQP), `POST /agent/heartbeat`
  (liveness). An empty token → the endpoints are disabled (404).
- **The `--role agent` role** (`internal/agent`, DB-less/broker-less): a poll→`prober`→post loop + heartbeat; only
  ops endpoints; config `pull.server_url`+`pull.token`, the region from `--region`. Reuses `prober` wholesale
  (`allow_private_ips` for internal targets).
- **Pull-region liveness** — `agent_heartbeats` (upsert per region); `liveRegionsUnion` in cli unions
  {RabbitMQ consumers} ∪ {agent heartbeats within 45s} → feeds the region picker and the D-0088 alert (a mgmt error
  skips the tick; the agent part is best-effort). Also works in a pure-pull deploy (mgmt=nil).
**Consequence:** a geo without a broker — outbound HTTPS only. Verified: unit — `store`
(claim-once/region-isolation/TTL/purge, heartbeat live/stale), api `TestAgentEndpoints`
(claim/results→sink/heartbeat/401/404-disabled); **E2E** (single-process, a monitor region=pull1 → the scheduler
into `pull_jobs` → the agent container claim→probe→post: 7 heartbeats, pull_jobs drained, `/regions` pull1
live:true — while the inproc pull1 worker does NOT serve it, so everything went through the agent). Per-region token scope
and RabbitMQ federation — out of scope for the first pass (spec).

## D-0092 — Refinements to phases A/B/C: per-region agent token · on-call overrides · visual synthetic builder
Three refinements to what shipped (D-0089/0090/0091), each a self-contained pass:
- **Per-region agent token (C).** Previously a single shared `pull.token` authorized any region. Added
  `pull.agents: [{region, token}]` — a region-bound token: an agent holding it can claim/heartbeat
  **only its own** region (`agentAuthorized(bearer, region)`: catch-all `pull.token` ∨ region token). Results
  (`/agent/results`, no region) are accepted from any valid agent token (routing by monitor_id).
  Backward compatible: `pull.token` remains an optional catch-all. Covered by api `TestAgentPerRegionTokenScope`.
- **On-call overrides / vacation cover (A).** An `oncall_overrides` table (migration 38); `domain.OnCallOverride`
  + `OnCallSchedule.Overrides`; `OnCall(t)` — an active override (StartsAt≤t<EndsAt) **beats the rotation**
  (the function stays pure, overrides are loaded with the schedule). Store CRUD + loading in GetOnCallSchedule and in
  `AdvanceEscalations.loadSchedule`. API: `GET …/current` (who is on call now), `GET/POST …/overrides`,
  `DELETE /oncall-overrides/{id}`. Frontend: in the Escalation view — "on call now", a list of cover overrides + a form.
  "Multiple rotations" are already covered by multiple schedules (an escalation step targets the needed one). Covered by domain
  `TestOnCallOverrideWins`, store `TestOnCallOverrideStore`, api `TestOnCallOverrideAPI`.
- **Visual synthetic builder (B).** The JSON textarea replaced by a structured step editor (method/url/headers/
  body/extract/assert) serializing into the same `config.scenario` JSON and deserializing on edit. Validation in
  the UI (`scenarioError`) + backend Validate as before. The backend unchanged. (A nuance: `as` is a TS reserved
  word; renamed to `av` in the v-for.)
**Consequence:** geo agents are isolated per region, on-call handles vacations, synthetics are edited visually.
Full `-race` over 29 packages; `vue-tsc`+`vite build`; the earlier phases' E2E did not regress.

## D-0093 — Pull transport: observability, edge buffer, long-poll, scoping/DB tokens (P0–P3)
Hardening of the geo pull (on top of D-0091/D-0092) per the agreed roadmap; the implementation adopts the
clarifications discussed in review.

**P0 — observability.** Metrics `cerbix_pull_jobs_pending{region}` and `cerbix_pull_agent_lag_seconds{region}`
(gauges): the leader samples `store.PullQueueStats` every 15s (`SELECT count, EXTRACT(EPOCH FROM now()-min(created_at))`),
writes the snapshot into the registry and logs a WARN `pull_region_lagging` at lag ≥120s. **Priority goes by lag, not count**
(count is noisy; lag grows only when jobs really aren't being taken). Complements D-0088: that one catches "no agent", lag catches
"the agent is alive (heartbeat flowing) but isn't taking jobs". Paging — a Prometheus rule on `..._lag_seconds` (see overview).

**P1 — edge ring buffer (clarified: the buffer is ALWAYS historical).** When the API is unreachable, the agent puts the cycle's
results into a **bounded in-memory ring** (`bufferCap=10000`, dropping the oldest + WARN); on reconnect it flushes them
via **`POST /agent/backfill`** — a **historical bulk** (`InsertHeartbeatsBulk`, append-only into heartbeats, **bypassing**
reconcileTransition/incidents/escalations), so replaying old down→up **does not spawn a retroactive alert
storm**. Live status is driven only by fresh probes after the reconnect. **Idempotency:** a unique `(monitor_id, ts)`
(migration 39) + `ON CONFLICT DO NOTHING` in the live insert and the bulk → a replay from the buffer (partial success) doesn't
double-count SLA. To that end the live insert now sets `ts = the probe's time` (not insert time). Separate poll/heartbeat goroutines (the long-poll
doesn't starve liveness).

**P2 — long-poll (LISTEN/NOTIFY, not gRPC).** `EnqueuePullJob` sends `pg_notify('pull_jobs', region)`;
`store.PullNotifier` — **one** background LISTEN connection + an in-process hub of waiters (not LISTEN-per-request → no
pool exhaustion), reconnect with backoff. `GET /agent/jobs` on an empty queue holds the request until a NOTIFY or a
**max-hold of 20s** (insurance against a lost notify), then re-claims. Delivery is near-instant, the RPS to the DB → nearly zero,
the transport stays pure HTTP.

**P3 — hermeticization.** (1) **Result scoping:** the agent sends `?region=`; `/agent/results` and `/backfill` reject
(403) heartbeats whose `monitor.region ≠ region` (`MonitorRegions`) — a compromised region token cannot forge
a foreign region. (2) **DB agent tokens:** an `agent_tokens` table (migration 40, hash+region+revoke); `agentAuth` resolves
the token from the DB (`ResolveAgentTokenRegion`) in addition to the config tokens; an admin API (global admin)
`POST/GET/DELETE /api/v1/agent-tokens` — issue (the secret shown once)/list/revoke **without a redeploy**.

**Verified:** `-race` over 30 packages; unit — store (`PullQueueStats`, `InsertHeartbeatsBulk` idempotent,
`PullNotifier` with real LISTEN/NOTIFY, `AgentTokenLifecycle`), metrics emit, agent edge buffer (fail→buffer→backfill,
ring drop), api (long-poll re-claim, backfill, results region-scope 403, DB-token issue/claim/wrong-region/revoke).
The rollup test was fixed (it inserted physically impossible heartbeats with an identical ts). **Housekeeping:** `store.PurgeStaleAgentHeartbeats` in the leader's maintain tick cleans up the heartbeat rows of dead agents (every agent restart leaves a row under a new `agent_id`); the threshold is 1h — far beyond the liveness window, live agents are untouched. Covered by store `TestAgentHeartbeatLiveRegions` (purge <1h no-op / >1h removes the dead one, the live one survives). Left out of scope (per review):
a gRPC/SSE stream — not needed, long-poll covers it; the upgrade is only fair at thousands of agents / sub-second delivery.

## D-0094 — Test connection for pull regions (RPC over the pull transport)
"Test connection" for a monitor in a **pull region** (a geo without an AMQP worker, HTTP agent only) previously ended in a **502**:
the test RPC went via RabbitMQ direct-reply-to (`checks.tests.<region>`), and in a pull region there is nobody to subscribe —
timeout/no-consumer. The solution (agreed: "properly, RPC over pull"): **symmetry with production jobs** — the test
also rides the pull queue, not AMQP.

**Mechanism.** A separate table `pull_tests` (migration 41: `id uuid`, `region`, `payload jsonb`, `result jsonb`,
`claimed_at`, `expires_at`, `created_at` + an index `(region, created_at)`) — we do **not** reuse `pull_jobs`, because a
test has a different lifecycle (we wait for the result back, a TTL of seconds, one-shot). `store.EnqueuePullTest` (INSERT
RETURNING id + `pg_notify('pull_tests', region)`), `ClaimPullTest` (UPDATE `claimed_at` via
`... WHERE claimed_at IS NULL AND result IS NULL AND expires_at>now() FOR UPDATE SKIP LOCKED` — claimed once,
scoped by region), `SavePullTestResult`, `GetPullTestResult` (`DELETE ... WHERE result IS NOT NULL RETURNING result` —
an atomic fetch+remove), `PurgeExpiredPullTests` (in the leader's maintain tick, next to `PurgeExpiredPullJobs`).

**Transport.** The agent, in a separate loop (`testPollEvery=1s`, its own goroutine — doesn't starve live-poll/heartbeat), polls
`GET /api/v1/agent/tests?region=…`: it claims one test, runs the same `runner.Run(job.Monitor)` as a production job
(payload = `dispatch.CheckJob{Monitor}` — an identical format), and posts the heartbeat to `POST /api/v1/agent/test-results`
(`{id, result}`). Both endpoints sit under the same region-scoped `agentAuth` as the rest of the AgentRouter.

**Routing (API side).** `pullTester{store}` implements `RegionTester`: enqueue → polls `GetPullTestResult`
every 200ms up to `ttl = timeout+6s`, on timeout → `"no agent responded in region %q"` (no silent fallback to
central). `regionRoutedTester{pullRegions, pull, fallback}` directs the test: a region from `cfg.Pull.Regions` → `pullTester`,
otherwise → the AMQP tester. Assembled in the CLI only when there is both an AMQP dispatcher, a non-empty `pull.regions`, and a store; inproc
(`--role all`) as before → `localTester`. Thus the test executes on **the same agent of the same region** as the production probes —
consistent with D-0088 (strict no-core-fallback).

**Verified:** `-race` over 30 packages; store `TestPullTestLifecycle` (enqueue → result-not-ready → claim region-scoped/once →
save → get-consumed-once → expired-not-claimed → purge); api `TestAgentTestEndpoints` (claim via the AgentRouter → an empty
response for a region without a test → post-result 204). **E2E** (the distributed stack, a live geo2 agent): injecting `pull_tests`
for geo2 → the agent claims via HTTP `GET /agent/tests`, probes `http://api:8080/healthz`, posts `POST /agent/test-results`
→ the result `up:true, code:200` (previously — 502).

## D-0095 — Mail delivery reliability: timeouts, TLS 465, SMTPUTF8, async subscription send
Incident: subscribing to a status page (`POST …/subscribers`) failed in the UI (`ERR_SOCKET_NOT_CONNECTED`/`Failed to fetch`)
after configuring SMTP via the GUI. The investigation uncovered a chain of four independent defects; all fixed.

**1. No timeout on the SMTP dial (the handler hung).** `net/smtp.SendMail` connects with no timeout — an unreachable/
wrong SMTP host blocked the request until the OS TCP timeout (minutes). The send ran **synchronously inside the HTTP request**, so
the request never completed and the browser tore down the socket. Replaced `smtp.SendMail` with our own `sendMailTimeout`
(`net.DialTimeout` **10s** + `conn.SetDeadline` **30s** for the whole session) — a dead endpoint now returns an error quickly
instead of hanging.

**2. Port 465 (implicit TLS / SMTPS) was not supported.** The mailer only knew plaintext+STARTTLS (587/25). On 465
the server expects a TLS handshake while the client waits for the greeting → mutual deadlock → i/o timeout. Now on port 465 the connection
is wrapped in TLS **before** the SMTP greeting (`tls.Client`+`Handshake`); STARTTLS is skipped in that mode. The session went from
"hangs for minutes" → connecting in **0.56s**.

**3. Forced SMTPUTF8 broke forwarding (the mail bounced downstream).** `smtp.Client.Mail` adds the
`SMTPUTF8` parameter to `MAIL FROM` when the server advertises the extension — **even for fully ASCII envelopes**. The local Exim
accepted the message, but on forwarding to AWS SES (which doesn't support SMTPUTF8) delivery bounced with a permanent error.
Our addresses are ASCII → SMTPUTF8 isn't needed. Replaced `c.Mail(from)` with a manual `mailFrom` (via the exported
`c.Text *textproto.Conn`) that sends `MAIL FROM:<…>` **without** SMTPUTF8 (keeping `BODY=8BITMIME` when advertised, +
a guard against CR/LF injection in From). This is an app-side fix (preferred per the mail admin's advice; the server-side workaround —
`utf8_downconvert=-1` in the Exim transport — was not needed).

**4. A synchronous send in the request path → async via the outbox.** The confirmation email is now **not** sent inline:
the handler renders the subject/body (it needs the confirm token + the public base URL) and puts a `subscriber_confirm` event into the
**existing transactional outbox** (`store.EnqueueOutbox`; the topic added to the whitelist CHECK by migration 42; the worker
`outbox.Worker` gained `WithMailer` + a case and sends via the mailer with the usual retry/backoff/dead-letter). Instance silence
does **not** mute the confirmation (it is transactional, not an alert). Subscribe returns `202 pending` in ~5ms regardless of
SMTP speed; a transient mail failure self-heals through retries. A clear error instead of `500 internal error`: when the
queue is unavailable — `503 "could not send confirmation right now, please try again later"`.

**Verified:** `-race` over 30 packages; api `TestSubscribeConfirmUnsubscribe` (the email **is enqueued**, not sent inline;
a confirmed re-subscribe doesn't spawn emails), `TestSubscribeDisabledWithoutMailer` (503); outbox `TestDeliversSubscriberConfirm`
(delivery via the mailer; silence doesn't mute it; without a mailer — fail→retry). **E2E** (live stack): the 465 SMTP connects in
0.56s (TLS handshake OK), subscribe → `202` in ~5ms. **Remaining on the configuration side** (not code, handed to the user):
`From=cerbix@example.com` is rejected by the server (`550 Sender verify failed` — the mailbox doesn't exist) → set a real
deliverable From; `PublicBaseURL=http://localhost:8082` produces non-working links in emails → set the external URL
(e.g. `https://status.example.com`) in the settings.

## D-0096 — Heartbeats: adaptive TimescaleDB hypertable + native compression
A "for growth" measure for the fastest-growing table. The decision was preceded by an analysis of alternatives
(a homegrown TSDB engine; hybrids with ClickHouse / VictoriaMetrics / QuestDB; InfluxDB; OpenTSDB) —
all of them lose not on the time-series part but on the **relational half of the problem**: SLA queries anti-join
heartbeats × maintenance_windows × monitors, heartbeat+status+outbox are written in a single transaction,
idempotency rides `ON CONFLICT (monitor_id, ts)`, FK cascade on monitor deletion. A hybrid breaks the
atomicity and requires rewriting the SLA layer; our load (~330 insert/s at 10k monitors×30s)
is trivial for PG. Bottom line: PG remains the only store, Timescale kicks in **adaptively**.

**Why adaptive rather than mandatory:** managed PG (AWS RDS, Cloud SQL) doesn't support
the extension at all — a hard requirement would cut them off for future OSS users; our CI runs
the tests on plain `postgres:16-alpine`. Licensing: compression is the Community edition (TSL),
free for self-hosted; cerbix (MIT) is unaffected. This was planned
in D-0017/iter-0033.

**Mechanism.** Migration 00043 (`-- +goose NO TRANSACTION`, a guarded DO): if `pg_extension`
contains timescaledb and heartbeats is not yet a hypertable — a table rebuild (native partitions
cannot be converted in place): a twin table → `create_hypertable(chunk=1 day)` → UNIQUE
(monitor_id, ts) → copy → swap; then `timescaledb.compress` (segmentby=`monitor_id`,
orderby=`ts DESC`) + `add_compression_policy(compress_after => 7 days)` — the window is deliberately wider
than the age of the agent's edge buffer, and inserting into compressed chunks still works (TS≥2.11).
Otherwise — a NOTICE and a no-op: we stay on declarative partitions. The migration does **not** run
`CREATE EXTENSION` itself: without preload that is a FATAL that kills the connection. Down — the reverse conversion
into the declarative schema of 00017.

**Runtime.** `Store.detectTimescale` at Open (pg_extension → timescaledb_information.hypertables);
`EnsureHeartbeatPartitions` in hypertable mode is a no-op (chunks are on-demand), `PurgeOldHeartbeats` —
`drop_chunks(older_than => cutoff)`; retention remains driven by the `heartbeats.retention_days` config
via the leader's maintain tick (not a TS retention policy — no config synchronization needed). Signatures
unchanged → scheduler/CLI untouched; all SELECT/INSERTs over heartbeats — without a single edit.

**Verified:** `-race` over 30 packages; the whole store suite in both modes — hypertable (cerbix_test on the
timescale image; the extension is inherited from template1) and declarative (a one-off
postgres:16-alpine = CI); new tests `TestHypertableRetention` (ensure=no-op, an on-demand chunk
for a 40-day backfill ts, idempotency, drop_chunks purge) and
`TestHypertableCompressionRoundTrip` (compress_chunk → read/duplicate/insert through compression);
the existing partition tests skip in hypertable mode and vice versa. **E2E on the live dev DB:**
44006 rows converted in 412ms with no loss, 4 daily chunks, the 7d policy registered,
a manual compress of old chunks: **6.2 MB → 344 KB (17.6×)**, reads through compression are correct;
a live `--role all` smoke on the converted DB — 14 fresh heartbeats/min landed in the auto-created
today's chunk.

**Revisit trigger (a dedicated TSDB alongside — VM/QuestDB, for metrics, not state):**
>100k monitors, intervals <10s, raw retention >1 year, or high-cardinality
per-request metrics. **Out of scope:** a continuous aggregate replacing heartbeats_daily (the
D-0017 follow-up), a TS retention policy.

## D-0097 — Incident context: heuristic RCA without AI (iter-0037)
A package of reliability features (the specs are split across iterations 0037–0040, ordered easy to hard;
geo quorum deliberately postponed until a discussion after the package). The first feature: an **auto-incident**
automatically gets one system timeline entry attached — a correlation summary instead of a manual
"what else went down": "⚡ Context: N other monitor(s) failed within ±5m (…); dominant error: …;
all failures in region …". The decision is "a heuristic, not an LLM": the data is already rich (msg/code/region/
co-failures), the desired result is deterministic, zero external dependencies and privacy questions;
LLM summarization is a separate opt-in decision for later.

**Mechanism.** domain: a pure `ClassifyProbeError(msg, code)` (8 classes, patterns > code),
`IncidentContext.Render()` with the `⚡ Context:` marker (= the idempotency key), the RootCause field
reserved for the dependency graph (iter-0040). store: `IncidentContext` — a single
bounded ±5m window query (cap 2000 rows) over the failing heartbeats of the project's monitors,
aggregation in Go; `AppendIncidentContext` — an idempotent INSERT (`WHERE NOT EXISTS body LIKE
marker%`), author=system, the status inherited from the incident. outbox: in `TopicIncidentEvent`
after a successful webhook delivery for `opened`+`source=auto` — a best-effort attach (a context
error → WARN, the event stays delivered; a re-delivery doesn't duplicate the entry).

**Verified:** unit domain/store/outbox (the classes, the window, the single-region hint, idempotency,
manual incidents get no context, a webhook failure → the attach is postponed); `-race` over 30 packages;
E2E — 3 monitors against a dead port → 3 auto-incidents, each with exactly one correct summary
(refused, region core). The SPA unchanged (the existing timeline).

## D-0098 — Multi-window multi-burn-rate alerts (iter-0038)
A refinement of D-0079 up to the Google SRE canon. A single window+threshold either is noisy (a short window)
or lags (a long one); the canon is a **pair of windows**: a rule fires when burn ≥ threshold in both
(long filters noise, short confirms "right now"), plus a page/ticket severity. The UI
was implemented against a pre-agreed SPA mockup (artifact before code).

**Mechanism.** `sla_targets.burn_rules jsonb` (migration 44; legacy → a single page rule with
short=long — a degenerate pair, the previous behavior; the old columns dropped). domain:
`BurnRule`+`Key()`, `ValidateBurnRules` (≤4, page|ticket, thr>0, 1m≤short<long≤7d),
`DefaultBurnRules` = page 14.4×(1h∧5m) + ticket 6×(6h∧30m). store: an upsert that carries the
firing latches over for unchanged rules (matched by Key; a configuration change resets the latch),
enable with nil rules seeds the defaults; `EvaluateBurnAlerts` — per-rule pairs of maintenance-
excluded counts, edge/latch per rule, an outbox event with the severity and both windows.
api: `burn_rules` in PUT (nil=keep/seed, []=off) with domain validation; `GET /sla` returns
the rules, `burn_firing` = any-rule. SPA: an inline rule editor in the expanded row of the SLO
table (severity/threshold/windows from a fixed set/state), "Reset to SRE defaults", 🔥/⚠️ badges
by the worst firing severity.

**Verified:** store `TestBurnRulesMultiWindow` (AND semantics: long-only does NOT alert; both →
one alert with attribution; latch carry-over/reset; defaults), the rewritten `TestEvaluateBurnAlerts`
(fire→latch→recovery); `-race` over 30 packages; vue-tsc+build; E2E on the live DB (migration 44,
PUT round-trip, 400 on short≥long, GET with the rules). Out of scope: severity→escalations,
project-level burn rules.

## D-0099 — Confirm phase: accelerated failure confirmation (iter-0039)
With confirmations, time-to-alert was `interval × failure_threshold` (~3 min at 60s×3).
The decision: accelerate **only the confirmation phase** (first counted failure → verdict/reset),
not a "flapping mode" — a constant speed-up would finish off a degrading service. The UI — per a
pre-agreed mockup.

**Mechanism.** `monitors.confirm_interval_seconds` (migration 45; default 10, 0=off, clamped
to [5s, interval] in Normalize, push/composite → 0; active at threshold>1).
`RecordCheckStatus`, when incrementing the counter without a verdict, sends `pg_notify('monitor_confirm')`
in the same transaction; `store.ConfirmNotifier` (a LISTEN hub modeled on PullNotifier) wakes the
scheduler leader: `nextRun` is pulled forward to the confirm interval (never pushed back), the
acceleration lives for one base interval after the last signal (recovery → the signals
stop → it decays; the snapshot refresh cleans up authoritatively and serves as a fallback for a
missed notify — `consecutive_failures` was added to the monitor selection).
Confirm-phase pull jobs carry a short TTL. **A cap `confirmCapPerRegion=50`** — anti-
thundering-herd: on a mass outage the region stays on its normal rhythm (WARN).
SPA: a field in the form's Reliability block + a "Confirming N/M…" pill (degraded style) in the list.

**Verified:** the domain clamp; store — real LISTEN/NOTIFY (a signal on fails 1..N-1, silence
on the verdict/recovery); scheduler — acceleration by the signal (1h → ~1s) and the cap; `-race` over 30 packages;
vue-tsc+build; **E2E: down in 25s instead of ~180s** (probe→verdict 11s; 60s×3, confirm 5s).

## D-0100 — Monitor dependency graph + cascade suppression (iter-0040)
The finale of the 0037–0040 reliability package. The pain: a database goes down → alerts for every dependent
service. A monitor declares its parents (`depends_on`, an M2M within its own project, a DAG); while any
transitive parent is down — the **delivery** of the child's alerts is muted, the facts are recorded honestly.
The key principle: "parent/child" is not a property of a monitor but the direction of an edge; the root
of the cascade is computed at the moment of each event (`DownAncestors`: a recursive CTE upward,
ancestors that are down or have an open auto-incident, cap 10).

**Mechanism.** Migration 46 (`monitor_dependencies`, CHECK self, FK CASCADE);
`ReplaceMonitorDependencies` (same-project + a cycle → typed errors → 400);
`depends_on` — a correlated aggregate in monitorColumns. outbox: a child's down transition and firing
burn alert with a down ancestor → a delivered no-op + a log + an idempotent ⏸ note in the
timeline; **fail-open** (a lookup error does not mute the alert); **recovery is never
suppressed**. `AdvanceEscalations` skips suppressed incidents (the ladder freezes until the
ancestor's recovery). The RCA context (D-0097) fills in the "likely root cause". SPA per the
agreed mockup: a multi-select with a client-side cycle check, a
"⏸ suppressed by <root>" badge, a Dependencies block in the details.

**Verified:** store (round-trip, foreign/self/transitive cycle, DownAncestors, the cascade,
the idempotent note); outbox (suppressed/recovery/burn/fail-open); `-race` over 30 packages;
vue-tsc+build; **E2E: parent+2 children → exactly one alert**, 2× suppressed in the log,
⏸ notes, the root cause in the context, a cycle via the API → 400. Limitations: the
"parent went down later" race — best-effort in v1; depth 10.

## D-0101 — Multi-region quorum via composite, option B (iter-0041)
Geo quorum was discussed and implemented as a **composition of existing primitives**, not a new model.
**Option A rejected** (per-monitor `regions[]`): it requires a region column in heartbeats with a
break of the `(monitor_id, ts)` unique key — a second migration of the hot hypertable in a row,
N status machines in reconcile and a rethink of SLA semantics (what is availability at 1-of-3?);
XL-sized while the pain of false positives from a single vantage point is already closed by
threshold+confirm phase (D-0099). **Option B**: N single-region monitors
(`name @ region`, no channels, auto_incident=off) + a composite with a new mode
`mode=quorum, config.quorum=M` — the composite does the alerting.

**Semantics:** the composite is DOWN only when ≥ M children are not-up ("2 of 3 regions confirmed
the failure"); a pending/deleted child = a down vote (consistent with all). The children are probed
each from its own region at its own rhythm (the phase spread is a feature: the quorum runs over fresh statuses);
per-region SLA is the children's SLA, heartbeats untouched. Msg with a tally:
`"2/3 children down (quorum 2)"`. Validation: 1 ≤ M ≤ len(children).

**The "Multi-region set" wizard** — pure frontend sugar (the backend only knows the mode):
chips of live regions, M=majority by default, a creation plan in the form, N+1
API calls with a rollback of what was created on error. A `quorum M/N` badge in the list.

**Verified:** domain/prober units (M boundaries, the tally, M=1≙all, missing=down vote);
`-race` over 30 packages; vue-tsc+build; E2E: 1/3 down → up, 2/3 down → down with the tally,
M>N → 400. **Revisit triggers for option A:** mass multi-region public targets
for which per-region timelines/SLA are needed within one monitor, or an OSS positioning
that requires per-monitor multi-region out of the box.

## D-0102 — Members lives in Settings; the global-admin group is "Administration" (iter-0042)
The sidebar section "Manage" held exactly one item (Members) while the screen itself was already
org-scoped end to end (`ws.orgId` drives every call), i.e. it *is* organization settings — so it
moved into Settings as the first tab of the Organization group, next to API tokens and Webhooks,
and the one-item sidebar section was removed. The instance-wide tab group is renamed
**Administration**: the old label "Instance" described where a setting lives, the new one describes
who uses the group — and with the Users page (D-0103) it stops being purely configuration. The
scope *key* stays `instance` in code (display-only rename, zero data/API impact). Deep links:
`SettingsView` reads `?tab=<key>` (validated against the tabs visible to the current user) and
`/members` redirects to `/settings?tab=members`, so old bookmarks survive. The panel itself was
extracted as a self-loading `MembersPanel.vue` instead of inlining ~370 lines into the already
monolithic SettingsView — the pattern for every future heavy tab (Users follows it in iter-0043).
Frontend-only: all four `/organizations/{orgID}/members` endpoints are untouched.

## D-0103 — Instance-wide Users administration for the global admin (iter-0043)
Users only surfaced through org member lists (`ListOrgMembers` INNER JOINs `memberships`), so an
OIDC JIT-provisioned account with no membership could sign in yet was invisible on every screen —
exactly the account an admin most needs to see. The new Settings → Administration → Users page
lists **every** user (user-keyed `AdminUser` DTO with aggregated memberships — one row per user,
unlike the membership-keyed `Member`) and manages them: global-admin toggle, add-to-org, delete.
Guard rails live server-side: you cannot change your own flag or delete yourself (anti-lockout),
and the last global admin can be neither demoted nor deleted (`CountGlobalAdmins`). Deletion is a
plain `DELETE FROM users` — every FK already cascades and the audit actor is `SET NULL`; deleting
the sole org_admin of an org is deliberately allowed (the global admin appoints a replacement).
Global actions are audited with `org_id = NULL` (migration 00047 relaxes the NOT NULL; empty-org
maps to NULL in `RecordAudit`, mirroring the NULL-actor pattern) — recorded now, a viewer UI can
come later. "Add to org" reuses `POST /organizations/{orgID}/members` with `user_id` instead of a
new endpoint, grants org-scope roles only (`project_admin` stays in the Members project-scope
flow), and no invitation flow was added: users exist only after their first sign-in.

## D-0104 — The last-org-admin guard does not bind a global admin
`isLastOrgAdmin` (400 on demoting or removing an org's sole org_admin) exists to keep an org
manageable from the inside. After D-0103 it became incoherent: a global admin could delete the
same user entirely from the Users page (memberships cascade, the org loses its only org_admin),
yet removing just the membership was refused. The guard's real invariant is "the org must not be
locked out of management" — and for a global admin it never is, because global admins manage every
org and can appoint a replacement. So both member endpoints skip the check when the principal is a
global admin and keep it for org_admins acting within their org (they *would* lock the org out).

## D-0105 — Build version in the SPA sidebar
The running build is now visible in the product: `GET /api/v1/version` (session-auth, deliberately
NOT public — version disclosure on an unauthenticated endpoint is a fingerprinting gift) returns
`buildinfo.Current()` (version/commit/go, already injected via ldflags by the Dockerfile and the
release workflows), and the SPA sidebar footer shows `cerbix <version>` with the commit in the
tooltip. Fetched once per session (cached in the session store). Dev builds honestly show `dev`;
tagged images get the real tag/sha from the GHCR workflow build-args.

## D-0106 — Audit-gap package: fix the config docs first (iter-0044)
The 2026-08 audit found zero dead config fields but real documentation drift, and with the strict
loader (`KnownFields(true)`) drift is not cosmetic: `oidc.admin_emails` from docs/overview.md does
not decode (real key: `bootstrap_admin_emails`), and `admin_email/password` were filed under
`local:` while living under `security:` — both copy-paste paths ended in a startup failure. Fixed
the table, added the missing `pull` section row and `local.login_rate_limit_per_minute` to
config.example.yaml (the only undocumented working key), and repaired the runnable worker command
(missing `serve`, missing `agent` in the role list). The package plan lives in
specs/func-audit-gaps.md; iterations run easiest → hardest.

## D-0107 — Auth housekeeping joins the leader maintenance tick (iter-0045)
The audit found two unbounded tables: expired `sessions` rows and abandoned `auth_flows` were
never deleted — the purge functions existed, were tested, and had no caller. They now run in the
scheduler leader's hourly maintenance tick next to partition/retention/pull-queue housekeeping
(warn-on-error: maintenance is never fatal to scheduling). Also dropped `ListMembershipsByOrg`
from the api Store interface — zero callers since `ListOrgMembers` took over; the store method
itself stays for tests.

## D-0108 — Minimum password length reads the live policy (iter-0046)
`auth_policy.min_password_len` was the only auth-policy field enforced from the startup config
instead of the settings snapshot: the UI saved it, nothing read it. Both enforcement points now
resolve the live value (api password change via a small `effectiveMinPasswordLen()` accessor —
the settings service when wired, else the config value; reset-confirm via the existing
`authPolicy()` fallback chain). YAML stays the pre-first-save seed, matching the resolver
contract everywhere else. No migration; instant effect on save.

## D-0109 — Render what the API already serves (iter-0047)
The audit's largest class was data served but never rendered. Now shown: HTTP method (the detail
page hardcoded "GET" for every http monitor — actively misleading for POST/HEAD checks), the full
alerting config on the monitor card (failure threshold / confirm / re-notify / push grace /
escalation policy / updated_at), who acknowledged an incident (new `acknowledged_by_name`
enrichment on the detail endpoint — the raw field is a UUID), the incident → monitor link and the
Alertmanager `external_key`, and who issued each API token/webhook (`created_by_email`, resolved
in the list handlers). Two dead store states got their consumers: `live.connected` drives a
header "reconnecting" chip (with a new `started` flag so unsubscribed pages stay quiet) and
`workspace.loading` drives an org-switcher skeleton. Enrichment is handler-level and best-effort —
deleted users resolve to nothing rather than failing the request.

## D-0110 — Branding logo becomes real (iter-0048)
`logo_url` was the worst of the dead-settings class: settable only via curl, publicly served,
rendered nowhere. It now has a form input and three consumers — sidebar, sign-in card, public
status-page header — each with the existing accent glyph as the empty-value fallback, so default
installs look exactly as before. The sign-in card also stops hardcoding "cerbix" and uses the
branding product name.

## D-0111 — Push endpoint panel on the monitor page (iter-0049)
A push monitor's entire contract is "POST this URL or be declared down", and the SPA never showed
the URL. The detail page now presents it front and center for push monitors — copyable URL, cron
one-liner, and the up/down rule spelled out (interval + grace). No backend change: the token was
already served; this closes the audit's top finding.

## D-0112 — Silence expiry is settable where the silence is (iter-0050)
`global_silence.until` was HIDDEN (backend honored it, UI couldn't set it) with a data-loss twist:
the UI's toggle-only PUT decoded into a fresh Alerting object and nulled an API-set expiry. The
Alerting tab now exposes an optional Until field and always round-trips the full object. Facts
keep recording regardless — only delivery is muted, per the outbox suppression contract.

## D-0113 — Pause, don't delete: webhook/channel toggles (iter-0051)
The Status column implied an ability the API did not have. Both resources now take a PATCH
`{enabled}` (delivery already filtered on the flag — the change is pure management surface), so a
noisy integration is paused without losing its signing secret or config. Webhook toggles are
audited (`webhook.toggle`); channels follow project-write like the rest of channel management.

## D-0114 — Status-page preview that opens, components that group (iter-0052)
The default-created status page had a broken preview link (internal → public 404) and the grouping
feature existed only for curl users. Preview now rides the previously-unused authed render
endpoint via `?preview=<id>` (with an explicit members-only banner; the public endpoint still
404s anonymously), unlisted links carry their token, and the component form finally exposes
group/description/position — with `description` added to the render DTO so it can actually show.

## D-0115 — Agent tokens get an admin surface (iter-0053)
DB-managed pull-agent tokens (D-issued for broker-less regions) had API+spec and no UI. A new
Administration tab lists/issues/revokes them, with the show-once secret contract shared with API
tokens and revoked rows kept visible as history. Pure frontend — the endpoints were already
guarded by requireGlobalAdmin.

## D-0116 — Owners can finally see their subscribers (iter-0054)
The subscribe flow wrote rows nobody could read back: no list endpoint, no UI. The status-page
editor now shows the audience (count, confirmed vs pending-confirm — the latter doubling as an
SMTP-deliverability signal) and lets an org admin remove an address; the confirm token stays
server-side. This closes the audit-gap package: all eleven findings of the "saved/served but
never used" class are resolved (D-0106…D-0116).

## D-0117 — Package 2 opens with the paper cuts (iter-0055)
The re-audit (three sweeps after D-0106…D-0116) confirmed the first package closed and surfaced a
second layer (spec func-audit-gaps-2). Its easiest slice: overview.md misstatements (agent role
needing RabbitMQ — it is broker-less by definition; an inline command that exits 2; missing
--region/dsn notes; mail key notation), the scheme-vs-port management-URL comment, the dead
OIDCSettings.UpdatedAt scan, and explicit test-support labels on the four caller-less store
functions so they stop tripping audits.

## D-0118 — Feed links follow the page's visibility (iter-0056)
Fixing the preview in D-0114 but not the feed links left half the job: RSS/Atom/JSON still 404'd
for internal/unlisted pages. Both link builders now mirror the preview logic — authed feed
endpoint for internal (session-gated), token-carrying public URL for unlisted.

## D-0119 — The audit window widens on demand (iter-0057)
A hard 30-row cut with no affordance is indistinguishable from "that's all there is". The list now
steps 30 → 100 → 500 via an explicit Show-more; 500 is the backend cap, so deeper forensics stays
a DB query by design.

## D-0120 — Escalation progress is visible where it matters (iter-0058)
The columns existed since 00035; only the read path was missing. The incident now carries
step/last-escalated, and the detail header shows the pill exactly in the window where it informs a
decision (open + unacknowledged + step>0).

## D-0121 — Monitor defaults reach the UI path; zero becomes a real value (iter-0059)
The zero-sentinel pattern in the create body made two different intentions collide: "use the
instance default" and "explicitly zero". retries/renotify_seconds switch to pointers (absent →
default, 0 → 0), and the form now prefills new monitors from a lightweight authed
/api/v1/monitor-defaults instead of a hardcoded copy — so the Settings page finally shapes what
operators create. Interval/timeout/failure_threshold keep the zero-sentinel: zero is not a valid
value for them, so the sentinel is unambiguous there.

## D-0122 — Escalation objects become editable; deletes state their blast radius (iter-0060)
"Fix a typo by deleting the schedule" destroyed its overrides and detached monitors without a
word. The composer now doubles as the editor (POST↔PUT switch), and both deletes require an
inline confirm that quantifies the consequence. Frontend-only: the PUT endpoints were waiting.

## D-0123 — require_totp scope: local sign-ins, by contract (iter-0061)
The re-audit flagged that SSO logins bypass the TOTP policy. Considered enforcing it after the
OIDC callback and rejected it: the IdP is the authority on how its users authenticate (Keycloak
et al. enforce MFA natively), and a second, application-level TOTP on top of SSO is UX-hostile
duplication. The scope is now written down (domain doc + the Settings hint) instead of implied,
and the previously unhandled totp_setup_required lockout finally tells the user their options.
This closes func-audit-gaps-2 (D-0117…D-0123).

## D-0124 — A committed E2E suite replaces ad-hoc scripts (iter-0062)
Every iteration was E2E-verified with throwaway Playwright scripts; the knowledge kept evaporating.
`e2e/` makes it durable: dockerized Playwright against a live stack, one shared authenticated
session (local logins are rate-limited — per-test logins are the primary flake source, and
`/auth/logout` from a shared-session spec kills the whole run), self-cleaning `e2e-`prefixed data,
23 tests over auth/monitors/incidents/escalation/settings/admin/status-pages/OIDC. Deliberately
not wired into CI: the suite needs a live stack and the CI policy is build-only (D-0087-era);
`./e2e/run.sh` is the local/pre-release gate.

## D-0125 — E2E coverage expansion (iter-0063…0066)
The suite grows 23 → 34 tests along the priority ladder from func-e2e-coverage: Alertmanager
ingest, SLA editor, probers exercised against the stack itself (conditions engine both ways,
instant test-connection, composite quorum), mail flows through a Mailpit sidecar (new dev-compose
profile `mail`), and time-based flows — measured confirm-phase acceleration and a full TOTP
lifecycle with RFC 6238 generated in-test. Region-affinity assertions are transport-aware by
design: the inproc dev dispatcher ignores regions, AMQP/pull enforce them. Runtime ~2min; still a
local/pre-release gate, not CI.

## D-0126 — In-flight results for deleted monitors are a fact of life (iter-0067)
The scheduler-snapshot design (D-era refresh ≤15s) means results can always arrive after a
monitor's deletion; that is a benign race, not a failure. The store now names it (FK 23503 on the
monitor FK → ErrNotFound) and ingest logs one INFO instead of ERROR-per-probe. The FK also loses
its heartbeats_new_ prefix (00043 swap leftover) via guarded migration 00048.

## D-0127 — The log tells the story, not just the failures (iter-0068/0069)
An operator could not reconstruct events from cerbix logs: no requests, no actors, no
transitions. Added an access-log middleware (API/auth at INFO, static at DEBUG, SSE excluded),
principal-stamped lifecycle events for every mutation (logEvent helper — the operator-side twin
of the audit trail), monitor_status_changed at the ingest transition, outbox_delivered on
success, and context.Canceled demoted out of the ERROR stream. log.level stays the single knob;
request IDs/tracing remain a future OTEL story.

## D-0128 — SSE keepalive the client can see (iter-0070)
Comments keep proxies happy but are invisible to EventSource JS, which made silent socket death
undetectable and any silence-based watchdog a flap machine on quiet systems. The keepalive is now
a real ping event and the client recreates the stream after two missed pings — the existing
reconnecting chip covers the gap. Native EventSource retry still handles the error-ful cases.

## D-0129 — The AMQP link heals itself (iter-0071)
Runtime broker loss used to be a silent, permanent consumer death behind a green /healthz. A
NotifyClose-driven supervisor now redials with capped backoff and resubscribes every consumer;
internal forwarding channels never close, so roles ride outages out without restarts. Logging is
state-transition (one broker_lost, one broker_reconnected, sparse progress lines) and the
scheduler compresses outage publish failures to an aggregated line per 10s — the reviewer's
"rate-limited logging" concern falls out as a side effect of fixing the real defect.

## D-0130 — Email channels use the timeout-bounded SMTP sender (iter-0072)
The outbox worker is a single goroutine; raw smtp.SendMail with no dial timeout let one dead
email channel freeze all alert delivery for ~130s per event. notify now reuses mailer's
timeout-bounded sender (the fix already shipped there for the subscribe/reset flows). First fix
of the func-hardening package from the deep audit.

## D-0131 — AMQP resilience covers channel death, not just connection loss (iter-0073)
The D-0129 supervisor closed connection-level loss but a channel-level death (queue deleted,
basic.cancel, 4xx, ack error) still parked consumers forever behind a live connection — the same
silent-death symptom. The consume/serve-tests loops now also retry on a short backoff independent
of the reconnect signal, and consumeOnce re-declares its queue every session so a deleted queue is
recreated. Also fixed the Close()/redial data race and a nil-logger panic path.

## D-0132 — Backfill tolerates deleted monitors; status race is quiet (iter-0074)
The InsertHeartbeat 23503→ErrNotFound fix (D-0126) missed the bulk backfill path (a batch aborts
wholesale on one FK violation → permanent backfill wedge and SLA loss) and the status-flip path
(logged ERROR). Bulk now pre-filters by the live monitor set (self-heals on retry); ingest logs
the status race as INFO. Same benign race, same quiet handling everywhere now.

## D-0133 — Every agent ingest is region-scoped; channel secrets are redacted (iter-0075)
Two boundary leaks from the deep audit. (1) The agent transport accepted region-less
`/results` and `/backfill` posts, authorized them with ANY configured per-region (or DB)
token, and skipped `enforceRegionScope` when the region was empty — so a token minted for
one region could forge heartbeats for any monitor in any other region; `/test-results` had
no region check at all, letting a valid agent poison another region's test result by
guessing its id. Every agent endpoint now requires a region: `agentAuthorized`/
`agentDBAuthorized` no longer honor the region-less path (only the instance-wide catch-all
token can even reach a handler region-less, and the handler still rejects it), and
`SavePullTestResult` scopes its UPDATE to the token's region. The real agent already sends
`?region=` on results/backfill; test-results now sends it too. (2) Notification-channel
`config` (decrypted bot tokens, SMTP passwords, secret-bearing Slack/webhook URLs) was
returned verbatim by the viewer-readable list endpoints. `NotificationChannel.Redacted()`
blanks `SecretChannelConfigKeys` and is applied to list and create responses; there is no
config-edit flow, so nothing round-trips the blanked values.

## D-0134 — Maintenance mutes alert delivery, not the transition record (iter-0076)
Maintenance suppression lived at enqueue: a monitor that went DOWN during a window had
its transition event dropped and its `last_notified_at` left NULL, so `EnqueueRenotifyReminders`
(which requires `last_notified_at IS NOT NULL`) never picked it up — the monitor stayed
silently down forever, even after the window closed. This contradicted the spine principle
that facts/events always record and only DELIVERY is muted (the escalation, dependency, and
instance-silence suppressions all already work that way). `RecordCheckStatus` now enqueues
the transition on every flip and stamps `last_notified_at` on a fresh down regardless of
maintenance; the outbox worker mutes the DOWN notify at delivery via
`MonitorInMaintenance` (fail-open on error, recovery never muted). A still-down monitor is
then re-alerted by the first renotify reminder that fires after the window. Incident
opening stays gated on the returned suppressed flag (maintenance shouldn't spawn incidents).
Caveat: a monitor with `renotify_seconds = 0` that goes down mid-window and stays down is
not re-alerted after the window — reminders are the carry-out mechanism, and that monitor
opted out of them.

## D-0135 — Scheduler leadership watchdog + leader/broker health gauges (iter-0077)
The scheduler held its leadership advisory lock on a pooled connection but `lead()` never
re-verified it. If that connection died (network blip, Postgres restart, pooler eviction)
the server released the session lock while the old leader kept dispatching jobs, running
renotify, and advancing escalations — another node could then win the lock, giving two
active leaders (double dispatch/renotify/escalation). `TryBecomeLeader` now also returns a
`check()` that probes the held connection (`pg_locks` for our own backend + reconstructed
key); the leader runs it every 5s and steps down + re-contends the moment the lock is not
confirmed held (fail-safe: any check error is treated as loss). Added two health gauges:
`cerbix_scheduler_leader` (1 leader / 0 standby, only emitted by a process running a
scheduler — exactly one `1` across the fleet is the invariant to alert on) and
`cerbix_broker_up` (AMQP reachability, wired off the connection supervisor's
loss/reconnect transitions, only emitted when the AMQP transport is in use). Both are the
paging signals for the silent-failure classes the hardening package targets.

## D-0136 — Region affinity holds in the shipped topologies (iter-0078)
Two wiring gaps let region affinity — "strict by design" — break in the topologies we
actually ship. (1) `--role all` (the prod and single-geo topology) never called
`WithPullRegions`, so a monitor in a pull region had its jobs published to the in-process
worker and probed from core instead of routed to `pull_jobs` for the region's agent — a
private target was unreachable and a public one gave a core-vantage result, silently. The
`all` scheduler now wires `WithPullRegions`/`WithPullMetrics`, and the test-connection
router was refactored so pull-region tests reach the agent (`pull_tests`) under `all` too,
not just the AMQP roles. (2) The distributed and geo composes ran the CORE worker DB-less,
but composite monitors derive their status from child statuses read from the DB — a
DB-less worker returns "composite evaluation unavailable" and the composite flaps down.
Composites are a core-region concern, so the core worker now uses `config.worker-core.yaml`
(worker config + a database connection) and depends on Postgres; remote geo workers stay
DB-less by design. Verified live: an `all`-mode pull-region monitor routes to `pull_jobs`
with zero local heartbeats, and a distributed core worker evaluates a composite correctly.

## D-0137 — Pool sizing, SLA-report deadlock, per-cadence timeouts (iter-0079)
Three leader-loop robustness gaps. (1) `pgxpool.New` used the default MaxConns
(max(4, numCPU)) while several connections are pinned for the process lifetime — the
leadership advisory lock plus the confirm/pull LISTEN notifiers — so on a small host
(2 CPUs → 4 conns, 3 pinned) query traffic could starve. `Open` now parses the DSN and
enforces a floor (MaxConns ≥ 12, MinConns ≥ 2) plus connection lifetime/idle/health-check,
honoring a larger operator-set `pool_max_conns`. (2) `EnqueueDueSLAReports` held a
transaction and then computed each project's SLIs via `s.pool` — a SECOND acquire while
holding one; under pool saturation the nested acquire blocks forever (self-deadlock). SLIs
now run on the transaction (`projectSLI` on a `sliQuerier` satisfied by both pool and tx).
(3) Every periodic leader task ran on the process-lifetime context, so one hung query (a
lock wait, a slow aggregate) stalled the whole tick and froze dispatch. Each sub-cadence
now runs under a bounded child context (`subCadenceTimeout` 30s; `maintainTimeout` 2m for
the drop_chunks/purge sweep). Verified by a 1-connection regression test that deadlocks on
the old SLA path and passes on the fix.

## D-0138 — Escalation/incident lifecycle hardening (iter-0080)
Three lifecycle gaps around auto-incidents and the on-call ladder. (1) A disabled monitor
with an open auto-incident escalated forever — it is no longer probed, so it can never
auto-recover to resolve the incident. `AdvanceEscalations` now requires `m.enabled`
(deletion was already handled — the JOIN drops the row); the ladder resumes if the monitor
is re-enabled and still down. (2) `ingest.openAutoIncident` had a check-then-create TOCTOU:
two concurrent down transitions could both open an auto-incident. Migration 00049 adds a
partial unique index (one non-resolved auto-incident per monitor, after resolving any
pre-existing duplicates); `CreateIncident` maps the 23505 to `ErrAlreadyOpen` and ingest
treats it as a benign race win. (3) An escalation-policy monitor whose incident-create
failed paged no one: the ladder pages OVER the incident and the flat down-notify is
suppressed, so a lost create meant total silence. Rather than a periodic reopener (which
would wrongly re-page manually-resolved incidents) or a delivery-time check (racy against
the ~ms incident-creation lag), `openAutoIncident` now retries a transient failure (3×,
250ms) and logs a persistent failure at ERROR with an `escalation` flag for log-based
alerting. A full delivery guarantee (incident committed in the status-transition
transaction) is a larger refactor left as a follow-up.

## D-0139 — Auth-surface hardening (iter-0081)
A focused security audit of six auth concerns; two needed no change (OIDC already
verifies audience/issuer/expiry/nonce/state via go-oidc; status-page cross-project
linking stays org-scoped — the org is the tenant boundary and cross-org is already
blocked, so intra-org cross-project is same-tenant by design). Four were real and fixed:

1. **Rate-limiter XFF spoofing + unbounded map.** The login limiter keyed on a blindly
   trusted `X-Forwarded-For` first hop, so an attacker set a unique XFF per request and
   never tripped the limit; the key map was also never evicted (memory DoS). `clientIP`
   now takes a `server.trusted_proxy_count` (default 0 = ignore XFF, key on the direct
   peer) and reads the client that many hops from the right of the XFF+peer chain — a
   spoofed leading XFF can't win a fresh bucket. `allow` opportunistically sweeps idle
   keys (bounded to the last window).
2. **Cross-tenant escalation references.** A monitor's `escalation_policy_id`, an
   escalation policy's step targets (channel/schedule ids), on-call schedule participants,
   and an override's channel id were all persisted without a same-project check (FKs
   enforce existence only; step targets are FK-less JSONB) — so a user could page another
   tenant's channel/schedule/policy. New `internal/api/tenant_scope.go` helpers assert
   same-project ownership in every create/update handler (mirrors `compositeChildrenOK`).
3. **Password change didn't invalidate other sessions.** `changePassword` now deletes the
   user's OTHER sessions (new `store.DeleteSessionsByUser`) while KEEPING the caller's
   current one (the raw session token is threaded through the request context). Standard
   behavior, and it preserves the shared-session model the E2E suite relies on. The
   password RESET path is deliberately left non-invalidating: the product/E2E contract
   treats a self-service reset from a logged-in browser as keeping that session, and the
   reset flow has no current-session token to preserve selectively — reset-time global
   invalidation is a separate product/UX decision.
4. **Public status-page id leakage.** The unauthenticated render embedded the full
   `domain.Incident`/`MaintenanceWindow`, leaking `project_id`/`monitor_id`, the
   `external_key` (e.g. an Alertmanager fingerprint) and `acknowledged_by` (a user id) to
   anyone. The public path now applies `PublicRedacted()` to incidents and maintenance
   windows; the authenticated preview keeps full detail.
