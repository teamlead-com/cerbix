import { flushPromises, mount } from "@vue/test-utils";
import { nextTick } from "vue";
import { describe, expect, it, vi } from "vitest";

import WorkspaceSwitcher from "@/components/WorkspaceSwitcher.vue";

const wsMock = vi.hoisted(() => ({
  orgs: [
    { id: "org-a", name: "Org A" },
    { id: "org-b", name: "Org B" },
  ],
  projects: [{ id: "old-project", name: "Old project" }],
  orgId: "org-a",
  projectId: "old-project",
  orgName: "Org A",
  projectName: "Old project",
  loading: false,
  transitionPending: false,
  transitionError: "",
  projectsEmpty: false,
  transition: { failedOrgId: "" },
  selectOrg: vi.fn(),
  selectProject: vi.fn(),
}));

const sessionMock = vi.hoisted(() => ({ isGlobalAdmin: true }));

vi.mock("@/stores/workspace", () => ({ useWorkspace: () => wsMock }));
vi.mock("@/stores/session", () => ({ useSession: () => sessionMock }));

describe("WorkspaceSwitcher", () => {
  function mountOpen() {
    return mount(WorkspaceSwitcher, {
      props: { open: true, canManageOrg: true },
    });
  }

  it("does not expose an old project label while an organization transition is pending", () => {
    wsMock.transitionPending = true;
    wsMock.orgName = "Org B";
    wsMock.projectName = "Old project";
    const wrapper = mountOpen();

    expect(wrapper.get("button[aria-expanded='true']").attributes("aria-controls")).toBeTruthy();
    expect(wrapper.text()).toContain("Switching organization…");
    expect(wrapper.text()).not.toContain("Old project");
    expect(wrapper.findAll("button").every((button) => (button.element as HTMLButtonElement).disabled)).toBe(true);
  });

  it("shows the empty-project label only after a successful empty response", () => {
    wsMock.transitionPending = false;
    wsMock.transitionError = "";
    wsMock.projects = [];
    wsMock.projectsEmpty = true;
    const wrapper = mountOpen();

    expect(wrapper.text()).toContain("No projects in this organization.");
    expect(wrapper.text()).not.toContain("Could not load projects");
  });

  it("shows a recoverable read error instead of the empty-project state", async () => {
    wsMock.orgId = "org-b";
    wsMock.projectsEmpty = false;
    wsMock.transitionError = "Could not load projects for the selected organization.";
    wsMock.transition.failedOrgId = "org-b";
    wsMock.selectOrg.mockResolvedValueOnce(true);
    const wrapper = mountOpen();

    expect(wrapper.text()).toContain("Could not load projects for the selected organization.");
    expect(wrapper.text()).not.toContain("No projects in this organization.");
    await wrapper.findAll("button").find((button) => button.text() === "Retry")!.trigger("click");
    await flushPromises();
    expect(wsMock.selectOrg).toHaveBeenCalledWith("org-b");
    expect(wrapper.emitted("update:open")?.at(-1)).toEqual([false]);
  });

  it("closes only after a project selection is accepted", async () => {
    wsMock.transitionError = "";
    wsMock.projectsEmpty = false;
    wsMock.projects = [{ id: "new-project", name: "New project" }];
    wsMock.selectProject.mockReturnValueOnce(false).mockReturnValueOnce(true);
    const wrapper = mountOpen();

    await wrapper.findAll("button").find((button) => button.text() === "New project")!.trigger("click");
    expect(wrapper.emitted("update:open")).toBeUndefined();
    await wrapper.findAll("button").find((button) => button.text() === "New project")!.trigger("click");
    expect(wrapper.emitted("update:open")?.at(-1)).toEqual([false]);
    expect(wrapper.emitted("project-selected")?.at(-1)).toEqual(["new-project"]);
  });

  it("emits an organization selection only after the transition commits", async () => {
    wsMock.transitionPending = false;
    wsMock.transitionError = "";
    wsMock.projectsEmpty = false;
    wsMock.selectOrg.mockResolvedValueOnce(true);
    const wrapper = mountOpen();

    await wrapper.findAll("button").find((button) => button.text() === "Org B")!.trigger("click");
    await flushPromises();

    expect(wsMock.selectOrg).toHaveBeenCalledWith("org-b");
    expect(wrapper.emitted("organization-selected")?.at(-1)).toEqual(["org-b"]);
  });

  it("gives the workspace menu a stable trigger relationship and focuses its first item", async () => {
    wsMock.transitionPending = false;
    wsMock.transitionError = "";
    wsMock.projectsEmpty = false;
    const wrapper = mount(WorkspaceSwitcher, {
      props: { open: false, canManageOrg: true },
      attachTo: document.body,
    });

    const trigger = wrapper.get("button[aria-expanded='false']");
    expect(trigger.attributes("aria-controls")).toBe("workspace-switcher-menu");
    await trigger.trigger("click");
    await wrapper.setProps({ open: true });
    await nextTick();

    expect(wrapper.get("button[aria-expanded='true']").attributes("aria-controls")).toBe("workspace-switcher-menu");
    expect(wrapper.get("#workspace-switcher-menu").attributes("role")).toBe("menu");
    const firstItem = wrapper.get("#workspace-switcher-menu [role='menuitem']");
    expect(document.activeElement).toBe(firstItem.element);
    wrapper.unmount();
  });

  it("closes the workspace menu on Escape and outside activation and restores the trigger focus", async () => {
    wsMock.transitionPending = false;
    wsMock.transitionError = "";
    wsMock.projectsEmpty = false;
    const wrapper = mount(WorkspaceSwitcher, {
      props: { open: false, canManageOrg: true },
      attachTo: document.body,
    });
    const trigger = wrapper.get("button[aria-expanded='false']");

    trigger.element.focus();
    await trigger.trigger("click");
    await wrapper.setProps({ open: true });
    await nextTick();
    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    await wrapper.setProps({ open: false });
    await nextTick();
    expect(wrapper.emitted("update:open")?.at(-1)).toEqual([false]);
    expect(document.activeElement).toBe(trigger.element);

    wrapper.unmount();
  });
});
