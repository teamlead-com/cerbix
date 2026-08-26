import { flushPromises, mount } from "@vue/test-utils";
import { describe, expect, it, vi } from "vitest";

import SearchBox from "@/components/SearchBox.vue";

// A search hit carries its own org and project, and every hit type must switch the workspace to
// them before navigating.
//
// It used to switch for PROJECT hits alone. A monitor or incident hit from another workspace then
// landed on a detail page whose reads and permission checks still ran against the previous tenant:
// the successor and convert pickers listed a stranger's services, and `canWrite` compared the
// workspace's org against the loaded subject's project, so a legitimate editor of the target saw
// acknowledge, resolve and postmortem disappear. Two entrances, one defect.

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), PATCH: vi.fn(), PUT: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));

const router = vi.hoisted(() => ({ push: vi.fn() }));
vi.mock("vue-router", () => ({ useRouter: () => router }));

const ws = vi.hoisted(() => ({
  orgId: "org-a",
  projectId: "proj-a",
  selectOrg: vi.fn(async (id: string) => {
    ws.orgId = id;
  }),
  selectProject: vi.fn((id: string) => {
    ws.projectId = id;
  }),
}));
vi.mock("@/stores/workspace", () => ({ useWorkspace: () => ws }));

async function pickFirstHit(hit: Record<string, unknown>) {
  ws.orgId = "org-a";
  ws.projectId = "proj-a";
  ws.selectOrg.mockClear();
  ws.selectProject.mockClear();
  router.push.mockClear();
  apiMock.GET.mockReset();
  apiMock.GET.mockResolvedValue({ data: { hits: [hit] } });

  const w = mount(SearchBox);
  const input = w.get("input");
  await input.setValue("check");
  await input.trigger("input");
  // The box debounces; the timer is real and short, so wait for it rather than faking the clock.
  await new Promise((r) => setTimeout(r, 260));
  await flushPromises();
  const buttons = w.findAll("button");
  expect(buttons.length).toBeGreaterThan(0);
  await buttons[0].trigger("click");
  await flushPromises();
  return w;
}

describe("a search hit brings its tenant with it", () => {
  it("switches org and project for a MONITOR hit in another workspace", async () => {
    await pickFirstHit({ type: "monitor", id: "mon-b", name: "checkout", org_id: "org-b", project_id: "proj-b" });

    expect(ws.selectOrg).toHaveBeenCalledWith("org-b");
    expect(ws.selectProject).toHaveBeenCalledWith("proj-b");
    expect(router.push).toHaveBeenCalledWith({ name: "monitor", params: { id: "mon-b" } });
  });

  it("switches org and project for an INCIDENT hit in another workspace", async () => {
    await pickFirstHit({ type: "incident", id: "inc-b", name: "outage", org_id: "org-b", project_id: "proj-b" });

    expect(ws.selectOrg).toHaveBeenCalledWith("org-b");
    expect(ws.selectProject).toHaveBeenCalledWith("proj-b");
    expect(router.push).toHaveBeenCalledWith({ name: "incident", params: { id: "inc-b" } });
  });

  it("does not churn the workspace for a hit already in it", async () => {
    await pickFirstHit({ type: "monitor", id: "mon-a", name: "checkout", org_id: "org-a", project_id: "proj-a" });

    expect(ws.selectOrg).not.toHaveBeenCalled();
    expect(ws.selectProject).not.toHaveBeenCalled();
    expect(router.push).toHaveBeenCalledWith({ name: "monitor", params: { id: "mon-a" } });
  });
});
