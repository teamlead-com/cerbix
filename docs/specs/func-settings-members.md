# Spec: Members moves into Settings, "Administration" group (func-settings-members)

## Purpose

Consolidate the information architecture of the SPA: the org-scoped Members screen moves
from the sidebar ("Manage" section, where it is the only item) into Settings as a tab in
the **Organization** group, and the global-admin-only tab group formerly labeled
"Instance" is renamed **Administration** — it will also host the upcoming Users page
(`func-admin-users`). Frontend-only; no API changes.

## Current state

- Sidebar (`frontend/src/components/AppShell.vue`): section "Manage" contains exactly one
  item — Members (`/members` route → `views/MembersView.vue`).
- `MembersView` is already org-scoped: everything keys off `ws.orgId` (workspace store),
  org switch reloads the list. Endpoints used: `GET/POST /organizations/{orgID}/members`,
  `PATCH/DELETE .../members/{membershipID}`, `GET .../audit`.
- `views/SettingsView.vue`: single route, tabs are local state grouped by scope —
  Project / Instance (spread in only for `session.isGlobalAdmin`) / Organization / Account.

## Target state

Settings tab groups:

| Group | Visible to | Tabs |
|---|---|---|
| Project | by project role | Notification channels, Incoming alerts |
| Organization | org members (manage: org_admin) | **Members** (new tab, first), API tokens, Webhooks |
| **Administration** (renamed from Instance) | global admin only | Authentication, Branding, Email, Alerting, Monitor defaults (+ Users later) |
| Account | everyone | Security |

## Requirements

1. `scopeLabels.instance` display label becomes `"Administration"`; the internal scope key
   stays `instance` (no data or API impact).
2. Members becomes a Settings tab with `scope: "org"`, listed first in the Organization
   group. Behavior is unchanged: invite (email + org/project scope + role), searchable
   member table with role editing and removal (org_admin only), role-permission matrix,
   audit log. Org switch in the header reloads the tab.
3. The Members UI is extracted into a reusable component
   (`components/settings/MembersPanel.vue`) instead of inlining into the already-large
   `SettingsView.vue`; `views/MembersView.vue` and the `/members` route are removed.
4. `/members` redirects to `/settings?tab=members` so old bookmarks keep working.
5. `SettingsView` initializes the active tab from `route.query.tab` when it names a tab
   the current user can see; invalid/forbidden values fall back to the default tab.
6. The sidebar "Manage" section is removed (Members was its only item). No other sidebar
   changes; Dead-letter and Settings pinned links stay as they are.
7. Non-managers keep the read-only view; 403 from the API keeps the existing friendly
   message pattern.

## Out of scope

The Users administration page (own spec: `func-admin-users`), any backend change,
URL-driven sub-routes for every Settings tab (only the `?tab=` entry point is added).

## Acceptance

- `docker … npm run build` (vue-tsc) passes.
- E2E on a live stack: Members tab appears under Organization for an org member; invite /
  role change / remove still work; org switcher reloads the tab; `/members` redirects to
  the tab; the "Administration" group label shows only for a global admin; the "Manage"
  sidebar section is gone.
