import { createPinia, setActivePinia } from "pinia";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { useSession } from "@/stores/session";

vi.mock("@/api/client", () => ({ api: { GET: vi.fn() } }));

// FR-024 D-0207 item 3: `canProjectAdmin` is the ONE predicate that separates editor from
// project_admin — the override controls render on its answer and nowhere is a role string compared.
// `canProjectWrite` (editor+) is unchanged by its arrival.

type Membership = { org_id: string; project_id?: string | null; role: string };

function sessionWith(memberships: Membership[], globalAdmin = false) {
  const s = useSession();
  s.user = { id: "u1", email: "u@example.com", is_global_admin: globalAdmin } as typeof s.user;
  s.memberships = memberships as typeof s.memberships;
  return s;
}

describe("session.canProjectAdmin", () => {
  beforeEach(() => setActivePinia(createPinia()));

  it("is true for a global admin regardless of memberships", () => {
    const s = sessionWith([], true);
    expect(s.canProjectAdmin("o1", "p1")).toBe(true);
    expect(s.canProjectWrite("o1", "p1")).toBe(true);
  });

  it("is true for an org-level org_admin of that org", () => {
    const s = sessionWith([{ org_id: "o1", project_id: null, role: "org_admin" }]);
    expect(s.canProjectAdmin("o1", "p1")).toBe(true);
    expect(s.canProjectAdmin("o1", "p2"), "org-level: every project of the org").toBe(true);
    expect(s.canProjectAdmin("o2", "p1"), "not another org").toBe(false);
  });

  it("is true for a project_admin of THAT project only", () => {
    const s = sessionWith([{ org_id: "o1", project_id: "p1", role: "project_admin" }]);
    expect(s.canProjectAdmin("o1", "p1")).toBe(true);
    expect(s.canProjectAdmin("o1", "p2"), "another project of the same org").toBe(false);
    expect(s.canProjectAdmin("o2", "p1")).toBe(false);
  });

  it("is false for an editor and a viewer — who may still WRITE (editor) but never override", () => {
    const editor = sessionWith([{ org_id: "o1", project_id: "p1", role: "editor" }]);
    expect(editor.canProjectAdmin("o1", "p1")).toBe(false);
    expect(editor.canProjectWrite("o1", "p1"), "canProjectWrite is unchanged: an editor writes").toBe(true);

    setActivePinia(createPinia());
    const orgEditor = sessionWith([{ org_id: "o1", project_id: null, role: "editor" }]);
    expect(orgEditor.canProjectAdmin("o1", "p1")).toBe(false);
    expect(orgEditor.canProjectWrite("o1", "p1")).toBe(true);

    setActivePinia(createPinia());
    const viewer = sessionWith([{ org_id: "o1", project_id: "p1", role: "viewer" }]);
    expect(viewer.canProjectAdmin("o1", "p1")).toBe(false);
    expect(viewer.canProjectWrite("o1", "p1")).toBe(false);
  });

  it("is false with no user at all", () => {
    const s = useSession();
    expect(s.canProjectAdmin("o1", "p1")).toBe(false);
    expect(s.canProjectWrite("o1", "p1")).toBe(false);
  });
});
