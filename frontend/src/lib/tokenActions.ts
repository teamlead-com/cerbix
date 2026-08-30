// FR-025 D12, D-0210 item 6, D-0212 item 1: the token form's optional `actions` ALLOW-LIST.
//
// The form offers ONLY the actions the picked role grants — an entry the role does not grant would be a
// token that can never do what its author wrote down, and the server refuses it (400
// `action_not_granted`) — so the SPA needs the grant table. `ROLE_GRANTS` MIRRORS `roleGrants` in
// `internal/authz/authz.go`, action for action; the server validates every list against the real table
// (`action_unknown`, `action_not_granted`), so a drift here shows as a refused form, never as a widened
// token. When authz.go gains an action or a grant, this table follows in the same change (the docs-check
// convention: a spec may not describe a grant the code lacks).

import type { components } from "@/api/schema";

export type Role = components["schemas"]["Role"];

/** The central action catalogue, in the order the chips are laid out (reads, writes, the gate, the record, the org). */
export const TOKEN_ACTIONS = [
  "project:read",
  "project:write",
  "project:manage",
  "gate:evaluate",
  "gate:policy:write",
  "gate:override",
  "change:record",
  "org:read",
  "org:manage",
] as const;
export type TokenAction = (typeof TOKEN_ACTIONS)[number];

/** Mirrors `roleGrants` in internal/authz/authz.go — the server is the authority; this only decides what to OFFER. */
export const ROLE_GRANTS: Record<Role, readonly TokenAction[]> = {
  org_admin: ["org:read", "org:manage", "project:read", "project:manage", "project:write", "gate:evaluate", "gate:policy:write", "gate:override", "change:record"],
  project_admin: ["project:read", "project:manage", "project:write", "gate:evaluate", "gate:policy:write", "gate:override", "change:record"],
  editor: ["org:read", "project:read", "project:write", "gate:evaluate", "gate:policy:write", "change:record"],
  viewer: ["org:read", "project:read", "gate:evaluate"],
};

/** The roles from the narrowest up, so `neededRole` names the LOWEST role that grants an action. */
const ROLE_LADDER: readonly Role[] = ["viewer", "editor", "project_admin", "org_admin"];

export function roleGrants(role: Role | string | undefined): readonly TokenAction[] {
  return ROLE_GRANTS[role as Role] ?? [];
}

/** Whether the role grants the action — the role half of D12's intersection, as authz.Can reads it. */
export function roleGrantsAction(role: Role | string | undefined, action: string): boolean {
  return (roleGrants(role) as readonly string[]).includes(action);
}

/** The lowest role that grants the action ("needs project_admin"), or "" when no role does. */
export function neededRole(action: TokenAction): Role | "" {
  return ROLE_LADDER.find((r) => roleGrantsAction(r, action)) ?? "";
}

/** The catalogue split for one role: what may be offered, and what is shown dormant with the role it needs. */
export function actionsFor(role: Role | string | undefined): { offered: TokenAction[]; lacking: { action: TokenAction; needs: Role | "" }[] } {
  const offered: TokenAction[] = [];
  const lacking: { action: TokenAction; needs: Role | "" }[] = [];
  for (const a of TOKEN_ACTIONS) {
    if (roleGrantsAction(role, a)) offered.push(a);
    else lacking.push({ action: a, needs: neededRole(a) });
  }
  return { offered, lacking };
}

/** The selection after a role change: only what the NEW role grants survives (the list can only narrow the role). */
export function pruneActions(selected: readonly string[], role: Role | string | undefined): TokenAction[] {
  return TOKEN_ACTIONS.filter((a) => selected.includes(a) && roleGrantsAction(role, a));
}

/** The action a narrowed token cannot do without: the service page, the timeline, every read. */
export const READ_ACTION: TokenAction = "project:read";

/** The mock's warning, shown when at least one chip is on and `project:read` is not among them. */
export const TOKEN_ACTIONS_WARNING_LEAD = "This token will not be able to read the service page.";
export const TOKEN_ACTIONS_WARNING_REST =
  "With project:read left out it can do only the actions listed and nothing else — which is the point of a CI token. It cannot be widened later; create another.";

/** True when the list is on (non-empty) and leaves `project:read` out. */
export function omitsRead(selected: readonly string[]): boolean {
  return selected.length > 0 && !selected.includes(READ_ACTION);
}

/** The read model's list as the table shows it: the entries, or "role decides" for a null list (every token that predates D12). */
export function actionsReadonlyLabel(actions: readonly string[] | null | undefined): string {
  return actions == null ? "role decides" : actions.length ? actions.join(", ") : "none";
}
