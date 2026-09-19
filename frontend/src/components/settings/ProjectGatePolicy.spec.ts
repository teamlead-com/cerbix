import { flushPromises, mount } from "@vue/test-utils";
import { beforeEach, describe, expect, it, vi } from "vitest";

import ProjectGatePolicy from "@/components/settings/ProjectGatePolicy.vue";

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), PUT: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));

const CLAUSES = {
  budget_exhausted: "block",
  budget_consumed: "warn",
  page_burn_firing: "block",
  ticket_burn_firing: "warn",
  service_incident_open: "warn",
};
const ONE_POLICY = {
  schema_version: 1,
  window: "30d",
  clauses: CLAUSES,
  budget_consumed_percent: 90,
  max_seal_lag_seconds: 900,
  unknown_behavior: "warn",
  revision: 3,
  updated_at: "2026-09-01T10:00:00Z",
  updated_by: "alice@example.com",
};

const ok = (data: unknown) => ({ data, response: new Response(null, { status: 200 }) });
const t = (wrapper: ReturnType<typeof mountPolicy>, id: string) => wrapper.find(`[data-testid="${id}"]`);

function mountPolicy() {
  return mount(ProjectGatePolicy, { props: { projectId: "p1", canManage: true } });
}
async function settle() {
  await flushPromises();
  await flushPromises();
}

beforeEach(() => {
  apiMock.GET.mockReset();
  apiMock.PUT.mockReset();
  apiMock.DELETE.mockReset();
});

describe("ProjectGatePolicy", () => {
  it("upgrades an inherited project policy to all-window schema v2 without copying a window", async () => {
    let policy = ONE_POLICY;
    apiMock.GET.mockImplementation(() => Promise.resolve(ok(policy)));
    apiMock.PUT.mockImplementation((_path: string, request: { body: Record<string, unknown> }) => {
      policy = { ...ONE_POLICY, ...request.body, revision: 4 } as typeof ONE_POLICY;
      return Promise.resolve(ok({ revision: 4 }));
    });
    const wrapper = mountPolicy();
    await settle();
    expect(t(wrapper, "project-gate-readonly").text()).toContain("One window");

    await t(wrapper, "project-gate-configure").trigger("click");
    await t(wrapper, "project-gate-window-mode-all").trigger("click");
    expect(t(wrapper, "project-gate-window").exists(), "the singular SLO selector is hidden").toBe(false);
    await wrapper.find("form").trigger("submit");
    await settle();

    expect(apiMock.PUT).toHaveBeenCalledTimes(1);
    expect(apiMock.PUT.mock.calls[0][1].body).toEqual({
      expected_revision: 3,
      schema_version: 2,
      window_mode: "all",
      clauses: CLAUSES,
      budget_consumed_percent: 90,
      max_seal_lag_seconds: 900,
      unknown_behavior: "warn",
    });
    expect(t(wrapper, "project-gate-readonly").text()).toContain("Worst of all configured windows");
    expect(t(wrapper, "project-gate-readonly").text()).not.toContain("SLO window");
  });
});
