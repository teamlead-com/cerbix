import { flushPromises, mount } from "@vue/test-utils";
import { describe, expect, it, vi } from "vitest";

import MonitorDetailView from "@/views/MonitorDetailView.vue";

// func-truthful-rendering §6 (FR-031, D-0235) against the REAL panel. The library tests in
// `lib/latencypanel.spec.ts` pin the arithmetic; these reach the rendered surface, because a test
// that names a mechanism it never reaches is evidence of nothing.

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), PUT: vi.fn(), PATCH: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));
vi.mock("vue-router", () => ({
  useRoute: () => ({ query: {}, params: { id: "m1" } }),
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
  RouterLink: { props: ["to"], template: "<a><slot /></a>" },
}));
vi.mock("@/components/AppShell.vue", () => ({
  default: { name: "AppShell", template: "<div><slot name='actions' /><slot /></div>" },
}));
vi.mock("@/stores/session", () => ({
  useSession: () => ({ canProjectWrite: () => true, isOrgAdmin: () => true, isGlobalAdmin: false }),
}));
vi.mock("@/stores/workspace", () => ({
  useWorkspace: () => ({ init: () => Promise.resolve(), orgId: "o1", projectId: "p1", orgName: "Acme", projectName: "API", projects: [] }),
}));
vi.mock("@/stores/branding", () => ({ useBranding: () => ({ load: () => Promise.resolve() }) }));
vi.mock("@/stores/live", () => ({
  useLive: () => ({ statuses: {} as Record<string, { status: string }>, connected: true, started: false, connect: () => {} }),
}));

const MONITOR = {
  id: "m1", name: "api", type: "http", target: "https://example.test", method: "GET",
  interval_seconds: 60, timeout_seconds: 10, retries: 0, enabled: true, region: "core",
  project_id: "p1", status: "up", execution_revision: 1,
};

/** 60 checks a minute apart with a REAL six-minute hole and one failure carrying no latency. */
function heartbeats(opts?: { withTimeout?: boolean }) {
  const t0 = Date.UTC(2026, 8, 3, 14, 12, 0);
  const out: Record<string, unknown>[] = [];
  for (let i = 0, m = 0; i < 60; i++, m++) {
    if (m >= 22 && m <= 27) m = 28;
    const ts = new Date(t0 + m * 60_000).toISOString();
    if (m === 29) { out.push({ monitor_id: "m1", ts, up: false, latency_ms: 0, code: 0, msg: "bad request: unsupported scheme" }); continue; }
    if (opts?.withTimeout && m === 40) { out.push({ monitor_id: "m1", ts, up: false, latency_ms: 10_000, code: 0, msg: "timeout" }); continue; }
    out.push({ monitor_id: "m1", ts, up: true, latency_ms: 70 + (i % 9), code: 200, msg: "ok" });
  }
  return out.reverse(); // the API returns newest first
}

async function mountPanel(opts?: { withTimeout?: boolean }) {
  for (const fn of Object.values(apiMock)) fn.mockReset();
  apiMock.GET.mockImplementation((path: string) => {
    if (path.endsWith("/monitors/{monitorID}")) return Promise.resolve({ data: MONITOR });
    if (path.endsWith("/heartbeats")) return Promise.resolve({ data: heartbeats(opts) });
    if (path.endsWith("/sla")) return Promise.resolve({ data: { windows: [] } });
    return Promise.resolve({ data: [] });
  });
  const w = mount(MonitorDetailView, { global: { stubs: { RouterLink: { template: "<a><slot /></a>" } } } });
  await flushPromises();
  await flushPromises();
  return w;
}

describe("the Response time panel", () => {
  it("draws every fetched check, including the failure that recorded no latency", async () => {
    const w = await mountPanel();
    const dots = w.findAll('[data-testid="lat-point"]');
    const marks = w.findAll('[data-testid="lat-baseline-mark"]');
    // 60 fetched: 59 with a latency, one without — and the one without is DRAWN, not dropped.
    expect(dots).toHaveLength(59);
    expect(marks).toHaveLength(1);
    expect(marks[0].attributes("data-ts")).toBe(new Date(Date.UTC(2026, 8, 3, 14, 41, 0)).toISOString());
  });

  it("draws no connecting stroke and no fill, because neither can span unproven time", async () => {
    const w = await mountPanel();
    const plot = w.find('[data-testid="lat-plot"]');
    // the only <path>/<polyline> a line chart needs is exactly what may not exist here
    expect(plot.findAll("path")).toHaveLength(0);
    expect(plot.findAll("polyline")).toHaveLength(0);
    expect(plot.html()).not.toContain("linearGradient");
  });

  it("puts real time on x, so a hole in the checks is a hole on the axis", async () => {
    const w = await mountPanel();
    const xs = w.find('[data-testid="lat-plot"]').findAll('[data-testid="lat-point"]')
      .map((c) => parseFloat(c.attributes("cx")!));
    const deltas = xs.slice(1).map((v, i) => v - xs[i]);
    const ordinary = Math.min(...deltas);
    const widest = Math.max(...deltas);
    // an index axis would make every step identical; a time axis makes the hole seven times wider
    expect(widest / ordinary).toBeGreaterThan(5);
  });

  it("carries the observation ruler: one tick per recorded check, and focusable empty spans", async () => {
    const w = await mountPanel();
    expect(w.findAll('[data-testid="lat-ruler-tick"]')).toHaveLength(60);
    const spans = w.findAll('[data-testid="lat-ruler-span"]');
    expect(spans).toHaveLength(59);
    await spans[0].trigger("focus");
    const readout = w.find('[data-testid="lat-span-readout"]');
    expect(readout.text()).toContain("no check recorded between");
    expect(readout.text()).toMatch(/UTC[+-]\d{2}:\d{2}/);
    expect(readout.text()).toContain("not late, not missed, not covered, not anomalous");
  });

  it("names the widest interval as an interval and never as a missed check", async () => {
    const w = await mountPanel();
    const note = w.find('[data-testid="lat-widest-gap"]').text();
    expect(note).toContain("widest interval between two recorded checks");
    expect(note).toContain("7m");
    // and never "0 min" for a real interval — the defect the running stack showed
    expect(note).not.toMatch(/\b0\s*m(in)?\b/);
    // it says what it measured, and explicitly disclaims what it cannot know
    expect(note).toContain("never");
    expect(note).toContain("a check was missed");
    // and it never counts checks it believes were due — the inference this contract forbids
    expect(note).not.toMatch(/\b\d+\s+(missed|missing)\b/);
    expect(note).not.toMatch(/\b(missed|missing)\s+\d+/);
  });

  it("states the timeout always and draws it only when it falls inside the computed extent", async () => {
    const off = await mountPanel();
    expect(off.find('[data-testid="lat-timeout"]').text()).toContain("timeout 10s");
    expect(off.find('[data-testid="lat-timeout"]').text()).toContain("outside this scale");
    expect(off.find('[data-testid="lat-timeout-rule"]').exists()).toBe(false);

    const on = await mountPanel({ withTimeout: true });
    expect(on.find('[data-testid="lat-timeout"]').text()).toContain("timeout 10s");
    expect(on.find('[data-testid="lat-timeout"]').text()).not.toContain("outside this scale");
    expect(on.find('[data-testid="lat-timeout-rule"]').exists()).toBe(true);
  });

  it("describes the population it drew, and never another window's", async () => {
    const w = await mountPanel();
    const header = w.find('[data-testid="lat-header"]').text();
    // 59 of 60 carried a latency, and the header says exactly that rather than implying all 60
    expect(header).toContain("59 of 60 checks with a recorded latency");
    expect(w.find('[data-testid="lat-subtitle"]').text()).toContain("no claim about whether a check was due");
    expect(w.find('[data-testid="lat-subtitle"]').text()).toContain("points only, no stroke and no fill");
  });

  it("gives a point a readout in local time over the canonical UTC instant, and highlights its row", async () => {
    const w = await mountPanel();
    const dots = w.findAll('[data-testid="lat-point"]');
    const last = dots[dots.length - 1];
    await last.trigger("focus");
    const readout = w.find('[data-testid="lat-point-readout"]');
    expect(readout.exists()).toBe(true);
    expect(readout.text()).toMatch(/UTC[+-]\d{2}:\d{2}/);
    expect(readout.text()).toContain(last.attributes("data-ts")!.replace(".000Z", "Z"));
    // one object, one hover: the matching Recent-checks row is marked
    const highlighted = w.findAll('[data-testid="recent-check-row"][data-highlighted="true"]');
    expect(highlighted).toHaveLength(1);
  });
});
