import { flushPromises, mount } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { describe, expect, it, vi } from "vitest";

import EscalationView from "@/views/EscalationView.vue";
import { useSession } from "@/stores/session";
import { useWorkspace } from "@/stores/workspace";

// E1 — editing a schedule leaves its rotation ANCHOR exactly where it was.
//
// The pre-fill sliced the zone marker off the RFC-3339 instant — `"…T00:00:00Z".slice(0, 16)` — and
// a `datetime-local` control reads its value as LOCAL. The save path parses it as local too, so the
// anchor moved by the viewer's offset every time anything about the schedule was saved: editing the
// NAME silently re-ordered who gets paged, by five and a half hours at UTC+05:30. What this range
// added was the NFR-025c zone hint beside the control, which then positively asserted "local time
// (UTC+05:30)" about a value that was in UTC — the range did not create the bug, it made the UI
// claim the bug away.
//
// The assertion is on the INSTANT that reaches the API, not on the control's text, and it holds in
// every zone: at UTC the two are identical and the case is vacuous, which is exactly why the gate
// runs this suite a second time at `TZ=Asia/Kolkata`.
//
// The mutation that must kill this: slice the instant again.

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), PATCH: vi.fn(), PUT: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));
vi.mock("@/components/AppShell.vue", () => ({
  default: { name: "AppShell", template: "<div><slot name='actions' /><slot /></div>" },
}));

// A whole-minute instant, because the control carries minute precision: seconds are truncated by
// the round trip whatever the zone, which is a property of `datetime-local` and not of this fix.
const ANCHOR = "2026-03-01T00:00:00Z";
const SCHEDULE = {
  id: "sch-1",
  project_id: "project-a",
  name: "primary",
  rotation: "weekly",
  shift_seconds: 604800,
  anchor_at: ANCHOR,
  participants: ["user-a"],
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
    if (path.endsWith("/notification-channels")) return Promise.resolve({ data: [{ id: "ch-1", name: "pager" }] });
    if (path.endsWith("/members")) return Promise.resolve({ data: [{ user_id: "user-a", email: "a@example.com" }] });
    return Promise.resolve({ data: [] });
  });
  apiMock.PUT.mockResolvedValue({ data: { ...SCHEDULE } });
  apiMock.PATCH.mockResolvedValue({ data: { ...SCHEDULE } });
  apiMock.POST.mockResolvedValue({ data: { ...SCHEDULE } });
  return mount(EscalationView, { global: { plugins: [pinia] } });
}

describe("a cosmetic edit does not move the rotation anchor", () => {
  it("sends back the instant it was given, in any zone", async () => {
    const w = mountView();
    await flushPromises();

    // Open the SCHEDULE for editing — the pre-fill under test. The page has an Edit control per
    // policy as well, so the right one is identified by what it opens rather than by its position.
    let opened = false;
    for (const b of w.findAll("button")) {
      if (b.text().trim() !== "Edit") continue;
      await b.trigger("click");
      await flushPromises();
      if (w.text().includes("Edit schedule: primary")) {
        opened = true;
        break;
      }
    }
    expect(opened, "no control opened the schedule editor").toBe(true);

    const nameInput = w.findAll("input").find((i) => (i.element as HTMLInputElement).value === "primary");
    expect(nameInput, "the schedule name field did not pre-fill").toBeTruthy();
    await nameInput!.setValue("primary (renamed)");

    // The form is submitted rather than the button clicked: the handler is bound with
    // `@submit.prevent`, so a click on a jsdom button does not reach it.
    const form = w.findAll("form").find((f) => f.find('input[type="datetime-local"]').exists() && f.text().includes("Edit schedule"));
    expect(form, "the schedule form is gone").toBeTruthy();
    await form!.trigger("submit");
    await flushPromises();

    const calls = [...apiMock.PUT.mock.calls, ...apiMock.PATCH.mock.calls, ...apiMock.POST.mock.calls];
    const sent = calls.map((c) => (c[1] as { body?: { anchor_at?: string } })?.body).find((b) => b?.anchor_at);
    expect(sent, `no request carried an anchor: ${JSON.stringify(calls)}`).toBeTruthy();

    // The same INSTANT, whatever zone this runner is in. A sliced pre-fill sends a different one in
    // every zone but UTC.
    expect(
      new Date(sent!.anchor_at!).getTime(),
      `the anchor moved from ${ANCHOR} to ${sent!.anchor_at} — a rename re-ordered the rotation`,
    ).toBe(new Date(ANCHOR).getTime());
  });
});
