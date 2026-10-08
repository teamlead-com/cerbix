import { flushPromises, mount, type VueWrapper } from "@vue/test-utils";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { nextTick } from "vue";

import PublicStatusHistoryView from "@/views/PublicStatusHistoryView.vue";

// iter-0203 (func-status-pages-incidents.md §13.5, FR-039): the incident history page, built to
// the approved mock. Geometry and the live API are proved by e2e/tests/status-history.spec.ts; this
// file pins the view's own rules: the month navigator, the partial and empty months, Show more,
// what goes into the URL, and the access shape it keeps.

const apiMock = vi.hoisted(() => ({ GET: vi.fn() }));
// The route is REACTIVE, as vue-router's is, so the view's watch on `month` is exercised.
const holder = vi.hoisted(() => ({
  route: { params: { slug: "public-status" }, query: {} as Record<string, string> },
}));
const pushMock = vi.hoisted(() => vi.fn());
vi.mock("@/api/client", () => ({ api: apiMock }));
vi.mock("vue-router", async () => {
  const { reactive } = await import("vue");
  holder.route = reactive(holder.route);
  return { useRoute: () => holder.route, useRouter: () => ({ push: pushMock }) };
});
vi.mock("@/composables/useTheme", async () => {
  const { ref } = await import("vue");
  return { useTheme: () => ({ theme: ref("light"), toggle: vi.fn() }) };
});
vi.mock("@/stores/branding", () => ({ useBranding: () => ({ logoUrl: "" }) }));

function past(i: number) {
  return {
    id: `incident-${i}`,
    title: `Past ${i}`,
    status: "resolved",
    impact: "minor",
    source: "manual",
    started_at: "2026-09-10T10:00:00Z",
    resolved_at: "2026-09-10T10:30:00Z",
    created_at: "2026-09-10T10:00:00Z",
    updated_at: "2026-09-10T10:30:00Z",
    affected_component_ids: [],
    updates: [],
  };
}

const MONTHS = [
  { month: "2026-10", count: 2 },
  { month: "2026-09", count: 3 },
  { month: "2026-08", count: 0 },
  { month: "2026-07", count: 1 },
];

function history(month: string, overrides: Record<string, unknown> = {}) {
  const starts: Record<string, string> = {
    "2026-10": "2026-10-01T00:00:00Z",
    "2026-09": "2026-09-01T00:00:00Z",
    "2026-08": "2026-08-01T00:00:00Z",
    "2026-07": "2026-07-10T12:00:00Z", // partial: the window starts mid-month
  };
  return {
    title: "Public Status",
    month,
    from: starts[month],
    to: "2026-10-08T12:00:00Z",
    history_from: "2026-07-10T12:00:00Z",
    months: MONTHS,
    incidents: month === "2026-08" ? [] : [past(1), past(2)],
    next_cursor: null,
    ...overrides,
  };
}

async function mountView(responder: (path: string, opts: { params: { query?: Record<string, string> } }) => unknown) {
  apiMock.GET.mockImplementation((path: string, opts: { params: { query?: Record<string, string> } }) =>
    Promise.resolve(responder(path, opts)),
  );
  const wrapper = mount(PublicStatusHistoryView, { attachTo: document.body });
  await flushPromises();
  return wrapper;
}

const months = (w: VueWrapper) => w.findAll('[data-testid="history-month"]');

describe("PublicStatusHistoryView", () => {
  beforeEach(() => {
    apiMock.GET.mockReset();
    pushMock.mockReset();
    holder.route.query = {};
  });
  afterEach(() => {
    document.body.innerHTML = "";
  });

  it("opens the server's default month and marks it in a navigator of the offered months", async () => {
    const w = await mountView(() => ({ data: history("2026-10") }));
    const [, opts] = apiMock.GET.mock.calls[0];
    expect(apiMock.GET.mock.calls[0][0]).toBe("/api/v1/public/status-pages/{slug}/history");
    expect(opts.params.query).toEqual({}); // no month: the server answers with the current UTC month
    expect(months(w).map((m) => m.text())).toEqual([
      "October 2026 UTC2 incidents",
      "September 2026 UTC3 incidents",
      "August 2026 UTC0 incidents",
      "July 2026 UTC1 incident",
    ]);
    expect(months(w)[0].attributes("aria-current")).toBe("page");
    expect(months(w)[1].attributes("aria-current")).toBeUndefined();
    expect(w.find('[data-testid="history-newer"]').attributes("disabled")).toBeDefined();
    expect(w.find('[data-testid="history-older"]').attributes("disabled")).toBeUndefined();
    expect(w.findAll('[data-testid="past-incident-row"]')).toHaveLength(2);
    expect(w.find("h2").text()).toBe("October 2026 UTC");
    w.unmount();
  });

  it("navigates months through the URL, keeping the unlisted token and never a cursor", async () => {
    holder.route.query = { token: "tok" };
    const w = await mountView(() => ({ data: history("2026-10", { next_cursor: "abc" }) }));
    expect(months(w)[1].attributes("href")).toBe("/status/public-status/history?month=2026-09&token=tok");
    await months(w)[1].trigger("click");
    expect(pushMock).toHaveBeenCalledWith({ query: { token: "tok", month: "2026-09" } });
    await w.find('[data-testid="history-older"]').trigger("click");
    expect(pushMock).toHaveBeenLastCalledWith({ query: { token: "tok", month: "2026-09" } });
    for (const [arg] of pushMock.mock.calls) expect(JSON.stringify(arg)).not.toContain("cursor");
    expect(w.find('[data-testid="history-back"]').attributes("href")).toBe("/status/public-status?token=tok");
    w.unmount();
  });

  it("appends the next page in place with the cursor, and stops when there is none", async () => {
    holder.route.query = { month: "2026-09" };
    const w = await mountView((_, opts) =>
      opts.params.query?.cursor === "c1"
        ? { data: history("2026-09", { incidents: [past(3)], next_cursor: null }) }
        : { data: history("2026-09", { next_cursor: "c1" }) },
    );
    expect(w.find('[data-testid="history-show-more"]').exists()).toBe(true);
    expect(w.text()).toContain("showing 2 of 3");
    await w.find('[data-testid="history-show-more"]').trigger("click");
    await flushPromises();
    const last = apiMock.GET.mock.calls.at(-1)!;
    expect(last[1].params.query).toEqual({ month: "2026-09", cursor: "c1" });
    expect(w.findAll('[data-testid="past-incident-row"]').map((r) => r.text())).toEqual([
      expect.stringContaining("Past 1"),
      expect.stringContaining("Past 2"),
      expect.stringContaining("Past 3"),
    ]);
    expect(w.find('[data-testid="history-show-more"]').exists()).toBe(false);
    expect(pushMock).not.toHaveBeenCalled();
    w.unmount();
  });

  it("says an empty month and a partial oldest month in words", async () => {
    holder.route.query = { month: "2026-08" };
    let w = await mountView(() => ({ data: history("2026-08") }));
    expect(w.find('[data-testid="history-empty"]').text()).toBe("No incidents were resolved in August 2026 UTC.");
    expect(w.find('[data-testid="history-partial"]').exists()).toBe(false);
    w.unmount();

    holder.route.query = { month: "2026-07" };
    w = await mountView(() => ({ data: history("2026-07") }));
    expect(w.find('[data-testid="history-partial"]').text()).toBe(
      "History covers the last 90 days; this month is shown from 10.07.2026 UTC.",
    );
    expect(w.find('[data-testid="history-older"]').attributes("disabled")).toBeDefined();
    w.unmount();
  });

  it("explains a month outside the history instead of showing an empty page", async () => {
    holder.route.query = { month: "2026-01" };
    const w = await mountView(() => ({
      error: { error: "month_outside_history: history covers the last 90 days" },
      response: { status: 400 },
    }));
    expect(w.find('[data-testid="history-refusal"]').text()).toContain("outside the 90-day incident history");
    expect(w.find('[data-testid="history-refusal"] a').attributes("href")).toBe("/status/public-status/history");
    w.unmount();
  });

  it("previews an internal page through the authenticated route when the editor link carries its id", async () => {
    holder.route.query = { preview: "page-1" };
    const w = await mountView((path) =>
      path === "/api/v1/status-pages/{pageID}/history"
        ? { data: history("2026-10") }
        : { error: { error: "not found" }, response: { status: 404 } },
    );
    expect(apiMock.GET.mock.calls.map((c) => c[0])).toEqual([
      "/api/v1/public/status-pages/{slug}/history",
      "/api/v1/status-pages/{pageID}/history",
    ]);
    expect(w.text()).toContain("Internal page");
    expect(months(w)[1].attributes("href")).toBe("/status/public-status/history?month=2026-09&preview=page-1");
    w.unmount();
  });

  // Review R1-2: one generation guard for the first page and Show more. The order responses
  // ARRIVE in is controlled here, which is the only way to see a stale answer win.
  function deferred<T>() {
    let resolve!: (v: T) => void;
    let reject!: (e: unknown) => void;
    const promise = new Promise<T>((res, rej) => {
      resolve = res;
      reject = rej;
    });
    return { promise, resolve, reject };
  }

  it("drops a late Show more answer of the month it has left", async () => {
    holder.route.query = { month: "2026-10" };
    const more = deferred<unknown>();
    apiMock.GET.mockImplementation((_: string, opts: { params: { query?: Record<string, string> } }) => {
      const q = opts.params.query ?? {};
      if (q.cursor === "oct-c1") return more.promise;
      return Promise.resolve({ data: history(q.month ?? "2026-10", q.month === "2026-09" ? {} : { next_cursor: "oct-c1" }) });
    });
    const w = mount(PublicStatusHistoryView, { attachTo: document.body });
    await flushPromises();
    await w.find('[data-testid="history-show-more"]').trigger("click");
    holder.route.query = { month: "2026-09" };
    await flushPromises();
    more.resolve({ data: history("2026-10", { incidents: [past(9)], next_cursor: null }) });
    await flushPromises();
    expect(w.find("h2").text()).toBe("September 2026 UTC");
    expect(w.text()).not.toContain("Past 9");
    expect(w.findAll('[data-testid="past-incident-row"]')).toHaveLength(2);
    w.unmount();
  });

  it("keeps the month the URL names when an earlier month's first page answers last", async () => {
    holder.route.query = { month: "2026-10" };
    const sep = deferred<unknown>();
    apiMock.GET.mockImplementation((_: string, opts: { params: { query?: Record<string, string> } }) => {
      const m = opts.params.query?.month ?? "2026-10";
      if (m === "2026-09") return sep.promise;
      return Promise.resolve({ data: history(m) });
    });
    const w = mount(PublicStatusHistoryView, { attachTo: document.body });
    await flushPromises();
    holder.route.query = { month: "2026-09" };
    await flushPromises();
    holder.route.query = { month: "2026-08" };
    await flushPromises();
    sep.resolve({ data: history("2026-09") });
    await flushPromises();
    expect(w.find("h2").text()).toBe("August 2026 UTC");
    expect(w.find('[data-testid="history-empty"]').exists()).toBe(true);
    w.unmount();
  });

  // Review R1-3: openapi-fetch REJECTS on a transport failure; a rejected promise must not leave a
  // spinner forever.
  it("recovers from a dropped connection on Show more, keeping the rows and allowing a retry", async () => {
    holder.route.query = { month: "2026-09" };
    let fail = true;
    apiMock.GET.mockImplementation((_: string, opts: { params: { query?: Record<string, string> } }) => {
      if (opts.params.query?.cursor === "c1") {
        return fail ? Promise.reject(new TypeError("Failed to fetch")) : Promise.resolve({ data: history("2026-09", { incidents: [past(3)], next_cursor: null }) });
      }
      return Promise.resolve({ data: history("2026-09", { next_cursor: "c1" }) });
    });
    const w = mount(PublicStatusHistoryView, { attachTo: document.body });
    await flushPromises();
    await w.find('[data-testid="history-show-more"]').trigger("click");
    await flushPromises();
    const button = w.find('[data-testid="history-show-more"]');
    expect(button.attributes("disabled")).toBeUndefined();
    expect(button.text()).toBe("Show more");
    expect(w.text()).toContain("Could not load more incidents");
    expect(w.findAll('[data-testid="past-incident-row"]')).toHaveLength(2);
    fail = false;
    await button.trigger("click");
    await flushPromises();
    expect(w.findAll('[data-testid="past-incident-row"]')).toHaveLength(3);
    w.unmount();
  });

  it("recovers from a dropped connection on the first load with a retry", async () => {
    holder.route.query = {};
    let fail = true;
    apiMock.GET.mockImplementation(() =>
      fail ? Promise.reject(new TypeError("Failed to fetch")) : Promise.resolve({ data: history("2026-10") }),
    );
    const w = mount(PublicStatusHistoryView, { attachTo: document.body });
    await flushPromises();
    expect(w.text()).not.toContain("Loading…");
    expect(w.find('[data-testid="history-load-error"]').exists()).toBe(true);
    fail = false;
    await w.find('[data-testid="history-retry"]').trigger("click");
    await flushPromises();
    expect(w.findAll('[data-testid="past-incident-row"]')).toHaveLength(2);
    w.unmount();
  });

  // Review round 2: the ACCESS MODE is state too. A late answer of the authenticated fallback (the
  // page was internal when it was asked) must not switch the view to internal preview after the
  // visitor has moved on and the public route is serving them — its rows were already dropped, and
  // so must be its mode, or the banner lies and Show more goes to the authenticated route.
  it("drops the access mode of a late authenticated answer along with its rows", async () => {
    holder.route.query = { month: "2026-10", preview: "page-1" };
    const authed = deferred<unknown>();
    let publicServes = false;
    apiMock.GET.mockImplementation((path: string, opts: { params: { query?: Record<string, string> } }) => {
      const q = opts.params.query ?? {};
      if (path === "/api/v1/status-pages/{pageID}/history") return authed.promise;
      if (!publicServes) return Promise.resolve({ error: { error: "not found" }, response: { status: 404 } });
      return Promise.resolve({ data: history(q.month ?? "2026-10", { next_cursor: "c1" }) });
    });
    const w = mount(PublicStatusHistoryView, { attachTo: document.body });
    await flushPromises(); // public 404, authenticated fallback pending
    publicServes = true; // the page is public now
    holder.route.query = { month: "2026-09", preview: "page-1" };
    await flushPromises();
    authed.resolve({ data: history("2026-10") });
    await flushPromises();
    expect(w.find("h2").text()).toBe("September 2026 UTC");
    expect(w.text()).not.toContain("Internal page");
    apiMock.GET.mockClear();
    await w.find('[data-testid="history-show-more"]').trigger("click");
    await flushPromises();
    expect(apiMock.GET.mock.calls[0][0]).toBe("/api/v1/public/status-pages/{slug}/history");
    w.unmount();
  });

  it("moves focus to the month heading when the month changes", async () => {
    holder.route.query = {};
    const w = await mountView((_, opts) => ({ data: history(opts.params.query?.month ?? "2026-10") }));
    holder.route.query = { month: "2026-09" };
    await nextTick();
    await flushPromises();
    expect(document.activeElement?.id).toBe("history-month-heading");
    expect(w.find("h2").text()).toBe("September 2026 UTC");
    w.unmount();
  });
});
