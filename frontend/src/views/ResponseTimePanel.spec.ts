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

/** The ledger answer the panel gets, or `undefined` for a server that has none (FR-032 §13a). */
type LedgerFixture = { windows: { due_at: string; verdict: string }[]; ledger_from: string | null };

async function mountPanel(opts?: { withTimeout?: boolean; ledger?: LedgerFixture }) {
  for (const fn of Object.values(apiMock)) fn.mockReset();
  apiMock.GET.mockImplementation((path: string) => {
    if (path.endsWith("/monitors/{monitorID}")) return Promise.resolve({ data: MONITOR });
    if (path.endsWith("/heartbeats")) return Promise.resolve({ data: heartbeats(opts) });
    if (path.endsWith("/sla")) return Promise.resolve({ data: { windows: [] } });
    if (path.endsWith("/expected-runs")) return Promise.resolve({ data: opts?.ledger });
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

// ─────────────────────────────────────────────────────────────────────────────────────────────
// FR-032 phase E — the stroke, at the REAL panel (§14, D-0240).
//
// `lib/latencypanel.spec.ts` pins the rule; these reach the rendered surface, because a test that
// names a mechanism it never reaches is evidence of nothing. The fixture's heartbeats are one a
// minute with a REAL six-minute hole (minutes 22–27 are absent), so a window fixture built from
// them can put the ledger's verdicts exactly where the panel's own geometry is interesting.

/** The instants the fixture's checks were recorded at, oldest first. */
function fixtureInstants(): number[] {
  return heartbeats()
    .map((hb) => Date.parse(hb.ts as string))
    .sort((a, b) => a - b);
}

/** One window per interval between adjacent recorded checks, all with the same verdict. */
function windowsForFixture(verdict: string): { due_at: string; verdict: string }[] {
  return fixtureInstants()
    .slice(1)
    .map((ms) => ({ due_at: new Date(ms).toISOString(), verdict }));
}

describe("the Response time panel · FR-032 stroke", () => {
  it("keeps FR-031's picture exactly when the server answers with no ledger", async () => {
    // An older server, a failed fetch, a push monitor, a disabled one. The panel must not draw a
    // line because nothing said not to — that is the inference FR-031 removed.
    const w = await mountPanel();
    expect(w.findAll('[data-testid="lat-stroke"]')).toHaveLength(0);
    expect(w.find('[data-testid="lat-expect-ruler"]').exists()).toBe(false);
    // And the observation ruler is untouched: it answers a different question.
    expect(w.find('[data-testid="lat-ruler"]').exists()).toBe(true);
  });

  it("draws ONE stroke when every window across the series is covered", async () => {
    const w = await mountPanel({
      ledger: { windows: windowsForFixture("covered"), ledger_from: new Date(0).toISOString() },
    });
    const strokes = w.findAll('[data-testid="lat-stroke"]');
    expect(strokes).toHaveLength(1);
    // Every drawn check is on it, including the failure that recorded no latency: that check
    // HAPPENED and answered its window, so skipping it would draw a line across a real run.
    expect(strokes[0].attributes("points")?.split(" ")).toHaveLength(60);
    // The expectation ruler appears with it, and its cells carry verdicts rather than colours
    // borrowed from the status vocabulary.
    const cells = w.findAll('[data-testid="lat-expect-cell"]');
    expect(cells.length).toBeGreaterThan(0);
    expect(cells.every((c) => c.attributes("data-kind") === "covered")).toBe(true);
  });

  it("splits the stroke AROUND a covered_late window rather than refusing the whole series", async () => {
    const windows = windowsForFixture("covered");
    windows[30].verdict = "covered_late";
    const w = await mountPanel({ ledger: { windows, ledger_from: new Date(0).toISOString() } });
    // Two segments: a monitor with one late answer still has defensible strokes either side of
    // it, and refusing them all would understate what the ledger proves.
    expect(w.findAll('[data-testid="lat-stroke"]')).toHaveLength(2);
    const late = w.findAll('[data-testid="lat-expect-cell"]').filter((c) => c.attributes("data-kind") === "late");
    expect(late).toHaveLength(1);
    expect(late[0].attributes("data-verdict")).toBe("covered_late");
  });

  it("draws a window nothing ran in as an OUTLINE, never a fill", async () => {
    const windows = windowsForFixture("covered");
    for (let i = 20; i <= 24; i++) windows[i].verdict = "expected_never_issued";
    const w = await mountPanel({ ledger: { windows, ledger_from: new Date(0).toISOString() } });
    const empty = w.findAll('[data-testid="lat-expect-cell"]').filter((c) => c.attributes("data-kind") === "empty");
    expect(empty).toHaveLength(5);
    // A fill would read as a recorded failure. An empty window is a fact about emptiness.
    expect(empty[0].attributes("fill")).toBe("none");
    expect(empty[0].attributes("stroke")).toBe("var(--down)");
    // And the stroke is broken by them.
    expect(w.findAll('[data-testid="lat-stroke"]').length).toBeGreaterThan(1);
  });

  it("hatches the span before ledger_from and marks where the ledger starts", async () => {
    const instants = fixtureInstants();
    const w = await mountPanel({
      ledger: {
        windows: windowsForFixture("covered"),
        // The ledger starts a third of the way into the drawn series.
        ledger_from: new Date(instants[20]).toISOString(),
      },
    });
    const cells = w.findAll('[data-testid="lat-expect-cell"]');
    const notStored = cells.filter((c) => c.attributes("data-kind") === "notStored");
    expect(notStored.length).toBeGreaterThan(0);
    // Not covered, and not missed either: the ledger holds nothing there (§12.3).
    expect(notStored[0].attributes("fill")).toBe("url(#er-hatch)");
    expect(w.find('[data-testid="lat-ledger-from"]').exists()).toBe(true);
    // The points before it are still DRAWN — they were really recorded — while no stroke reaches
    // back past the marker.
    expect(w.findAll('[data-testid="lat-point"]').length).toBeGreaterThan(0);
    const strokes = w.findAll('[data-testid="lat-stroke"]');
    expect(strokes).toHaveLength(1);
    expect(strokes[0].attributes("points")?.split(" ").length).toBeLessThan(60);
  });

  it("asks the ledger for the span it actually drew, not a fixed range", async () => {
    await mountPanel({ ledger: { windows: [], ledger_from: null } });
    const call = apiMock.GET.mock.calls.find((c: unknown[]) => String(c[0]).endsWith("/expected-runs"));
    expect(call).toBeTruthy();
    const q = (call![1] as { params: { query: { from: string; to: string; limit: number } } }).params.query;
    const instants = fixtureInstants();
    // A fixed range would fetch windows for time the panel is not showing and could miss the ones
    // it is.
    expect(Date.parse(q.from)).toBe(instants[0]);
    expect(Date.parse(q.to)).toBeGreaterThan(instants[instants.length - 1]);
    expect(q.limit).toBe(200);
  });
});
