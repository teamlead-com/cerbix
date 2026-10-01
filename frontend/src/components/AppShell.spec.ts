import { defineComponent, h, nextTick } from "vue";
import { flushPromises, mount, type VueWrapper } from "@vue/test-utils";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import AppShell, { type BreadcrumbItem } from "@/components/AppShell.vue";

const routerMock = vi.hoisted(() => ({ push: vi.fn() }));
const themeModule = vi.hoisted(() => ({ theme: { value: "light" } }));
const sessionMock = vi.hoisted(() => ({
  initials: "EA",
  user: { email: "admin@example.invalid", display_name: "Example Admin" },
  version: "0.3.5",
  commit: "unknown",
  fetchVersion: vi.fn(),
  isGlobalAdmin: true,
  isOrgAdmin: vi.fn(() => true),
}));
const workspaceMock = vi.hoisted(() => ({
  orgId: "org-a",
  orgName: "Example Organization",
  projectId: "project-a",
  projectName: "API Platform",
  serviceCounts: { "project-a": 0 } as Record<string, number>,
  ensureServiceCount: vi.fn(),
}));
const brandingMock = vi.hoisted(() => ({
  productName: "cerbix",
  announcement: { enabled: true, text: "Maintenance window starts at 22:00 UTC.", level: "info" },
}));
const liveMock = vi.hoisted(() => ({ started: false, connected: true }));
const uiMock = vi.hoisted(() => ({ openCreate: vi.fn(), closeCreate: vi.fn() }));

vi.mock("@/composables/useTheme", async () => {
  const { ref } = await import("vue");
  const theme = ref<"light" | "dark">("light");
  themeModule.theme = theme;
  return {
    useTheme: () => ({
      theme,
      toggle: () => {
        theme.value = theme.value === "dark" ? "light" : "dark";
      },
      set: (next: "light" | "dark") => {
        theme.value = next;
      },
    }),
  };
});
vi.mock("vue-router", async () => {
  const vue = await import("vue");
  return {
    RouterLink: vue.defineComponent({
      name: "RouterLink",
      inheritAttrs: false,
      props: { to: { type: null, default: undefined } },
      emits: ["click"],
      setup(props: { to?: unknown }, { attrs, slots, emit }: { attrs: Record<string, unknown>; slots: Record<string, () => unknown>; emit: (event: string, value: unknown) => void }) {
        return () =>
          vue.h(
            "a",
            {
              ...attrs,
              href: "#",
              onClick: (event: MouseEvent) => emit("click", event),
            },
            slots.default?.(),
          );
      },
    }),
    useRouter: () => routerMock,
    isNavigationFailure: (value: unknown) => Boolean((value as { failure?: boolean } | null)?.failure),
  };
});
vi.mock("@/stores/session", () => ({ useSession: () => sessionMock }));
vi.mock("@/stores/workspace", () => ({ useWorkspace: () => workspaceMock }));
vi.mock("@/stores/branding", () => ({ useBranding: () => brandingMock }));
vi.mock("@/stores/live", () => ({ useLive: () => liveMock }));
vi.mock("@/stores/ui", () => ({ useUi: () => uiMock }));

const WorkspaceSwitcherStub = defineComponent({
  name: "WorkspaceSwitcher",
  props: {
    open: { type: Boolean, default: false },
    menuId: { type: String, default: "workspace-switcher-menu" },
  },
  emits: ["update:open"],
  setup(props, { emit }) {
    return () =>
      h(
        "button",
        {
          type: "button",
          id: `${props.menuId}-trigger`,
          "aria-expanded": String(props.open),
          "aria-controls": props.menuId,
          onClick: () => emit("update:open", !props.open),
        },
        "Example Organization API Platform",
      );
  },
});

const plainStub = (name: string, tag = "div") =>
  defineComponent({ name, setup(_, { slots }) { return () => h(tag, slots.default?.()); } });

const navigationLabels = [
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

let wrappers: VueWrapper[] = [];

function mountShell(props: { active?: string; crumbs?: BreadcrumbItem[] } = {}) {
  const wrapper = mount(AppShell, {
    props: {
      active: props.active ?? "dashboard",
      crumbs: props.crumbs ?? [
        { label: "Example Organization" },
        { label: "API Platform", to: { name: "dashboard" } },
        { label: "Dashboard" },
      ],
    },
    attachTo: document.body,
    global: {
      stubs: {
        BrandMark: plainStub("BrandMark", "span"),
        CreateDialog: plainStub("CreateDialog"),
        SearchBox: plainStub("SearchBox", "input"),
        WorkspaceSwitcher: WorkspaceSwitcherStub,
      },
    },
  });
  wrappers.push(wrapper);
  return wrapper;
}

describe("AppShell responsive navigation and context", () => {
  beforeEach(() => {
    routerMock.push.mockReset();
    routerMock.push.mockResolvedValue(undefined);
    themeModule.theme.value = "light";
    brandingMock.announcement.enabled = true;
    brandingMock.announcement.text = "Maintenance window starts at 22:00 UTC.";
    brandingMock.announcement.level = "info";
    sessionMock.isGlobalAdmin = true;
    workspaceMock.serviceCounts = { "project-a": 0 };
    document.body.innerHTML = "";
  });

  afterEach(() => {
    wrappers.forEach((wrapper) => wrapper.unmount());
    wrappers = [];
    document.body.innerHTML = "";
  });

  it("preserves the 240px desktop track and 56px topbar structure", () => {
    const wrapper = mountShell();

    expect(wrapper.find(".grid-cols-\\[240px_1fr\\]").exists()).toBe(true);
    expect(wrapper.find("header.h-14").exists()).toBe(true);
    expect(wrapper.find("aside").exists()).toBe(true);
  });

  it("exposes a labelled mobile trigger with a stable drawer relationship", () => {
    const wrapper = mountShell();
    const trigger = wrapper.get("button[data-testid='navigation-trigger']");

    expect(trigger.attributes("aria-label")).toBe("Open navigation");
    expect(trigger.attributes("aria-expanded")).toBe("false");
    expect(trigger.attributes("aria-controls")).toBe("navigation-drawer");
  });

  it("opens the drawer, moves focus inside, and repeats every labelled route once", async () => {
    const wrapper = mountShell({ active: "monitors" });
    const trigger = wrapper.get("button[data-testid='navigation-trigger']");

    await trigger.trigger("click");
    await nextTick();

    expect(trigger.attributes("aria-expanded")).toBe("true");
    const drawer = wrapper.get("#navigation-drawer");
    expect(drawer.exists()).toBe(true);
    expect(drawer.attributes("role")).toBe("dialog");
    expect(drawer.element.contains(document.activeElement)).toBe(true);
    for (const label of navigationLabels) {
      expect(drawer.findAll("a").filter((link) => link.text().includes(label))).toHaveLength(1);
    }
    expect(drawer.get("a[aria-current='page']").text()).toContain("Monitors");
  });

  it.each([320, 375, 768, 900])("keeps the labelled drawer access path at %dpx", async (width) => {
    Object.defineProperty(window, "innerWidth", { configurable: true, value: width });
    const wrapper = mountShell();

    const trigger = wrapper.get("button[data-testid='navigation-trigger']");
    expect(trigger.attributes("aria-controls")).toBe("navigation-drawer");
    await trigger.trigger("click");
    await nextTick();
    expect(wrapper.get("#navigation-drawer").text()).toContain("Example Organization");
    expect(wrapper.get("#navigation-drawer").text()).toContain("API Platform");
    expect(wrapper.get("#navigation-drawer").findAll("a")).toHaveLength(navigationLabels.length);
  });

  it("closes on Escape, outside activation, and route navigation while restoring trigger focus", async () => {
    const wrapper = mountShell();
    const trigger = wrapper.get("button[data-testid='navigation-trigger']");

    trigger.element.focus();
    await trigger.trigger("click");
    await nextTick();
    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    await nextTick();
    expect(wrapper.find("#navigation-drawer").exists()).toBe(false);
    expect(document.activeElement).toBe(trigger.element);

    await trigger.trigger("click");
    await nextTick();
    await wrapper.get("[data-overlay-backdrop]").trigger("click");
    await nextTick();
    expect(wrapper.find("#navigation-drawer").exists()).toBe(false);
    expect(document.activeElement).toBe(trigger.element);

    await trigger.trigger("click");
    await nextTick();
    await wrapper.get("#navigation-drawer a").trigger("click");
    await nextTick();
    expect(wrapper.find("#navigation-drawer").exists()).toBe(false);
    expect(document.activeElement).toBe(trigger.element);
  });

  it("keeps the drawer open when a duplicate RouterLink navigation is reported", async () => {
    routerMock.push.mockResolvedValueOnce({ failure: true });
    const wrapper = mountShell();
    const trigger = wrapper.get("button[data-testid='navigation-trigger']");

    trigger.element.focus();
    await trigger.trigger("click");
    await nextTick();
    await wrapper.get("#navigation-drawer a").trigger("click");
    await flushPromises();

    expect(routerMock.push).toHaveBeenCalledTimes(1);
    expect(wrapper.find("#navigation-drawer").exists()).toBe(true);
    expect(trigger.attributes("aria-expanded")).toBe("true");
  });

  it("keeps the drawer open when a RouterLink navigation guard rejects", async () => {
    routerMock.push.mockRejectedValueOnce(new Error("navigation rejected"));
    const wrapper = mountShell();
    const trigger = wrapper.get("button[data-testid='navigation-trigger']");

    trigger.element.focus();
    await trigger.trigger("click");
    await nextTick();
    await wrapper.get("#navigation-drawer a").trigger("click");
    await flushPromises();

    expect(routerMock.push).toHaveBeenCalledTimes(1);
    expect(wrapper.find("#navigation-drawer").exists()).toBe(true);
    expect(trigger.attributes("aria-expanded")).toBe("true");
  });

  it("integrates the account menu with accessible relationships, focus, Escape, and outside close", async () => {
    const wrapper = mountShell();
    const trigger = wrapper.get("#account-menu-trigger");

    trigger.element.focus();
    await trigger.trigger("click");
    await nextTick();
    expect(trigger.attributes("aria-expanded")).toBe("true");
    expect(trigger.attributes("aria-controls")).toBe("account-menu");
    expect(wrapper.get("#account-menu").attributes("role")).toBe("menu");
    const firstItem = wrapper.get("#account-menu [role='menuitem']");
    expect(document.activeElement).toBe(firstItem.element);

    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    await nextTick();
    expect(trigger.attributes("aria-expanded")).toBe("false");
    expect(document.activeElement).toBe(trigger.element);

    await trigger.trigger("click");
    await nextTick();
    await wrapper.get("[data-overlay-backdrop]").trigger("click");
    await nextTick();
    expect(trigger.attributes("aria-expanded")).toBe("false");
    expect(document.activeElement).toBe(trigger.element);
  });

  it("renders typed breadcrumbs as a labelled landmark with linked parents and a current item", () => {
    const wrapper = mountShell({
      crumbs: [
        { label: "Example Organization" },
        { label: "API Platform", to: { name: "dashboard" } },
        { label: "Monitors", to: { name: "monitors" } },
        { label: "checkout" },
      ],
    });
    const breadcrumbs = wrapper.get("nav[aria-label='Breadcrumb']");

    expect(breadcrumbs.findAll("a").map((link) => link.text())).toEqual(["API Platform", "Monitors"]);
    expect(breadcrumbs.get("[aria-current='page']").text()).toBe("checkout");
  });

  it("communicates the next theme state and pressed state", async () => {
    const wrapper = mountShell();
    const control = wrapper.get("button[data-testid='theme-toggle']");

    expect(control.attributes("aria-label")).toBe("Switch to dark theme");
    expect(control.attributes("aria-pressed")).toBe("false");
    await control.trigger("click");
    await nextTick();
    expect(control.attributes("aria-label")).toBe("Switch to light theme");
    expect(control.attributes("aria-pressed")).toBe("true");
  });

  it.each([
    ["info", "polite"],
    ["warning", "polite"],
    ["critical", "assertive"],
  ] as const)("maps %s announcements to %s live policy", (level, live) => {
    brandingMock.announcement.level = level;
    const wrapper = mountShell();
    const announcement = wrapper.get("[role='status']");

    expect(announcement.attributes("aria-live")).toBe(live);
    expect(announcement.attributes("aria-atomic")).toBe("true");
  });

  it("keeps every desktop navigation label available in the shell", () => {
    const wrapper = mountShell();
    for (const label of navigationLabels) expect(wrapper.text()).toContain(label);
  });
});
