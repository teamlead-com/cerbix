import { flushPromises, mount } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { beforeEach, describe, expect, it, vi } from "vitest";

import ProjectMembersPanel from "@/components/settings/ProjectMembersPanel.vue";
import { useSession } from "@/stores/session";
import { useWorkspace } from "@/stores/workspace";

// iter-0206 (FR-040, func-project-members.md): the project Members panel reads and writes the
// PROJECT endpoints, offers project roles only, never sends a project_id/org_id of its own, keeps the
// organization-wide access read-only, and hands a change to the user's OWN membership to its parent.
const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), PATCH: vi.fn(), PUT: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));

const members = [
  { id: "m-me", user_id: "me", org_id: "o1", project_id: "p1", role: "project_admin", email: "me@example.com", display_name: "Me" },
  { id: "m-theo", user_id: "theo", org_id: "o1", project_id: "p1", role: "editor", email: "theo@example.com", display_name: "Theo" },
];
const inherited = [{ role: "org_admin", email: "ana@example.com", display_name: "Ana" }];

beforeEach(() => {
  setActivePinia(createPinia());
  const ws = useWorkspace();
  ws.orgId = "o1";
  ws.projectId = "p1";
  const s = useSession();
  s.user = { id: "me", email: "me@example.com" } as never;
  s.memberships = [{ org_id: "o1", project_id: "p1", role: "project_admin" }] as never;
  for (const f of Object.values(apiMock)) f.mockReset();
  apiMock.GET.mockImplementation(async (path: string) => {
    if (path === "/api/v1/organizations") return { data: [{ id: "o1", slug: "acme", name: "Acme" }] };
    if (path === "/api/v1/organizations/{orgID}/projects") return { data: [{ id: "p1", slug: "api", name: "API" }] };
    if (path === "/api/v1/projects/{projectID}/members") return { data: { members, inherited } };
    return { data: [] };
  });
});

describe("ProjectMembersPanel", () => {
  it("reads the PROJECT endpoint and shows organization access read-only", async () => {
    const w = mount(ProjectMembersPanel);
    await flushPromises();

    const paths = apiMock.GET.mock.calls.map((c) => c[0]);
    expect(paths).toContain("/api/v1/projects/{projectID}/members");
    expect(paths).not.toContain("/api/v1/organizations/{orgID}/members");

    expect(w.findAll('[data-testid="project-member-role"]')).toHaveLength(2);
    const inh = w.get('[data-testid="project-inherited"]');
    expect(inh.text()).toContain("ana@example.com");
    expect(inh.text()).toContain("Org Admin");
    // Read-only: no role picker, no remove button in the organization block.
    expect(inh.findAll("select")).toHaveLength(0);
    expect(inh.findAll("button")).toHaveLength(0);
  });

  it("invites with project roles only and no scope of its own", async () => {
    apiMock.POST.mockResolvedValue({ data: { id: "m-new" } });
    const w = mount(ProjectMembersPanel);
    await flushPromises();
    await w.get('[data-testid="project-invite-toggle"]').trigger("click");

    const options = w.get('[data-testid="project-invite-role"]').findAll("option").map((o) => o.attributes("value"));
    expect(options).toEqual(["project_admin", "editor", "viewer"]);

    await w.get('[data-testid="project-invite-email"]').setValue("dana@example.com");
    await w.get('[data-testid="project-invite-send"]').trigger("click");
    await flushPromises();
    expect(apiMock.POST).toHaveBeenCalledTimes(1);
    const [path, opts] = apiMock.POST.mock.calls[0];
    expect(path).toBe("/api/v1/projects/{projectID}/members");
    expect(opts.params.path.projectID).toBe("p1");
    expect(opts.body).toEqual({ email: "dana@example.com", role: "editor" });
  });

  it("shows the server's 409 message for a duplicate", async () => {
    apiMock.POST.mockResolvedValue({ error: { error: "already a member of this project" } });
    const w = mount(ProjectMembersPanel);
    await flushPromises();
    await w.get('[data-testid="project-invite-toggle"]').trigger("click");
    await w.get('[data-testid="project-invite-email"]').setValue("theo@example.com");
    await w.get('[data-testid="project-invite-send"]').trigger("click");
    await flushPromises();
    expect(w.get('[data-testid="project-invite-error"]').text()).toContain("already a member of this project");
  });

  it("asks before removing the user themselves, then hands the change to the parent", async () => {
    apiMock.DELETE.mockResolvedValue({});
    const w = mount(ProjectMembersPanel);
    await flushPromises();
    const mine = w.get('[data-testid="project-member-me@example.com"]');
    await mine.get('[data-testid="project-member-remove"]').trigger("click");
    expect(mine.text()).toContain("Remove yourself?");
    await mine.get('[data-testid="project-member-confirm-remove"]').trigger("click");
    await flushPromises();
    expect(apiMock.DELETE.mock.calls[0][0]).toBe("/api/v1/projects/{projectID}/members/{membershipID}");
    expect(w.emitted("self-changed")).toHaveLength(1);
  });

  it("removing someone else stays in the panel", async () => {
    apiMock.DELETE.mockResolvedValue({});
    const w = mount(ProjectMembersPanel);
    await flushPromises();
    const theo = w.get('[data-testid="project-member-theo@example.com"]');
    await theo.get('[data-testid="project-member-remove"]').trigger("click");
    expect(theo.text()).not.toContain("Remove yourself?");
    await theo.get('[data-testid="project-member-confirm-remove"]').trigger("click");
    await flushPromises();
    expect(w.emitted("self-changed")).toBeUndefined();
    expect(w.find('[data-testid="project-member-theo@example.com"]').exists()).toBe(false);
  });
});
