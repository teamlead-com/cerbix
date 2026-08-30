# Spec: Authentication and authorization (sec-authn-authz)

## Purpose

Any OpenID Connect provider as the authentication source (Keycloak, Auth0, Okta, Google,
Entra ID — selected solely via `oidc.issuer`), authorization in the cerbix DB, sessions; plus
local login, API tokens and machine access. The provider is **not hardcoded** (D-0043).

## Authentication (implemented in iter-0003; provider-agnostic — iter-0035, D-0043)

- OIDC Authorization Code + PKCE (S256) against any conformant issuer; discovery via
  `oidc.issuer`; ID token verification (JWKS, `iss`/`aud`, signature, exp) + nonce.
- Endpoints: `GET /auth/login` (state+nonce+PKCE in `auth_flows`, redirect to the provider),
  `GET /auth/callback` (code exchange, verify, JIT, session, cookie), `GET|POST /auth/logout`.
- **Discovery for the UI** (D-0045): public `GET /auth/config` → `{local, oidc,
  oidc_button_label}`; the SPA renders a provider-neutral login button labeled from
  `oidc.button_label` (default "Continue with SSO"). The local form — only if `local`.
- **JIT provisioning**: `UpsertUserByOIDCSub(sub, email, name)` on first login; identity
  is keyed on the `oidc_sub` claim (column `users.oidc_sub`, D-0044).
- **Bootstrap**: an email from `oidc.admin_emails` receives `is_global_admin` on login.
- **Sessions**: server-side; a cookie (HttpOnly, SameSite=Lax, Secure per config) holds
  an opaque token; the DB stores only the SHA-256 hash (`sessions.token_hash`). TTL from config.
- Code: `internal/auth/*` (incl. `handlers.go` `ConfigHandler`),
  `internal/store/{users,sessions,authflows}.go`, migrations `00001`/`00002`.

## Authorization (implemented)

- `internal/authz`: `Principal{UserID, IsGlobalAdmin, Memberships}` (+ `Actions`, a token's
  allow-list — FR-025 D12), the `Action` set, a declarative matrix `role→actions`,
  `Can(action, orgID, projectID)` + `VisibleScope` (its query-scope mirror) + `InOrg` +
  `VisibleOrg`/`VisibleProject`.
- A single source of access rules; global admin — bypass. For the matrix see `func-tenancy-rbac.md`.
- Middleware `RequireAuth`: cookie → session → user+memberships → `Principal` in the context;
  then service-token (Bearer) → client-credentials JWT; 401 without a valid principal.
  Handlers call `Can`; isolation on every read/write.

## Default local login (FR-015, implemented in iter-0005)

Besides OIDC, cerbix has **built-in (local) authentication** — so the service can be
deployed and administered without an external OIDC provider (dev environments, bootstrap,
isolated environments). Roles and permissions are the same (Global Admin / Org Admin / Project Admin /
Editor / Viewer); local login issues the same server-side session as OIDC.

Implementation:

- **Users** (migration `00004`): `users.oidc_sub` NULLABLE + `password_hash text
  NULL`. A user is either OIDC-backed (`oidc_sub`) or local (`password_hash`).
  A partial unique index on `email` for local users.
- **Hashing**: **argon2id** (`internal/auth/password.go`), PHC encoding, a random
  salt, constant-time comparison; only the hash is stored; passwords/hashes are not logged.
- **Bootstrap admin**: on startup, if local login is enabled and the system is empty,
  an `is_global_admin` is created from `security.admin_email`+`_password`. The password is **not generated
  and not logged** — if it is not set, the admin is not created (log `bootstrap_admin_skipped`).
- **Endpoints**: `POST /auth/local/login` (username(email)+password[+`totp`] → session+cookie),
  the shared `/auth/logout`; password change — `POST /api/v1/me/password` (verification of the current one,
  minimum length of the new one; UI — Settings → Security).
- **Password reset (self-service, D-0068)** — local accounts only, requires a configured
  `mail`. Migration `00023`: `password_reset_tokens(user_id, token_hash, expires_at, used_at)`
  — only the hash is stored (`HashToken`), TTL 1 hour, single-use (consume = `UPDATE … used_at
  WHERE used_at IS NULL AND expires_at > now() RETURNING user_id`). `POST /auth/local/reset/request`
  {email} — always `200 {ok:true}` (no user enumeration), per-IP rate limit (the shared loginLimiter);
  on a local email match sends a letter with the link `${public_base_url}/reset?token=…`.
  `POST /auth/local/reset/confirm` {token, new_password} — consumes the token → `SetPassword`; invalid/
  expired/used → `400`. `/auth/config` returns `password_reset` (=`local && mail`), so the
  login page shows the "Forgot?" link only when the reset actually works.
- **Two-factor authentication (TOTP, D-0064)** — local accounts only (for
  OIDC, MFA is on the provider side). Package `internal/totp` (RFC 6238: HMAC-SHA1, 6 digits,
  30s period, base32 secret without padding, ±1 step tolerance, constant-time; no dependencies).
  Migration `00019`: `users.totp_secret` (encrypted with the same keyring cipher as monitor
  secrets) + `users.totp_enabled`; table `totp_recovery_codes(user_id, code_hash,
  used_at)` — recovery codes are stored only as hashes (`HashToken`), single-use.
  Self-service (Settings → Security): `POST /api/v1/me/totp/enroll` (generates a pending
  secret + an `otpauth://` URI, 2FA still off), `…/enable` (verifies a live code → enables,
  returns 8 recovery codes **once only**), `…/disable` (re-asks the password → wipes the
  secret+codes). With 2FA enabled, after a correct password a valid TOTP **or** an
  unused recovery code is required; otherwise `401 {"totp_required":true}` (an incorrect **password**
  remains a uniform 401 without a hint). `/api/v1/me` returns `totp_enabled`.
- **Config** (`local`): `enabled`, `security.admin_email`, `security.admin_password`,
  `min_password_length` (default 8), `login_rate_limit_per_minute` (default 10, 0=off).
  Validation: `local ⇒ database`.
- **Compatibility**: OIDC and local login work simultaneously; `auth.New` builds OIDC
  only if `oidc.issuer` is set; routes are registered per the enabled methods.
- **Security**: a uniform 401 response on an incorrect login/password (no user enumeration);
  per-IP sliding-window rate limit (429 above the limit, D-0031); a ban on logging secrets
  (`forbidigo`).

**FR-015** — DONE. **NFR-010** — argon2id/hash-only/uniform-errors/no-logging/rate-limit:
DONE (rate limit — D-0031).

## Machine access (iter-0013/iter-0027)

- **cerbix API tokens** (`api_tokens`, only the SHA-256 hash is stored; project/org + role);
  `Authorization: Bearer cbx_…` → a scoped principal (iter-0013).
- **OIDC client-credentials** (iter-0027, D-0034; provider-agnostic, D-0043): a machine
  OAuth2 client, the JWT is verified by issuer+signature (`ccVerifier`, audience relaxed),
  JIT by `sub`, access — via membership. Both schemes converge on the same `authz.Can`.
- **Per-token `actions` allow-list (FR-025 D12, iter-0165; D-0209/D-0212).** `api_tokens.actions
  text[] NULL`. `NULL` means what it always meant — the token's role decides. A non-null list is an
  ALLOW-LIST intersected with the role inside the ONE central predicate and nowhere else: `Can(action)`
  for a token principal is `roleGrants[role] ∋ action AND (actions IS NULL OR action ∈ actions)`, and the
  query-scope mirror `VisibleScope` intersects the same list (or a narrowed token could still enumerate
  what it may not read). Project VISIBILITY is membership, not action: `VisibleProject` — the
  404-versus-403 predicate — reads membership alone, so a narrowed token is 403, never 404, on its own
  project (D-0212 item 2). The list is validated at creation against the central `Action` catalogue
  (400 `action_unknown`) and against the token's own role (400 `action_not_granted` naming the entry —
  the operator's mistake surfaces at the form, not at the pipeline's first 403), is immutable after
  creation (`PATCH`/`PUT` → 405; a different list is a new token), and appears in the token's read model
  (`null` or an array) and in the `token.create` audit row. The middleware copies the list onto the
  principal (`internal/auth/middleware.go`); handlers keep calling `Can` with an action and compare no
  role string. The canonical CI token is `role: editor, actions: [gate:evaluate, change:record]` — it asks
  the gate and records changes and can do nothing else. Code: `internal/authz/authz.go`,
  `internal/store/apitokens.go`, `internal/api/handlers_apitokens.go`; spec `func-change-intelligence.md`
  D12, invariants 16–17.

## Requirements

- FR-004 (provider-agnostic OIDC + JIT + sessions + `/auth/config`) — DONE (D-0043/44/45).
- NFR (**security**): secrets/cookies/bearer are not logged; session tokens — hash only;
  `auth_flows` are single-use (`DELETE ... RETURNING`) and have a TTL; PKCE + nonce are mandatory.
- FR-013 (API tokens + OIDC client-credentials) — DONE (iter-0013/iter-0027, D-0034).

## Open questions

- The specific realm/client convention in prod depends on the chosen IdP (any OIDC issuer) —
  needed for a live e2e and the prod config; there is no code dependency on the vendor.
- Session storage at scale (currently Postgres; as it grows — evaluate Redis).
