import { describe, expect, it } from "vitest";

import {
  READ_ACTION,
  ROLE_GRANTS,
  TOKEN_ACTIONS,
  actionsFor,
  actionsReadonlyLabel,
  neededRole,
  omitsRead,
  pruneActions,
  roleGrants,
  roleGrantsAction,
  type Role,
  type TokenAction,
} from "@/lib/tokenActions";

// FR-025 D12 / D-0210 item 6 / D-0212 item 1: the token form offers ONLY what the picked role grants,
// because the server refuses anything else at creation (400 `action_not_granted`).
//
// The ORACLE below is `roleGrants` in internal/authz/authz.go, transcribed by hand from the Go map —
// not imported from the module under test. If the Go table and lib/tokenActions.ts ever diverge, this
// file fails and names the role; that is the whole point of writing it twice.

const AUTHZ_GO_ROLE_GRANTS: Record<Role, string[]> = {
  // domain.RoleOrgAdmin
  org_admin: ["org:read", "org:manage", "project:read", "project:manage", "project:write", "gate:evaluate", "gate:policy:write", "gate:override", "change:record"],
  // domain.RoleProjectAdmin
  project_admin: ["project:read", "project:manage", "project:write", "gate:evaluate", "gate:policy:write", "gate:override", "change:record"],
  // domain.RoleEditor
  editor: ["org:read", "project:read", "project:write", "gate:evaluate", "gate:policy:write", "change:record"],
  // domain.RoleViewer
  viewer: ["org:read", "project:read", "gate:evaluate"],
};

const ROLES = Object.keys(AUTHZ_GO_ROLE_GRANTS) as Role[];
const sorted = (xs: readonly string[]) => [...xs].sort();

describe("tokenActions — ROLE_GRANTS mirrors internal/authz/authz.go, grant for grant", () => {
  it("the same four roles, and the same action set under each", () => {
    expect(sorted(Object.keys(ROLE_GRANTS))).toEqual(sorted(ROLES));
    for (const role of ROLES) {
      expect(sorted(ROLE_GRANTS[role]), `roleGrants[${role}] must match internal/authz/authz.go`).toEqual(sorted(AUTHZ_GO_ROLE_GRANTS[role]));
    }
  });

  it("every granted action is in the catalogue, and the catalogue holds nothing no role grants", () => {
    const granted = new Set(ROLES.flatMap((r) => AUTHZ_GO_ROLE_GRANTS[r]));
    for (const role of ROLES) for (const a of ROLE_GRANTS[role]) expect(TOKEN_ACTIONS as readonly string[], `${a} is not in the catalogue`).toContain(a);
    for (const a of TOKEN_ACTIONS) expect(granted, `${a} is offered by no role — it could only ever be refused`).toContain(a);
  });

  it("the ladder is a ladder: a wider role grants everything a narrower one does", () => {
    const ladder: Role[] = ["viewer", "editor", "project_admin", "org_admin"];
    // project_admin drops `org:read` by design (it is not org-wide), so the containment is checked
    // where the grants are actually nested: viewer ⊂ editor, and project_admin ⊂ org_admin.
    for (const a of ROLE_GRANTS.viewer) expect(ROLE_GRANTS.editor, `editor must grant ${a}`).toContain(a);
    for (const a of ROLE_GRANTS.project_admin) expect(ROLE_GRANTS.org_admin, `org_admin must grant ${a}`).toContain(a);
    expect(ROLE_GRANTS.project_admin, "project_admin is a PROJECT role: no org read").not.toContain("org:read");
    expect(ladder.map((r) => roleGrants(r).length)).toEqual([3, 6, 7, 9]);
  });

  it("an unknown role grants nothing — a form for a role this SPA does not know offers no chip", () => {
    expect(roleGrants("superuser")).toEqual([]);
    expect(roleGrants(undefined)).toEqual([]);
    expect(roleGrantsAction("superuser", "project:read")).toBe(false);
  });
});

describe("tokenActions — actionsFor: what the form offers, and what it shows dormant", () => {
  it("every role splits the whole catalogue into offered and lacking, in the catalogue's order", () => {
    for (const role of ROLES) {
      const { offered, lacking } = actionsFor(role);
      expect(offered, `${role}: offered must be exactly the role's grants`).toEqual(TOKEN_ACTIONS.filter((a) => AUTHZ_GO_ROLE_GRANTS[role].includes(a)));
      expect([...offered, ...lacking.map((l) => l.action)].sort()).toEqual([...TOKEN_ACTIONS].sort());
      expect(offered.filter((a) => lacking.some((l) => l.action === a)), "nothing is both").toEqual([]);
    }
  });

  it("a viewer is offered the three reads and is told which role each other action needs", () => {
    const { offered, lacking } = actionsFor("viewer");
    expect(offered).toEqual(["project:read", "gate:evaluate", "org:read"]);
    expect(lacking.map((l) => [l.action, l.needs])).toEqual([
      ["project:write", "editor"],
      ["project:manage", "project_admin"],
      ["gate:policy:write", "editor"],
      ["gate:override", "project_admin"],
      ["change:record", "editor"],
      ["org:manage", "org_admin"],
    ]);
  });

  it("`change:record` — D12's CI token — is offered from editor up, and never to a viewer", () => {
    expect(actionsFor("viewer").offered).not.toContain("change:record");
    expect(actionsFor("viewer").lacking.find((l) => l.action === "change:record")!.needs).toBe("editor");
    for (const role of ["editor", "project_admin", "org_admin"] as Role[]) expect(actionsFor(role).offered).toContain("change:record");
    expect(neededRole("change:record")).toBe("editor");
    expect(neededRole("gate:override")).toBe("project_admin");
    expect(neededRole("org:manage")).toBe("org_admin");
    expect(neededRole("project:read")).toBe("viewer");
  });

  it("an unknown role offers nothing and lacks everything — no chip that could only 400", () => {
    const { offered, lacking } = actionsFor("superuser");
    expect(offered).toEqual([]);
    expect(lacking).toHaveLength(TOKEN_ACTIONS.length);
  });
});

describe("tokenActions — pruneActions: a role change can only NARROW the list", () => {
  it("what the new role does not grant is dropped; the catalogue's order is kept", () => {
    const picked: TokenAction[] = ["gate:evaluate", "change:record", "project:read"];
    expect(pruneActions(picked, "editor")).toEqual(["project:read", "gate:evaluate", "change:record"]);
    expect(pruneActions(picked, "viewer"), "a viewer grants no change:record").toEqual(["project:read", "gate:evaluate"]);
    expect(pruneActions(["org:manage", "gate:override"], "editor")).toEqual([]);
    expect(pruneActions(["org:manage", "gate:override"], "org_admin")).toEqual(["gate:override", "org:manage"]);
  });

  it("nothing outside the catalogue survives, and nothing is invented", () => {
    expect(pruneActions(["project:read", "not:an:action"], "org_admin")).toEqual(["project:read"]);
    expect(pruneActions([], "org_admin"), "an empty list stays empty — 'role decides'").toEqual([]);
    expect(pruneActions(["project:read"], "superuser")).toEqual([]);
  });

  it("pruning is idempotent and never widens what a narrowing already dropped", () => {
    const once = pruneActions(["project:read", "gate:override"], "editor");
    expect(once).toEqual(["project:read"]);
    expect(pruneActions(once, "org_admin"), "widening the role does not bring back a dropped chip").toEqual(["project:read"]);
  });
});

describe("tokenActions — the read model and the warning", () => {
  it("the warning fires exactly when the list is ON and leaves project:read out", () => {
    expect(READ_ACTION).toBe("project:read");
    expect(omitsRead([])).toBe(false);
    expect(omitsRead(["project:read", "change:record"])).toBe(false);
    expect(omitsRead(["gate:evaluate", "change:record"]), "D12's CI token: it cannot read the service page").toBe(true);
  });

  it("the immutable list is shown as it is; a token that predates D12 says `role decides`", () => {
    expect(actionsReadonlyLabel(null)).toBe("role decides");
    expect(actionsReadonlyLabel(undefined)).toBe("role decides");
    expect(actionsReadonlyLabel([])).toBe("none");
    expect(actionsReadonlyLabel(["gate:evaluate", "change:record"])).toBe("gate:evaluate, change:record");
  });
});
