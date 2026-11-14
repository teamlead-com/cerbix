# sec-hardening-2 — the 2026-11 security findings: authentication boundaries, request forgery, probe input, and the public port

> **STATUS: revision 7 — APPROVED by the OWNER, 2026-11-13. Iteration 1 (H-1, H-5, H-6) implemented in
> [iter-0209](../iterations/iter-0209.md) (D-0276); iteration 2 (H-2, H-13, H-1 fix 8) in
> [iter-0210](../iterations/iter-0210.md) (D-0277; H-2 amended 2026-11-14 by two owner rulings and
> the external review of that iteration); iteration 3 (H-3, H-4, H-9a, H-14) in
> [iter-0211](../iterations/iter-0211.md) (D-0278; the S4 content-type rule extended to `/auth/local/*`
> by an owner ruling of 2026-11-14); iteration 4 (H-7, H-8, H-9b, H-10, H-11, H-12) in
> [iter-0212](../iterations/iter-0212.md) (D-0279), with the CSP report-only — its enforcement is the one
> item left, scheduled for the release after. The two owner questions are answered (§8).**
> Six independent review rounds: **4/6/5/1**, **1/5/2/1**, **0/3/3/1**, **0/1/3/0**, **0/1/1/0**,
> **0/0/1/0** (P0/P1/P2/P3). Round 6 found no P0 or P1, called the design of all four iterations
> complete, and stated that once its one mechanical P2 was written in — it is — the spec needs no
> seventh round. The owner also ruled that the H-13 canary fix ships inside iteration 2, not as a
> separate hotfix. Every finding of every round was re-verified against the code and is dispositioned
> in §9. The reviewer confirmed both of the author's refusals (H-4: a request with neither `Origin`
> nor `Sec-Fetch-Site`; H-1: no blanket rule on `scp`) and accepted all three decision amendments.
> Round 3 found no P0; one of its P1s is a defect **live on `main`** in the canary prober (H-13).
>
> Origin: an external whole-tree review delivered to the owner on 2026-11-13; every finding was
> re-verified by reading the code before it was admitted. By the precedent of
> [`func-hardening.md`](func-hardening.md) and the audit-gap packages this package carries **no
> requirement number**.
>
> **SPA surface.** Six deltas to existing screens: the OIDC settings form and a `kind` badge in the
> member lists (H-1, iteration 1); the WebSocket TLS control in the monitor editor (H-2, iteration
> 2); the status-page list's links, `tokenActions.ts`, and `rel` on the support link in
> `PublicStatusView.vue`/`LoginView.vue` (H-3, H-9a, iteration 3); `index.html` and the logout call
> in `session.ts` (H-9b, iteration 4). By CLAUDE.md a mock delta is approved before the frontend
> code of its iteration.
>
> Ordering (author's proposal, endorsed by the review): this package ships **before** FR-042
> ([`func-incident-impact.md`](func-incident-impact.md)) and FR-041
> ([`func-phone-alerts.md`](func-phone-alerts.md)). FR-041 adds unauthenticated inbound endpoints and
> a phone-number store; FR-042 adds a new subscriber mail (its D5) that H-14 must precede.

## 1. Findings, ranked

| # | Sev | Finding | Where | Amends |
| --- | --- | --- | --- | --- |
| H-1 | P0 | A bearer JWT minted by the realm for ANY client authenticates as the human whose `sub` it carries | `internal/auth/auth.go:227`, `internal/auth/middleware.go` (pre-iter-0209 `principalFromJWT`, now `internal/auth/machine.go`), `internal/store/users.go:35-67` | D-0034 |
| H-2 | P0 | The WebSocket prober writes an unvalidated target into a raw socket and accepts any TLS certificate; the RabbitMQ management URL is built by concatenation from `config.path` | pre-iter-0210 `wsTarget` and the concatenated handshake in `internal/prober/websocket.go`, `rabbitMgmtURL` in `internal/prober/rabbitmq.go`; the userinfo check at `internal/domain/monitor.go:360`; the parsers are now in `internal/domain/targets.go` | — |
| H-3 | P1 | Reads gated by `InOrg` alone ignore a token's action allow-list and return `unlisted_token` to every reader | `internal/api/handlers_statuspage.go:58-69`, `internal/api/handlers_feed.go:19-25`, `internal/authz/authz.go:273-283`, `internal/api/handlers.go:55-90` | D-0139 preamble |
| H-4 | P1 | No request-forgery defence beyond `SameSite=Lax`; a state-changing `GET /auth/logout` | `internal/auth/auth.go:374-384`, `internal/api/handlers.go:603-628`, `internal/auth/handlers.go:31-41` | — |
| H-5 | P1 | A password reset does not revoke the account's sessions | `internal/store/users_reset.go`, `internal/auth/handlers_reset.go:76-126` | D-0139 item 3 |
| H-6 | P1 | Unbounded bodies and passwords reach Argon2id; `reset/confirm` hashes before any limiter or token check | `internal/auth/local.go:57-59`, `internal/auth/handlers_reset.go:78-110`, `internal/auth/ratelimit.go` | — |
| H-13 | P1 | A credential header follows a redirect to another host: always for the shared prober client (synthetic `{{secret:…}}` headers), and on any two-hop chain for the canary prober, whose guard compares with the previous hop while `net/http` restores headers from the first | `internal/prober/prober.go:63-68`, `internal/prober/synthetic.go:68-72`, `internal/prober/canary.go:196-207` | D-0218 (D3c1: three points, listed in H-13) |
| H-9a | P1 | The unlisted page's `?token=` leaks through `Referer` to any external link (no `Referrer-Policy`) | `internal/web/web.go:28-55`, `frontend/src/views/PublicStatusView.vue` | — |
| H-7 | P2 | A TOTP code is accepted again inside its ±1-step window | `internal/totp/totp.go:60-73`, `internal/store/users_totp.go` | — |
| H-8 | P2 | No `IdleTimeout`/`ReadHeaderTimeout`; three probers read from a peer with no length bound | pre-iter-0212 `http.Server` literal in `httpsrv.New` (now `newHTTPServer`, `internal/httpsrv/server.go:86-94`); `internal/prober/ssh.go:54`, `internal/prober/redis.go:62`, `internal/prober/websocket.go:95` (the reads, now bounded) | — |
| H-9b | P2 | No `X-Content-Type-Options`, `X-Frame-Options` or CSP on any surface | `internal/web/web.go:28-55`, `frontend/index.html` | — |
| H-10 | P2 | `branding.support_url` accepts any string and is rendered as `<a :href>` | `internal/domain/settings.go:35-53`, `frontend/src/views/LoginView.vue` | — |
| H-11 | P2 | `/metrics` and the raw `readyz` error text are unauthenticated on the application port | pre-iter-0212 `readyz` and the single mux in `httpsrv.New` (now `internal/httpsrv/server.go:144`) | — |
| H-12 | P2 | `/readyz` ignores `brokerUp` although the registry tracks it | `internal/metrics/metrics.go:900-905` | — |
| H-14 | P2 | Subscribers of a page later made `internal` keep receiving incident mail | `internal/store/subscribers.go:96-103` (no visibility predicate) | — |

Severity: P0 — an authentication or tenant boundary is crossed, or arbitrary bytes reach the private
network; P1 — a realistic attacker gains a capability they do not hold; P2 — defence in depth.

## 2. The P0s

### H-1 — the client-credentials verifier authenticates humans

**What D-0034 decided.** `ccVerifier` relaxes the audience check so a service-account token (whose
`aud` is its own client) can call the API; safety came from "a service-account client with no cerbix
memberships gets a principal with no access".

**The new argument (accepted by review).** `principalFromJWT` provisions through
`UpsertUserByOIDCIdentity(issuer, sub, …)` — the identity table the interactive SSO login writes. A
realm user's `sub` is the same in every client's token, so a token any other application obtained for
human X is accepted as X, with X's memberships. D-0043 later opened OIDC to shared and public IdPs,
which widens it from "another app in our realm" to "any site using the same IdP".

**Why revision 1's fix was not enough (review P0-1, P0-2).** It accepted `azp == client_id`, which is
true of every *interactive* cerbix token; it let the bearer path create the row first and mark it
`machine`, so a human not yet signed in could be pre-claimed; it applied the email allowlist to
machine tokens, which carry no verified email; it put the new setting in YAML although a saved
`oidc_settings` row replaces the YAML entirely (`resolveEffective`, `internal/auth/auth.go:178-196`);
and it shipped no migration for the column it relied on.

**Fix.**
1. **An explicit list of machine clients.** OIDC settings gain `machine_clients` — client ids whose
   tokens the bearer path accepts — in `oidc_settings`, the settings API, the OIDC form, and the YAML
   bootstrap (same DB → config resolution as every other OIDC field). Validation refuses cerbix's
   own interactive `client_id` in it. Default: empty, which disables the JWT bearer path.
2. **Which claim names the client.** The first present of `azp`, `client_id`, `cid` (Okta), `appid`
   (Entra ID v1) — `azp`/`client_id` alone would answer 401 to every Okta and Entra-v1 machine token.
   **All non-empty claims of that set must be equal**, else 401: "first present wins" would let one
   claim shadow another. The claim that matched is logged.
3. **Delegated (user) tokens are refused even from a listed client.** A listed client that also
   allows a user flow (authorization code, password grant, token exchange) mints tokens with the same
   `azp` and a human's `sub`. The bearer path refuses a token that carries a non-empty `nonce` or
   `sid`, or (Okta) a `cid` different from its `sub`. The review also proposed refusing any `scp`;
   that is **not** adopted as a rule, because whether `scp` appears on client-credentials tokens
   differs by provider — the builder confirms each provider's token shape against its documentation
   and records it in the iteration report. These markers narrow the hole; what **closes** it is rule
   5, and the runbook says a listed client must be service-account-only at the IdP.
4. **`users.kind`, `human` | `machine`.** Migration in iteration 1: `machine` where
   `oidc_sub IS NOT NULL AND last_seen_at IS NULL AND password_hash IS NULL AND NOT is_global_admin
   AND email LIKE '%@clients'` — the synthetic address `principalFromJWT` gives a token without an
   email (`internal/auth/machine.go:127-130` since iter-0209); everything else `human`. The narrower rule is
   deliberate: migration 00111's own comment says a human whose sessions were purged before it has a
   NULL stamp, and revision 2's wider rule would have filed dormant human admins as machines, with
   their rights, on the bearer path.
5. **Two store methods, so a refusal writes nothing.** Today both paths call one upsert that
   overwrites `email`/`display_name` on conflict before any check could run.
   - Bearer: `UpsertMachineUserByOIDCIdentity` — `INSERT … 'machine' ON CONFLICT (oidc_issuer,
     oidc_sub) DO UPDATE … WHERE users.kind = 'machine'`. A `human` row returns no row → the
     transaction is **rolled back** → **401**, no mutation. It never applies the bootstrap-admin
     grant. `principalFromJWT` then checks `kind = 'machine'` again and answers 401 otherwise — a
     concurrent sign-in may have flipped the row. **The `kind` it checks must be read no earlier
     than the memberships.** `loadPrincipal` makes two autocommit reads, `GetUser` then
     `ListMembershipsForUser` (`internal/auth/auth.go:326-333`); a `kind` taken from the first could
     be `machine` while the second already returns the memberships granted to the human a moment
     later. Because machine → human is irreversible, `kind = 'machine'` read *after* the memberships
     proves they were a machine's. So: re-read the user after `loadPrincipal`, or load both in one
     statement.
   - Interactive: `UpsertHumanUserByOIDCIdentity` on `/auth/callback` reads the existing row
     `FOR UPDATE` (the old `kind` must be known: `INSERT … ON CONFLICT … RETURNING` yields only the
     new one), then sets `kind = 'human'`, permanently. If the row was `machine`, in the same
     transaction **all its memberships are deleted and `is_global_admin` is set to false** — the
     bootstrap-admin rule (`internal/auth/handlers.go:204-212`) then re-grants it only on a verified
     bootstrap email — with a security log line and an audit row. A person must never inherit what
     was granted to something badged as a machine.
     **When that `FOR UPDATE` read finds no row it locks nothing**, and a bearer request may insert
     a `machine` row before the next statement. So the insert is `INSERT … 'human' ON CONFLICT
     (oidc_issuer, oidc_sub) WHERE oidc_sub IS NOT NULL DO NOTHING RETURNING …`; an empty result
     means a concurrent transaction created the row, and the method repeats the `FOR UPDATE` branch
     in the same transaction — which then sees `machine` and strips. An `ON CONFLICT DO UPDATE SET
     kind = 'human'` there would flip the row while skipping the strip and the audit row.
   - **Both** keep the legacy claim of a pre-issuer row (`oidc_issuer IS NULL`,
     `internal/store/users.go:44-48`) inside their transaction. The unique index treats NULL issuers
     as distinct, so a bearer path without it would insert a second, `machine`, row beside a legacy
     human; with it, the legacy row is claimed, found `human`, and the rollback leaves it untouched.
   - `PATCH /api/v1/admin/users/{id}` refuses `is_global_admin = true` for a `machine` user
     (`internal/api/handlers_users.go`). A machine identity that needs instance-wide rights does not
     exist in this model; an API token does that job.
6. **The email allowlist stays an interactive-login rule.**
7. **Upgrade.** Breaking for instances using client-credentials (accepted by the owner, §8): with
   `machine_clients` empty they get 401. CHANGELOG upgrade note in iteration 1; every refusal logs
   `oidc_bearer_refused` with the client claim found and a reason code;
   `cerbix_oidc_bearer_refused_total{reason}`. The note also covers the one mis-filing the narrow
   backfill leaves: a service account whose token carries a **real** email (a GCP service account)
   is filed `human` and then refused with `reason=subject_is_human`; the operator deletes that user
   (Settings → Users) and the next token re-creates it as `machine` — its memberships must be
   granted again.

8. **A token never mints a token (added after the iteration-1 code review; ships in iteration 2).
   OWNER, 2026-11-14.** `createApiToken` and the token delete do not look at `ViaToken`, so a
   `machine` identity holding `org_admin` can issue a `cbx_` token. `stripMachineGrants` removes
   memberships, not `api_tokens`, so that token outlives the flip to `human` and is then listed
   under the person's email. Of the two repairs — revoke on the flip, or refuse the issue — the
   owner chose the refusal: creating and deleting an API token answer **403** to any principal
   with `ViaToken` (an OIDC machine JWT or a `cbx_` token), the rule project membership management
   already follows (`internal/api/handlers_projectmembers.go:28`). Listing is unchanged. Breaking
   for automation that issues tokens with a token: CHANGELOG upgrade note. The same iteration
   corrects the wording that sent operators the wrong way — the `Grant admin` tooltip in
   `UsersPanel.vue` and the runbook say "use an API token" for instance-wide automation, but an API
   token is bound to one organization and never carries global admin; the text becomes "global
   administration is for people; automation gets organization or project rights".

**Residual, stated.** A listed client that can still mint a token with a human's `sub` and no
session marker, before that human's first sign-in, creates a `machine` row under that `sub`. An
administrator may grant it membership, seeing the `machine` badge. When the human signs in, rule 5
strips those memberships — so the person gains nothing, and the machine integration loses its grants,
visibly, in the audit log. That is the intended failure: loud, and on the side of the boundary.

**Invariant S1.** A realm JWT is accepted on the bearer path only if its client claim is a listed
machine client, it carries no delegated-session marker, and its subject is not a `human` user; every
other JWT is 401 and changes no row. A `machine` row that becomes `human` keeps no membership and no
global-admin flag. No `machine` user is ever a global admin. No principal authenticated by a token
creates or deletes an API token (from iteration 2).

**Test.** Hermetic mock-OIDC (D-0034's harness): foreign-client token with a human `sub` → 401;
cerbix's own interactive token as bearer → 401; listed client → principal; listed client + `nonce`
or `sid` → 401; `cid`/`appid`-only tokens accepted when listed; bearer against a `human` row → 401
and the row byte-identical; machine row with memberships, then SSO → `human`, zero memberships, audit
row; a machine row holding `is_global_admin` (set directly in the fixture) → cleared on the flip;
`PATCH` refusing the grant; a legacy NULL-issuer human row + bearer → 401, one row, unchanged;
tokens whose client claims disagree → 401; concurrent bearer and callback for one `sub`, both
when the row exists and when neither has created it yet (the first-insert race), asserting that no
interleaving yields a bearer principal with a human's memberships or a flipped row that kept a
machine's; DB-saved OIDC settings honour `machine_clients`;
the backfill table (dormant human admin stays `human`) on both storage modes.

### H-2 — probe input reaches the socket

**Fact.** `wsTarget` splits by prefix and first `/`; the handshake is concatenated and written.
`Monitor.Validate` rejects only URL userinfo, and only when `url.Parse` succeeds, so a target with
`\r\n` passes; `POST …/monitors/test` reaches the prober without saving. TLS is
`InsecureSkipVerify: true` unconditionally. `rabbitMgmtURL` builds `scheme://host` + `config.path`
with no leading-slash or control-character rule, so `path = "@10.0.0.9:8080/x"` moves the request —
and its Basic-Auth credentials — to another host.

**Fix.**
1. `websocket`: `url.Parse`, scheme in {`ws`,`wss`,`http`,`https`}, request built with
   `http.NewRequest` + `req.Write(conn)`.
2. Socket-written targets (`ssh`, `redis`, `rabbitmq` AMQP mode): `net.SplitHostPort`, non-empty
   host, numeric port, no whitespace or control bytes. `rabbitmq` management mode: `path` must start
   with `/` and contain no control byte, `@`, or `?`-less authority trick; the URL is assembled with
   `url.URL{…}`, not concatenation.
   - **Amended 2026-11-14 (OWNER, iter-0210 ruling 1): the port is optional.** A socket target is
     `host` or `host:port`, because these types default the port and the UI documents that. When a
     port is written it is numeric, 1–65535. Requiring one would have refused every bare-host
     monitor.
   - **Amended 2026-11-14 (external review of iter-0210):**
     - The management `path` starts with a single `/` in the prober too. A relative path is refused,
       not silently prefixed; migration `00114` writes the `/` into existing rows.
     - The management target has ONE parser, `domain.ParseRabbitMgmtTarget`, for the write surfaces
       and the prober. Its rules: control bytes judged on the raw value; `http`/`https`
       case-insensitive; a host; a written port 1–65535; no userinfo, query or fragment;
       `tls: "true"` demands `https`.
     - The path keeps its percent-encoding byte for byte (`%2F` is RabbitMQ's default vhost).
3. **TLS option, under the name the tree already uses: `tls_skip_verify`** (`redis`, `mysql`,
   `rabbitmq`, `internal/domain/credsettings.go`, `NewMonitorView.vue`). Default `false`. It is
   registered as a **plain typed setting** for `MonitorWebSocket` in `PrepareTypedSettings`
   (`internal/domain/typedsettings.go`) — not in the credential schemas, or
   `RequiresExecutionEnvelope` would demand a credential envelope for every WebSocket monitor — so
   the API and Monitoring-as-Code both accept it. `internal/api/handlers_monitors.go` calls
   `PrepareCredentialSettings` only for credentialed types today (create, test, update); it switches
   to `PrepareTypedSettings` so the API validates the key as strictly as the file provider does.
4. **Migration:** `tls_skip_verify = "true"` only for existing `websocket` monitors whose target
   starts `wss://` or `https://`, so none flips DOWN on upgrade; plaintext ones are untouched. The
   CHANGELOG note tells MaC users to add the key to their bundle, or the next apply resets it.
   - **Amended 2026-11-14 (OWNER, iter-0210 ruling 2): `ws://` for a target without a scheme.** The
     old prober dialled such a target as `ws://`. Migration `00113` writes that scheme into the
     existing rows, and every new write requires a scheme.
5. **Added 2026-11-14 (external review of iter-0210): every other target.**
   - A control byte in the raw target is refused for every type that probes one.
   - `http` and `promql` targets must parse (a parse failure is a refusal, not a skipped userinfo
     check), with an `http`/`https` scheme and a host. A `promql` base URL carries no query or
     fragment.
   - `promqlProber` re-parses its target and builds `/api/v1/query` as a `url.URL`, keeping the
     base path.
   - The userinfo rule of D-0145 stays a write rule, so a legacy row keeps probing until its next
     edit.
   - Migration `00115` trims ASCII whitespace from the edges of the targets whose old prober
     trimmed them (`websocket`, `ssh`, `redis`, `rabbitmq`, `postgres`, `mysql`), so none flips
     DOWN or answers 400.
   - **Amended 2026-11-14 (second external review of iter-0210):**
     - The `http` prober parses its target before any request.
     - A written port must be 1–65535 for `http` and `promql`.
     - No probe message carries a request URL.
     - `00113` trims what its prober trimmed. `00115` repairs targets the first `00113`
       doubled, and removes control bytes from an `http` fragment.
     - A base path ending in an escaped slash keeps it.

**Invariant S2.** No prober writes a byte of `monitor.target` or `monitor.config` to a socket or into
a URL that did not pass through that protocol's parser; a hostile value is refused at all three write
surfaces — API create/update, test-connection, MaC decode — and again by the prober before it dials.

**Test.** Hostile-input table per prober × the three surfaces (`handlers_monitors.go` create/update
and test, `fileprovider` decode) → refused; forced past validation → prober errors before dialing;
`wss://` self-signed fixture DOWN by default, UP with `tls_skip_verify`; MaC round-trip of the key.

## 3. The P1s

### H-3 — reads behind `InOrg`

D-0139 (its preamble; iter-0081 item 2) settled that a status page is an organization-level aggregation. That
stands. Two boundaries created since then are not honoured by `InOrg`: action-scoped API tokens
(D-0212) and project-only roles (D-0273).

**Fix.**
1. **The D-0212 two-step, unchanged:** `!p.InOrg(org)` → **404**; in the org but without the action →
   **403**. (Revision 1 said 404 for both, contradicting D-0212's "visibility is membership".)
2. New `ActionStatusPageRead`, held by every human role — org-level and project-level — and by a
   token only when listed; added to the catalogue, `TOKEN_ACTIONS` and `ROLE_GRANTS`
   (`frontend/src/lib/tokenActions.ts`).
3. **Project-only members DO read the organization's status pages. OWNER, 2026-11-13.** The page is
   an org-level aggregation (D-0139) and membership of any project of the org is membership of the
   tenant. `Principal.Can(action, orgID, "")` rejects a project-scoped membership today
   (`internal/authz/authz.go:216-244`), so the status-page read check is "the principal holds
   `ActionStatusPageRead` through ANY membership in this org, org-level or project-level, and its
   token allow-list (if any) lists it" — a dedicated predicate with its own test, not a call with an
   empty project id. `NavigationLinks.vue` is unchanged. What H-3 removes is therefore exactly two
   things: the action-scoped token that never listed the action, and the unlisted token in clear.
4. `unlisted_token` is returned only to `ActionOrgManage`; others get `""` plus
   `has_unlisted_token: true`. `StatusPagesView.vue` then opens an unlisted page through
   `?preview=<pageID>` and the authenticated feed route, as it already does for `internal` pages —
   otherwise its links 404.
5. **Adjacent reads — one contract per route.** Revision 2 said "the two-step with their read
   actions", which would have answered 403 to every project-only member on the two calls the SPA makes
   at start (`frontend/src/stores/workspace.ts:51` and `:182`): `RoleProjectAdmin` holds no
   `ActionOrgRead`, and `Can(action, org, "")` skips project-scoped memberships
   (`internal/authz/authz.go:229-241`).
   - `getProject`: `!VisibleProject` → 404; `!Can(ActionProjectRead, org, project)` → 403.
   - `listProjects`: `!InOrg` → 404; a principal holding `ActionProjectRead` through no membership of
     the org (the any-membership predicate of fix 3) → 403; the list is filtered by
     `Can(ActionProjectRead, org, project.ID)`.
   - `getOrganization`: `!InOrg` → 404; any **human** member reads the organization's metadata, as
     today; a token needs `org:read` or `project:read` in its allow-list, else 403.
   - `listOrganizations`: a token gets its own organization by id — never its synthetic actor id in a
     `uuid` comparison (500 today) — under the same allow-list rule, else an empty list.
   - `GET /api/v1/events` and `GET /api/v1/me` have the same two defects (a token's synthetic actor
     id in a `uuid` comparison → 500 on PostgreSQL, masked by the fakes; no allow-list check):
     `events` resolves its project set through `VisibleScope(ActionProjectRead)` and answers 403 to a
     token without `project:read`; `me` answers 403 to a token principal — it describes a user.
   - Three reads are tenant-free by design and stay open to any authenticated principal:
     `GET /api/v1/version`, `/api/v1/regions`, `/api/v1/monitor-defaults`. They are the **entire**
     allow-list of the route-enumeration test.
   Verified: `cerbix gate check` and `cerbix change record` take ids and make one POST each
   (`internal/cli/gate.go`, `internal/cli/change.go`); a `[gate:evaluate]` token calls none of these.

**Invariant S3.** No read handler lets an action-scoped token read what its allow-list omits; a
principal lacking the read action gets 403 inside its org and 404 outside; every human member of any
project still loads the SPA workspace (a test signs in as a project-only admin, editor and viewer);
the route-enumeration test runs against PostgreSQL, not the fakes, and no route answers 500; no principal without `ActionOrgManage` receives an unlisted
token. A route-enumeration test asserts the first clause.

### H-4 — request forgery

**Fact.** `SameSite=Lax` blocks cross-*site* POSTs, so the exposure is a sibling host
(`wiki.company.com` → `cerbix.company.com`). Nothing inspects `Origin`/`Sec-Fetch-Site`;
`decodeJSON` ignores the content type; `GET /auth/logout` ends the session.

**Fix.**
1. `http.CrossOriginProtection` (standard library since Go 1.25; the module floor is 1.26) in front of
   the session-authenticated router. It rejects unsafe methods that are not same-origin by
   `Sec-Fetch-Site`, and otherwise compares `Origin`'s host with `Host` — **no scheme comparison**, so
   it is correct behind a TLS-terminating proxy, where revision 1's "scheme+host+port" rule would
   have refused every mutation. Bypass patterns: the public router (status pages, push ingest), the
   agent router, and FR-041's future telephony router. It is called through a thin wrapper
   (`Check` + `SetDenyHandler`) that answers a refusal with the API's JSON error shape, not the
   library's `text/plain`, and that **skips a request to `/api/*` carrying a bearer credential** — an API
   token or JWT is not an ambient credential.
   That skip is only sound with a change to `RequireAuth`, which revision 3 wrongly said was already
   true: it tried the **session cookie first** and reached the bearer only afterwards (`RequireAuth`
   before iter-0211), so a request with a cookie *and* any `Authorization: Bearer x`
   would skip the forgery check and still run as the session. Revision 4 answered with "any
   `Authorization` header disables the cookie", which is wrong the other way: behind a reverse proxy
   that adds HTTP Basic (or Negotiate) authentication the browser sends `Authorization: Basic …`
   beside the session cookie on every SPA call and on the `EventSource`, and every one of them would
   be 401. The predicate is the **scheme**:
   - `hasBearerAuth(r)`: some value of `Authorization` begins with the scheme `Bearer`,
     case-insensitively.
   - `RequireAuth`: when it is true, **the cookie is never read**; there must be exactly one
     `Authorization` value and it must carry a valid non-empty token, else 401. When it is false —
     no header, or a proxy's `Basic`/`Negotiate`/anything else — only the session cookie
     authenticates.
   - The wrapper skips the forgery check only for `/api/*` requests with `hasBearerAuth(r)`. So a
     cookie beside a non-bearer `Authorization` is still checked, and `POST /auth/logout` — mounted
     on the application mux outside `RequireAuth`, reading the cookie itself
     (`internal/auth/handlers.go:224`) — is never skipped whatever it carries.
   The wrapper therefore sits in front of the whole application mux, `/auth/*` included, with the
   bypass patterns above.
2. `decodeJSON` requires `Content-Type: application/json` **when it decodes a body** → 415. Body-less
   mutations (`POST /auth/logout`, acknowledge, retire, every `DELETE`, the gate's empty-body POST)
   are unaffected — revision 1's S4 would have broken all of them.
3. `GET /auth/logout` is removed (the SPA and the E2E suite POST).

**A request with neither header is allowed — the one review point accepted in part (§9, P0-3).** The
review proposed 403 for it. Every browser that can run script sends `Origin` on a cross-origin
unsafe request, so its absence identifies a non-browser client, which holds no ambient cookie to
abuse; refusing it would break `curl`, the CLI, and Playwright's `page.request` — which is how the
shared E2E session calls the API. This is the standard library's documented stance and is adopted
as is.

**Invariant S4.** A browser-originated unsafe request that is not same-origin is refused before any
handler runs; a request that presents a `Bearer` credential is never authenticated by its
cookie, and a request authenticated by its cookie is never exempt from the check; a JSON body is decoded only under a JSON content type; no GET changes state.

**Test.** Table over (auth kind × `Sec-Fetch-Site` × `Origin` × proxy `Host` × body/no body) on one
mutating route and one body-less route, including bearer + a foreign `Origin` → accepted, the 403 body being JSON, cookie + `Authorization: Bearer x` (junk, empty or duplicated) + a foreign `Origin` → 401, never
the session; cookie + `Authorization: Basic …` same-origin → 200 as the session (the proxy case) and
with a foreign `Origin` → 403; `POST /auth/logout` with a cookie, a bearer and a foreign `Origin` →
403, session intact; `GET /auth/logout` → 405; `make dev-test` green unchanged.

### H-5 — a password reset keeps sessions alive (amends D-0139 item 3)

D-0139 left the reset path non-invalidating **on purpose**: the first cut of iter-0081 revoked on
both change and reset and broke `e2e/tests/mail.spec.ts`, which resets the shared admin and restores
the password "through the still-valid admin session". It recorded the question as "a separate
product/UX decision". This is that decision, and the argument is the threat model: a reset is what an
account owner does when the password is believed compromised — the one moment existing sessions most
need to die.

**Fix.**
1. `ResetPasswordWithToken` deletes **all** of the user's sessions in its transaction.
2. `changePassword` keeps D-0139's behaviour (other sessions revoked, the caller's kept) but
   `SetPassword` + `DeleteSessionsByUser(userID, exceptToken)` become one transaction; today the
   delete's error is swallowed.
3. `mail.spec.ts` resets a dedicated `e2e-` user, never the shared admin — in the same iteration, or
   `make dev-test` fails.
4. API tokens are **not** revoked by a password change or reset: they are org-scoped credentials with
   their own lifecycle. Stated so nobody assumes otherwise.

(Revision 1 extended the rule to "an admin-initiated password set and disabling a user". Neither
exists — `internal/api/handlers_users.go` has only `SetGlobalAdmin` and `DeleteUser`. Removed.)

**Scope, verified.** A row never holds both a local password and an OIDC identity: the only
`CreateLocalUser` call is the bootstrap administrator (`internal/auth/local.go:143`);
`UpsertUserByOIDCIdentity` keys on `(issuer, sub)` and never matches by email, so a person with the
bootstrap address who signs in through SSO gets a second, separate row; and the two password writers
both require an existing hash (the reset token is issued only for `password_hash IS NOT NULL`,
`changePassword` answers "not a local account"). So H-5 revokes the sessions of a local account only
and can never end an SSO session — but that local account is the bootstrap global admin.

**Invariant S5.** After a reset, every session created before it is `ErrNotFound`; after a change,
every session but the caller's is, atomically with the new hash. Tested in the store (both paths,
including a forced failure between hash and delete) and in E2E on the dedicated user.

### H-6 — bodies, passwords and limiters

**Fix.**
1. **Length caps where the cost is:** `auth.HashPassword` and `auth.VerifyPassword` refuse a password
   over 1024 bytes. One rule covers login, reset, `POST /api/v1/me/password` and TOTP disable.
2. `MaxBytesReader` 4 KiB on the `/auth/local/*` handlers; the API router gets a default body cap of
   1 MiB (handlers with their own smaller cap keep it).
3. **Limiters, split** (revision 1's single "IP and email" key allowed either an account-lockout DoS
   or no protection, depending on how it was read):
   - per-IP, checked **before** the body is read, on `login`, `reset/request` and `reset/confirm`
     (the last has no limiter today and runs Argon2id — 64 MiB — before it looks at the token);
   - per-(IP, normalized email) after decode;
   - per-email counting **failures only**, at a threshold several times the per-IP one, so noise from
     elsewhere cannot lock a real user out of a clean address.
   A shared limiter across replicas stays a non-goal (§6).

**Invariant S6.** No endpoint hashes a password longer than 1024 bytes; no unauthenticated endpoint
reads more than 4 KiB or reaches Argon2id without passing the per-IP limiter; failed attempts against
one email from other addresses do not block a correct login from a clean address — a limiter test
drives exactly that: N failures for one email from address A, then a correct login from address B
succeeds.

### H-13 — credentials follow redirects (promoted from revision 1's "Parked"; live on `main`)

Verified: `NewRunnerWithGuard` builds `&http.Client{Transport: transport}` with the default redirect
policy (it does not use `Guard.HTTPClient`), and `syntheticProber` substitutes project secrets into
arbitrary step headers. `net/http` drops `Authorization` and `Cookie` on a host change and nothing
else, and does not treat an `https → http` downgrade on the same host as a change.

**The canary prober's existing guard does not hold either, and revision 3 was wrong to cite it as the
model.** `internal/prober/canary.go:196-207` compares each hop with the **previous** one
(`via[len(via)-1]`). But `net/http` re-copies the *original* request's headers onto every new hop
before calling `CheckRedirect` (`makeHeadersCopier`). So on `victim → evil/1 → evil/2` the header is
deleted on hop 1, restored by the library on hop 2, and kept — hop 2 has the same origin as hop 1.
The canary's binding-backed credential reaches `evil/2` today.

**Two more carriers, found in round 4.** Headers are not the only thing the library re-sends.
- **The body.** On 307/308 `net/http` rebuilds the request body from `GetBody` before it calls
  `CheckRedirect` (`/usr/local/go/src/net/http/client.go:686-694`), and `strings.NewReader` /
  `bytes.NewReader` bodies always have one. A synthetic step may put `{{secret:<binding>}}` in its
  body (`internal/domain/syntheticbindings.go:177-186`) and a canary a `secret_ref` value in its
  submit body or multipart fields — a 307 to another host forwards the materialized secret whole.
- **`Referer`.** The library sets it to the previous hop's full URL, query string included, after
  copying headers (`/usr/local/go/src/net/http/client.go:708-710`). The caller never supplied it, so
  a rule about caller-supplied headers does not see it; a target URL with a token in its query
  reaches the redirect's host.

**Fix.** One **predicate** — "has any hop of this chain, the current one included, left the origin of
the first request (`via[0]`)?" — shared by the Runner's client (http, promql, rabbitmq management,
synthetic) and the canary prober. Origin is lower-cased host **and port**; for the Runner the one
strict upgrade `http://host(:80) → https://host(:443)` of the first request does not count as
leaving (`http://` targets are legal there and an ingress answering 301 to `https://` is ordinary;
dropping Basic Auth on it would turn working monitors DOWN). It is evaluated against `via[0]`, never
the previous hop, so `evil → evil` and a return to the original host stay "left".

What each client does with it differs, because the canary's contract (FR-029, D-0218) is stricter
and stays so:

| | Runner's client | Canary prober |
| --- | --- | --- |
| Hop limit | the library's 10 | `canaryMaxRedirects = 3`, unchanged |
| Hop refused outright | scheme not `http`/`https`; userinfo in the URL (the transport would turn it into `Authorization: Basic`); `https → http` | `CanaryHopViolation` on every hop, unchanged: not `https`, no host, userinfo. No `http → https` exception — `http` is never legal for a canary |
| Once the chain has left | every header the first request carried is removed | the binding-backed set (`canaryBindingBacked`) is removed; ordinary document headers (`x-tenant`) and the runner's own are kept, as D3c1 says |
| Library-set `Referer` | removed on **every** hop — a prober has no use for it | same |
| 307/308 carrying a body, chain has left | the hop is **refused** for **any** non-empty body | the hop is **refused** when the body is **secret-bearing**; other bodies are forwarded |

- **Why the body rule differs.** Revision 5 used the narrow rule for both clients and said the
  synthetic prober would recognise a `{{secret:…}}` placeholder in a step body. It cannot:
  `CredentialKeyring.materialize` substitutes the value into the scenario **before** the prober runs
  (`internal/dispatch/credentials.go:643-650`), so the prober sees plain text — the rule would have
  passed a unit test and done nothing on a real worker. And a step body may carry a `{{var}}`
  extracted from an earlier response (`internal/prober/synthetic.go:89-95`): a session or refresh
  token obtained with the secret, with no placeholder to mark it. So the Runner's client refuses a
  cross-origin 307/308 with any body. Of its types only `synthetic` sends one (the `http` monitor
  sends none, `internal/prober/prober.go:196`), and a scenario step that must POST to another
  origin names that origin in its own `url`.
- **The canary keeps the narrow rule**, because there the fact is known where the request is built:
  the stored document holds a marker and the executor resolves it. `canaryRenderValue` marks the
  request secret-bearing whenever it resolves a `CanaryValueSecret`, **recursively** — the value may
  sit inside a list or an object — and the multipart field renderer does the same. A body without a
  secret is forwarded as today: an upload redirected with 307 to a storage host is a legitimate
  canary flow. The hop is refused, not sent body-less — a canary that succeeds without what it was
  told to send is a false UP, the reasoning `CanaryHopViolation` already records.
- **Amendments to D-0218 / `func-async-canary.md` (D3c1), three, all written into both documents in
  iteration 2** — that spec says redirects are "governed by D3c1 and only by D3c1", so nothing may
  be left implied: (a) the origin compared is the first request's, and leaving it is sticky, instead
  of a comparison with the previous hop; (b) a new hop refusal — a cross-origin 307/308 whose body
  or multipart fields resolved a `secret_ref`; (c) the library's `Referer` is removed on every hop.
  The stripped header set, the hop limit and `CanaryHopViolation` are untouched.
- **Two existing tests change:** `TestCanaryDropsEveryBindingHeaderOnACrossHostRedirect` and
  `TestCanaryProvenanceNeverReachesEitherTargetOnTheWire`. Both build their monitor with the
  `canaryMonitor` helper, whose default submit body contains a `secret_ref`
  (`internal/prober/canary_test.go:63`), and then follow a cross-host 307 — they have been sending
  that secret to the second host all along. Both are about headers: each mutate callback drops the
  `token` field from the body, and the secret-in-body case becomes a new test that expects the
  refusal. The other three redirect tests (same-origin, scheme downgrade, userinfo) are untouched.
- The new refusal's reason starts with `redirect refused:` and contains no `://`, like
  `CanaryHopViolation`'s — `failureClass` (`internal/prober/failure.go:112`) drops any text with a
  URL in it, and the operator would see an unclassified failure.
- `rabbitmqProber` builds a second `http.Client` when `tls_skip_verify` is set
  (`internal/prober/rabbitmq.go:91-100` since iteration 2, which made it copy `CheckRedirect`) and copies only `Transport` and `Timeout`; it must copy
  `CheckRedirect`, or that branch escapes the rule entirely.

**Invariant S13.** On any hop of any chain: no header the first request carried (canary: no
binding-backed header), no request body (canary: no secret-bearing body) and no `Referer` is sent to an origin — host **or
port** — other than the first request's, save the Runner's strict `http(:80) → https(:443)` upgrade;
nothing is sent over a weaker scheme; every HTTP client a prober constructs carries the rule; the
canary's FR-029 hop rules hold unchanged.

**Test.** Chains `A → B`, `A → B/1 → B/2`, `A → B → A`, `A → A:other-port`, `http://A → https://A`,
`https://A → http://A`, a `Location` with userinfo, a non-http scheme, against each prober and the
RabbitMQ `tls_skip_verify` branch; canary: a submit with a `secret_ref` in its body (top level, nested in a list, in a multipart field)
+ 307 to `B` → refused, the same without a secret → delivered; synthetic, **driven through
`dispatch.ValidateAndMaterialize`** so the substitution is the real one: a step body with a
`{{secret:…}}`, with an extracted `{{var}}`, and a static one + 307 to `B` → all refused, 307 within
`A` → delivered; a target with a query string + 302 to `B` → no `Referer` at `B`. The two-hop case
and the secret-body cases are RED on `main`; the three other existing canary redirect tests stay green
unmodified.

### H-9a — the unlisted token and `Referer`

The `?token=` contract **does not change** (revision 1 proposed moving it to a path or a cookie, which
would have 404'd every link already handed out and every RSS/Atom subscription — review P0-4).
Instead: `Referrer-Policy: no-referrer` on `/status/*` and `/api/v1/public/*`,
`strict-origin-when-cross-origin` elsewhere, and `rel="noopener noreferrer"` on the `support_url`
link. Ships in iteration 3 with H-3 — it protects the same capability token.

**Invariant S9a.** No request leaving a public status page carries a `Referer` — asserted on the
response headers of the page and feed routes, and in E2E on a click of the support link.

## 4. The P2s

- **H-7 TOTP replay.** Migration `users.totp_last_step bigint NULL` (TOTP state lives on `users`;
  there is no separate row), reset to NULL by `SetTOTPSecret`/`DisableTOTP`. `totp.Validate` returns
  the matched step; **login** acceptance is one compare-and-set —
  `UPDATE users SET totp_last_step = $2 WHERE id = $1 AND (totp_last_step IS NULL OR totp_last_step < $2)`
  — so two concurrent submissions of one code cannot both pass. `totpEnable` does **not** write the
  step: it is made single-use by `UPDATE … SET totp_enabled = true WHERE NOT totp_enabled`.
  Recording the step there would refuse the first sign-in made in the same 30 s — which is what
  `e2e/tests/timeflows.spec.ts` does, and what a person checking their new 2FA does.
  **S7:** a code authenticates at most once, proven by a parallel test; enabling 2FA and signing in
  with the same code both succeed.
- **H-8 timeouts and bounds.** `IdleTimeout: 60s`, `ReadHeaderTimeout: 5s` (the existing
  `ReadTimeout` is 15 s). `ssh` and `redis` read through `io.LimitReader(conn, 4 KiB)`; the WebSocket
  handshake response through `io.LimitReader(conn, 16 KiB)`. **S8:** every read from a probed peer
  is bounded — a fixture peer that streams without a newline ends the probe at the limit.
- **H-9b headers and CSP.** `X-Content-Type-Options: nosniff`; `X-Frame-Options: DENY`
  (`SAMEORIGIN` on public status pages). The pre-paint theme script moves out of
  `frontend/index.html` into a static file, because an inline script cannot pass `script-src 'self'`.
  Policy, in full: `default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline';
  img-src 'self' data: https:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'self';
  frame-ancestors 'none'` (`'self'` on public pages). `img-src` admits `https:` because `branding.logo_url` is a documented
  external URL (`frontend/src/components/BrandMark.vue`). `POST /auth/logout` answers **200 with
  `{"redirect_to": …}`** instead of a 302: `session.ts` calls it with `fetch`, which would follow a
  redirect to an external `post_logout_url` and be blocked by `connect-src 'self'`; the SPA then
  navigates with `window.location.assign`. Because that makes the value a DOM sink — a 302
  `Location` never executed `javascript:`, `assign` does — `post_logout_url` is validated on write
  (`setOIDCSettings`, which stores it unchecked today) **and** when served: a local path (`/…`, not
  `//…`, and containing no backslash or control byte — a browser resolves `/\evil.example` as
  another host) or `http(s)://` with a non-empty host; anything else is served as `/`. Report-only for one release, then enforced.
  **S9b:** `make dev-test` runs with zero CSP reports — the Playwright fixture subscribes to
  `securitypolicyviolation` on every page and fails the test on one; without that listener a
  report-only violation is invisible.
- **H-10 `support_url`.** `https`/`http` with a non-empty host, or `mailto:` with a non-empty address;
  anything else refused on write, and a stored value that fails the rule is served as `""` by the
  public branding read (validation on write does not clean what is already saved).
  **S10:** no `support_url` reaches a client unless it is `http(s)` with a host or `mailto:` with an
  address — tested on write and on a row seeded with `javascript:`.
- **H-11 operational endpoints.** With `server.ops_listen` set, `/metrics` **and** `/readyz` answer
  an explicit **404** on the application port (`/healthz` stays on both — it reveals nothing). They
  are registered as `http.NotFoundHandler()`, not merely left out: the application mux ends in the
  SPA catch-all (`internal/httpsrv/server.go:70-72`), which would answer `/readyz` with 200 and
  `index.html` while the database is down. `ops_listen` is set in the **production** config example
  only (bound to loopback or the internal network in the compose file); the dev stacks keep one port,
  so `wait_ready` in the `Makefile` is untouched. No container healthcheck is added: the `cerbix`
  service has none today and the image is distroless — no shell, `curl` or `wget` to run one.
  `config.Validate` refuses `ops_listen == listen`. `readyz`'s 503 body carries a stable reason
  code; the raw error goes to the log. **S11:** with `ops_listen` set, `/metrics` and `/readyz` answer
  404 on the application port (not the SPA's 200) and serve on the ops port; no 503 body contains
  driver or infrastructure error text.
- **H-12 readiness.** One predicate feeds `Ready()`, `LastError()` and the `cerbix_ready` gauge — they
  are three separate formulas today (`metrics.go:885-918`), and `pingDatabase` clears `lastError`,
  so a broker outage would answer 503 with an empty reason while the gauge stayed 1. **S12:** with the
  broker down an AMQP-role process answers 503 `broker_unreachable` and `cerbix_ready` is 0.
- **H-14 subscribers of a hidden page.** `ConfirmedSubscriberEmailsForProject` gains
  `sp.visibility IN ('public','unlisted')`. Must precede FR-042, whose D5 adds another subscriber
  mail. **S14:** a page made `internal` mails no subscriber — store test on both visibility transitions.

## 5. Iterations

| # | Scope | Migration | SPA | Notes |
| --- | --- | --- | --- | --- |
| 1 | H-1, H-5, H-6 | `users.kind` + backfill; `oidc_settings.machine_clients` | OIDC form field, `kind` badge in member lists (mock delta) | OpenAPI + `gen:api`; `mail.spec.ts` moved to a dedicated user; **CHANGELOG upgrade note** (machine clients) |
| 2 | H-2, H-13 (canary prober included), H-1 fix 8 | `tls_skip_verify` for existing `wss`/`https` WebSocket monitors, `ws://` for scheme-less ones (`00113`); the RabbitMQ management path's `/` (`00114`); edge whitespace of six types' targets (`00115`) | WebSocket TLS control (existing pattern); `Grant admin` tooltip wording | **CHANGELOG upgrade notes** (MaC bundles; a token no longer issues tokens) |
| 3 | H-3, H-4, H-9a, H-14 | — | status-page list links, `tokenActions.ts`, `rel` on the support link | OpenAPI (`has_unlisted_token`, 403/415), E2E on the live stack |
| 4 | H-7, H-8, H-9b, H-10, H-11, H-12 | `users.totp_last_step` | `index.html` theme script, logout call in `session.ts` | OpenAPI + `gen:api` (logout answers 200 JSON; `docs/runbook.md` and `sec-authn-authz.md` updated); prod config example (`ops_listen`); CSP enforced one release after report-only |

Every iteration: tests first (RED on `main`, GREEN on the branch); `-race` on **both storage modes**
for 1, 2 and 4 (each carries a migration); `make spa-snapshot` after any frontend change;
`make dev-test`; iteration report, decision record, traceability rows, CHANGELOG.

## 6. Non-goals

- The originating review's **scalability** findings — verified real, not security boundaries; a
  separate ops spec (not yet written) with load numbers behind it.
- **Prober depth** (gRPC TLS/mTLS, DNS record types, SSH beyond the banner).
- A **shared** login limiter across replicas.
- Rotating `unlisted_token` on membership change; changing the `?token=` URL contract.
- The non-atomic `depends_on` write in `createMonitor`/`updateMonitor`
  (`internal/api/handlers_monitors.go:448-455`): verified, a consistency defect, belongs to an
  audit-gap package.

## 7. Parked, with the review's ruling

- `secrets.dispatch_envelope` absent from `docker/config.prod.yaml` — **not a defect**: that file is
  `role=all`, whose dispatcher is in-process; no credential crosses a broker.
- The last-org-admin check (`internal/api/handlers.go:358-367`) reads then writes without a lock —
  **confirmed, left parked (P3)**: needs two org admins demoting each other in the same instant, and
  a global admin restores the role.

## 8. Owner decisions (2026-11-13)

1. H-3: a **project-only** member reads the organization's status pages — **yes** (H-3 fix 3).
2. H-1: the breaking default (`machine_clients` empty ⇒ JWT bearer path off) is **accepted**, with
   the upgrade note and the refusal log. Seeding the list from tokens seen before the upgrade was
   declined: it would keep the hole open for exactly the instances that have it.

## 9. Reviews — dispositions

### Review of revision 1

| Finding | Disposition |
| --- | --- |
| P0-1 pre-SSO claim of `kind = machine`; `azp == client_id` | **Accepted.** H-1 fix 1 (explicit machine clients, own client id refused) and 3 (sign-in sets `human`, permanently). The review's Keycloak-specific claim sniffing was not adopted: an explicit list works for every IdP. |
| P0-2 upgrade breakage, allowlist on machines, DB-saved OIDC | **Accepted.** Migration + backfill, allowlist stays interactive-only, setting lives in `oidc_settings`, upgrade note. |
| P0-3 S4 breaks body-less mutations and proxies | **Accepted**, except "403 when both headers are absent" — **declined**, reason in H-4. |
| P0-4 moving `?token=` breaks issued links and feeds | **Accepted.** Contract kept; `no-referrer` (H-9a). |
| P1-1 SPA links, navigation, 403 vs 404, adjacent reads | **Accepted.** H-3 fixes 1–5; the navigation needs no change because the owner ruled that project-only members read status pages (§8). |
| P1-2 `tls_insecure` name, MaC, RabbitMQ path, migration scope | **Accepted.** H-2 fixes 2–4. |
| P1-3 D-0139, non-existent admin operations, non-atomic change | **Accepted.** H-5 rewritten as an explicit amendment. |
| P1-4 redirect header leak is real | **Accepted.** Promoted to H-13. |
| P1-5 CSP vs the inline theme script, `img-src data:` | **Accepted.** H-9b. |
| P1-6 lockout DoS, `reset/confirm`, authenticated routes | **Accepted.** H-6 fixes 1–3. |
| P2-1 TOTP migration and race | **Accepted.** H-7. |
| P2-2 `Ready`/`LastError`/gauge drift, S11 scope | **Accepted.** H-11, H-12. |
| P2-3 `ReadHeaderTimeout` value, WebSocket read bound | **Accepted.** H-8. |
| P2-4 three write surfaces in the H-2 test; Referer priority | **Accepted.** S2's test; H-9a is P1 in iteration 3. |
| P2-5 subscribers of an `internal` page | **Accepted.** H-14. |
| P3-1 `support_url` host and read-side suppression | **Accepted.** H-10. |

### Review of revision 2

| Finding | Disposition |
| --- | --- |
| P0-1 H-3 fix 5 answers 403 to project-only members at SPA start | **Accepted.** One contract per route (H-3 fix 5); S3 gains the project-only sign-in test. |
| P1-1 membership inheritance on machine → human; backfill files dormant admins as machines; the shared upsert mutates before refusing | **Accepted.** Two store methods, memberships stripped on the flip, narrowed backfill (H-1 fixes 4–5). |
| P1-2 `azp`/`client_id` miss Okta and Entra v1; delegated tokens of dual-purpose clients | **Accepted**, except refusing every `scp` — **not adopted as a rule**: its presence on client-credentials tokens differs by provider; the builder verifies per provider (H-1 fix 3). |
| P1-3 `CrossOriginProtection` ignores `Authorization`; `text/plain` refusal | **Accepted.** Wrapper skips bearer, JSON deny handler (H-4 fix 1). |
| P1-4 same-host TLS upgrade drops headers; RabbitMQ's second client | **Accepted.** H-13. |
| P1-5 unregistered `/readyz` falls into the SPA catch-all; `wait_ready` | **Accepted.** Explicit 404; `ops_listen` in the prod example only (H-11). |
| P2-1 CSP vs external logos and the logout redirect | **Accepted.** `img-src … https:`; logout answers JSON (H-9b). |
| P2-2 TOTP step written by `totpEnable` | **Accepted.** H-7. |
| P3-1 SPA list, D-0139 reference, `PrepareTypedSettings` on the API | **Accepted.** Header, §1, H-2 fix 3. |

### Review of revision 3

| Finding | Disposition |
| --- | --- |
| P1-1 two-hop redirect restores headers; the canary's guard has the defect on `main` | **Accepted.** H-13 rewritten around the first request's origin; the canary prober is in scope. |
| P1-2 `RequireAuth` reads the cookie before the bearer | **Accepted.** The author's statement was false; H-4 fix 1 now changes `RequireAuth`. |
| P1-3 `is_global_admin` survives the flip; legacy NULL-issuer rows; READ COMMITTED; disagreeing client claims; recovery path | **Accepted.** H-1 fixes 2, 5 and 7. |
| P2-1 `events` and `me` 500 for tokens; tenant-free reads | **Accepted.** H-3 fix 5; S3 runs on PostgreSQL. |
| P2-2 no healthcheck to move; distroless image | **Accepted.** H-11. |
| P2-3 `post_logout_url` as a DOM sink; OpenAPI in iteration 4 | **Accepted.** H-9b; §5. |
| P3-1 empty S10/S11; missing test notes | **Accepted.** S5, S6, S8, S9a, S9b, S10, S11, S14. |

### Review of revision 4

| Finding | Disposition |
| --- | --- |
| P1-1 a secret in a 307/308 body and the library's `Referer` cross the origin | **Accepted**, narrowed: the hop is refused only for a **secret-bearing** body, which the executor knows; refusing every cross-origin body would break the legitimate upload-then-307 canary flow the tree already tests. `Referer` is removed on every hop. |
| P2-1 "one policy" collides with FR-029 / D3c1; scheme and userinfo on the Runner; port missing from S13 | **Accepted.** One shared predicate, two behaviours (H-13 table); D-0218 amended only in which origin is compared. |
| P2-2 "any `Authorization`" breaks Basic-auth proxies; `POST /auth/logout` outside `RequireAuth` | **Accepted.** The predicate is the `Bearer` scheme; the skip is `/api/*` only (H-4 fix 1). |
| P2-3 `kind` read before memberships; empty `FOR UPDATE` on first sign-in | **Accepted.** H-1 fix 5, both points, with the first-insert race in the test. |
| Note: `/\evil.com` passes a naive local-path check | **Accepted.** H-9b. |

### Review of revision 5

| Finding | Disposition |
| --- | --- |
| P1-1 the synthetic prober never sees `{{secret:…}}` after dispatch; an extracted `{{var}}` carries a derived secret in a body | **Accepted**, the reviewer's simpler option: the Runner's client refuses a cross-origin 307/308 with any body; the narrow rule stays for the canary only, marked recursively. |
| P2-1 the existing provenance test sends a body secret across a 307; the D-0218 amendment was understated | **Accepted.** The fixture change and all three amendments are named in H-13. |

### Review of revision 6

| Finding | Disposition |
| --- | --- |
| P2-1 a second canary test shares the fixture and would turn RED | **Accepted.** Both tests named in H-13. |
| Note: the refusal reason must pass `failureClass` | **Accepted.** H-13. |

## 10. Acceptance summary

S1 (bearer boundary) · S2 (probe input) · S3 (read authorization) · S4 (request forgery) ·
S5 (session revocation) · S6 (password and body bounds, limiters) · S7 (TOTP once) · S8 (bounded
reads) · S9a (no Referer from public pages) · S9b (CSP, zero reports) · S10 (`support_url`) ·
S11 (ops endpoints) · S12 (readiness) · S13 (headers and redirects) · S14 (subscribers and
visibility).
