import { flushPromises, mount } from "@vue/test-utils";
import { describe, expect, it, vi } from "vitest";

import ServiceReliability from "@/components/ServiceReliability.vue";

const apiMock = vi.hoisted(() => ({
  GET: vi.fn(),
  PUT: vi.fn(),
}));
vi.mock("@/api/client", () => ({ api: apiMock }));

// The honesty states of spec §11/§12, as the operator sees them (iter-0144): every absent
// number is a dash carrying the payload's own reason; the ∅ state offers NO aggregate; the
// live signal is categorical and separate; the objective editor uses the one client rule.
// The [218] fix pass adds the tenant-context generation contract (a late response from a
// previous project/service/window must not land), the subordinate error states, the
// provisional-tail request, bucket-weighted geometry, the repair mask, the mutable
// objective, and the per-segment strips.

type SeriesFixture = { from: string; to: string; step: string; points: unknown[] };

function mountWith(
  report: Record<string, unknown>,
  health?: Record<string, unknown>,
  series?: SeriesFixture,
  extra?: {
    tail?: SeriesFixture;
    // per-segment fixtures keyed by the segment's exact `from` ([221] P1-2); "never" hangs
    // that one request forever ([228] P1-2's deferred-one case)
    segmentSeries?: Record<string, SeriesFixture | "never">;
    reportFails?: boolean;
    healthFails?: boolean;
    // REJECTED promises, not {error} payloads — openapi-fetch rethrows network failures
    // and the component must treat both forms identically at each boundary ([221] P1-1).
    healthRejects?: boolean;
    healthNever?: boolean;
    seriesRejects?: boolean;
    seriesFails?: boolean;
  },
) {
  apiMock.GET.mockReset();
  apiMock.PUT.mockReset();
  apiMock.GET.mockImplementation((path: string, opts?: { params?: { query?: { from?: string } } }) => {
    if (path.endsWith("/reliability")) {
      if (extra?.reportFails) return Promise.resolve({ error: { error: "boom" } });
      return Promise.resolve({ data: report });
    }
    if (path.endsWith("/health")) {
      if (extra?.healthRejects) return Promise.reject(new TypeError("fetch failed"));
      if (extra?.healthNever) return new Promise(() => {});
      if (extra?.healthFails) return Promise.resolve({ error: { error: "boom" } });
      return Promise.resolve({ data: health ?? { unstable: true, as_of: "2026-08-16T12:00:00Z", sli: "unknown", diagnostics: "unknown" } });
    }
    if (path.endsWith("/reliability/series")) {
      const from = opts?.params?.query?.from;
      // Per-segment requests carry the segment's exact from — route them first.
      if (extra?.segmentSeries && from && from in extra.segmentSeries) {
        const fix = extra.segmentSeries[from];
        if (fix === "never") return new Promise(() => {});
        return Promise.resolve({ data: fix });
      }
      if (extra?.seriesRejects) return Promise.reject(new TypeError("fetch failed"));
      if (extra?.seriesFails) return Promise.resolve({ error: { error: "boom" } });
      // The main timeline is TWO requests: the sealed window from report.from, and the
      // provisional tail from report.sealed_through — route each to its own fixture.
      if (from === report.sealed_through)
        return Promise.resolve({ data: extra?.tail ?? { from: "", to: "", step: "day", points: [] } });
      return Promise.resolve({ data: series ?? { from: "", to: "", step: "day", points: [] } });
    }
    return Promise.resolve({ data: {} });
  });
  return mount(ServiceReliability, {
    props: { projectId: "p1", serviceId: "svc-1", canWrite: true, hasSli: true },
  });
}

const goodDurations = { GoodUs: 1, BadUs: 0, UnknownUs: 0, ExcludedUs: 0, HealthyUs: 1, DegradedUs: 0, DownUs: 0, HealthUnknownUs: 0 };
const badDurations = { GoodUs: 0, BadUs: 5, UnknownUs: 0, ExcludedUs: 0, HealthyUs: 0, DegradedUs: 0, DownUs: 5, HealthUnknownUs: 0 };

const okReport = {
  service_id: "svc-1",
  window: "30d",
  as_of: "2026-08-16T12:00:00Z",
  sealed_through: "2026-08-16T11:58:00Z",
  from: "2026-07-17T11:58:00Z",
  to: "2026-08-16T11:58:00Z",
  status: "ok",
  storage_continuity: true,
  expected_buckets: 43200,
  sealed_buckets: 43200,
  coverage: 0.994,
  durations: goodDurations,
  availability: 99.972,
  objective: 99.95,
  budget: { objective: 99.95, objective_updated_at: "2026-08-01T00:00:00Z", allowed_downtime_ratio: 0.0005, actual_downtime_ratio: 0.00028, remaining_ratio: 0.00022, burned_percent: 56, met: true },
  burn: [
    { window: "1h", status: "ok", rate: 0.4, expected_buckets: 60, sealed_buckets: 60, storage_continuity: true, coverage: 1 },
    { window: "6h", status: "insufficient_sealed_coverage", reason: "sealed_through_behind_window", expected_buckets: 360, sealed_buckets: 360, storage_continuity: true, coverage: 1 },
  ],
  segments: [],
};

describe("ServiceReliability honesty states", () => {
  it("renders the numbers, the objective and the per-window burn verdicts in the ok state", async () => {
    const wrapper = mountWith(okReport, { unstable: true, as_of: "x", sli: "healthy", diagnostics: "failing", failing_monitors: ["redis"] });
    await flushPromises();
    expect(wrapper.find('[data-testid="svc-kpi-availability"]').text()).toContain("99.972%");
    expect(wrapper.find('[data-testid="svc-kpi-availability"]').text()).toContain("objective 99.95%");
    const burn = wrapper.find('[data-testid="svc-kpi-burn"]').text();
    expect(burn).toContain("0.4×");
    // The 6h verdict renders the payload REASON as operator prose — never the machine
    // status token, never 0× ([218] P2).
    expect(burn).toContain("sealed watermark has not reached");
    expect(burn).not.toContain("insufficient_sealed_coverage");
    // The two live layers stay separate: a failing diagnostic never touches the SLI pill.
    expect(wrapper.find('[data-testid="svc-health-sli"]').text()).toBe("healthy");
    expect(wrapper.find('[data-testid="svc-health-diag"]').text()).toBe("failing");
  });

  it("withholds every number behind the ∅ banner and gives each segment its range and its own strip", async () => {
    const wrapper = mountWith(
      {
        ...okReport,
        status: "ok",
        availability: undefined,
        objective: undefined,
        budget: undefined,
        aggregate_withheld: "spans_definition_revisions",
        segments: [
          { revision_id: "r1", revision: 1, epoch_id: "e1", epoch_seq: 1, from: "2026-08-01T00:00:00Z", to: "2026-08-05T00:00:00Z", buckets: 10, durations: goodDurations, availability: 99.9, coverage: 0.99, declared_reconstruction: true },
          { revision_id: "r2", revision: 2, epoch_id: "e2", epoch_seq: 2, from: "2026-08-05T00:00:00Z", to: "2026-08-10T00:00:00Z", buckets: 10, durations: goodDurations, availability: 99.5, coverage: 0.98, declared_reconstruction: false },
        ],
      },
      undefined,
      {
        // The global series already splits points at epoch boundaries — UNIQUE-epoch
        // segments slice it with NO extra request ([228] P1-2).
        from: "a",
        to: "b",
        step: "day",
        points: [
          { start: "2026-08-02T00:00:00Z", epoch_id: "e1", revision_id: "r1", provisional: false, buckets: 24, durations: goodDurations },
          { start: "2026-08-06T00:00:00Z", epoch_id: "e2", revision_id: "r2", provisional: false, buckets: 24, durations: badDurations },
        ],
      },
    );
    await flushPromises();
    expect(wrapper.find('[data-testid="svc-no-total"]').text()).toContain("No single availability number");
    expect(wrapper.find('[data-testid="svc-kpi-availability"]').text()).toContain("—");
    expect(wrapper.find('[data-testid="svc-kpi-availability"]').text()).not.toContain("99.972");
    // Each segment is complete on its own terms ([218] P1-6): range, numbers, own strip.
    const segments = wrapper.findAll('[data-testid="svc-segment"]');
    expect(segments).toHaveLength(2);
    expect(segments[0].text()).toContain("rev 1");
    expect(segments[0].text()).toContain("availability 99.9%");
    expect(segments[0].find('[data-testid="svc-segment-range"]').text()).toBe("01.08.2026 – 05.08.2026");
    expect(wrapper.find('[data-testid="svc-reconstruction"]').exists()).toBe(true);
    // Unique-epoch strips issued NO per-segment requests: the only series calls are the
    // sealed window and the provisional tail.
    type SeriesCall = [string, { params: { query: { from: string } } }];
    const seriesCalls = (apiMock.GET.mock.calls as SeriesCall[]).filter(([p]) => p.endsWith("/reliability/series"));
    expect(seriesCalls.map(([, o]) => o.params.query.from).sort()).toEqual([okReport.from, okReport.sealed_through].sort());
    const strips = wrapper.findAll('[data-testid="svc-segment-strip"]');
    expect(strips).toHaveLength(2);
    expect(strips[0].find('rect[data-state="good"]').exists()).toBe(true);
    expect(strips[0].find('rect[data-state="bad"]').exists()).toBe(false);
    expect(strips[1].find('rect[data-state="bad"]').exists()).toBe(true);
  });

  it("keeps two same-epoch reconstruction parts apart: no point is duplicated or mixed across the boundary", async () => {
    // [221] P1-2's defining case: a reconstruction boundary at 12:00 inside one day
    // legitimately yields TWO segments with the SAME epoch_id. The daily rollup point for
    // that day would aggregate both halves — client-side slicing of a merged series put it
    // in BOTH strips. Per-segment exact-range requests make that impossible: the server
    // filters buckets before grouping, so each strip receives only its own half (both
    // halves even truncate to the same step start, which no client-side range filter could
    // tell apart).
    const wrapper = mountWith(
      {
        ...okReport,
        availability: undefined,
        objective: undefined,
        budget: undefined,
        aggregate_withheld: "spans_definition_revisions",
        segments: [
          { revision_id: "r1", revision: 1, epoch_id: "e1", epoch_seq: 1, from: "2026-08-01T00:00:00Z", to: "2026-08-01T12:00:00Z", buckets: 720, durations: goodDurations, availability: 100, coverage: 0.99, declared_reconstruction: true },
          { revision_id: "r1", revision: 1, epoch_id: "e1", epoch_seq: 1, from: "2026-08-01T12:00:00Z", to: "2026-08-02T00:00:00Z", buckets: 720, durations: badDurations, availability: 42.5, coverage: 0.99, declared_reconstruction: false },
        ],
      },
      undefined,
      undefined,
      {
        segmentSeries: {
          "2026-08-01T00:00:00Z": { from: "a", to: "b", step: "day", points: [{ start: "2026-08-01T00:00:00Z", epoch_id: "e1", revision_id: "r1", provisional: false, buckets: 720, durations: goodDurations }] },
          "2026-08-01T12:00:00Z": { from: "a", to: "b", step: "day", points: [{ start: "2026-08-01T00:00:00Z", epoch_id: "e1", revision_id: "r1", provisional: false, buckets: 720, durations: badDurations }] },
        },
      },
    );
    await flushPromises();
    const strips = wrapper.findAll('[data-testid="svc-segment-strip"]');
    expect(strips).toHaveLength(2);
    // Exactly ONE rect per strip — nothing duplicated across the boundary…
    expect(strips[0].findAll("rect[data-state]")).toHaveLength(1);
    expect(strips[1].findAll("rect[data-state]")).toHaveLength(1);
    // …and each half keeps its OWN state: the reconstruction part good, the live part bad.
    expect(strips[0].find("rect").attributes("data-state")).toBe("good");
    expect(strips[1].find("rect").attributes("data-state")).toBe("bad");
    // The requests carried the exact half-day ranges.
    type SeriesCall = [string, { params: { query: { from: string; to: string } } }];
    const segCalls = (apiMock.GET.mock.calls as SeriesCall[]).filter(
      ([p, o]) => p.endsWith("/reliability/series") && o.params.query.from.startsWith("2026-08-01"),
    );
    expect(segCalls.map(([, o]) => `${o.params.query.from}..${o.params.query.to}`).sort()).toEqual([
      "2026-08-01T00:00:00Z..2026-08-01T12:00:00Z",
      "2026-08-01T12:00:00Z..2026-08-02T00:00:00Z",
    ]);
  });

  it("clips the repair mask to the segment's own half — the other half keeps its real state", async () => {
    // [228] P1-3: both exact half-day requests return points whose start is the TRUNCATED
    // step start (00:00). A repair confined to the first half must not mask the second.
    const wrapper = mountWith(
      {
        ...okReport,
        availability: undefined,
        objective: undefined,
        budget: undefined,
        aggregate_withheld: "spans_definition_revisions",
        repairing: [{ from: "2026-08-01T06:00:00Z", to: "2026-08-01T07:00:00Z" }],
        segments: [
          { revision_id: "r1", revision: 1, epoch_id: "e1", epoch_seq: 1, from: "2026-08-01T00:00:00Z", to: "2026-08-01T12:00:00Z", buckets: 720, durations: goodDurations, availability: 100, coverage: 0.99, declared_reconstruction: true },
          { revision_id: "r1", revision: 1, epoch_id: "e1", epoch_seq: 1, from: "2026-08-01T12:00:00Z", to: "2026-08-02T00:00:00Z", buckets: 720, durations: badDurations, availability: 42.5, coverage: 0.99, declared_reconstruction: false },
        ],
      },
      undefined,
      undefined,
      {
        segmentSeries: {
          "2026-08-01T00:00:00Z": { from: "a", to: "b", step: "day", points: [{ start: "2026-08-01T00:00:00Z", epoch_id: "e1", revision_id: "r1", provisional: false, buckets: 720, durations: goodDurations }] },
          "2026-08-01T12:00:00Z": { from: "a", to: "b", step: "day", points: [{ start: "2026-08-01T00:00:00Z", epoch_id: "e1", revision_id: "r1", provisional: false, buckets: 720, durations: badDurations }] },
        },
      },
    );
    await flushPromises();
    const strips = wrapper.findAll('[data-testid="svc-segment-strip"]');
    expect(strips).toHaveLength(2);
    // The repair [06:00,07:00) lies inside the FIRST half only.
    expect(strips[0].find("rect").attributes("data-state")).toBe("repairing");
    expect(strips[1].find("rect").attributes("data-state")).toBe("bad");
  });

  it("renders a fast segment strip even while another segment's request hangs", async () => {
    // [228] P1-2: per-segment results land AS THEY COMPLETE — one hung exact request must
    // not suppress the strips that already answered, and the hung one is explicit PENDING.
    const wrapper = mountWith(
      {
        ...okReport,
        availability: undefined,
        objective: undefined,
        budget: undefined,
        aggregate_withheld: "spans_definition_revisions",
        segments: [
          { revision_id: "r1", revision: 1, epoch_id: "e1", epoch_seq: 1, from: "2026-08-01T00:00:00Z", to: "2026-08-01T12:00:00Z", buckets: 720, durations: goodDurations, availability: 100, coverage: 0.99, declared_reconstruction: true },
          { revision_id: "r1", revision: 1, epoch_id: "e1", epoch_seq: 1, from: "2026-08-01T12:00:00Z", to: "2026-08-02T00:00:00Z", buckets: 720, durations: badDurations, availability: 42.5, coverage: 0.99, declared_reconstruction: false },
        ],
      },
      undefined,
      undefined,
      {
        segmentSeries: {
          "2026-08-01T00:00:00Z": "never",
          "2026-08-01T12:00:00Z": { from: "a", to: "b", step: "day", points: [{ start: "2026-08-01T00:00:00Z", epoch_id: "e1", revision_id: "r1", provisional: false, buckets: 720, durations: badDurations }] },
        },
      },
    );
    await flushPromises();
    const segments = wrapper.findAll('[data-testid="svc-segment"]');
    expect(segments).toHaveLength(2);
    expect(segments[0].find('[data-testid="svc-segment-strip-pending"]').exists()).toBe(true);
    expect(segments[0].find('[data-testid="svc-segment-strip"]').exists()).toBe(false);
    expect(segments[1].find('[data-testid="svc-segment-strip"]').exists()).toBe(true);
    expect(segments[1].find("rect").attributes("data-state")).toBe("bad");
  });

  it("issues NO per-segment requests for unique-epoch segments, however many there are", async () => {
    // [228] P1-2's bound: N ordinary segments must not become N HTTP requests — the global
    // series already carries their points, split at epoch boundaries.
    const segs = [];
    const pts = [];
    for (let i = 1; i <= 6; i++) {
      segs.push({ revision_id: "r1", revision: 1, epoch_id: `e${i}`, epoch_seq: i, from: `2026-08-0${i}T00:00:00Z`, to: `2026-08-0${i + 1}T00:00:00Z`, buckets: 1440, durations: goodDurations, availability: 99.9, coverage: 0.99, declared_reconstruction: false });
      pts.push({ start: `2026-08-0${i}T00:00:00Z`, epoch_id: `e${i}`, revision_id: "r1", provisional: false, buckets: 24, durations: i % 2 ? goodDurations : badDurations });
    }
    const wrapper = mountWith(
      { ...okReport, availability: undefined, objective: undefined, budget: undefined, aggregate_withheld: "spans_definition_revisions", segments: segs },
      undefined,
      { from: "a", to: "b", step: "day", points: pts },
    );
    await flushPromises();
    type SeriesCall = [string, { params: { query: { from: string } } }];
    const seriesCalls = (apiMock.GET.mock.calls as SeriesCall[]).filter(([p]) => p.endsWith("/reliability/series"));
    // Exactly the sealed window + the provisional tail — nothing per segment.
    expect(seriesCalls).toHaveLength(2);
    expect(wrapper.findAll('[data-testid="svc-segment-strip"]')).toHaveLength(6);
  });

  it("states the reason with a dash — never 100% — for an unmeasured window", async () => {
    const wrapper = mountWith({
      ...okReport,
      status: "unavailable",
      reason: "zero_decidable_time",
      availability: undefined,
      budget: undefined,
      burn: undefined,
    });
    await flushPromises();
    const reason = wrapper.find('[data-testid="svc-report-reason"]');
    expect(reason.text()).toContain("unavailable");
    expect(reason.text()).toContain("Nothing was measured");
    expect(wrapper.find('[data-testid="svc-kpi-availability"]').text()).toContain("—");
    // The only "100" anywhere is inside the reason PROSE; no numeric availability exists.
    expect(wrapper.find('[data-testid="svc-kpi-availability"]').text()).not.toContain("99.972");
    expect(wrapper.find('[data-testid="svc-kpi-budget"]').text()).toContain("—");
  });

  it("rejects a post-round-invalid objective with no request and sends the canonical value otherwise", async () => {
    const wrapper = mountWith({ ...okReport, objective: undefined, budget: undefined, burn: undefined });
    await flushPromises();
    await wrapper.find('[data-testid="svc-objective-open"]').trigger("click");
    const input = wrapper.find('[data-testid="svc-objective-input"]');
    await input.setValue("99.99995");
    await wrapper.find('[data-testid="svc-objective-save"]').trigger("click");
    expect(wrapper.find('[data-testid="svc-objective-error"]').text()).toContain("above 0 and below 100");
    expect(apiMock.PUT).not.toHaveBeenCalled();

    apiMock.PUT.mockResolvedValue({ data: { window: "30d", objective: 99.9999 } });
    await input.setValue("99.99994");
    await wrapper.find('[data-testid="svc-objective-save"]').trigger("click");
    await flushPromises();
    expect(apiMock.PUT).toHaveBeenCalledTimes(1);
    const putArgs = apiMock.PUT.mock.calls[0] as [string, { body: { objective: number } }];
    expect(putArgs[1].body.objective).toBe(99.9999);
  });

  it("marks provisional series points and never colours unknown as a status", async () => {
    const wrapper = mountWith(okReport, undefined, {
      from: "a",
      to: "b",
      step: "day",
      points: [
        { start: "2026-08-13T00:00:00Z", epoch_id: "e1", revision_id: "r1", provisional: false, buckets: 24, durations: goodDurations },
        { start: "2026-08-14T00:00:00Z", epoch_id: "e1", revision_id: "r1", provisional: false, buckets: 24, durations: { ...goodDurations, GoodUs: 0, UnknownUs: 5, HealthyUs: 0, HealthUnknownUs: 5 } },
        { start: "2026-08-15T00:00:00Z", epoch_id: "e2", revision_id: "r1", provisional: true, buckets: 6, durations: goodDurations },
      ],
    });
    await flushPromises();
    const timeline = wrapper.find('[data-testid="svc-timeline"]');
    expect(timeline.exists()).toBe(true);
    // The unknown tick is SHORT (y offset), the provisional tick is translucent — the two
    // fixed encodings of the approved motif.
    const unknownTick = timeline.find('rect[data-state="unknown"]');
    expect(unknownTick.attributes("y")).toBe("5");
    const provisionalTick = timeline.find('rect[data-provisional="true"]');
    expect(provisionalTick.exists()).toBe(true);
    expect(timeline.find('rect[data-marker="epoch"]').exists()).toBe(true);
    expect(wrapper.find('[data-testid="svc-provisional-note"]').text()).toContain("reduced");
  });

  it("never lands a deferred response from a previous service into the current screen", async () => {
    // [218] P0: start the GETs for service A, switch to B, let B complete, THEN let the
    // stale A response arrive — the screen must keep showing B.
    apiMock.GET.mockReset();
    apiMock.PUT.mockReset();
    let resolveA!: (v: unknown) => void;
    const reportB = { ...okReport, service_id: "svc-B", availability: 98.765, objective: 98 };
    apiMock.GET.mockImplementation((path: string, opts?: { params?: { path?: { serviceID?: string } } }) => {
      if (path.endsWith("/reliability")) {
        if (opts?.params?.path?.serviceID === "svc-A") return new Promise((r) => { resolveA = r; });
        return Promise.resolve({ data: reportB });
      }
      if (path.endsWith("/health"))
        return Promise.resolve({ data: { unstable: true, as_of: "x", sli: "healthy", diagnostics: "ok" } });
      return Promise.resolve({ data: { from: "", to: "", step: "day", points: [] } });
    });
    const wrapper = mount(ServiceReliability, {
      props: { projectId: "p1", serviceId: "svc-A", canWrite: true, hasSli: true },
    });
    await wrapper.setProps({ serviceId: "svc-B" });
    await flushPromises();
    expect(wrapper.find('[data-testid="svc-kpi-availability"]').text()).toContain("98.765%");
    resolveA({ data: { ...okReport, service_id: "svc-A", availability: 11.111 } });
    await flushPromises();
    const kpi = wrapper.find('[data-testid="svc-kpi-availability"]').text();
    expect(kpi).toContain("98.765%");
    expect(kpi).not.toContain("11.111");
  });

  it("resets the objective draft on a context switch and drops a late PUT from the old context", async () => {
    // [218] P0: a draft typed while viewing one service must never be applied or surfaced
    // under another.
    const wrapper = mountWith(okReport);
    await flushPromises();
    await wrapper.find('[data-testid="svc-objective-open"]').trigger("click");
    const input = wrapper.find('[data-testid="svc-objective-input"]');
    // The editor opens PREFILLED with the canonical current value ([218] P1-5).
    expect((input.element as HTMLInputElement).value).toBe("99.95");
    await input.setValue("97");
    let resolvePut!: (v: unknown) => void;
    apiMock.PUT.mockImplementation(() => new Promise((r) => { resolvePut = r; }));
    await wrapper.find('[data-testid="svc-objective-save"]').trigger("click");
    await wrapper.setProps({ serviceId: "svc-2" });
    await flushPromises();
    // The context switch closed and cleared the editor…
    expect(wrapper.find('[data-testid="svc-objective-editor"]').exists()).toBe(false);
    // …and the old context's PUT outcome (an error here) must not resurface in the new one.
    resolvePut({ error: { error: "stale failure from svc-1" } });
    await flushPromises();
    expect(wrapper.find('[data-testid="svc-objective-error"]').exists()).toBe(false);
    expect(wrapper.find('[data-testid="svc-objective-editor"]').exists()).toBe(false);
    // Re-opening prefills from the CURRENT report, not the abandoned draft.
    await wrapper.find('[data-testid="svc-objective-open"]').trigger("click");
    expect((wrapper.find('[data-testid="svc-objective-input"]').element as HTMLInputElement).value).toBe("99.95");
  });

  it("shows a transport error for a failed health request instead of the categorical unknown", async () => {
    const wrapper = mountWith(okReport, undefined, undefined, { healthFails: true });
    await flushPromises();
    // [218] P1-1: "health request failed" is NOT SLI unknown — no pill is rendered at all.
    expect(wrapper.find('[data-testid="svc-health-error"]').text()).toContain("transport problem");
    expect(wrapper.find('[data-testid="svc-health-sli"]').exists()).toBe(false);
    // The report itself is unaffected.
    expect(wrapper.find('[data-testid="svc-kpi-availability"]').text()).toContain("99.972%");
  });

  it("shows a transport error for a failed series request instead of an empty timeline", async () => {
    const wrapper = mountWith(okReport, undefined, undefined, { seriesFails: true });
    await flushPromises();
    expect(wrapper.find('[data-testid="svc-series-error"]').text()).toContain("transport problem");
    expect(wrapper.find('[data-testid="svc-timeline"]').exists()).toBe(false);
    expect(wrapper.find('[data-testid="svc-kpi-availability"]').text()).toContain("99.972%");
  });

  it("catches a REJECTED health fetch at its own boundary without hiding the intact report", async () => {
    // [221] P1-1 repro A: openapi-fetch RETHROWS a rejected network fetch — it must land
    // in healthError, never in the primary report error.
    const wrapper = mountWith(okReport, undefined, undefined, { healthRejects: true });
    await flushPromises();
    expect(wrapper.find('[data-testid="svc-health-error"]').text()).toContain("transport problem");
    expect(wrapper.find('[data-testid="svc-health-sli"]').exists()).toBe(false);
    // The intact report is still on screen — the primary error was NOT set.
    expect(wrapper.find('[data-testid="svc-kpi-availability"]').text()).toContain("99.972%");
    expect(wrapper.text()).not.toContain("Could not load the reliability report");
  });

  it("catches a REJECTED series fetch at its own boundary without hiding the intact report", async () => {
    const wrapper = mountWith(okReport, undefined, undefined, { seriesRejects: true });
    await flushPromises();
    expect(wrapper.find('[data-testid="svc-series-error"]').text()).toContain("transport problem");
    expect(wrapper.find('[data-testid="svc-timeline"]').exists()).toBe(false);
    expect(wrapper.find('[data-testid="svc-kpi-availability"]').text()).toContain("99.972%");
    expect(wrapper.text()).not.toContain("Could not load the reliability report");
  });

  it("renders health as PENDING while the timeline and segment strips load INDEPENDENTLY of it", async () => {
    // [221] P1-1 repro B + [228] P1-1: a null health is an unanswered request, not the
    // genuine categorical unknown — and a hung health request must not gate the sealed,
    // tail, or per-segment series requests.
    const wrapper = mountWith(
      {
        ...okReport,
        availability: undefined,
        objective: undefined,
        budget: undefined,
        aggregate_withheld: "spans_definition_revisions",
        segments: [
          { revision_id: "r1", revision: 1, epoch_id: "e1", epoch_seq: 1, from: "2026-08-01T00:00:00Z", to: "2026-08-05T00:00:00Z", buckets: 10, durations: goodDurations, availability: 99.9, coverage: 0.99, declared_reconstruction: false },
          { revision_id: "r2", revision: 2, epoch_id: "e2", epoch_seq: 2, from: "2026-08-05T00:00:00Z", to: "2026-08-10T00:00:00Z", buckets: 10, durations: goodDurations, availability: 99.5, coverage: 0.98, declared_reconstruction: false },
        ],
      },
      undefined,
      {
        from: "a",
        to: "b",
        step: "day",
        points: [
          { start: "2026-08-02T00:00:00Z", epoch_id: "e1", revision_id: "r1", provisional: false, buckets: 24, durations: goodDurations },
          { start: "2026-08-06T00:00:00Z", epoch_id: "e2", revision_id: "r2", provisional: false, buckets: 24, durations: badDurations },
        ],
      },
      { healthNever: true },
    );
    await flushPromises();
    expect(wrapper.find('[data-testid="svc-health-pending"]').text()).toContain("Checking");
    expect(wrapper.find('[data-testid="svc-health-sli"]').exists()).toBe(false);
    expect(wrapper.find('[data-testid="svc-health-error"]').exists()).toBe(false);
    // Everything else rendered anyway: the report, the main timeline, both segment strips.
    expect(wrapper.find('[data-testid="svc-no-total"]').exists()).toBe(true);
    expect(wrapper.find('[data-testid="svc-timeline"]').exists()).toBe(true);
    expect(wrapper.findAll('[data-testid="svc-segment-strip"]')).toHaveLength(2);
  });

  it("settles health honestly even when the report itself fails", async () => {
    // [228] P1-1's inverse: a report failure must not leave an unissued health request
    // labelled pending forever — health is its own task and lands in its pills.
    const wrapper = mountWith(okReport, { unstable: true, as_of: "x", sli: "healthy", diagnostics: "ok" }, undefined, { reportFails: true });
    await flushPromises();
    expect(wrapper.text()).toContain("Could not load the reliability report");
    expect(wrapper.find('[data-testid="svc-health-pending"]').exists()).toBe(false);
    expect(wrapper.find('[data-testid="svc-health-sli"]').text()).toBe("healthy");
  });

  it("turns a REJECTED objective PUT into the editor's own error, not an unhandled rejection", async () => {
    // The same defect class as [221] P1-1, at the write boundary: a network failure must
    // surface in the editor and unstick the Save button.
    const wrapper = mountWith(okReport);
    await flushPromises();
    await wrapper.find('[data-testid="svc-objective-open"]').trigger("click");
    await wrapper.find('[data-testid="svc-objective-input"]').setValue("99.9");
    apiMock.PUT.mockImplementation(() => Promise.reject(new TypeError("fetch failed")));
    await wrapper.find('[data-testid="svc-objective-save"]').trigger("click");
    await flushPromises();
    expect(wrapper.find('[data-testid="svc-objective-error"]').text()).toContain("Could not save");
    expect(wrapper.find('[data-testid="svc-objective-editor"]').exists()).toBe(true);
    expect(wrapper.find('[data-testid="svc-objective-save"]').attributes("disabled")).toBeUndefined();
  });

  it("fetches the provisional tail after sealed_through as its own bounded request and merges it", async () => {
    // [218] P1-2: the report window ends AT sealed_through, so the reduced-opacity tail the
    // mock promises can only come from a second request [sealed_through, ceil(as_of)).
    const wrapper = mountWith(
      okReport,
      undefined,
      {
        from: "a",
        to: "b",
        step: "day",
        points: [{ start: "2026-08-15T00:00:00Z", epoch_id: "e1", revision_id: "r1", provisional: false, buckets: 24, durations: goodDurations }],
      },
      {
        tail: {
          from: "a",
          to: "b",
          step: "day",
          points: [{ start: "2026-08-16T00:00:00Z", epoch_id: "e1", revision_id: "r1", provisional: true, buckets: 7, durations: goodDurations }],
        },
      },
    );
    await flushPromises();
    type SeriesCall = [string, { params: { query: { from: string; to: string; step: string } } }];
    const seriesCalls = (apiMock.GET.mock.calls as SeriesCall[]).filter(([p]) => p.endsWith("/reliability/series"));
    expect(seriesCalls).toHaveLength(2);
    const tailCall = seriesCalls.find(([, o]) => o.params.query.from === okReport.sealed_through);
    expect(tailCall).toBeDefined();
    // as_of 2026-08-16T12:00:00Z at day step ceils to the next midnight.
    expect(tailCall![1].params.query.to).toBe("2026-08-17T00:00:00.000Z");
    const rects = wrapper.find('[data-testid="svc-timeline"]').findAll("rect[data-state]");
    expect(rects).toHaveLength(2);
    expect(rects[1].attributes("data-provisional")).toBe("true");
    expect(wrapper.find('[data-testid="svc-provisional-note"]').text()).toContain("7 buckets after sealed_through");
  });

  it("weights tick widths by bucket count so a boundary fragment cannot pose as a full day", async () => {
    // [218] P1-3: 24-bucket and 6-bucket points must NOT occupy equal widths.
    const wrapper = mountWith(okReport, undefined, {
      from: "a",
      to: "b",
      step: "day",
      points: [
        { start: "2026-08-14T00:00:00Z", epoch_id: "e1", revision_id: "r1", provisional: false, buckets: 24, durations: goodDurations },
        { start: "2026-08-15T00:00:00Z", epoch_id: "e1", revision_id: "r1", provisional: false, buckets: 6, durations: goodDurations },
      ],
    });
    await flushPromises();
    const rects = wrapper.find('[data-testid="svc-timeline"]').findAll("rect[data-state]");
    expect(rects).toHaveLength(2);
    expect(rects[0].attributes("data-weight")).toBe("24");
    expect(rects[1].attributes("data-weight")).toBe("6");
    const w0 = parseFloat(rects[0].attributes("width")!);
    const w1 = parseFloat(rects[1].attributes("width")!);
    expect(w0).not.toBe(w1);
    expect(w0 / w1).toBeCloseTo(4, 6);
  });

  it("masks a point under an active repair as work in progress, not as its stored status", async () => {
    // [218] P1-4 / §12.1: the store keeps the old facts while a repair is pending; the UI
    // must not present them as data.
    const wrapper = mountWith(
      {
        ...okReport,
        repairing: [{ from: "2026-08-10T00:00:00Z", to: "2026-08-11T00:00:00Z" }],
      },
      undefined,
      {
        from: "a",
        to: "b",
        step: "day",
        points: [
          { start: "2026-08-09T00:00:00Z", epoch_id: "e1", revision_id: "r1", provisional: false, buckets: 24, durations: badDurations },
          { start: "2026-08-10T00:00:00Z", epoch_id: "e1", revision_id: "r1", provisional: false, buckets: 24, durations: badDurations },
        ],
      },
    );
    await flushPromises();
    const timeline = wrapper.find('[data-testid="svc-timeline"]');
    expect(timeline.findAll('rect[data-state="bad"]')).toHaveLength(1);
    expect(timeline.findAll('rect[data-state="repairing"]')).toHaveLength(1);
    expect(wrapper.find('[data-testid="svc-repairing-mask-note"]').exists()).toBe(true);
    expect(wrapper.find('[data-testid="svc-repairing"]').text()).toContain("work in progress");
  });

  it("lets an existing objective be edited and sends the canonical updated value", async () => {
    // [218] P1-5 / §11.3: the objective is a mutable current-view parameter — a stored
    // target must offer the 99.95 → 99.99 change.
    const wrapper = mountWith(okReport);
    await flushPromises();
    await wrapper.find('[data-testid="svc-objective-open"]').trigger("click");
    const input = wrapper.find('[data-testid="svc-objective-input"]');
    expect((input.element as HTMLInputElement).value).toBe("99.95");
    apiMock.PUT.mockResolvedValue({ data: { window: "30d", objective: 99.99 } });
    await input.setValue("99.99");
    await wrapper.find('[data-testid="svc-objective-save"]').trigger("click");
    await flushPromises();
    expect(apiMock.PUT).toHaveBeenCalledTimes(1);
    const putArgs = apiMock.PUT.mock.calls[0] as [string, { body: { objective: number; window: string } }];
    expect(putArgs[1].body.objective).toBe(99.99);
    expect(putArgs[1].body.window).toBe("30d");
    expect(wrapper.find('[data-testid="svc-objective-editor"]').exists()).toBe(false);
  });
});
