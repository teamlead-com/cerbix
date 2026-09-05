import { flushPromises, mount } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { describe, expect, it, vi } from "vitest";

import EscalationView from "@/views/EscalationView.vue";
import { useSession } from "@/stores/session";
import { useWorkspace } from "@/stores/workspace";

// NFR-025, the control-surface half, at the one place a single hint serves TWO controls.
//
// The vacation-cover row is `starts → ends` on one line: two `datetime-local` inputs, one subject.
// The first repair put a single `localInputZoneHint(starts_at)` beside both and declared
// `data-covers="2"` — and the reviewer refused it, correctly. A declaration that one label covers
// two controls is not evidence that it says the right thing about the SECOND: a range crossing a
// DST change genuinely has two offsets, and a hint computed from the start alone tells an operator
// the end is in a zone it is not.
//
// So the surface must bind BOTH values. This mounts the real view, types a range across the
// European spring change, and reads what the operator sees.

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), PATCH: vi.fn(), PUT: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));
vi.mock("@/components/AppShell.vue", () => ({
  default: { name: "AppShell", template: "<div><slot name='actions' /><slot /></div>" },
}));

const SCHEDULE = {
  id: "sch-1",
  project_id: "project-a",
  name: "primary",
  rotation: "weekly",
  shift_seconds: 604800,
  anchor_at: "2026-03-01T00:00:00Z",
  members: [],
  overrides: [],
};

function mountView() {
  const pinia = createPinia();
  setActivePinia(pinia);
  const ws = useWorkspace();
  ws.orgId = "org-a";
  ws.projectId = "project-a";
  ws.loaded = true;
  const session = useSession();
  session.user = { id: "user-a", is_global_admin: true } as typeof session.user;
  for (const fn of Object.values(apiMock)) fn.mockReset();
  apiMock.GET.mockImplementation((path: string) => {
    if (path.endsWith("/oncall-schedules")) return Promise.resolve({ data: [SCHEDULE] });
    // The vacation row is `v-if="canWrite && channels.length"` — an operator with no channel has
    // nothing to cover WITH, so the form is honestly absent. One channel makes it appear.
    if (path.endsWith("/notification-channels")) return Promise.resolve({ data: [{ id: "ch-1", name: "pager" }] });
    return Promise.resolve({ data: [] });
  });
  return mount(EscalationView, { global: { plugins: [pinia] } });
}

describe("the vacation-cover range names the zone of BOTH ends", () => {
  it("names one offset for a range inside one, and both across a DST change", async () => {
    const w = mountView();
    await flushPromises();

    const inputs = w.findAll('input[type="datetime-local"]');
    expect(inputs.length, "the vacation range inputs are gone").toBeGreaterThanOrEqual(2);

    // Europe/Berlin's spring change is 2026-03-29. A window from the 28th to the 30th is entered
    // at UTC+01:00 and ends at UTC+02:00 — the case the single-value hint could not answer.
    await inputs[0].setValue("2026-03-28T12:00");
    await inputs[1].setValue("2026-03-30T12:00");
    await flushPromises();

    const hint = w.find('[data-testid="vacation-zone"]');
    expect(hint.exists(), "the vacation zone hint is missing").toBe(true);
    // The runner's zone decides the digits, so the assertion is the SHAPE: when the two ends
    // differ, the operator is shown two offsets and not one. At a zone with no DST in that window
    // they legitimately coincide, and the single form is then the honest answer.
    const both = /^local time \(UTC[+-]\d{2}:\d{2} → UTC[+-]\d{2}:\d{2}\)$/;
    const one = /^local time \(UTC[+-]\d{2}:\d{2}\)$/;
    expect(hint.text()).toMatch(new RegExp(`${both.source}|${one.source}`));

    // Whether this run can SEE the difference depends on the runner's zone: a zone with no DST
    // between the two dates legitimately renders one offset for both, and asserting a change
    // there would be asserting the runner's configuration. So the reaction is required only when
    // the zone actually distinguishes the two ends — and the binding itself is held in every zone
    // by the source scan in `wallclock.spec.ts` ("passes two distinct values to every range zone
    // hint"), which is what kills a call site that passes the start twice.
    const before = hint.text();
    await inputs[1].setValue("2026-03-28T13:00");
    await flushPromises();
    const after = w.find('[data-testid="vacation-zone"]').text();
    if (both.test(before)) {
      expect(after, `the hint ignored the end: before=${before} after=${after}`).not.toBe(before);
      expect(after).toMatch(one);
    } else {
      expect(before).toMatch(one);
      expect(after).toMatch(one);
    }
  });
});
