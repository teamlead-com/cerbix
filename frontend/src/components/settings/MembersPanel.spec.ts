import { flushPromises, mount } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { beforeEach, describe, expect, it, vi } from "vitest";

import MembersPanel from "@/components/settings/MembersPanel.vue";
import { useSession } from "@/stores/session";
import { useWorkspace } from "@/stores/workspace";

// iter-0155 extracted the audit row into AuditRows so the instance trail and this org trail share ONE
// grammar. That refactor had no regression guard at all — this is it, written when the gap was noticed
// rather than after it cost something. What it pins is the org panel's own contract through the shared
// component: it reads the ORG endpoint, renders its own vocabulary, and marks a removal as destructive.
const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), PATCH: vi.fn(), PUT: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));

const audit = [
  { id: "e1", org_id: "o1", action: "member.add", target: "viewer", actor_name: "Ada", via_token: false, created_at: new Date().toISOString() },
  { id: "e2", org_id: "o1", action: "member.remove", target: "user x", actor_name: "", actor_email: "", via_token: false, created_at: new Date().toISOString() },
];

beforeEach(() => {
  setActivePinia(createPinia());
  const ws = useWorkspace();
  ws.orgId = "o1";
  ws.projectId = "p1";
  // isOrgAdmin reads the store's own memberships list and wants the ORG-level role, not a project one.
  const s = useSession();
  s.memberships = [{ org_id: "o1", role: "org_admin" }] as never;
  apiMock.GET.mockReset();
  apiMock.GET.mockImplementation(async (path: string) => {
    // The panel calls ws.init() first, which re-reads the org list — an empty answer there would
    // clear ws.orgId and the members read would bail before the audit trail is ever fetched.
    if (path === "/api/v1/organizations") return { data: [{ id: "o1", slug: "acme", name: "Acme" }] };
    if (path === "/api/v1/organizations/{orgID}/projects") return { data: [{ id: "p1", slug: "api", name: "API" }] };
    if (path === "/api/v1/organizations/{orgID}/audit") return { data: audit };
    if (path === "/api/v1/organizations/{orgID}/members") return { data: [] };
    return { data: [] };
  });
});

describe("MembersPanel audit trail", () => {
  it("reads the ORG endpoint and renders its own vocabulary through the shared rows", async () => {
    const w = mount(MembersPanel);
    await flushPromises();

    const paths = apiMock.GET.mock.calls.map((c) => c[0]);
    expect(paths).toContain("/api/v1/organizations/{orgID}/audit");
    // The org panel must never reach the instance listing — that is a global-admin surface.
    expect(paths.some((p: string) => p.includes("/admin/audit"))).toBe(false);

    const rows = w.findAll('[data-testid="audit-row"]');
    expect(rows).toHaveLength(2);
    expect(rows[0].text()).toContain("added a member");
    // An actor that cannot be resolved reads as itself, never as a borrowed human name.
    expect(rows[1].text()).toContain("machine");
    // A removal carries the destructive dot; an addition does not.
    expect(rows[1].html()).toContain("bg-down");
    expect(rows[0].html()).toContain("bg-accent");
  });
});
