import { defineComponent, h } from "vue";
import { flushPromises, mount } from "@vue/test-utils";
import { beforeEach, describe, expect, it, vi } from "vitest";

import NavigationLinks from "@/components/NavigationLinks.vue";

const sessionMock = vi.hoisted(() => ({
  isGlobalAdmin: true,
}));
const workspaceMock = vi.hoisted(() => ({
  projectId: "project-a",
  serviceCounts: { "project-a": 0 } as Record<string, number>,
}));
const routerMock = vi.hoisted(() => ({ push: vi.fn() }));

vi.mock("vue-router", async () => {
  const actual = await vi.importActual<typeof import("vue-router")>("vue-router");
  return {
    ...actual,
    useRouter: () => routerMock,
    isNavigationFailure: (value: unknown) => Boolean((value as { failure?: boolean } | null)?.failure),
  };
});
vi.mock("@/stores/session", () => ({ useSession: () => sessionMock }));
vi.mock("@/stores/workspace", () => ({ useWorkspace: () => workspaceMock }));

const RouterLinkStub = defineComponent({
  name: "RouterLink",
  inheritAttrs: false,
  props: { to: { type: null, default: undefined } },
  emits: ["click"],
  setup(props, { attrs, slots, emit }) {
    return () =>
      h(
        "a",
        {
          ...attrs,
          href: typeof props.to === "string" ? props.to : "#",
          onClick: (event: MouseEvent) => emit("click", event),
        },
        slots.default?.(),
      );
  },
});

const labels = [
  "Dashboard",
  "Services",
  "Monitors",
  "SLA & SLO",
  "Gate decisions",
  "Escalation",
  "Incidents",
  "Status pages",
  "Dead-letter",
  "Settings",
];

describe("NavigationLinks", () => {
  beforeEach(() => {
    routerMock.push.mockReset();
    routerMock.push.mockResolvedValue(undefined);
    sessionMock.isGlobalAdmin = true;
    workspaceMock.projectId = "project-a";
    workspaceMock.serviceCounts = { "project-a": 0 };
  });

  function mountLinks(active = "dashboard") {
    return mount(NavigationLinks, {
      props: { active },
      global: { stubs: { RouterLink: RouterLinkStub } },
    });
  }

  it("owns the complete labelled navigation list in its existing order", () => {
    const wrapper = mountLinks();
    const rendered = wrapper.findAll("a").map((link) => link.text().replace("new", "").trim());

    expect(rendered).toEqual(labels);
    expect(wrapper.text()).toContain("new");
  });

  it("marks the active route and keeps the Gate decisions document icon", () => {
    const wrapper = mountLinks("gate-decisions");
    const active = wrapper.get("a[aria-current='page']");

    expect(active.text()).toContain("Gate decisions");
    expect(active.find("rect").attributes("x")).toBe("5");
    expect(active.find("path").attributes("d")).toBe("M8 8h8M8 12h8M8 16h5");
  });

  it("uses the operating contrast role for the Project section label", () => {
    const wrapper = mountLinks();
    const projectLabel = wrapper.findAll("div").find((node) => node.text().trim() === "Project");

    expect(projectLabel).toBeDefined();
    expect(projectLabel!.classes()).toContain("text-ink-2");
    expect(projectLabel!.classes()).not.toContain("text-ink-3");
  });

  it("emits navigation for every RouterLink so a drawer owner can close after a route click", async () => {
    const wrapper = mountLinks();
    await wrapper.get("a[href='#']").trigger("click");
    await flushPromises();

    expect(routerMock.push).toHaveBeenCalledTimes(1);
    expect(wrapper.emitted("navigate")).toHaveLength(1);
  });

  it("does not emit navigation when RouterLink navigation is duplicated", async () => {
    routerMock.push.mockResolvedValueOnce({ failure: true });
    const wrapper = mountLinks();

    await wrapper.get("a[href='#']").trigger("click");
    await flushPromises();

    expect(routerMock.push).toHaveBeenCalledTimes(1);
    expect(wrapper.emitted("navigate")).toBeUndefined();
  });

  it("does not emit navigation when a router guard rejects the transition", async () => {
    routerMock.push.mockRejectedValueOnce(new Error("navigation rejected"));
    const wrapper = mountLinks();

    await wrapper.get("a[href='#']").trigger("click");
    await flushPromises();

    expect(routerMock.push).toHaveBeenCalledTimes(1);
    expect(wrapper.emitted("navigate")).toBeUndefined();
  });

  it("does not expose global administration navigation to non-admins", () => {
    sessionMock.isGlobalAdmin = false;
    const wrapper = mountLinks();

    expect(wrapper.text()).not.toContain("Dead-letter");
    expect(wrapper.text()).toContain("Settings");
  });
});
