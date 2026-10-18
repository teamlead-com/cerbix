# Spec: a project admin manages the members of their own project (func-project-members)

**Status:** revision 2, independent review APPROVED with P3 notes (applied below); APPROVED by the
owner 2026-10-17 (iter-0206, FR-040). Revision 1 was reviewed (CHANGES REQUIRED: one P1 on API
tokens, P2s on OpenAPI, races, acceptance and the E2E login); the dispositions are at the end.
The mock (`docs/design/mock-project-members.html`) was approved by the owner 2026-10-17; implemented
in iter-0206.

## Purpose

The role table on the Members page promises a Project Admin "Manage project members & settings"
(`frontend/src/components/settings/MembersPanel.vue`), and `authz.ActionProjectManage` is documented
as "manage a project's members and settings" (`internal/authz/authz.go`). Neither is true for
members: every member endpoint (`GET/POST /organizations/{orgID}/members`,
`PATCH/DELETE …/members/{membershipID}`) requires `ActionOrgManage`, which a Project Admin does not
hold. Today only an Org Admin (or a global admin) can grant anyone access to a project. The owner
decided (2026-10-17) that a Project Admin manages the members of their own project.

## Current state

- Roles by scope (`domain.Role.ValidForScope`, DB check `memberships_role_scope_chk`): org scope —
  `org_admin`, `editor`, `viewer`; project scope — `project_admin`, `editor`, `viewer`.
- An org-scoped membership applies to the org and every project in it; a project-scoped one applies
  only to its project (`Principal.Can`). Grants add up across a user's memberships.
- A user may hold one org-level and one project-level membership per project at once (unique
  indexes `memberships_user_org_uniq`, `memberships_user_org_project_uniq`); a project-scoped row
  can only name a project of its own org (composite FK); deleting a project cascades its
  project-scoped memberships.
- **No handler checks `project:manage` at all today** — the action exists only in the role matrix,
  its tests and the token-actions chip catalogue (`frontend/src/lib/tokenActions.ts`). Anything this
  spec gates on it is a new power, including for API tokens.

## Target state

### API — new project-scoped member endpoints

Four endpoints, each gated ONLY by the existing `projectAccess(w, r, projectID,
authz.ActionProjectManage)` (`internal/api/handlers_monitors.go`), which resolves the project and
its permission BEFORE anything else: unknown project, a project of another org, or a project the
caller cannot see → `404`; a visible project without `project:manage` → `403`. A membership is never
loaded before that gate passes, so an Editor or Viewer probing membership ids always gets `403` and
learns nothing about which ids exist. (The org handler `memberAccess` loads the membership first;
that order is NOT copied.)

**Machine principals are refused.** Any principal with `ViaToken` — a `cbx_` API token AND an
OIDC client-credentials JWT (`internal/auth/middleware.go`, which sets `ViaToken` on both) — gets
`403` on all four endpoints, before the project gate — the same answer for any project id, so it
discloses nothing — following the precedent of
`totpEnroll` (`internal/api/handlers_totp.go`). Reason: no handler honours `project:manage` today,
so letting tokens through would silently give every existing project-scoped `project_admin` token —
including the default "role decides" form with no allow-list — the power to grant people lasting
access that survives the token's revocation, audited with a NULL actor (a client-credentials
principal carries a real user id, but the same "machine grants lasting human access" argument
applies). `project:manage` therefore
stays a no-op for tokens, as it is today. The token form keeps offering the `project:manage` chip
(`frontend/src/lib/tokenActions.ts`; the allow-list is validated against the server's action
catalogue, and removing a catalogue entry is a separate change); the token documentation states
that `project:manage` grants a token nothing. (Org-admin tokens can already manage members through the
org endpoints; that is unchanged and out of scope.)

1. `GET /api/v1/projects/{projectID}/members` →
   `{ "members": [ProjectMember], "inherited": [InheritedMember] }`
   - `members`: the project-scoped memberships of THIS project — `id`, `user_id`, `role`, `email`,
     `display_name`, `last_active_at`, `created_at`.
   - `inherited`: the org-level memberships of the project's org, read-only — ONLY `role`,
     `email`, `display_name`. No membership id, no user id, no last-active: a Project Admin needs to
     know who reaches the project, nothing they could act on. Global admins are not memberships and
     do not appear. A user with both an org-level and a project-level membership appears in both
     lists.
2. `POST /api/v1/projects/{projectID}/members` — body `{ "email" | "user_id", "role" }`; the
   membership is created with `org_id` = the project's org and `project_id` = this project. `role`
   must be a project role (`project_admin`, `editor`, `viewer`); anything else → `400`. A body
   naming `project_id` or `org_id` is rejected as an unknown field (`400`). Adding by email works as
   on the org endpoint (the user must have signed in once). Creation errors are told apart (the org
   endpoint folds them all into `400 could not create membership`):
   - the user already has a membership in this project → `409 "already a member of this project"`
     (unique violation);
   - unknown `user_id` → `400 "unknown user"` (foreign-key violation on the user);
   - the project disappeared between the gate and the `INSERT` → `404` (foreign-key violation on
     the project).
3. `PATCH /api/v1/projects/{projectID}/members/{membershipID}` — body `{ "role" }`, a project role.
4. `DELETE /api/v1/projects/{projectID}/members/{membershipID}`.

For 3 and 4 the membership must be a project-scoped membership of THIS project; anything else
(an org-level membership, another project's, another org's, an unknown id) → `404`.

**Scope is in the SQL, not only in the handler.** The store functions behind 3 and 4 update and
delete `WHERE id = $1 AND project_id = $2`. The list in 1 selects `WHERE project_id = $1` for
`members` and `WHERE org_id = <project's org> AND project_id IS NULL` for `inherited`.

**Races.** A membership that disappears between the gate and the write — removed by someone else,
or cascaded away with its project — yields `404`, not `500`: the scoped `UPDATE`/`DELETE` affecting
zero rows is a not-found, logged at no error level. The org handlers have the same race and today
answer `500` (`updateMember`, `removeMember` map any store not-found to "internal error"); that is a
pre-existing defect, and it is FIXED in this iteration: both org handlers map the store's
not-found to `404` the same way, with an API test for each.

### Rules that keep it from becoming an escalation path

- A Project Admin can never create, change or remove an org-level membership (the endpoints cannot
  address one; the SQL is scoped), never grant `org_admin` (not a project role; also refused by the
  DB check), never reach another project or org (the project gate, the composite FK).
- Granting `project_admin` to another user IS allowed — it is the highest project role and does not
  exceed the granter's own scope.
- No "last project admin" lock: an Org Admin or a global admin can always manage the project, so it
  cannot be locked out. (A global admin may even leave an org without an org admin —
  `removeMember`/`updateMember` — and is then the one who can restore it.)
- A Project Admin may demote or remove themselves. The principal is rebuilt on every request
  (`internal/auth/auth.go`), so their next call is `403`; the UI confirms this one action and,
  after it, hides the tab without an error (a point for the mock).
- The existing org endpoints are unchanged and remain org-admin only; a `project_admin` principal
  keeps getting `403` there.

### Audit

Every write is recorded in the org audit log as today: actions `member.add`, `member.role_change`,
`member.remove`; target `"<role> · user <user id> · project <project id>"` (remove:
`"user <user id> · project <project id>"`). The org endpoints use the same format whenever the
membership is project-scoped (today their `member.add` omits the project even with `project_id`),
so one action has one target shape on both paths. A Project Admin cannot read the org audit log
(`listAudit` requires `org:manage`), so they do not see this history; that is out of scope here and
stated, not an accident.

### OpenAPI and the SPA client

`openapi.yaml` (the API's source of truth) gains the four paths, the `ProjectMembers`,
`ProjectMember` and `InheritedMember` schemas, request bodies without `project_id`/`org_id`, and the
`400`/`403`/`404` responses. The SPA uses the typed client: `npm run gen:api`, then
`make spa-snapshot`.

### UI — Settings

- A new **Members** tab in the **Project** group, visible when the current user can manage the
  current project — `session.canProjectAdmin(orgId, projectId)` (Org Admin, global admin, Project
  Admin of that project). Its comment names a non-existent `ActionProjectAdmin`; it is corrected to
  `ActionProjectManage`. The tab shows the project's members (editable: role, remove), the inherited
  org-level access (read-only, with a note that an Org Admin manages it), and an invite form limited
  to this project and to project roles.
- The organization **Members** tab is unchanged.
- The role table: the "Manage project members & settings" row becomes accurate for Project Admin;
  no other row changes.
- Exact layout comes from the mock, including the state after a Project Admin removes themselves.

### Known exposure

Adding by email answers "no user with that email" when the address is not a user of the instance.
An Org Admin can already probe addresses this way; now every Project Admin of every organization on
a multi-tenant instance can too. Accepted with the message unchanged; the instance user list itself
stays global-admin only. Two existing traits of this path are inherited as is: the email match is
case-sensitive (`internal/store/users.go`), and OIDC users may share an email, in which case the
oldest user (by `created_at`) is chosen. A per-principal rate limit on the POST (the
`internal/api/changelimiter.go` pattern) is NOT added in this iteration; it is noted as a possible
hardening.

## Out of scope

Invitations for users who have never signed in; a Project Admin managing org-level members, reading
the org audit log, deleting the project or changing org settings; API tokens managing members of a
project; changing the org Members tab; fixing the case-sensitive email match.

## Acceptance

**Store tests (real database, both storage modes; two orgs, two projects each)** — the scoped list,
update and delete touch only the named project's project-scoped rows; `inherited` lists only the
project org's org-level rows; an update or delete of a membership that is org-level, of another
project or gone affects zero rows. These are the tests that hold the SQL scope; the API test fake
(`internal/api/api_test.go` fakeStore) does not filter by project and must not be the only guard.

**API tests (written first, RED → GREEN):**
- A Project Admin lists, adds, re-roles and removes members of their project.
- `404` for: an org-level membership id, another project's membership, another org's membership,
  an unknown id, a project of another org, a project the caller cannot see.
- `403` for: a project the caller sees as Viewer or Editor (checked before any membership is
  loaded — the same `403` for an existing and a non-existing membership id), a `cbx_` token
  principal (with and without an allow-list naming `project:manage`), a client-credentials JWT
  principal, a `project_admin` principal on all four ORG endpoints.
- `409` for a duplicate membership; `400` for an unknown `user_id`.
- The org `PATCH`/`DELETE` of a membership removed in between → `404` (the pre-existing defect).
- `400` for: `org_admin` or an unknown role on POST and PATCH; a body with `project_id` or
  `org_id`.
- Org Admin and global admin succeed. Audit rows carry the target format above. A membership
  removed between the gate and the write → `404`.

**Mutations (manual protocol, each killed by a named test):**
1. drop `AND project_id = $2` from the scoped update/delete → store test;
2. drop the `project_id` filter from the `members` list → store test;
3. drop the `org_id` filter from `inherited` → store test;
4. gate on `ActionProjectRead` instead of `ActionProjectManage` → API test (Editor 403);
5. remove the "membership of this project, else 404" check → API test;
6. let `org_admin` through PATCH → API test;
7. remove the token refusal → API test;
8. load the membership before the project gate → API test (same 403 for existing and unknown id).

**vitest:** the tab's visibility for Org Admin, global admin, Project Admin of the current project,
Project Admin of another project, Editor, Viewer; the invite form offers project roles only.

**E2E (live single stack):** the Keycloak fixture user `testuser@example.com` in a context of its
own with an EMPTY `storageState` (the pattern of `e2e/tests/admin.spec.ts`), never the shared admin
session and never `/auth/logout` on it; `test.skip` outside the single topology. The admin session
grants the fixture user `project_admin` on the E2E project through the API and removes it in
`finally`. The member the fixture user adds is `admin@cerbix.local` — on a fresh database the only
other provisioned user (`auth.setup` provisions the fixture; other realm users exist in cerbix only
after their own first sign-in) — as `viewer`, also removed in `finally`. The fixture user opens
Settings → Project → Members, adds that member, changes the role, removes them; the org Members tab still shows its existing message ("You need org-admin rights to
view members."). The expected `make dev-test` count in `CLAUDE.md` is updated.

**Gates:** `go vet`, `make docs-check`, full `-race` on both storage modes, `npm run build`, vitest,
`make spa-snapshot`, `make dev-test`. CHANGELOG entry under the next unreleased version.

## Review of revision 1 — dispositions

- P1-1 API tokens silently gain member management: accepted — tokens are refused on all four
  endpoints (403), `project:manage` stays a no-op for tokens; acceptance and mutation 7.
- P2-1 the spec index: accepted — `docs/specs/README.md` lists this file.
- P2-2 OpenAPI: accepted — section "OpenAPI and the SPA client", `gen:api`, `spa-snapshot`.
- P2-3 races and scope in SQL: accepted — 404 for a vanished membership, `AND project_id` in the
  SQL; the org handlers' 500 recorded as pre-existing.
- P2-4 acceptance: accepted — 404/403 split, gate before load, store tests on a real database, the
  org-endpoint 403 for `project_admin`, the unknown-field 400.
- P2-5 the E2E login: accepted — the Keycloak fixture user in its own empty context.
- P3-1 `inherited` shape: accepted — role, email, display name only.
- P3-2 the email oracle: accepted as stated, with the multi-tenant note, the case-sensitive match and
  shared OIDC emails; no rate limit in this iteration.
- P3-3 wording and self-demotion: accepted.
- P3-4 audit target and visibility: accepted.
- P3-5 names: accepted — `projectAccess`, `session.canProjectAdmin` (comment fixed), the
  "nothing checks `project:manage`" statement.
- P3-6 mutations: accepted — the list above.
- P3-7 housekeeping: CHANGELOG and the `CLAUDE.md` E2E count are in the gates.

## Review of revision 2 — APPROVED, P3 notes applied

- P3-1 `ViaToken` covers client-credentials JWTs too: refusal stated for both; both in the 403
  matrix.
- P3-2 who the E2E adds: `admin@cerbix.local` as viewer, removed in `finally`.
- P3-3 the org handlers' 500 on a vanished membership: decided — fixed in this iteration.
- P3-4 POST errors: 409 duplicate, 400 unknown user, 404 vanished project.
- P3-5 the `project:manage` token chip: kept, documented as granting nothing; the org path's audit
  target gains the project for project-scoped memberships.
