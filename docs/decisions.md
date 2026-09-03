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

**Persistent dev-stack clarification (iter-0117):** `docker/config.dev.yaml` opts in with one fixed,
public, development-only key so its persistent Postgres volume remains readable across E2E runs that
create encrypted push tokens. It is deliberately not a production secret and must never be reused;
`config.example.yaml` keeps encryption empty/opt-in and production injects a random key. Distributed
workers continue using `config.worker-core.yaml` with no at-rest key, as required by D-0155. A wiring
test validates both sides. This avoids a dev-only split-brain state where the server reports ready but
every monitor-list read fails on ciphertext left by an earlier security test; it does not add a
decrypt fallback or weaken the hard error for a genuinely missing key.

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
(`ConfirmedSubscriberEmailsForProject`; the join was subscribers→components→monitors, **superseded by D-0180** — it is now the inverse of the page's project axis), wired via a cli
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
**Superseded (ed652d0, 2026-08-08):** `store.Migrate` now holds a session `pg_advisory_lock` for the
whole goose run, so concurrent roles serialize and the race above can no longer happen. The one-off
`migrate` remains the recommendation — as a way to keep a schema change deliberate, not as a
workaround. Raised again by issue #40, whose premise came from the un-updated overview text.

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

## D-0140 — Remaining correctness sweep (iter-0082)
The final hardening slice — a grab-bag audit of eight items; two needed no change (SSE
has no WriteTimeout, correct for long-lived streams; SSE filtering already happens before
emission so there is no cross-tenant leak — pushing the project filter before buffering is
a memory optimization, not a correctness bug, deferred). Six fixed:
1. **Push failure_threshold** — a dead-man's-switch timeout is a single definitive signal,
   but push monitors were routed through the confirmation-gated status machine, so a
   threshold of N delayed "down" by N missed intervals. `Monitor.Normalize` now pins push
   `FailureThreshold` to 1.
2. **ICMP echo id/seq match** — the ping prober accepted any EchoReply, so a concurrent
   ping or stray reply on a shared raw socket read as a false "up". It now sends a random
   per-probe seq + 16-byte payload and matches seq+payload (and id on the raw socket).
3. **SSRF CGNAT gap** — `100.64.0.0/10` (RFC 6598) is neither `IsPrivate` nor
   non-global-unicast in Go, so it was treated as public. The guard now blocks it (gated by
   `allow_private_ips`), alongside the already-covered RFC1918/loopback/link-local/metadata.
4. **OIDC unbounded client on the boot path** — discovery/JWKS/exchange used
   `http.DefaultClient` (zero timeout) and the first sync ran synchronously at startup, so a
   hung IdP could block boot indefinitely. A 15s-bounded client is injected via
   `oidc.ClientContext` (+ the token exchange), and the first `SyncOIDC` moved into the
   reloader goroutine.
5. **Shutdown goroutine drain** — background workers (outbox, ingest, scheduler, worker,
   notifiers) were fire-and-forget; shutdown only awaited the HTTP server. A `sync.WaitGroup`
   now drains them (10s-bounded) after ctx cancel, so none is left mid-write.
6. **Unique-slug 409 + FK existence oracle** — duplicate org/project slug now maps 23505 →
   `store.ErrConflict` → 409 (was a raw 500); `monitorInOrg` returns an identical "monitor
   not found" for a wrong-org id as for a nonexistent one, closing a cross-tenant
   enumeration oracle. (The add-member-by-email helpful message is left as an accepted UX
   affordance for semi-trusted org admins.)

## D-0141 — Consilium-driven security & reliability hardening (iter-0083)
A prioritized audit pass (run as Claude+Gemini consilium sessions) beyond the
`func-hardening` epic. Every accepted finding fixed as its own `-race`+E2E-verified commit;
the calibration of where the consilium's first instinct was over/under-stated is recorded
in iter-0083 §4. Thirteen fixes across four blocks:

**Secret exposure & auth.** (1) Login timing oracle (CWE-203): the not-found path now runs a
decoy Argon2id verify (`a.decoyHash`) so response time no longer distinguishes known from
unknown usernames. (2) `updateChannel` returns `Redacted()`; a monitor's `push_token` is
stripped from read-only viewers (`WithoutPushToken` unless `ActionProjectWrite`). (3) OIDC
fail-closed: an enforced email allowlist requires a non-empty *verified* permitted email
(was a bypass), a mandatory-TOTP login refuses rather than issuing an MFA-less session when
the user row can't be read, and the OIDC client is 15s-bounded. (4) `ReencryptSecrets` now
covers the columns it had missed — monitor config secrets, `users.totp_secret`, and (block 4)
`push_token_enc` — so key rotation orphans nothing.

**SSRF egress & tenant scope.** (5) Webhook/notify/SMTP delivery routes through
`prober.Guard` (IP-pinning dialer, `Proxy=nil`, redirect cap; SMTP keeps its TLS ServerName;
CGNAT `100.64/10` blocked) — a user-controlled destination host can no longer reach internal
addresses. **Correction (P0.2):** this delivery guard uses a SEPARATE `notification_egress`
policy defaulting to **deny-private**, not the prober policy (which allows private for
operator-chosen probe targets) — the original wiring reused `prober.AllowPrivateIPs=true`, so
the guard was effectively open for editor-controlled destinations. OIDC discovery/JWKS/token
is a distinct **operator-trusted** path on a 15s-bounded client and is **NOT** routed through
`prober.Guard` (the earlier claim here was wrong); an internal Keycloak is a supported
deployment, so identity egress is trusted configuration, not user input. (6) Global search scopes to the caller's visible orgs/projects *in SQL* (`= ANY`)
before ranking/LIMIT, via `authz.VisibleScope` + `store.SearchScope`, closing a cross-tenant
result-crowding leak.

**Correctness spine.** (7) Outbox claim-token CAS (migration 00050) — a stale worker's
delivered/fail write is a no-op. (8) **RecordResult** (migration 00051): heartbeat insert +
status flip + transition-outbox event in ONE transaction, gated by the probe timestamp
against `last_result_ts` — a duplicate re-delivery is deduped (no double failure-count) and a
stale/out-of-order probe is recorded for SLA but never overrides newer live state. Incident
reconciliation stays a separate concern (D-0138). (9) AMQP publisher reopens its channel on a
channel-level exception (connection still up) instead of wedging behind `broker_up=1`. (10)
Pull-job **lease** (migration 00052): claims lease rather than DELETE; the agent acks by
echoing claim tokens on its results POST; an un-acked lease lapses and re-delivers (safe now
that #8 dedups) — at-most-once → at-least-once. The scheduler no longer advances a pull
monitor's cadence on a failed enqueue.

**Supply-chain & at-rest.** (11) Go toolchain → 1.25.12 (GO-2026-5856 crypto/tls ECH leak);
`.ci` off EOL 1.24; the existing govulncheck CI job is the gate. (12) `MaxBytesReader` on the
public (64 KiB) and agent (16 MiB) routers; poison AMQP messages dead-letter to a durable
`checks.dead` queue (results keep NO time-TTL — a slow ingest must never drop real results —
so a broker-native DLX with mutated queue args was deliberately NOT used); `Dockerfile` uses
`npm ci` + lockfile; dependabot `gomod`/`docker` repointed `/backend`→`/` (silent no-ops
after the module moved to root). (13) Push tokens encrypt-at-rest (migration 00053):
`push_token` plaintext → `push_token_hash` (SHA-256 blind index, UNIQUE, for lookup) +
`push_token_enc` (keyring-encrypted); reveal UX preserved, so no rotate/revoke UI was needed.

## D-0142 — Result ingest protocol: origin/timestamp/revision (spec func-result-protocol, P0a/P0b)
Supersedes the ad-hoc iter-0083 freshness gate (worker-clock `hb.Ts` vs `last_result_ts`),
which a review showed could be corrupted three independent ways (future clock freezing live
state; a result of an OLD config winning by a newer timestamp; a dead-man DOWN racing a real
ping). Full contract in `docs/specs/func-result-protocol.md`. Key rulings:

- **Three trusted server-side origins**, established by which typed store entrypoint is
  called — never inferred from the result body: `RecordScheduledResult` (revision-gated),
  `RecordPushResult` (push HTTP handler only, via a dedicated `PushResultRecorder`, NOT the
  shared `ResultSink`), `RecordDeadmanResult` (scheduler leader, direct — no synthetic
  heartbeat through the dispatcher).
- **`execution_revision`** (`BIGINT NOT NULL DEFAULT 1`) is a config generation, bumped ONLY
  by `UpdateMonitor` (bind to the method, not the `config` column — `ReencryptSecrets`
  rewrites `config` but must not bump). Gate evaluated under the monitor's `FOR UPDATE` lock
  BEFORE the heartbeat insert: a revision failure inserts no heartbeat (a result of another
  config is invalid for SLA too). `last_result_ts` resets to NULL on bump.
- **Timestamps:** `observed_at` (raw, nullable) vs `ts` (effective/ordering). Authoritative
  clock is `statement_timestamp()` (statement-scope, unlike transaction-scope `now()`).
  Scheduled outcomes: missing→reject, fresh→apply, out-of-order-within-retention→SLA-only,
  future-beyond-skew→quarantine (no insert), outside-retention→ignore (no insert). Push is
  never rejected on client clock; ordering is server `received_at` captured at ingress.
- **Dead-man** re-checks staleness atomically and applies DOWN through the SAME
  `recordCheckStatusTx` (confirmation/maintenance/transition-outbox preserved), not a raw
  UPDATE.
- **`result.revision_mode: observe|enforce`** (default `enforce`; `observe` tolerates only a
  MISSING revision on scheduled results — a present-mismatch is always rejected).
  **`observe` is a temporary migration mode: removed no later than iter-0089** (≈ three
  iterations after it ships in P0b), or one release after a confirmed prod `enforce` cutover,
  whichever is sooner — it must never become a permanent compatibility bypass.
- `result.allowed_skew` (default 5m, validated `>0` and `<=1h`).

**Historical backfill is exempt from revision gating (explicit exception).** The general
rule "a result of another config is invalid for SLA" applies to LIVE-state ingest.
Historical backfill (`RecordHistoricalResults`, agent replay) preserves a fact that was
true at its historical moment and NEVER touches current state — so revision gating (which
exists to protect current state) does not apply; per-row timestamp bounds still do
(missing/future/outside-retention skipped), and inserts are SLA-only + idempotent. Recorded
here so the exemption is not read as contradicting the general rule.

**Dead-man moves from edge-only to periodic down-sampling (intentional SLA change).**
`StalePushMonitors` currently carries `m.status <> 'down'`, so a push monitor is sampled
DOWN exactly once per outage. To make sample-based SLA reflect a sustained push outage, that
guard is dropped from both the stale-selection query and the dead-man CAS; the dead-man
inserts a DOWN heartbeat each idle tick (throttled by `nextRun`), does NOT advance
`last_result_ts`, and produces exactly one transition/outbox/incident (`prev != cur` guard).
Sampling stops the moment a real ping advances the watermark.

Deferred (separate axes, not this decision): `job_id` correlation + strict
`observed_at >= job_issued_at` (with P0b/job correlation); `state_sequence` for outbox
delivery ordering (#4, P2) — orthogonal to `execution_revision`.

## D-0143 — Result-protocol implementation + hardening backlog delivered (iter-0085)
Implements the D-0142 contract and the whole iter-0084 §5 deferred backlog. Each item is its
own `-race`+E2E-verified commit; schema commits verified in both storage modes.

**P0a — timestamp hygiene + typed origins.** `heartbeats.observed_at` (migration 00054), the
strict `result:` config block, and `result_*` outcome metrics land inert first (expand
phase). `RecordScheduledResult` runs the ordered pipeline (missing-ts reject → `FOR UPDATE` +
`statement_timestamp()` → timestamp bounds before insert → INSERT ON CONFLICT → dedup →
watermark: out-of-order = SLA-only | fresh = apply). Push gets a dedicated `RecordPushResult`
(via `PushResultRecorder`, not the shared `ResultSink`), and every origin reuses one
post-commit `ingest.Reconciler` (SSE + auto-incident). `RecordHistoricalResults` is the
SLA-only, bounded, live-state-never-touched backfill path.

**P0b — execution_revision gate.** `monitors.execution_revision` (migration 00055), bumped
only by `UpdateMonitor`, evaluated under the row lock BEFORE the heartbeat insert; missing →
reject in `enforce` / tolerated+counted in `observe`, present-mismatch → reject always.
`RecordDeadmanResult` is the scheduler leader's typed entrypoint (atomic staleness re-check,
DOWN through the same `recordCheckStatusTx`, periodic down-sampling).

**P1 — reliability.** Readiness-gated push-token encrypt-at-rest backfill (no plaintext
bearer served during the upgrade window); pull ACK/lease correctness (malformed jobs are not
acked so they re-deliver; claim batch bounded).

**P2 — hardening list (audit #1/#3/#4/#5/#7/#8/#9/#10/#11/#13).** Monitor resource caps in
`Monitor.Validate` (#3); strict env-expand (undefined `${VAR}` fails Load — #11), `TruncateAll`
refuses non-`test` DBs (#8), migrate advisory lock (#10); fail-closed crypto RNG (#13) +
public-DTO redaction (`PublicRedacted()` — #9); atomic password reset (#7) + TOTP lockout
fixes + subscriber unsubscribe link kept (#6); delivered-outbox purge (#4), outbox CAS
`applied` signal (#1), dependency-graph advisory lock (#5).

**Follow-on P2 (config/migration-bearing).**
- **#14** — rate-limit `server.trusted_proxy_cidrs`: honors X-Forwarded-For only when the
  direct peer is inside a trusted network, then walks the chain right-to-left skipping
  trusted proxies (the first untrusted address is the client). SUPERSEDES the hop-count model
  (`trusted_proxy_count`) when set; empty = legacy hop-count. Closes the dual-path spoof where
  a request reaching the origin directly could forge XFF and mint a fresh limiter bucket.
- **#12** — OIDC identity keyed by `(issuer, subject)`, not subject alone (migration 00056,
  partial unique index on `(oidc_issuer, oidc_sub)`). `UpsertUserByOIDCSub` →
  `UpsertUserByOIDCIdentity(issuer, sub, …)` threading the ID-token `iss`; a legacy NULL-issuer
  row for the subject adopts the current issuer once, guarded so it can't steal a bound
  identity. Two providers minting the same `sub` no longer collapse into one account
  (identity-confusion / account-takeover).
- **#2** — outbox `state_sequence` (migration 00057): a monotonic per-monitor counter bumped
  in the same tx as each applied transition, carried in the `MonitorTransition` event, checked
  at delivery — an event whose `Seq` is older than the monitor's current `state_sequence` is
  dropped as superseded (a stale DOWN after a delivered recovery; a reminder for a
  since-recovered monitor). `Seq==0` (legacy) is never stale. Realizes the item D-0142 §9
  deferred; orthogonal to `execution_revision`.

## D-0144 — Push liveness watermark & dead-man re-arm (iter-0085)
Refines D-0142's blanket "`UpdateMonitor` resets `last_result_ts = NULL` on bump", which a
post-P0b review showed is a **false-DOWN defect for push monitors**: `last_result_ts` was
overloaded as both the scheduled out-of-order watermark and the push/dead-man liveness
watermark. Nulling it on any edit of a long-created push monitor made
`COALESCE(last_result_ts, created_at)` fall back to the stale `created_at`; the bumped
revision then matched the dead-man CAS, so an ordinary edit (even a rename) — including one
from the future file-provider — fired a false synthetic DOWN + incident.

Shipped fix (migration 00058, `monitors.push_armed_at`):
- **`last_result_ts` is the REAL-ping watermark only.** `UpdateMonitor` PRESERVES it for push
  monitors (they have no scheduled ordering compare) and still resets it to NULL for scheduled
  monitors (§3 rationale unchanged).
- **`push_armed_at` is a server-owned re-arm epoch.** Dead-man freshness is
  `COALESCE(GREATEST(push_armed_at, last_result_ts), created_at)`, used identically by
  `StalePushMonitors` and the `RecordDeadmanResult` CAS; `created_at` is the floor only when
  both are NULL.
- **`disabled → enabled` starts a fresh liveness epoch** (re-arm from the enable moment):
  `push_armed_at = statement_timestamp()` (a pre-disable ping is not proof of liveness; no
  `last_result_ts` is fabricated), live state resets to `pending`, the confirmation counter
  clears, and `state_sequence` bumps WITHOUT an outbox event, so an undelivered pre-disable
  DOWN is dropped as superseded (D-0143 #2). First ping → `pending → up` (no recovery
  notification); no ping past the window → `pending → down` with one alert.
- **`disable`** just drops the monitor from dead-man evaluation (the `enabled` filter).
  Shrinking `interval`/`grace` on an enabled monitor can turn it stale at once — that is the
  new policy applying, not a reset.

**Lifecycle follow-up (incident/escalation).** Setting `pending` on re-enable exposed a
lifecycle regression: a pre-disable auto-incident could keep paging. Two coupled fixes:
- `AdvanceEscalations` now gates on `m.enabled AND m.status = 'down'` (was `enabled` only):
  the on-call ladder never fires while the monitor is `pending` (the re-arm window); it
  resumes only once the monitor is confirmed down again (dead-man or a real failure).
- The reconciler resolves an open auto-incident on **any transition INTO up**
  (`cur == up && prev != up`), not just `down → up`, so a re-enabled monitor recovering as
  `pending → up` closes its stale incident. A normal first `pending → up` (nothing open) is
  a safe no-op (one lookup). If instead no ping arrives, the dead-man fires `pending → down`,
  reuses the still-open incident (no double-open), and the ladder resumes.

The pending/counter/`state_sequence` reset applies to all monitor types on re-enable;
`push_armed_at` is push-only. Contract recorded in `func-result-protocol` §11; regression
tests `store.TestPushUpdatePreservesLiveness`, `store.TestAdvanceEscalationsRequiresDownStatus`,
`ingest.TestReconcilerClosesIncidentOnPendingToUp`.

## D-0145 — Monitoring as Code is a tenant-scoped file reconciler in the API control plane (spec, iter-0086)

Cerbix will expose Monitoring as Code through named static file providers whose directory
contents are hot-reconciled without process restart/reload. This is not an in-memory config
swap: PostgreSQL remains the runtime/history source of truth, while files are desired state
only for resources owned by their provider. The native syntax is a versioned
`ProjectBundle` (`format: 1` once per file, resource maps keyed by stable source UID), not a
Kubernetes `apiVersion`/`kind` envelope and not a Go/DB serialization.

Organizations/projects must already exist; static provider scope is explicitly
`instance|organization|project`, and bundle resolution/reference checks cannot escape it.
Ownership is per monitor, so UI/API monitors coexist and are never diffed as provider
orphans. Normal CRUD is rejected for file-managed declarative fields; there is no automatic
adoption by name. A project bundle is the atomic/fault-isolation unit. Invalid input keeps
its last-known-good state; ambiguous invalid scans suspend orphaning. Valid disappearance
marks orphan and disables after grace, but never hard-deletes heartbeat/SLA/incident/audit
history; restoration reuses DB identity and push token.

The reconciler is an internal `api`/`all` component, with one PostgreSQL advisory-lock
leader per provider and no `controller` role/service. Scheduler propagation is via an
in-transaction notification plus its normal DB refresh fallback. Semantic no-ops never
write/bump revision; real config changes use the D-0142 `UpdateMonitor`/
`execution_revision` contract, so production enablement depends on P0b. Dynamic files are
strict and bounded, allow no environment expansion/inline secrets, and expose explicit
degraded/LKG observability rather than applying partial invalid state. Full contract,
failure matrix, rollout, and acceptance tests: `docs/specs/func-monitoring-as-code.md`.

**Addendum (iter-0087, contract foundation).** Applying spec §3.1 ("a type is available only
when every required type-specific field has a strict non-secret schema"), the v1 file
provider's supported monitor types are the ones fully expressible by common fields —
`http, tcp, icmp, dns, tls, grpc, websocket, ssh, push`. Types needing a typed `settings`
object (`composite`, `synthetic`) or credentials (`postgres, mysql, redis, promql,
rabbitmq`) are rejected by the parser with a bounded `unsupported_type` reason until their
strict non-secret `settings` schema (composite/synthetic) or the `secret_ref` contract
(credentialed types) lands in a later iteration. This is the spec's own scope clause, not a
simplification: there is no generic `config` escape hatch, and any secret-bearing key is
rejected `inline_secret_forbidden` before type resolution.

**Addendum (2026-09-01, owner: `promql` is admitted, and a target may not carry credentials).** Two
decisions, taken together because the second is what makes the first safe to leave alone.

**`promql` joins the file-supported types.** The iter-0087 addendum above listed it among the types
"needing credentials", beside `postgres, mysql, redis, rabbitmq`. That classification was wrong and
stayed wrong through FR-020: `promqlProber` reads exactly ONE config key — `query` — and sends a plain
unauthenticated GET to `<target>/api/v1/query`; it has never had a credential slot, and
`domain.CredentialedType` has never returned true for it. So the thing standing between it and a
bundle is not a `secret_ref` contract, it is one typed non-secret field. Consequence for the parser:
the branch that reads "a type has `settings` IF AND ONLY IF it is credentialed" becomes "IF AND ONLY
IF it has a schema", which is what §3.1's rule has said in words since the beginning. `composite` and
`synthetic` stay out: children-by-UID and a multi-step definition are real schema problems, not a
missing field.

**An authenticated Prometheus stays unsupported, and the sentence saying so must be precise.** The
owner's answer is a regional agent placed where Prometheus lives — which is this product's own design
for unreachable targets, not a workaround. But an agent does not authenticate: it speaks the same
HTTP and collects the same 401. What the operator must provide is an UNAUTHENTICATED PATH for the
prober — an IP allowlist in the fronting proxy, a localhost listener, a network policy — and the
documentation says exactly that, because "put an agent next to it and it will handle auth" is a
sentence a reader can act on for a day before discovering it is false.

**A monitor target may not carry userinfo, on any surface.** Go's `net/http` turns
`https://user:pass@host` into an `Authorization: Basic` header on its own (verified against
`httptest`), so that URL works today — and the password is then stored as plaintext in
`monitors.target`, which `Monitor.Redacted()` does not touch, so every VIEWER of the project reads it
in the monitor list. The file provider has always refused such a target
(`inline_secret_forbidden`); the UI and API accept it. The decision is to refuse it everywhere, at the
domain boundary, and to say so in the changelog: this breaks an installation that is relying on it
today, and a rejected upgrade an operator can see beats a credential leak they cannot. Nothing about
this admits a credential slot to `promql` — a monitor that needs one is the unsupported case above.

**Status.** Both landed with this record, 2026-09-01. `promql` is in `fileSupportedTypes` and its
settings go through `domain.PrepareTypedSettings`, the one entry point that replaced the
"settings iff credentialed" branch; the userinfo rule sits in `Monitor.Validate`, so every write
surface passes through it. What did NOT change: stored monitors are never re-validated on read, so an
installation living on a userinfo target keeps probing and is refused at its next EDIT — the next
release owes that upgrade note, and the roadmap carries the obligation.

## D-0146 — File-provider canonical hash excludes dependency edges (iter-0089)
Spec §7 lists "dependency UIDs" among the set-like values folded into the canonical semantic
hash, while §9.2 / D-0142 require a dependency-only change to be a `dependency_update` that
does NOT bump `execution_revision`. Folding dependencies into a single hash makes those two
clauses contradict: any dependency edit would change the hash and be classified as a
revision-bumping `update`.

Resolution (reconciles both, no spec weakening): the canonical **monitor** hash covers only
the monitor's own execution-affecting config (name, type, target, schedule, conditions,
tags, region, enabled, auto-incident, settings) and EXCLUDES dependency UIDs. Dependencies
are compared as a separate normalized (sorted+deduped) set. The planner then classifies:
`noop` iff config-hash equal AND dependency-set equal; `update` (revision bump) iff the
config-hash changed; `dependency_update` (NO revision bump) iff only the dependency-set
changed. This preserves §7's intent (order/comment-insensitive no-op detection, including
dependency order) and honors §9.2/D-0142 exactly. `execution_revision` is bumped only by a
real monitor-config change, never by a dependency-graph edit.

## D-0147 — File-provider watcher is poll-based, not event-driven (iter-0095)
The Monitoring-as-Code watcher (spec §11) is implemented as a bounded directory POLL
(a name+size+mtime fingerprint sampled every ~1s) plus debounce and the spec-mandated
periodic full resync — NOT an inotify/fsnotify event stream. Rationale: the spec already
requires the periodic resync and states correctness "cannot depend solely on" filesystem
events (they may be coalesced or lost, and ConfigMap/git-sync replace the directory inode);
a poll + resync meets the observable contract without adding an fsnotify dependency or its
platform edge cases. Recorded consequences: a change is detected within one poll interval +
debounce rather than instantly, and a pure `chmod` (no size/mtime change) is not observed
until the next resync. If sub-second latency is later required, an fsnotify hint layer can be
added in front of the same reconcile path without changing the model. This is a deliberate
architecture decision, not an omission.

## D-0148 — File-provider audit fires on any persisted state change; scheduler NOTIFY only on execution change (iter-0109)
Spec §9 step 10 requires an audit record on a "changed apply" but does not enumerate what
counts as changed. This decision fixes the contract: the `file_provider.apply` audit row is
written for ANY persisted ownership/config state change in the apply transaction — create,
update, dependency change, restore, first orphan-mark, un-orphan, and grace-disable — i.e.
whenever `stateChanged` is true. A pure no-op (nothing persisted) writes no audit. Separately,
the scheduler wake (`pg_notify(monitor_config_changed)`) fires ONLY when EXECUTION config
changed (`execChanged`: create/update/re-enable/disable) — a dependency-only or ownership-only
change affects delivery-time suppression, not scheduling, so it must not force a reschedule.
Rationale: an external review found the code audited only on exec/dep changes while §9 reads as
"any changed apply", so first-orphan and un-orphan (which do mutate persisted generation/owner
state) went unaudited — code and spec diverged. Auditing all ownership-state transitions is the
literal reading of §9 and gives operators a complete lifecycle trail; the NOTIFY axis stays
narrow so no-op-for-scheduling changes don't churn the leader. Consequence: an un-orphan with an
unchanged config is audited (an ownership transition) but does NOT bump `execution_revision` or
NOTIFY (D-0142 revision-safety preserved).

## D-0149 — GitLab CI removed from the project (2026-08-10)
GitLab CI is not used for this project, so the pipeline is removed: `.gitlab-ci.yml`, the
`.ci/` includes (`common`/`backend`/`frontend`), and the `ops-cicd.md` skeleton spec are
deleted. The former NFR-008 ("CI runs lint + `go test -race` + coverage gate") is retired and
its rows dropped from `docs/status.md` and `docs/traceability.md`. Quality gates remain enforced
locally per the docs workflow: `go vet`, `go build`, `go test -race -count=1 ./...` (both storage
modes for heartbeat/retention changes), the docker frontend `vue-tsc` build, and the live-stack
E2E (`e2e/run.sh`, `e2e/mac-smoke.sh`). If a hosted pipeline is reintroduced later, it should
re-run exactly those commands. This is a deliberate scope removal, not a regression.

## D-0150 — Project deletion is a hard cascade delete, org-admin-only, refused for file-managed projects (iter-0111)
`DELETE /api/v1/projects/{projectID}` (FR-018) permanently removes a project. Three choices:
(1) **Hard delete, not archive** — every FK into `projects` is already `ON DELETE CASCADE`
(monitors → heartbeats/rollups/incidents/notifications/dependencies, SLA, escalation, on-call,
notification channels, project-scoped tokens/webhooks/status-pages, file bundles), so a single
`DELETE FROM projects WHERE id AND org_id` wipes the subtree in one tx; a soft-delete/trash column
would fight the schema for no product ask. (2) **Org-admin (`ActionOrgManage`) only** — symmetric
with project creation, which already requires org-manage; a project-scoped Project Admin
administers content inside a project but not its existence. Global admin passes. (3) **Refused
`409 managed_by_file` when the project owns file-provider bundles/monitors** — a running file
provider would recreate it on the next reconcile, so an API delete would be silently undone; the
operator must remove the YAML instead. The file-ownership check and the `DELETE` share one tx so a
concurrent apply can't reclaim ownership between them. Confirmation is type-the-slug in the UI
(irreversible, GitHub-style); the API guard is purely RBAC. Verified: `store.TestDeleteProject`
(both storage modes — cascade, tenant scoping, `ErrManagedByFile`), `api.TestDeleteProjectAuthz`
(204/403/404/409), and a live-stack E2E (`e2e/tests/project-delete.spec.ts`). Drive-by: the
mislabeled `delete: Remove a member` operation that `openapi.yaml` had attached to
`/organizations/{orgID}/projects` is moved to its real path
`/organizations/{orgID}/members/{membershipID}` (it had drifted out of sync with `schema.d.ts`).

## D-0151 — Organization deletion is a hard cascade delete, global-admin-only, refused for file-managed orgs (iter-0112)
`DELETE /api/v1/organizations/{orgID}` (FR-019) is the org-level analogue of D-0150. (1)
**Hard delete** — every FK into `organizations` is `ON DELETE CASCADE` (memberships, projects →
their whole subtrees, org-level status pages, org-scoped tokens/webhooks, audit_logs), so one
`DELETE FROM organizations WHERE id` wipes the tenant in one tx; no new migration. (2)
**Global-admin (`ActionGlobalManage`) only** — symmetric with `createOrganization`; an org admin
cannot delete their own org. The gate is checked *before* any existence lookup so non-admins get
`403` with no existence leak; a global admin hitting a missing org gets `404`. (3) **Refused `409
managed_by_file` when the org owns file-provider-managed projects** — unlike a deleted project
(which a reconcile recreates), a reconcile does NOT recreate a missing org; it fails tenant
resolution (`ErrBundleTenantNotFound`) and would error on every pass, so refusing keeps the
provider out of a perpetual error state. The check and the `DELETE` share one tx. (4) The
`org.delete` **audit is written with `org_id = NULL`** (an instance action; `audit_logs.org_id` is
nullable since migration 00047) precisely so the row survives the cascade that drops the org's own
`audit_logs`. Confirmation is type-the-slug in the UI; the API guard is RBAC. Verified:
`store.TestDeleteOrganization` (both storage modes — two-level cascade, tenant survival,
`ErrManagedByFile`), `api.TestDeleteOrganizationAuthz` (204/403/404/409), and a live-stack E2E
(`e2e/tests/org-delete.spec.ts`).

## D-0152 — MaC target inline-secret guard: reject URL userinfo AND known secret query keys AND malformed URL-shaped targets (FR-017)
The file provider forbids inline secrets everywhere (§2.9, §19, NFR-014). For a monitor `target`
this is enforced structurally in `buildMonitor` before type support, so a credential can never leak
through a different reason. (1) A well-formed URL whose **userinfo** is populated
(`https://user:pass@host`, `postgres://user:pass@host/db`, password-only `https://:pass@host`)
rejects with `inline_secret_forbidden`. (2) A well-formed URL whose **query string** carries a key
in the finite `secretSettingKeys` set (`token`, `api_key`/`apikey`, `password`, `secret`,
`client_secret`, …) rejects — reusing the *same* classification that already rejects inline settings
secrets, matched case-insensitively and after percent-decoding, so `https://h/?token=…`,
`?API_KEY=…`, `?tok%65n=…` and duplicate secret keys are all caught. A query that fails
`url.ParseQuery` (e.g. `?token=%zz`) rejects conservatively — it cannot be proven free of a secret
key. A query with only non-secret keys (`?x=1`) is accepted. (3) A **URL-shaped** target (carries a
`://` scheme separator) that FAILS `url.Parse` also rejects: a parse failure (invalid percent-escape
like `https://u:pw@h/%zz`, or a control character) means the target cannot be proven free of
embedded credentials, and `domain.Monitor.Validate` only checks the target is non-empty — so a
malformed-but-credentialed target would otherwise be persisted verbatim (the P1 bypass this decision
closes). (4) Rejection messages **never echo the raw target or any query value** (they name only a
known key label), so logs/status/metrics don't leak the credential. **This does NOT weaken §2.9/§19/
NFR-014**: query credentials are rejected, not tolerated — an earlier draft that scoped them out was
withdrawn as an unacceptable silent requirement simplification. Verified:
`fileprovider.TestDecodeStrictRejections` (userinfo user:pass / user-only / DSN / malformed-with-
creds / password-only; query token / mixed-case API_KEY / percent-encoded key / duplicate key /
malformed encoding), `TestDecodeTargetNoUserinfoAccepted` (host:port, ICMP/SSH hostname, clean
`?x=1`), `TestDecodeTargetRejectionDoesNotLeakSecret` (userinfo + malformed + secret query value),
and `TestBuildMonitorControlCharTargetRejected`.

## D-0153 — A provider scope change quarantines prior-scope managed rows; it never silently adopts or deletes them (FR-017)
The file provider owns rows by provider NAME (`provider_id` is the name; D-0152 context), and absence
is trusted only within the provider's current static scope (§9.1/§10, the #2 fix:
`ProviderScopeConfig.Includes`). A consequence must be documented as deliberate policy, not a trap:
when a provider restarts with the SAME name but a NARROWER or DIFFERENT scope value, its prior-scope
projects fall outside the current authority. Those rows are **quarantined**: they keep running, remain
provider-owned and read-only in the UI/API, still count in diagnostics, and are NEITHER changed nor
orphaned/deleted by the new scope — the reconcile skips them before the absence check and emits a
throttled `file_provider_owned_out_of_scope` warning. This is a safety quarantine: the provider cannot
observe the old scope's directory intent, so silently orphaning (destroy) or adopting (take over) would
both be wrong. **Operator procedure** (runbook): to move authority, give the new scope a NEW provider
name (the old name's rows stay put, cleanly separable); to retire the old scope, either revert the
provider to its old scope and let normal reconcile manage/orphan the rows, or perform an explicit,
reviewed release/migration — never rely on the scope change itself to clean up. **Widening** is the safe
direction: rows that fall back INSIDE a widened scope resume normal valid-absence semantics (a project
absent from the snapshot is orphaned as usual). Verified: `config.TestProviderScopeIncludes` (matrix)
and `runtime.TestReconcileSkipsOutOfScopeOwnedProjects` (in-scope absence orphaned; out-of-scope owned
untouched).

## D-0154 — Repo-wide golangci-lint pre-existing debt is waived for the fix/mac-hardening branch; tracked as a dedicated cleanup MR (iter-0114)
`AGENTS.md` (Makefile:24-25) makes `golangci-lint run ./...` a DoD gate. On `fix/mac-hardening`
that command exits 1 with **42 findings** (errcheck 32, forbidigo 2, staticcheck 8), **all
pre-existing in files this hardening pass never modified** (e.g. `internal/cli/cli.go:91-93,612`
help-text `Fprintln`/`disp.Close`; `internal/store/projects.go` + test `conn.Close`/`tx.Rollback`;
forbidigo ×2 in `internal/store/{oidc.go,reencrypt.go}` where the `client_secret` column is
legitimately queried; staticcheck ×8 across unrelated packages). This pass's own changed scope is
lint-clean: `golangci-lint run ./internal/fileprovider/... ./internal/config/...` = 0 issues, and no
finding lands on a line this pass changed (the three it briefly introduced — 2 gofmt + 1 QF1008 —
were fixed in `45ba456`). **Decision:** the owner **waives** the pre-existing repo-wide lint debt for
this branch rather than expanding the branch to touch unrelated files (which would violate scope
discipline and mix concerns). The hardening pass therefore meets DoD on its changed scope. Clearing
the 42 findings is tracked as a **separate, dedicated cleanup MR** — consistent with the existing
`.golangci.yml` note that style-heavy linters are "intentionally deferred to a dedicated cleanup MR".
No monitoring/reliability contract changes; nothing is silently marked green — the repo-wide RED is
recorded (iter-0114 §4) and explicitly waived here.

**Close-out (iter-0117):** the owner subsequently made the dedicated cleanup explicit with
“fix everything”, so the waiver no longer applies to the current branch state. All 42 findings
were fixed without disabling or weakening a linter; `golangci-lint v2.12.2 run ./...` is the
repo-wide gate. Intentional reads of the `client_secret` column remain two narrow, inline
`forbidigo` suppressions at the database encryption boundary; neither value is logged. Output,
close, rollback, and stream errors are now handled or explicitly discarded where no recovery is
possible, and Prometheus rendering stops after the first writer error. This is cleanup of the
recorded debt, not a change to monitoring, tenant, or reliability semantics.

## D-0155 — Secret inventory (FR-020): two keyrings, AAD context binding, verifiable wire barrier, linearization-point dispatch (design contract)
Design decisions behind spec `func-secret-inventory` (r6, independently design-approved after six
review passes). (1) **Two keyrings, never shared:** the at-rest master (`security.encryption_key`)
encrypts the inventory and NEVER leaves core; per-region **dispatch keyrings**
(`security.dispatch`, `{primary{id,key}, previous[]}` per region) seal credential envelopes —
an executor holds only its region's keys, so a compromised worker/agent exposes at most **all
retained payloads of its region until TTL/DLQ purge** (the recorded exposure statement), never
the database. Executor profiles carrying the master key are REJECTED at startup, not ignored. A
`default` keyring is single-trust-domain only; combining it with per-region keys requires
`shared_trust_acknowledged: true`, which widens the exposure statement explicitly. (2) **AAD
context binding, canonical length-prefixed encoding (`enc:v2a`)**: at rest AAD =
`(project_id, secret_id)` (stable ids — rename stays metadata-only; cross-tenant ciphertext
transplant fails authentication); in dispatch AAD = `(v, region, key_id, monitor_id,
execution_revision, field_name, job_id)` (no cross-context transplant; exact same-job replay is
transport behavior, fenced at ingest). Auth failure NEVER falls back to a legacy AAD-less
decrypt, and the legacy string API rejects `enc:v2a` tokens instead of passing ciphertext
through. (3) **Verifiable wire barrier:** envelope payloads ride versioned queues
(`checks.jobs.v2.<region>`, `checks.tests.v2.<region>`) and version-predicated pull claims
(`protocol_version` in the claim query); `secrets.enabled` REQUIRES
`dispatch_envelope: enforced` — keys can ship ahead (enforced + enabled:false, the expand
phase), but the inventory can never dispatch over legacy plaintext. Executor config/decrypt
failures are a typed `probe_error` outcome (no heartbeat, no status flip — operational, never a
false DOWN). (4) **Linearization point:** the authoritative per-dispatch DB read (config +
eligibility + routing + cadence from ONE read) is the dispatch-authorization boundary — changes
committed before the read are honored; a change committed after cannot recall the enqueued job:
it remains in-flight exposure until ACK/TTL/DLQ purge, its result rejected by the D-0142 fence.
Immediate revocation = rotate the credential at the target; hard recall is a non-goal.
(5) **Target transport is honest egress:** ref-based monitors default to encrypted transport
(postgres `sslmode: require` — stated as encrypted-NOT-verified; mysql/redis `tls: true`
verified per Go defaults; rabbitmq management https); insecure/skip-verify is an explicit,
visible opt-in in the monitor definition. (6) **Tenant invariant at the store surface:** every
secret query takes and predicates `project_id`; the materializer resolves via
`monitors JOIN monitor_secret_refs JOIN project_secrets` with equality on all project ids;
referential integrity is a normalized ref table whose secret FK is
`ON DELETE NO ACTION DEFERRABLE INITIALLY DEFERRED` (commit-time delete guard mapped to
`409 secret_in_use`; project-delete cascade stays order-independent). Delivery is iterative:
iteration 1 = inventory + config + API/UI (this decision's contract), iterations 2–3 wire refs,
materialization, envelope, fencing and prober TLS per the spec's §10 plan. Runtime ownership is
part of the trust boundary, not deployment convention: `worker` mounts ops HTTP and regional
consumers only; generic outbox/settings/mailer/user API and the future authoritative materializer
are owned by `api`/`scheduler`/`all`, which are the roles permitted to hold the at-rest master.
Implementation closed in iter-0116: the wire barrier covers AMQP jobs/tests and physically
version-predicated pull jobs/tests; the authoritative read supplies routing and cadence as well
as config/revision; executor key failures are diagnostic-only and readiness-visible; at-rest
reencryption uses exact-ciphertext CAS plus a bounded zero-old-key convergence proof. The
dispatch-key and at-rest-key rotation procedures are deliberately separate in `runbook.md`.

## D-0156 — Startup failure uses cancel/drain-before-close; deployment wiring mirrors role ownership (iter-0117)

The `serve` lifecycle has one idempotent cleanup order for both normal shutdown and every
fail-fast startup return: **cancel the root context → wait a bounded interval for background
users → close the dispatcher → close the database**. Closing shared infrastructure first lets
already-started goroutines repeatedly hit a closed pool and can leave an HTTP-less process alive;
defer order is therefore part of the application reliability contract, not an implementation
detail. Settings and OIDC refresh loops are caller-owned and join the same tracked group. Cleanup
waits for that group up to its explicit bound, warns on timeout, and only then closes dependencies;
tests cover both cooperative drain and the bounded non-cooperative case. A live missing-file-
provider-root start must exit promptly without `closed pool` churn.

Static role ownership also applies to deployment manifests. Every shipped `api`/`all` service
must mount its configured Monitoring-as-Code root read-only at the exact configured path; the
single and distributed Compose profiles are checked from parsed Compose YAML. Missing or
unreadable roots remain strict fail-fast errors — runtime directory creation or silent provider
downgrade is forbidden. Finally, the diagnostics transport contract is stable for empty state:
global responses always contain array-valued `bundles` and `providers`, and organization
responses always contain array-valued `bundles`; empty means `[]`, never `null` or omission.

## D-0157 — RabbitMQ 4.3 compatibility is verified; retained data upgrades stay explicit and staged (iter-0117)

The shipped Compose files require an explicit, persisted `CERBIX_RABBITMQ_IMAGE`: no static
default is safe for both a retained 3.12 volume and a volume already upgraded to 4.3. The fresh
production, base-dev, and geo-dev env templates select 4.3, while an existing installation must
first pin its current image and then follow `3.12 → 3.13 → 4.2 → 4.3`, or a reviewed blue/green
migration. Each env file maps one-to-one to its named RabbitMQ volume. The operator procedure and
rollback rules live in `docs/runbook.md`.

Queue names, routing, and the physical v1/v2 credential wire separation are unchanged and both
protocol paths are exercised live against 4.3. The shared regional test-RPC queues
(`checks.tests[.v2].<region>`) do change from transient+auto-delete to durable+auto-delete:
RabbitMQ 4.3 rejects transient non-exclusive queues, while exclusive queues cannot be shared by
multiple workers. The durable queue *definition* survives broker restart; individual test requests
remain transient, time-bounded RPCs and may be lost during a restart. Auto-delete removes the
queue after its last consumer leaves. Per-request reply queues remain server-named transient
exclusive. Worker startup synchronously establishes both enabled test consumers before readiness,
and RPC publishes are mandatory so a nonexistent regional queue fails promptly. If an auto-delete
queue still exists briefly with zero consumers, the routed request remains bounded by the normal
RPC timeout; mandatory publish cannot detect that consumer gap.

Changing durability under the same queue name is not rolling-compatible. The application queue
shape must therefore be migrated on the old broker with test traffic stopped and old workers fully
drained before any broker hop; rollback across that boundary likewise requires stopping workers
and deleting only empty test queues. No procedure may delete heartbeat, incident, audit, or
non-empty broker data.

## D-0158 — Make orchestrates only fixed non-production topologies and never guesses persisted state (iter-0118)

The repository exposes a small Make facade for repeatable local verification of the shipped
single-process, distributed-role, and geo Compose topologies. Existing `build`, `test`, `race`,
and `lint` retain their native Go meanings. Dev-stack goals use only the fixed
`docker/docker-compose.yml`, `docker/docker-compose.geo.yml`, `docker/.env.dev`, and
`docker/.env.geo` paths; they cannot be redirected to the production manifest. Base and geo use
separate env files because they own separate RabbitMQ volumes that can legitimately be at different
upgrade checkpoints. Each env file is mandatory and pins its own broker volume. Make never invents
a broker default and never rewrites an existing pin. Compose execution discards a same-named shell
image override and pins the project name, so neither `CERBIX_RABBITMQ_IMAGE` nor
`COMPOSE_PROJECT_NAME` can bypass the file/volume binding. Initializing either file from its
fresh-install template is a separate, explicit operation and is refused when the corresponding
dev broker volume already exists.

Topology changes are explicit. An `up` goal fails when a mutually exclusive base/geo or
single/distributed topology is already running instead of silently stopping it. Distributed and
geo startup order is build, healthy infrastructure, one explicit migration, role processes, then
per-role `/readyz`; unpublished role endpoints are checked from a short-lived container on the
internal Compose network. `down` removes containers and networks only: no goal passes `-v`, prunes
volumes, or deletes application data. E2E goals target hard-coded loopback URLs and run only after
the corresponding readiness contract; they never point the state-mutating browser suite at an
arbitrary host.

These goals are development/test conveniences, not RabbitMQ migration tooling. D-0157 and the raw,
per-hop runbook commands remain authoritative for retained-volume upgrade and rollback.

## D-0159 — Service reliability (FR-021): two axes, duration-weighted facts, sealing with a real ingest handshake, maintenance as a retroactive declaration (design contract)

FR-021 introduces the object the product's five existing capabilities are *about*: an
operational unit whose reliability is **explicitly declared** rather than inferred from a
grouping of checks. This record fixes the design contract; `func-service-reliability.md` (r8,
APPROVED) carries the prose and the 47 acceptance invariants.

The design took **seven adversarial passes** before it was implementable — nine P0s, then
seven, then seven again, then three, three, one, one. The middle round is the one worth
recording: **all seven of its findings were defects introduced while closing the previous
round's**, which is why the practice of reviewing a specification the way code is reviewed is
recorded here as part of the decision and not as a footnote. Every one of those defects would
have cost a migration and a storage rewrite had it been found after implementation.

**Two axes, not one revision stream.** A `definition_revision` is what a human declared
availability to MEAN; an `evaluation_epoch` is what the system was MEASURING. They have
different owners, different authority rules and different reporting semantics, and one stream
cannot carry both. A fact references the **epoch only**, which resolves to exactly one
revision. Every definition-revision transaction creates a matching epoch unconditionally — a
revision without one is an unsatisfiable foreign key, not a latency problem. Execution-driven
epochs are created eagerly in the same transaction as the monitor write, are skipped only when
the canonical **evaluation-semantics** hash is unchanged, and resolve the declaration in force
at their own `effective_at`, including a pending same-boundary revision.

**The evaluation-semantics projection is exhaustive, not an allowlist.** Every field that can
bump `execution_revision` is explicitly classified IN or OUT, with no default, beside the field
set that decides that bump. This is the FR-020 lesson applied before the fact rather than
after: a rule with a silent default mishandles the next field added. `target` is IN — a
narrower snapshot would have let a target change produce no epoch while §12.1 correctly says a
target change makes two numbers incomparable. Secret **material** never enters the row or the
hash; credential identity and generation do.

**Facts are duration-weighted, in microseconds, on two axes.** `good/bad/unknown/excluded` for
availability and `healthy/degraded/down/health_unknown` for health, each summing to
`bucket_size` exactly, with `excluded` shared. The rejected alternative was to quantize the
whole 60s bucket to one enum. Its error is not random but systematically pessimistic and scales
with bucket size: under "BAD dominates", one second of downtime costs sixty seconds of budget,
and for a 99.99% objective over 30 days a single one-second blip consumes 23% of the month.
Durations also make hour and day rollups exact associative sums instead of a second rounding
applied on top of the first. Microseconds rather than milliseconds because breakpoints derive
from `timestamptz`; a millisecond store would need an undocumented rounding rule with a
conservation correction inside an error budget.

**Coverage is a second, independent axis, and it cannot be bought.** Storage continuity (the
buckets exist, contiguously, through the watermark) and decidable coverage
(`(good+bad)/(good+bad+unknown)`) answer different questions and both must pass. Without the
second, 129,600 materialized buckets of which one is GOOD report `100% / 90d` from sixty
seconds of measurement. `missing_data_policy: ignore` may never move time into
`excluded_duration`: it removes an UNKNOWN member only while other known members keep the
interval decidable, and when it removes the last source the interval is UNKNOWN. Otherwise the
coverage axis is defeated from the settings page. `min_decidable_coverage` is fixed at 0.95 and
is **not operator-settable** in phase 1, for the same reason.

**Sealing needs a real handshake, not a visibility claim.** "A heartbeat counts if it is
visible to the sealing transaction" is not a mechanism: PostgreSQL takes a snapshot per
statement, and the losing ingest could not discover it had lost. Phase 1 specifies
`service_bucket_ingest`, upserted in the heartbeat's own transaction; the seal **upserts then
locks** every bucket's row in its range — locking only rows that happen to exist leaves a
phantom for a bucket that received no heartbeat — and an ingest that then finds the fact SEALED
records an aggregated, idempotent late-arrival instead of dirtying the bucket. The handshake
fires only for a heartbeat that was **actually inserted**: every ingest path is
`ON CONFLICT DO NOTHING`, so gating on delivery rather than insertion would file an
already-counted duplicate as data the seal excluded. Membership is resolved **as of the
heartbeat's own bucket**, from `sli[]`, never as of now. A monitor in no service's SLI at that
instant writes nothing, which is what makes "zero services costs nothing" true rather than
rhetorical.

**`sealed_through` is defined by contiguity.** It is the greatest boundary `T` such that every
bucket before it exists and is sealed, so a materialization hole HOLDS the watermark instead of
being jumped over. A window therefore ends at `sealed_through`, not at `now` — a window ending
at `now` always contains an unsealed tail and would be permanently partial — and the live
signal is a separately named, explicitly unstable `current_health`.

**Maintenance is a retroactive declaration, and its two removal intents are different acts.**
`archive` says "no longer in active inventory" and never rewrites sealed past; the evaluator
reads a retained row over `[starts_at, min(ends_at, cancel_effective_at))` **regardless of
`archived_at`**, so a later recompute cannot silently turn an archive into an annul. `annul`
says "this exclusion was a mistake" and carries preview, audit and the raw-availability fence.
Creation has one intent, so it is decided purely by range. Beyond raw retention the mutation
fails closed with `409 unrecomputable_range` — never partially applied, and never a silent
no-op, which of the three outcomes is the worst because it looks like a completed command.
Every mutation invalidates in its own transaction (generation bump, coalesced ranges,
`repairing` reads, watermark retraction), and every repair batch commits under a
`maintenance_generation` CAS.

**A retroactive mutation carries a preview token whose confirm is a CAS over the SET.**
Re-reading the generations of the services already known proves those rows did not move; it
does not prove the affected set is the same one. The set is therefore re-resolved inside the
mutating transaction, required to be exactly equal, and — because row locks cannot protect a
row that does not exist yet — serialized by a project-scoped `service_membership` advisory
transaction lock that confirm and **every enumerated set-changing path** both take. An
`approximate` preview cannot be confirmed at all; `raw_floor` is checked as a monotonic
predicate, since the floor advances continuously and byte equality would make every token stale
by construction.

**Work is a set of ranges, not a current job.** A newer epoch queues its own disjoint range and
never cancels unfinished historical work; supersession is legal only by atomically assuming the
union; the epoch is resolved **per bucket**, not per job. Cancelling an unfinished backfill on
a newer epoch would strand its buckets and stall `sealed_through` at the hole forever. Work
partitions are bucket-aligned — a sub-bucket partition cannot coexist with a whole-bucket
primary key and a conservation CHECK — and the sub-bucket dimension is exercised by the
property test's oracle instead.

**Ownership is a migration, not a reuse.** `LeaderSession` is the file-provider path; the
scheduler holds its advisory lock on a pinned connection but writes through the pool, so a
deposed leader can still commit. Every service-fact batch runs on the **lock-owning
connection**. The election mechanism stays the existing advisory lock; what changes is that
lock ownership and the writing connection become the same thing. Cadence derives from ONE
mechanism — a per-slice deadline of `min(remaining cycle budget, max_dispatch_delay)` enforced
caller-side over the whole slice, with server timeouts re-derived from the remainder before
every statement, because `statement_timeout` bounds each statement and not a transaction.

**Storage: native UTC RANGE partitions in BOTH modes**, no hypertable and no compression in
phase 1. The reason is specific to this table rather than a general preference: it rewrites
SEALED rows weeks old under an audited recompute, which is exactly the access pattern
compressed chunks serve badly. One code path and retention by partition drop are worth more
here than reusing the heartbeat machinery. Revisit after phase 2 with measured workload.

**Bundle format 2, and the monitor slug it requires.** Services are a new top-level resource
map admitted only in a later format; format 1 stays valid. References are **monitor slugs**,
because monitors had no project-unique name, UUIDs are unusable as an authoring contract, and
restricting membership to file-owned monitors would contradict the coexistence matrix. The
migration is expand → deterministic collision-safe backfill → contract, and a **file-owned row
takes its provider source UID** so the same Git-tracked bundle yields the same slug on every
installation. The slug is immutable in phase 1. A bundle declaring a slug already held by a
UI-owned service is rejected with `service_slug_owned_by_ui` and the bundle keeps its
last-known-good; adoption never happens by name.

**Lock order extends FR-020's, it does not replace it.** The project `service_membership`
advisory lock is outermost, then referenced secret rows, then referenced routing rows taken
`FOR KEY SHARE` explicitly, then monitors by id, then services by id, then their declaration,
epoch and range rows, then ingest and fact rows by `(service_id, bucket_start)` ascending.
Routing rows take no *explicit* lock in the product today, but referential integrity takes
implicit ones in **two opposite directions** — `UpdateMonitor` acquires the monitor before its
FK reaches the policy, while deleting a policy acquires the policy first. That is a
pre-existing FR-012 hazard, recorded here rather than asserted away; FR-021's own paths take
the routing rows explicitly so they cannot join the cycle, and fixing `UpdateMonitor` is
backlog rather than a change FR-021 makes silently.

**Boundaries.** Service is not a security boundary — authorization stays at the project level.
It is not a service catalog; repository links, documentation, tech stack and deployment
metadata are rejected by the field-admission rule. Service SLOs **calculate and display but do
not alert** in phases 1–2: turning them on without an ownership rule would page twice for one
failure. Zero services is a valid installation state forever, and at zero services the feature
writes no rows and schedules no work.

## D-0160 — Credential dispatch r7: trusted carrier, execution binding, structural gate, carrier generations (FR-020)

A conformance audit of the shipped FR-020 implementation against its own spec found three
blockers and one P1, each independently confirmed. Two of the three were holes in the
specification, faithfully implemented — the r6 §9 matrix never asked for the tests that would
have caught them, which is why a green suite and a closed acceptance report coexisted with
them. This record fixes the amended contract; the spec carries the prose, D-0155 keeps the
two-keyring model and the exposure statements, and the AAD clause of D-0155 is superseded
here.

**Threat model — option A, chosen by the owner.** The carrier's own routing metadata is
**trusted**. A party able to author it — writing an arbitrary `pull_jobs` row, or publishing
into a legacy-generation queue — is permanently outside the model. The cost is recorded
without softening, because a future reader must not rediscover it as a surprise: someone
holding broker write, a credential typically distributed more widely than the core database's,
can emit a legacy-generation job for a credentialed monitor and obtain an anonymous probe that
reports Up. Monitoring can be made to lie by anyone who can write to the queue. The rejected
alternative, **option B**, was a root `job_auth` MAC over the canonical execution DTO, carrier
generation, monitor id, execution revision and job id, carried by **every** job in `enforced`
mode including credential-free ones — the only construction that actually closes the downgrade
path, since a credential-free job has zero ciphertexts and therefore no GCM tag and no MAC of
any kind for a per-field binding to work with. B was declined for now as full-job integrity:
it changes the non-credentialed dispatch path and requires a fully capable fleet, which makes
it materially larger than FR-020 and wrong to smuggle in under a credential-dispatch clause.
If B is later adopted it gets its own spec section and decision record; note that the
tri-state rule below forbids a *credential envelope* on a credential-free schema and does not
forbid a `job_auth` tag there.

**Execution binding.** The dispatch AAD binds the credential to its identity context *and* to
the execution the job describes: `body_digest`, a SHA-256 over a versioned canonical encoding
of a dedicated credential-execution DTO — deliberately not `domain.Monitor` and not raw
payload bytes. Without it, anyone able to write to the carrier keeps a valid envelope, edits
the target, and receives the plaintext credential at an endpoint of their choosing while the
executor's authentication passes, because nothing it verified had changed. The DTO covers what
decides where the credential goes, over what transport, and how many times it is transmitted:
`type`, `target`, `timeout`, `retries`, the `conditions` values, and every normalized
non-secret execution setting of the type. Identity parts already in the AAD are not
duplicated; state, display, cadence, delivery fields and the `*_ref` NAME are excluded because
the executor never reads them. The guarantee is therefore credential use and the remote side
effects of a credentialed probe — **not** a general signature of the job, and it must not be
widened into one silently.

**Canonical encoding, normative here so it survives independent of prose.** It reuses
`secret.CanonicalAAD` framing — a uvarint part count followed by uvarint-length-prefixed parts
— rather than introducing a second format. DTO version `1`, emitted first as the decimal ASCII
string `"1"`, numbered independently of the envelope version and the carrier generation. Fixed
field order, never map iteration order. Integers as decimal ASCII without padding, which
removes width and endianness as questions instead of answering them. Strings as raw UTF-8
under the length prefix. `conditions` as a count part followed by each condition's parts in
array order — order is fixed for determinism, not because reordering an all-must-pass set
changes the retry count. Config keys in byte-wise sorted order, key part then value part.
Absent and explicitly-set-to-default **encode identically**, since normalization runs before
sealing and either form may legitimately arrive. Both digests enter the outer AAD as raw
32-byte SHA-256 output, never hex and never base64. Golden vectors pin all of it, because two
implementations can each be self-consistent and mutually incompatible.

**Structural gate — the check does not live behind the branch it protects.** The severe defect
was not which fields an envelope carries but who decides whether one is required: an executor
that opens an envelope only when present executes a job whose envelope was removed entirely as
an ordinary job, so a `redis` monitor skips `AUTH`, PINGs, and an auth-less target answers Up.
Deleting one JSON member turned an authenticated check into an anonymous one, with no key and
no forgery. Therefore: one mandatory `Validate/Materialize` entrypoint that every executor
path reaches the prober through, so a new call site that forgets the check cannot be written;
the credential requirement resolved **tri-state** from the effective schema — required (exact
non-empty field set), forbidden (an envelope present at all is a failure, which is the correct
handling of `rabbitmq mode: amqp`), invalid (anything else); and every failure typed
`probe_error` raised before any connection to the target. A credential that decrypts to a
zero-length value counts as missing, or the rule is bypassable by content instead of by shape.
`field_set_digest` binds the canonical sorted field-name set into each field's AAD; with
today's single-field schemas the exact policy is the operative protection and the digest is a
primitive so that a future multi-field envelope cannot be partially truncated without a second
security review — stated plainly rather than as defence-in-depth. Verification order is fixed:
structural gate, then field-set policy, then AEAD.

**Carrier generations, named rather than delegated.** Changing what the AAD binds is a wire
change, so it ships as envelope `v: 2` and never as a redefinition of `v: 1`; a silent
redefinition breaks a rolling upgrade in both directions. Today's `protocol_version = 2` /
`checks.{jobs,tests}.v2.<region>` / `/v2/{jobs,tests}` carrier already *means* envelope `v: 1`
and every capability-1 executor consumes it, so **carrier generation 3** (`protocol_version =
3`, `.v3` queues, `/v3` endpoints) carries envelope `v: 2`, and a capability-1 executor
neither consumes nor claims it — asserted as physical isolation, not as a capability
predicate, since a predicate does not stop a consumer already sitting on a shared queue.
Capability is generational (`credential_envelope: 2`), and the existential readiness check
counts executors capable of the generation core is about to emit. Decisively, the carrier
generation is **trusted metadata delivered out of band** — stamped by the transport adapter
from the queue a message was consumed from, or the generation the server selected for a claimed
row — and is never read from `job.ProtocolVersion`, which is body content an attacker edits;
sourcing it from the payload would make the whole gate self-referential.

**The wire barrier is one-directional.** It exists to stop an incapable executor from
receiving a newer generation and must never stop a capable one from receiving an older. The
AMQP half was already specified ("capable workers consume v1 and v2 during the transition");
its pull mirror was missing, and that omission is the availability blocker: non-credentialed
monitors are enqueued as legacy rows, an `enforced` region's agent is necessarily capable, and
an agent claiming only the newest generation leaves every ordinary monitor's row to expire by
TTL — no probe, no heartbeat, no DOWN, no alert. Enabling a security feature must never
silently disable monitoring. A capable agent therefore claims through one capability-scoped
operation that atomically leases every generation at or below its declared capability under a
single shared `max` and one lease, with the generation stamped by the server; sequential
long-polls are insufficient because an empty first class sleeps out the window, and
independent per-class claims over-lease. The same applies to `pull_tests`, not jobs alone.

**Backoff.** The floor covers every path that ends without a dispatched job, publish/enqueue
failure included, not materialization alone — a broker outage is the one fault guaranteed to
hit every credentialed monitor at once, and leaving that path uncovered turns each tick into a
full read-decrypt-seal storm. Semantics are state, not rate: failure increments the
consecutive-failure counter, sets next-eligible to `now + backoff` and does not mark the probe
sent; success resets the counter, marks it sent and restores ordinary cadence. "Not marked as
sent" and "eligible again next tick" are different statements.

**Acceptance.** FR-020/NFR-015 return to `IN_PROGRESS` and the iter-0116 acceptance is
withdrawn. That report stands as written and is superseded, not corrected — its matrix could
not have caught these defects. Re-acceptance requires a fresh iteration report against the
amended §9, with each regression **verified to fail when its fix is reverted**: all three
blockers passed a green suite, so "the tests are green" is not evidence here. Until the claim
fix ships, the feature must not be enabled in a region served by an HTTP-pull agent.

## D-0161 — Service fact adoption: declared automatic bound, under-lock enforcement, one shipped operator artifact (FR-021)

The DEFAULT-month adoption mechanism (spec §10.11, added by this decision) carries an
externally observable limit and an operator-only recovery mode; both are contract, so both
are recorded here rather than only in code and the runbook.

**The automatic bound.** The fenced cutover's workload is inherently O(rows still in DEFAULT
for the month) — under native partitioning, removing a row from DEFAULT before ATTACH hides
it from the parent, so the fence cannot shrink incrementally without lying to readers. The
automatic cadence therefore declares `adoption_fence_max_rows = 100000` and refuses larger
months. The gate is enforced **twice**: an unlocked preflight (cheap, takes no parent lock,
stops the doomed-fence-every-cadence loop), and a second count **under the parent's ACCESS
EXCLUSIVE lock** before the sweep — the unlocked count alone was a TOCTOU (a writer commits
between count and lock), reproduced by a deterministic regression. Only the under-lock check
makes the bound an invariant; the preflight is a courtesy.

**The bound is a row count, not a wall-clock guarantee.** A month under 100k rows on slow
storage may exhaust the 5s fence budget every cadence. The operator path therefore covers
ANY persistent automatic-adoption failure after quiescence, not only the oversize refusal.

**One shipped artifact, not a documented procedure.** Recovery is
`cerbix adopt-fact-month --config <path> --month YYYY-MM [--timeout 10m]`: the SAME adoption
code path (copy-authoritative staging, deadline-enveloped fence) with an operator-chosen
budget and the gate off, validated month input, idempotent rerun. It is tested end-to-end
through the real command line against its own migrated database, and the store-level
regression proves it adopts a month the automatic gate refuses. The previous runbook psql
skeleton (placeholders, "<all non-PK columns>") is deleted: an operator artifact that cannot
be executed as written is documentation debt wearing a recovery procedure's name.

**Staging lifecycle keys are verified by their COMPLETE definition** before sweeps are
skipped: constraint type, validation, both column-name arrays in declared order, referenced
relation, ON DELETE and ON UPDATE actions, MATCH type, deferrability and its initial state.
Anything reserved-named and non-exact — including an unvalidated key of the correct shape —
fails closed. Name-plus-partial-predicate matching accepted foreign keys over the wrong
columns (deleting a service would not cascade its staging facts) and an INITIALLY IMMEDIATE
epoch key; both shapes are regressions now.

## D-0162 — No production canceller for maintenance-origin repair ranges in phase 1 (FR-021, closes the iter-0133 "yet")

iter-0133 stated honestly that cancel-by-origin exists at the store layer but "NO production
canceller for maintenance origins is wired yet". The "yet" implied future work; this record
closes it as a DECISION instead: phase 1 wires no production canceller, by design.

The spec requires range cancellation in exactly two places — ranges of a
`superseded_before_effect` row are cancelled in that same transaction (§6.5), and service or
project deletion cancels its ranges inside the deleting transaction (§16) — and explicitly
forbids the generalization: "a newer epoch or revision enqueues its own range; it never
cancels unfinished historical ranges" (§10.8). A maintenance archive already enqueues its own
repair over the truncated span; cancelling the earlier window's pending range by origin would
discard recompute work another window may still require (the exact-union hazard iter-0133
documented when it made ambiguous merged origins NULL). Recomputing a range that turned out
unnecessary costs bounded work; discarding one that was necessary costs correctness. The
store-layer capability and its tests remain as the safety proof that cancel-by-origin is
sound where a future phase needs it (e.g. phase-5 alert ownership); wiring it is that
phase's decision, not an omission of this one.

## D-0163 — Service reliability reporting surface: fixed burn pair, on-read rollups, era-anchored insufficiency (FR-021 phase 2, iter-0138)

Three implementation choices inside §11/§12's stated contract are externally observable, so
they are owned here (the [152] lesson: an observable behavior that lives only in code is a
finding waiting to be written).

**The reporting burn pair is FIXED at 1h/6h.** §11.1 says "burn rate = multi-window, reusing
the existing rule shapes", and §13 forbids service burn ALERTING until phase 5 — the shared
`burn_rules` column is schema-rejected for the service scope (00077). What remains for
phase 2 is display, and the displayed pair is the approved mock's card ("Burn 1h 0.4x /
6h 0.6x"): windows `[sealed_through − w, sealed_through)`, quoted only when the equivalent
real-time window holds any sealed time (else `insufficient_sealed_coverage`), never
aggregated across a definition boundary, never 0× from an empty denominator. Phase 5 may
replace the fixed pair with rule-derived windows; that is an alerting-ownership decision.

**Hour/day rollups are computed on read, not stored.** §10.2 defines rollups as exact
associative duration sums keyed by epoch; it mandates behavior, not a table. A 90d window is
one indexed aggregate over ≤130k narrow rows per service; materialized rollup tables would
add a write path, a consistency obligation and a repair story with no acceptance invariant
asking for them. `ServiceReliabilitySeries` groups by (step × epoch × state) — provisional
time rolls up separately and an epoch boundary inside one step yields one point per epoch,
which is exactly the never-merge rule. Revisit only with measured read pressure (same
posture as D-0159's hypertable deferral).

**Insufficiency anchors at `era_start`** (00071): a window reaching left of the current
contiguous era is `insufficient_history` with no window aggregate — the number "90d" would
otherwise silently mean "the 3d that exist". Segments still carry their own numbers, so the
UI can show what exists without the report vouching for what does not. The window aggregate
also stays withheld across definition revisions even when a diagnostics-only declaration
caused the boundary (invariant 43's rule is categorical; invariant 42 is honored where it
binds — no reliability NUMBER moves, and the epoch snapshot hash proves the SLI semantics
did not).

## D-0164 — current_health is a right-continuous POINT evaluation at the DB as_of (FR-021 phase 2, iter-0140)

The live signal's semantics are a reliability contract and are owned here and in spec §11.3,
not only in iteration prose ([184] P2-5).

**The contract.** `current_health` is the declared SLI semantics evaluated AT one instant:
the DB clock's `as_of` (the report snapshot's own `statement_timestamp()`), right-continuous
— an observation, a stale deadline, a maintenance edge or an epoch boundary effective
exactly at as_of is included. Observations after as_of (accepted by ingest under
`result.allowed_skew`) are excluded. The evaluation goes through the SAME member and
aggregation semantics as the facts — `reliability.StateAt` is composed from the reducer's
own pieces (`buildSeries` → `censusAt(t)` → `aggregate`), so the live signal and a
materialized bucket can never disagree about what a member's state at an instant means.

**No fixed-width approximation.** Two window shapes were tried and rejected in review:
`[as_of − 1µs, as_of)` is the left limit ([175]), and `[as_of, as_of + 1µs)` is splittable —
freshness durations parse through `time.ParseDuration` with nanosecond granularity, so a
derived stale deadline can fall strictly inside ANY fixed width ([178]). A point evaluation
is not an optimization of the window; it is the only shape the precision model admits.

**Unchanged neighbors.** The categorical mapping stays the four declared values
(down/degraded/healthy/unknown, with an exclusion in force at as_of reading unknown); the
signal remains `unstable: true` by construction and never reads stored facts; the §11
reporting numbers remain sealed-facts-only and are untouched by this decision.

## D-0165 — SLA objectives live in the open interval (0,100) (FR-021, iter-0142)

The FINAL iter-0141 review ([195]) found a fix-induced P0: widening the objective column to
numeric(7,4) (00078) made objective=100 a STORABLE live configuration, and the shared budget
math deliberately answers a zero allowed budget with zero — `sla.BurnRate(100, …) = 0` and
`ErrorBudget.burned_percent = 0` — so a monitor at objective=100 with burn alerting enabled
would ride out a total outage with burn 0×, no alert, and the service surface printing
`rate:0, status:ok`: the exact never-0× lie §11/§13 exist to prevent. numeric(6,4) had been
rejecting 100 by ACCIDENT of precision, not by contract.

**The decision (owner option A, reviewer-recommended).** Objectives are the OPEN interval
(0,100): the RAW input must satisfy >0 and <100 (100.00004 is rejected as said, never rounded
into range), the canonical value — half-up at four decimals, the numeric(7,4) representation
— must remain inside (0,100) as well (99.99995 rounds to 100 and is rejected), and the
maximum admissible objective is 99.9999. `domain.CanonicalObjective` is the one rule for
every scope; migration 00079 tightens the schema CHECK to the same open bound (clamping the
never-released 100 rows first), so every writer — API or not — meets the same fence. The
allowed<=0 sentinel in `sla.BurnRate`/`ErrorBudget` becomes unreachable by construction and
stays as robustness.

**Deliberately out of scope.** A true zero-error-budget objective (alerting semantics for
"any down second is a breach", a finite wire verdict instead of ∞×) is a legitimate future
feature and needs its own cross-scope specification; admitting the value without those
semantics is how the P0 happened.

## D-0166 — FR-021 phase 3: the impact graph outside the declaration, the fenced correlation topic, the bounded one-transaction attempt (iter-0146)

Phase 3's §14 amendment (design APPROVED [269] after rounds [260]/[265]/[267]) chose, and
iter-0146 implemented, the load-bearing decisions of the service impact graph:

- **Edges live OUTSIDE both reliability axes.** A `service_dependencies` edge creates no
  definition revision and no epoch, enters no canonical hash, and moves no sealed fact —
  §6.2/6.3 already classified dependency wiring as delivery, not measurement. Consequence:
  the edge set needs its OWN concurrency token (`graph_generation`, 409 on stale,
  first-committer-wins) and its own per-delta audit rows with typed actor attribution
  (canonical `actor_user_id`/`via_token` + a machine label for the file track), because the
  declaration CAS and `created_by` cannot testify for it.
- **One deletion contract, the §15.1 shape.** A desired file edge pins its target: an
  unresolvable `depends_on` slug rejects the bundle keeping a last-known-good that stays
  literally true; deleting a pinned target is a 409 naming the provider; an ORPHANED managed
  service still pins (orphans ARE file-owned LKG).
- **Correlation is a dedicated FENCED outbox topic** (`incident_correlation`), enqueued in
  the incident's opening transaction — never a rider on webhook delivery. The claim fence is
  schema, not runbook: an immutable `fenced` column plus `CHECK (NOT fenced OR status <>
  'pending')` make the legacy-claimable state unrepresentable for the row's whole lifetime
  (enqueue, retry, dead, both replays), so a mixed-version fleet can neither claim,
  attempt-burn nor dead-letter a topic it cannot dispatch. Every post-phase-2 topic inherits
  this by the one topic→class map (`domain.FencedTopic`).
- **The attempt is ONE bounded transaction.** membership→graph advisory locks give it a
  single committed snapshot; incident rows lock FOR UPDATE ascending with the open state
  rechecked under the lock; links and 🕸 notes commit together or not at all; the canonical
  path is endpoint-inclusive, root-first, shortest-then-lexicographic, ≤ 11 slugs. Witnesses
  are bounded per REACHABLE endpoint service (5, oldest by `(started_at, id)`, SQL-capped
  and endpoint-scoped reads per the [283] disposition), with overflow returned, logged and
  counted (`cerbix_service_impact_witness_overflow_total`) — never silent, and never counted
  for services the anchor cannot reach.
- **Impacts are an authenticated-detail enrichment**, tenant-scoped at the store boundary
  (the link rows' own `project_id`), absent from the incident list and from every public and
  internal status-page projection until phase 4 opts in.

Review chain: implementation round 1/2 [276] (1 P0 + 5 P1 + 1 P2, all closed), fix pass
[281], FINAL 2/2 [283] (one residual P1 — witness scoping — closed under the [263]-delegated
disposition recorded in iter-0146 §9; per the two-round contract no third review round ran).

## D-0167 — FR-021 phase 4: the status-page service projection, `no_data`, and the composite lifecycle (iter-0150)

Phase 4's §15.0/§15.5 amendment (design gate cleared [310], owner-approved mock
`design/mock-status-projection.html`) chose, and iter-0150 implemented, the following:

- **The third component source is a DISCRIMINATOR, not a second populated column.**
  `components.source ∈ {monitor, service, manual}` decides what the line renders from; the
  inactive binding stays DORMANT so a conversion is revertible without re-choosing what it
  replaced. An exclusivity CHECK over "which column is set" cannot coexist with reversibility,
  so the CHECK asserts only that the ACTIVE binding exists. Consequence: every read path must
  branch on the discriminator — the shipped renderer branched on `monitor_id != ""` and would
  have kept publishing the OLD monitor of a converted component, and its
  `if ManualStatus != ""` override would have let a dormant manual status beat a live service.
- **`no_data` is a public status that is deliberately OUTSIDE the severity ladder.** Whether
  "we do not know" is better or worse than declared maintenance is a false comparison, so the
  page summary is **worst-of-MEASURED plus an unmeasured COUNT** (`summary`, `summary_state`,
  `unmeasured_count`) rather than a single ordered value. `severity()`'s unknown default moved
  from 0 (operational) to worst-measured, so a future enum value can never silently mean fine.
  `no_data` is never operator-settable: refused by CHECK and by both write paths.
- **Three inherited public statements are changed on purpose (§17).** A `pending` monitor
  component, a manual component with no status, and an EMPTY page all used to publish
  `operational`. They now publish `no_data` / `no_data` / `summary_state: empty`. The unit tests
  that pinned the old answers were REWRITTEN and each additionally asserts the old answer is
  gone, so a regression fails loudly instead of passing quietly.
- **Conversion is explicit in both directions, previewed, and structurally CAS-fenced.** Consent
  binds `components.revision` AND `status_pages.component_generation`, the latter bumped by ANY
  component mutation on the page — a NEIGHBOUR's edit invalidates a preview, because the preview
  showed the page SUMMARY. Mismatch is `409 page_configuration_stale` (first committer wins);
  confirming without the tokens is `400`. Preview and confirm share ONE validator, so a preview
  cannot promise what the confirmation then refuses, and the preview renders through the SAME
  resolver as the public page, so it cannot predict a page the server would not produce.
- **Deletion is specified per case, and the refusals are typed.** An actively-bound service is
  `RESTRICT` → a conflict naming the page and the component (`SET NULL` would be the automatic
  conversion this decision forbids; `CASCADE` would delete a customer-visible row for an internal
  reason). A DORMANT service binding is swept in the same transaction with both counters bumped,
  which is what makes `RESTRICT` mean *rendered* rather than *ever mentioned*. The monitor keeps
  its shipped `SET NULL (monitor_id)` — an inherited automatic exception, recorded as one.
- **The page-scope rule is a DEFERRED CONSTRAINT TRIGGER on both sides.** "A project-scoped page
  admits only that project's components" must read `status_pages`, which a CHECK cannot, and the
  `(status_page_id, org_id)` FK proves only the org. One trigger refuses a foreign component;
  the other refuses NARROWING a page's scope while it holds one. Deferred, so a legitimate
  multi-statement rearrangement is judged at COMMIT on its final state.
- **The public render is bounded in ROWS, not merely in statements.** One batched projection with
  a constant statement count, a per-page ceiling persisted as `max(50, current)` that may only
  SHRINK, and an absolute fail-closed public ceiling of 500 in code above which the page returns
  `status_page_over_safe_limit` naming the count and the limit — never a truncated subset posing
  as the whole page, while the authenticated view still lists everything.
- **Absence carries its reason, and a failed read is not absence.** The 90-day strip is built
  from SEALED buckets only and ends at `sealed_through`, never at "now"; `sealed_in_window`, not
  "any fact exists", decides whether a strip is drawn; days with zero decidable time are ABSENT
  rather than 0%. An ACTIVE service the projection cannot find degrades the component with a
  logged, named failure (`service_unreadable`) instead of publishing the calm statement
  `no_data`, which would present a bug as a fact.
- **The composite stays visible and active; retiring is a separate, explicit act** (owner
  decision). The link is ONE stored fact (`monitors.superseded_by_service_id`, tenant-composite
  FK, `SET NULL` on service deletion) rendered from BOTH ends, so there is no pair to fall out of
  sync. Recording it bumps no `execution_revision` — an annotation must not force a re-probe.
  `retire` is ONE transaction setting `retired_at` (lifecycle) **and** `enabled = false`
  (execution), with the config fence, the `state_sequence` advance that makes a queued transition
  stale at delivery, and the epoch fan-out for every referencing service; `retired_at` alone
  would leave a "retired" monitor probing and paging. Reversible, audited, refused for a
  file-managed monitor. Composite→service conversion is one serialized transaction (composite
  `FOR UPDATE` → service → declaration → link → audit), idempotent on the link column, and a slug
  collision is a conflict naming the existing slug rather than a suffixed twin.
- **The `quorum` translation refuses rather than approximates.** A composite states its threshold
  as a DOWN-vote count and a service as a minimum GOOD count, so the arithmetic is
  `degraded_min = n − M + 1`; transcribing the number would invert the meaning. A flat "M of N
  children" is not expressible as per-region quorum plus a region rollup, so a quorum composite
  whose children span regions is refused with a named reason. A conversion that quietly changes
  what "down" means on a customer-facing page is worse than no conversion tool.
- **`RetiredAt` and `SupersededByServiceID` are classified SemanticPresentation.** `RetiredAt`
  does accompany an execution change, but `enabled` is the field that CARRIES it and is already
  SemanticEvaluation; listing both would hash the same fact twice and would let a future path
  that set only `retired_at` register as a semantic change while the monitor kept probing.

- **The tenant rule keys on the PRESENCE OF A BINDING, and the page-scope guard holds a LOCK.**
  Two corrections from review round 1/2 [314] that the first implementation got wrong in ways worth
  recording: a composite FK is MATCH SIMPLE, so `(monitor_id, source_project)` with a NULL project
  is not enforced at all — keying the CHECK on `source` therefore left a dormant foreign binding
  unconstrained on both sides. And `DEFERRABLE INITIALLY DEFERRED` is not a lock: each transaction
  validates at COMMIT against its OWN snapshot, so an insert and a page-narrowing could both commit
  a state neither would have accepted. The guard takes the page row `FOR UPDATE`; the narrowing side
  already row-locks it.
- **Deleting a monitor that a page RENDERS is a DB-level transition.** `ON DELETE SET NULL
  (monitor_id)` clears the binding and leaves `source='monitor'`, which the active-binding CHECK
  rejects — so the first implementation broke an ordinary monitor delete outright. The transition
  (source → manual, binding cleared, `source_project` kept only while another binding needs it) is a
  `BEFORE DELETE` trigger on `monitors`, because the D-0150 project cascade deletes monitors through
  FK actions with NO application on the path. For the same reason both counters — the page generation
  and each component's revision — are DB-owned: "any component mutation bumps the generation" cannot
  be application discipline while FK actions are part of the contract.
- **The ceiling's rule is "no write may create headroom", not monotonicity.** It is LOWERED by every
  removal and may never exceed `max(50, current count)`, which is by construction a value at which
  an oversized page is still full. A flat no-increase rule could not express the legacy state the
  backfill itself creates.
- **The projection is PAGE-scoped, and the withholding rule has ONE owner.** An org-level page spans
  projects, so a per-project batch is neither one snapshot nor a constant statement count. And
  `uptime_90d` with its `withheld_reason` comes from `decideServiceWindow`, EXTRACTED from the
  authenticated report so the page cannot quote what the report withholds; the page mirrors the
  report's branch order for the no-watermark and no-SLI pre-checks. The monitor half is batched too:
  amplification left in place is amplification claimed gone. A short-TTL render cache keyed to page +
  access shape + unlisted token is the RATE bound the ceiling cannot provide.
- **A failed read is PUBLICLY distinguishable and counted.** An active service the projection cannot
  find carries `unavailable: true` on the public payload and increments
  `cerbix_status_page_component_unreadable_total`. Bytes identical to a calm `no_data` were the
  confusion invariant 71a exists to forbid; the marker says something about cerbix, not about the
  customer's topology.
- **Converting a composite REQUIRES an explicit `sli[]`, and refuses partial child loss.** §15.5 says
  explicit confirmation and never a silent "all children", so there is no default anywhere in the
  stack. Every live child joins the operational CONTEXT regardless. A composite whose declared
  children are not all live is refused by name, since `all` over 2 is not `all` over 3. The quorum
  translation is `degraded_min = n − M + 1` with `healthy_min` EQUAL to it — the exact binary mapping,
  because a composite has two states and adding a degraded band would report more than it did on a
  customer-facing page. Widening that vocabulary is a later owner decision with a preview.
- **The lifecycle actions are COMPOSITE-only, and §15.1's "may MUTATE a resource" means
  DECLARATION authority — not every column of the row.** This narrowing is the substance of the
  [316] disposition and it is what makes the file-ownership split coherent: `retire`/`reactivate`
  and the conversion refuse for a file-managed composite because they write `enabled` or create a
  declaration, both of which a reapply would restate; the `superseded_by_service_id` annotation is
  PERMITTED there because it is not in bundle format 2, enters no canonical hash or generation, and
  cannot be contradicted by any reconcile. Refusing it would remove the only way to annotate a
  file-managed composite while protecting nothing.

Impact links stay NON-PUBLIC (phase 4 declined projecting them): a status page would publish the
dependency graph, and that needs its own owner decision in a later phase.

Review chain: round 1/2 [314] REJECTED with 2 P0 + 7 P1 + 2 P2; the disposition on the single
contested item is [315]/[316] (split accepted, with §15.1 narrowed as above). Round 2 carries a
direct regression for every finding — including the two-session page-scope race in both commit
orders, the rendered-monitor delete under API and cascade, a MEASURED statement count, and
page-versus-report agreement on the withheld reason.

## D-0168 — FR-021 phase 5: alerting ownership, per-signal ARMED delegation, and the refusal to suppress a close (iter-0151)

Phase 5 answers the question §13 deliberately left open: monitor burn alerting already pages, so
turning on service alerts without an ownership rule pages twice for one failure. The owner's decisions
(2026-08-17) and the two design rounds ([324] REJECTED before code, [326] REJECTED with a closed
authority ledger, amended and dispositioned here under the delegated authority) produce the following.

**The owner's seven decisions.** Ownership is DECLARED at the service and defaults to OFF; a service
alerts on TWO signals — a LIVE health transition and a SEALED burn breach; the alert NOTIFIES and opens
no incident; service burn rules are the same `BurnRule` monitors use; routing is NARROWED to the
service's on-call schedule or the project's channels with escalation POLICIES deferred;
`escalation_step` IS suppressed for a delegated monitor; and losing sight of a paged service CLOSES the
alert with a named reason rather than as "recovered".

- **`owns_paging` alone silences NOTHING.** Delegation is per SIGNAL and must be ARMED: owns_paging ∧ a
  policy that can page the current state ∧ a QUOTABLE last verdict ∧ the CURRENT (config generation,
  target generation, effective definition revision, canonical rule key) ∧ a FRESH DB-clock lease ∧
  effective ROUTABILITY. Three findings drove each clause and each one could lose a page:
  a HOLD is a *successful* evaluation, so quotability had to join arming or a CLEAR rule holding on
  `nothing_sealed` would mute a member's real burn alert while being structurally unable to fire; an
  unroutable service satisfied every other clause and delivered nothing; and a missing or stale
  evaluation is not evidence of coverage. Everything ambiguous FAILS OPEN and pages.
- **A RECOVERY is never suppressed.** Arming is evaluated at delivery and changes between an onset and
  its close: a monitor DOWN can fail-open while dis-armed, the evaluator can then arm, and the matching
  UP would be muted — leaving a recipient holding a DOWN that can never end. Only onset-like events
  (DOWN transitions and reminders, `escalation_step`, `firing = true` burn) are suppressible. An
  episode table per monitor per owner per generation would be the general fix; refusing to suppress
  closes is the fix that cannot be got wrong, and a duplicate "recovered" is strictly safer than an
  unmatched "down".
- **`escalation_step` is in the suppressed set, and that is the finding that mattered most.** The
  worker already drops the flat DOWN `monitor_transition` for a monitor with an escalation policy and
  auto-incident; its real pages are the ladder's steps over the open incident. Suppressing only
  transitions and burn would have delivered phase 5's promise to everyone EXCEPT the installations that
  page correctly. The ladder's rows, progress and incident are untouched; only delivery is muted.
- **Membership is the CURRENT EFFECTIVE definition revision**, never `service_member_refs`, which is
  rewritten at authoring time while a declaration becomes effective on its bucket boundary. Otherwise a
  monitor added at 12:00:30 for a 12:01 revision is suppressed at 12:00:40 by a service that is still
  measuring the old definition and cannot replace the page. Arming is stamped with the revision id.
- **Delivery is at-least-once, ordered — and the spec says so.** The first draft claimed a
  same-transaction enqueue made redelivery harmless; the worker explicitly permits a duplicate external
  send when the delivery mark fails. Phase 5 adds `emitted_seq` per service and a per-rule sequence,
  both checked at delivery so a retried onset cannot re-announce a state the service has left. A CLOSE
  whose ONSET never reached a channel is still delivered.
- **Routing is narrowed because "the ladder applies unchanged" was not implementable.** Escalation steps
  are defined relative to an incident start with acknowledgement, progress and repeat state, and owner
  decision 3 forbids service incidents. Phase 5 resolves the service's on-call schedule at ONSET, falls
  back to the project's channels, and leaves `services.escalation_policy_id` unconsumed with the UI
  saying so. A later phase needs a durable non-incident occurrence before "ladder" means anything here.
- **An ONSET creates a durable episode with an IMMUTABLE recipient snapshot**, which is what makes a
  close both correct and possible: a rotating schedule must not receive a close for an onset it never
  saw, and a close must still deliver after the service, target or rule has been deleted — so every
  removal path enqueues its close in the same transaction, with a named reason. Losing the ROUTE closes
  nothing: dis-arming is not a recovery.
- **The service burn latch is normalized** per `(service_id, project_id, sla_target_id, rule_key)` with
  the canonical key excluding every server-owned field, so a rule cannot change identity by firing. The
  MONITOR latch stays inside `sla_targets.burn_rules` exactly as it is: phase 5 changes no monitor
  behaviour.
- **Bounded slices, not one global transaction.** Per-project caps do not bound an installation, and a
  single global snapshot can monopolize the leader's connection. Keyset slices with a hard cap, a
  per-slice deadline, cursor fairness, constant statements per slice, per-project maintenance scoping,
  and generation/lease CAS against a deposed evaluator.
- **A stalled evaluator marks the SCHEDULER not-ready, never the API.** The stall is exactly the state
  in which delegation dis-arms and members resume paging; reporting ready would hide a degradation,
  while taking the API out of rotation would turn it into an outage.

`any`/`quorum` tolerated failure is APPROVED as designed and is not reopened: with live coverage armed
and fresh, one DOWN member under a declaration that tolerates it does not phone-page, and the monitor
stays red with its incident open and its delegation named.

Review chain: design round 1/2 [324] REJECTED before code (4 P0 + 10 P1 + 2 P2), revised design [325],
FINAL round 2/2 [326] REJECTED with a closed authority ledger and no third round. The P0/P1 items of
[326] are amended in §16.1/§16.4/§16.4a/§16.4b/§16.5a/§16.6a/§16.6b with invariants 75–91 and the
§16.10 test matrix; implementation proceeds under that record.

## D-0169 — FR-021 closes at a measured boundary, and §16.9 becomes an explicit non-goal (iter-0153)

Phases 1–5 of `func-service-reliability.md` are shipped and reviewed, and the question this decision
answers is not "is there more to build" — there always is — but **what "FR-021 is done" is allowed to
mean**. Until iter-0153 it meant a chain of review verdicts: nothing in the repository mapped the
spec's 91 numbered acceptance invariants (§19 for phases 1–4, §16.8 for phase 5) or its 24-scenario
required matrix (§16.10) to a test, 36 invariant numbers appeared in no document at all, and the only
way to audit the claim was to re-read thirty immutable iteration reports.

**The requirement closes against a discharge map, not against a memory.** `docs/traceability.md`
carries one row per invariant and per required scenario, naming the test that holds it, and
`make docs-check` fails when a row cites a test the tree lacks or when any of the 115 numbers has no
row. Building the map found seven properties that were specified, believed and unpinned — invariants
1, 25, 27, 35, 37, 78 and 86 — none of them a product defect, each now carrying a test that fails
behaviourally when the property is broken (iter-0153 §2.7). Invariant 86 is the sharpest evidence for
why the map is the boundary rather than a formality: the spec's own required matrix demanded that
regression, and it did not exist.

**§16.9 is an explicit NON-GOAL of FR-021, not unfinished work inside it.** Service incidents (owner
decision 3), escalation POLICIES for services (owner decision 5 — which first needs a durable
non-incident occurrence carrying started/resolved/ack/progress/repeat state, a subsystem and not a
refinement), retroactive alerting, cross-project delegation, per-member severity inside a service page,
and suppression beyond the three named topics are all deliberately outside this requirement. They stay
described in §16.9 so the reasoning survives; when any of them is commissioned it opens its OWN
requirement with its own acceptance invariants, rather than reopening a closed one. A requirement that
stays open for everything it could have been never closes, and the checklist stops meaning anything —
which this repository had already demonstrated: 28 status rows sat at `IN_PROGRESS` for eight
iterations after the verdict that closed them (iter-0153 §2.6).

**Consequence.** `FR-021` and `NFR-016` are `DONE` in `docs/status.md`; the discharge tables are the
evidence and the gate keeps them true; the deferred set is a stated non-goal with a named owner
decision behind each item. Invariant 47 is superseded rather than deleted (§16.4 lifts the phase-2
rejection it asserted), because other documents cite invariants by number.

## D-0170 — FR-022 commissioned: a Service can own an incident, and its six decisions are delegated (iter-0155)

The owner commissioned Service incidents — the first of §16.9's six deferred items — and delegated the six
decisions the design-gate input had raised to that document's own recommendations. Recording that is the
point of this entry: each decision below was a JUDGEMENT of mine that the owner adopted, not a fact derived
from the code, and a later reader deserves to know which is which.

**The six, resolved.** A service alert AUTO-OPENS an incident, on a LIVE onset only and never on a burn
breach, under the same three gates that decide whether it pages at all (armed coverage, `confirm_evaluations`,
a fresh DB-clock lease). The incident lives in the EXISTING `incidents` table under an exclusive anchor —
at most one of `monitor_id` / `service_id`, because a manual project-level incident has neither today —
rather than a second table, since phase 4 already paid for the alternative when an implicit
`monitor_id != ""` discriminator published a converted component's old monitor. A service incident does
NOTHING to its members' incidents: two timelines, both true. It reaches the status page; its §14 impact
links do not, keeping §15.0's refusal to publish internal topology. The escalation ladder does not ride it
in this requirement — §16.9's escalation item needs a durable non-incident occurrence first, and bundling
them would smuggle a subsystem in through a feature. And a postmortem snapshots its member set at open
time, the same device phase 5 used for an episode's recipients, because a postmortem is read after the
world moved.

**The consequence that matters more than any of them: FR-022 SUPERSEDES FR-021 invariant 86** — "no service
alert opens, resolves or annotates an incident". That invariant is true today and held by
`TestAServiceAlertNeverTouchesTheIncidentTables`, written in iter-0155. So the spec requires three things to
move IN THE SAME CHANGE as the code: invariant 86 gains a SUPERSEDED note keeping its number, its discharge
row moves to the test holding the new rule, and that test is REWRITTEN rather than deleted — exactly as
phase 5 rewrote the phase-2 burn-rejection test when it inverted. Without that, this repository repeats
invariant 47's history: a spec asserting the opposite of what its own code does, left standing for a phase.

**Consequence.** `FR-022`/`NFR-017` stay `TODO` until the spec is reviewed adversarially and a UI mock is
approved; the spec carries fourteen numbered acceptance invariants and a required test matrix written
before the code, because iter-0155 established that an unmapped invariant list is worth nothing and that
`make docs-check` is what keeps a mapped one true.

## D-0171 — FR-022 closes against its own enforced discharge map, and one of its invariants was corrected rather than implemented (2026-08-19)

**Context.** FR-021 closed against a map of 91 invariants and 24 scenarios that `make docs-check` refuses to
let go stale (D-0169), after that arc found 36 invariant numbers cited nowhere. FR-022 was written with
sixteen numbered invariants and a sixteen-line test matrix BEFORE the code, so the same instrument applies
to it — and applying it before closing, rather than after, is the whole point.

**Decision.** FR-022 and NFR-017 are DONE, closed against the map in `traceability.md`: every one of the
sixteen §6 invariants and the sixteen §7 scenarios has a row naming a test that exists, enforced by the gate
(now 91 + 24 + 16 + 16). Two consequences are recorded as part of the closure rather than left implicit:

1. **Invariant 14 was CORRECTED, not implemented.** As written — "every write is audited with its actor and
   tenant, in the mutating transaction" — it was false of the product, not merely unimplemented: incident
   writes carry no audit row for EITHER anchor. Implementing it would have meant changing the monitor path
   inside a requirement forbidden from touching it. The spec keeps the number, quotes the original, states
   what FR-022 does promise (the absence of asymmetry, pinned by a test), and **an audit trail for incident
   writes is hereby recorded as an open gap needing its own requirement**, where it can be designed for both
   anchors at once. It is NOT in this repository yet and nothing in FR-022 provides it.
2. **Invariant 8's discharge is honest about what rests on judgement.** "Byte-identical" is discharged by the
   unchanged suite plus the surfaces named by me as reachable — my judgement — plus the browser suite on a
   live stack, which is not. The row says which half is which.

**Consequence.** The FR-022 obligation toward FR-021 invariant 86 is discharged: the note, the discharge row,
§16.10's scenario 24 and the rewritten test all moved in the change that made them necessary. §16.9's other
items (escalation policies for services, retroactive alerting, cross-project delegation, per-member severity,
suppression beyond three topics) remain non-goals and still open their own requirements. What FR-022 does NOT
add, deliberately: a way to open a service incident BY HAND (the create API takes no service anchor), a
webhook `incident.opened` event for a service incident, and any UI for the member snapshot the API serves.

## D-0172 — the next §16.9 item is escalation for services, and FR-022 is what unblocked it (2026-08-19)

**Context.** §16.9 lists what FR-021 phase 5 deliberately did not do. Service incidents were the first item
and closed as FR-022 (D-0171). Asked for the next one, the honest reading of the remaining five is that they
are not all the same kind of thing: **retroactive alerting** and **per-member severity** are stated as
POSITIONS with their reasons ("a rule enabled today says nothing about last week"; "which member is
diagnostics"), not as deferrals; **cross-project delegation** and **suppression beyond the three topics** are
open questions nobody has asked for. Only **escalation policies for services** was deferred with a stated
blocker — owner decision 5: it needs "a durable non-incident occurrence with started/resolved/ack/progress/
repeat state before 'the ladder' means anything for a service".

**Decision.** FR-023 / NFR-018 are commissioned and specified in `func-service-escalation.md`. FR-022
removed D5's blocker in the exact terms it was written in: a service alert now opens an incident carrying
`started_at`, `acknowledged_at`, `escalation_step` and `last_escalated_at`. There is no new subsystem to
build and no migration to write — the policy column has existed on `services` since phase 5 and is
deliberately unread, and the progress columns are the ones the monitor ladder already uses.

**Two decisions inside it are worth the record.** The ladder FAILS CLOSED where delegation fails open: a
missing, stale or unreadable verdict does not advance a step, because ambiguity at delivery time means *a
page exists* while ambiguity in a ladder would mean *a page multiplies* on a state nobody can confirm. And
the service GRAPH does not pause the ladder, against the obvious symmetry with the monitor dependency pause,
because §14 states its own position — the impact graph "annotates and links; never suppresses, merges or
hides" — and a graph sold as advisory must not become a suppression mechanism because a second feature found
it convenient.

**Consequence.** Writing the spec corrected one of its own decisions before any code existed: fencing
`TopicEscalationStep` was the first answer, and `domain.FencedTopics`' doc comment says why a PRE-fence topic
must stay legacy — a currently-deployed worker claims `status = 'pending'` and would stop seeing escalation
steps entirely during a rolling upgrade. The payload evolves compatibly instead, and what an OLD worker does
with a service step is stated rather than left to be discovered.

## D-0173 — FR-023 closes, and two of its invariants were written by the implementation (2026-08-19)

**Context.** FR-023 was specified before any code (D-0172) with fourteen invariants and a seventeen-line test
matrix. Implementing it produced two findings that the spec could not have had, and both are now invariants
rather than footnotes.

**Decision.** FR-023 and NFR-018 are DONE, closed against the map in `traceability.md`: sixteen invariants and
nineteen scenarios, each naming a test that exists, enforced by `make docs-check` (FR-021 91+24, FR-022 16+16,
FR-023 16+19).

**The two added invariants, and why they belong in the requirement rather than in a report.**

1. **Invariant 15 — the policy had no write path.** D1 said the API "accepts" the policy. It accepts it only
   at CREATE time: there is no service update endpoint at all, so a service that already existed could not be
   given one. Harmless while the column was inert; a hole the moment FR-023 made it decide who is woken. The
   route is its own transaction with its own audit action naming what moved, a no-op writes nothing at all,
   and a foreign policy is refused BY NAME rather than as an FK violation an API could only render as a 500.
2. **Invariant 16 — the operator needs it in the product, not only in the API.** The SPA control is
   independent of the paging declaration: separate write, separate save, separate error, and it does not
   disappear when the declaration cannot be read — which is where my first implementation put it, and what
   the unit tests refused.

**Two decisions worth keeping in view.** The ladder FAILS CLOSED where delegation fails open, because
ambiguity at delivery time means *a page exists* while ambiguity in a ladder would mean *a page multiplies* on
a state nobody can confirm. And the service GRAPH does not pause the ladder, against the obvious symmetry with
the monitor dependency pause, because §14 states its own position — the impact graph "annotates and links;
never suppresses, merges or hides".

**Consequence.** §16.9's escalation bullet is SUPERSEDED, with the bullet kept so the record of the deferral
survives its end. Of what remains in §16.9, nothing is a deferral: retroactive alerting and per-member
severity are POSITIONS with their reasons, and cross-project delegation and suppression beyond the three
topics are open questions nobody has asked for. The audit trail for INCIDENT writes stays an open gap from
D-0171, unaffected by this requirement and still needing its own.

## D-0174 — the product is a service reliability platform, and "control plane" is bounded rather than banned (2026-08-19)

**Context.** The public positioning still described cerbix as "self-hosted uptime & SLA monitoring", written
before FR-021 made the Service a first-class reliability domain and before FR-022/FR-023 gave it incidents and
an escalation ladder. The proposal on the table was to relabel the product a **Reliability Control Plane**.
That was reviewed against the code rather than accepted.

**What the code supports.** Versioned reliability definitions (immutable declaration revisions + evaluation
epochs), explicit SLI membership separate from context members, region-aware and quorum aggregation,
GOOD/BAD/UNKNOWN with reasons, one duration-weighted timeline per Service, service SLO / error budget / burn
rate quoted against a seal watermark with two independent coverage axes, incidents on either anchor,
dependency-impact candidates, status pages, on-call ladders, and a reconciler that drives all of it from files.
There is also a genuine control/data-plane split by role: `api`/`scheduler` hold desired state, `worker`/`agent`
execute probes.

**Decision.** The primary noun is **"service reliability platform"**, and the headline is
**"Define what reliable means for a service — then measure it and run the response."** "Control plane" is kept
in the README as a CONCEPT with an explicit boundary — a control plane for reliability DEFINITIONS and
OPERATIONAL RESPONSE, not for traffic, deploys or infrastructure — because unbounded it promises actuation the
product does not perform and implies cerbix sits in the request path, which it does not. For SRE-facing text
the defensible phrase is **"system of record for service reliability"**: it claims ownership of the definition
and the facts without promising execution.

**Why not "observability".** cerbix ingests its own probe results and no external telemetry. The word is used
only for its OWN operational surface (`/metrics`, `/healthz`, structured logs) and for the neighbouring stack
it integrates with (PromQL as a check source). Never as a category the product belongs to.

**Banned claims, recorded so nobody has to re-derive them:** observability platform, APM, distributed
tracing, traces/spans, single pane for all telemetry, replaces Prometheus/Grafana, metrics backend or TSDB, log
aggregation, arbitrary queries or a query language, service catalog, **automatic root-cause analysis** (the
product gives correlation CANDIDATES and a heuristic context note over a dependency graph the operator
declared — §14 states it "records candidates, it never elects a single culprit"), AIOps/anomaly detection/ML,
full-stack monitoring, real-time telemetry ingestion, auto-remediation or self-healing, agentless (an agent
role exists), and any numeric guarantee such as "99.99%" or "zero false positives".

**Consequence.** README, `project-description.md` and the `ingest` wording in `overview.md` are rewritten;
`openapi.yaml`'s `ServiceDetail.reliability` description said SLO/budget/burn "are phase 2" when phase 2
shipped in iter-0144 and the numbers live on `…/services/{id}/reliability` — corrected, since a stale API
description is a false statement about capability, not a wording preference. The non-goals are now stated
PUBLICLY in the README, quoted from the specification instead of softened.

## D-0175 — a ladder carried across the 00085 upgrade starts at the upgrade, and that is an EXCEPTION (2026-08-26)

**Context.** FR-023 §8 makes retroactive escalation a non-goal, and iter-0161 §13 enforced it by freezing
each incident's ladder in a snapshot taken when the incident opens. An incident that opened without a
policy has no snapshot and never escalates, however long the policy has been attached since. Migration
00085 then had to answer a question the non-goal does not cover: what happens to incidents that were
ALREADY OPEN when the snapshot table appeared. Without a backfill they lose their ladder permanently
(iter-0161 §16). With a naive backfill they page every elapsed step at once on the first pass
afterwards, which is the non-goal itself, arriving through the migration (§17).

**What cannot be known.** WHEN a policy was attached to a service is recorded nowhere. A carried-over
incident that opened with no policy is indistinguishable, in the data, from one that had this policy
from its first second. No predicate over the existing rows separates them.

**Decision.** The snapshot carries `due_base`, the instant its step offsets are measured from:

- a row written when an incident OPENS uses the incident's own `started_at` — unchanged behaviour, and
  what AC-0157-1's "steps fire from the incident's start" describes;
- a row written by 00085's backfill uses the MIGRATION's instant. The ladder starts at the upgrade: its
  first step fires on the next pass, its later steps wait their real delays.

**This is an exception, not the same rule as §13, and calling it the same would be wrong.** A native
attach-after-open never escalates that incident at all; a legacy carried-over incident does escalate,
prospectively. The two differ in what they do, and they are justified differently: §13 refuses to page
because the operator's intent is knowable and was "not for this incident", while this refuses to page
for ELAPSED time because intent is unknowable, and chooses continuity over silence for the time that
follows. It applies to exactly one population — the rows 00085's backfill wrote — and cannot recur:
every incident opened after the migration gets a snapshot at open.

**Consequence.** Monitor incidents set `due_base` to their start explicitly, so NFR-018's "byte-identical
monitor ladder" holds. AC-0157-1's sentence remains true for every incident cerbix opens from now on and
carries this exception for the upgrade population; iter-0157 is closed and immutable, so the correction
is forward-only, here and in iter-0161 §17. Verified both ways: a backfill copying `started_at` fails the
column assertion, and a ladder measuring from `started_at` fails with "the first pass after the upgrade
fired 3 steps, want exactly its first".

## D-0176 — an onset nobody can receive is withheld, not recorded (2026-08-26)

**Context.** §16.6 said an alert with no resolvable recipient "is recorded and counted with
`cerbix_service_alert_undeliverable_total` rather than silently dropped" — written when the service
signal stood alone. Two things have changed under it since. FR-022 made an onset open an INCIDENT, and
FR-021 §16.1 made a monitor's own alert be SUPPRESSED while its service covers it.

**The failure that forced the question.** With ownership on, a pageable DOWN and no enabled channel,
the old rule recorded an announcement, opened an incident, and advanced `live_firing` — while the
delegation query, which checks routability live, correctly told the member to keep paging. Then the
route comes back. Delegation arms and the member falls silent; the service has no edge left to
announce, because it spent it on an empty route. The outage is never paged by anybody. The recording
was not free: it had to latch, and the latch is what made the silence permanent.

**Decision (owner, 2026-08-26).** An ONSET with no resolvable recipient is WITHHELD: no episode, no
outbox row, no incident, and NO LATCH. The next evaluation sees the same candidate and the same streak
and announces the moment somebody can be told. CLOSES are never withheld — §16's polarity rule — since
an announcement already made must be able to end whatever the route looks like now. It applies to both
signals: the burn arm withholds a FIRE edge on the same condition, or the two signals would disagree
about what "armed" means.

**Rejected alternative:** keep recording, but neither open an incident nor latch. It satisfies both
documents' letters and writes an undeliverable episode plus an outbox row on EVERY pass — one every
few seconds for as long as the route is broken — so the honest record becomes the noisiest table in
the product, and the outbox worker retries deliveries that cannot succeed.

**Consequence.** What is superseded is the ENQUEUE-time half of §16.6's sentence, not the metric it
names. `cerbix_service_alert_undeliverable_total` exists and keeps its own job at DELIVERY time
(`outbox.go`): an announcement whose recipient snapshot is empty, or whose every channel has gone
since the onset, is counted there rather than mistaken for a page. That case is real and unchanged —
the recipients were resolvable when the announcement was made.

Withheld onsets are a different fact and get their own bounded counter,
`cerbix_service_alert_withheld_total{signal,reason}` — the reason a fixed pair, `unroutable` and `no_governing_revision`, because a broken route and a declaration that has not taken effect are different problems with different owners — rather than a fourth value on the evaluations
`outcome` label: that label partitions the units of work a pass performed, and a withheld onset is
not a fourth kind of unit — it is something that did NOT happen to one. Folding it in would have made
`ok` and `withheld` overlap, since `ok` counts every service evaluated.
Verified both ways in both arms: dropping the gate opens an incident nobody received, and withholding
while still latching leaves the restored route with no edge to announce.

## D-0177 — `incident_event` gets the ordering fence `service_alert` has always had (2026-08-26)

**Context.** The review refused to accept "narrow the claim" for the outbox's delivery order, and the
owner agreed: a subscriber can be told an incident RESOLVED and then, from a retried older row, that
it OPENED. FR-021 §16.5 solved this for `service_alert` with a per-service sequence and a delivery
gate; `incident_event` — older, and the topic that reaches webhooks and status-page subscribers — had
neither.

**Two independent causes, and both are fixed here.** Within one claim, `UPDATE … RETURNING` has no
defined row order: the `ORDER BY` inside its sub-select decides which rows are claimed, not the
sequence they are returned in, so one batch holding an opening and its resolution could hand them to
the dispatcher either way round. Across claims and workers, ordering is not available at all — a
retry arrives minutes later, and another worker may be mid-delivery.

**Decision.** `incidents.event_seq` (migration 00086) is advanced by every path that enqueues a
lifecycle event, inside the transaction that writes the fact, and stamped into the payload. Ordering
is then enforced in the CLAIM: it will not release an event while an earlier event of the same
incident is undelivered, and it returns its batch in the order the rows became DUE — the original due
time, captured before the claim rewrites it into a lease. Nothing is dropped at delivery; a first
version tried that and is dissected below.

**Amended the same day, because dropping the stale member is not the same as keeping order.** Two
things were missing and both are now in:

- the CLAIM refuses to hand out an event while an EARLIER event of the same incident is undelivered,
  so one batch cannot carry both ends of a lifecycle and two workers cannot race them. A DEAD
  predecessor blocks too: releasing past it would deliver a resolution whose opening was parked for an
  operator, an ending to an announcement nobody received. The stream waits for the replay, and the
  dead row is already the thing an operator looks at. Migration 00087 indexes the question;
- `incident_event` became a FENCED topic, and the class is enforced by the DATABASE (migration 00088)
  rather than by whichever binary happens to be inserting. Fencing the consumer alone was only half a
  barrier: `enqueueOutboxTx` reads `domain.FencedTopics()` of the RUNNING process, so an old api or
  scheduler still alive through a rolling upgrade goes on writing legacy rows — for exactly the window
  the barrier exists to cover. A BEFORE INSERT trigger settles it for every producer of every version,
  and the rows an old one already wrote are promoted once. The topic list is duplicated into SQL on
  purpose and guarded by a parity test, the same instrument the topic whitelist has.
  The cost is the fenced class's usual one: during a rolling upgrade these events wait for a worker
  that can order them, which is a delay rather than a wrong order.
- **The rolling upgrade acquires ONE stop.** The trigger fences rows; it cannot reach a delivery an old
  worker already has in flight, and the promotion only takes that worker's claim token away so it
  cannot settle the row afterwards. Upgrades crossing 00088 therefore stop the outbox owners
  (`all`/`api`/`scheduler`) before migrating — probers and agents keep running — and the runbook
  carries the procedure. Skipping it loses no event and corrupts nothing; it risks out-of-order
  delivery — the defect the migration exists to end — bounded by one ALREADY-CLAIMED batch per old
  outbox owner (up to 50 rows each, each fanning out to every hook and subscriber), because a worker
  continues through its batch after losing the claim-token CAS. Replacing the token stops settlement
  and release, not calls already in memory.
- Rows written before any of this carry NO sequence, so every such row of one incident ties at zero.
  A tie is not a predecessor, which would have released a whole legacy backlog in one batch, so the
  claim orders on (sequence, created_at, id) — the only order those rows have.

**The delivery-time gate is GONE, and its removal is part of this decision.** It compared the event's
sequence with the incident's CURRENT one and dropped the older, which failed in both directions. It
dropped an OPENING that had never been delivered merely because a later fact existed, so a subscriber
received the update — or the resolution — for an outage nobody had told them had begun: the same
silence the fence exists to prevent, arriving by a different route. And it read that sequence BEFORE
calling the deliverer, so two workers could interleave around it; a check whose answer can change
while the thing it authorises is in flight is not a fence. The sequence stays in the payload for
consumers and for diagnosis; ordering is the claim's job, where it is durable.

**The guarantee is DISPATCH order, and saying more than that would be a promise nobody can keep**
(owner decision, 2026-08-26). Cerbix orders what it controls: which row a claim releases, and the
order in which the worker calls the deliverer. It does not control arrival. A worker whose lease
expires mid-call can still be inside an HTTP request or an SMTP session; cancelling it does not
un-send bytes the far side has already accepted, and no CAS on our side reaches into a receiver's
queue. A guarantee of "no subscriber ever sees `opened` after `resolved`" for an ARBITRARY webhook is
not provable by any lease, and this decision does not make it.

What the receiver gets instead is the means to order and dedupe for itself: every `incident_event`
payload carries the incident's id and this event's `seq`, and the pair is unique per event, monotonic
per incident, and stable across the retries that at-least-once delivery guarantees will happen.

Two strategies rest on that pair, and they answer different questions. **Highest-seq-wins** — keep the
greatest `seq` applied per incident, discard anything lower — is one integer, is idempotent under
retry, and cannot regress; it is a CURRENT-STATE projection, and the discarded event is lost to the
receiver, so it is always right about now and may never have shown a step it skipped. **Expected-next
with buffering** — hold events that run ahead of the next `seq` due and apply as gaps fill —
reconstructs the exact history, and needs a policy for a gap that never fills: an event can be
dead-lettered on our side and will then never arrive, so an unbounded wait stalls that incident
forever. Calling the first one "exact ordering" was wrong, and the correction is recorded rather than
quietly edited: it dedupes and it monotonically advances, which is not the same thing.

Payloads written before D-0177 carry no `seq`, so the pair is not unique for them — they predate the
contract, and the fence never turns a missing number into a missing page.

**What this still does NOT claim.** The claim keeps an incident's events in dispatch order; it does not
serialize a worker against a route that vanished between enqueue and delivery, and it does not
constrain what a remote system does with what it received. That is the same guarantee `service_alert`
has.

It is NOT the sentence an earlier draft of this decision ended on — "nobody is told an outage began
after it ended". Under the owner's narrowing that claim is false, and it cited the mutations of the
delivery gate this decision DELETED, so the evidence described a mechanism that is no longer in the
tree. What is actually true: cerbix will not CALL a deliverer with an incident's events out of order,
and a receiver that uses `seq` will not APPLY them out of order. Between those two lies the network,
which nobody here owns. Verified by the committed sequence regression and the claim-ordering tests,
not by a gate that no longer exists.

## D-0178 — the burn blindness reason gets its own facts, and one new name (2026-08-26)

**Context.** `burnRuleCoversSQL` folds five independent facts into one boolean: the verdict/firing/
sequence shape, the config generation, the target generation, a recorded error, and the lease. The
badge and the delivery lookup then had to say WHY a rule was not quotable, and did it from three
aggregates that did not map onto those five, with `stale_lease` as the default for anything left over.

So the ordinary D-0176 shape was diagnosed as a stalled evaluator. A burn FIRE withheld for want of a
route persists exactly as `last_verdict = 'fire'`, `firing = false`, `emitted_seq = 0`, with a FRESH
lease — that is what the evaluator writes on purpose, so the next pass finds the same unannounced
onset and announces it as soon as somebody can be told. The reason reported was `stale_lease`, which
sends an operator to look at a scheduler that is working while the thing to fix is a notification
channel. The review proved it on real Postgres against the committed tree, twice: `stale_lease` while
the route was gone, and `stale_lease` again after it came back.

**Decision.** Every fact the covers predicate uses gets its own clause and its own name, and the burn
classifier is `burnRuleCoversSQL` taken apart in the conjunction's own order. `stale_lease` now means
one thing — an eligible latch whose lease has expired — and never stands in for something unnamed.
The withheld onset is diagnosed the way the LIVE arm diagnoses its own: `unroutable` while there is no
route, because that is the actionable truth, then `onset_pending` once the route is back and the next
evaluation has yet to announce.

**One value is added: `latch_inconsistent`.** After every legitimate cause is named, a latch that
still fails the covers predicate is in a shape the evaluator does not write — `clear` while still
marked firing. It is also the classifier's last case, and that is deliberate: if the conjunction
refused and nothing above explains it, the row is uninterpretable by construction, and saying so beats
naming a cause that is not the cause. It is a defect or legacy/corrupt state, never a configuration
somebody chose, and the UI says "report this". Adding it required a decision record because §16.6b
makes the vocabulary normative — the metric's label set and the UI's translation table are both
contracts.

**The selection order is published as a RANK column**, not as "the table's own order". The spec said
the latter while the table was grouped for readability, so the published order and
`coverageReasonRank` disagreed on two pairs and nobody noticed: every selection test of the day had
picked pairs the two spellings agreed on. The rank is now explicit in §16.6b, the rows are written in
it, and `TestTheRankOrderIsTheOneTheSpecPublishes` pins a pair that used to disagree.

**Verified:** `TestAWithheldBurnOnsetIsDiagnosedAsTheRouteAndThenAsPending` asserts the persisted latch
really is `fire`/not-firing/`seq = 0`/FRESH before asking, then requires `unroutable` and then
`onset_pending`; removing the two new clause cases fails it with `latch_inconsistent`.
`TestADisabledTargetsLatchDoesNotNameTheReason` was rewritten to expire a lease for real — its first
version wrote an unannounced FIRE and asserted `stale_lease`, which codified this very defect.

## D-0179 — coverage means somebody was TOLD, not that an announcement was enqueued (2026-08-26)

**Context.** D-0176 closed the case where the evaluator finds no route AT EVALUATION: the onset is
withheld, nothing latches, and the next pass announces it as soon as somebody can be told. The review
kept pointing out that this is not the whole class, and it was right. The window between the ENQUEUE
and the DELIVERY is a second one, and it produced the same swallow:

1. the evaluator resolves a route, enqueues the onset and latches firing;
2. the channel is disabled or deleted before the worker gets to it;
3. the worker resolves ZERO recipients, counts it, and terminally SUCCEEDS — correctly, because no
   retry reaches a channel that is gone and a dead letter for it helps nobody;
4. the latch still says firing, so the evaluator sees no edge and announces nothing further;
5. the route comes back, and the arming conjunction — which asked only whether the onset had been
   COMMITTED — armed coverage and silenced the member monitors for a page nobody ever received.

The same chain runs without any race whenever the on-call participant changes between the onset and
the restoration: §16.4a deliberately keeps the ORIGINAL recipients for the close, so the current
recipient never heard the onset either.

**Decision. `delivered_seq`, and arming requires it.** Both latch tables carry a delivered sequence
(migration 00089), advanced by the outbox worker only when a delivery SUCCEEDED for at least one
recipient.

**Amended 2026-08-27, and this was a P0 of its own.** The first version credited on
`ChannelDelivery.Resolved`, which counts channel ROWS that exist and are enabled — it is set
immediately after `EnabledChannelsByIDs`, before any SMTP session or HTTP request. So a service whose
only channel returned 500 resolved one, delivered none, and was credited: the members' own alerts were
suppressed for an announcement nobody received, and permanently, because once the outbox dead-letters
the retry the latch stays firing and no further edge is coming. That is the very defect this decision
exists to close, reintroduced inside its own fix. `ChannelDelivery` now carries THREE numbers —
`Requested`, `Resolved`, `Delivered` — because "asked", "existed" and "succeeded" are three facts and
collapsing any two loses the one that matters, and the credit reads `Delivered`.
The arming conjunction's committed-onset clause becomes `delivered_seq >= emitted_seq`. Until an
announcement is received, members keep paging for themselves — noisier, never silent, which is the
direction §16.1 takes at every other ambiguity.

The credit is monotonic, guarded by `emitted_seq = $seq`, and therefore idempotent under the retries
at-least-once delivery guarantees will happen; a retry of a superseded onset cannot credit the current
one. A CLOSE is never credited — coverage is about a LIVE announcement, and crediting an ending would
arm a service whose alert is over. Both the worker and the store refuse it, because a safety property
with one guard is one refactor from having none.

**What was NOT taken.** The review offered a second option: re-edge on zero resolved, so a restored
route gets a fresh onset with a new sequence. It is the better product behaviour and it is a bigger
change — a new edge needs an episode identity decision, since the current episode is already open and
its uniqueness index says one per service. Not arming is the safe half and it is complete on its own:
nobody is silenced. The re-edge is recorded as available, not done.

**The upgrade is conservative in the other direction, deliberately.** Existing rows carry no delivery
evidence, so a truthful default of zero would declare every armed service undelivered at once and
every member monitor of every covered service would start paging in the same minute. An upgrade that
pages an installation is not a safety improvement. Migration 00089 seeds `delivered_seq = emitted_seq`
for existing rows: they keep the coverage they had, and the new rule governs from the first
announcement after the upgrade. The window is one already-emitted onset per latch and it closes at
that latch's next edge. This is the same class of legacy exception as D-0175, and it is named rather
than described as the same rule.

**One value is added to §16.6b: `onset_undelivered`** — distinct from `onset_pending`, which is an
announcement not yet made. The two have different fixes: wait, versus go and repair a channel.

**A defect of my own, found while proving this.** The mutation that removed the delivered term from
`burnRuleCoversSQL` left the tests green, because the burn classifier had been given its own
`burnOnsetUndelivered` case sitting BESIDE the gate rather than inside it. That is two implementations
of one rule — exactly what D-0177's §34 work removed from between the badge and delivery, reintroduced
one section later. The blindness diagnostics are now all inside the gate's branch, so each is
reachable only when the gate has refused and every one of them is mutation-testable through it.

**And two queries were dead.** `activeLiveDelegationSQL` and `activeBurnDelegationSQL` stopped running
when `ActiveDelegation` moved to `candidateCoverageSQL`, and nothing said so: Go does not complain
about an unused package-level var, and three comments in other files still pointed maintainers at them
as the place where coverage is decided. Deleted, with their orphaned clauses, and the comments now
name the conjunction itself.

`TestATotalSendFailureIsNotAnAnnouncement` pins the case the fake could not previously express (one
channel resolved, its send failed): no credit, and the event still retries. The fake had ONE field
for both numbers, which is why a test asserting "somebody was told" passed on a delivery nobody got.

**Verified:** `TestCoverageNeedsAnAnnouncementSomebodyReceived` and its burn twin walk the whole chain
— delivered onset arms, lost delivery dis-arms with `onset_undelivered`, a restored route alone does
NOT re-arm, a fresh delivered announcement does. `TestMarkServiceAlertDeliveredIsGuardedAndIdempotent`
covers the superseded retry, the duplicate, the refused close and the unidentified burn payload.
`TestAnAnnouncementNobodyReceivedIsNotCreditedAsDelivered` proves the worker leaves no credit for a
zero-resolved delivery and does not retry it. Removing the delivered term from either arm's gate fails
these with `coverage armed for an onset NOBODY received`.

## D-0180 — a status page's project set is ONE axis, asked in both directions (2026-08-26)

**Context.** Three surfaces decided which projects a status page reports, and all three decided it
differently.

- The public render seeded an empty set and added each component's resolved project. A
  project-scoped page whose components are all MANUAL therefore reported none of its own project's
  incidents.
- The feed seeded the page's own project and then walked `components → monitors`, one `GetMonitor`
  per component. A service-backed component contributed nothing, so a Service-only page rendered
  incidents its own RSS did not carry.
- The subscriber fan-out was that same monitor JOIN with neither half of the axis. A page made
  entirely of Service components showed an incident and emailed nobody about it.

The last one is the sharp edge: a reader who subscribed to a page sees the outage on it and never
gets the mail. Webhooks fired; the people who asked to be told did not hear.

**Decision.** `page.project_id` when the page is project-scoped, UNION every non-NULL
`components.source_project`. One SQL owner, `store.StatusPageProjectIDs`, and
`ConfirmedSubscriberEmailsForProject` is its exact INVERSE — a subscriber is entitled to mail about
precisely the incidents their page shows them, so the two must be derived from one axis rather than
kept in step by hand.

**NOT filtered by `source`.** A conversion deliberately keeps a dormant binding's `source_project`,
and `resolveComponents` still resolves such a component to that project, so the page goes on showing
its incidents. Filtering here would narrow the mail below the page — the same disagreement in the
other direction. Whether a manual component with a dormant binding *should* keep reporting that
project is a separate question with its own owner; this decision preserves the current page
semantics rather than changing them in a bug fix.

**Verified:** `TestAPageProjectSetIsOneAxisInBothDirections` uses the two shapes the old spellings
each lost, neither masked by a monitor component of the same project — a project-scoped page with
only manual components, and a Service-only org page — and asserts the forward set AND the subscriber
inverse for both. `TestADormantBindingKeepsThePageReportingItsProject` pins the no-`source`-filter
rule. Dropping the own-project arm fails with `want its OWN project`; restoring the monitor JOIN in
the inverse fails with `the mail about it went to nobody`.

## D-0181 — a service ladder ends at its last step, and the spec said otherwise (2026-08-26)

**Context.** FR-023's D8 declared "no renotify knob for services" — correct, and still the decision —
and then justified it with a sentence that was not true: "the policy's own steps (including its
repeat) are the repeat mechanism". They are not. The repeat branch is
`p.RepeatLast && inc.renotifySeconds > 0`, and the service arm passes zero because a service has no
such interval. `repeat_last` on a policy attached to a service does exactly nothing.

Nothing was broken by this; a service ladder ending at its last step is the intended behaviour. Two
things were wrong around it. The spec told a reader that a mechanism existed which does not, so
anyone reasoning about coverage from the document reached the wrong conclusion. And the UI offered
`repeat last step (renotify)` on a policy that may be attached to a service, with nothing saying the
toggle is inert there — a policy shared by monitors and services behaves differently on each.

**Decision.** The non-goal STANDS: no renotify interval for services, here or by the back door. What
changes is that it is now said honestly in all three places. D8 carries a forward-only correction
naming the false sentence rather than quietly deleting it. The escalation form states, beside the
control, that repeat uses each monitor's renotify interval and that service incidents have none. And
the behaviour is a TEST rather than a zero that happens to disable a branch —
`TestAServiceLadderDoesNotRepeatItsLastStep` gives the frozen ladder `repeat_last = true`, finishes
the ladder, moves the clock six hours, and requires no further step.

Giving services a renotify cadence remains a separate requirement with its own owner. Inventing one
here — a default, a fallback to some other interval — would be exactly the "random cadence" the
review warned against.

**A defect of my own, caught by the mutation.** The first version of that test set `repeat_last` on
the LIVE policy, and an open incident climbs the ladder FROZEN when it opened (D-0175). The frozen
`repeat_last` stayed false, so the test never reached the branch it names and the mutation that gives
the service arm a non-zero interval left it green. It now writes the snapshot and ASSERTS the frozen
value before proceeding, and the mutation fails with `a finished service ladder fired 1 more step(s)
with repeat_last on`.

## D-0182 — repairing the rows the lifecycle fixes could not reach (2026-08-26)

**Context.** Everything earlier in this arc stopped NEW damage: a resolved incident is terminal in the
write rather than in a prior read, a service incident resolves with its episode, a member snapshot
names the declaration that governed. None of it touched rows already written by the versions that did
not, and the review was right that a future-write test is not evidence for a defect that has already
written customer history.

FIVE classes exist, not the four an earlier draft of this decision counted: three are repairable from
the row itself and two are not, and that line is the decision. (The one the draft missed is the
mirror of the first — `resolved` with no resolution time — which the migration has always repaired
because the CHECK below is a biconditional and would otherwise refuse to install.)

**Repaired.**

*Resurrected incidents* — `resolved_at` set with a status that walked back off `resolved`. Before the
CAS, a writer holding a stale status read could commit it after somebody else's resolve. These are
not cosmetic: the row occupies the partial unique indexes that permit ONE open incident per subject,
so the next outage cannot open one and the second failure is invisible in the timeline. `resolved_at`
is the durable fact — only a resolve stamps it and nothing clears it — so the status is the field
that lied, and it is corrected to match.

*Resolved rows with no resolution time* — the mirror, reachable by older code. `updated_at` is the
closest durable instant the row still carries and is never later than the resolve that set the
status, so it is used rather than invented. This one is stamped SILENTLY, with no timeline note:
nothing about that incident's history is wrong — it resolved, and the column recording when simply
was not filled — so a note saying it was repaired would tell a reader something untrue. The other two
get one, because in those the record itself said the wrong thing.

*Stranded service incidents* — an open auto-incident whose health episode is closed (or never
existed) and whose service is not firing now. Disowning used to end the alert and leave the incident
open forever. The predicate is deliberately narrow: the service must still exist and must NOT be
firing, because a repair that closes a live outage is worse than the defect it repairs.

Both get a timeline note saying what happened, guarded by the `🔧 Repaired:` prefix so a rerun adds
no second one — the same marker pattern the suppression notes use.

**Counted, not repaired.**

*Member snapshots possibly taken from a revision that was not yet governing.* Two obstacles, and the
second is decisive. `incident_member_snapshots` stores the member LIST and no revision id, so a wrong
snapshot cannot be recognised by inspection — nothing in the row says where it came from. And the
obvious substitute, rebuilding from "the revision effective at `started_at`", is exactly the guess the
review warned against: `started_at` defaults to the TRANSACTION clock while the evaluator chose its
revision at `statement_timestamp()`, so a transaction crossing a boundary can carry a `started_at`
earlier than the revision that actually governed. What the migration emits is a BOUND — snapshots on
incidents whose service had any revision take effect afterwards — named as an upper bound and not as
a defect count, because most of that set is correct.

*Anchorless auto-incidents* — both the monitor and the service are gone, so nothing identifies what
they were about. Attaching an owner would be attaching somebody else's history.

Both are reported as migration warnings and carry a runbook procedure. Reporting is the honest
outcome: an operator who knows the row exists can decide, and a migration that guesses cannot be
un-guessed.

**And the invariant becomes the database's.** `CHECK ((status = 'resolved') = (resolved_at IS NOT
NULL))`, both directions, after the repair. Every path that resolves stamps the time and none
un-resolves — that is true of the code today, and "resolved is terminal" was already once true in a
read and false in the write, which is what a constraint is for.

**Verified:** `TestTheRepairMigrationFixesWhatItCanAndLeavesWhatItCannot` migrates a throwaway
database to 89, writes the shapes the old releases produced, runs the REAL 90 and asks what happened
to each — including two negative controls, a service that is STILL FIRING and a HUMAN's incident, both
of which must be untouched. Dropping the status correction fails the migration outright on the new
CHECK; dropping the `live_firing = false` guard fails with `the repair closed an incident for a
service that is STILL FIRING`.

## D-0183 — PostgreSQL 14 is not supported, and that is decided rather than pending (2026-08-27)

**Context.** iter-0161 opened on a production upgrade that died on PostgreSQL 14: five migrations use
the column-list `ON DELETE SET NULL (col)` form that arrived in 15, and the plain form cannot
substitute — on a composite FK it nulls EVERY referencing column including the NOT NULL `project_id`,
which is the bug `00070` exists to fix. `cerbix migrate` was made to refuse 14 before touching a file,
and the runbook carries the recovery.

What was left open was the product question underneath: SHOULD cerbix support 14 at all? It sat in
`DoD-0161` as needing the owner's commission, which is the right place for it and the wrong place to
leave it indefinitely — an open question in a status document reads as work someone is going to do.

**Decision (owner, 2026-08-27): no.** PostgreSQL 15 is the floor and stays there.

The cost of the alternative is what makes this easy: supporting 14 means emulating column-list
`ON DELETE SET NULL` with triggers across five migrations and maintaining those triggers indefinitely,
in the exact code path whose correctness the composite FKs exist to guarantee. A hand-written trigger
that nulls one column of a composite key on delete is precisely the kind of thing that works until a
cascade arrives from an unexpected direction — and it would carry the tenancy invariants.

Nothing in the code changes: the version guard, its error message and the runbook already implement
this decision. What changes is that the question is CLOSED, so no document describes it as pending.

## D-0184 — a dormant binding keeps reporting its project, and that is chosen (2026-08-27)

**Context.** Converting a status-page component from monitor-backed to manual keeps the old binding
DORMANT so the change is reversible, and `resolveComponents` sets `Project = c.SourceProject` before
it branches on source, so a manual component with a dormant binding still brings that project's
incidents onto the page. D-0180 made the subscriber fan-out the exact inverse of that axis, so the
question moves both surfaces together — which is what having one axis is for.

D-0180 recorded this as inherited behaviour preserved by a bug fix, and said the question belonged to
its own owner. It does, and leaving it in a document as pending was itself a cost: an open question
in a living document reads as work somebody is going to do.

**Decision (owner, 2026-08-27): it keeps reporting.** The axis stays `page.project_id` UNION every
non-NULL `components.source_project`, unfiltered by `source`.

Three reasons, in order of weight.

*Narrowing has the sharper failure.* Flipping a component to manual during an outage is an ordinary
move — the automated signal is wrong or noisy and an operator wants to narrate it themselves — and
under a narrow axis that act would silently remove the in-flight incident from the page and cut the
subscriber mail mid-event. A change that can make a live outage disappear from a status page is the
worse of the two directions, and it would do it at the moment the page matters most.

*Retiring a system from a page already has an explicit act:* delete the component. If conversion to
manual also meant "stop reporting this project", two different intentions would share one control,
and the reversibility the dormant binding exists to provide would stop being harmless.

*The confusion the narrow option fixes is a presentation problem.* "A reader sees an incident about a
project with no visible component" is real and mild, and the answer to it is how the page renders,
not throwing the incident away. Losing data to remove an ambiguity is a bad trade.

**Reversible, and cheap to reverse:** one predicate in `statusPageProjectsSQL` plus its inverse in
`statusPageReportsProjectSQL`. If an installation's operators use conversion to mean "removed from
the page", this is the line to change — and `TestADormantBindingKeepsThePageReportingItsProject`
already pins the behaviour either way, so the change would announce itself.

## D-0185 — services get the renotify cadence D8 declined (2026-08-27)

**Context.** FR-023's D8 said "no renotify knob for services" and justified it with a sentence that
was false: that the policy's own repeat was the mechanism. It is not — the repeat branch is
`RepeatLast && renotifySeconds > 0`, and a service had no interval to supply, so `repeat_last` on a
service-attached policy did nothing whatever. D-0181 corrected the claim, said the non-goal itself
still stood, and left the knob to a separate decision, because inventing a cadence in a bug fix is
exactly the "random cadence" the review warned against.

**Decision (owner, 2026-08-27): add it, mirroring the monitor exactly.** `services.renotify_seconds`
(migration 00091), 0 = off, otherwise 60..86400. It supersedes D8's non-goal; D8's corrected note
stays, because the reasoning it carries about the false justification is still worth reading.

Three properties make this an added control rather than an imposed cadence, and each is a deliberate
choice:

*Zero is the default and zero is off.* No existing service starts repeating because a column appeared.
`TestAServiceLadderDoesNotRepeatItsLastStep` — written when the non-goal was the whole answer — keeps
pinning that, and it is now half of a pair rather than the last word.

*The floor is 60 seconds.* A cadence shorter than a minute is a way to page somebody every few
seconds by typing one number, and the validator refuses it with the range in the message. Same bounds
as the monitor's.

*The cadence is read LIVE from the service, not frozen into the incident's ladder.* The snapshot
(D-0175) exists so an incident climbs the steps it started with — re-timing a page already in flight
is the defect it prevents. A repeat cadence is a different kind of thing: it governs how often the
last step recurs FROM NOW ON, and an operator turning it down during a noisy incident means "stop
paging me every ten minutes", which is an instruction about the present rather than a rewrite of what
already happened.

It is PAGING configuration, so it joins the DB-owned alerting generation: changing it dis-arms
delegation until the new generation has been evaluated, like every other paging field, enforced by
00082's trigger rather than by whichever write path remembered.

**Verified:** `TestAServiceRepeatsItsLastStepOnTheCadenceItWasGiven` (not before the cadence, yes
after it, and an acknowledgement stops it) paired with the existing off-by-default test, and
`TestChangingTheRenotifyCadenceDisarmsUntilReevaluated`. Reading a constant zero instead of the column
fails the first with `the repeat fired 0 step(s) after six minutes of a five-minute cadence`; dropping
the column from the generation trigger fails the second with `changing the repeat cadence left
coverage armed`.

## D-0186 — a delivery is bounded by the claim that authorised it (2026-08-27)

**Context.** D-0177 narrowed the outbox's guarantee to DISPATCH order and said plainly that arrival is
not ours: cancelling a call does not un-send bytes a receiver has already accepted. The review
sketched what IS ours and iter-0161 §31 recorded it as available rather than done — one whole-call
deadline drawn from the claim's lease, every branch context-aware including SMTP.

The gap it closes: the claim token stops a deposed worker from SETTLING a row it no longer owns, and
that fence lives in the database. It cannot stop that worker from still being inside an HTTP request
or an SMTP session while the row's new owner sends the same event, because that happens outside the
database entirely. The overlap window was however long a delivery happened to take.

**Decision.** `ClaimDueOutbox` returns the `next_attempt_at` it wrote — the lease — on the event, and
`process` derives the delivery context's deadline from it, minus two seconds of headroom. A lease
already spent sends NOTHING: the row belongs to somebody else, and the duplicate's settle would lose
the CAS anyway. A zero lease means the previous behaviour, for an older store or a caller that built
the event itself.

**The bound is on DELIVERY only, and that is the load-bearing detail.** The settling writes run on the
caller's context. A delivery that used its whole budget would otherwise reach `MarkOutboxDelivered`
with a cancelled context, and a successful send recorded as a failure goes back to the queue and pages
the recipients twice — the exact duplicate this decision exists to reduce, introduced by its own fix.
The headroom is the same concern from the other side: a budget of the WHOLE lease hands the row over
at the instant we try to mark it delivered.

**SMTP was the branch that ignored it.** `sendMailTimeout` dialled on `context.Background()` and set a
fixed session deadline, so a mail send could outlive any budget. It now takes the caller's context,
dials with it, and sets the connection deadline to the EARLIER of the session span and the context's
deadline — the minimum, because a long budget must not extend a session past its own limit and a short
one must not be ignored because the session limit is generous. `mailer.SendMailContext` is the new
entry point; `SendMailTimeout` stays for callers with no deadline to offer.

Bounded fanout comes for free: the deadline is on the whole call, so channel N+1 fails once it passes
and the event retries.

**Amended 2026-08-27, in review.** Three gaps in the same decision, all of them the shape it was
written to prevent.

The RECORDING writes were still on the bounded context, one layer below the settle: the coverage
credit and the condemnation live inside `deliver`, which receives the budget. A send that used its
whole budget therefore lost its credit — the page went out and coverage never armed — and lost its
condemnation, which means the outage is never re-announced. `deliver` now takes both contexts and the
recording writes take the caller's.

The subscriber confirmation still dialled on `context.Background()`, so a hung SMTP endpoint could
outlive the claim. `MailSender` is `SendContext` now, and `Mailer.Send` keeps its signature for
callers with no deadline.

And the budget was computed as `time.Until(LeaseUntil)` — a database timestamp minus a worker
timestamp. Under skew that either delivers after the lease really ended or skips a claim that was
perfectly good, which is a poor look for a monitoring product. The claim returns the database's
`now()` alongside the lease, the budget is their difference, and the worker spends it against its own
elapsed time: only the two clocks' RATES are assumed equal.

**Amended again 2026-08-27.** Two of the three fixes above were incomplete in the same direction.

The budget was still counted from AFTER `ClaimDueOutbox` returned, and the lease starts at the
database's `now()` INSIDE that statement — so its planning, scan and round trip were lease already
spent, handed back to the worker as budget it does not have. A slow claim then delivers past the lease
it believes it is inside. The batch clock is taken before the call.

And condemnation ignored `delivered_seq`. A partial delivery credits on the first attempt and can
still fail the event — three channels reached, a fourth timing out — and when the retries run out,
`condemnDead` fires unconditionally. That said nobody heard an announcement three people received, and
the evaluator would have paged all of them again. The guard is in the SQL of
`MarkServiceAlertUndeliverable`, both arms, because there are two call sites and they fail
differently.

**One more, from the same review: a claim taken and never attempted must be handed back.**
`ClaimDueOutbox` increments `attempts` for every row in the batch and the worker delivers them one at
a time, so a slow event at the front leaves the ones behind it past their lease before their turn.
Returning without delivering is right; leaving the attempt spent is not — enough turns like that and
an event dead-letters having never been sent once. `ReleaseOutboxClaim` refunds the attempt and makes
the row due, guarded by the claim token so a row somebody else already owns is left alone.

**Verified:** `TestDeliveryIsBoundedByTheClaimsLease` — a spent lease sends nothing and settles
nothing; the deliverer receives a deadline strictly inside the lease; a delivery that burns its whole
budget is still SETTLED; and an event with no lease gets no deadline. Removing the bound fails with
`the deliverer was called 1 time(s) for a row this worker no longer owns`; removing the headroom fails
with `the delivery budget is 29.99s of a 30s lease`; moving the settle onto the bounded context fails
with `a delivery that used its whole budget was not settled`.

**Two fakes had to be corrected to make those mutations meaningful**, and both corrections are the
point rather than incidental. The notifier fake blocked on `ctx.Done()` with no fallback, so removing
the bound HUNG the suite instead of failing it — a mutation that hangs teaches nothing. And the store
fake ignored its context, so the settle mutation passed: a fake that does not model the property being
asserted cannot witness its absence.

## D-0187 — the outage nobody heard about gets announced again (2026-08-27)

**Context.** D-0179 closed the swallow: an announcement that reached nobody no longer arms coverage,
so the member monitors keep paging for themselves. It stopped there on purpose, and the gap it left
was named in its own text as the option NOT taken — the incident stays open, the service is still
down, and its own alert was never received by anyone. Forever: the latch says firing, so the
evaluator sees no edge, and fixing the channel changes nothing.

The reason it was deferred is the episode-identity question. `service_alert_episodes` permits ONE
open episode per (service, signal, target, rule), so a second onset cannot simply be opened beside
the first.

**Decision. Re-announce through the ORDINARY onset path, superseding the episode nobody heard.**

The onset path already closes an open episode before opening the next one, so the identity question
answers itself: the re-announcement is an announcement in every respect — new sequence, fresh
recipient snapshot, its own episode, its own outbox row — rather than a special case every handler of
an announcement would have to remember. What it must NOT do is inherit the old episode's recipients:
that snapshot names people who could not be reached, and §16.4a's rule that a close reaches whoever
heard the onset is satisfied vacuously when nobody did.

**The trigger is a CONDEMNED sequence, and that is a second column rather than a reuse of the first.**
`delivered_seq < emitted_seq` is also true of an event still sitting in the outbox, and re-announcing
on it would send a second copy of something merely slow. `undelivered_seq` (migration 00092) is set by
the worker only on the terminal paths — an empty recipient snapshot, or a delivery that resolved
nobody — never where a retry is still owed. A 500 is retried, so it does not condemn.

**The superseded episode is closed as `undelivered`, a new reason.** The onset path's ordinary
`policy_changed` would be a lie: nothing about the policy changed, and a false cause in a record an
operator reads during an incident is worse than no record.

**D-0176 keeps applying.** A re-announcement with no route is WITHHELD and counted, exactly like a
first announcement — announcing into the same emptiness would be the defect this fixes, running in a
loop.

**It cannot loop.** The re-announcement's own sequence has not been condemned, so the next pass is
quiet. A re-announcement that repeats every cadence would be a pager loop rather than a fix, and the
test asserts the third pass is silent.

**Verified:** `TestAnUndeliveredOnsetIsAnnouncedAgainOnceThereIsSomebodyToTell` walks the whole shape —
announce, stay quiet while down, condemn, withhold while unroutable (counted), re-announce once the
route returns with a NEW episode and a higher sequence, the old episode closed as `undelivered`, and
silence afterwards. `TestOnlyATerminalFailureCondemnsAnAnnouncement` pins the boundary from the
worker's side, including that a 500 does not condemn. Disabling the re-announcement fails with `the
outage was not re-announced after the route came back`; triggering on "not delivered yet" instead of
"condemned" fails with `a service that stayed down announced again`.

**Amended 2026-08-27, in review.** Two gaps, and both were mine to see.

The BURN arm had none of this. The migration added `undelivered_seq` to both latch tables and the
worker could condemn either signal, but only the live evaluator gained a re-announce branch — so an
undelivered burn FIRE stayed firing forever on exactly the signal nobody was looking at. The burn
mirror is `TestAnUndeliveredBurnFireIsAnnouncedAgainOnceThereIsSomebodyToTell`.

And "not covered: an event that dies by exhausting its retries" does not survive its own invariant.
105 says the trigger is "no retry owed", and an announcement that ran out of attempts is as
permanently unheard as one whose channels were all deleted — the class was closed by the words and
open in the code. `condemnDead` handles it, decoding the payload itself because the delivery path
that would have parsed it is the one that just failed. Only `service_alert` is condemned there;
anything else that dead-letters is an operator's problem and says so through the dead-letter surface.

## D-0188 — FR-024 Reliability Gate: the design gate is closed (2026-08-27 → 2026-08-28)

> **Partly SUPERSEDED the same day by the design review (D-0189, D-0190).** Two clauses below are no
> longer the contract: the `budget_below` threshold as a remaining ratio with default 0.10 (an error of
> units — it is `budget_consumed_percent >= N`, default 90, spec D3), and CLI exit 3 for `UNKNOWN`
> (the exit follows `action`, spec D4/D16). The effective contract is the spec's D3, D4 and D16; this
> record keeps its text so the correction can be read against what it corrected.

**Context.** After iter-0161 closed, the independent reviewer proposed the next direction — closing the
SRE loop from measured reliability to a release decision — as Change Intelligence plus an error-budget
release gate. The adversarial pass in the party reduced it: the gate is a DECISION over facts cerbix
already computes and needs no new store; change events are a separate, later requirement. The owner
approved the direction and `docs/specs/func-reliability-gate.md` was written as a design gate — fourteen
decisions, nine settled in the pass and five left to the owner.

**The owner closed the five the same day.** No dedicated `gate` token role in v1 (additive later, not
removable later). CLI exit 0 for both `ALLOW` and `WARN`, 2/3/4/1 for `BLOCK`/`UNKNOWN`/`NOT_CONFIGURED`/
error — fixed now because changing it breaks every pipeline. An open incident WARNS rather than blocks,
because the deploy is very often the fix. `budget_below` takes a per-policy threshold, default 0.10.
`coverage_not_armed` is REMOVED from the clause vocabulary: it is a fact about who is paging, not about
reliability, and letting it decide would confuse the two questions this product keeps apart — it stays
in the response as evidence only. Policy is per service with no inheritance. Decisions are PERSISTED
with bounded retention and a read endpoint, because an id that cannot be looked up is not evidence.

**What this does not decide.** Nothing about implementation order beyond "backend on the reviewed
contract; SPA after an approved mock". The independent design review of the document is in progress and
may reopen a DECIDED item with new facts, which is the review's job and not a defect of the gate.

## D-0189 — FR-024 revision 2: what the design review found, and the four owner calls (2026-08-28)

**Context.** The independent design review rejected revision 1 of `func-reliability-gate.md` with 4 P0,
9 P1 and 3 P2, every finding checked against the code before it was accepted. The P0s were all of one
kind — a claim the code does not support: the named budget owner (`decideServiceWindow`) owns
withholding and the budget is computed after it, per window, so a policy without a window had no
defined fact and a "0.10 remaining" threshold was an error of units; `UNKNOWN` sat outside the decision
order and the CLI would have exited non-zero against an operator who chose `warn`; five separate reads
were presented as one instant; and "every decision audited", "decisions persisted" and "O(1) reads"
cannot all be true.

**Four of the findings were the owner's to close, and were, the same day:**

- *the window is part of the policy* (mandatory `window`; burn clauses read only that target; worst-of-all
  declined because the answer would depend on which windows somebody added);
- *the threshold is `budget_consumed_percent >= N`*, default 90, because `BurnedPercent` already owns
  that quantity and `RemainingRatio` is an absolute share of time;
- *decisions are a bounded ledger, and only policy/override mutations are audited* — a busy pipeline must
  not bury the audit log under its own heartbeat;
- *authorisation is three central `authz.Action`s over the existing roles*; a token-scoped gate
  capability is a follow-up requirement, because `domain.Role` is shared by memberships and tokens and a
  narrower token is a change to RBAC, not an addition;
- *gate policy is owned by the UI/API even on a file-managed service* — the reviewer recommended the
  opposite (declarative, in format 2), and that alternative is recorded in the spec as declined rather
  than forgotten.

**The rest was the author's to fix and is fixed in revision 2:** `state` and `action` split with a total
algebra; one `REPEATABLE READ` transaction with `evaluated_at` as its first statement; `as_of` and
`sealed_through` given their real meanings; `valid_until` replaced by `facts_fresh_until` with a stated
non-promise; override lifecycle (7-day maximum, one active, bound to policy revision, server-derived
actor); ledger lifecycle (immutable snapshot, survives rename and delete, retention); policy evolution
(`schema_version`, exhaustive clause set, CAS); the tenant contract corrected to 400/404 as the code
actually behaves; the CLI as a security surface (token by environment only, TLS verify, no skip flag);
the threat model corrected — there is no API rate limiter and a decision is not one row; burn clauses
named by severity.

**Two things worth keeping from this round.** The worst finding was not a design error but a wrong
statement about the code — I named an owner without reading what it returned. And the review caught my
own evidence again: revision 1's threat model asserted a mitigation ("existing rate limiting") that does
not exist in the tree. Same class as iter-0161 §47, now in a document rather than a test.

## D-0190 — FR-024 revision 3: the seal lag is bounded, and an override does not rewrite the fact (2026-08-28)

**Context.** Design review round 2 of `func-reliability-gate.md` (revision 2, `113308f`) returned 2 P0,
5 P1 and 4 P2. Round 1 was closed on substance; these were new.

**The P0 that matters.** Revision 2 called the seal lag "an accepted cost" and did not bound it. A
30-day window stays quotable when the materializer has been stopped for a week — the window simply ends
at an old watermark — so a gate would go on saying `ALLOW` on a budget nobody had measured since, and a
policy that ignores burn clauses has nothing left to notice. **Owner decision:** `max_seal_lag_seconds` (named `max_seal_lag` at the time, renamed in revision 4) is a
policy field, default **15 minutes**. Past it, every budget clause is UNAVAILABLE with `seal_stale`.

> **Rationale corrected by D-0191.** This record originally said "`1m..24h`" and justified the default
> with "the seal cadence is one minute". Both were wrong about the code: the one-minute constant is the
> daily heartbeat ROLLUP (`rollupEvery`), not the service seal — the sealer runs every second and seals
> up to `FloorToBucket(now − LateArrivalGrace)` over 60 s buckets with a 120 s grace, so a healthy
> pipeline's lag sits in `[2m, 3m)` and a one-minute policy would be stale forever. The floor is derived
> (300 s) and the per-policy choice stands for the reason that was true all along: it is a BUSINESS
> tolerance for stale data, not a property of how often anything runs. The sentence that once followed
> this note — a service "probed every second and one probed hourly should not share a bound" — was the
> same false reason in other words (probe frequency does not change when buckets seal) and is removed
> rather than left as a second answer (D-0192). The lag is stated by the REPORT PATH and shown on the service page, so the gate and the
screen read one number; the alternative of a single product-wide constant was declined because the
tolerance for stale data is a property of the SERVICE's business, and one number for every service could
only be changed by a release.

**The other P0.** An override was written to flip `state` as well as `action`, with an `original_state`
kept alongside. That rewrote the observed fact for the operator's convenience, and the ledger would have
recorded a history that did not happen. An override changes ONLY `action`; `state` and `reasons[]` stay
what reliability said; `unoverridden_action` carries what the pipeline would otherwise have been told;
the metric sees `state="BLOCK",action="ALLOW",overridden="true"`.

**The rest, author's fixes:** an expired override releases its slot (the create transaction closes it as
`expired` under the service lock, since a partial unique index cannot consult `now()`), and a policy
DELETE revokes like an edit; the ledger read is a PROJECT-scoped route with no service-existence check,
proven by an HTTP read after the service is deleted; an explicit presence table for the response, since
`NOT_CONFIGURED` has no policy revision and a deleted target has no objective; a process-wide AND
per-principal load bound checked before any transaction opens, plus a begin-through-commit budget; D4's
closing sentence brought into line with its own algorithm; the API strict — the request carries every
assignment, the server fills nothing, the defaults are the UI's template; "no new store" narrowed to
what is true; the live `status.md` NFR-019 row corrected from revision 1's false owner; and D-0188's two
superseded clauses marked as such rather than left as a second live answer.

## D-0191 — FR-024 revision 4: a floor you can reach, one freshness formula, bounds on rate, and an actor you can name (2026-08-28)

**Context.** Focused confirmation of revision 3 (`a16ea70`) returned 4 P1 and 4 P2 (party [25]). No
owner question this round; the per-policy `max_seal_lag_seconds` (then still spelled `max_seal_lag`; renamed in revision 4) stays the owner's authority.

**The floor.** Revision 3 allowed a `max_seal_lag` (the field's name at the time) of one minute. The code cannot satisfy that: buckets
are 60 s, the late-arrival grace is 120 s, the sealer seals up to `FloorToBucket(now − grace)`, so a
healthy pipeline's lag is `[2m, 3m)` before queueing. A one- or two-minute policy would be `seal_stale`
on a system doing exactly what it should, and a strict API must not accept a value that is unreachable.
The floor is now DERIVED — `CanonicalBucket + LateArrivalGrace + one bucket = 300 s` — stated in the
domain beside the constants it depends on, with a live-materializer boundary test. D-0190's rationale
("the seal cadence is one minute") was about a different mechanism and is corrected in place.

**One freshness formula.** `facts_fresh_until` was defined over leases alone, so a budget-only policy had
an expiry the field could omit, and a burn lease later than the seal horizon could name an instant the
budget was already stale. It is now the minimum over the seal horizon (`sealed_through + max_seal_lag`, as the field was named at the time)
and every lease the policy depends on, present whenever any of them exists.

**Bounds on rate, not only concurrency.** An in-flight cap bounds how many reports run at once; it does
not bound how many run in a row. One principal at concurrency 1 could create expensive reports and
immutable ledger rows without limit, so "evaluate and read and nothing more" was not a bound. Per-principal
and process-wide RATE limits join the caps, all checked before any transaction opens; retention is
validated within `7..365` days, purged in bounded batches, with a backlog metric; the rate bound has a
mutation.

**An actor you can name.** For an API-token principal the typed attribution the audit log uses is
`actor_user_id = NULL, via_token = true` — after commit that reads as "some token". Overrides now store
an immutable server-derived `actor_label` (`token:<name>`, or the user) beside the typed half, for the
revoker too, and a client-supplied actor field is refused.

**Smaller:** `max_seal_lag_seconds` as the one wire and storage type (integer seconds, whole minutes); D4's
wording aligned with its own vocabulary; D6a's "first statement" made precise — the first
SNAPSHOT-BEARING statement, after the deadline wrapper's `SET LOCAL`s, which establish no snapshot.

**What this round says about the author.** Two of the four P1s were claims about the code that the code
does not support — the seal cadence and the reachability of the floor — the same failure as revisions 1
and 2, and the same one iter-0161 closed on. The review has now caught it in a document five times.

## D-0192 — FR-024 revision 5: arithmetic that produces its own number, bounds with numbers, and a guard against my own stale sentences (2026-08-28)

**Context.** Focused confirmation of revision 4 (`d540ae4`) returned 3 P1 and 5 P2 (party [27]). No owner
question. The fixes of revision 4 were right in substance; the residue was precision and consistency.

**Arithmetic.** Revision 4 wrote "`CanonicalBucket + LateArrivalGrace + one bucket of headroom = 300 s`",
and 60 + 120 + 60 is 240. A derived constant whose formula does not produce it is not derived: a future
change to the constants would have moved the floor wrongly. The formula is now
`LateArrivalGrace + CanonicalBucket + 2 × CanonicalBucket = 300 s` — the healthy upper bound plus two
buckets of headroom — and the domain test asserts the expression, not the literal.

**Bounds with numbers.** Revision 4 named eight resource bounds and gave none a key, a default, a range
or an algorithm. Two authors would have built two limiters and the config loader could validate nothing.
§5a is a table: eight `gate.*` keys with default/min/max, token-bucket semantics with `Retry-After`, a
purge cadence and batch, metric names with units. And a sentence the table needed: the bounds are
PROCESS-LOCAL, so a cluster's allowance scales with its replicas — a shared limiter would live in the
database the bound protects, and is declined.

**One freshness set, option (b).** `facts_fresh_until` is over decision-constraining inputs only —
the seal horizon when a budget clause can decide, the burn leases of rules whose clause can decide.
Coverage is evidence and never a clause; an `ignore`d rule decides nothing. Revision 4 had mixed both
in, so an `ALLOW` could sit beside a freshness already in the past for a fact that decided nothing.

**The revoker gets the whole triple** (`revoked_by_user_id`, `revoked_via_token`, `revoked_by_label`);
revision 4 said "the same way" and gave two columns.

**A guard.** Four rounds in a row found a normative sentence still carrying the previous contract.
`make docs-check` now refuses the retired spellings in the spec and in `decisions.md` outside superseded
passages, and a duplicated schema header. It is a guard against a specific author — this one — and it is
in the gate because the review has had to be that guard five times.

**On P2-4.** The review reported two mechanical duplicates in revision 4. Neither reproduces by `grep`
against `d540ae4` (each string occurs once in the spec and nowhere else); the duplicate-header guard is
added regardless, because a check that would have caught it is cheaper than a second look.

## D-0193 — FR-024 revision 6: a ledger sized for what a deploy gate is, aged out by dropping partitions (2026-08-28)

> **Superseded in part by D-0194 (2026-08-28).** The partition period below is one month; revision 7
> makes it one UTC day, because month-sized partitions kept a row up to 31 days past the retention
> knob. The capacity table, the lower rates and the partition-removal principle stand.

**Context.** Focused confirmation of revision 5 (`3771622`) returned 3 P1 and 3 P2 (party [30]). One was
the owner's.

**The capacity contract (owner, 2026-08-28).** Revision 5 finally gave the resource bounds numbers, and
the numbers defeated the bounds' purpose: a default of 600 decisions per minute PERMITTED 77.8 million
immutable rows per replica over the retention, the hard maximum 7.8 billion, and a row-level batch
DELETE had to keep pace with that forever. The owner chose both halves of the fix. The rates are re-sized
to what a deploy gate is — 10 per minute per principal, 60 per minute per process by default, 600 the
hard maximum — and §5a now prints the rows per day and per retention those numbers permit, per replica,
so nobody raises a limit without seeing the table it is raising. And the ledger is RANGE-partitioned by
`evaluated_at`, one partition per month, in both storage modes; retention is the DROP of whole
partitions at or before the cutoff — one catalog operation regardless of rows, no dead tuples, no DELETE
WAL — so the purge outruns any permitted ingest by construction rather than by a load test that would
have to be repeated at every change of the numbers.

**The guard's scope was the hole it was meant to close.** The first stale-spelling guard skipped every
fenced block, and the normative schema of §5 IS a fenced block — so a retired column name in the very
definition of the column would have passed. It also let any line mentioning a revision number through,
and did not read `docs/status.md`, which had kept a retired spelling and outranks the decisions. The
guard now scans the spec entirely except one fence explicitly marked as the fixture, the FR-024/NFR-019
status rows, and decision sections by heading; exemptions are blockquotes and the two phrases a
supersession note uses. And it has fixture tests run by `make docs-check`, because the author of the
guard had, the same hour, announced a revision with a red gate hidden behind a pipe.

**The rest.** The purge is a partition-maintenance pass on the scheduler leader inside
`subCadenceTimeout`, off the dispatch loop like every other leader sub-cadence, with two catalog gauges
that never count rows; the limiters get `rejected_total{reason}`, a duration histogram and
`errors_total{kind}`, no principal labels, and runbook thresholds against those names. Limiter
acquisition order is pinned — permit first, token second, a refusal never costs what it did not use —
and `Retry-After` is `ceil`, never below 1. `LateArrivalGrace` moves to the domain beside
`CanonicalBucket` so `MinSealLag` can be derived there without an import cycle or a copied constant.
D6a's doubled "first" is gone.

## D-0194 — FR-024 revision 7: daily partitions, a detach that never blocks an insert, and a ledger that refuses rather than strands (2026-08-28)

**Context.** Focused confirmation of revision 6 (`4e5bd15`) returned 5 P1 and 2 P2 (party [33]). One was
the owner's; one did not reproduce.

**The partition period (owner, 2026-08-28).** D-0193 chose partitions a month wide. The reviewer showed
what that does to the knob: with whole-partition removal a row lives until its partition's upper bound
passes the cutoff, so `retention_days = 7` keeps rows up to 38 days and `90` up to 121, while §6 promised
"older than retention is purged" — and the rows carry `actor_label`, which makes the ceiling matter, not
only the floor. Offered daily partitions (exact to within a day; precedent `heartbeats`, 00017) or
keeping monthly ones with the knob re-specified as a minimum, the owner chose daily. At the 365-day
maximum that is at most 373 attached partitions (the figure at the time; D-0195 corrects it to 396 with the maximum lead), which PostgreSQL carries without comment and which
`DETACH … CONCURRENTLY` makes irrelevant to inserts.

**Removal that cannot block a write, and a ledger that refuses rather than strands.** Revision 6 said
"DROP" and left the locks unsaid; a plain `DROP TABLE` of a partition takes `ACCESS EXCLUSIVE` on the
parent and would have queued every decision insert behind it. Revision 7 detaches CONCURRENTLY (`SHARE
UPDATE EXCLUSIVE` on the parent only) and drops the standalone table afterwards, finalizes an
interrupted detach on the next pass, and runs every DDL under `lock_timeout 2s` / `statement_timeout
10s`, retried next pass rather than waited out. It also declines the DEFAULT partition heartbeats have:
the decision row is written in the decision transaction, so a decision the ledger cannot hold is not a
decision — the insert fails, the API answers 503 `ledger_unwritable`, nothing is emitted. A lead of
created-ahead days (default 7) and a `writable_horizon_seconds` gauge make that failure need a leader
absent for the whole lead, by which time `max_seal_lag_seconds` has long since turned every decision
into `UNKNOWN`. Stranding rows in a DEFAULT partition would instead have needed a row-level DELETE to
honour retention — the very thing D-0193 removed.

**Off the dispatch loop, this time actually.** Revision 6 claimed the purge ran "inside
`subCadenceTimeout`, off the dispatch loop". The scheduler's `withTimeout` runs its function inline on
the ticker; a slow DDL there would have held monitor dispatch for up to 30 s. The maintenance pass now
runs in the leader's own goroutine in the shape `serviceFactMaintenanceLoop` already has — started with
leadership, cancelled and joined before `lead` returns — and invariant 18 requires a test that blocks the
detach while the dispatch tick keeps firing.

**Identity on a partitioned table.** A primary key on a RANGE-partitioned table must contain the
partition key, so it is `(evaluated_at, id)` and `id` is not database-unique across partitions. The
route stays id-only and honest about it: `gen_random_uuid()` ids, a read that probes the `(id)` child
index of every attached partition without pretending to prune, and a 500 `ledger_identity` (the contract at the time) rather than a
choice if two rows ever share an id. No registry table, no sequence.

**Bytes.** The capacity table counted rows; each row carried JSON evidence with an unbounded list of
definition revisions. The list is now `{count, first_id, last_id, digest}` — recoverable, this record said at the time, from the retained
revisions by the window the evidence names — evidence is canonical JSON, the row is CHECK-bounded (4 KiB
+ 1 KiB + 4 KiB), and the table gained byte columns at 1.5 KiB typical and 10 KiB bound, with a realistic
200-decisions-a-day row so the saturated numbers read as what they are.

**The guard's vocabulary.** Revision 6's matrix still required `decision_purge_batch` (the name at the time)
and two metric names §5a no longer defined, and the guard passed because its retired list stopped at revision
5. The list now retires revision 6's spellings too, with fixtures for each.

**Not reproduced.** The review reported the D8a heading duplicated at spec lines 205–206 of `4e5bd15`;
at that hash line 205 is the heading and 206 is prose, and the heading occurs once. Recorded so the
claim is not re-litigated.

## D-0195 — FR-024 revision 8: a fence made of the connection, creation under the weaker lock, a registry of what is ours, and an id that names its day (2026-08-28)

**Context.** Focused confirmation of revision 7 (`476643a`) returned 1 P0, 7 P1 and 3 P2 (party [35]),
and retracted round 6's D8a duplicate (two overlapping `sed` ranges). Every item is a runtime fact of
PostgreSQL or of this codebase, verified before it changed the text; none needed the owner.

**The fence is the connection.** Revision 7 ran the maintenance pass in a goroutine of the scheduler
leader and called "cancelled and joined before `lead` returns" a fence. It is not one: the scheduler's
advisory lock sits on a pinned connection while its work uses the pool, so when that connection dies
PostgreSQL frees the lock at once, a successor may start, and the old node keeps issuing statements
until its next watchdog tick. The pass now takes the gate's own `store.LeaderSession` — the shape
file-provider apply already uses for exactly this reason — and every statement of a pass runs on the
lock-owning connection. A dead connection has no lock and can carry no statement; the deposed side is
silenced by construction. The test terminates the old backend mid-detach and requires the successor to
remove the partition exactly once.

**Creation under the weaker lock.** The PostgreSQL 15 reference says it plainly: creating a partition
with `PARTITION OF` requires `ACCESS EXCLUSIVE` on the parent. Revision 7's creation would have queued
decision inserts behind it for up to the statement timeout. Partitions are built standalone (`LIKE …
INCLUDING …` plus a bounds CHECK) and `ATTACH`ed under `SHARE UPDATE EXCLUSIVE`. Every missing day in
the lead is created every pass, unbudgeted at the time; the removal budget is separate; and configuration load
refuses a cadence-and-budget pair that cannot remove two partitions a day.

**A lifetime bound that is true.** Daily granularity adds up to a day, the cadence up to another, and a
refused lock or a lost session adds what it adds. The spec now promises `retention + 1 day +
purge_every` under a healthy pass and calls everything past it backlog — gauged and alerted, not
promised away.

**The deferred drop.** PostgreSQL 15.9's release notes fix "possible crashes and 'could not open
relation' errors in queries on a partitioned table occurring concurrently with a DETACH CONCURRENTLY
and immediate drop of a partition"; cerbix enforces 15.0. Rather than raise the floor, the drop waits a
cadence and runs only when no backend holds a transaction older than the detach — safe on every
supported 15.x, with ≥ 15.9 recommended in the runbook.

**A registry of what is ours.** `IF NOT EXISTS` proves nothing about the relation that exists, and a
table left standalone by a crash between detach and drop has no parent link to prove it was ever ours.
`service_gate_decision_partitions` records day, deterministic name, OID and state; every act validates
the catalog against it on the same connection and refuses the whole day on any disagreement, counted
and paged. A crash after a completed detach is the ordinary reconciled case.

**The id names its day.** Revision 7's id-only read probed every attached partition — up to 396 with
the maximum lead, not the 373 written at the time — with no limiter on reads. The id is now a UUIDv7 whose timestamp is
`evaluated_at`'s millisecond, taken from the database clock inside the decision transaction so it cannot
disagree with the partition key; the read prunes to one partition, refuses out-of-window ids without a
query, and shares the in-flight permits.

**Two smaller truths.** The revision digest is the complete surviving evidence: `service_definition_revisions`
cascades with its service and `DeleteService` cascades the rest, so revision 7's "recoverable from the
retained revisions" was false on a path D10 expressly supports. And the two rate buckets are debited as a
unit or not at all; revision 7 debited the principal's before finding the process bucket empty.

**Not reproduced.** "`two different limiters` repeated twice in §5a": at `476643a` the phrase occurs once
(the §5a opening paragraph). The §8 "eight bounds" was real and is nine.

## D-0196 — FR-024 revision 9: a registry that survives its own crashes, ownership by marker, two lifetime boundaries, and a listing the SPA can build (2026-08-28)

**Context.** Focused confirmation of revision 8 (`63c1d57`) returned 8 P1 and a set of P2 (party [37]),
and retracted round 7's "two different limiters twice" (the same overlapping-`sed` cause as the D8a
retraction). Every item is a contract or runtime fact; none needed the owner.

**Crash consistency.** Revision 8's registry had four states and declared every registry/catalog
disagreement terminal — yet an ordinary crash between a DDL and its registry write produces exactly
such a disagreement, and a refused day with no DEFAULT partition eats the writable horizon. Revision 9
makes every transition PostgreSQL allows inside a transaction atomic with its registry write (create +
unique index + comment + insert; attach + state; drop + state) and gives the one non-transactional
statement, `DETACH … CONCURRENTLY`, a write-ahead `detaching` intent with all three crash outcomes
reconciled. The matrix crashes the pass after every statement.

**Ownership by marker.** An OID is a locator, not an identity: `pg_dump`/restore renumbers every
relation and OIDs are reused after DROP, so revision 8 would have refused every partition of a restored
installation. Each partition carries `COMMENT ON TABLE … 'cerbix:gate-ledger:<owner_token>'`, written in
the creating transaction; the pass validates marker and shape and refreshes `relid` when the marker
matches a new OID. Refusal is reserved for what no crash produces, skips only that day, and is counted in
a maintenance error family of its own rather than the evaluation one.

**Two boundaries.** Revision 8's single lifetime formula was a cadence short — the drop it deferred
added a second one — and let "lifetime" mean parent visibility while the gauges counted the detached
table. The spec now states logical readability (until detach: `retention + <1 day + ≤ purge_every`) and
physical retention (until the deferred drop: `… + ≤ 2 × purge_every`) separately, and the application-
side "404 without a query" prefilter is gone: it needed a `today` no API node owns and contradicted
backlog, and PostgreSQL prunes the decoded-day predicate to zero children anyway.

**Budgets that converge.** Steady state is two removal stage-ops a day (detach today's eligible day,
drop yesterday's detached one); revision 8 accepted exactly that and called it catch-up. Load now
refuses fewer than four a day, and creation — left without a budget in revision 8, up to 31
two-statement sequences at the maximum lead — has its own budget, nearest horizon first, with its own
convergence check.

**The id, enforced.** Revision 8 pruned the read to one partition by the id's day while still specifying
a 500 for "two rows with one id in two partitions" — a case that query could never see — and trusted the
writer to keep id and `evaluated_at` in step. The database now CHECKs the id's millisecond against
`evaluated_at` and each partition carries a local unique index on `id`, so the case is impossible and
the 500 is gone.

**The listing.** The SPA decision history had a query note and no contract. It has a route now:
required range of at most 31 days, page size at most 200, keyset order `(evaluated_at, id) DESC`, opaque
cursor, empty page for a foreign service, and a test that pages under a concurrent writer.

**Mechanical drift fixed.** The doubled `cerbix_gate_decisions_bytes` clause in D10 (a replacement that
kept its end marker) and the *Ledger* matrix line that still said "rows older than retention are
purged". The reported repeat of "Revision 5 gave numbers…" on consecutive lines of §5a does not
reproduce at `63c1d57` (`grep -c` gives one) — recorded, as the earlier two were.

## D-0197 — FR-024 revision 10: session settings that stay in the session, the policy/override routes, indexes that match the listing, and reservations instead of counts (2026-08-28)

**Context.** Focused confirmation of revision 9 (`80a89ff`) returned 6 P1 and a set of P2 (party [39]),
and retracted round 8's third overlapping-`sed` finding; the reviewer has stopped using adjacent
inclusive ranges. None of the items needed the owner. From this revision the spec text lives in the
working tree until the gate is confirmed — the owner asked that revisions not be committed one by one
(2026-08-28), which the standing commit policy already said and this arc had ignored.

**Session poisoning.** `DETACH … CONCURRENTLY` cannot run in a transaction, so its timeouts are
session-level `SET`s, and revision 9 returned that pinned connection to the shared `pgxpool` with them
in place — the next borrower would have inherited a 2 s lock timeout at random. Every session `SET` is
now paired with a `RESET` on every path, `Release()` resets again, and a connection whose reset cannot
be proved is hijacked and closed rather than returned; the matrix reacquires and reads `SHOW` on the
clean, lock-timeout, statement-timeout and dead-connection paths. The gate's advisory key is slot 3 of
the `"cerbix" + slot` namespace, with a distinctness test the repository did not have.

**The routes.** Policy was "a write" and override a shorthand for nine revisions. D13a pins six routes
with `expected_revision` on policy `PUT`/`DELETE`, one active override bound to the revision the caller
saw, and revocation by immutable override id — because "delete the current override" from a stale
screen would have revoked a newer one. The A-expired/B-created race is in the matrix.

**Indexes the listing actually uses.** The keyset order was `(project_id, evaluated_at DESC, id DESC)`
over an index on `(project_id, evaluated_at DESC)`, and an optional `service_id` would have filtered a
busy project's whole 31-day range for a sparse service's 50 rows. Two indexes now match the two paths;
the redundant parent `(id)` index is gone; each partition carries exactly four indexes, and the
physical-size estimate is stated over that set. The range is half-open, `from < to`, the cursor binds
strictly below by row comparison, `LIMIT + 1` produces `next_cursor`, a malformed cursor is 400.

**Reservations, not counts.** Revision 9 bounded creation by a count whose own arithmetic
(`2 × 2 s × 8 = 32 s`) exceeded the 30 s pass. Creation now stops at its count or at 12 s, whichever is
first, so removal always has a reserved slice; the convergence arithmetic is labelled the healthy,
uncontended claim it is. The drop predicate is inclusive (`<=` one cadence), which is what makes
"physical retention `≤ 2 × purge_every`" arithmetic rather than hope.

**Drift.** The threat row said reads were rate-bounded — they take concurrency permits only; list items
listed `state` and `action` as additions — `state` is already always present and `action` is absent for
`NOT_CONFIGURED`, never `null`. Both fixed and their spellings retired.

## D-0198 — FR-024 revision 11: a revision that is a generation, bodies that carry everything, a cursor that skips nothing, and a release that is a proof (2026-08-28)

**Context.** Focused confirmation of revision 10 (working tree over `80a89ff`) returned 7 P1 and 2 P2
(party [41]), no retractions and no sed artefacts. None of the items needed the owner; the text stays in
the working tree.

**The generation.** Revision 10's CAS was defeated by its own schema: `DELETE` removed the only policy
row and a re-create started at revision 1 again, so a screen holding the old revision 1 could write over
— or override — a different policy. The policy row is now never deleted: `DELETE` tombstones it and bumps
the revision in the same statement, a re-create is an `UPDATE` at `revision + 1`, `expected_revision:
null` matches only absent-or-tombstoned, and the CAS runs before D14's no-op comparison so an identical
body with a stale revision is 409. The delete-recreate race joins the stale-UI race in the matrix.

**The bodies.** Revision 10's "exact" policy body omitted `schema_version` and `budget_consumed_percent`
while D11 says the server fills nothing in, and the override body carried an `action` D9 does not let a
client choose. The fence now lists every D11/D14 field, bodies are decoded strictly, and the override
body has no `action`.

**History.** Revision 10 promised inert overrides "remain readable history" and gave only an active-only
`GET`. There is now a by-id read of any override with both actor triples and a derived status, and a
bounded list of the last 50; invariant 17's "later read" names the route.

**The cursor.** `LIMIT + 1` fetched the extra row and revision 10 encoded the cursor from it, so with a
strict lower bound that row was returned on neither page. The probe now only answers "is there more";
the cursor comes from the last returned row; `limit = 2` over three rows yields 2 + 1.

**Reservations enforced per statement.** Checking elapsed time between operations let an attach started
at 11.9 s run a 10 s statement timeout into the removal slice. Timeouts are clamped to the slice deadline
per statement and a step does not start unless its clamped worst case fits.

**Release as a proof.** `LeaderSession.Release()` today unlocks on `context.Background()`, ignores the
unlock's boolean and returns the connection; a blackholed `RESET` would have held shutdown forever and an
unknown unlock outcome could have pooled a lock-owning connection. Cleanup now runs under its own 3 s
deadline, must observe both `RESET`s and `pg_advisory_unlock`'s boolean, and hijacks-and-closes on any
unknown. The test oracle is the poisoned pid never re-borrowed and the successor's acquisition, not pool
cardinality, which `MinConns` replenishment makes unstable.

**Smaller truths.** A 31-day query over daily partitions plans an Append with one child scan per
surviving partition; the EXPLAIN contract asserts matching child indexes and no post-filter, not "one
scan". Item presence in the listing equals the by-id response, with present-and-null `service_id` for a
deleted service distinct from absent `override_id` for "never applied".

## D-0199 — FR-024 revision 12: timers that are wall bounds, one 30-second timeline, an override status that is a function, and a history that is a window (2026-08-28)

**Context.** Focused confirmation of revision 11 (working tree) returned 4 P1 and 3 P2 (party [43]),
all consequences of revision 11's own new text. None needed the owner; the text stays in the working
tree.

**Timers are not additive.** Revision 11 admitted a creation statement only if `lock_timeout +
statement_timeout` fit before the slice deadline. PostgreSQL's `statement_timeout` measures the whole
command, lock wait included, and `lock_timeout` is a narrower timer inside it — so after the first fast
statement of the create transaction the 12 s sum no longer fit and the transaction would have rolled
back on every pass. The clamp is now a wall bound: `statement_timeout = min(10 s, remaining)`,
`lock_timeout = min(2 s, statement_timeout)`, the transaction's context deadline at the boundary, no
start under 500 ms remaining; the matrix runs a whole create transaction under a fake clock advanced
between statements.

**One timeline.** Revision 11 bounded the pass at 30 s and then gave cleanup an independent 3 s, making
33. Work now ends at 27 s (creation to 12, removal to 27) and the release proof owns `[27, 30]`;
invariant 18 measures acquire→cleanup as one 30 s lifecycle, including the case where removal spends its
budget and a `RESET` blackholes.

**Status as a function.** Revision 11's four override statuses overlapped its own write lifecycle: D9
closes an override as `policy_changed` when the policy is edited, yet the text called that row inert; B's
creation closes A as `expired`, yet the matrix expected A without `revoked_at`. Every closure now sets
`revoked_at` and `revoked_reason`; only `manual` carries a human revoker; `status` is a six-row
precedence table over reason, expiry and revision match, table-tested with a precedence mutation.

**History is a window over unbounded storage.** Revision 11's claim that the one-active, seven-day
regime keeps override history small was false — a project admin can create and revoke in a loop forever. The list is a bounded read, newest 50 by
`created_at DESC, id DESC` over a matching index so a hot service never sorts and equal timestamps order
deterministically; the audit log is the record.

**Smaller truths.** §4 still counted six routes (there are eight, and the guard now retires "six");
`DELETE` without or with a malformed `expected_revision`, a malformed override id and a non-positive
`limit` each have a pinned 400; the EXPLAIN index assertions are made on populated, analysed children
only, since the planner may rightly seq-scan an empty one.

## D-0200 — FR-024 revision 13: a status over facts, a traversal that says what it is, and a timeline that includes acquiring it (2026-08-28)

**Context.** Focused confirmation of revision 12 (working tree) returned 2 P1 and 3 P2 (party [45]);
the timer algebra, the 27/3 split, the history index, the 400s and the populated-only EXPLAIN proof were
accepted. None of the remaining items needed the owner; the text stays in the working tree.

**Status over facts.** Revision 12's precedence ranked a recorded `expired` reason above a policy
mismatch, so an unrevoked, expired, mismatched row read `inert` — and then read `expired` after an
unrelated override's creation wrote the `expired` reason onto it. The same persisted row changed status
because housekeeping ran, contradicting the sentence beneath the table. The precedence now joins each
reason with the live fact it records — manual → revoked; mismatch or tombstone or a policy reason → inert;
expiry by reason or by clock → expired; else active — so a closure that records what was already true
cannot move a status, and the matrix asserts the overlapping case on both sides of its closure.

**The traversal says what it is.** Revision 9 "proved" that a concurrently inserted decision cannot be
skipped because its `evaluated_at` is later than page 1's first item. It is taken near the start of the
decision transaction, not at commit; a decision begun before page 1 and committed after lands inside the
traversal, and a partition detached between pages removes rows. The listing is now stated as live keyset:
a returned key never repeats, rows committed before page 1 and still attached appear exactly once,
concurrent commits and detaches are outside the guarantee, and the matrix holds a decision transaction
open across page 1 to make the limitation visible. A pipeline that needs a fixed set reads by id.

**The timeline includes acquiring it.** `pool.Acquire` blocks; a pass whose authority arrives at 13 s
cannot begin removal at 12 s. Slices are now relative to acquisition — late authority skips creation and
starts removal at once if ≥ 500 ms remain before 27 s — and the cleanup deadline is the absolute
`min(now + 3 s, passStart + 30 s)`.

**Attribution and nullability.** "Non-null for manual" was wrong for a manual revoke by an API token,
whose `revoked_by_user_id` is intentionally null under the typed-token contract. Attribution is PRESENT
for `manual` only, with the label always set and the user id nullable; every field of the override
record is always present, never absent — active rows carry the five closure fields as null, system
closures set the two lifecycle fields and leave attribution null.

## D-0201 — FR-024 design gate APPROVED at revision 13; what the approval does and does not cover (2026-08-28)

**Context.** Focused confirmation of revision 13 (working tree over `80a89ff`) returned no findings
(party [47]). The reviewer re-read the working-tree text independently rather than the disposition, ran
`git diff --check` and `make docs-check` (17 guard fixtures, the full references and acceptance-map
check), and recorded that no FR-024 code exists.

**Decision.** The design is approved as written in `docs/specs/func-reliability-gate.md` at revision 13.
Revisions 10–13, which lived in the working tree at the owner's instruction, are committed together as
ONE accepted design phase — the commit-per-review-round pattern of revisions 6–9 (`4e5bd15`, `476643a`,
`63c1d57`, `80a89ff`) is the exception this record closes, not the rule.

**What the approval is not.** It is not implementation approval: FR-024 and NFR-019 stay `TODO` until
implementation discharges every one of the 21 invariants of §6 and the full §7 matrix with named tests
and mutations. The UI mock gate named in the lifecycle — an owner-approved mock of the policy editor and
the decision history before any SPA code — still stands. The PostgreSQL 15.8 prepared-statement case is
approved as a REQUIRED implementation gate to run, not as something claimed done.

**Record of the arc.** Thirteen revisions, twelve review rounds, five owner decisions (D-0188, D-0189,
D-0190, D-0193, D-0194), eight implementer-authority revisions (D-0191, D-0192, D-0195 … D-0200), three
reviewer retractions (overlapping `sed` ranges, each recorded), and a stale-spelling guard that retired
the vocabulary of every superseded revision and caught its own author in prose seven times before a
commit.

## D-0202 — FR-024/NFR-019 deferred: an approved design, parked until somebody asks for it (2026-08-28)

**Context.** The design gate was approved at revision 13 (D-0201) and the UI mock passed the reviewer's
static contract pass at its third revision (party [59]; visual approval by the owner still pending). Asked
what the requirement is for, the owner heard the plain answer — a deploy gate a CI pipeline asks before
releasing a service, driven by the service's error budget and live alerts, every answer recorded — and
where it came from: the reviewer proposed it in the party as the logical continuation of FR-021, and the
owner said yes to writing it as a design gate. No cerbix user had asked for it.

**Decision (owner, 2026-08-28): option 2 of three — defer.** The specification stays approved at revision
13; the mock stays in `docs/design/` with its notes section marked deferred, neither approved nor rejected
visually; no code is written, no migration is added, no Vue file is touched. FR-024 and NFR-019 stay
`TODO` in the status — the deferral is stated in the row text, not as a fourth status value: the
owner declined to widen a vocabulary `docs-check` deliberately keeps to three states for one row. The alternatives were to build it in full (a phase the size of FR-021) or to
build a minimum (policy + `gate check` + ledger, no overrides and no history UI); both were declined for
now because the effort would go into a capability nobody has asked to use, while approved-but-unbuilt
surfaces from iter-0155 still wait on the owner.

**What deferral preserves.** Thirteen spec revisions, thirteen owner and implementer decisions
(D-0188 … D-0201), the stale-spelling guard and its 18 fixtures — all of it stays in the tree and keeps
being checked by `make docs-check`, so the design cannot rot silently. What it does not preserve is
currency: the codebase the spec cites (`LeaderSession`, `serviceFactMaintenanceLoop`, `subCadenceTimeout`,
the storage-mode branches) will move, so **resuming means a fresh focused review against the then-current
code before any implementation**, then the owner's visual approval of the mock, then the §6/§7 discharge.

**Closed as iter-0162** (docs only; the reviewer's [63] named that the arc had run outside the iteration workflow).

**Recorded for the process.** The question "what is this spec for?" arrived after twelve review rounds.
It should have been asked — by me — before the first one: a design gate opened on a reviewer's proposal
needs the owner's *why* stated in the spec's first paragraph, not inferred from a "давайте".

## D-0203 — the four per-round FR-024 commits squashed; the hashes the record cites stay resolvable (2026-08-28)

**Context.** Revisions 6–9 of the FR-024 specification were committed one per review round (`4e5bd15`,
`476643a`, `63c1d57`, `80a89ff`), against the commit policy of 2026-08-16 (iter-0162 §5). The owner asked
for them to be squashed. All of it was local and unpushed; nothing after `v0.1.5` has left this machine.

**Decision (owner, 2026-08-28).** The four commits are squashed into one, `ef88535` ("design revisions 6–9,
squashed"), and the three commits that followed are replayed on top of it unchanged in content. Because
their parents changed, their hashes changed too:

| Record cites | Now |
| --- | --- |
| `4e5bd15`, `476643a`, `63c1d57`, `80a89ff` (revisions 6–9) | `ef88535` |
| `07a1e9b` (design approved at revision 13, D-0201) | `5df6ed2` |
| `9877d30` (deferred, D-0202) | `ad31309` |
| `0b9c525` (iter-0162) | `36f3134` |

**What keeps the record honest.** The spec banner, D-0201, D-0202, iter-0162 and the party transcript cite
the OLD hashes, and iter-0162 is immutable, so they are not rewritten. Instead the old tip is kept under
the tag `backup/pre-squash-2026-08-28`: every cited hash stays a real, `git show`-able object in this
repository, and this table is the map from citation to current history. The tree at the new tip is
byte-identical to the old one (`git diff 0b9c525 36f3134` is empty), `make docs-check` passed on it before
`main` moved, and the reviewer's approved range `9877d30..0b9c525` is now `ad31309..36f3134` with the same
content.

**Not done.** Revisions 1–5 (`46379fa` … `3771622`) were left as they are — they predate the owner's
repeated instruction and the party's verified ranges start after them — and nothing was pushed.

## D-0204 — FR-024/NFR-019 resumed: implementation of the approved design as iter-0163 (2026-08-29)

**Context.** One day after deferring the reliability gate (D-0202), the owner asked to start work on
`docs/specs/func-reliability-gate.md` under the full AGENTS.md workflow, with role separation through
subagents, the party listener never stopped, and commits made only when code actually changes — never
pushed by the implementer.

**Decision (owner, 2026-08-29).** FR-024 and NFR-019 are `IN_PROGRESS` in iter-0163. The design stays
revision 13, unchanged; nothing in D-0201's approval is reopened. D-0202's resume path is honoured in
order: the reviewer's fresh focused review of the specification against the current code (requested
in the new party, [6]) — trivially current since the code has not changed since the approval, but asked
for rather than assumed; the owner's visual pass on the mock before any SPA code, while backend, CLI and
API proceed under the approved design; and the discharge of the 21 invariants and the §7 matrix, with the
PostgreSQL 15.8 prepared-statement case run, not asserted.

**Shape of the work.** Eight tasks P0→P2 in `iter-0163.md`: schema + domain + config; store core
(policies, overrides, the one-transaction decision, ledger reads); the fenced ledger maintenance; API +
limiter + metrics + audit + OpenAPI; the CLI; the test matrix; the docs that must move; and the SPA,
gated. Each lands as its own commit when its code is green; the reviewer sees each range in the party.

**What this record does not change.** D-0202's reason stands as written — the requirement came from the
reviewer's proposal, not a user ask — and its process lesson stands too; the owner's decision to build
anyway is the owner's, recorded here without re-arguing it.

## D-0205 — FR-024 at implementation: three forward corrections to revision 13 (2026-08-29)

**Context.** Agent C, writing the runbook and overview against the approved specification and the
landed code, found the specification disagreeing with itself in one place and silent in two others.
Corrections are made forward, in the text, each marked with this record; nothing in D-0201's approval
is reopened.

1. **The policy write action is `gate:policy:write`.** D12 names the three `authz.Action`s
   (`gate:evaluate`, `gate:policy:write`, `gate:override`); D13a's authorization sentence said
   `gate:policy`. D12 is the section that defines the actions; D13a is corrected to match it.
2. **CLI usage errors exit 2; redirects are not followed.** D16 defined the decision exit codes and left
   usage errors and 3xx unstated. The CLI exits 2 on a usage error — the exit-code family every `cerbix`
   verb already uses — with no stdout line, so no pipeline can read it as `BLOCK`; and it refuses to
   follow a redirect (exit 1, naming the `Location`) because following one would send the bearer to a
   host the operator did not name. D16 now says both.
3. **The horizon alert pairs with `absent()`.** The four ledger gauges are exported only by the process
   holding the gate session and are cleared at step-down (a deposed node does not speak), so a vanished
   leader makes `cerbix_gate_decisions_writable_horizon_seconds` ABSENT rather than low; the §4 threshold
   and the runbook say so.

**Also corrected while there** (stale living docs Agent C noticed outside FR-024): the
`docs/specs/README.md` row for `func-service-incidents.md` still called it "not commissioned" after
FR-022 closed at iter-0156 (D-0171), and `docs/overview.md`'s FR-021 paragraph still said the budget and
burn numbers were "phase 2 and absent from the API" long after phase 2 shipped.

## D-0206 — FR-024 ledger maintenance at implementation: three deviations from D10's letter, each toward its intent (2026-08-29)

**Context.** Changeset 3 (the fenced partition pass, `internal/store/gatemaintenance.go`, iter-0163) met
three PostgreSQL facts the specification did not name. Each is resolved toward what D10 protects —
inserts never blocked, authority never outliving its connection, a refusal never wider than one day.

1. **The client-side deadline is the slice end plus a 100 ms net.** D10 says the transaction's context
   deadline "is the slice boundary itself". The server's `statement_timeout` is clamped to the slice, so
   the server speaks first; a client deadline at the SAME instant would race it and turn an orderly
   `57014` into a context cancellation on the pinned connection — after which pgx closes the connection,
   which is the fence, but the pass would lose the server's classified error. The net is the repository's
   existing pattern (the deadline wrapper reserves the same way); a silent server still falls to the net.
2. **Shape is checked per act, not in the survey.** `pg_get_expr(relpartbound)`, `pg_get_constraintdef`
   and `pg_total_relation_size` take `ACCESS SHARE` on the relation (and `pg_get_expr` on the parent), so
   a survey that read every partition's shape would stall on one hand-locked partition and refuse the
   whole pass. The survey reads only marker, state and `pg_inherits`; the shape of ONE day is checked
   right before acting on it, under that day's clamps, so a locked partition refuses its day by
   `lock_timeout` and the other days proceed — D10's "that day alone", kept.
3. **Gauges are non-fatal when a partition is locked.** For the same reason `pg_total_relation_size` can
   block; a pass whose gauge query hits `lock_timeout` publishes NO gauges for that pass
   (`GaugesValid = false`, the failure listed, not counted as an error), rather than failing the pass or
   publishing a partial sum. The previous values stay exported until the next successful pass.

**Also decided here.** The scheduler calls ONE store method, `RunGateLedgerMaintenancePass`, which owns
acquire → work → release as a single lifecycle: authority is the gate session, never scheduler
leadership. The gate advisory key is `0x6365726269780003` — slot 3 of the `"cerbix" + slot` namespace —
with a pairwise-distinctness test. Wiring: `--role all` and `--role scheduler` construct the loop from
the five `gate.decision_*` keys; the gate session pins one pool connection for at most 30 s per pass.

## D-0207 — FR-024 SPA phase opened: the owner approved the mock; how the mock is read into product surfaces (2026-08-29)

**Decision.** The owner approved `docs/design/mock-reliability-gate.html` visually on 2026-08-29 («апрув
мока», chat), which is the gate D-0204 left in front of AC-0163-8. The SPA is built to the mock's six screens
as follows, and every reading below is a decision, not a drift:

1. **Product surfaces.** Screen 1 → the `Release gate` card on the service detail, after Alerting and before
   Dependencies (D13: facts → who is woken → who may deploy), with its empty state, the "Latest decision ·
   last 30 days" card and a compact "What a pipeline sees" card (the `cerbix gate check` command for THIS
   project and service, the four exit codes). Screen 2 → the policy editor inline in that card (every field
   explicit, client validation mirroring the server's rules, Save/Discard/Delete with `expected_revision`,
   the delete confirmation listing what changes and what is kept, the 409 banner with Reload). Screen 3 → the
   latest-decision detail with its reasons and evidence, the active-override card and the add-override form.
   Screen 4 → a project-scoped `Gate decisions` view (`/gate/decisions`, `?service=` pre-filter from the
   card's "all decisions →", ≤ 31-day range, cursor paging, "Show 50 more", the `range_too_wide` refusal
   inline) and a by-id view (`/gate/decisions/:id`) rendering the full immutable record. Screen 5 → a
   per-service `Override history` view (`/services/:id/gate/overrides`). Screen 6 → states of the
   latest-decision card (UNKNOWN with `seal_stale`, `never_sealed`, budget withheld) and a compact
   five-answers legend inside the card's help.
2. **Explanatory cards are not built.** "Who sees which controls" and the CLI transport examples (429/503)
   teach the reviewer the contract; the product expresses them by NOT rendering a control the role cannot
   use and by the CLI's own stderr. Nothing in the mock's spec-notes becomes UI.
3. **RBAC in the SPA.** `gate:evaluate` is viewer+, so the card and both histories render for everyone who
   sees the service; `gate:policy:write` controls render for `session.canProjectWrite` (editor+); the
   override controls render only for a NEW helper `session.canProjectAdmin` (org_admin/project_admin or
   global admin) — the existing helper does not separate editor from project_admin and the mock requires
   the separation. The service being file-managed does NOT make the gate read-only (D13), so the card does
   not take the detail view's `canWrite` (which excludes managed services) but derives its own flags.
4. **The SPA never asks the gate.** The latest decision is the newest ledger row for the service over the
   last 30 days (`limit=1`), the same read as the history view; opening a page never creates a decision
   (§9). The error-budget figure on the card is the value the decision itself quoted — `reasons[].value`
   of a budget clause — and is omitted when no budget clause is in `reasons[]`; the card never fetches a
   fresher number from the report path to stand beside a decision it did not belong to (NFR-019's "same
   snapshot" read strictly). The mock's KPI is rendered when, and only when, the decision carries it.
5. **Concurrency discipline as `ServiceAlerting.vue`.** Every load is guarded by a generation counter and
   an `AbortController`; unmount and route/workspace changes abort in-flight reads and drop stale
   responses; writes re-read after success; refusals are shown verbatim through one code→message map
   (`revision_conflict`, `override_active`, `override_not_active`, `expected_revision_required`,
   `not_configured`, `range_too_wide`, `none_active`).

**Why.** The mock is the approved contract and the six screens map onto four product surfaces; writing
the mapping down keeps "fidelity to the mock" reviewable against a named list rather than an impression.

**Consequences.** AC-0163-8 IN_PROGRESS (iter-0163 §0 task 8, sub-tasks 8a–8d); row 15's "shown on the
service page" clause is discharged when `seal_lag` renders on the card and a test reaches it; the
`make dev-test` count grows by the UI spec; `make spa-snapshot` after the frontend change (CLAUDE.md).

## D-0208 — iter-0163 closed: FR-024 / NFR-019 (the reliability gate) are DONE (2026-08-29)

**Decision.** The iteration that implemented the approved design (D-0201) closes with FR-024 and NFR-019
`DONE`. Basis: every acceptance criterion AC-0163-1…8 is DONE with evidence (`docs/status.md`); the FR-024
discharge table in `docs/traceability.md` reaches all 21 invariants of §6 with named tests and the
mutations killed; the §7 matrix ran in both storage modes; the independent reviewer approved every range on
record — [16], [26]/[30], [44]/[46], [60], [69], [74], [82]/[84], [94] — and no finding is open. The SPA was
built only after the owner's visual approval of the mock (D-0207) and shipped with the three review
findings of its design pass fixed and tested ([86], [88], [90]).

**What the record keeps.** Three forward corrections to the design (D-0205), three maintenance deviations
toward D10's intent (D-0206), the mock-to-product reading (D-0207); the service detail now carries its SLO
target inventory (`3fc19e3`), a small API addition the SPA needed. Commits are one landed change each, per
the owner; docs-only commits were made at phase completion at the reviewer's request ([71]) so the committed
status never lagged landed work. The close-out needed two commits because the first, `98dd58d`, was
committed with a message claiming content its aborted edit script had not written (iter-0163 §4). Nothing
was pushed; pushing, tagging and any squash are the owner's.

**Consequences.** `docs/specs/func-reliability-gate.md` header reads IMPLEMENTED; iter-0163 is immutable
from its closing commit; the next requirement is the owner's call (FR-025 Change Intelligence is a
candidate the spec names as out of scope here).

## D-0209 — FR-025 Change Intelligence: design approved at revision 3; the owner's seven answers (2026-08-30)

**Decision.** `docs/specs/func-change-intelligence.md` is the approved design of FR-025 / NFR-020. The
independent reviewer approved revision 3 at party [15] after two rounds (revision 1 at [9]: 2 P0 — tenant
integrity of the link table, a false persistence claim on the comparison; 4 P1; 3 P2. Revision 2 at [13]:
2 P1 — the DB CHECK claiming Unicode rules a CHECK cannot enforce, the normalization owner per layer
unstated; 1 P2). The owner answered the seven questions of §11 the same day, accepting every
recommendation: (1) three kinds — `deploy | rollback | flag` — as D17 named them; a configuration change is
a `deploy` with a `ref`; (2) a service's changes CASCADE on delete — a change is the service's fact, the
incident note remains as text — the ledger's outlive rule is deliberately not copied; (3) correlation
reaches the own service and its `probable_root` upstream services only, never downstream; (4) comparison
horizons are `15m | 1h | 6h | 24h`, no `7d` — a slow burn is the burn rules' job; (5) a token's `actions`
list is immutable after create — a different list is a new token; (6) the CI capability is the per-token
ALLOW-LIST intersected with the role inside `authz.Can`, not a dedicated `ci` role — one mechanism that
generalizes to every future action; (7) `external_id` identity is case-sensitive after NFC — CI run ids are
opaque and folding would merge identities a system distinguishes.

**Why.** The design phase closes the way FR-024's did (D-0201, D-0203): the reviewer owns consistency and
testability, the owner owns the product choices, and the accepted phase is committed once.

**Consequences.** FR-025 and NFR-020 enter `docs/status.md` as `TODO` with the design cited; the spec's
header reads DESIGN APPROVED; iter-0164 task 3 is DONE. Before any SPA code: an owner-approved UI mock of
the service timeline, the changes card and the before/after comparison. Backend work — schema, domain,
store, API, CLI, the token allow-list — may proceed under the approved design in the next iteration, as
FR-024's did while its mock waited. Numbers reserved: iter-0165 for the implementation.

## D-0210 — FR-025 UI mock approved: how the six screens become product surfaces (2026-08-30)

**Decision.** The owner approved `docs/design/mock-change-intelligence.html` visually on 2026-08-30 («мок ок»,
chat); the independent reviewer approved its static contract the same day (party [29], after [23] and [27]:
no by-identity group route, an explicit horizon control on the card with its scope stated, the started-only
group shown without a strip mark and with "before/after unavailable until a terminal phase"). AC-0165-7 is
open. The mock is read into the product as follows, each reading a decision:

1. **Screen 1 → the `Changes` card** on the service detail between `Release gate` and `Dependencies`: change
   marks on the existing `ReliabilityStrip` — one per TERMINAL phase, placed by `occurred_at`, kind-shaped
   (▲ deploy ▼ rollback ◆ flag) in the accent, never a state hue; grouped rows newest by latest phase with
   phases in the domain order, source/identity, actor, the gate decision the change rested on (live
   state/action or `aged out`), `preceded INC-… by −lag`, and a before/after figure at the horizon chosen in
   the card header (`range` and `before/after at` are two explicit controls; the horizon applies to every
   row); a started-only group is listed with "no terminal phase yet — no mark, before/after unavailable";
   an empty state with the CLI line. NO record form: the record is the pipeline's (D14).
2. **Screen 2 → nothing in the SPA**: the CLI flow is documentation (runbook, overview).
3. **Screen 3 → the comparison view** reached from a row: two sides + Δ, horizon selector, `pending` with
   `sealed_through` and no partial figure, `withheld` with the page's reason word; addressed by
   `(source, external_id, horizon)` — there is no by-identity group route.
4. **Screen 4 → the incident page's `Preceded by` section** (own-service and probable-root rows with the
   anchored phase and lag, a link to the comparison) and the `🚀 Changes:` note rendered like `⚡ Context:`
   in the incident timeline.
5. **Screen 5 → the per-service timeline view** (`/services/:id/changes`): range ≤ 92 days frozen at Apply,
   kind (set) and source filters, groups with phases, decision, preceded, before/after column at the
   header's horizon, `range_too_wide` refused client-side and rendered identically from the server, cursor
   paging, live-traversal note.
6. **Screen 6 → the token form and read model** in Settings: the optional `actions` allow-list offered from
   the role's grants only, the warning that a narrowed token cannot read the service page, the list shown
   read-only afterwards (immutable, D-0209).
7. **RBAC in the SPA**: every change surface is `project:read` (viewer+); no control writes a change; the
   token form is for org/project admins as today. Concurrency discipline as `ServiceGate.vue`: generation
   counter + `AbortController` per load, abort on unmount and route/workspace change, drop stale responses.

**Why.** The approved mock is the contract; naming the mapping keeps "fidelity to the mock" reviewable
against a list, as D-0207 did for the gate.

**Consequences.** SPA work (iter-0165 task 7) may begin once the API changeset (task 3) exposes the routes;
`make spa-snapshot` after any frontend change; the mock file is committed with the first landed change of
iter-0165; row 19 of the FR-025 discharge map is reached when the marks render from a test.

## D-0211 — FR-025 comparison: `pending` applies to either side whose end exceeds the seal (2026-08-30)

**Decision.** Revision 3 of `func-change-intelligence.md` made `pending` the sealing outcome of the `after`
side only ("for `after` only — `pending` when `T + h > sealed_through`"). Implementation met the case the
letter had not named: a change reported minutes ago has `T > sealed_through`, so its `before` window
`[T − h, T)` is not sealed either; changeset 2 rendered that side `withheld: undecidable` with the page's
stale-watermark reason, and the reviewer refused it as an unrecorded change of a sealed-data response
contract (P1 [33]). The owner decided, the same day: **a side whose end exceeds `sealed_through` is
`pending` with `sealed_through` stated — `after` when `T + h > sealed_through`, `before` when
`T > sealed_through` — never a partial figure, never `undecidable`**, because the facts are not undecidable,
they are not yet sealed. D8, invariant 11, §7 and the mock's screen 3 are amended in place; the spec header
names the correction.

**Why.** `undecidable` is the page's word for a range it cannot decide; a not-yet-sealed range is a
different thing and the operator must be able to tell them apart — one resolves itself when the seal
catches up, the other does not.

**Consequences.** `compareSideTx` has one clamp for both sides; the API's side shape is unchanged
(`{pending: true, sealed_through}`); the test `TestChangeCompareEitherSideIsPendingPastSealedThrough`
asserts both sides pending with the watermark and no `delta`. Recorded before the code changed, as the
reviewer required.

## D-0212 — FR-025 API changeset: a token's list must be granted by its role; visibility stays membership; the metric sets (2026-08-30)

**Decision.** Four readings made while the API of FR-025 was built, recorded before the range is claimed:
1. **`action_not_granted`.** Revision 3 validated a token's `actions` against the action catalogue only
   (`action_unknown`). The owner decided (2026-08-30) that an entry the token's ROLE does not grant is also
   refused at creation — 400 `action_not_granted` naming it — because the intersection in `authz.Can` would
   otherwise let an operator create a token whose listed action silently never works, and the mistake would
   surface at the pipeline's first 403 instead of at the form. The handler asks the central predicate through
   a shadow principal shaped as the middleware builds one; the store keeps validating the catalogue.
2. **Visibility is membership.** `VisibleProject` — the 404-versus-403 predicate every project route
   consults first — was `Can(project:read)`, which after changeset 2 consulted the allow-list, so the very
   CI token D12 describes (`[gate:evaluate, change:record]`) got 404 on its own project, the record route
   included. `VisibleProject` now reads membership alone (D12's own sentence: "its project is still visible
   for 404-vs-403 purposes because visibility is membership, not action"); `Can` and its query-scope mirror
   `VisibleScope` keep intersecting the list.
3. **The rejected-reason set is closed and wider than "the 400/409 codes"**: `body_invalid` for a shape
   refusal that has no code, and the four §5a limiter codes — an uncounted 429 would hide exactly the load
   the limiter sheds.
4. **`cerbix_changes_retained` counts rows**, sampled once per retention pass by the leader through
   `CountServiceChanges`, cleared on leadership loss (a deposed node does not speak), the previous value kept
   when a sample fails.

**Why.** Each is a place where the letter of revision 3 was silent or wrong at the code face; each is
recorded here and amended in D12, invariant 17 and D15 in the same commit, so the contract and the code say
the same thing before the reviewer sees the range.

**Consequences.** `openapi.yaml` lists `action_not_granted` on token creation; the SPA's token form offers
only the role's grants (mock screen 6 already does); the runbook's metric rows use the closed reason set.

## D-0213 — iter-0165 closed: FR-025 / NFR-020 (change intelligence) are DONE (2026-09-01)

**Decision.** The iteration that implemented the approved design (D-0209) closes with FR-025 and NFR-020
`DONE`. Basis: AC-0165-1…7 are DONE with evidence (`docs/status.md`); the FR-025 discharge table in
`docs/traceability.md` reaches all 23 invariants of §6 and the nine groups of the §7 matrix with named tests
and the mutations killed, with no `PARTIAL` row; the store matrix ran in BOTH storage modes under `-race`,
independently, one attended run per DSN (hypertable ok 616.764 s, declarative partitions ok 591.406 s); the
full `-race` suite is green (33 packages); the browser suite is 58 passed / 1 skipped / exit 0 on a live
stack; and the independent reviewer issued a disposition for every commit in the chain.

**The dispositions are recorded as the reviewer worded them, not collapsed.** Four ranges are approved as an
EFFECTIVE SLICE while the original commit is NOT APPROVED AS SUBMITTED, because each shipped with defects
that later commits fixed:

| slice | original, not approved as submitted | approved as | verdict |
| --- | --- | --- | --- |
| API | `7260e68` — findings [8], [11], [14] | + `2c662a4`, `424edcd`, `a1cfe00`, `4f3a046` | [20] |
| CLI | `74ce187` — finding [52] of the previous party | + `25e181a` | [24] |
| SPA | `d2e0f3c` — findings [31], [32] | + `0c8a489`, `41b688c`, `3ad656f`, `caf8e6e`, `7bca415`, `673f707`, `381bfd4` | [47] |
| docs / checker | `35ded9a` — findings [49], [50], [54] | + `056eff5`, `b320290` | [56] |
| live evidence | `f27133d` — incomplete and internally inconsistent | correction slice `7c45db0..201f542` | [72], [77] |

`cee8790` is **SUPERSEDED** by `28bf8cd` and is NOT SUBJECT TO STANDALONE APPROVAL: it is neither approved
nor an open defect ([77]). Approved outright: `c6c74dd` [99 prev], `5cace76` (narrow) [39 prev], `927c8e6`
[52], `fb625ca` [29], `8e41b24` [62 prev], `3b43b70` + `f5654d7` [62], `28bf8cd` [97 prev], `3ad656f` [39],
`f8f8cb4` + `42ae4ab` [74], `9402801` + `1297577` [75].

**The live-run boundary is part of this decision, not a footnote.** `iter-0165-live-evidence.md` records a
run at `f8f8cb4` with image `sha256:0c560a1026a4`. That run has NOT been reproduced. What exists is a
current-head confirmation — `iter-0165-cli-evidence.md`, its own image, twenty-one labelled entries of which
nineteen are CLI invocations carrying exit codes and two are the HTTP records that make the revocation chain
checkable — and both files say so in their own words. Approving the corrected state is not approving the
historical claim, and the close-out does not pretend otherwise ([72]).

**Why it is worth recording how this iteration went.** Of the reviewer's findings after the code was
complete, the large majority were defects in my own FIXES and in the EVIDENCE I produced, not in the product:
a bounded pool that could stall the queue it bounded, that pool fenced by the wrong generation, a commit title
claiming coverage the change did not have, a `@ts-expect-error` in a file the type-check excludes, an
assertion of inclusion under a documentation claim of equality, a checker that could bless a duplicated or
over-long acceptance table, a live-run report typed from scrollback instead of a retained log, a 401 offered
as proof of a revocation, and a document header whose claim about itself was false — twice, the second time
inside the sentence correcting the first. The product moved once in that whole sequence. The lesson entered
in the iteration report is that a fix is a change and earns the same adversarial reading as the code it
fixes, and that a claim about an artefact — including a count — is a claim like any other.

**Consequence.** FR-025 and NFR-020 are `DONE`; `func-change-intelligence.md` is `IMPLEMENTED`; iter-0165 is
CLOSED and immutable from the commit that carries this record. Nothing in the chain has been pushed; that
remains the owner's.

## D-0214 — FR-026 / NFR-021: incident writes are audited, and the design is approved at revision 4 (2026-09-01)

**Context.** D-0171 left "the audit trail for INCIDENT writes" as an open gap, and iter-0156 §2.8 had already
proved the shape of it: FR-022's invariant 14 as written ("every write audited with actor and tenant, in the
mutating transaction") was false of the PRODUCT, not merely unimplemented — an incident write leaves no
`audit_logs` row for either anchor. What FR-022 keeps is the absence of asymmetry between the two anchors. An
operator can resolve someone else's incident, acknowledge a page or publish a postmortem, and the
organization's audit trail says nothing. The gap was the only named item left in the backlog after iter-0165
closed.

**The owner's three sizing answers (2026-09-01).** Only PRINCIPAL writes are audited — a machine's work is
recorded in the incident's own timeline, and a flapping service would bury the log, which is the same reason
D10 of `func-reliability-gate.md` keeps gate DECISIONS out of `audit_logs`. The row is written INSIDE the
mutating transaction, the `insertGateAudit` pattern of FR-020/FR-024, not the best-effort `h.audit` helper —
that is what makes "audited" a property of the recorded fact and what makes invariant 14's wording true. And
NO read is added: the rows appear in the organization's existing audit listing; an incident-scoped panel and a
project-scoped read were both considered and declined, and stay named as follow-ups.

**The review, and what it changed.** Three rounds against the working tree, each verified against the code
rather than accepted. Revision 1 was rejected with 1 P0, 5 P1 and 2 P2. The P0 was mine and it mattered: `nil`
as "the machine actor" meant a handler that forgot the argument produced exactly what the requirement
forbids — an unaudited principal write — and revision 1's own test matrix asserted that behaviour. The actor
now travels in the DOOR (`…ByPrincipal` requires it, `…BySystem` has no actor parameter), so a forgotten actor
is a compile error. Round 1 also found that the postmortem writer owns no transaction (`UpsertPostmortem` is
one pooled statement that cannot tell a create from a replace), that a repeated acknowledgement is not a no-op
today because `updated_at` is written unconditionally, that the machine list omitted the `🕸 Impact:` note,
and that a concurrent Alertmanager firing answers 500 where the sequential retry answers "ignored". Round 2
found that the regression for the acknowledgement no-op was pinned to a surface that does not read
`updated_at` at all, that the machine list still was not enumerated by writer (`⏸ Suppressed:` alone has two
independent writers, in `dependencies.go` and `alertdelegation.go`, and neither is in `incidentcontext.go`),
that "the loser is silent" is not an observable contract for a parallel resolve, and that a system door for a
writer with no machine caller would widen the unaudited surface for nothing. Round 3 found that the
`internal/api` AST guard proves only what is REACHED: a `…BySystem` method declared for a writer nobody calls
passes it untouched, so a second guard now pins what EXISTS over the declarations of `internal/store`.

**Decision.** The design of `func-incident-audit.md` is APPROVED at revision 4 (owner, 2026-09-01). FR-026 and
NFR-021 enter `docs/status.md` as `TODO`, per the rule iter-0164 followed for FR-025: a requirement gets its
row when its design is approved, and revisions 1–4 are committed ONCE as the accepted design phase (D-0203).
The scope question the owner settled explicitly: **D8b stays inside FR-026** — the concurrent receiver retry
is a normal part of the real Alertmanager path, and without it "one mutation → one audit row" is a false
claim rather than an invariant. The same reasoning carries D8a. Both are user-visible behaviour changes and
are named as the only two exceptions to NFR-021's "nothing else moves".

**Consequence.** The spec is the contract for an implementation iteration that is NOT yet opened; nothing in
the product changes until it is. Two guards, each fixture-tested, hold the door surface — one over
`internal/api` for what is reached, one over `internal/store` declarations for what exists — and a door built
ahead of its caller fails the second even while the first passes. The nine machine writers are enumerated by
name in D1, and a writer that grows a sibling without a §7 case is a gap that table exists to make visible.
The two follow-ups D9 declined — an incident-scoped audit panel and a project-scoped audit read — remain open
and unclaimed.

## D-0215 — PromQL takes optional basic auth, and the tri-state gains a default (2026-09-01)

**Context.** The D-0145 addendum of the same day admitted `promql` to the file provider and stated
that an authenticated Prometheus stays unsupported: the operator was to place a regional agent where
Prometheus lives and give the prober an unauthenticated path. The owner reversed that within the day.
The reversal is recorded rather than edited into the earlier text, because the rejected position had a
real argument — YAGNI, and one fewer credential surface — and a future reader deserves to see both.

**Decision.** `promql` carries OPTIONAL basic auth. Basic only: bearer tokens are out, deliberately,
because basic is what Prometheus implements natively (`--web.config.file`) and half-supporting a
second scheme is worse than naming it unsupported. No TLS fields either: the target is a full URL, so
the scheme decides, and the prober has no skip-verify to offer — declaring the keys would claim a
capability that does not exist.

**Why the schema needed a new mechanism.** The credential requirement of `func-secret-inventory.md`
§4.7 is a TRI-STATE resolved from the effective schema — required, forbidden, invalid — and the
executor's gate acts on it. "Optional" is not one of the three, and it must not become one: the whole
point is that the gate never guesses. So the shape is rabbitmq's, a discriminator with variants —
`auth_mode: none` (forbidden) and `auth_mode: basic` (required) — with ONE addition: the
discriminator may declare a DEFAULT for the case where the key is absent. Without that default every
promql monitor written before today would resolve to "invalid" at the executor gate and stop probing,
and materializing `auth_mode: none` into their config would rewrite the canonical hash of each one.
The default is resolved and never written, exactly as `tls_skip_verify` is.

**The key is `auth_mode`, not `auth`.** The file provider refuses a settings key literally named
`auth` as credential-bearing — `auth: Bearer …` is the shape that guard exists to catch. Renaming the
discriminator was the correct side to give way: a discriminator can be called anything, while
weakening the guard for every type to fit one schema's spelling would trade a security property for a
spelling preference.

**What came for free, and what did not.** The dispatch gate, the envelope's expected-field set and the
`monitor_secret_refs` normalization are all schema-driven, so they needed no per-type change — adding
the schema wired the path from bundle to executor. What needed writing: the prober's `SetBasicAuth`
(mirroring the RabbitMQ management probe, and NOT sent at all when no username is configured, so an
unauthenticated Prometheus is never handed an empty header), the SPA's optional credential block, and
a cross-check refusing a blank-after-trim `query` — `required` rejects `""` but accepts `"   "`, and a
whitespace-only expression is a monitor that is configured, scheduled and permanently meaningless.

**Consequence.** `func-monitoring-as-code.md` §3.1's "an authenticated Prometheus is NOT supported on
any surface" is SUPERSEDED and rewritten. A monitor with no `auth_mode` behaves exactly as before, at
validation and at dispatch. The unsupported case is now narrower and still named: a Prometheus behind
a scheme other than basic — a bearer-token proxy, mTLS — for which the agent-plus-unauthenticated-path
answer of the D-0145 addendum still stands, with its own precise sentence about the agent not
authenticating.

## D-0216 — a credential inside a synthetic scenario, and the blast radius of protecting it (2026-09-02)

**Context.** An operator asked whether a synthetic monitor could carry a bearer token. It can — and the
token is then stored in cleartext in `monitors.config`, returned to every principal who may READ the
monitor, skipped by key rotation, and echoed into `heartbeat.msg` with the whole request URL whenever a
step's transport fails. `func-oncall-synthetic-pull.md` FR-SYN-1 already promised "encryption like the
other types" and its §217 promised inclusion in `reencrypt`; D-0090 promised that the message "never
echoes bodies/headers". So this is a spec-versus-code defect of the same class as FR-022's invariant 14,
not a new feature — and the SYN requirements have no rows in `docs/status.md`, which is how a false
promise survived unnoticed.

**Three stages, decided separately (spec `sec-synthetic-secrets.md`, FR-028 / NFR-023).** Stage 0: no
probe result carries a request URL. Stage 1: the scenario is encrypted at rest, covered by rotation and
a backfill, and withheld from a principal who cannot write the monitor. Stage 2: a secret inside a
scenario becomes a NAMED BINDING resolved from the project inventory, never a literal — which also
removes the reason a synthetic monitor cannot be declared in a bundle, without delivering that.

**What the review changed.** Two blockers, both verified before they moved the text: a scoped
`setting_key` would break every inventory RENAME in a project, because `repointSecretRefs` looks the key
up in the flat config and fails the whole rename when it is absent (deliberately); and the
test-before-save path sends the literal scenario to a worker in an ordinary generation-1 job, which
contradicted a non-goal the spec had written. The review also refused a design that redacted
"secret-looking" values — secrets sit in bodies, URLs and assert values too, so a header-shaped guess
is bypassable — and it demanded a canonical binding descriptor before the anti-relocation invariant
could be tested at all.

**The owner's ruling, which withdrew a decision the reviewer and I had both agreed on.** Revision 3
made instance READINESS depend on a configured at-rest key and a completed migration. The owner
rejected the premise: a service must not go down because of an unset environment variable or one
misconfigured monitor. For a monitoring platform that is not taste — an instance that stops reporting
ready removes visibility into everything else at the moment it is needed most. The product already
holds that line and neither of us looked: an executor that cannot open a job's credential answers with
a per-job typed diagnostic and flips readiness only when the failure PERSISTS
(`internal/worker/worker.go`). So readiness is never coupled to the key or to the backfill; a scenario
that cannot be protected is refused at WRITE time; one that cannot be opened is a per-monitor probe
error with no heartbeat, status or SLA mutation; and the backfill is an ordinary idempotent task.

**Consequence.** The cost is stated rather than hidden: on an instance with no key, legacy plaintext
scenarios stay plaintext until an operator supplies one. That is preferable to trading a leak for an
outage. Stage 0 ships with this record; stages 1 and 2 are specified and unbuilt. The URL rule is
type-wide rather than synthetic-only, because the same defect was reproduced on an ordinary `http`
monitor and on `promql`: `net/http` embeds the request URL in every error, so `Msg: err.Error()`
published whatever the target's query carried.

### Addendum — stage 2 as built, and the two things review caught in it (2026-09-02)

Stage 2 shipped: a credential inside a scenario is a NAMED BINDING, declared as
`{{secret:<binding>}}` in the document with the inventory secret's name in a FLAT config key,
`scenario_secret_<binding>_ref`. The flat key is the decision. The design the reviewer approved put the
reference in a scoped `setting_key` and, because `repointSecretRefs` looks a ref up in the flat config,
that shape made every inventory rename in a project fail as soon as one synthetic monitor used it —
BLOCKER 1 of round 2, which demanded a scenario-aware repoint inside the rename transaction. Under the
flat key there is nothing to repoint specially: rename, delete counting and rotation run on the path
`password_ref` already runs on, and `TestScenarioBindingRidesTheOrdinaryRefPath` proves it on a live
database instead of asserting it. D6a is withdrawn, and the code it asked for does not exist.

**Two beliefs of mine that tests disproved, both worth recording because the reasoning sounded right.**
I argued the execution digest already covered the scenario, so a relocated placeholder could not
survive; it covers the keys of a credential SCHEMA, and synthetic has none, so an attacker holding a
valid envelope could rewrite a step's URL and have the credential delivered wherever they chose. The
scenario and its ref keys became EXECUTION BINDING KEYS, which puts the stored bytes under
`EnvelopeV2`'s body digest — the property D8a's canonical descriptor was for, without the descriptor.
That exposed the second: only V2 binds the body, so a binding REQUIRES a body-bound envelope, and on an
older carrier the materializer answers `carrier_too_old` per monitor rather than issuing a job that
looks protected and is not.

**D7 is narrowed to what it can enforce, and FR-028 no longer claims more.** A literal secret is not
detectable by shape, so the rule is the header-NAME rule: a header in the finite secret-capable set must
be exactly one placeholder. A credential in an unlisted header or in a body stays legal — encrypted at
rest by stage 1, withheld from viewers, kept out of probe results by stage 0 — and the spec, the status
row and this record all say so. The stronger property needs a restrictive typed request model for the
scenario, which is an owner's decision and separate work; a heuristic over VALUES is not on the table.

**D10 flipped, and the spec follows the code.** The approved text promised that an unsaved synthetic
test would run through the same authoritative materialization the saved path uses. As built it is
refused instead — "save the monitor before testing it", naming the binding — because that endpoint
builds an unsaved monitor with no id, no execution revision and no stored refs, so honouring the
promise meant a second resolution path on the one surface whose job is to fail closed.

**D6b, the P0 review found in the SHIPPED code (round 5).** Every helper matched the
`scenario_secret_` prefix and none asked the monitor TYPE, so the key was accepted on an http monitor —
where it wrote a `monitor_secret_refs` row, added an expected envelope field, joined the execution
digest and raised the carrier generation the dispatch gate demanded. No credential escaped: every step
fails closed and the value never left the core. What broke is availability, and it broke at DISPATCH for
a config the write surface had accepted — the shape of failure the owner's blast-radius ruling exists to
prevent. The type is now decided once, at the write boundary, and gated in three more places so no
single fix is load-bearing. The dispatch gate refuses rather than ignores such a job; that is safe to
add only because stage 2 has never been released.

**What is still not done, stated in the status row rather than implied by silence:** the SPA cannot
declare a binding, so the feature is reachable through the API and not through the editor, and a
synthetic monitor still cannot live in a Monitoring-as-Code bundle (D9). And the release that first
carries stage 2 owes two breaking-change notes: URL userinfo is refused on every write surface, and a
synthetic monitor whose `authorization`-class header holds a literal now fails validation on its next
write (it keeps probing; nothing re-validates on read).

## D-0217 — FR-028 closed: a credential inside a synthetic scenario is a secret everywhere it can be one (2026-09-02)

**Decision.** FR-028 and NFR-023 are DONE. The independent reviewer approved stage 2 at party [30] for
`b3c99b6` + `084b49d` + `258adb5`, subject to the documentation diff committed as `900aa1b`, after five
rounds and with its own verification of the domain, dispatch, api and store packages and of
`make docs-check`. iter-0166 carries the per-range dispositions; D-0216 and its addendum carry the design
and the three beliefs of mine that tests disproved.

**What closes, in one line each.** No probe result carries a request URL, for any monitor type. A
scenario is ciphertext at rest, covered by rotation and by an idempotent non-fatal backfill, and
withheld from a principal who cannot write the monitor. A declared credential is a named binding
resolved from the project inventory, validated by one rule on all four write surfaces, delivered in a
body-bound envelope and substituted into the scenario with no copy beside it. And the binding belongs to
a synthetic monitor and to nothing else, with the pre-rule row's lifecycle pinned by seeded tests rather
than assumed away.

**What does NOT close, and is not counted as closed anywhere.** The SPA cannot declare a binding, so the
feature is reachable through the API and Monitoring-as-Code writes and not through the editor; per the
process that needs an owner-approved mock before any SPA code exists. And a synthetic monitor still
cannot live in a bundle (D9): stage 2 removed the secret-reference reason for the exclusion and not the
nested-schema one. Both are named in the requirement rows. The reviewer's disposition calls them deferred
product scope; the owner's call is what moves them.

> **Update, 2026-09-03:** the owner approved the mock and the editor landed in iter-0167, so the first of
> those two is closed — the mapping panel, the credential-bearing header as a binding picker, the D10
> notice at the Test button, and a defect the work found: `canSubmit` demanded a target the form hides for
> `synthetic`, so that type could not be created from the SPA at all. D9 stands.

**The honest limit of the guarantee, stated here because a closure record is where people look for it.**
FR-028 does not make it impossible to put a credential in a scenario. A literal is detectable only by
KEY NAME — a header in the finite secret-capable set must hold exactly one placeholder — so a credential
pasted into an ordinary header or into a body is legal, on every surface including test-before-save. It
is encrypted at rest, withheld from viewers and kept out of probe results, and it travels to the executor
inside the ordinary job rather than in an envelope. Buying the stronger property means a restrictive
typed request model for the scenario; that is an owner's decision and separate work, and a heuristic over
VALUES is refused rather than deferred.

**One release obligation each for the next tag,** already carried in `docs/roadmap.md`: a monitor target
with credentials in its URL userinfo is refused on every write surface (D-0145 addendum), and a synthetic
monitor whose `authorization`-class header holds a literal fails validation on its next write. Both keep
probing; nothing re-validates on read.

**Two follow-up rulings from the reviewer after approval, recorded so they are not re-litigated
(2026-09-02).** First: the dispatch-side refusal of a `scenario_secret_*` key on a non-synthetic type is
PERMANENT. I had justified it by "no release carries the key", an argument that expires at the first tag
that ships stage 2 and would have left the rule standing on a dead reason; the ruling puts it on carrier
INTEGRITY instead — a job whose config the core would never have stored is not a job to execute, now or
later. The code comment says that, and no longer says the other thing. Second, for the SPA editor when
the owner calls for it: the mock must show the binding → inventory mapping EXPLICITLY, carry D7's rule
inline where the operator types a header rather than as a refusal after saving, and state the
"save before test" contract in the editor itself. Those three are the mock's acceptance criteria, and
they are in `docs/roadmap.md` beside the deferral so the mock is not designed from scratch.

**A defect this arc found and did not fix, with an owner-visible pointer instead of silence:**
`TestGateMaintCrashAfterEveryRemovalStatementConverges/after_drop.commit` (FR-024) fails under machine
load, reproducing at `b3c99b6` in a clean worktree, so it predates this work. It is in the roadmap's
standing work with a starting point.

## D-0218 — a typed external canary, and the two architecture rulings that keep it from being a rewrite (2026-09-03)

**Context.** The owner asked for an external canary that can run a controlled asynchronous API
journey — submit, correlate, await a terminal result, assert — with an async media-upload API as the first use case:
a multipart upload answered by `202` and a `task_id`, an SSE `task.completed`, and assertions on
`s3_path`, `byte_size` and `media_type`. The brief was explicit about what it must NOT become: not a
browser runner, not a queue consumer, not a webhook receiver, not shell execution, not a plugin host,
and above all not `synthetic` widened into a workflow DSL.

**Decision.** A new monitor type `async_canary` with exactly one workflow kind,
`async_transaction_v1`, specified in [`func-async-canary.md`](specs/func-async-canary.md) as FR-029 /
NFR-024. Nested typed schema with closed unions, no `settings` map, no JSON string, no expression
language, no user-supplied code. Monitoring-as-Code first; the UI comes last and only as a typed form.

**Why not extend `synthetic`, in one sentence that this project paid for two weeks ago.** `synthetic`
is an untyped document, so every rule about it has to GUESS — and FR-028's D7 is the receipt: unable
to detect a credential by value, it fell back to a rule about header NAMES, which then refused the
ordinary canary login flow (extract a token, send it as `Authorization: Bearer {{token}}`) because it
cannot tell a runtime value from a pasted secret. A schema does not guess: a field either takes a
`secret_ref` or it does not. The same root gives three more properties for free — a bundle can carry
the type at all (FR-028 D9's blocker was the flat settings map), the semantic hash becomes a statement
about FIELDS rather than bytes, and every bound is a typed maximum checked at the write boundary
instead of re-derived at execution time.

**Ruling 1 — a capability, not a fifth role.** The brief asked for `cerbix serve --role canary`. It is
refused: `worker` (AMQP) and `agent` (pull) are already DB-less executors with region registration,
token auth, readiness and lifecycle, and a fifth role would duplicate all four to gain nothing. The
capability negotiation this needs is not new either — the scheduler already asks a region what it can
open and lowers the carrier generation when the answer is no; that generalizes from one number to a
SET of `workflow_kind@version`, and `no_capable_runner` falls out of it rather than being invented.
Running the canary on its own host stays a DEPLOYMENT decision (another `agent`, own flag, own egress),
which is where isolation belongs.

**Ruling 2 — one probe stays one heartbeat; the transaction does not outlive its job.** Splitting
submit from await was considered and rejected, and the reason is the product's spine: the executor
answers the job it was given, and the heartbeat it returns is the whole measurement — status,
confirmations, SLI and alerting all rest on that. A split forces an in-flight transaction into CORE
storage, which means storing exactly the values NFR-024 forbids storing (the correlation id, the result
path); it glues latency from two jobs with two clocks; and it turns one failure mode into four. The
price of the chosen shape is a delivery held open for the duration, and the specification pays it in
four named places rather than discovering it later: a per-TYPE timeout ceiling (900 s) so the global
300 s bound is untouched for everything else, a per-JOB pull lease instead of today's 60-second
constant (a 10-minute await under a 60-second lease is a re-claim, which is a duplicate external
submit), a separate AMQP queue per workflow kind because prefetch is 16 and sixteen canaries would
starve a region's ordinary checks, and stage-level bounds inside the one probe so a hung submit fails
as `submit`.

**The boundary where ruling 2 stops being right, named now rather than discovered.** It holds while
the completion bound is minutes. A journey measured in hours cannot hold a delivery, and the answer
then is a different feature — asynchronous result ingest — not a larger number. 900 seconds is the
type's hard ceiling for exactly that reason.

**What is honestly half-guaranteed, stated because FR-028 taught the cost of implying more.** The
executor sends a stable `Idempotency-Key` derived from the monitor and the SCHEDULED RUN — not the job
id, so a redelivery, a re-claim after a lease expiry and a transport retry all carry the same key.
Whether a second submit with that key creates a second external task is the TARGET's contract, not
ours. If the target ignores the header, retries duplicate, and no design here prevents it.

**Order.** The owner put this ahead of R2 (FR-026 incident audit, designed and unbuilt) and R3 (the
dependency sweep, four frontend majors) on 2026-09-03, against the recommendation to take R3 first.
Recorded in `docs/roadmap.md` with both the cost and the reason, so a later reader sees a decision
rather than a drift.

**The owner's three answers, same day (revision 2).** The ceiling went DOWN rather than up, and far
enough to delete the mechanism: `async_canary` inherits the product's existing `maxTimeoutSeconds = 300`
and there is no per-type bound at all. The smaller number is not tidiness — the in-flight deadline is
derived from the timeout, so 900 s parked a crashed runner's monitor for a quarter of an hour inside a
monitoring product; `interval >= timeout` made it four samples an hour where one failure is 25 % of the
hour; and two confirmations pushed the page out to half an hour. What it costs is stated: a journey
whose declared promise exceeds five minutes cannot be expressed in v1, and if the first use case can
legitimately exceed it under load, phase C's first measurement — the journey's p99 — is what moves the
number, with evidence rather than argument. The reaping policy is the OPERATOR's problem and not a
schema field, which is also the honest call because cerbix holds no rights on the object store and a
mandatory declaration would be an unverifiable claim. Private-address targets stay refused in v1 with
no override.

**The owner's sign-off on the one deviation from the brief (2026-09-03).** The brief wrote
`secret_ref: <project-secret-name>` at each position; the design keeps the field NAME and makes its
value a BINDING declared once under `workflow.secrets`. The owner chose the binding map explicitly
after the alternative was offered twice, and the independent reviewer recommended the same on
technical grounds: one reference per secret, the existing rename, delete-counting and rotation paths
reused unchanged, and the positions covered by the execution digest without nested storage. The
literal form would have needed either a nested-aware repoint or a synthesized binding name — the two
options FR-028 already paid to compare. Recorded here so the deviation reads as a decision with a name
on it rather than as an implementer's preference.

**Status.** Design revision 8, **APPROVED** at party [66] (2026-09-03) — phase A closed: in review with the independent reviewer, who has returned
eight P0s over two passes — three on revision 1 (an undecidable body-secret claim, a URL policy that
contradicted its own E2E, and a target-controlled correlation id reaching a URL unescaped) and five on
revision 3 (no accepted-status contract for submit, bounds written as adjectives instead of numbers, a
nested secret reference with no rename/rotation/digest story, completion headers left undefined, and a
header name pushed through a JSON-path grammar), and two on revision 4 — a claim that the flat ref
key would let `repointSecretRefs` work unchanged, which was false because that function decodes
`monitors.config` into a `map[string]string` and a nested object would have broken the rename for
every monitor in the project; and a per-region concurrency limit with no enforcement point; two on revision 5, both contradictions
introduced by the previous round's own fixes (a persisted `secrets` map that would go stale against
the flat ref key a rename updates, and a three-strike saturation streak with no counter, storage or
freshness semantics); and two on revision 6, both things I asserted without checking (a hash rule that
contradicted itself on rename, and an SLI exclusion that reopened a decision the owner had made, on an
analogy that was factually wrong about what "ungoverned" means in FR-021); and three on revision 7,
two of which were one recurring mistake — changing a decision and leaving the test matrix demanding the
old behaviour — plus a real hole where the cross-host credential rule covered completion and not
submit, the request that actually carries the credential. All seventeen are answered in the spec. FR-029 and NFR-024
are now `TODO` rows in `docs/status.md`; no code exists and `async_canary` is not a monitor type in
the tree. Phase B is the next unit of work and carries its own gate.

## D-0219 — an incident write says who wrote it: FR-026 / NFR-021 implemented, and the two design claims the code found wrong (2026-09-03)

**Context.** FR-022 promised, as invariant 14, that "every write is audited with actor and tenant, in the
mutating transaction". Building its discharge map found the promise false of the PRODUCT rather than
merely unimplemented: an incident write left no `audit_logs` row for either anchor. D-0171 named the gap,
iter-0156 §2.8 proved it, and D-0214 approved a design for it at revision 4. This is that design built,
as the second leg of the v0.1.9 arc the owner ordered on 2026-09-03 as canary → R2 → R3, with the
independent review deferred to one request at the end.

**Decision.** FR-026 and NFR-021 are DONE at iter-0168. Every incident mutation made by a PRINCIPAL
writes exactly one `audit_logs` row in the same transaction, through a door whose signature requires the
actor; the nine enumerated MACHINE writers keep their own doors and write nothing, because their record
is the incident's own timeline and auditing them would bury a tenant's log under a flapping service. The
vocabulary is five typed constants; the target carries ids and both ends of a transition and never a
body. No migration, no route, no read.

**Two claims in the approved design that the implementation found wrong, recorded rather than quietly
corrected.**

1. *The guard exemption.* Invariant 4's `internal/api` guard was first built with
   `handlers_alertmanager.go` exempted BY NAME, on the reasoning that the receiver is "a machine writer
   that happens to arrive over HTTP". D1 says otherwise, and so does the product: Alertmanager posts with
   a project-write token, a token is a principal, and the receiver's create and resolve are audited. The
   exemption hid the very defect the guard exists to catch — the receiver was wired to the system doors
   and audited nothing. There is no exemption now, and both guards are additionally driven by fixtures
   that contain the violation. The general lesson is the one this repository keeps relearning: an
   exemption written to make a guard pass is a guard that has stopped guarding.

2. *What D2a's row lock can be observed to do.* The design asked for the incident's row lock and the
   matrix asked for "a competing timeline write that must serialize behind it". That assertion cannot
   distinguish the lock, because `postmortems.incident_id` is a foreign key and the insert takes a KEY
   SHARE lock on the same row either way. What the explicit `FOR UPDATE` buys is the ORDER — the tenant
   is resolved before the write, so an unknown incident is `ErrNotFound` rather than a foreign-key error
   and the `created`/`updated` decision is taken with the row held — and that is what the test now says
   and the mutation now catches. The waiting assertion is kept and labelled with what it cannot see.

**Consequences.** FR-022's invariant 14 now names the requirement that closed it and its state. No
requirement in the tree is now specified and unbuilt.
`make docs-check` gained an FR-026 gate that compares §6 as a SET against the discharge map in both
directions, verified by removing a row and watching it fail. Two user-visible behaviours changed, both
making an existing claim true rather than adding behaviour: a repeated acknowledgement is a genuine
no-op and no longer moves the incident's modification time (D8a), and a duplicate alert delivery that
arrives concurrently is ignored with HTTP 200 and `ignored=1` instead of answering 500 (D8b). Nine
mutations were killed; the two that survived are written down with the reason, in the iteration report
and in the tests themselves.

## D-0220 — the canary is built for five of its six phases, and what is missing is written down (2026-09-03)

**Context.** D-0218 approved the design at revision 8. The owner then ordered the whole of v0.1.9 as
canary → R2 → R3, all phases without stopping, reviewed once at the end. This records what phases B–E
actually produced and — more importantly — what they did not, because a requirement that ships partly
built and reads as finished is the failure mode this repository has spent several iterations correcting.

**Decision.** FR-029 and NFR-024 are `IN_PROGRESS`, not `DONE`. Phases B–E are built and their §6
invariants are discharged, with two rows `PARTIAL` and naming the gap. Three things are carried
forward, each stated in the rows, the spec's lifecycle banner, the discharge map and the runbook:

1. **Capability announcement, its dispatch filter and `no_capable_runner`.** Nothing filters a canary
   job away from an executor binary older than this release; that binary answers `unsupported monitor
   type "async_canary"` — an ordinary DOWN, per monitor, self-healing on upgrade. The operational rule
   is therefore: upgrade a region's executors BEFORE declaring a canary there. That belongs in the
   runbook and is in it, rather than being discovered by whoever declares the first canary.
2. **Phase F, the typed UI form.** The mock exists (`docs/design/mock-async-canary.html`) and waits on
   the owner's visual approval, which is the standing gate for every SPA surface here (D-0207). The
   mock's own contract is that there is no JSON editor on any screen, including the read view — a
   textual view of a canonical document is a view somebody edits as text.
3. **The per-workflow-kind AMQP queue.** The kind rides the existing per-region queue.

**Two things worth keeping beyond this feature.**

- **A defect older than the canary was found by building it**: the agent's pull-claim lease was
  hardcoded at 30 seconds, so ANY pull monitor with a timeout past 30 s was re-claimable mid-probe.
  Migration 00096 makes the lease per-job. The canary did not cause it; the canary is what made it
  visible.
- **The Go suite could not see a database constraint.** Every Go test passed while the real `monitors`
  table refused `async_canary`, because no store test writes to that table — a live E2E found it.
  Migration 00097 fixes the constraint and `TestEveryValidMonitorTypeIsAcceptedByTheDatabase` now
  asserts the type vocabulary against the database itself, so the next new type cannot repeat it. The
  general shape — a test suite that agrees with itself about a fact only the database owns — is worth
  remembering.

## D-0221 — the dependency sweep, and two majors that turned out to be one decision (2026-09-03)

**Context.** Eight open dependabot branches, all newer than the last sweep (iter-0159, 2026-08-19),
including four frontend MAJORS. The owner ordered this third, behind the canary and the incident audit,
against my recommendation to take it first — recorded in the roadmap that way so the cost would be
visible if it bit.

**Decision.** Seven of the eight are taken: the go-modules group (`amqp091-go` 1.14.0, `grpc` 1.83.1),
the frontend group (`@types/node`, `eslint`, `vue-tsc`), `docker/setup-buildx-action` 4.3.0, the
`golang` build image 1.27.0-bookworm, and the majors `jsdom` 30, `vite` 8 and `vue-router` 5.

**Two things worth keeping.**

1. **iter-0159's rule — "each major alone with its own verification" — has an exception the rule could
   not have anticipated.** `vue-router@5` declares `peerOptional vite@"^7.3.0 || ^8.0.0"`, so on Vite 6
   the install fails outright with `ERESOLVE`. The two majors are ONE decision. What was done, and what
   is written down, is: Vite 8 alone first (verified), then vue-router 5 on top of it (verified again).
   The alternative — presenting two independent verifications that never happened — is the shape of
   claim this repository has spent iterations learning to refuse.
2. **TypeScript 7 is DECLINED, with a reason, and its branch stays open.** `vue-tsc` 3.3.11 cannot
   drive the native compiler port: `npm run type-check` dies inside `vue-tsc/index.js` with
   `ERR_PACKAGE_PATH_NOT_EXPORTED` before type-checking a single file. There is nothing here to fix and
   no workaround short of dropping the type-check from the build, which would trade a real gate for a
   version number. Closing the dependabot branch would lose the reason, so it stays open.

**On the order.** The predicted cost of taking this last was that four majors would land on a larger
surface after a feature that touches the frontend. It did not bite: 38 files / 418 vitest tests stayed
green through every bump, and the one failure had nothing to do with the order. The prediction is kept
in the roadmap beside the outcome, because a wrong prediction is worth as much to a later reader as a
right one.

**On the test database, which cost about an hour.** The full `-race` suite failed 43 tests with
`TruncateAll` deadlocks and organization-slug conflicts, and none of it was the code: another agent was
running its own verification against the SAME `cerbix_test` database, visible directly in
`pg_stat_activity`. The fix is per-run isolation — a private database named so that `TruncateAll`'s
"name must contain test" guard accepts it — not coordination. Worth remembering the next time a suite
fails in a way that looks systemic and reproduces nowhere.

## D-0222 — the redirect policy's provenance is a context value, not a wire header (2026-09-03)

**Context.** A reviewer P0 on the v0.1.9 canary range. `internal/prober/canary.go` needs to know which
of a request's headers a BINDING produced, so the cross-host redirect rule can drop exactly those —
`net/http` strips only `Authorization` and has never heard of `x-api-key`. The first implementation
carried that set in an `X-Cerbix-Binding-Backed` request header and removed it after `client.Do`
returned. `client.Do` is when the request is SENT, so the removal was always too late: the initial
target and every redirect hop received an undeclared internal header enumerating which headers hold
credentials.

**Decision.** Provenance travels on the request CONTEXT. `CheckRedirect` reads it from `req.Context()`,
which is available on every hop and serialized onto no socket. No wire header is set, and none needs
deleting.

**Why the tests missed it.** Phase C's redirect tests asserted what the target must not KEEP and what it
must keep. None asserted what the target must never RECEIVE. That is a distinct question, and the new
test asks it on both hops — by `x-cerbix-` PREFIX rather than by the one header name, so the next
internal marker cannot reach the wire past this guard either. The general lesson: for anything a
request carries, "is it stripped correctly?" and "is it sent at all?" are two assertions, and passing
the first says nothing about the second.

**Also corrected: the review range itself.** The single review request named `bd92df7..dc48509`, which
begins AFTER the canary implementation commits (`f04ea0d` phase B, `24b3230` C, `c634406` D, `02c05a1`
and `bd92df7` E). It therefore asked for approval of "the whole arc" over a range containing almost
none of it — the reviewer refused it as a P0, correctly. The effective range for the implementation
review is everything after the APPROVED design commit: `8626125..HEAD`.

**And the release was cut before this gate closed.** v0.1.9 was tagged, pushed and published while the
review request sat unanswered; the owner deleted the release by hand. The review is a GATE, not a
notification, and a green local gate set is not a substitute for it — this P0 is what the gate was for.
The owner then ruled on the tag as well (2026-09-03): **v0.1.9 is dropped entirely, local and remote,
and recreated only after the fixes and the review.** It is dropped rather than moved because a tag that
points at one tree and then silently at another is worse than no tag; nothing consumed it (every asset
had zero downloads before the release was removed).

## D-0223 — the canary in-flight slot is keyed by the RUN, and a dispatch that never delivered gives it back (2026-09-03)

**Context.** Two P0s from the independent reviewer's full pass over the canary range, both against
FR-029 invariant 8 — "the scheduler never has two in-flight executions of one monitor" — which the
implementation did not actually hold.

**P0-2 — a claim without a delivery leaked the slot.** The scheduler claimed `canary_inflight` BEFORE
the AMQP publish, the pull enqueue, the payload marshal and the credential-path publish, and none of
those failure branches released it. Four transient faults therefore consumed a region's whole cap until
`timeout + 60s`, and every canary in that region then reported `region_saturated` — a DOWN naming a
cause that had nothing to do with what happened. A broker blip became a fleet of false outages.

Every branch between the claim and a successful delivery now releases. A cancelled context is the one
case that cannot, because no statement will run as the process goes away; the lease TTL is the backstop
there, which is what a TTL is for.

**P0-3 — a stale result released a newer run's slot.** The row persisted `run_key`, but
`RecordScheduledResult` deleted by `monitor_id` alone and the heartbeat carried no run identity at all.
So: run 1 claims, its lease lapses, run 2 claims — and run 1's late result deletes run 2's row, letting
run 3 start beside run 2. That is precisely the concurrency the lease exists to forbid, reached through
the door the lease did not cover.

The scheduled run now travels the whole way — job → result → heartbeat (`Heartbeat.CanaryRunKey`,
stamped in `prober.Runner.Run` beside the execution revision, because both answer the same question) —
and both the release and `RecordScheduledResult` delete `WHERE monitor_id AND run_key`.

**A third defect the fix uncovered, named rather than folded in silently.** The run key was written
inline in the credential materializer, which the PLAIN dispatch path never reaches: a canary with no
bindings claimed its slot with an EMPTY run key, and a slot keyed by nothing can never be released by
key — it would have parked its monitor for the full TTL on every run. The formula now has one owner,
`domain.CanaryRunKeyAt`, both paths stamp through it (onto a COPY of the config, because the snapshot's
map is shared with every reader of that tick), and `ClaimCanaryInflight` refuses an empty key loudly
instead of accepting an unreleasable row.

**Consequences.** Four mutations killed: dropping the release after a failed publish, keying the
release by monitor alone, accepting an empty run key, and dropping the plain path's stamp. The
`Heartbeat` gains one optional field, so an executor older than this release simply returns no run key,
releases nothing, and its slot returns at the TTL — degradation, not breakage, and it matches the
upgrade order the runbook already states.

## D-0224 — the phase-F mock is approved, and a real project name is scrubbed from every example (2026-09-03)

**Decision.** The owner approved `docs/design/mock-async-canary.html` visually on 2026-09-03, which
lifts the standing gate on FR-029 phase F: the typed UI form may now be built to it. The mock's
contract stands as drawn — there is no JSON editor on any screen, including the read view, because a
textual view of a canonical document is a view somebody edits as text.

**The correction that came with the approval.** The owner found a REAL project name used as example
data in the mock. It had spread far past the mock: 37 occurrences across `func-async-canary.md`,
`docs/decisions.md`, the mock itself, and THREE `_test.go` fixture files — because the original brief
named the real first use case and that name was carried straight into every example rather than
replaced with a placeholder at the moment the first example was written.

Everything is now neutral (`Media upload journey`, `upload-token`, `media-upload`,
`bundles/canaries.yaml`), and the sweep was tree-wide rather than mock-only, since the owner's
instruction was to clean it everywhere and most of it was not in the mock.

**And it is kept clean by a gate, not by care.** `scripts/check-docs-references.py` gained
`check_customer_names()`, which walks `docs`, `internal`, `e2e`, `cmd` and `scripts` — the whole tree,
not the living-document list, precisely because the living-document list would have missed the fixture
files that held most of the leak. Verified the way every gate here has to be: by planting a file
containing the name and watching `make docs-check` fail on it.

**Why this is a decision and not a tidy-up.** Example data names nobody. A customer name in a spec, a
fixture or a mock outlives the conversation it came from and ships in the repository — and it reached
the tree through six commits and a design review without anyone catching it, mine included.

## D-0225 — a red CI job must be readable by whoever has to fix it (2026-09-03)

**Context.** `Backend (declarative partitions)` went red on the v0.1.9 release commit and
`Backend (timescaledb hypertables)` went red on the commit after it. Both times the cause was
unreadable from here: `GET /actions/jobs/{id}/logs` requires repository ADMIN, which this token does
not have, and the web view needs a signed-in session. The first failure was diagnosed only after a
human pasted the log by hand — and it turned out to be a test asserting a property of the MACHINE
(`storm too weak to be meaningful: only 61 writes`), fixed in `ee74890`. The second is still
undiagnosed. The independent reviewer named it in the release disposition: "no accessible
log/reproduction, поэтому release gate фактически не пройден" (party [79]).

**Decision.** The failing test names go into the job's ANNOTATIONS, which the check-runs API serves
without admin. `.github/workflows/tests.yml` gains a `Surface the failing tests as annotations` step
(`if: failure()`) that extracts `--- FAIL`, `<file>_test.go:NN:`, `panic:` and `FAIL<tab>` lines and
emits them as `::error::`.

**Two things verified rather than assumed, because this is a change to a GATE.**

1. **The gate still fails on a failing suite.** The output is captured to a file and `cat`-ed rather
   than piped: `run:` in GitHub Actions is `bash -e` WITHOUT `pipefail`, so `go test | tee` would
   report `tee`'s exit code and the job would go GREEN on failing tests. Turning a gate off while
   appearing to improve it is the worst available outcome, so the capture is followed by an explicit
   `exit 1`. Checked in both directions: failing → 1, green → 0.
2. **The extraction produces what is needed.** Run against a real failing `go test` with two distinct
   failures; the annotations carry the test names, the `file:line` and the messages.

Two fallbacks keep "red for no visible reason" from being a reachable state: no output file at all
says the suite died before running, and unrecognised output prints the last twenty lines.

**What this does NOT do.** It does not diagnose the outstanding hypertable failure. That has not
reproduced locally in four configurations — store alone, the whole `./...`, declarative-partition mode
on a throwaway `postgres:16-alpine`, and pinned to two cores — and the failure moves between matrix
jobs by runner load, which points at a second test of the class `ee74890` fixed.

**The strongest candidate, named so the next red run can be matched against it in one step — and
deliberately NOT changed.** `internal/store/gatemaintenance_timeline_internal_test.go:159` takes
`passStart` from `time.Now()` and uses a `> 500ms` threshold to tell the PREAMBLE survey ("at ~0")
apart from the removal-phase survey. On a starved runner the preamble survey can itself land past
500 ms, be misidentified as the removal-phase one, and fail something downstream — the same class as
the storm: a real-clock threshold standing in for a logical distinction. It is left alone because it
has not been observed failing, and it sits in the most intricate suite in the tree (FR-024, twelve
review rounds); a change made on a hypothesis there risks breaking a property that is real and that I
do not fully hold. If the next annotation names it, the fix is a logical discriminator instead of a
duration. If it names something else, this paragraph is a wrong guess kept on the record.

## D-0226 — the typed canary form, and two seams it found (2026-09-03)

**Context.** FR-029 phase F, unblocked by the owner's visual approval of the mock (D-0224). "UI as a
typed form only. No JSON editor." The owner asked for it directly after the release disposition.

**Decision.** The form is built to the approved mock: bindings declared once as stage 0, then the five
stages, every rule met AT THE FIELD, and no JSON editor on the create view or the read view. The rules
live in `frontend/src/lib/canaryWorkflow.ts`, mirroring `internal/domain/canary.go` the way
`scenarioBindings.ts` mirrors `syntheticbindings.go`, and inventing nothing of their own: a client-only
refusal would be a rule nobody could discover from the API, and a client-only permission would be a lie.

**What the client deliberately does NOT do: canonicalize.** The stored document is one canonical string
(D3e), and duplicating canonicalization in TypeScript would be a second implementation of a
hash-relevant function — which is how two write surfaces begin disagreeing about identity. The form
sends ordinary JSON and the server normalizes it.

**Seam 1 — the API surface never canonicalized, and D3e was false of it.** The file provider called
`CanaryConfig` and always stored the canonical form; the API path only PARSED and VALIDATED the
document and stored whatever string the caller sent. Two monitors with the same workflow and a
different key order therefore had different stored documents and different semantic hashes, so a
Monitoring-as-Code re-apply over an API-created canary read as CHANGED forever. Canonicalization moved
into `Monitor.Normalize()`, which both surfaces already call; a document that does not parse is left
untouched, because `Normalize` cannot fail and the refusal is `Validate`'s to make with a message that
names the position. `TestBothWriteSurfacesStoreTheSameCanonicalDocument` asserts the two surfaces AGREE
rather than that one of them "looks canonical", which is the property the hash rests on. Mutation:
removing the canonicalization — CAUGHT.

**Seam 2 — `async_canary` was never in `openapi.yaml`.** The type shipped API-reachable and
undocumented, so the generated TypeScript schema did not know it and the form could not compile against
it. That is the same class FR-028 already paid for ("the feature was API-reachable and undocumented,
which makes it unusable rather than merely un-designed"). Both monitor type enums and all three
`config` descriptions now carry the type and its document contract, and `schema.d.ts` is regenerated.

**A third gap, found by a failing test rather than by inspection.** The workflow validator covers
workflow rules; `interval >= timeout` is a MONITOR rule and it covered nothing. A form that never
surfaced it would have produced a 400 after Create, so it is surfaced at the cadence field with the
reason — one canary probe may not overlap the next, because the in-flight lease would refuse the
second run and report `already_in_flight` forever.

**And one test of mine that was about spelling rather than about the contract.** The first version of
"has NO JSON editor anywhere" grepped the rendered HTML for `/workflow.*json/` and failed on the
innocent label "Required JSON fields". It asserts the mechanism now: no `textarea`, no control whose
value is the document, and every stage present as typed fields.

**Consequences.** 23 library cases, 12 real-component cases, one live-stack spec, four killed
mutations (a credential header keeping a free-text box, the document carrying a project-secret name,
the cadence rule dropped, the placeholder allowed in the submit URL). vitest 40 files / 453 tests. FR-029
and NFR-024 stay `IN_PROGRESS`: capability announcement and the per-kind AMQP queue remain, and phase F
closing does not close them.

## D-0227 — the phase-F parity claim was false, and the seam that makes it checkable (2026-09-03)

**Context.** A P1 from the independent reviewer on the phase-F range (party [84]). D-0226 claimed
client/server validation parity and rested it on `TestTheFormsValidDocumentIsValidToTheServer`, which
proves exactly ONE happy vector. The reviewer said plainly that one vector cannot prove that every
client-accepted shape is server-valid, and then found five categories where it is not. They were right,
and I had even written that the test's sufficiency was worth disputing — which is not the same as
testing it.

**What was actually wrong.** Each of these returned `[]` from `canaryRefusals` while the Go validator
refuses it:

1. `fixture_ref` was free text checked only for emptiness — `https://evil.example/file.wav` passed the
   form. The registry is the whole contract: the runner carries the bytes and verifies a pinned
   SHA-256, so a key it does not have is not a fixture. It is a select over the registry now.
2. The correlation marker: the client substituted every marker and then parsed, so `.../x{{ … }}` and
   two markers in one URL both passed. The server requires at most one, occupying a whole path
   segment. `canaryPlaceholderIsWholeSegment` is mirrored.
3. Body keys: the grammar and the 64-key bound were unchecked, and `fieldMap` silently dropped a blank
   key and overwrote a duplicate — so the typed controls could change what the operator meant without
   saying anything.
4. Field lists: expressions, duplicates and over-long paths passed; a poll failure condition could be
   half-specified (values with no path, or a path with no values); and `lifecycle_path` is required
   UNCONDITIONALLY by `validateCanaryResult`, not only for a `lifecycle_prefix` cleanup.
5. A multipart submit forbids `content-type` (its encoder owns the boundary) and an ordinary header
   needs a value. Neither was checked.

**And a sixth the reviewer did not list, found by the new seam within minutes of writing it.** The
client emitted `submit.multipart = {file_field, fields}`, nested; the persisted canonical document has
those FLAT on `submit`. So the ENTIRE multipart arm of the form built a document the server rejected
with `unknown field "multipart"` — and 44 client tests never saw it, because not one of them ran the
document through the server.

**Decision — the seam, in two halves that cannot pass alone.**
`frontend/src/lib/canaryWorkflow.seam.spec.ts` enumerates every union variant the form can produce,
keeps only the ones the CLIENT calls valid, and compares them against a committed fixture;
`internal/domain/canaryform_seam_test.go` reads that fixture and runs each variant through the real
`Monitor.Validate` and back through `ParseCanaryConfig`. The Go half also asserts COVERAGE — every
branch of all four unions must appear — because a fixture that quietly shrank to one row is exactly the
failure the reviewer caught in its predecessor.

The TypeScript half COMPARES and only writes under `CERBIX_UPDATE_SEAM=1`. A silent rewrite would put
the drift into `git status` where it can be committed without a thought; failing says what changed and
makes regenerating deliberate.

**Two holes closed while closing this one.** The fixture is only regenerated by running the frontend
suite — and CI never ran it: the frontend job type-checked and built and never executed the 463 tests,
so a red suite could reach `main` unnoticed. `npm test` is a CI step now, which is worth doing on its
own and is what makes this seam a gate rather than a habit.

**Consequences.** 32 library cases (10 new, one per named branch), the seam's two halves, three killed
mutations against the seam itself (a non-registry fixture, a shrunken fixture, a marker demoted to a
segment fragment) plus one against the form (drift in what it produces). The general lesson, and it is
the second time this arc has taught it: a parity claim between two implementations needs a test that
ENUMERATES, not one that demonstrates.

## D-0228 — a bound the client forgets is a defect the reviewer finds, so the constants stop being remembered (2026-09-03)

**Context.** The residual half of the phase-F P1 (party [91]). After D-0227 mirrored the five named
categories, the reviewer ran complete client-valid forms again and found three more families the mirror
still omitted: `CanaryMaxJSONPathDepth` (a nine-segment path passed the form everywhere a path is
taken), `CanaryMaxStringLeafBytes` and `CanaryMaxBodyBytes` (a 1025-byte leaf and a 9 KB body), and the
1024-byte cap on `cleanup.prefix`. They also noted that a comment claiming `fieldListRefusals` mirrors
`validateCanaryFieldList` was false — the helper omitted depth while saying it did not.

**The shape of the problem, which matters more than the three items.** This is the third review round
finding the same defect: the form mirrors most of a rule set and silently omits one member. Fixing each
named omission is not a mechanism; it is waiting for the next reviewer. The reviewer put it exactly:
the 15 positive seam variants "cannot detect an omitted negative rule" — a positive fixture proves what
IS built and is blind to what is missing.

**Decision — the bounds stop being something a client remembers.**
`internal/domain/canarybounds_test.go` publishes all 21 bounds to a fixture and asserts the published
set matches the size of the const block, so adding a bound without publishing it fails in Go.
`frontend/src/lib/canaryBounds.spec.ts` asserts every published bound is either MIRRORED with the same
value or listed as unreachable from the form **with a reason in writing** — "not used" is how an
omission disguises itself as a decision. A stale justification fails too: a bound justified as
unreachable that IS mirrored, or that no longer exists in Go, is its own defect.

Three of the 21 are declared unreachable and say why: body depth and list elements (the form's body is
flat — one row is one top-level key holding a scalar or a binding) and the correlation-id length (the
target produces it at run time and the form never carries one).

**Bytes, not code units.** Go's `len` counts bytes; JavaScript's `.length` counts UTF-16 code units, so
`"é"` is one there and two to Go. Every byte-valued bound now goes through `canaryByteLength`, and the
parity spec asserts the difference on a 600-character, 1200-byte string — a gap that only ever opens
for non-ASCII input, which no happy-path test produces.

**Consequences.** 37 library cases (5 new boundary families, each asserted AT the bound and one step
past it, because a bound tested only past its edge cannot tell a correct limit from an off-by-one), the
bounds gate itself, and seven killed mutations: a forgotten constant, a constant mirrored with the wrong
value, `.length` substituted for byte length, and one per newly applied bound. 42 files / 470 tests.

The general lesson, third statement of it in this arc and the first one that is structural rather than
verbal: **when two implementations must agree on a set, publish the set and gate on the difference.**
Enumerating by hand is how the third omission happened after two rounds of promising to be careful.

**Correction, recorded because I overstated it.** The `len(bounds) == 21` assertion does NOT provide
automatic source-level enumeration: it fires only when a maintainer updates that map, and a constant
added to a different block is invisible to it. I claimed it made forgetting impossible; the reviewer
corrected that (party [93]) and was right. It narrows the window rather than closing it. Closing it
would take parsing the const block, which is worth doing and is carried as debt rather than pretended
away.

## D-0229 — the client encoded differently from the server, and the server rewrote the operator's numbers (2026-09-03)

**Context.** Two P1s on the phase-F bounds fix (party [93]). Not missing rules this time: rules applied
to the wrong bytes.

**P1-A — the body-size check measured something the server does not.** `fieldRowRefusals` measured
`JSON.stringify(...)`; Go measures `json.Marshal(...)`, which HTML-escapes `<`, `>` and `&` into
six-byte `<` sequences. A body of eight rows of a thousand angle brackets measured ~8 KB here and
~48 KB to Go, so the bound was unenforceable — six times out, not the "within a few bytes" an earlier
comment of mine claimed. That comment is the same defect as the one D-0228 recorded: **a comment that
overstates removes the reader's reason to check.**

**P1-B — numeric coercion was lossy, and silently so.** `typedValue` routed every numeric-looking
value through `Number(raw)`: `9007199254740993` became `…92` before the server ever saw it, and a
400-digit token became `Infinity` and then `null`, a value the closed algebra refuses. A 400 is a bad
outcome; a silently altered document is a worse one, because nothing reports it.

**And the server had the same defect from the other side, which the client finding exposed.**
`ParseCanaryConfig` did not call `dec.UseNumber()`, so `parseCanonicalBodyValue`'s `json.Number` branch
was dead and every number arrived as a `float64`. Canonicalisation then REWROTE the stored document:
`9007199254740993` was stored as `9.007199254740992e+15` and `1.10` as `1.1`. That is the document the
execution digest covers and the executor sends. Measured, not inferred — the probe is in
`TestABodyNumberSurvivesCanonicalisationExactly`.

**Decision.** The server calls `UseNumber()` and keeps the token. The client stops using
`JSON.stringify` for the document: `canaryEncode` mirrors Go's escaping (`<`, `>`, `&`, U+2028/9,
control characters) and emits numbers as RAW TOKENS, so one mechanism fixes both P1s — the measurement
now counts what Go counts, and the operator's digits reach the wire untouched.

The rule the client mirrors for numbers is the server's actual one: **representability, not
exactness.** `json.Number.Float64()` must succeed, so a 400-digit token is refused on both sides while
`9007199254740993` is legal and preserved — a float64 cannot hold it exactly, and nothing asks it to.
The seam found this: the 400-digit variant was refused by Go, and that refusal is what turned a guess
about the rule into the rule.

**Consequences.** 41 library cases, 20 seam variants (five new ones exercising the encoding: HTML
characters near the byte bound, a number past float64's exact range, a trailing zero and an exponent, a
300-digit token, multibyte and control characters), and mutations killed for measuring with
`JSON.stringify`, for dropping the HTML escapes, and for re-introducing `Number()` coercion. 475 tests.

## D-0230 — the escaping table stops being remembered, and a number is the whole JSON grammar (2026-09-03)

**Context.** Two findings on the encoder that D-0229 introduced (party [95]). Neither is a missing
rule: both are the client disagreeing with Go about bytes it had just promised to match.

**The escaping, wrong twice in opposite directions.** `JSON.stringify` UNDER-counted a body because Go
HTML-escapes `<`, `>` and `&` into six bytes each. The hand-rolled replacement OVER-counted, because Go
writes SHORT escapes for backspace and form feed where it wrote six-byte ones. Under-counting lets the
server refuse what the form accepted; over-counting makes the form refuse what the server would take.
Both break the same promise, and both came from recalling Go's source rather than reading it — I was
wrong about `encoding/json` twice in two rounds, from memory, with the answer one test away.

So the table is published, not recalled. `internal/domain/canaryencoding_test.go` writes what
`encoding/json` ACTUALLY produces for 28 inputs — every control character, the HTML three,
U+2028/U+2029, multibyte, an emoji, DEL (which Go leaves literal), and a map — and
`frontend/src/lib/canaryEncoding.spec.ts` asserts the client reproduces each byte for byte. The
mutations that pass through it are the reviewer's own finding, a dropped U+2028 escape, and escaping
DEL when Go does not.

**A number is the whole JSON grammar, or the type changes silently.** `isNumberToken` omitted
`[eE][+-]?digits`, so `1e3` was emitted as a STRING while the server accepts `json.Number("1e3")` and
preserves it — a semantic type change with no refusal shown.

**And the seam variant meant to cover it was named for coverage it did not have.** "a number with a
trailing zero and an exponent" contained `1.10` and `0.1` and no exponent at all. That is the third
form of the same defect this arc keeps producing — a comment that overstates, a claim that outruns its
test, and now a NAME that promises what the fixture omits. All three remove the reader's reason to
check, and all three were mine.

**Consequences.** 43 test files / 506 tests. 21 seam variants, two new: every exponent spelling, and
the short-escape characters carried so the byte difference stays measurable rather than remembered.
Mutations killed for restoring the six-byte escapes, for dropping U+2028, for escaping DEL, and for
removing the exponent from the grammar.

## D-0231 — the canary's capability announcement: the queue is the barrier, and the claim declares (2026-09-03)

**Context.** FR-029 invariant 6 was the last unbuilt half of the canary, and iter-0169 named it as such
rather than implying it: *"The executor receives a job ONLY if it announced the workflow kind and
version; a region with no such executor produces `no_capable_runner`, a version skew produces
`capability_mismatch`, and both are bounded, per-monitor and eventually a normal monitor DOWN — never
an indefinite pending."* Until now a canary in a region whose binaries predate the type answered
`unsupported monitor type` — an ordinary DOWN with the wrong reason, and the runbook told operators to
upgrade a region's executors before declaring a canary there.

**Decision 1 — the announcement is a SET of `<kind>@<version>` tokens, generalized from the envelope
generation.** §D1 of the spec pointed at this: the credential path already answers "is this region
ready for what I am about to emit" and it does so generationally, because an executor that can open
generation 1 is not evidence for a region core is about to emit generation 2 into. A canary has the
same shape — a future `async_transaction_v2` must not land on a runner that knows only v1 — so the
announcement carries the version and the scheduler asks for the token it is about to emit. Matching is
EXACT in both directions: a newer runner does not silently accept an older contract it may have stopped
honouring.

The token a job needs comes from the DOCUMENT, not from core's constant. A workflow of a kind this core
does not run asks for a runner that announces that kind, so it is refused rather than dispatched
hopefully. That is one line of code and it is the difference between a capability check and a guess.

**Decision 2 — the two failures are named separately, because the operator's fix differs.** A region
that announced nothing needs a runner started; a region that announced another version needs an upgrade
finished. From a boolean they are the same "false", so the sources return the announced SET and the
scheduler names `no_capable_runner` or `capability_mismatch` from the difference. A lookup that FAILS
refuses too — not evidence of capability, and dispatching into a region core could not verify is the
one outcome the invariant forbids.

**Decision 3 — the AMQP half is a queue, not a flag.** D-0160 settled this argument once already for
envelopes: *a capability CHECK does not stop a consumer from consuming.* A canary published to the
shared per-region queue in a mixed fleet is taken by whichever worker gets there first, including the
one that cannot run it. So a canary rides `checks.canary.<kind>@<version>.<region>`, bound only by a
worker whose RUNNER has the type — and consuming that queue IS the announcement core reads back
through the management API. Two carriers, not one, because `secrets.enabled: false` must keep a
SECRETLESS canary working on a worker that can open no envelope at all, and that worker must remain
physically unable to receive one: `checks.canary.<token>.<region>` for generation 1 and
`checks.canary.v3.<token>.<region>` for generation 3. Both announce the same capability; whether the
region can also take an envelope is a different question the credential readiness check already
answers, and asking it twice would give two answers that can disagree.

The publisher's envelope rule on that carrier is EXACT rather than the generic "generation 2 and up
must carry one": a canary needs an envelope precisely when its workflow binds a secret. Stated that
way it refuses both mistakes — a stripped envelope, and an envelope on the queue a capability-0 worker
consumes.

**Decision 4 — the pull half filters at the CLAIM, because a queue cannot do it there.** Every agent in
a region claims from one table, so the scheduler's refusal to enqueue is not the whole barrier: in a
mixed fleet the region DOES announce (its new agent did), and the old agent would take the canary and
fail it. `pull_jobs` therefore carries the capability the job REQUIRES (migration 00098, NULL for
everything that any agent may run), the claim declares what it can run in `X-Cerbix-Workflow-Kinds` —
per request, exactly as `X-Cerbix-Credential-Envelope` is — and the claim query filters. An agent that
declares nothing still claims every ordinary job, which is the direction that matters: a barrier that
starved the ordinary path would be the worse bug, and that is the shape of the D-0160 defect where a
capable claim returned only its own generation and left every ordinary monitor's row to expire.

**Decision 5 — role=all announces from its runner, not from a constant.** The in-process executor's
capability is this binary's, and the credential path already paid for forgetting that: until D-0160's
local wiring existed, the commonest deployment never moved past generation 2 and the whole amendment
was inert there. So `WithLocalCanaryRegions` exists — and both it and the AMQP worker's declaration are
derived from `runner.Supports(domain.MonitorAsyncCanary)`, one check, so the two paths cannot drift
into announcing different things.

**What the tests reach, and the mutations killed.** The gate: a region that announced nothing writes one
DOWN with `no_capable_runner` and takes NO in-flight slot (the check precedes the lease, so an
incapable region cannot park a slot it can never release); a skewed region says `capability_mismatch`;
an agent announcement does not speak for an AMQP region; a failed lookup refuses; an instance with no
canary pays for no lookup at all. Mutations killed: the gate always passing, the reason collapsed to
one value, and the resolver made eager. The store: the announcement is unioned across LIVE agents only,
proven by AGEING the row rather than by shrinking the window — a non-positive window falls back to the
function's own default and would have proven nothing. The claim filter: an old agent takes the ordinary
job and only that, a v2 agent takes neither, the capable agent takes the canary; mutation killed for
removing the clause.

**And one comment corrected before a reviewer had to.** `declaredKinds` normalizes nil to an empty
array, and my comment claimed a nil slice would make the comparison NULL "rather than false" and leave
every ordinary job unclaimable. A mutation proved that false: pgx sends nil as NULL, `= ANY(NULL)` is
NULL, and a WHERE treats NULL as not-true — the same answer. The helper stays because that agreement
rests on three-valued logic holding under a clause nobody has negated yet, but the comment now says so.
Fourth instance in this arc of the defect D-0228 named: **a claim that outruns its test.**

**Addendum — reviewer P1 on the range (party [99]/[100]): role=all announced a pull-served core.**
`pull.regions: [core]` is legal, and role=all announced its in-process runner for `core` regardless.
But a pull-served region is executed by its AGENTS — every job for it goes to `pull_jobs`, whatever
else runs in the process — so the local runner was not evidence of capability there. The consequence
was the exact failure this decision exists to forbid: the gate passed, a capability-bound row was
enqueued that no legacy agent could claim, no heartbeat was written, and the monitor stayed silent
until the row's TTL. Fixed where the reviewer said the invariant belongs: at RESOLVE time in
`canaryAnnouncements`, which now skips any local region that is pull-served, so neither builder order
nor configuration can reintroduce it. `TestTheInProcessRunnerDoesNotSpeakForAPullServedRegion`
asserts both builder orders: a bounded `no_capable_runner`, no in-flight slot taken, no row enqueued.
Mutation killed by removing the guard.

The same shape exists on the credential side — `localCredentialRegions` raises `carrierGeneration`
for `core` without excluding pull-served regions — and is deliberately NOT touched in this range: it
predates the canary, it changes what generation core EMITS rather than whether a monitor reports at
all, and widening a review-fix range is how a second defect hides behind the first. It is named here
so it is a known thing and not a discovery.

## D-0232 — the in-process executor is no evidence about a pull-served region, on the credential side too (2026-09-03)

**Context.** The reviewer approved the canary capability range (party [102]) and withheld release
approval on the residual D-0231's addendum had named: `localCredentialRegions` raised
`carrierGeneration[core]` to 3 and marked core credential-ready on the strength of the in-process
executor, without asking whether core was pull-served. `pull.regions: [core]` is legal. A pull-served
region is executed by its AGENTS — every job for it goes to `pull_jobs` — so in role=all with that
configuration core emitted a generation-3 row that a capability-1 agent cannot claim (it polls the
generation-2 endpoint, which returns rows at or below 2). The row expired by TTL; the credentialed
monitor had no outcome at all. Same configuration precondition as the canary P1, same silence, and
this one reaches ordinary postgres/mysql/redis monitors. It predates the canary; the canary work is
what made it visible.

**Decision.** The exclusion lives where the canary's does: at resolve time, in the loop that folds the
in-process announcement into the readiness maps — a pull-served region contributes nothing there.
Readiness and carrier for such a region then come only from what its agents announced
(`LiveCredentialReadyAgentRegions` at envelope v1 for readiness, at v2 for the carrier), which is the
rule the rest of the block already followed for every other source. Neither builder order nor
configuration can restore the override.

**What the test reaches.** `TestTheInProcessExecutorDoesNotRaiseAPullServedRegionsCarrier`: role=all
wiring in BOTH builder orders, `pull.regions: [core]`, a credentialed postgres monitor in core, and a
single live agent that announced envelope v1. Expected: one pull row, at generation 2 — the carrier
that agent can open. Removing the guard emits generation 3 and the test fails. To make that assertion
honest the fake store stopped lying in two places: `MaterializeExecutionConfigs` now picks the job's
carrier from the policy it is handed (floor 2), as the real one does, and `LiveCredentialReadyAgentRegions`
answers by minimum capability, as the real query does. A fake that ignored the policy would have
passed with the defect in place.

**Not changed.** The `!ready` branch for a pull region with NO live agent keeps its existing behaviour
(a logged `no_capable_executor`, a metric, a backoff) — that path predates both findings and is the
same for AMQP regions without workers; making it a per-monitor heartbeat is a separate decision.

## D-0233 — a monitor write says who wrote it: FR-026's rules applied to the three monitor writers (2026-09-03)

**Context.** FR-029 invariant 13 promised that an acknowledged `cleanup.kind: none` is "visible in the
API, the UI and the audit trail". The discharge map could cite tests for the first two and said so
about the third: a monitor write — create, update, delete — left no `audit_logs` row, for any type,
and no requirement claimed it. The reviewer listed it as the fourth FR-029 gap at [79]; after [106]
it was the one `PARTIAL` left in the map. The owner ordered the residuals closed (2026-09-03).

**Decision.** Not a new requirement: an amendment to FR-026 (§10, D11–D13, invariants 17–21), because
every rule needed already exists there and inventing a second audit vocabulary for the same table is
how the two drift. The three writers the API reaches become `CreateMonitorByPrincipal`,
`UpdateMonitorByPrincipal`, `DeleteMonitorByPrincipal`, taking the validated `AuditActor` and writing
one row inside the mutating transaction; the bare writers are unexported, reached by the tests through
an `export_test.go` hook, so the product has no exported unaudited monitor writer and a forgotten actor
is a compile error. No `…BySystem` door: the file provider is D1's machine writer and has its own
INSERT, and its record is the bundle. Three closed words — `monitor.create`, `monitor.update`,
`monitor.delete`. The target names the document and never its contents: id, slug, type, region; an
`enabled` flip on update; and, for a canary with an acknowledged `cleanup.kind: none`,
`cleanup=none acknowledged` — the invariant-13 clause, made true.

**What stays as it was, and why.** `monitor.retired` / `monitor.reactivated` / `monitor.successor.*`
predate FR-026 and write through `auditProjectActTx` with a `GraphActor`; D3 declined to unify the
actor shapes and this amendment does not reopen that. The scheduler's status and sequence writes, the
re-encryption sweep and the secret-rename re-point touch monitor rows and are not "a monitor write" in
the sense an operator asks about; they are named here so their absence from the trail reads as a
decision.

**Correction from review (party [112], P1).** The first version let the CALLER decide what the delete
row said: `DeleteMonitorByPrincipal` took a whole `domain.Monitor` and built the target and the tenant
from it, while the writer deleted by id alone. Two consequences, both real: a concurrent update between
the handler's read and the delete lock made the audited region stale, and an internal caller could pass
monitor A's id with monitor B's project — deleting A while filing the row under B's organization. The
door now takes an ID; `deleteMonitor` reads project, slug, type and region in the same statement that
takes `FOR UPDATE`, and the audit is built from THAT row. The update hook likewise gets the `RETURNING`
row rather than the caller's struct, so an omitted slug (legal: "keep it") still names the stored one.
Two regressions and two mutations: auditing a caller-shaped struct fails both.

**Evidence.** Store: one row per door, in the transaction, rolled back with it, with the D13 shapes
including the canary suffix; a zero-valued actor refused before any statement. API: the second guard
extended — `internal/store` declares no exported bare monitor writer outside test files, with a fixture
that violates it. Live: a canary created through the API with an acknowledged `cleanup.kind: none`
shows in `GET /api/v1/organizations/{orgID}/audit` as `monitor.create … cleanup=none acknowledged`.

## D-0225 — addendum: the log was readable after all, and the candidate was the wrong guess (2026-09-03)

**What changed.** `GET /actions/jobs/{id}/logs` answered **302** to a signed download for the token this
session holds — not the 403 this decision recorded. The hypertables job on `ee74890` (run 33725637820)
failed in the `Tests (-race, …)` step with TWO store tests, and `internal/store` took **942.8 s** on the
runner against ~660 s here:

1. `TestGateMaintCrashAfterEveryRemovalStatementConverges/after_drop.commit` —
   `crashed=false err=<nil>`.
2. `TestConfirmedAnnulActuallyRewritesTheSealedFacts` — `repair slice: store: complete repair range:
   timeout: context deadline exceeded`.

Neither is the candidate this decision named. That paragraph said "if it names something else, this
paragraph is a wrong guess kept on the record" — it is, and it stays.

**Failure 1, mechanism confirmed by reproduction rather than by reading.** The D10 drop gate asks
`pg_stat_activity` whether ANY backend of the database holds a transaction older than `detached_at`
(the cached-plan hazard, 15.9) and defers the drop if one does — correctly, and silently: the pass
returns nil. The test injected its crash on the statement AFTER `drop.commit`, so a deferred drop reads
as `crashed=false err=<nil>`. Opening a foreign transaction before the plant reproduces the exact text
locally on the first try; on the CI service container, 942 s of churn make an autovacuum worker the
plausible holder — plausible, not proven, and the addendum says which. The test now injects the crash
on the first pass that actually REACHES the drop (bounded retries; a closed gate is a deferral, not the
property under test), and `TestGateMaintADropIsDeferredWhileAnOlderTransactionIsOpen` pins the
mechanism with a transaction the test opens itself: nil, nothing dropped, and the next pass drops once
the transaction ends. The product is unchanged.

**Failure 2, mechanism read from the code and matched to the message.** The leader's repair slice runs
its closing lifecycle write INSIDE the slice deadline (`leaderLifecycle`, §10.10). `drainRepair` gave
each slice 2 s; on a slow database the batch used the budget and the closing `UPDATE … state =
'complete'` missed it — the exact message. In production the range stays `running` under its 60 s
lease and the next claim resumes at the durable cursor; the helper failed instead. It now mirrors the
recovery — expires the lease as the clock would and claims again — and still fails on any other error.
Product unchanged. Production slices are 250 ms (`maxDispatchDelay`), so the test was not
under-budgeted relative to the product; the closing write's 40 ms reserve is the design's own trade.

**What this does not claim.** Neither failure reproduced here under a single pinned core with the
whole suite running alongside (three attempts, all green); the first reproduces only with the foreign
transaction, the second not at all locally. A re-run of the failed jobs was refused (403: the token has
no `actions:write`), so "green on re-run" is not evidence this addendum has. The evidence it has: the
log, the two mechanisms, one reproduction, and tests that no longer read a legitimate deferral or a
legitimate lease as a failure.

## D-0228 — addendum: the bounds are enumerated from the source, not counted by hand (2026-09-03)

The `len(bounds) == 21` this decision recorded as debt ("fires only when a maintainer updates this
map") is replaced by source-level enumeration in both directions: `canarybounds_test.go` parses the
canary source files for every `CanaryMax…` / `CanaryMin…` constant and parses ITSELF for the identifiers
the published map references, and each set must equal the other. Adding `CanaryMaxPhantomThing = 7` to
the const block fails the test with its name, without anyone remembering a number. The naming rule is
the contract — a bound spelled otherwise is invisible to the client gate and must be renamed, a
review-visible edit rather than a silent omission.

**And the first version of that fix repeated the defect it replaced (party [112], P2).** It parsed a
hand-written list of four file names, so a bound in a new `canary*.go` was invisible until somebody
remembered to extend the list — the hand count in another form, which is exactly what D-0228 set out to
remove. The enumeration now globs every non-test `canary*.go` in the package directory, fails if the glob
finds none, and `TestTheBoundEnumerationSeesAnUnlistedCanaryFile` proves it against a COPY of the
directory carrying a `canary_phantom.go` that exists nowhere in the tree.

## D-0234 — a monitor says what it is for: an optional 200-character description (2026-09-03)

**Context.** The owner: "a stranger who comes to look may not understand a monitor's purpose from its
name alone." One optional field, with a limit, shown wherever monitors are listed, and nothing about
existing monitors may break. The mock (`docs/design/mock-monitor-description.html`) was approved with
two answers — the limit is 200, search does not match on it — and this record fixes the rest.
Specification: `docs/specs/func-monitor-description.md` (FR-030).

**Decisions.**

1. **200 characters, counted as code points on both sides**, asserted against one fixture so the form
   and the server cannot disagree — the exact drift the canary form paid three review rounds for
   (D-0227…D-0230). A named constant, `domain.MaxMonitorDescriptionRunes`.
2. **Empty is the default and means "none".** `NOT NULL DEFAULT ''`; omitted on create is empty, omitted
   on update is unchanged, `""` on update clears; whitespace is trimmed. Every existing monitor reads
   back empty and every surface renders as before when it is empty — no placeholder, no empty line, no
   change in row or card height. That is the whole compatibility promise, and a test per surface holds
   it.
3. **Where it appears is decided by how much room there is to read it**: a second line under the name on
   `/monitors` (one line, ellipsis, full text as tooltip — not a sixth column); the beginning of a line on
   a dashboard panel, cut by the panel's own width in CSS so the grid keeps its shape; the whole text on
   the monitor page; a textarea with a live `n / 200` count on the form, with the refusal met at the
   field and Create/Save disabled while over.
4. **Where it does not appear is named**: public status pages (internal wording), notifications (their
   payload is a separate decision), search (the owner's answer), service pages and incident chips (one
   click away). An absence that is written down is a decision; one that is not is an oversight.
5. **A bundle owns the field like every other**: `description` is an optional key with the same bound; a
   bundle without it declares "none"; a change to it alone is a change (the canonical hash covers it);
   removing the key clears it. The first draft of this point said "re-applying over a UI-set description
   clears it" — a scenario the product cannot produce, because a file-managed monitor refuses UI and API
   writes (`ErrManagedByFile`). The test found that; the record says what is true instead.
6. **A description change is a monitor update** and audits as one (D-0233); its text never reaches the
   audit target.

**Correction from review (P1, 2026-09-03): the dashboard height contract as written was false.** The
spec said the card "keeps its height and its grid". It does not: the description is a flex child of a
`flex flex-col gap-[13px]` card, so a described card is one line (~23px) taller, and `truncate` only
prevents a SECOND line. Measured with `MonitorCardDescription.spec.ts` — a described card has exactly
one more flex child — rather than argued.

Two facts shaped the resolution. The approved mock renders exactly what the code renders, so the false
thing was the prose, not the appearance. And this card's height has always depended on its content:
`v-if="monitor.type !== 'push'"` (latency) and the error-budget meter mean a push monitor is already
shorter than an http monitor beside it — the same test asserts that, so "the panel keeps its height"
was never true of this component for any field.

The owner chose to keep the second line and state the real contract (2026-09-03): a card with a
description is one line taller; inside a grid row the items stretch, so no card is taller than its
neighbour and the ROW grows by that line, an undescribed card in it gaining trailing space. Reserving
the line on every card was declined — it would change every existing dashboard for people who never
write a description. Rewriting the sentence to match the code was not treated as sufficient on its own:
the geometry is now asserted structurally, in the regression the reviewer asked for.

## D-0235 — a rendering claims no more than the facts behind it (2026-09-03)

**Context.** The owner brought two complaints and one question. On the monitor page he wanted
Grafana-style hover values on the Response time panel, and asked whether that steps onto
Grafana's territory at all; on the Services page he did not like the long red and green bars in
Reliability. Reading the code for the second one turned a padding complaint into a truthfulness
problem, and the same shape turned out to sit on a third surface he pointed at later, the /sla
project-objective card. Specification:
[`func-truthful-rendering.md`](specs/func-truthful-rendering.md) (FR-031, NFR-025). Reviewed
across party [133]–[151]; every geometry, encoding and tenant-context question below carries the
reviewer's ruling, and two of this session's own proposals were overruled and are recorded as
such.

**Decisions.**

1. **The question is answered no.** A hover readout over data the page has already fetched is
   not observability tooling. The line is drawn explicitly and permanently for this package: no
   query builder, no time-range picker on these panels, no drag-zoom, no panel composition, no
   second metric or overlaid series. What justified the work was not "only one panel" but that
   the panel was **less defensible than the table beside it** — it dropped an enumerable class
   of real failures and had no time axis at all.

2. **The timeline's geometry becomes clock time.** Cells are generated from the requested UTC
   range at the rollup grain instead of from whichever points the response happens to carry, and
   unstored time occupies width in its own encoding. A 30d window over a service with two and a
   half days of facts was filling 100% of the strip while the KPI directly above it read
   `storage 2873 of 43200 buckets`. This **refines** party [218] P1-3 rather than reversing it:
   bucket count was a proxy for time extent, the two agree wherever storage is complete, and
   they diverge only in a storage hole. Ruled at [140].

3. **The gaps were decoration.** Padding was 8% of each tick's own width, which is a hairline at
   ninety ticks and a canyon at three, reading as absence while meaning nothing. Confirmed by
   pixel measurement of the owner's screenshot against the model — predicted gaps 54.0 px and
   81.2 px against 54 px and 81 px measured, cells summing to 1076 px against a 1075 px strip —
   rather than by eye, after a first version of the finding cited eyeballed numbers.

4. **A cell becomes a proportional stack, and the short tick is retired.** Winner-takes-all
   colouring rendered a service at `availability 99.667%` as a wall of red once only three wide
   ticks were on screen. The replacement carries a **capped, presentation-only floor** on the
   non-good slice so a one-second outage still cannot vanish — recorded as a trade-off, because
   "bad wins the cell" was a *semantic* guarantee and a pixel floor is a *rendering* one. The
   consequence is a deliberate change to the motif approved in 2026-08 (§12.2 of
   `func-service-reliability.md`, amended in place): inside a stack, height is the slice's
   quantity, so height can no longer carry its identity. Ruled at [143] and [138].

5. **`not-stored` is not `unknown`, and gets a fifth encoding.** `unknown` is a decided verdict
   with `unknown_us` behind it; a missing bucket row is the absence of a verdict. `unknown`
   becomes a solid `--ink-3` slice with no status hue, `not-stored` a hatched slice with a cell
   outline labelled "no stored bucket", and `provisional` remains the sole user of opacity. This
   session had them conflated; corrected at [136].

6. **Segment lanes ride the shared axis, and their width is never floored.** A one-day and a
   three-day segment were the same length, so length said nothing. A lane's horizontal extent
   claims duration, so a sub-pixel segment is met by a **non-geometric anchored marker** that
   says it is not to scale, with collisions stacking vertically — never by a silently widened
   lane, which would reintroduce the very defect on the surface being fixed. The slice floor and
   the lane rule are therefore two mechanisms for two axes: height tolerates a floor, width does
   not. This session proposed widening-with-a-label and was overruled at [140].

7. **The Response time panel draws real timestamps, and no stroke across unprovable time.** Two
   of this session's proposals were overruled and the rulings are better. An ordinal axis with a
   wall-clock tooltip still invites slope-as-rate, so the axis carries real time ([134]). And the
   frontend may not derive "a due check is missing" from `interval_seconds` plus a tolerance:
   cadence, probe overlap and dispatch delay are application semantics, and such a formula is a
   product contract only once specified and tested, not a chart heuristic ([136]). **v1 therefore
   draws points with no connecting stroke and no area fill** — the fill implies a continuity the
   data cannot support — with the panel's value moving into the hover readout. The owner accepted
   that consequence explicitly. The named follow-up that earns the line back is a
   contiguous-segment fact computed where cadence is actually known.

8. **The timeout is stated always and drawn only inside the plot extent.** This session proposed
   drawing it once samples reached 25% of it; the reviewer called that a magic number, correctly,
   and the extent boundary is factual instead. It is also doubly sufficient: a timeout that
   actually occurs is recorded at roughly the timeout, so it raises the extent itself. The
   session's own objection to the extent rule was withdrawn on that evidence.

9. **Identity is UTC; presentation is local.** The owner asked whether the range could be
   browser time. Grain, requested range, cell identity and all arithmetic stay UTC — local-day
   cells would stop decomposing the number printed above them, and two viewers in different zones
   would read different per-cell figures for one published availability. Every human-readable
   time renders in the browser's zone with its offset named, plus the UTC instant for log
   correlation, and **a UTC cell is never labelled as the viewer's calendar day**: a UTC day
   starts at 05:00 for a UTC+5 viewer, so the tooltip states its true local extent. The
   mechanism is **two named functions** — one for an instant, one for a UTC cell extent — with no
   generic formatter that could be handed a bucket and produce a local calendar day, and offsets
   resolved **at the instant** via `Intl` rather than from a cached current offset, since a 30d
   window in late March or October crosses a DST boundary as the ordinary case. No timezone
   setting in v1. Ruled at [143].

10. **The /sla project-objective card becomes read-only with an explicit Edit.** The owner's
    report was that the buttons stay after Save and mislead; the mechanism was that a successful
    save never cleared the draft while Clear did, so an unsent draft and a stored fact rendered
    identically. The owner chose the Edit shape over keeping the form open, and the reviewer
    recommended the same, for a structural reason: a closed editor cannot hold a stale draft.
    **An open form is not a substitute for a draft-state model**, so all of it holds regardless
    of shape — clear the draft on success, a bounded success confirmation, Save disabled when it
    would do nothing (by the existing D-0165 rule), and the two P0 items below. Recorded as a
    change to an **approved** mock (`docs/design/mock-project-objective.html`, iter-0155).

11. **Two P0 tenant-context defects on that card, found while reading it and not reported by the
    owner.** `resetMaintenanceState()` resets seven pieces of editor and busy state and its own
    comment claims to cover every one, but not `projDraft`/`projErr`/`projSaving` — so a value
    typed for project A is still in the box under project B, where Save writes it as a
    perfectly legitimate write the store cannot refuse. And neither writer takes a load
    generation, while every neighbouring writer in the file does and one says why ("the project
    moved under this response"). The two fixes are **complementary, not alternatives**: one stops
    a stale draft being displayed, the other stops a stale response being written. Ruled P0 at
    [147]; the second was the reviewer's find, verified here against the code before acceptance.

12. **The pre-existing zone inconsistency is NFR-025, split into a half that ships and a half
    that stays open.** Four SPA files render browser-local times without naming a zone while the
    reliability card renders UTC, and nothing says which is which. It predates this package and
    is not fixed by it. **NFR-025a** — the §8 mechanism plus every timestamp on the FR-031
    surfaces going through it — ships here; **NFR-025b** — the five enumerated call sites
    substituted onto it — stays `TODO`, and iter-0174 may not report NFR-025 as DONE.
    The split exists because the specification's first revision asserted an instance-wide
    invariant ("every rendered timestamp in the SPA names its zone") while §9 deliberately
    excluded those five sites: two statements that cannot both be the shipped contract. Reviewer
    P1 at [153], accepted; the contradiction was resolved by scoping the SHIPPED invariant, not
    by weakening the follow-up, which keeps its identifier, its acceptance criterion and its open
    status. Widening this iteration to four unrelated views was declined at [143].

13. **Scope is one iteration, by the owner's override.** The reviewer proposed scoping the
    segment-lane work separately; the owner wants the card fixed in one visit. It carries its own
    acceptance row and its own discharge so it can fail without dragging the rest down. Recording
    the heartbeat request's `limit` bound as a spec constraint was declined by the owner.

14. **Colliding marks cluster by the lane rule, extended rather than duplicated.** Drawing the
    mock produced a case the design had not covered: several definition changes minutes apart put
    their boundary marks inside one pixel at *any* window zoom. The contract — cluster the
    colliding events into one mark; anchor it at the **earliest real boundary**, never at a
    midpoint, because the anchor is the cluster mark's only geometric claim; carry the count with
    the exact local `[first, last]` extent, its numeric offset and the UTC instants; and list every
    event in the cluster **chronologically** in the readout, because a count may not stand in for
    the changes it counts. Ruled at [157].

15. **Every figure a mock displays must be derived from its own fixture.** The most instructive
    finding of the whole arc: the drawing's segment percentages and KPI were borrowed from the
    owner's screenshot while its geometry came from the mock's own fixture, so
    `availability 99.667%` and `coverage 98.5%` sat beside cells that computed something else —
    the mock committing, one level up, exactly the defect it exists to fix. The mock now holds ONE
    fixture and derives every cell, lane, segment figure and KPI from it through the spec's own
    formulas (`func-service-reliability.md` §11.1), with the provisional tail excluded per §11.3;
    its script is executed under a DOM stub as part of preparing it, so the figures are run rather
    than asserted. The `/sla` cards are the one exception and say so on the page: there is no
    fixture behind an operator's typed objective, and what that surface specifies is a state
    machine rather than a figure. Reviewer P1 at [159].

16. **A lane's readout carries its storage verdict, and withholds availability without it.** The
    real-time axis of decision 6 immediately exposed a defect nothing else had: a segment spanning
    28 days with 300 stored minutes renders 27 days of `not-stored` and, in the first drawing,
    printed `availability 100% · coverage 100%` beside it. `coverage = 100%` says only that every
    observation that exists was decidable, which a storage hole makes worthless as a warrant, and
    `func-service-reliability.md` §11.2 makes the two axes independent with both required. So each
    segment now states `storage contiguous` or `<stored> of <extent> · incomplete`, and an
    incomplete segment quotes no availability. Coverage stays printed as its own separately named
    fraction, because it answers a different question. Reviewer P1 at [161].

    **Corrected at [176], before any code:** the first revision had the withheld dash carrying the
    canonical payload codes `window_precedes_materialization_era` / `storage_gap`, derived in the
    component. That crosses an ownership boundary — a client may derive PRESENTATION from the
    series it holds, but it may not manufacture a wire verdict whose canonical meaning belongs
    server-side. The dash now carries a **display-only explanation with no code identity**
    ("records begin later in this segment" / "missing records inside this segment") under an
    explicit display-only fence: it never leaves the component, never enters metrics, audit or the
    API, and never drives reliability state. The numerical storage verdict stays client-derived
    under the same fence. Publishing a canonical per-segment reason is a follow-up server contract.
    This session flagged the risk when starting implementation instead of discovering it in
    review, which is the only reason it cost a paragraph rather than a rewrite.

17. **Four corrections the mock's own review forced, recorded because a drawing that gates an
    implementation has to be right.** (a) The fixture's stored buckets totalled 2770 while the KPI
    drawn above the strip read `2873 of 43200` — a mock whose computed evidence disagrees with its
    own stated source cannot gate anything; the fixture now totals 2873 and every dependent label
    moved with it. (b) At 1440px the axis's identity label overlapped the terminal date tick, and
    an identity label may not overwrite a fact; it lives on its own line now, on both the strip and
    the latency panel. (c) The proposed boundary cluster claimed `×3` over a four-minute extent
    while only **two** of the three lane starts are transitions inside the window — the third is
    the first segment's start, four weeks away at the window's own left edge. It reads `×2` with
    the exact two transitions listed. (a)–(c) were reviewer P1s at [157]. (d) The panel drew 62
    minutes of checks and labelled it "last 60 checks" while six of those minutes hold no check at
    all — the API returns ROWS, so sixty recorded checks span sixty-six minutes here. Found by
    executing the mock's own script rather than reading it.

18. **The owner asked for the line back, and it cannot be earned — the reason is structural.** He
    chose to bring the contiguity fact into this iteration rather than take it as a follow-up, on a
    cost estimate of this session's that was wrong: "no migration needed". The design was rejected
    at [166] on three readings of the tree, verified here before acceptance and two of them this
    session's own misreadings. `internal/prober/prober.go` runs `Retries + 1` attempts each under
    its own `context.WithTimeout(m.Timeout())`, so a run may occupy `(retries+1) × timeout` and not
    one timeout. `result.allowed_skew` is only step 4b's clock test in `internal/store/monitors.go`
    (`ts.Before(hb.JobIssuedAt.Add(-skew))`) — a bound on how far *before* its job's issue an
    observation may claim to be, bounding neither queue delay nor delivery; using it in a spacing
    allowance was not a stretch of its meaning but the wrong quantity. And `job_issued_at` is not a
    column of `heartbeats` at all. So two received heartbeats bound **observed spacing** and
    nothing more: queue wait is unbounded under broker trouble, leader absence leaves no trace, and
    cerbix cannot witness its own absence. No fifth term rescues a formula whose subject is absent
    from the data. The corrected cost went back to the owner and he re-decided for the honest panel.

19. **Absence is rendered positively: the observation ruler.** A thin band under the plot carries
    one neutral tick per recorded heartbeat and nothing between them, so unobserved time is drawn
    rather than left as whitespace — uniformly, with a six-minute interval and an ordinary minute
    being the same statement at different sizes. An empty span means only "no check was recorded
    between adjacent points": not late, missed, covered or anomalous. **It has no threshold**, and
    that is the load-bearing part: a fixed 12 CSS-pixel rule was specified at [169] and
    **withdrawn** at [171] after this session did the arithmetic and brought it back — the panel
    runs at 16.5 px per minute, so 12 px marks all 59 intervals and distinguishes the hole from an
    ordinary minute not at all, while any threshold relative to the panel's own spacing would be
    the anomaly detector [166] removed wearing a legibility costume. Accessibility may merge
    adjacent sub-hit-target spans into one focus target, preserving every tick and the real
    geometry, and **interaction grouping is never rendered as semantic grouping**. Declared
    maintenance is out of scope: with no stroke, no value crosses excluded time, so the truth
    condition holds by construction and a band is recorded as a named optional.

20. **FR-032 is opened for the fact cerbix does not record.**
    `docs/specs/func-expected-run-ledger.md` states the problem, the evidence that the fact is
    missing, and the four facts a solution must carry — materialized due windows; issue, claim and
    terminal-outcome timestamps; configuration bound to the *historical* run rather than the
    monitor's current fields; and retention semantics. It contains **no design**, and says so: no
    schema, no API, no retention rule, with volume, ownership and the DB-less roles named as open
    questions. Only its acceptance criteria may ever permit a stroke between adjacent covered
    windows. It is deliberately not carried by iter-0174, which ships the honest panel while this
    changes the reliability data model.

21. **One more of this session's own forbidden inferences, caught by executing the mock rather than
    reading it.** The panel's prose read "6-minute hole" — a count of *missing checks*, which
    assumes six checks were due, which is exactly what decision 18 establishes cannot be known. It
    now states the interval between two recorded checks and nothing else: "widest interval between
    two recorded checks here is 7 minutes, 19:33 → 19:40 — and the panel says only that, never that
    a check was missed."

22. **One defect only the running stack could show, recorded because the lesson is the method.**
    Photographed in the live app, the Response time panel read "widest interval between two recorded
    checks: **0 min**" for an eleven-second interval — `Math.round(ms / 60000)`, which is a false
    statement about the interval on the one panel whose subject is not making those. Every fixture
    in the suite happens to use whole minutes, so nothing but looking at it would have caught it.
    Fixed by `gapLabel`, which picks a unit that fits, with tests and an assertion that a real
    interval is never rendered as zero. The iteration also records what was NOT photographed: the
    Reliability card's live pixels, because the dev stack has no service and sealing one takes real
    time.

23. **Two P1s in the implementation, and both are the same mistake in two places: a rendering
    convenience quietly overriding the invariant above it.** Found at party [178] and verified here
    before acceptance.
    (a) **The drawn width was floored.** `Math.max(0.25, …)` applied to every cell, so on a 30-day
    axis anything under ~10.8 minutes was drawn as ~10.8 minutes — width overstating duration, on
    the surface built to stop exactly that. Fixed by separating the FACTUAL width (exact, never
    floored, drawn only when positive) from the HIT TARGET (widened, centred, and marked
    `data-affordance="non-geometric"`), so a sub-pixel cell stays reachable without its drawing
    claiming the extra time.
    (b) **The floor could buy height the cell did not have.** It floored the slice, capped the debt,
    and paid only out of `good` — so a 34 px cell holding one second of `bad` and no good returned
    35.9996 px, which SVG clips, making a picture that claims more time than it has AND hides the
    evidence of doing so. Fixed by funding the floor in a stated order (sealed good → provisional
    good → absence) and RETURNING whatever cannot be funded. The last funder overrules this
    session's own earlier note that absence may never shrink: with one second of `bad` and no good,
    "the problem is visible", "absence never shrinks" and "the total is exact" cannot all hold, and
    the first was ruled at [143] while the third is not negotiable. Recorded as spec invariant 6a.

    One of the three regression tests for (b) was written only because a MUTATION SURVIVED: removing
    the give-back loop failed nothing, so the case with no funder at all did not exist yet. It does
    now, and it kills that mutant.

24. **The evidence count was internally inconsistent, and the fix is a distinction rather than a
    number.** Party [177] said "thirteen mutants" while `traceability.md` said twelve and the
    iteration report said 576 tests against a later 585 — reviewer P2 at [178]. The thirteen came
    from folding a defect the tests CAUGHT into the count of mutants planted and KILLED. They are
    different kinds of evidence: a killed mutant says a test would notice a regression, a caught
    defect says a test noticed a mistake. The records now state both separately — **fifteen mutants
    killed, four defects caught with nothing planted, and one the running stack caught** — with 585
    tests over 50 files. Every gate figure in this arc is from a run in this session's own
    environment; the reviewer has no npm and none of it is attributed to him.

**Status.** IMPLEMENTED at iter-0174 (2026-09-03); FR-031 and NFR-025a are DONE, NFR-025b and
FR-032 stay open by design. The mock
(`docs/design/mock-truthful-rendering.html`) is drawn, carries the two cases the reviewer asked
to see — a sub-pixel segment with its marker and a UTC cell tooltip showing its true local extent
— and awaits the owner's approval, which gates all frontend code.
