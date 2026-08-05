# Spec: Multitenancy and RBAC (func-tenancy-rbac)

## Purpose

The organizations/projects model, user membership, the role matrix and strict data
isolation between unrelated orgs/projects.

## Data model (implemented in iter-0002)

- `organizations(id uuid, slug unique, name, timestamps)`.
- `projects(id uuid, org_id FK, slug, name, timestamps)`, `unique(org_id, slug)`,
  `unique(id, org_id)` (a composite target for the membership FK).
- `users(id uuid, oidc_sub unique NULL, email, display_name, is_global_admin, password_hash NULL, timestamps)`
  — `oidc_sub` = the OIDC subject claim (any issuer, D-0043/D-0044); local users — `password_hash`.
- `memberships(id, user_id FK, org_id FK, project_id NULL, role, created_at)`;
  composite FK `(project_id, org_id) → projects(id, org_id)`; a `CHECK` on the role by scope;
  partial unique indexes on `(user_id, org_id)` when `project_id IS NULL` and on
  `(user_id, org_id, project_id)` when `project_id IS NOT NULL`.

Code: `internal/domain/domain.go` (entities + `Role`/`Scope` + `Membership.Validate`),
`internal/store/*` (repositories, migration `migrations/00001_init.sql`).

## Roles (matrix)

| Role | Scope | Meaning |
|---|---|---|
| Global Admin | global | `users.is_global_admin`; access to all orgs/projects. |
| Org Admin | org | Projects/members/monitors of the entire organization. |
| Project Admin (Maintainer) | project | Management of a single project and its members. |
| Editor | org or project | Create/edit, without member management. |
| Viewer | org or project | Read-only. |

`Role.ValidForScope`: org → {org_admin, editor, viewer}; project → {project_admin, editor,
viewer}. Duplicated as a `CHECK` in the schema (single source: the domain; the schema is a protective barrier).

## Authorization (`internal/authz`, iter-0003)

`Can(action, orgID, projectID)` against the declarative matrix `role→actions`:

| Role | Actions |
|---|---|
| org_admin | OrgRead, OrgManage, ProjectRead, ProjectManage, ProjectWrite |
| project_admin | ProjectRead, ProjectManage, ProjectWrite |
| editor | OrgRead, ProjectRead, ProjectWrite |
| viewer | OrgRead, ProjectRead |

Org-scoped membership applies to the entire org and all of its projects; project-scoped — only to its
own project. `ActionGlobalManage` (creating/deleting orgs) — global admin only. `InOrg` —
org visibility under any membership (including project-only).

## Isolation (implemented: selections + HTTP enforcement)

- Selections: `ListOrganizationsForUser`, `ListProjectsForUser`; the composite FK guarantees
  that a project membership belongs to its own org.
- HTTP: middleware `auth.RequireAuth` puts a `Principal` in place; `internal/api` handlers call
  `Can`/`InOrg`. Invisible resources → 404 (existence hidden), insufficient rights in a visible
  org → 403. Lists are filtered by visibility.

## Requirements

- FR-005: CRUD of organizations (Global Admin) and projects (Org Admin) + listing/adding
  members — DONE (`internal/api`, `internal/store`).
- FR-006 / NFR-006 (**domain invariant**): all reads/writes are limited to accessible
  orgs/projects; a user without membership does not see a foreign org (404). DONE — covered by
  authz/api unit tests and store integration tests.
- Member management in the UI — iter (frontend, Phase 4).

## Open questions

- Inviting users: currently `POST members` accepts an existing `user_id`
  (the user appears after their first login). Invitation by email — later.
- Bootstrap Global Admin — via `oidc.admin_emails` (promotion on login). DONE.
