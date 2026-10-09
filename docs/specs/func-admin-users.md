# Spec: Instance-wide Users administration (func-admin-users)

## Purpose

Give the Global Admin a single place to see and manage **every user of the instance**,
regardless of org membership. Today users are only visible through org member lists
(`ListOrgMembers`, INNER JOIN via `memberships`), so OIDC JIT-provisioned users with no
membership are invisible everywhere while still able to authenticate. The page lives in
Settings → **Administration** → Users (see `func-settings-members` for the IA change).

## Requirements

### API (all under `requireGlobalAdmin`, mounted next to `/api/v1/admin/outbox/*`)

1. `GET /api/v1/admin/users?q=` — every user with:
   - user fields (id, email, display_name, is_global_admin, created_at),
   - `auth_type`: `local` (password_hash set), `oidc` (oidc_sub set), or `both`,
   - `last_active_at` (the user's `last_seen_at`, as in `ListOrgMembers` — see "Last active"
     below),
   - `memberships`: array of `{org_id, org_name, project_id?, project_name?, role}` —
     empty array for org-less users (LEFT JOIN, never hides a user).
   - `q` filters by email/display_name substring (case-insensitive).
2. `PATCH /api/v1/admin/users/{userID}` body `{is_global_admin: bool}`:
   - reuses `store.SetGlobalAdmin`;
   - **cannot change your own flag** (400) — prevents accidental self-lockout;
   - **cannot demote the last global admin** (400, via new `CountGlobalAdmins`).
3. `DELETE /api/v1/admin/users/{userID}`:
   - **cannot delete yourself** (400); cannot delete the last global admin (400);
   - a plain `DELETE FROM users` — every FK already cascades (memberships 00001,
     sessions 00002, TOTP 00019, reset tokens 00023) and `audit_logs.actor` is SET NULL;
   - deleting a user who is the sole org_admin of some org is allowed: the global admin
     can appoint a replacement afterwards (documented behavior, not a guard).
4. "Add to organization" reuses `POST /organizations/{orgID}/members`: `AddMember` gains
   an optional `user_id` that takes precedence over `email` (email lookup is LIMIT 1 and
   ambiguous when a local and an OIDC account share an address). A global admin passes the
   existing `InOrg` check unconditionally. The `isLastOrgAdmin` guard (no demoting/removing
   an org's sole org_admin) does **not** bind a global admin: an org without an org_admin is
   always recoverable by the global admin, consistent with deleting the account entirely.

### Audit

Global actions must be auditable: migration `00047` relaxes `audit_logs.org_id` to
nullable; `RecordAudit` maps an empty org to NULL. New actions: `user.global_admin`
(target: user id + new value) and `user.delete` — recorded with `org_id = NULL`.
(Viewing global audit entries in the UI is out of scope for this iteration.)

### Store / domain

- `domain.AdminUser` — user-keyed DTO (a user in 3 orgs is ONE row with 3 memberships),
  unlike membership-keyed `domain.Member`.
- `store.ListAllUsers(ctx, q)`, `store.DeleteUser(ctx, id)`, `store.CountGlobalAdmins(ctx)`
  in a new `internal/store/users_admin.go`.
- `internal/api` Store interface grows → `fakeStore` in `api_test.go` follows
  (outbox/scheduler fakes unaffected: their interfaces do not include user methods).

### OpenAPI / SPA

- `openapi.yaml`: paths + schemas (`AdminUser`, `AdminUserMembership`, `UpdateAdminUser`;
  `AddMember.user_id`), then regenerate `frontend/src/api/schema.d.ts`.
- `components/settings/UsersPanel.vue`, tab `users` (scope instance = Administration
  group, first position). Table: search box, user (avatar/initials, name, email),
  auth-type pill (local/OIDC), membership chips per org (`Acme (org_admin)`), a
  **"no organization"** warning badge for org-less users, last active, global-admin star.
  Row actions: Grant/Revoke admin (confirm), Add to org, Delete (confirm; destructive
  styling). Self row: admin toggle and delete disabled with a tooltip.
  "Add to org" opens an **inline expansion row** under the user (not a floating popover —
  that gets clipped by the table's overflow container; at most one open at a time):
  multi-select org chips from `ws.orgs` — global admin sees all orgs; orgs the user is
  already in are disabled — and a picked-orgs list where **the role is set per
  organization** (default `editor`, no ordering between picking and role choice), with an
  explicit "Set all to" bulk action over the already-picked rows. Submit is one
  membership call per org, each with its own role; failures are reported per org and the
  failed picks stay in the form.
- 403 → existing friendly-message pattern (non-admins never see the tab anyway).

## Last active (iter-0205, D-0272)

`last_active_at` on the instance user list (`GET /admin/users`) and on an organization's member
list (`ListOrgMembers`) is `users.last_seen_at`, a column of its own (migration `00111`). It
used to be derived as `max(sessions.created_at)`, which forgot a user as soon as their sessions
were gone — a session lives 24 h by default, the scheduler purges expired ones, and logout
deletes its own — so a user who had signed in days ago, or had signed out, read "never".

- **Set on sign-in.** Creating a session stamps `last_seen_at = now()` in the same statement,
  for every sign-in path — they all create their session through one store call
  (`CreateSession`).
- **Refreshed by use.** A request authenticated by a session cookie refreshes it, at most once
  per 5 minutes per user: the API process skips users it stamped less than 5 minutes ago, and
  the SQL refuses to move a stamp younger than that, so several API processes cannot multiply
  the writes. The refresh is best effort: a failure is logged and never denies the request.
- **Not refreshed by machines.** A service-account bearer token has its own `last_used_at`; an
  OIDC client-credentials JWT is not an interactive sign-in. Neither moves `last_seen_at`.
- **Never removed with sessions.** Logout, session expiry and the expired-session purge leave
  it alone.
- **Backfill.** The migration sets it from each user's newest remaining session. A user whose
  sessions were all purged before the upgrade reads "never" until they next sign in — that
  history no longer exists anywhere.
- Writing it does not change `users.updated_at`.

## Out of scope

Inviting users who have never signed in (no invitation flow exists — users appear via
OIDC JIT, bootstrap admin, or future local signup), editing email/display name, password
resets from the admin page, a global audit viewer.

## Acceptance

- `go test -race` green in both storage modes (a migration is included), new store tests
  (`ListAllUsers` incl. org-less user + search, `DeleteUser` cascade, `CountGlobalAdmins`)
  and handler tests (403 for non-admin, self-change/self-delete 400, last-admin 400,
  `addMember` by `user_id`).
- vue-tsc build green.
- E2E on a live stack (`--profile single --profile sso`): the OIDC-provisioned
  `testuser@example.com` shows up with the "no organization" badge → Add to org →
  Grant admin → Revoke → Delete; self-demotion attempt returns 400; audit rows with
  NULL org recorded.
