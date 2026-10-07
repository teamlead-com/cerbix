import { test, expect, type Locator, type Page } from "@playwright/test";
import { apiGet, apiSend, ensureE2EWorkspace } from "./helpers";

// iter-0202: hover readouts that an operator could not read.
//
//   1. The reliability strip's cell readout was centred on its cell with nothing keeping it inside
//      the card, and the card clips (`overflow-hidden`), so a cell near either edge lost part of
//      its readout.
//   2. The response-time panel's point readout appeared only while the pointer sat on a dot a few
//      pixels wide, and it was inserted below the chart, so the card jumped in height on every
//      enter and leave.
//
// Both are geometry, which jsdom does not have, so they are proved here in a real browser. The
// report and series are route fixtures — sealing real buckets across two definition revisions
// takes hours — and the services and monitors around them are real and `e2e-` prefixed.

/** Is every corner of `el` the topmost thing the browser hit-tests there? A readout is
 *  `pointer-events: none`, which elementFromPoint skips, so it is made hit-testable for the probe
 *  and restored. Clipping by an ancestor, falling outside the viewport and being covered all fail. */
async function cornersVisible(el: Locator): Promise<boolean[]> {
  return el.evaluate((node) => {
    const h = node as HTMLElement;
    const prev = h.style.pointerEvents;
    h.style.pointerEvents = "auto";
    const r = h.getBoundingClientRect();
    const pts: [number, number][] = [
      [r.left + 2, r.top + 2], [r.right - 2, r.top + 2],
      [r.left + 2, r.bottom - 2], [r.right - 2, r.bottom - 2],
    ];
    const out = pts.map(([x, y]) => {
      const hit = document.elementFromPoint(x, y);
      return !!hit && h.contains(hit);
    });
    h.style.pointerEvents = prev;
    return out;
  });
}

const FROM = "2026-08-17T00:00:00Z";
const TO = "2026-09-16T00:00:00Z";
const DAY = 86_400_000;
const HOUR_US = 3_600_000_000;
type Durations = Record<"GoodUs" | "BadUs" | "UnknownUs" | "ExcludedUs" | "HealthyUs" | "DegradedUs" | "DownUs" | "HealthUnknownUs", number>;
const good: Durations = { GoodUs: 24 * HOUR_US, BadUs: 0, UnknownUs: 0, ExcludedUs: 0, HealthyUs: 24 * HOUR_US, DegradedUs: 0, DownUs: 0, HealthUnknownUs: 0 };
// The first day carries every state, so its readout is the tallest one the strip can show.
const mixed: Durations = {
  GoodUs: 20 * HOUR_US, BadUs: 2 * HOUR_US, UnknownUs: HOUR_US, ExcludedUs: HOUR_US,
  HealthyUs: 19 * HOUR_US, DegradedUs: HOUR_US, DownUs: 2 * HOUR_US, HealthUnknownUs: 2 * HOUR_US,
};
function sum(...ds: Durations[]): Durations {
  const out = { ...ds[0] };
  for (const d of ds.slice(1)) for (const k of Object.keys(out) as (keyof Durations)[]) out[k] += d[k];
  return out;
}
const days = (Date.parse(TO) - Date.parse(FROM)) / DAY;
const laterDays = sum(...Array.from({ length: days - 1 }, () => good));

function seriesFixture() {
  const points = [];
  for (let t = Date.parse(FROM), i = 0; t < Date.parse(TO); t += DAY, i++) {
    const first = i === 0;
    points.push({
      start: new Date(t).toISOString().replace(".000Z", "Z"),
      epoch_id: first ? "e1" : "e2", revision_id: first ? "r1" : "r2",
      provisional: false, buckets: 1440, durations: first ? mixed : good,
    });
  }
  return { from: FROM, to: TO, step: "day", points };
}

function reportFixture(serviceID: string) {
  const seg = (rev: number, from: string, to: string, buckets: number, durations: Durations, availability: number) => ({
    revision_id: `r${rev}`, revision: rev, epoch_id: `e${rev}`, epoch_seq: rev, from, to,
    buckets, durations, availability, coverage: 1, declared_reconstruction: false,
  });
  return {
    service_id: serviceID, window: "30d", as_of: TO, sealed_through: TO, from: FROM, to: TO,
    status: "ok", storage_continuity: true, expected_buckets: 43200, sealed_buckets: 43200, coverage: 1,
    durations: sum(mixed, laterDays), aggregate_withheld: "spans_definition_revisions", burn: [],
    // The first lane owns the strip's LEFT edge and the second its RIGHT edge.
    segments: [
      seg(1, FROM, "2026-08-18T00:00:00Z", 1440, mixed, 90.909),
      seg(2, "2026-08-18T00:00:00Z", TO, 41760, laterDays, 100),
    ],
  };
}

async function routeReliability(page: Page, serviceID: string) {
  await page.route(`**/api/v1/projects/*/services/${serviceID}/reliability?*`, (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(reportFixture(serviceID)) }));
  await page.route(`**/api/v1/projects/*/services/${serviceID}/reliability/series?*`, (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(seriesFixture()) }));
}

async function openService(page: Page, slug: string) {
  await page.goto("/");
  const { projectID } = await ensureE2EWorkspace(page);
  const created = await apiSend(page, "post", `/api/v1/projects/${projectID}/services`, { slug, name: `E2E ${slug}` });
  expect(created.status()).toBe(201);
  const svc = await created.json();
  await routeReliability(page, svc.id);
  await page.goto(`/services/${svc.id}`);
  await expect(page.getByTestId("svc-segment-strip")).toHaveCount(2);
}

type Beat = { ts: string; up: boolean; latency_ms: number; code: number; msg: string };

/** A real (disabled) monitor whose heartbeats are the given fixture, oldest first. */
async function openMonitor(page: Page, name: string, beats: Beat[]) {
  await page.goto("/");
  const { projectID } = await ensureE2EWorkspace(page);
  const mon = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
    name, type: "http", target: "http://cerbix:8080/healthz", interval_seconds: 30, region: "core", enabled: false,
  });
  expect(mon.status()).toBe(201);
  const monitor = await mon.json();
  const body = beats.map((b) => ({ monitor_id: monitor.id, ...b })).reverse(); // the API is newest first
  await page.route(`**/api/v1/monitors/${monitor.id}/heartbeats?*`, (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(body) }));
  await page.goto(`/monitors/${monitor.id}`);
}

const minuteBeats = (n: number, t0: number, f?: (i: number) => Partial<Beat>): Beat[] =>
  Array.from({ length: n }, (_, i) => ({
    ts: new Date(t0 + i * 60_000).toISOString(), up: true, latency_ms: 70 + (i % 9), code: 200, msg: "ok", ...f?.(i),
  }));

const centre = (b: { x: number; y: number; width: number; height: number }) => ({ x: b.x + b.width / 2, y: b.y + b.height / 2 });

test.describe("hover readouts", () => {
  test.afterEach(async ({ page }) => {
    const { projectID } = await ensureE2EWorkspace(page);
    for (const s of await apiGet(page, `/api/v1/projects/${projectID}/services`)) {
      if ((s.service.slug as string).startsWith("e2e-readout")) {
        await apiSend(page, "delete", `/api/v1/projects/${projectID}/services/${s.service.id}`);
      }
    }
    for (const m of await apiGet(page, `/api/v1/projects/${projectID}/monitors`)) {
      if ((m.name as string).startsWith("e2e-readout")) await apiSend(page, "delete", `/api/v1/monitors/${m.id}`);
    }
  });

  test("a reliability cell at either edge of the strip shows its whole readout", async ({ page }) => {
    await openService(page, "e2e-readout-edges");
    const lanes = page.getByTestId("svc-segment-strip");

    // The cells that sit against the card's left and right edges, in the main strip and the lanes.
    const timeline = page.getByTestId("svc-timeline");
    const cases: [string, Locator][] = [
      ["main strip, first cell", timeline.getByTestId("strip-cell-hit").first()],
      ["main strip, last cell", timeline.getByTestId("strip-cell-hit").last()],
      ["first lane, its only cell (left edge)", lanes.nth(0).getByTestId("strip-cell-hit").first()],
      ["last lane, its last cell (right edge, bottom of the card)", lanes.nth(1).getByTestId("strip-cell-hit").last()],
    ];
    for (const [name, hit] of cases) {
      await hit.hover();
      const readout = page.getByTestId("strip-readout");
      await expect(readout, name).toBeVisible();
      expect(await cornersVisible(readout), `${name}: a corner of the readout is clipped or off-screen`)
        .toEqual([true, true, true, true]);
      await page.mouse.move(0, 0);
      await expect(readout).toHaveCount(0);
    }

    // At phone width the strip runs to the page's own 16px gutter, so a readout centred on an edge
    // cell would leave the VIEWPORT, not just the card: it is pushed back in from that edge.
    await page.setViewportSize({ width: 420, height: 800 });
    for (const [name, hit] of [
      ["phone width, first cell", timeline.getByTestId("strip-cell-hit").first()],
      ["phone width, last cell", timeline.getByTestId("strip-cell-hit").last()],
    ] as [string, Locator][]) {
      await hit.hover();
      const readout = page.getByTestId("strip-readout");
      await expect(readout, name).toBeVisible();
      expect(await cornersVisible(readout), `${name}: a corner of the readout is clipped or off-screen`)
        .toEqual([true, true, true, true]);
      await page.mouse.move(0, 0);
      await expect(readout).toHaveCount(0);
    }
  });

  test("a readout that fits on neither side of its strip is still kept on screen", async ({ page }) => {
    // Review R1-4: a viewport too short for the readout below AND above the strip, though tall
    // enough for the readout itself. The tallest readout (the first, mixed cell) at mid-height.
    await page.setViewportSize({ width: 800, height: 300 });
    await openService(page, "e2e-readout-short");
    const hit = page.getByTestId("svc-timeline").getByTestId("strip-cell-hit").first();
    await hit.evaluate((el) => el.scrollIntoView({ block: "center" }));
    await hit.hover();
    const readout = page.getByTestId("strip-readout");
    await expect(readout).toBeVisible();
    const r = (await readout.boundingBox())!;
    expect(r.height, "this fixture's readout must fit the viewport, or the case proves nothing").toBeLessThan(300 - 16);
    // Both sides, by the placement's own rule: the strip is the readout's parent, with a 2px gap and
    // an 8px viewport margin (ReliabilityStrip.vue placeReadout).
    const strip = await readout.evaluate((el) => {
      const b = el.parentElement!.getBoundingClientRect();
      return { top: b.top, bottom: b.bottom };
    });
    expect(300 - 8 - (strip.bottom + 2), "the readout fits below the strip: this case proves nothing").toBeLessThan(r.height);
    expect(strip.top - 2 - 8, "the readout fits above the strip: this case proves nothing").toBeLessThan(r.height);
    expect(await cornersVisible(readout), "a corner of the readout is clipped or off-screen").toEqual([true, true, true, true]);
  });

  test("an open readout follows its cell when the page scrolls", async ({ page }) => {
    await openService(page, "e2e-readout-scroll");
    const hit = page.getByTestId("svc-segment-strip").nth(1).getByTestId("strip-cell-hit").nth(10);
    await hit.evaluate((el) => el.scrollIntoView({ block: "center" }));
    // Keyboard focus, not hover: under a still pointer a scroll brings ANOTHER cell under it, which
    // is a new hover rather than this one moving.
    await page.mouse.move(0, 0);
    await hit.focus();
    const readout = page.getByTestId("strip-readout");
    await expect(readout).toBeVisible();
    const offset = async () => (await readout.boundingBox())!.y - (await hit.boundingBox())!.y;
    const before = await offset();
    await page.evaluate(() => window.scrollBy(0, 40));
    await expect.poll(offset, { message: "the readout stayed where it was while its cell scrolled" }).toBeCloseTo(before, 0);
    await expect(readout).toBeVisible();
  });

  test("a recorded check is easy to hover and the card does not jump", async ({ page }) => {
    await openMonitor(page, "e2e-readout-latency", minuteBeats(60, Date.now() - 60 * 60_000));
    const points = page.getByTestId("lat-point");
    await expect(points).toHaveCount(60);
    const card = page.locator("section", { has: page.getByTestId("lat-plot") });
    const restHeight = (await card.boundingBox())!.height;
    const readout = page.getByTestId("lat-point-readout");

    // Near a dot rather than exactly on its few-pixel centre is still that dot.
    const a = (await points.nth(20).boundingBox())!;
    const b = (await points.nth(21).boundingBox())!;
    const ay = a.y + a.height / 2;
    await page.mouse.move(a.x + a.width / 2 + 5, ay);
    await expect(readout, "a pointer 5px from a dot's centre reaches no readout").toBeVisible();
    await expect(readout).toContainText(`${70 + (20 % 9)}ms`);
    expect((await card.boundingBox())!.height, "the card changed height when the readout appeared").toBeCloseTo(restHeight, 0);

    // Away from every dot, and back: whatever the readout area shows, the card keeps its height.
    await page.mouse.move((a.x + b.x + b.width) / 2, ay - 40);
    expect((await card.boundingBox())!.height, "the card changed height when the readout left").toBeCloseTo(restHeight, 0);
    await page.mouse.move(b.x + b.width / 2 - 5, b.y + b.height / 2);
    await expect(readout).toBeVisible();
    expect((await card.boundingBox())!.height).toBeCloseTo(restHeight, 0);
  });

  test("the pointer reaches the NEAREST check, not the one drawn last", async ({ page }) => {
    // Review R1-3: two checks a second apart and a third an hour later. The first and second are a
    // fraction of a pixel apart on the time axis but 4ms apart on the latency axis; the pointer at
    // the first dot's exact centre must read the first check.
    const t0 = Date.now() - 2 * 60 * 60_000;
    await openMonitor(page, "e2e-readout-nearest", [
      { ts: new Date(t0).toISOString(), up: true, latency_ms: 70, code: 200, msg: "ok" },
      { ts: new Date(t0 + 1000).toISOString(), up: true, latency_ms: 74, code: 200, msg: "ok" },
      { ts: new Date(t0 + 3_600_000).toISOString(), up: true, latency_ms: 72, code: 200, msg: "ok" },
    ]);
    const points = page.getByTestId("lat-point");
    await expect(points).toHaveCount(3);
    const first = centre((await points.nth(0).boundingBox())!);
    const second = centre((await points.nth(1).boundingBox())!);
    expect(Math.abs(first.y - second.y), "the fixture's two dots must be apart vertically").toBeGreaterThan(4);
    const readout = page.getByTestId("lat-point-readout");
    await page.mouse.move(first.x, first.y);
    await expect(readout).toContainText("70ms");
    await page.mouse.move(second.x, second.y);
    await expect(readout).toContainText("74ms");
  });

  test("at phone width the targets keep their size and no readout changes the card's height", async ({ page }) => {
    // Review R1-1 and R1-2: the plot's real width was never measured, so the targets were sized for
    // the 1072px fallback; and a readout taller than the reserved slot (a long message, a wrapped
    // span sentence) pushed the card down.
    await page.setViewportSize({ width: 420, height: 900 });
    const long = "upstream returned 503 Service Unavailable: " + "the gateway could not reach any healthy backend in the pool; ".repeat(4);
    await openMonitor(page, "e2e-readout-phone", minuteBeats(12, Date.now() - 12 * 60_000, (i) =>
      i === 6 ? { up: false, latency_ms: 0, code: 0, msg: "connect: connection refused" }
        : i === 9 ? { msg: long }
          : {}));
    const plot = page.getByTestId("lat-plot");
    const points = page.getByTestId("lat-point");
    await expect(points).toHaveCount(11);
    const width = (await plot.boundingBox())!.width;
    await expect(plot, "the plot's measured width is not its real width").toHaveAttribute("data-plot-px", String(Math.round(width)));
    // The mouse reaches only what is on screen: bring the plot to the middle of the viewport.
    await plot.evaluate((el) => el.scrollIntoView({ block: "center" }));

    const card = page.locator("section", { has: plot });
    const restHeight = (await card.boundingBox())!.height;
    const readout = page.getByTestId("lat-point-readout");
    const heightUnchanged = async (what: string) =>
      expect((await card.boundingBox())!.height, `the card changed height for ${what}`).toBeCloseTo(restHeight, 0);

    // 5px beside an isolated dot, horizontally and vertically.
    const p = centre((await points.nth(3).boundingBox())!);
    await page.mouse.move(p.x + 5, p.y);
    await expect(readout, "5px to the side of a dot reaches no readout").toBeVisible();
    await heightUnchanged("a short point readout");
    await page.mouse.move(0, 0);
    await page.mouse.move(p.x, p.y + 5);
    await expect(readout, "5px below a dot reaches no readout").toBeVisible();

    // A failure with no latency: its baseline mark is a target too.
    const mark = centre((await page.getByTestId("lat-baseline-mark").first().boundingBox())!);
    await page.mouse.move(0, 0);
    await page.mouse.move(mark.x + 5, mark.y);
    await expect(readout).toContainText("no latency recorded");

    // The long message, and an empty span, at this width.
    const lp = centre((await points.nth(8).boundingBox())!);
    await page.mouse.move(lp.x, lp.y);
    await expect(readout).toContainText("healthy backend");
    await heightUnchanged("a point readout with a long message");
    await page.mouse.move(0, 0);
    await page.getByTestId("lat-ruler-span").nth(2).hover();
    await expect(page.getByTestId("lat-span-readout")).toBeVisible();
    await heightUnchanged("an empty-span readout");
  });

  test("a response-time readout never covers the sticky top bar", async ({ page }) => {
    // Re-review R2-1: the readout overlay and the top bar were both `z-10` in one stacking context,
    // and the readout comes later in the DOM, so scrolled under the bar it was painted over it.
    await page.setViewportSize({ width: 420, height: 900 });
    const long = "upstream returned 503 Service Unavailable: " + "the gateway could not reach any healthy backend in the pool; ".repeat(4);
    await openMonitor(page, "e2e-readout-topbar", minuteBeats(12, Date.now() - 12 * 60_000, (i) => (i === 9 ? { msg: long } : {})));
    const points = page.getByTestId("lat-point");
    await expect(points).toHaveCount(12);
    await points.nth(9).focus(); // focus, so the readout stays open while the page scrolls
    const readout = page.getByTestId("lat-point-readout");
    await expect(readout).toContainText("healthy backend");
    // The other half of the stacking rule: a readout taller than its slot is still drawn OVER the
    // card that follows, whole.
    await readout.evaluate((el) => el.scrollIntoView({ block: "center" }));
    const slot = (await page.getByTestId("lat-readout-slot").boundingBox())!;
    expect((await readout.boundingBox())!.height, "this readout must be taller than its slot, or the case proves nothing")
      .toBeGreaterThan(slot.height + 40);
    expect(await cornersVisible(readout), "the card below is painted over the readout").toEqual([true, true, true, true]);
    const bar = page.getByTestId("app-topbar");
    const barBox = (await bar.boundingBox())!;
    await page.evaluate((dy) => window.scrollBy(0, dy), (await readout.boundingBox())!.y - barBox.height / 2);
    const r = (await readout.boundingBox())!;
    const b = (await bar.boundingBox())!;
    expect(r.y < b.y + b.height && r.y + r.height > b.y, "the readout was not scrolled under the bar: this case proves nothing").toBe(true);
    const onTop = await page.evaluate(([x, y]) => {
      const hit = document.elementFromPoint(x, y);
      return !!hit && !!hit.closest('[data-testid="app-topbar"]');
    }, [b.x + b.width / 2, Math.max(b.y, r.y) + 4]);
    expect(onTop, "the readout is painted over the top bar").toBe(true);
  });

  test("at 260px the widest-gap line stays inside the card and hovering still keeps its height", async ({ page }) => {
    // Re-review R2-2: the always-visible widest-gap line was taken out of flow with the readouts, so
    // where it wraps taller than the reserved slot it ran out of the card.
    await page.setViewportSize({ width: 260, height: 900 });
    // A long widest gap (days, hours and minutes) gives the line its longest wording.
    const gapMs = (2 * 24 * 60 + 3 * 60 + 17) * 60_000;
    const t0 = Date.now() - gapMs - 12 * 60_000;
    const beats = minuteBeats(12, t0).map((b, i) => (i < 6 ? b : { ...b, ts: new Date(Date.parse(b.ts) + gapMs).toISOString() }));
    await openMonitor(page, "e2e-readout-narrow", beats);
    const plot = page.getByTestId("lat-plot");
    await expect(page.getByTestId("lat-point")).toHaveCount(12);
    await plot.evaluate((el) => el.scrollIntoView({ block: "center" }));
    const card = page.locator("section", { has: plot });
    const gap = page.getByTestId("lat-widest-gap");
    await expect(gap).toBeVisible();
    const c = (await card.boundingBox())!;
    const g = (await gap.boundingBox())!;
    const slot = (await page.getByTestId("lat-readout-slot").boundingBox())!;
    // The line is part of the slot's reserved height, so it never runs past the slot — and so never
    // out of the card, which is where the reviewer saw it at this width.
    expect(g.y + g.height, "the widest-gap line runs past its slot").toBeLessThanOrEqual(slot.y + slot.height);
    expect(g.y + g.height, "the widest-gap line runs out of the bottom of the card").toBeLessThanOrEqual(c.y + c.height);
    const p = centre((await page.getByTestId("lat-point").nth(4).boundingBox())!);
    await page.mouse.move(p.x, p.y);
    await expect(page.getByTestId("lat-point-readout")).toBeVisible();
    expect((await card.boundingBox())!.height, "the card changed height when a readout appeared").toBeCloseTo(c.height, 0);
  });
});
