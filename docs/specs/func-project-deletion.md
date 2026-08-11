# func-project-deletion — Delete a project (FR-018)

Status: TODO (spec) · Owner rule: application (tenant lifecycle) / transport (RBAC) ·
Decision: D-0150 · Iteration: iter-0111

## 1. Context & motivation

cerbix models tenancy as **org → project → monitors**. Projects can be *created*
(`POST /api/v1/organizations/{orgID}/projects`, `CreateDialog.vue` → `ws.createProject`)
and *read/listed*, but there is **no way to delete one**. A project accumulates
monitors, heartbeats, incidents, SLA config, escalation/on-call, channels, tokens,
webhooks and (optionally) file-provider bundles; today the only way to remove a
mistaken or decommissioned project is direct DB surgery. The Org-Admin role text in
`MembersPanel.vue` already advertises *"Create/delete projects"*, so the capability is
expected but unimplemented.

This spec adds a first-class, RBAC-guarded, irreversible **delete project** operation.

## 2. Requirement

**FR-018** — An organization admin can permanently delete a project they administer.
Deletion removes the project and all data owned by it (monitors and their history,
incidents, SLA, escalation/on-call, notification channels, project-scoped tokens and
webhooks, status pages scoped to the project). The action is irreversible, requires an
explicit type-to-confirm step, is audited, and is refused for projects owned by a file
provider.

## 3. Scope & non-goals

**In scope**
- `DELETE /api/v1/projects/{projectID}` → `204 No Content`.
- Store `DeleteProject(ctx, orgID, projectID)` — single tenant-scoped delete, DB cascade.
- Org-admin (and global-admin) authorization; 403 otherwise, 404 when not visible.
- Guard: refuse (`409 managed_by_file`) when the project has file-provider-managed
  monitors or bundles (a reconcile would recreate it — see §7.3).
- Audit event `project.delete` (org-scoped).
- Frontend: a **Danger Zone** in project-scoped Settings with a type-the-slug confirm
  modal; `ws.deleteProject` + post-delete workspace switch.
- OpenAPI + regenerated TS schema.

**Non-goals**
- No soft-delete / archive / trash / restore (hard delete only — matches the schema,
  which is `ON DELETE CASCADE` throughout; see §5).
- No "last project in org" guard — an org may legitimately have zero projects.
- No org deletion (separate feature).
- No bulk / multi-project deletion.
- No new migration — the cascade already exists (§5).

## 4. Behavior & semantics

1. **Hard delete via DB cascade.** A single `DELETE FROM projects WHERE id=$1 AND
   org_id=$2` removes the project row; every child table's `ON DELETE CASCADE` fanout
   (§5) wipes owned data atomically inside one transaction. No application-level
   multi-table deletion is needed.
2. **Tenant scoping.** The delete is always scoped by `org_id` **and** `project_id`;
   `0` rows affected → `404` (never a cross-tenant delete).
3. **Confirmation.** The UI requires typing the project **slug** to enable the delete
   button (mirrors the create dialog's slug centrality and GitHub-style destructive
   confirms). The API itself does not require a confirm token — the guard is RBAC.
4. **Audit.** One `project.delete` audit row (actor, org_id, project_id, slug/name),
   written in the same transaction as the delete.
5. **In-flight checks.** The scheduler works off an in-memory snapshot (15 s refresh)
   plus `monitor_config_changed`; removed monitors drop out on the next refresh. A late
   heartbeat insert for an already-deleted monitor simply fails its FK and is logged —
   no corruption. (No explicit NOTIFY is required, but see §9 open question.)

## 5. Cascade map (already enforced by the schema — verified)

Every FK to `projects(id[,org_id])` is `ON DELETE CASCADE`, and the second level
(monitors → their children) cascades too. Deleting one project row therefore removes:

| Table (migration) | Link | Note |
| --- | --- | --- |
| `monitors` (00003) | `project_id` CASCADE | → cascades to its own children below |
| `heartbeats` (00017 partitions / 00043 hypertable) | `monitor_id` CASCADE | all history |
| `heartbeats_daily` rollups (00015) | `monitor_id` CASCADE | |
| `monitor_dependencies` (00046) | `monitor_id`, `depends_on_id` CASCADE | incl. cross-project edges pointing in |
| `notifications` (00011) | `project_id` + `monitor_id` CASCADE | |
| `incidents` (00006) | `project_id` CASCADE | (+ `incident_updates` via incident FK) |
| `sla` config + reports (00005) | `project_id`/`monitor_id` CASCADE | |
| `escalation_policies`, `oncall_schedules` (00035) | `project_id` CASCADE | |
| `notification_channels` (00001) | `(project_id,org_id)` CASCADE | |
| `status_pages` (00007) | `project_id` CASCADE (nullable) | only **project-scoped** pages; org-level pages (`project_id NULL`) survive |
| `api_tokens` (00008) | `project_id` CASCADE (nullable) | only project-scoped tokens; org-scoped tokens survive |
| `webhooks` (00010) | `project_id` CASCADE (nullable) | project-scoped only |
| `file_provider_bundles` (00059), `managed_monitors` (00060) | composite CASCADE | but see §7.3 guard |

**Side effect to document (not a bug):** org-level status-page **components** that
reference a deleted monitor have `monitor_id` set to `NULL` (`status_pages` 00007:26
`ON DELETE SET NULL`) — such components become detached "manual" components rather than
disappearing. Incidents' `monitor_id` is likewise `SET NULL` (00009) but the incidents
themselves are project-scoped and cascade-deleted anyway.

## 6. API

```
DELETE /api/v1/projects/{projectID}
  → 204 No Content            deleted
  → 403 Forbidden             caller is not an admin of the owning org
  → 404 Not Found             project not visible to the caller / wrong tenant
  → 409 managed_by_file       project has file-provider-managed monitors/bundles
```

Mounted in `internal/api/api.go` next to the existing project routes
(`GET /api/v1/projects/{projectID}` at line 373). Handler `h.deleteProject` in a new or
existing `handlers_projects.go`, behind the standard session-auth middleware. The
`{orgID}` is resolved from the project (not taken from the path) so the tenant guard
uses the project's real owner.

## 7. Guards & edge cases

### 7.1 Authorization
Require **org admin** of the project's org (global admin passes via `InOrg`). A
project-scoped Project-Admin may administer content *inside* a project but may **not**
delete the project itself (deletion is an org-lifecycle action) — consistent with the
role matrix wording. 403 for anyone below org admin.

### 7.2 Visibility
Non-members (and project-scoped members whose principal cannot see the project) get
**404**, never 403 — matching the codebase's "invisible ⇒ 404" convention.

### 7.3 File-provider-owned projects (important)
If the project owns any `managed_monitors` / `file_provider_bundles`, a running file
provider would **recreate** the project + monitors from its YAML on the next reconcile,
making deletion futile and confusing. Refuse with **`409 managed_by_file`** (reuse the
existing error contract used on managed-monitor mutation) and tell the operator to
remove the provider's files instead. Checked inside the delete transaction to avoid a
TOCTOU with a concurrent apply.

### 7.4 Concurrency
Delete runs in one transaction; the `409` file-provider check and the `DELETE` share the
tx so an interleaved reconcile cannot slip monitors back in between check and delete.

### 7.5 No "last project" block
Deleting the org's only project is allowed (org can be empty). The UI warns but does not
prevent it.

## 8. Store

```go
// DeleteProject removes a project and (via ON DELETE CASCADE) everything it owns.
// Scoped by org for tenant safety. Returns ErrNotFound when 0 rows match.
// Returns ErrManagedByFile when the project has file-provider-managed monitors/bundles.
func (s *Store) DeleteProject(ctx context.Context, orgID, projectID string) error
```
- One tx: (a) `SELECT 1 FROM managed_monitors … / file_provider_bundles …` guard →
  `ErrManagedByFile`; (b) `DELETE FROM projects WHERE id=$1 AND org_id=$2` → `ErrNotFound`
  if `RowsAffected()==0`; (c) `RecordAudit(tx, actor, orgID, "project.delete", projectID, …)`.
- Adding `DeleteProject` to the `Store` interface in `internal/api/api.go` will break
  `fakeStore` in `internal/api/api_test.go` (intentional — implement the stub). The
  scheduler/outbox fakes do not include project-lifecycle methods and are unaffected.

## 9. Open questions (resolve during review)

- **Q1 — snapshot signal.** Do we NOTIFY `monitor_confirm`/`monitor_config_changed`
  after delete to nudge the scheduler, or rely on the 15 s snapshot refresh? Proposal:
  rely on the refresh (delete is rare, 15 s latency to stop probing a gone monitor is
  fine); revisit if E2E shows noisy FK-failure logs from in-flight heartbeats.
- **Q2 — the stray OpenAPI `delete:`** under `/api/v1/organizations/{orgID}/projects`
  is mislabeled *"Remove a member"* (a spec artifact). Fix/relocate it as a drive-by in
  this iteration (it is unrelated to project deletion but confusing in the same area).

## 10. Frontend

- **Placement.** Project-scoped **Settings → Danger Zone** (a small red-accented card at
  the bottom of the project settings surface), matching the token/panel styles already
  used for Tokens/Webhooks. Not in the create dialog.
- **Confirm modal.** Reuses the create-dialog visual language: a modal that names the
  project, lists what will be destroyed (monitors N, incidents, history, channels…),
  and requires typing the exact **slug** to enable a red **Delete project** button.
- **Store.** `ws.deleteProject(id)` in `stores/workspace.ts` (symmetric to
  `createProject`): `DELETE`, then on success remove it from `projects`, switch to
  another project (or clear `projectId`), and route to `dashboard`.
- **Errors.** `409 managed_by_file` → friendly message ("This project is managed by a
  file provider — remove its config files to delete it."). `403` → the existing
  not-authorized pattern.
- **UI mock required & approved before any frontend code** (project convention).

## 11. RBAC summary

| Caller | Result |
| --- | --- |
| Global admin | 204 |
| Org admin (owning org) | 204 |
| Project admin / lower / other org | 403 (member) / 404 (not visible) |

## 12. Verification

1. `go vet`, `go build`, `go test -race -count=1 ./...` in **both storage modes**
   (timescale `cerbix_test` + throwaway `postgres:16-alpine`) — deletion touches
   `heartbeats`, which is adaptive, so the cascade must be exercised in both.
2. New store tests: `DeleteProject` happy path (cascade actually removes monitors +
   heartbeats + incidents + channels), tenant scoping (wrong org ⇒ ErrNotFound),
   `ErrManagedByFile` when file-owned, and that **org-level** status pages / org-scoped
   tokens survive.
3. API tests: 204 for org/global admin; 403 for project-admin/member; 404 for
   outsider/unknown; 409 for a file-managed project.
4. Frontend: docker `npm run build` (vue-tsc).
5. **E2E** on a live stack (`e2e/run.sh`): create an `e2e-`prefixed project + a monitor,
   open Settings → Danger Zone, type-slug-confirm delete, assert it disappears from the
   switcher and its monitors/incidents 404; assert a non-admin cannot see the control.

## 13. Deliverables (process)

- This spec (`docs/specs/func-project-deletion.md`).
- Approved UI mock (artifact) **before** frontend code.
- Implementation (store + API + OpenAPI + TS + frontend).
- `-race` (both DB modes) + E2E on a live stack.
- `docs/iterations/iter-0111.md`, decision **D-0150** (hard-delete via cascade +
  type-slug confirm + org-admin guard + file-provider refusal), a row in
  `docs/traceability.md` (FR-018), and an FR-018 line in `docs/status.md`.
