import { flushPromises, mount, type VueWrapper } from "@vue/test-utils";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import PublicStatusView from "@/views/PublicStatusView.vue";

const apiMock = vi.hoisted(() => ({
  GET: vi.fn(),
  POST: vi.fn(),
  DELETE: vi.fn(),
}));
const routeMock = vi.hoisted(() => ({
  params: { slug: "public-status" },
  query: {} as Record<string, string>,
}));
vi.mock("@/api/client", () => ({ api: apiMock }));
vi.mock("vue-router", () => ({ useRoute: () => routeMock }));
vi.mock("@/composables/useTheme", () => ({
  useTheme: () => ({ toggle: vi.fn() }),
}));
vi.mock("@/stores/branding", () => ({
  useBranding: () => ({ logoUrl: "", footerText: "", supportUrl: "" }),
}));

const LONG_UPDATE =
  "Checkout requests are timing out for a subset of customers while the database pool is being recovered. This complete message must remain available in the expanded timeline.";

function incident(
  index: number,
  impact: "critical" | "major" | "minor" | "none" = "major",
  overrides: Record<string, unknown> = {},
) {
  return {
    id: `incident-internal-${index}`,
    project_id: "project-internal",
    monitor_id: `monitor-internal-${index}`,
    title: `Incident ${index}`,
    status: "investigating",
    impact,
    source: "auto",
    started_at: `2026-09-21T0${Math.min(index, 9)}:00:00Z`,
    created_at: `2026-09-21T0${Math.min(index, 9)}:00:00Z`,
    updated_at: `2026-09-21T${String(10 + index).padStart(2, "0")}:00:00Z`,
    affected_component_ids: index <= 2 ? ["component-db"] : [],
    updates: [
      {
        status: "investigating",
        body: `Initial public update ${index}`,
        created_at: `2026-09-21T${String(10 + index).padStart(2, "0")}:00:00Z`,
      },
      {
        status: "identified",
        body: index === 1 ? LONG_UPDATE : `Latest public update ${index}`,
        created_at: `2026-09-21T${String(11 + index).padStart(2, "0")}:00:00Z`,
      },
    ],
    ...overrides,
  };
}

function renderFixture(
  activeIncidents = [incident(1, "critical"), incident(2, "minor")],
) {
  return {
    slug: "public-status",
    title: "Public Status",
    visibility: "public",
    summary: "degraded",
    summary_state: "impaired",
    unmeasured_count: 0,
    updated_at: "2026-09-21T12:00:00Z",
    components: [
      {
        id: "component-db",
        name: "Database primary",
        group: "Data services",
        description: "Primary database",
        status: "degraded",
        uptime_90d: 99.2,
      },
      {
        id: "component-cache",
        name: "Cache secondary",
        group: "Data services",
        description: "Shared cache",
        status: "operational",
        uptime_90d: 99.9,
      },
      {
        id: "component-api",
        name: "Public API",
        group: "Application services",
        status: "operational",
        uptime_90d: 100,
      },
    ],
    active_incidents: activeIncidents,
    maintenance: [
      {
        id: "maintenance-public",
        project_id: "",
        reason: "Database upgrade",
        starts_at: "2026-09-22T01:00:00Z",
        ends_at: "2026-09-22T02:00:00Z",
      },
    ],
    recent_incidents: [
      {
        ...incident(20, "minor", {
          id: "recent-internal",
          title: "Past incident",
          status: "resolved",
          resolved_at: "2026-09-20T12:00:00Z",
          affected_component_ids: [],
        }),
      },
    ],
  };
}

let currentRender: ReturnType<typeof renderFixture>;

async function mountView(render = renderFixture()): Promise<VueWrapper> {
  currentRender = render;
  apiMock.GET.mockImplementation((path: string) => {
    if (path === "/api/v1/public/status-pages/{slug}")
      return Promise.resolve({ data: currentRender });
    return Promise.resolve({ error: { error: "not found" } });
  });
  const wrapper = mount(PublicStatusView, { attachTo: document.body });
  await flushPromises();
  return wrapper;
}

function activeHeaders(wrapper: VueWrapper) {
  return wrapper.findAll('[data-testid="active-incident-header"]');
}

function getByRole(
  wrapper: VueWrapper,
  role: "heading",
  options: { name: string },
) {
  const selectors =
    role === "heading" ? "h1,h2,h3,h4,h5,h6,[role='heading']" : "";
  const match = wrapper
    .findAll(selectors)
    .find((candidate) => candidate.text().trim() === options.name);
  if (!match) throw new Error(`Unable to find ${role} named ${options.name}`);
  return match;
}

describe("PublicStatusView service-first active incidents", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-09-21T20:00:00Z"));
    apiMock.GET.mockReset();
    apiMock.POST.mockReset();
    apiMock.DELETE.mockReset();
    routeMock.query = {};
    Object.defineProperty(HTMLElement.prototype, "scrollIntoView", {
      configurable: true,
      value: vi.fn(),
    });
  });

  afterEach(() => {
    document.body.innerHTML = "";
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it("suppresses every all-clear cue for an operational page with an active Major incident", async () => {
    const wrapper = await mountView({
      ...renderFixture([incident(1, "major")]),
      summary: "operational",
      summary_state: "operational",
      components: renderFixture([]).components.map((component) => ({
        ...component,
        status: "operational",
      })),
    });

    const hero = wrapper.get('[data-testid="overall-status"]');
    expect(
      getByRole(wrapper, "heading", { name: "1 active incident" }).element
        .tagName,
    ).toBe("H1");
    expect(hero.text()).toContain(
      "Major impact. All measured services are currently operational.",
    );
    expect(hero.text()).not.toContain("All systems operational");
    expect(hero.attributes("data-visual")).toBe("warning");
    expect(hero.classes()).not.toContain("bg-up-weak");
    expect(
      wrapper
        .get('[data-testid="overall-status-icon"]')
        .attributes("data-icon"),
    ).toBe("alert");
    expect(
      wrapper.find('[data-testid="overall-status-check-icon"]').exists(),
    ).toBe(false);
    expect(
      wrapper.get('[data-testid="overall-status-alert-icon"]').exists(),
    ).toBe(true);
  });

  it("keeps an impaired component headline and adds incident context", async () => {
    const wrapper = await mountView(renderFixture([incident(1, "critical")]));
    const hero = wrapper.get('[data-testid="overall-status"]');
    expect(
      getByRole(wrapper, "heading", { name: "Degraded performance" }).element
        .tagName,
    ).toBe("H1");
    expect(hero.text()).toContain(
      "Some services are experiencing elevated latency. We’re on it.",
    );
    expect(hero.text()).toContain("1 active incident · Critical impact.");
    expect(hero.text()).not.toContain("1 active incidentCritical impact");
  });

  it("renders identical hero composition for public and authenticated preview data", async () => {
    const render = {
      ...renderFixture([incident(1, "major")]),
      summary: "operational",
      summary_state: "operational",
    };
    const publicWrapper = await mountView(render);
    const publicHero = publicWrapper.get('[data-testid="overall-status"]');
    const publicComposition = {
      headline: publicHero.get("h1").text(),
      supporting: publicWrapper
        .get('[data-testid="overall-status-supporting"]')
        .text(),
      visual: publicHero.attributes("data-visual"),
      classes: publicHero.classes(),
      icon: publicWrapper
        .get('[data-testid="overall-status-icon"]')
        .attributes("data-icon"),
    };
    publicWrapper.unmount();
    document.body.innerHTML = "";

    routeMock.query = { preview: "status-page-id" };
    apiMock.GET.mockImplementation((path: string) => {
      if (path === "/api/v1/public/status-pages/{slug}") {
        return Promise.resolve({ error: { error: "not found" } });
      }
      if (path === "/api/v1/status-pages/{pageID}/render") {
        return Promise.resolve({ data: render });
      }
      return Promise.resolve({ error: { error: "not found" } });
    });
    const previewWrapper = mount(PublicStatusView, { attachTo: document.body });
    await flushPromises();
    const previewHero = previewWrapper.get('[data-testid="overall-status"]');
    expect({
      headline: previewHero.get("h1").text(),
      supporting: previewWrapper
        .get('[data-testid="overall-status-supporting"]')
        .text(),
      visual: previewHero.attributes("data-visual"),
      classes: previewHero.classes(),
      icon: previewWrapper
        .get('[data-testid="overall-status-icon"]')
        .attributes("data-icon"),
    }).toEqual(publicComposition);
    expect(previewWrapper.text()).toContain(
      "Internal page — visible to signed-in members only",
    );
  });

  it("renders the canonical section order and preserves configured group/component order", async () => {
    const wrapper = await mountView();
    expect(
      wrapper
        .findAll("[data-section]")
        .map((section) => section.attributes("data-section")),
    ).toEqual([
      "overall",
      "components",
      "active-incidents",
      "maintenance",
      "past-incidents",
      "subscribe",
    ]);
    expect(
      getByRole(wrapper, "heading", { name: "Scheduled maintenance" }).element
        .tagName,
    ).toBe("H2");
    expect(
      wrapper
        .findAll('[data-testid="status-component"]')
        .map((component) => component.text()),
    ).toEqual([
      expect.stringContaining("Database primary"),
      expect.stringContaining("Cache secondary"),
      expect.stringContaining("Public API"),
    ]);
    const groupHeadings = wrapper
      .find('[data-section="components"]')
      .findAll(".border-b.border-border.px-\\[18px\\].py-\\[11px\\]")
      .map((heading) => heading.text());
    expect(groupHeadings).toEqual(["Data services", "Application services"]);
  });

  it("shows one lifecycle badge, one impact badge, metadata, and no public source", async () => {
    const wrapper = await mountView();
    for (const row of wrapper.findAll('[data-testid="active-incident"]')) {
      expect(row.findAll('[data-testid="lifecycle-badge"]')).toHaveLength(1);
      expect(row.findAll('[data-testid="impact-badge"]')).toHaveLength(1);
    }
    const first = activeHeaders(wrapper)[0];
    expect(first.text()).toContain("Opened");
    expect(first.text()).toContain("Updated");
    expect(first.text()).toContain("2 updates");
    expect(first.text()).not.toContain("auto");
    expect(wrapper.text()).not.toContain("Show full timeline");
  });

  it("starts collapsed, exposes stable ARIA controls, toggles from the keyboard, and reveals full text", async () => {
    const wrapper = await mountView();
    const first = activeHeaders(wrapper)[0];
    expect(first.attributes("aria-expanded")).toBe("false");
    expect(first.attributes("aria-controls")).toBe("active-incident-panel-1");
    expect(wrapper.find('[data-testid="active-incident-panel"]').exists()).toBe(
      false,
    );
    const preview = first.get('[data-testid="latest-update-preview"]');
    expect(preview.classes()).toContain("incident-preview-clamp");
    expect(preview.text()).toBe(LONG_UPDATE);

    await first.trigger("keydown", { key: "Enter" });
    await wrapper.vm.$nextTick();
    expect(first.attributes("aria-expanded")).toBe("true");
    const panel = wrapper.get(`#${first.attributes("aria-controls")}`);
    expect(panel.attributes("role")).toBe("region");
    expect(panel.text()).toContain(LONG_UPDATE);
    expect(panel.text()).toContain("Latest");

    await first.trigger("keydown", { key: " " });
    await wrapper.vm.$nextTick();
    expect(first.attributes("aria-expanded")).toBe("false");
  });

  it("keeps eight incidents flat and groups nine incidents only by impact", async () => {
    const impacts = ["none", "minor", "major", "critical"] as const;
    const eight = Array.from({ length: 8 }, (_, index) =>
      incident(index + 1, impacts[index % impacts.length]),
    );
    let wrapper = await mountView(renderFixture(eight));
    expect(
      wrapper.findAll('[data-testid="active-incident-group"]'),
    ).toHaveLength(1);
    expect(
      wrapper.findAll('[data-testid="impact-group-heading"]'),
    ).toHaveLength(0);
    wrapper.unmount();
    document.body.innerHTML = "";

    const nine = Array.from({ length: 9 }, (_, index) =>
      incident(index + 1, impacts[index % impacts.length]),
    );
    wrapper = await mountView(renderFixture(nine));
    expect(
      wrapper.findAll('[data-testid="active-incident-group"]'),
    ).toHaveLength(4);
    expect(
      wrapper
        .findAll('[data-testid="impact-group-heading"]')
        .map((heading) => heading.text()),
    ).toEqual([
      "Critical impact",
      "Major impact",
      "Minor impact",
      "None impact",
    ]);
    expect(
      wrapper
        .findAll('[data-testid="impact-group-heading"]')
        .some((heading) => heading.text().includes("Investigating")),
    ).toBe(false);
    const displayed = activeHeaders(wrapper).map(
      (header) => header.text().match(/Incident \d+/)?.[0],
    );
    expect(displayed.slice(0, 2)).toEqual(["Incident 8", "Incident 4"]);
  });

  it("sorts by impact, then updated time, then the original API order", async () => {
    const sameTime = "2026-09-21T16:00:00Z";
    const wrapper = await mountView(
      renderFixture([
        incident(1, "minor", { title: "Minor first", updated_at: sameTime }),
        incident(2, "critical", {
          title: "Critical older",
          updated_at: "2026-09-21T14:00:00Z",
        }),
        incident(3, "critical", {
          title: "Critical tied first",
          updated_at: sameTime,
        }),
        incident(4, "critical", {
          title: "Critical tied second",
          updated_at: sameTime,
        }),
        incident(5, "major", {
          title: "Major",
          updated_at: "2026-09-21T19:00:00Z",
        }),
      ]),
    );
    expect(
      activeHeaders(wrapper).map((header) =>
        header.find(".font-semibold.leading-snug").text(),
      ),
    ).toEqual([
      "Critical tied first",
      "Critical tied second",
      "Critical older",
      "Major",
      "Minor first",
    ]);
  });

  it("opens, scrolls to, and focuses the first displayed incident linked from a component without changing the URL", async () => {
    const pushState = vi.spyOn(window.history, "pushState");
    const replaceState = vi.spyOn(window.history, "replaceState");
    const wrapper = await mountView();
    const link = wrapper.get('[data-testid="component-incident-link"]');
    expect(link.text()).toBe("2 active incidents");

    await link.trigger("click");
    await flushPromises();
    const first = activeHeaders(wrapper)[0];
    expect(first.attributes("aria-expanded")).toBe("true");
    expect(document.activeElement).toBe(first.element);
    expect(HTMLElement.prototype.scrollIntoView).toHaveBeenCalledOnce();
    expect(pushState).not.toHaveBeenCalled();
    expect(replaceState).not.toHaveBeenCalled();
    expect(window.location.hash).toBe("");
    expect(window.location.href).not.toContain("monitor-internal");
    expect(window.location.href).not.toContain("project-internal");
    expect(window.location.href).not.toContain("incident-internal");
  });

  it("does not invent a timeline update when an incident has none", async () => {
    const wrapper = await mountView(
      renderFixture([
        incident(1, "major", {
          updates: [],
          updated_at: "2026-09-21T12:00:00Z",
        }),
      ]),
    );
    const header = activeHeaders(wrapper)[0];
    expect(header.text()).toContain("0 updates");
    expect(header.text()).toContain("No further details were published.");
    await header.trigger("click");
    expect(
      wrapper.get('[data-testid="active-incident-panel"]').text(),
    ).toContain("No further details were published.");
    expect(
      wrapper
        .get('[data-testid="active-incident-panel"]')
        .findAll(".border-l-2"),
    ).toHaveLength(0);
  });
});
