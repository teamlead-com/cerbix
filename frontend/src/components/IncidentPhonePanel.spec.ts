import { type VueWrapper, flushPromises, mount } from "@vue/test-utils";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import IncidentPhonePanel from "@/components/IncidentPhonePanel.vue";
import { relTime } from "@/lib/incident";
import type { PhoneDelivery } from "@/lib/phone";
import { instantClockDayLabel } from "@/lib/wallclock";
import IncidentDetailView from "@/views/IncidentDetailView.vue";

// FR-041a §4.5 and §5.5, mock §4 (4a–4g), revision 6 and the §14 tables: the incident page's Phone
// panel. What is pinned here:
//
//   * WHEN it shows (revision 6): with a delivery, OR with telephony on. Telephony off and no
//     delivery → no panel at all (4g-1); a trace is never hidden because the switch is off now
//     (4g-2, P7). A failed read is 4d whatever the switch says, because the deliveries are unknown.
//   * The rows are the API's, in the API's order — OLDEST first (P3-2) — never re-sorted.
//   * Each row: the reason chip in words and hue, the channel, the mask (never a number), SMS / Call,
//     the alert without the subject the page already names (one label for a monitor's recovery,
//     "Service alert · recovered" for a service's), the second line (HTTP status, provider code,
//     attempts — never "timeout", "5 s" or "retrying"; a storm limit named without a number; the
//     recipient's name for a not_deliverable cause), and the notes (`no_incident_ref` with its
//     words; an unknown note as its raw chip alone).
//   * The view's read (`loadPhone`): an answer for an incident the page has left is dropped — it
//     never paints its deliveries on the incident now on screen.

// Only the last describe mounts IncidentDetailView; these mocks are what it needs (as in
// IncidentDetailView.spec.ts), with a route whose id can change under the mounted page.
const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));
const routeRaw = vi.hoisted(() => ({ params: { id: "inc-a" } }));
vi.mock("vue-router", async () => {
  const { reactive } = await import("vue");
  const route = reactive(routeRaw);
  return { useRoute: () => route, RouterLink: { props: ["to"], template: "<a><slot /></a>" } };
});
vi.mock("@/components/AppShell.vue", () => ({
  default: { name: "AppShell", template: "<div><slot name='actions' /><slot /></div>" },
}));
vi.mock("@/stores/session", () => ({ useSession: () => ({ canProjectWrite: () => true, telephonyEnabled: true }) }));
const workspaceRaw = vi.hoisted(() => ({
  init: () => Promise.resolve(),
  orgId: "o1",
  projectId: "p1",
  orgName: "Org A",
  projectName: "Project A",
  transitionPending: false,
  transitionError: "",
  transition: { generation: 0, pendingOrgId: "", error: "", failedOrgId: "" },
}));
vi.mock("@/stores/workspace", async () => {
  const { reactive } = await import("vue");
  const ws = reactive(workspaceRaw);
  return { useWorkspace: () => ws };
});
vi.mock("@/stores/ui", () => ({ useUi: () => ({ takeIncidentNotice: () => "" }) }));

const NOW = "2026-11-18T14:28:00Z";
const AT = (hms: string) => `2026-11-18T${hms}Z`;

function delivery(over: Partial<PhoneDelivery>): PhoneDelivery {
  return {
    id: "d",
    channel_id: "nc",
    channel_name: "SMS — on-call",
    kind: "sms",
    alert_kind: "service_health",
    number_masked: "+1•••••0147",
    reason: "sent",
    notes: [],
    provider_status: 201,
    provider_code: null,
    provider_ref: null,
    segments: 1,
    attempts: 1,
    created_at: AT("14:06:58"),
    updated_at: AT("14:06:59"),
    subject: { kind: "service", name: "checkout" },
    service_state: "down",
    escalation_step: null,
    recipient_display_name: null,
    ...over,
  };
}

const step = (n: number): Partial<PhoneDelivery> => ({
  alert_kind: "escalation_step",
  escalation_step: n,
  service_state: null,
});

// Mock 4a: a service incident on its second escalation step, every part-(a) outcome that can meet
// one incident — oldest first, as the API returns it.
const MOCK_4A: PhoneDelivery[] = [
  delivery({ id: "a1", provider_ref: "SM3f00000000000000a91c" }),
  delivery({ id: "a2", channel_name: "NOC duty phone", number_masked: "+44•••••0199", provider_ref: "SM8b00000000000000004de" }),
  delivery({
    id: "a3",
    channel_name: "SMS — Eli",
    number_masked: null,
    reason: "not_deliverable",
    notes: ["recipient_not_eligible"],
    provider_status: null,
    segments: 0,
    attempts: 0,
    recipient_display_name: "Eli Moreau",
    created_at: AT("14:06:59"),
  }),
  delivery({
    id: "a4",
    channel_name: "Call — Ada",
    kind: "voice",
    number_masked: "+90•••••6869",
    reason: "provider_rejected",
    provider_status: 400,
    provider_code: "21215",
    segments: 0,
    created_at: AT("14:14:02"),
    ...step(1),
  }),
  delivery({ id: "a5", reason: "outcome_unknown", provider_status: null, segments: 0, created_at: AT("14:21:05"), ...step(2) }),
  delivery({
    id: "a6",
    channel_name: "Call — Chen",
    kind: "voice",
    reason: "provider_unreachable",
    provider_status: 503,
    segments: 0,
    attempts: 3,
    created_at: AT("14:21:05"),
    ...step(2),
  }),
  delivery({
    id: "a7",
    channel_name: "Call — Dara",
    kind: "voice",
    number_masked: "+49•••••3302",
    reason: "sending",
    provider_status: null,
    segments: 0,
    attempts: 0,
    created_at: AT("14:21:06"),
    ...step(2),
  }),
];

const mounted: VueWrapper[] = [];
function mountPanel(props: { state?: "loading" | "ready" | "failed"; deliveries?: PhoneDelivery[]; telephonyEnabled?: boolean }) {
  const w = mount(IncidentPhonePanel, {
    props: { state: props.state ?? "ready", deliveries: props.deliveries ?? [], telephonyEnabled: props.telephonyEnabled ?? false },
  });
  mounted.push(w);
  return w;
}

const norm = (s: string) => s.replace(/\s+/g, " ").trim();
const panel = (w: VueWrapper) => w.find('[data-testid="incident-phone-panel"]');
const rows = (w: VueWrapper) => w.findAll('[data-testid="delivery-row"]');
type Row = ReturnType<typeof rows>[number];
const part = (row: Row, id: string) => norm(row.get(`[data-testid="${id}"]`).text());
const detail = (row: Row) =>
  row
    .findAll('[data-testid="delivery-detail"] > *')
    .map((e) => norm(e.text()))
    .join(" ");

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date(NOW));
});
afterEach(() => {
  while (mounted.length) mounted.pop()!.unmount();
  vi.useRealTimers();
});

describe("IncidentPhonePanel — when it shows (revision 6, §5.5)", () => {
  it("4g-1: no delivery and telephony off → no panel at all", async () => {
    const w = mountPanel({ deliveries: [], telephonyEnabled: false });
    await flushPromises();
    expect(panel(w).exists()).toBe(false);
    expect(norm(w.text())).toBe("");
  });

  it("4c: no delivery and telephony on → the panel with its one empty line", async () => {
    const w = mountPanel({ deliveries: [], telephonyEnabled: true });
    expect(panel(w).exists()).toBe(true);
    expect(norm(w.get('[data-testid="incident-phone-title"]').text())).toBe("Phone");
    expect(norm(w.get('[data-testid="incident-phone-empty"]').text())).toBe("No phone alert was sent for this incident.");
    // No count chip on an empty list; the status-page chip stays.
    expect(w.find('[data-testid="incident-phone-count"]').exists()).toBe(false);
    expect(norm(w.get('[data-testid="incident-phone-public"]').text())).toBe("not on the status page");
    expect(rows(w)).toHaveLength(0);
  });

  it("4g-2: deliveries and telephony off → the panel with both rows; the switch-off row reads its sentence", () => {
    const w = mountPanel({
      telephonyEnabled: false,
      deliveries: [
        delivery({
          id: "g1",
          channel_name: "SMS — Ada",
          number_masked: "+90•••••6869",
          alert_kind: "monitor_down",
          subject: { kind: "monitor", name: "checkout-http" },
          service_state: null,
          created_at: "2026-11-18T08:02:11Z",
        }),
        delivery({
          id: "g2",
          channel_name: "SMS — Ada",
          number_masked: null,
          reason: "telephony_disabled",
          alert_kind: "monitor_recovered",
          subject: { kind: "monitor", name: "checkout-http" },
          service_state: null,
          provider_status: null,
          segments: 0,
          attempts: 0,
          created_at: "2026-11-18T09:47:30Z",
        }),
      ],
    });
    expect(panel(w).exists()).toBe(true);
    expect(norm(w.get('[data-testid="incident-phone-count"]').text())).toBe("2 deliveries");
    const [sent, off] = rows(w);
    expect(part(sent, "delivery-alert")).toBe("Monitor down");
    expect(part(off, "delivery-reason")).toBe("Phone alerts off");
    expect(part(off, "delivery-alert")).toBe("Monitor recovery");
    expect(detail(off)).toBe("Phone alerts were switched off on this instance.");
    expect(w.find('[data-testid="incident-phone-empty"]').exists()).toBe(false);
  });

  it("4d: a failed read says what it is not, dashed — with telephony off as with it on", () => {
    for (const telephonyEnabled of [false, true]) {
      const w = mountPanel({ state: "failed", telephonyEnabled });
      expect(panel(w).exists(), `telephony ${telephonyEnabled}`).toBe(true);
      const el = w.get('[data-testid="incident-phone-unavailable"]');
      expect(norm(el.text())).toBe("Phone activity could not be read — this is not “nothing was sent”.");
      expect(el.classes()).toContain("border-dashed");
      expect(w.find('[data-testid="incident-phone-empty"]').exists()).toBe(false);
      expect(w.find('[data-testid="incident-phone-count"]').exists()).toBe(false);
    }
  });

  it("renders nothing while the list is being read — the rule reads the list first", () => {
    const w = mountPanel({ state: "loading", telephonyEnabled: true });
    expect(panel(w).exists()).toBe(false);
  });
});

describe("IncidentPhonePanel — mock 4a", () => {
  it("counts the deliveries: “7 deliveries”, “1 delivery”", () => {
    expect(norm(mountPanel({ deliveries: MOCK_4A }).get('[data-testid="incident-phone-count"]').text())).toBe("7 deliveries");
    expect(norm(mountPanel({ deliveries: [MOCK_4A[0]] }).get('[data-testid="incident-phone-count"]').text())).toBe("1 delivery");
  });

  it("keeps the API's order — oldest first — and never re-sorts", () => {
    expect(rows(mountPanel({ deliveries: MOCK_4A })).map((r) => r.attributes("data-id"))).toEqual([
      "a1",
      "a2",
      "a3",
      "a4",
      "a5",
      "a6",
      "a7",
    ]);
    // Rows whose times would sort otherwise stay as given: the server owns the order.
    const given = [MOCK_4A[6], MOCK_4A[0], MOCK_4A[3]];
    expect(rows(mountPanel({ deliveries: given })).map((r) => r.attributes("data-id"))).toEqual(["a7", "a1", "a4"]);
  });

  it("draws every row: reason chip and hue, channel, mask, SMS / Call, alert, second line", () => {
    const w = mountPanel({ deliveries: MOCK_4A });
    const want = [
      { chip: "Sent", tone: "text-up", cn: "SMS — on-call", mask: "+1•••••0147", kind: "SMS", alert: "Service alert · down", l2: "HTTP 201 1 segment 1 attempt ref SM3f…a91c" },
      { chip: "Sent", tone: "text-up", cn: "NOC duty phone", mask: "+44•••••0199", kind: "SMS", alert: "Service alert · down", l2: "HTTP 201 1 segment 1 attempt ref SM8b…04de" },
      {
        chip: "Not deliverable",
        tone: "text-ink-3",
        cn: "SMS — Eli",
        mask: null,
        kind: "SMS",
        alert: "Service alert · down",
        l2: "recipient_not_eligible Eli Moreau could no longer open this project when it was sent",
      },
      { chip: "Rejected by provider", tone: "text-down", cn: "Call — Ada", mask: "+90•••••6869", kind: "Call", alert: "Escalation step 1", l2: "HTTP 400 · code 21215 1 attempt" },
      { chip: "Outcome unknown", tone: "text-degraded", cn: "SMS — on-call", mask: "+1•••••0147", kind: "SMS", alert: "Escalation step 2", l2: "1 attempt" },
      { chip: "Provider unreachable", tone: "text-degraded", cn: "Call — Chen", mask: "+1•••••0147", kind: "Call", alert: "Escalation step 2", l2: "HTTP 503 3 attempts" },
      { chip: "Sending", tone: "border-dashed", cn: "Call — Dara", mask: "+49•••••3302", kind: "Call", alert: "Escalation step 2", l2: "waiting for the provider's answer" },
    ];
    const r = rows(w);
    expect(r).toHaveLength(7);
    want.forEach((x, i) => {
      const row = r[i];
      expect(part(row, "delivery-reason"), `row ${i + 1} chip`).toBe(x.chip);
      expect(row.get('[data-testid="delivery-reason"]').classes(), `row ${i + 1} tone`).toContain(x.tone);
      expect(part(row, "delivery-channel"), `row ${i + 1} channel`).toBe(x.cn);
      if (x.mask) expect(part(row, "delivery-mask"), `row ${i + 1} mask`).toBe(x.mask);
      else expect(row.find('[data-testid="delivery-mask"]').exists(), `row ${i + 1} has no number`).toBe(false);
      expect(part(row, "delivery-kind"), `row ${i + 1} kind`).toBe(x.kind);
      expect(part(row, "delivery-alert"), `row ${i + 1} alert`).toBe(x.alert);
      expect(detail(row), `row ${i + 1} second line`).toBe(x.l2);
    });
    // The cause of not_deliverable is the small mono chip; outcome_unknown has its sentence.
    expect(r[2].get('[data-testid="delivery-note"]').text()).toBe("recipient_not_eligible");
    expect(part(r[4], "delivery-why")).toBe(
      "The process sending it stopped before the provider's answer was stored. It may have arrived; cerbix did not send it again, so it is not counted as delivered.",
    );
    expect(r[0].find('[data-testid="delivery-why"]').exists()).toBe(false);
  });

  it("heads the panel “Phone”, the count, and “not on the status page”", () => {
    const w = mountPanel({ deliveries: MOCK_4A });
    expect(norm(w.get('[data-testid="incident-phone-title"]').text())).toBe("Phone");
    expect(norm(w.get('[data-testid="incident-phone-public"]').text())).toBe("not on the status page");
  });

  it("puts the clock over “21m ago”, with the offset of the instant", () => {
    const w = mountPanel({ deliveries: MOCK_4A });
    const row = rows(w)[0];
    const at = instantClockDayLabel(MOCK_4A[0].created_at);
    expect(part(row, "delivery-clock")).toBe(at.clock);
    expect(at.clock).toMatch(/^\d\d:\d\d:\d\d$/);
    expect(part(row, "delivery-ago")).toBe("21m ago");
    expect(part(row, "delivery-ago")).toBe(relTime(MOCK_4A[0].created_at));
    expect(part(row, "delivery-offset")).toBe(at.offset);
  });

  it("names the alert without the subject the page already names; one label for a monitor's recovery", () => {
    const w = mountPanel({
      deliveries: [
        delivery({ id: "k1", alert_kind: "monitor_recovered", subject: { kind: "monitor", name: "checkout-http" }, service_state: null }),
        delivery({ id: "k2", alert_kind: "service_recovered", service_state: null }),
        delivery({ id: "k3", alert_kind: "monitor_down", subject: null, service_state: null }),
      ],
    });
    expect(rows(w).map((r) => part(r, "delivery-alert"))).toEqual(["Monitor recovery", "Service alert · recovered", "Monitor down"]);
  });

  it("shows only masked numbers — a full number that reaches the panel anyway is never printed", () => {
    // The API sends no full number; this one is planted where a careless row would pick it up. The
    // channel's name is the operator's own text and is printed as it is, so it is not planted there.
    const FULL = "+905551236869";
    const leaky = MOCK_4A.map((d) => ({ ...d, number: FULL, to: FULL, recipient_number: FULL }) as PhoneDelivery);
    const w = mountPanel({ deliveries: leaky });
    const UNMASKED = /\d{7,}/; // a mask keeps at most 4 digits in a row; no other text here has 7
    expect(FULL, "the pattern catches the planted number").toMatch(UNMASKED);
    expect(rows(w)).toHaveLength(7);
    expect(w.text()).not.toMatch(UNMASKED);
    expect(w.html()).not.toContain("5551236869");
    expect(part(rows(w)[3], "delivery-mask")).toBe("+90•••••6869");
  });

  it("never says “timeout”, “5 s” or “retrying”", () => {
    const w = mountPanel({ deliveries: MOCK_4A });
    expect(norm(w.text())).not.toMatch(/timeout|retrying|5 s\b/i);
  });
});

describe("IncidentPhonePanel — 4b: every part-(a) reason as the panel prints it", () => {
  it("prints the chip and the second line of each of the nine reasons", () => {
    const cases: [Partial<PhoneDelivery>, string, string][] = [
      [{ reason: "sending", provider_status: null, attempts: 0 }, "Sending", "waiting for the provider's answer"],
      [{ reason: "sent" }, "Sent", "HTTP 201 1 segment 1 attempt"],
      [{ reason: "provider_unreachable", provider_status: 429, segments: 0, attempts: 2 }, "Provider unreachable", "HTTP 429 2 attempts"],
      [{ reason: "provider_rejected", provider_status: 302, segments: 0 }, "Rejected by provider", "HTTP 302 · redirect not followed 1 attempt"],
      [{ reason: "outcome_unknown", provider_status: null, segments: 0 }, "Outcome unknown", "1 attempt"],
      [
        { reason: "not_deliverable", notes: ["no_verified_number"], recipient_display_name: "Ada Park", number_masked: null, provider_status: null, attempts: 0 },
        "Not deliverable",
        "no_verified_number Ada Park had no verified number when it was sent",
      ],
      [
        { reason: "voice_unsupported_by_provider", kind: "voice", provider_status: null, attempts: 0 },
        "Calls not supported",
        "The instance's provider cannot place calls.",
      ],
      [{ reason: "telephony_disabled", provider_status: null, attempts: 0 }, "Phone alerts off", "Phone alerts were switched off on this instance."],
      [
        { reason: "storm_limited", notes: ["storm_per_incident"], provider_status: null, attempts: 0 },
        "Storm limit",
        "too many messages to this number for this incident in the last hour",
      ],
    ];
    const w = mountPanel({ deliveries: cases.map(([over], i) => delivery({ id: `r${i}`, ...over })) });
    const r = rows(w);
    expect(r).toHaveLength(9);
    cases.forEach(([, chip, l2], i) => {
      expect(part(r[i], "delivery-reason"), `reason ${i}`).toBe(chip);
      expect(detail(r[i]), `reason ${i} second line`).toBe(l2);
    });
  });

  it("names a storm limit without a number, for either limit", () => {
    const w = mountPanel({
      deliveries: [
        delivery({ id: "s1", reason: "storm_limited", notes: ["storm_per_incident"], provider_status: null, attempts: 0, segments: 0 }),
        delivery({ id: "s2", reason: "storm_limited", notes: ["storm_per_number"], provider_status: null, attempts: 0, segments: 0 }),
      ],
    });
    for (const row of rows(w)) expect(detail(row)).not.toMatch(/\d/);
    expect(detail(rows(w)[1])).toBe("too many messages to this number in the last hour");
  });

  it("prints the link notes beside any reason with their words", () => {
    const w = mountPanel({ deliveries: [delivery({ id: "n1", notes: ["no_public_url", "link_omitted"] })] });
    const row = rows(w)[0];
    expect(row.findAll('[data-testid="delivery-note"]').map((n) => n.text())).toEqual(["no_public_url", "link_omitted"]);
    expect(row.findAll('[data-testid="delivery-note-label"]').map((n) => norm(n.text()))).toEqual([
      "sent without a link",
      "link dropped to fit the message",
    ]);
  });

  it("shows an unknown note as its raw chip alone, never dropped", () => {
    const w = mountPanel({ deliveries: [delivery({ id: "n2", notes: ["a_future_note"] })] });
    const row = rows(w)[0];
    expect(row.findAll('[data-testid="delivery-note"]').map((n) => n.text())).toEqual(["a_future_note"]);
    expect(row.findAll('[data-testid="delivery-note-label"]')).toHaveLength(0);
  });
});

describe("IncidentPhonePanel — 4e: a monitor's DOWN sent before the incident opened", () => {
  it("reads the no_incident_ref note as “sent before the incident opened — no link”", () => {
    const w = mountPanel({
      deliveries: [
        delivery({
          id: "e1",
          channel_name: "SMS — Ada",
          number_masked: "+90•••••6869",
          alert_kind: "monitor_down",
          subject: { kind: "monitor", name: "checkout-http" },
          service_state: null,
          notes: ["no_incident_ref"],
        }),
      ],
    });
    expect(norm(w.get('[data-testid="incident-phone-count"]').text())).toBe("1 delivery");
    const row = rows(w)[0];
    expect(part(row, "delivery-alert")).toBe("Monitor down");
    expect(part(row, "delivery-note")).toBe("no_incident_ref");
    expect(part(row, "delivery-note-label")).toBe("sent before the incident opened — no link");
    expect(detail(row)).toBe("HTTP 201 1 segment 1 attempt no_incident_ref sent before the incident opened — no link");
  });
});

// Task 6 review M-1: `loadPhone` checks, after its await, that the page is still on the incident it
// read for. Without that check, a slow answer for incident A that lands after the page moved to B
// paints A's deliveries on B's page.
describe("IncidentDetailView — the Phone panel's read drops an answer for an incident the page has left", () => {
  function deferred<T>() {
    let resolve!: (value: T) => void;
    const promise = new Promise<T>((res) => {
      resolve = res;
    });
    return { promise, resolve };
  }
  const incident = (id: string) => ({
    id,
    project_id: "p1",
    title: `incident ${id}`,
    status: "investigating",
    impact: "major",
    source: "manual",
    started_at: "2026-11-18T14:00:00Z",
    impacts: [],
  });

  beforeEach(() => {
    routeRaw.params.id = "inc-a";
    apiMock.GET.mockReset();
    apiMock.POST.mockReset();
  });

  it("A's late answer, arriving on B's page, paints nothing", async () => {
    const phoneA = deferred<{ data: { deliveries: PhoneDelivery[] } }>();
    apiMock.GET.mockImplementation((path: string, init?: { params?: { path?: { incidentID?: string } } }) => {
      const incidentID = init?.params?.path?.incidentID ?? "";
      if (path.endsWith("/updates")) return Promise.resolve({ data: [] });
      if (path.endsWith("/postmortem")) return Promise.resolve({ error: { error: "not found" } });
      if (path.endsWith("/phone-activity")) {
        return incidentID === "inc-a" ? phoneA.promise : Promise.resolve({ data: { deliveries: [] } });
      }
      return Promise.resolve({ data: incident(incidentID) });
    });
    const w = mount(IncidentDetailView, { global: { stubs: { RouterLink: { props: ["to"], template: "<a><slot /></a>" } } } });
    mounted.push(w);
    await flushPromises();
    expect(apiMock.GET).toHaveBeenCalledWith("/api/v1/incidents/{incidentID}/phone-activity", { params: { path: { incidentID: "inc-a" } } });
    expect(panel(w).exists(), "A's read is still out").toBe(false);

    const { reactive } = await import("vue");
    reactive(routeRaw).params.id = "inc-b";
    await flushPromises();
    expect(w.text()).toContain("incident inc-b");
    expect(norm(w.get('[data-testid="incident-phone-empty"]').text())).toBe("No phone alert was sent for this incident.");

    phoneA.resolve({ data: { deliveries: [delivery({ id: "late-a", channel_name: "SMS — for A only" })] } });
    await flushPromises();
    expect(rows(w), "A's delivery is not drawn on B's page").toHaveLength(0);
    expect(w.text()).not.toContain("SMS — for A only");
    expect(norm(w.get('[data-testid="incident-phone-empty"]').text())).toBe("No phone alert was sent for this incident.");
  });
});
