import { flushPromises, mount } from "@vue/test-utils";
import { describe, expect, it, vi } from "vitest";

import OrgDangerZonePanel from "@/components/settings/OrgDangerZonePanel.vue";

const workspaceMock = vi.hoisted(() => ({
  currentOrg: { id: "org-a", slug: "org-a", name: "Org A" },
  deleteOrg: vi.fn(),
}));
const routerMock = vi.hoisted(() => ({ push: vi.fn() }));

vi.mock("@/stores/workspace", () => ({ useWorkspace: () => workspaceMock }));
vi.mock("vue-router", () => ({ useRouter: () => routerMock }));

describe("OrgDangerZonePanel", () => {
  it("clears its busy state when replacement loading rejects after deletion", async () => {
    workspaceMock.deleteOrg.mockRejectedValueOnce(new Error("Could not load projects for the selected organization."));
    const wrapper = mount(OrgDangerZonePanel);

    await wrapper.find("button").trigger("click");
    await wrapper.find("input").setValue("org-a");
    const vm = wrapper.vm as unknown as { confirmDelete: () => Promise<void> };

    await expect(vm.confirmDelete()).resolves.toBeUndefined();
    await flushPromises();

    expect(wrapper.text()).toContain("Could not load projects for the selected organization.");
    const deleteButton = wrapper.findAll("button").find((button) => button.text().includes("Delete organization"));
    expect(deleteButton?.element.disabled).toBe(false);
    expect(routerMock.push).not.toHaveBeenCalled();
  });
});
