import { flushPromises, mount } from "@vue/test-utils";
import { describe, expect, it, vi } from "vitest";

import MonitorDetailView from "@/views/MonitorDetailView.vue";
// The SAME formatter the panel uses. A test that spelled the instant itself would be asserting its
// own arithmetic, and would keep passing if the panel's zone handling changed underneath it.
import { instantLabel } from "@/lib/wallclock";

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
function heartbeats(opts?: { withTimeout?: boolean; noLatencies?: boolean }) {
  const t0 = Date.UTC(2026, 8, 3, 14, 12, 0);
  const out: Record<string, unknown>[] = [];
  for (let i = 0, m = 0; i < 60; i++, m++) {
    if (m >= 22 && m <= 27) m = 28;
    const ts = new Date(t0 + m * 60_000).toISOString();
    if (m === 29) { out.push({ monitor_id: "m1", ts, up: false, latency_ms: 0, code: 0, msg: "bad request: unsupported scheme" }); continue; }
    if (opts?.withTimeout && m === 40) { out.push({ monitor_id: "m1", ts, up: false, latency_ms: 10_000, code: 0, msg: "timeout" }); continue; }
    // E8's fixture: every check failed and recorded no latency at all, so the panel has points and
    // no measured population.
    if (opts?.noLatencies) { out.push({ monitor_id: "m1", ts, up: false, latency_ms: 0, code: 0, msg: "connect: connection refused" }); continue; }
    out.push({ monitor_id: "m1", ts, up: true, latency_ms: 70 + (i % 9), code: 200, msg: "ok" });
  }
  return out.reverse(); // the API returns newest first
}

/** The ledger answer the panel gets, or `undefined` for a server that has none (FR-032 §13a). */
type LedgerFixture = {
  windows: { due_at: string; verdict: string; reserved_at?: string; withheld_reason?: string }[];
  ledger_from: string | null;
};

async function mountPanel(opts?: { withTimeout?: boolean; noLatencies?: boolean; ledger?: LedgerFixture }) {
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

  // E8 — the out-of-scale note is gated on the SCALE, not on whether an average exists.
  //
  // It carried `&& stats.avg != null`, so a panel where no check recorded a latency showed
  // "timeout 10s" with no rule drawn and no note: the one case where the reader has nothing else to
  // go on, and the panel said neither that the timeout was on the scale nor that it was off it.
  //
  // The mutation that must kill this: require an average again.
  it("declares the timeout off the scale even when nothing recorded a latency", async () => {
    const w = await mountPanel({ noLatencies: true });
    expect(w.find('[data-testid="lat-header"]').text()).toContain("no check recorded a latency");
    const timeout = w.find('[data-testid="lat-timeout"]').text();
    expect(timeout).toContain("timeout 10s");
    expect(
      timeout,
      "the timeout is neither drawn nor declared off the scale, so the reader is told nothing",
    ).toContain("outside this scale");
    expect(w.find('[data-testid="lat-timeout-rule"]').exists()).toBe(false);
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
    // E7 — the p95 legend names the MEASURED population, because that is what p95 is over.
    //
    // It named the DRAWN one — "p95 · last 60 checks" — while the statistic is computed from the
    // 59 that recorded a latency. The two differ exactly when a monitor has been failing, which is
    // when this panel is most likely to be read.
    //
    // The mutation that must kill this: name `stats.drawn` in the legend again.
    const legend = w.text();
    expect(legend).toContain("p95 · last 59 checks with a recorded latency");
    expect(legend).not.toContain("p95 · last 60 checks");
    expect(w.find('[data-testid="lat-subtitle"]').text()).toContain("no claim about whether a check was due");
    expect(w.find('[data-testid="lat-subtitle"]').text()).toContain("points only, no stroke and no fill");
  });

  // E4 — an expectation cell covers only the window it belongs to.
  //
  // The width came from the number of POINTS while the cells are positioned at WINDOW instants, so
  // whenever windows outnumbered points every cell was wider than its window and painted over its
  // neighbours. The windows arrive newest-first, so the oldest paints last: a covered cell could
  // cover the missed windows beside it, and because the hit rectangles overlapped identically the
  // readout named a window the pointer was not over. Windows outnumbering points is not an edge
  // case — it is exactly what a missed run looks like.
  //
  // The fixture is that shape: a window per minute across a span the series covers with far fewer
  // points, including the six-minute hole.
  //
  // The mutation that must kill this: size the cells from `panelPoints.length` again.
  it("gives each expectation cell a width its own window can hold", async () => {
    // A window every THIRTY seconds across the drawn span: more than twice as many windows as
    // points, which is what a monitor whose probes are sparser than its schedule produces — and
    // what the point-derived width cannot fit. A grid as sparse as the points hides the defect,
    // because the 0.62 factor absorbs a small excess.
    const instants = fixtureInstants();
    const windows: { due_at: string; verdict: string }[] = [];
    for (let ms = instants[0], i = 0; ms <= instants[instants.length - 1]; ms += 30_000, i++) {
      windows.push({
        due_at: new Date(ms).toISOString(),
        verdict: i % 7 === 0 ? "expected_never_issued" : "covered",
      });
    }
    expect(windows.length, "the grid is not denser than the series, so this case sees nothing")
      .toBeGreaterThan(2 * instants.length);
    const w = await mountPanel({ ledger: { windows, ledger_from: new Date(0).toISOString() } });
    const cells = w.findAll('[data-testid="lat-expect-cell"]');
    expect(cells.length, "no cells were drawn, so this case sees nothing").toBeGreaterThan(2);

    const noneOverlap = (found: ReturnType<typeof w.findAll>, what: string) => {
      const boxes = found
        .map((c) => ({ x: Number(c.attributes("x")), w: Number(c.attributes("width")) }))
        .sort((a, b) => a.x - b.x);
      for (let i = 1; i < boxes.length; i++) {
        const prevEnd = boxes[i - 1].x + boxes[i - 1].w;
        expect(
          prevEnd,
          `${what} ${i - 1} ends at ${prevEnd} and ${what} ${i} starts at ${boxes[i].x}`,
        ).toBeLessThanOrEqual(boxes[i].x + 0.001);
      }
    };
    // The PAINTED cell: a covered cell painting over the window beside it.
    noneOverlap(cells, "cell");
    // And the POINTER TARGET, which this test claimed in prose and never asserted (reviewer P1,
    // party [346]). The painted cells took 0.62 of the gap while every hit rect grew by 2.5 on each
    // side, so below a gap of ~13px the targets overlapped and the readout could name a window the
    // pointer was not over — E4's second half, alive under a green test that said otherwise.
    const hits = w.findAll('[data-testid="lat-expect-hit"]');
    expect(hits.length, "no hit targets were drawn, so this case sees nothing").toBe(cells.length);
    noneOverlap(hits, "hit target");
    // And the target is still USABLE: it must be at least as wide as the cell it belongs to, or the
    // fix would have bought non-overlap by making the panel harder to point at.
    const cellW = cells.map((c) => Number(c.attributes("width"))).sort((a, b) => a - b);
    const hitW = hits.map((c) => Number(c.attributes("width"))).sort((a, b) => a - b);
    for (let i = 0; i < cellW.length; i++) expect(hitW[i]).toBeGreaterThanOrEqual(cellW[i]);
  });

  // E3 — the subtitle describes the picture the panel DREW, in BOTH mounts.
  //
  // It was a constant, and it survived phase E: on a ledger-carrying monitor the panel drew
  // strokes, drew a ruler of due windows, printed a legend explaining both — and beneath them the
  // line "points only, no stroke and no fill … this panel makes no claim about whether a check was
  // due there". The legend and the subtitle described two different pictures, and only one of them
  // was on the screen.
  //
  // Asserted in the ledger-ON mount, which is where the defect lives; the ledger-OFF assertion above
  // is what keeps the fix from becoming an unconditional swap. A test written only in the mount the
  // suite already had could not have seen this at all.
  //
  // The mutation that must kill this: make the subtitle a constant again, either way round.
  it("stops denying the strokes it draws once the ledger answers", async () => {
    const w = await mountPanel({
      ledger: { windows: windowsForFixture("covered"), ledger_from: new Date(0).toISOString() },
    });
    expect(w.findAll('[data-testid="lat-stroke"]').length, "this mount draws no stroke, so it cannot see E3").toBeGreaterThan(0);
    const subtitle = w.find('[data-testid="lat-subtitle"]').text();
    expect(subtitle).not.toContain("points only, no stroke and no fill");
    expect(subtitle).not.toContain("makes no claim about whether a check was due");
    expect(subtitle).toContain("a stroke joins adjacent points");
    // It still names the window it drew, which the old sentence also did.
    expect(subtitle).toMatch(/last \d+ checks/);
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

// ─────────────────────────────────────────────────────────────────────────────────────────────
// FR-032 phase F — the cell vocabulary, as a SET (§7.4, mock approved 2026-09-05).
//
// The panel used to map `covered`, `covered_late` and pre-`ledger_from` time, and send EVERYTHING
// ELSE to the `--down` outline whose meaning is "a run was due here and nothing ran". So `unknown`,
// `issued_never_claimed` and `claimed_never_finished` were all drawn as missed runs — and `unknown`
// had been assigned the hatch by the phase-E mock this panel was approved against. On the shipped
// default (the ledger carrier off) EVERY window reads `unknown`, so the ruler drew a full row of
// missed runs on an instance where nothing was missed.
//
// These cases pin every verdict the API can return, as a set rather than one by one, because the
// way this breaks again is a NEW verdict falling through a default arm.

const VERDICT_CELLS: [string, string][] = [
  ["covered", "covered"],
  ["covered_late", "late"],
  ["expected_never_issued", "empty"],
  ["issued_never_claimed", "empty"],
  ["claimed_never_finished", "empty"],
  ["unknown", "notStored"],
  ["reserved", "reserved"],
];

describe("the expectation ruler's vocabulary", () => {
  it("draws every verdict the API can return, and never sends one to a default", async () => {
    const instants = fixtureInstants().slice(1);
    // One window per verdict, laid along the drawn span so each gets its own cell.
    const windows = VERDICT_CELLS.map(([verdict], i) => ({
      due_at: new Date(instants[i]).toISOString(),
      verdict,
      ...(verdict === "reserved" ? { reserved_at: new Date(instants[i] - 1000).toISOString() } : {}),
    }));
    const w = await mountPanel({ ledger: { windows, ledger_from: new Date(0).toISOString() } });
    const cells = w.findAll('[data-testid="lat-expect-cell"]');
    const got = new Map(cells.map((c) => [c.attributes("data-verdict"), c.attributes("data-kind")]));

    for (const [verdict, kind] of VERDICT_CELLS) {
      expect(got.get(verdict), `${verdict} must be drawn as ${kind}`).toBe(kind);
    }
    // And the two that carry the loudest claims are asserted from the other side as well: neither
    // may be the missed-run cell, which is what both of them were.
    expect(got.get("unknown"), "unknown drawn as a missed run is the default-configuration defect").not.toBe("empty");
    expect(got.get("reserved"), "reserved drawn as a missed run asserts an absence nobody knows").not.toBe("empty");
  });

  it("gives an unrecognised verdict the cell that claims nothing", async () => {
    // A verdict this build has never heard of — a newer server, or a rollback. The old default arm
    // called it a missed run; the honest answer is the hatch, because not knowing what a name means
    // is exactly the case where the panel cannot say what happened.
    const windows = [{ due_at: new Date(fixtureInstants()[1]).toISOString(), verdict: "some_future_state" }];
    const w = await mountPanel({ ledger: { windows, ledger_from: new Date(0).toISOString() } });
    const cell = w.find('[data-testid="lat-expect-cell"]');
    expect(cell.attributes("data-kind")).toBe("notStored");
  });

  it("renders a reserved window as an outline in the neutral hue, never as a fill", async () => {
    const windows = [{
      due_at: new Date(fixtureInstants()[1]).toISOString(),
      verdict: "reserved",
      reserved_at: new Date(fixtureInstants()[1] - 1000).toISOString(),
    }];
    const w = await mountPanel({ ledger: { windows, ledger_from: new Date(0).toISOString() } });
    const cell = w.find('[data-testid="lat-expect-cell"]');
    // A fill would read as a recorded failure; the failure hue would say the run did not happen.
    expect(cell.attributes("fill")).toBe("none");
    expect(cell.attributes("stroke")).toBe("var(--ink-3)");
    expect(cell.attributes("stroke")).not.toBe("var(--down)");
  });

  it("says in words what a reserved window is, and names the instant it has", async () => {
    const due = fixtureInstants()[1];
    const windows = [{
      due_at: new Date(due).toISOString(),
      verdict: "reserved",
      reserved_at: new Date(due - 1000).toISOString(),
      withheld_reason: "publish_failed",
    }];
    const w = await mountPanel({ ledger: { windows, ledger_from: new Date(0).toISOString() } });
    const hit = w.findAll('[data-testid="lat-expect-hit"]');
    expect(hit.length, "a cell two pixels wide needs a hit area or nothing can reach it").toBeGreaterThan(0);
    await hit[0].trigger("focus");
    const readout = w.find('[data-testid="lat-cell-readout"]');
    expect(readout.exists()).toBe(true);
    expect(readout.text()).toContain("reserved at");
    expect(readout.text()).toContain("no dispatch recorded");
    // The reason travels when there is one, and it is the API's own word rather than a paraphrase.
    expect(readout.text()).toContain("publish_failed");
    // The instant named is `reserved_at`, which is the only instant this window has.
    expect(readout.text()).toContain(instantLabel(new Date(due - 1000).toISOString()));
  });

  it("never calls an unknown window a missed run in words either", async () => {
    const windows = [{ due_at: new Date(fixtureInstants()[1]).toISOString(), verdict: "unknown" }];
    const w = await mountPanel({ ledger: { windows, ledger_from: new Date(0).toISOString() } });
    await w.findAll('[data-testid="lat-expect-hit"]')[0].trigger("focus");
    const text = w.find('[data-testid="lat-cell-readout"]').text();
    expect(text).toContain("unknown");
    expect(text).not.toContain("nothing was dispatched");
  });
});

// `ledger_from` outranks the stored verdict in the WORDS, not only in the fill (final-tree audit,
// party [65]).
//
// The cell geometry always got this right — the kind is decided by `ms < ledgerFrom` before the map
// is consulted — but `cellLabel` carried an exception for `unknown`, so a window before the ledger
// begins whose stored verdict happened to be `unknown` was described as "dispatched on a carrier
// that carries no job identity". That is a claim about a DISPATCH, made about time the ledger
// explicitly cannot answer for, and it is reachable whenever a truncation fence moves `ledger_from`
// past a row that still exists.
describe("a window before ledger_from says only that", () => {
  const beforeLedger = async (verdict: string, extra: Record<string, string> = {}) => {
    const instants = fixtureInstants();
    // The window is early in the drawn span; the ledger begins LATER, so this row is behind the
    // fence while still being returned by the API.
    const windows = [{ due_at: new Date(instants[2]).toISOString(), verdict, ...extra }];
    const w = await mountPanel({
      ledger: { windows, ledger_from: new Date(instants[20]).toISOString() },
    });
    return w;
  };

  it("hatches an unknown window behind the fence and never says it was dispatched", async () => {
    const w = await beforeLedger("unknown");
    const cell = w.find('[data-testid="lat-expect-cell"]');
    expect(cell.attributes("data-kind")).toBe("notStored");
    await w.findAll('[data-testid="lat-expect-hit"]')[0].trigger("focus");
    const text = w.find('[data-testid="lat-cell-readout"]').text();
    expect(text).toContain("before the ledger begins");
    expect(text).toContain("claimable as nothing");
    // The overclaim: any statement about how the run was dispatched, about a carrier, or about an
    // identity, for time the ledger says it holds nothing for.
    expect(text).not.toContain("dispatched");
    expect(text).not.toContain("carrier");
    expect(text).not.toContain("correlate");
  });

  it("says the same of a reserved window behind the fence, and claims no reservation", async () => {
    const instants = fixtureInstants();
    const w = await beforeLedger("reserved", {
      reserved_at: new Date(instants[2] - 1000).toISOString(),
      withheld_reason: "publish_failed",
    });
    const cell = w.find('[data-testid="lat-expect-cell"]');
    expect(cell.attributes("data-kind")).toBe("notStored");
    await w.findAll('[data-testid="lat-expect-hit"]')[0].trigger("focus");
    const text = w.find('[data-testid="lat-cell-readout"]').text();
    expect(text).toContain("before the ledger begins");
    expect(text).not.toContain("reserved at");
    expect(text).not.toContain("no dispatch recorded");
    expect(text).not.toContain("publish_failed");
  });

  it("still describes an unknown window that is INSIDE the ledger's range", async () => {
    // The other half, so the fix cannot be "always say before the ledger begins": a window the
    // ledger does hold, dispatched below the carrier, keeps its own wording.
    const windows = [{ due_at: new Date(fixtureInstants()[20]).toISOString(), verdict: "unknown" }];
    const w = await mountPanel({ ledger: { windows, ledger_from: new Date(0).toISOString() } });
    await w.findAll('[data-testid="lat-expect-hit"]')[0].trigger("focus");
    const text = w.find('[data-testid="lat-cell-readout"]').text();
    expect(text).toContain("unknown");
    expect(text).toContain("carrier");
    expect(text).not.toContain("before the ledger begins");
  });
});
