# func-org-deletion — Delete an organization (FR-019)

Status: TODO (spec) · Owner rule: application (tenant lifecycle) / transport (RBAC) ·
Decision: D-0151 · Iteration: iter-0112

## 1. Context & motivation

The tenancy model is **org → project → monitors**. Organizations can be *created*
(`POST /api/v1/organizations`, global-admin only) and read, but there is **no way to
delete one** — the only removal path today is direct DB surgery. This is the org-level
analogue of FR-018 (delete project, D-0150) and reuses the same shape.

## 2. Requirement

**FR-019** — A global admin can permanently delete an organization. Deletion removes the
organization and everything it owns (all projects and their subtrees — monitors &
history, incidents, SLA, escalation/on-call, channels — plus memberships, org-level
status pages, org-scoped tokens/webhooks, and the org's audit log). Irreversible,
type-the-slug confirmation, audited at the instance level, and refused for organizations
that own file-provider-managed projects.

## 3. Scope & non-goals

**In scope**
- `DELETE /api/v1/organizations/{orgID}` → `204 No Content`.
- Store `DeleteOrganization(ctx, orgID)` — single delete, DB cascade.
- **Global-admin** (`ActionGlobalManage`) authorization; 403 for everyone else, 404 when
  the org does not exist.
- Guard: refuse (`409 managed_by_file`) when the org has file-provider-managed
  projects/monitors (§7.3).
- Instance-level audit event `org.delete` (`org_id = NULL`).
- Frontend: a **Danger Zone** in org-scoped Settings (global-admin only) with a
  type-the-slug confirm modal; `ws.deleteOrg` + post-delete org switch.
- OpenAPI + regenerated TS schema.

**Non-goals**
- No soft-delete / archive / restore (hard delete — matches the schema; §5).
- No "last organization" guard — an instance may have zero orgs.
- No bulk deletion; no project deletion here (that is FR-018).
- No new migration — the cascade already exists (§5); `audit_logs.org_id` is already
  nullable (migration 00047).

## 4. Behavior & semantics

1. **Hard delete via DB cascade.** `DELETE FROM organizations WHERE id=$1` removes the org
   row; every child FK's `ON DELETE CASCADE` (§5) wipes owned data in one transaction —
   including each project, which itself cascades to monitors/heartbeats/incidents/etc.
2. **Global-admin gate first.** Non-global-admins get `403` *before* any existence check,
   so the endpoint never leaks whether an org exists to non-admins. A missing org (for a
   global admin) is `404`.
3. **Confirmation.** The UI requires typing the org **slug** to enable delete. The API
   guard is RBAC only.
4. **Instance-level audit.** One `org.delete` row with `org_id = NULL` (an instance action
   by a global admin; `target` = the deleted org id/slug). It is recorded with a NULL org
   precisely so it **survives** the cascade that removes the org's own `audit_logs`.
5. **In-flight checks.** Removed monitors drop out of the scheduler snapshot on the next
   refresh (15 s); late heartbeat inserts for gone monitors fail their FK and are logged.

## 5. Cascade map (already enforced by the schema — verified)

Every FK to `organizations(id)` is `ON DELETE CASCADE`; deleting one org row removes:

| Table (migration) | Link | Note |
| --- | --- | --- |
| `memberships` (00001) | `org_id` CASCADE | every membership in the org |
| `projects` (00001) | `org_id` CASCADE | → each project cascades to monitors → heartbeats/rollups, incidents, SLA, escalation/on-call, channels, dependencies, notifications, project-scoped tokens/webhooks/status-pages, file bundles (see func-project-deletion §5) |
| `status_pages` (00007) | `org_id` CASCADE | org-level pages (project-scoped ones go via their project) |
| `api_tokens` (00008) | `org_id` CASCADE | org- and project-scoped tokens |
| `webhooks` (00010) | `org_id` CASCADE | |
| `audit_logs` (00018) | `org_id` CASCADE | the org's audit trail (the `org.delete` audit itself is written with `org_id NULL`, so it survives) |

No `SET NULL` edge dangles at the org level (unlike project deletion, where org-level
status-page components pointing at a deleted monitor detach — that only applies when a
*project* is deleted under a surviving org).

## 6. API

```
DELETE /api/v1/organizations/{orgID}
  → 204 No Content            deleted
  → 403 Forbidden             caller is not a global admin
  → 404 Not Found             organization does not exist
  → 409 managed_by_file       the org owns file-provider-managed projects/monitors
```

Mounted in `internal/api/api.go` next to the org routes; handler `h.deleteOrganization`
in `handlers.go`, behind session-auth middleware.

## 7. Guards & edge cases

### 7.1 Authorization
Require **global admin** (`ActionGlobalManage`), symmetric with `createOrganization`. An
org admin of the target org cannot delete it (deleting an org is an instance-lifecycle
action). Checked first → `403` for all non-global-admins (no existence leak).

### 7.2 File-provider-owned orgs
If any project in the org owns `managed_monitors` / `file_provider_bundles`, refuse with
**`409 managed_by_file`**. Unlike a deleted project (which a reconcile would recreate),
the reconcile does **not** recreate a missing org — it fails tenant resolution
(`ErrBundleTenantNotFound`) and logs an error on every pass. Refusing keeps the file
provider from entering a perpetual tenant-not-found error state; the operator removes the
provider's files/config first. Checked in the same tx as the delete.

### 7.3 No "last org" block
Deleting the only org is allowed (an empty instance is valid; a global admin can create a
new org). The UI warns but does not prevent it.

### 7.4 Concurrency
The file-provider check and the `DELETE` share one transaction, so an interleaved
reconcile cannot slip a managed project back in between check and delete.

## 8. Store

```go
// DeleteOrganization removes an organization and (via ON DELETE CASCADE) everything it
// owns — projects and their subtrees, memberships, org-level status pages, org-scoped
// tokens/webhooks, and the org's audit rows. Returns ErrNotFound when 0 rows match, and
// ErrManagedByFile when the org owns file-provider-managed projects/monitors.
func (s *Store) DeleteOrganization(ctx context.Context, orgID string) error
```
- One tx: (a) `SELECT 1 WHERE EXISTS(managed_monitors WHERE org_id=$1) OR EXISTS(
  file_provider_bundles WHERE org_id=$1)` → `ErrManagedByFile`; (b) `DELETE FROM
  organizations WHERE id=$1` → `ErrNotFound` if `RowsAffected()==0`.
- Adding `DeleteOrganization` to the `Store` interface in `internal/api/api.go` breaks
  `fakeStore` in `internal/api/api_test.go` (intentional — implement the stub). The
  scheduler/outbox fakes are unaffected.
- The `org.delete` audit is recorded by the handler (`h.audit` with an empty orgID ⇒ NULL)
  *after* a successful delete, matching the existing best-effort audit pattern.

## 9. Frontend

- **Placement.** Org-scoped **Settings → Danger Zone**, a small red card, gated on
  `session.isGlobalAdmin` (a separate tab from the project-scoped Danger Zone added in
  FR-018; both live under their own scope group in the settings nav).
- **Confirm modal.** Same visual language as the project one: names the org, lists what
  will be destroyed (all projects & their monitors/history, incidents, members, channels,
  tokens), requires typing the org **slug** to enable a red **Delete organization** button.
- **Store.** `ws.deleteOrg(id)` in `stores/workspace.ts` (symmetric to `createOrg`):
  `DELETE`, then drop it from `orgs`, switch to another org (reload its projects) or clear,
  and route to `dashboard`.
- **Errors.** `409 managed_by_file` → friendly message; `403` → the existing not-authorized
  pattern.
- **UI mock required & approved before any frontend code** (project convention).

## 10. RBAC summary

| Caller | Result |
| --- | --- |
| Global admin | 204 |
| Org admin of the target org | 403 |
| Anyone else | 403 (404 only for a global admin hitting a missing org) |

## 11. Verification

1. `go vet`, `go build`, `go test -race -count=1 ./...` in **both storage modes**
   (timescale `cerbix_test` + throwaway `postgres:16-alpine`) — the cascade reaches
   `heartbeats` (adaptive), so it must pass in both.
2. New store tests: `DeleteOrganization` cascade (projects + monitors + memberships +
   audit gone), `ErrNotFound` for an unknown id, `ErrManagedByFile` when file-owned, and
   that other orgs are untouched.
3. API tests: 204 for global admin; 403 for org admin / member / outsider; 404 for a
   global admin on an unknown org; 409 for a file-managed org.
4. Frontend: docker `npm run build` (vue-tsc).
5. **E2E** on a live stack (`e2e/tests/org-delete.spec.ts`): as a global admin, create an
   `e2e-` org + a project + monitor, open Settings → org Danger Zone, type-slug-confirm
   delete, assert the redirect to the dashboard and that the org and (via cascade) its
   project + monitor return 404. Non-admin authorization is covered by the api unit test
   (`TestDeleteOrganizationAuthz`).

## 12. Deliverables (process)

Spec (this file) → approved UI mock → implementation (store + API + OpenAPI + TS +
frontend) → `-race` (both DB modes) + E2E → `docs/iterations/iter-0112.md`, decision
**D-0151**, a row in `docs/traceability.md` (FR-019), and an FR-019 line in
`docs/status.md`.
