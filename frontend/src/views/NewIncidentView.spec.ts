import { flushPromises, mount } from "@vue/test-utils";
import { beforeEach, describe, expect, it, vi } from "vitest";

import NewIncidentView from "@/views/NewIncidentView.vue";

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn() }));
const routerMock = vi.hoisted(() => ({ push: vi.fn() }));
const workspaceMock = vi.hoisted(() => ({
  init: vi.fn(() => Promise.resolve()),
  orgId: "org-a",
  orgName: "Example Organization",
  projectId: "project-a",
  projectName: "Project A",
  projects: [
    { id: "project-a", name: "Project A" },
    { id: "project-b", name: "Project B" },
  ],
}));
const sessionMock = vi.hoisted(() => ({ canProjectWrite: vi.fn(() => true) }));

vi.mock("@/api/client", () => ({ api: apiMock }));
vi.mock("vue-router", () => ({ useRouter: () => routerMock }));
vi.mock("@/stores/workspace", () => ({ useWorkspace: () => workspaceMock }));
vi.mock("@/stores/session", () => ({ useSession: () => sessionMock }));
vi.mock("@/components/AppShell.vue", () => ({
  default: {
    name: "AppShell",
    props: { crumbs: { type: Array, default: () => [] } },
    template: `
      <div>
        <nav data-testid="breadcrumbs">
          <template v-for="crumb in crumbs" :key="crumb.label">
            <a v-if="crumb.to" :data-route="crumb.to.name">{{ crumb.label }}</a>
            <span v-else>{{ crumb.label }}</span>
          </template>
        </nav>
        <slot name="actions" /><slot />
      </div>
    `,
  },
}));

describe("NewIncidentView breadcrumb scope", () => {
  beforeEach(() => {
    apiMock.GET.mockReset();
    apiMock.GET.mockResolvedValue({ data: [] });
    apiMock.POST.mockReset();
    routerMock.push.mockReset();
    workspaceMock.init.mockClear();
    workspaceMock.projectId = "project-a";
    workspaceMock.projectName = "Project A";
  });

  it("does not present a selectable cross-project form project as a false dashboard parent", async () => {
    const wrapper = mount(NewIncidentView, {
      global: { stubs: { RouterLink: { props: ["to"], template: "<a><slot /></a>" } } },
    });
    await flushPromises();

    await wrapper.findAll("select")[0].setValue("project-b");
    await flushPromises();

    const breadcrumbs = wrapper.get("[data-testid='breadcrumbs']");
    expect(breadcrumbs.text()).toContain("Example Organization");
    expect(breadcrumbs.text()).toContain("New incident");
    expect(breadcrumbs.find("[data-route='dashboard']").exists()).toBe(false);
    expect(breadcrumbs.find("[data-route='incidents']").exists()).toBe(false);
    expect(breadcrumbs.text()).not.toContain("Project B");
  });
});
