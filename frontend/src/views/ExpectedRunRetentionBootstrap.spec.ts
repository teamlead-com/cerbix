import { flushPromises, mount } from "@vue/test-utils";
import { beforeEach, describe, expect, it, vi } from "vitest";

// FR-032 §13a — the retention BOOTSTRAP, at the real panel.
//
// The panel does not know `ledger.expected_run_retention_days`. It opens at the documented default
// of fourteen, and every request it makes before the server has told it otherwise is clamped to a
// bound that may be wrong in either direction:
//
// - too NARROW (an instance keeping ninety days): the question is answered, and the answer both
//   teaches the real bound and describes less time than the panel drew;
// - too WIDE (an instance keeping seven): the question is refused with `range_too_wide` and no
//   body, so the retry at the enforced minimum of two days is what teaches the bound — and that
//   answer describes even less.
//
// Both leave the older part of the drawn span holding no windows, which `coversInterval` renders
// exactly like "no run was due here" — the one distinction this requirement exists to make. Found
// in review of revision 31, twice: the second path survived the first fix (party [7], [9]).
//
// These cases are here rather than in `lib/latencypanel.spec.ts` because the defect is in the
// FETCH, and no library test can see it. Every case re-imports the view: the learned retention is
// module-scoped, so what one case learns would otherwise be the next case's assumption.

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

const DAY_MS = 86_400_000;
const HOUR_MS = 3_600_000;
const T_END = Date.UTC(2026, 8, 5, 12, 0, 0);

const MONITOR = {
  id: "m1", name: "nightly-export", type: "http", target: "https://example.test", method: "GET",
  interval_seconds: 43_200, timeout_seconds: 10, retries: 0, enabled: true, region: "core",
  project_id: "p1", status: "up", execution_revision: 1,
};

/**
 * Sixty checks `stepMs` apart, newest first as the API returns them. At twelve hours the drawn span
 * is 29.5 days and at four hours it is 9.8 — both wider than a bound the panel might hold, and both
 * ordinary: a monitor's interval may be up to a day (`domain.maxIntervalSeconds`).
 */
function heartbeats(stepMs: number) {
  const out: Record<string, unknown>[] = [];
  for (let i = 0; i < 60; i++) {
    out.push({
      monitor_id: "m1", ts: new Date(T_END - i * stepMs).toISOString(),
      up: true, latency_ms: 80 + (i % 5), code: 200, msg: "ok",
    });
  }
  return out;
}

const instantsFor = (stepMs: number) =>
  heartbeats(stepMs).map((hb) => Date.parse(hb.ts as string)).sort((a, b) => a - b);

type LedgerCall = { from: number; to: number; refused: boolean };

/**
 * A server that behaves like `handlers_expectedruns.go`: it REFUSES a range wider than its
 * configured retention with no body at all, and otherwise answers only the windows inside the range
 * it was actually asked for, with `ledger_from` at its own retention floor. Answering more than was
 * asked would hide exactly the defect these cases exist for.
 */
async function mountWithServer(opts: { retentionDays: number; stepMs: number }) {
  const calls: LedgerCall[] = [];
  const instants = instantsFor(opts.stepMs);
  const to = T_END + 1000; // the panel's half-open top: the last point's own window is included
  const ledgerFromMs = Math.max(instants[0] - DAY_MS, to - opts.retentionDays * DAY_MS);

  for (const fn of Object.values(apiMock)) fn.mockReset();
  apiMock.GET.mockImplementation((path: string, init?: unknown) => {
    if (path.endsWith("/monitors/{monitorID}")) return Promise.resolve({ data: MONITOR });
    if (path.endsWith("/heartbeats")) return Promise.resolve({ data: heartbeats(opts.stepMs) });
    if (path.endsWith("/sla")) return Promise.resolve({ data: { windows: [] } });
    if (path.endsWith("/expected-runs")) {
      const q = (init as { params: { query: { from: string; to: string } } }).params.query;
      const from = Date.parse(q.from);
      const askedTo = Date.parse(q.to);
      if (askedTo - from > opts.retentionDays * DAY_MS) {
        calls.push({ from, to: askedTo, refused: true });
        return Promise.resolve({ error: { error: "range_too_wide" } });
      }
      calls.push({ from, to: askedTo, refused: false });
      const windows = instants
        .slice(1)
        .filter((ms) => ms >= from && ms < askedTo)
        .map((ms) => ({ due_at: new Date(ms).toISOString(), verdict: "covered" }));
      return Promise.resolve({
        data: {
          windows,
          ledger_from: new Date(ledgerFromMs).toISOString(),
          retention_days: opts.retentionDays,
          next_cursor: null,
        },
      });
    }
    return Promise.resolve({ data: [] });
  });

  // A fresh module registry per case, so a learned retention never leaks between them.
  vi.resetModules();
  const view = (await import("@/views/MonitorDetailView.vue")).default;
  const w = mount(view, { global: { stubs: { RouterLink: { template: "<a><slot /></a>" } } } });
  for (let i = 0; i < 5; i++) await flushPromises();
  return { w, calls, instants, to, ledgerFromMs };
}

const strokePointCount = (w: { findAll: (s: string) => { attributes: (a: string) => string | undefined }[] }) =>
  w.findAll('[data-testid="lat-stroke"]')
    .map((s) => s.attributes("points")?.split(" ").length ?? 0)
    .reduce((a, b) => a + b, 0);

describe("the expected-run panel's retention bootstrap", () => {
  beforeEach(() => vi.resetModules());

  it("asks again with the wider retention the first answer taught it, and strokes the whole drawn span", async () => {
    // An instance keeping ninety days. The assumed fourteen is ANSWERED, so nothing fails and
    // nothing retries — the truncation is silent, which is what made this survive review once.
    const { w, calls, instants, to } = await mountWithServer({ retentionDays: 90, stepMs: 12 * HOUR_MS });

    expect(calls.map((c) => c.refused)).toEqual([false, false]);
    expect(calls[0].from).toBe(to - 14 * DAY_MS); // clamped to the guess
    expect(calls[0].from).toBeGreaterThan(instants[0]); // and therefore inside what the panel drew
    expect(calls[1].from).toBe(instants[0]); // the whole drawn span: ninety days is wider than it
    expect(calls[1].to).toBe(to);

    // The SECOND answer is the one on screen. With only the first, the older half of the span holds
    // no windows and the stroke reaches back a fortnight and stops.
    expect(w.findAll('[data-testid="lat-stroke"]')).toHaveLength(1);
    expect(strokePointCount(w)).toBe(60);
    const cells = w.findAll('[data-testid="lat-expect-cell"]');
    expect(cells).toHaveLength(59);
    expect(cells.every((c) => c.attributes("data-kind") === "covered")).toBe(true);
  });

  it("asks a third time when the bound was learned from the MINIMUM fallback, not just from the default", async () => {
    // An instance keeping seven days. Fourteen is refused with no body, two is answered and teaches
    // seven — and stopping there draws two days of a nine-day panel while the server holds seven.
    const { w, calls, instants, to, ledgerFromMs } = await mountWithServer({ retentionDays: 7, stepMs: 4 * HOUR_MS });

    expect(calls.map((c) => c.refused)).toEqual([true, false, false]);
    // The drawn span is 9.8 days, so the assumed fourteen never clamps it: the panel asks for
    // everything it drew and the seven-day instance refuses the whole question.
    expect(calls[0].from).toBe(instants[0]);
    expect(calls[1].from).toBe(to - 2 * DAY_MS); // the enforced minimum, which teaches the bound
    expect(calls[2].from).toBe(to - 7 * DAY_MS); // the bound itself, which is what the panel needs

    // What reached the screen is the seven-day answer: the stroke covers every point the ledger can
    // defend, which is every point at or after `ledger_from`.
    const drawn = instantsFor(4 * HOUR_MS);
    const defensible = drawn.filter((ms) => ms >= ledgerFromMs).length;
    const twoDaysOnly = drawn.filter((ms) => ms >= to - 2 * DAY_MS).length;
    expect(defensible).toBeGreaterThan(twoDaysOnly); // the case is only meaningful if it discriminates
    expect(strokePointCount(w)).toBe(defensible);

    // And the time the server genuinely cannot speak for is marked as such rather than left blank.
    expect(w.find('[data-testid="lat-ledger-from"]').exists()).toBe(true);
    const kinds = w.findAll('[data-testid="lat-expect-cell"]').map((c) => c.attributes("data-kind"));
    expect(kinds).toContain("covered");
    expect(w.findAll('[data-testid="lat-point"]').length).toBeGreaterThan(defensible);
  });

  it("asks once when the instance keeps exactly what the panel assumed", async () => {
    // Nothing learned changes the question, so a second request would be duplication on every load.
    const { calls, to } = await mountWithServer({ retentionDays: 14, stepMs: 12 * HOUR_MS });
    expect(calls).toHaveLength(1);
    expect(calls[0].from).toBe(to - 14 * DAY_MS);
  });

  it("asks once when the drawn span was never clamped, however wide the retention turns out to be", async () => {
    // Sixty checks a minute apart: the whole span is inside the assumption, so the first answer is
    // already complete and a learned ninety days changes nothing about it.
    const { calls, instants } = await mountWithServer({ retentionDays: 90, stepMs: 60_000 });
    expect(calls).toHaveLength(1);
    expect(calls[0].from).toBe(instants[0]);
  });
});
