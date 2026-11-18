import { type VueWrapper, flushPromises, mount } from "@vue/test-utils";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import ChannelDeliveriesPanel from "@/components/settings/ChannelDeliveriesPanel.vue";
import type { PhoneDelivery } from "@/lib/phone";

// FR-041a §4.5, mock 3g: a channel's last 50 phone deliveries, opened from its row. Rows come in
// the API's order (NEWEST first) and are never re-sorted. What each row shows follows the owner's
// rulings (§14 points 2–4): the subject, state and step of the alert; one label for a monitor's
// recovery; the provider's HTTP status, code and attempts — never "timeout" or "retrying"; a storm
// limit named from `notes`, never its value; masked numbers only.

const apiMock = vi.hoisted(() => ({ GET: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));

// The instants are built from the RUNNER's local calendar, so "today" and "yesterday" are the same
// rows in every zone the suite runs in (review I-1: built in UTC, the 17th's afternoon is already the
// 18th at UTC+09). The product call passes no zone; the panel speaks the viewer's.
const local = (day: number, hms: string) => {
  const [h, m, s] = hms.split(":").map(Number);
  return new Date(2026, 10, day, h, m, s);
};
const NOW = local(18, "12:00:00");
const TODAY = (hms: string) => local(18, hms).toISOString();
const YESTERDAY = (hms: string) => local(17, hms).toISOString();

function delivery(over: Partial<PhoneDelivery>): PhoneDelivery {
  return {
    id: "d",
    channel_id: "nc-ada",
    channel_name: "SMS — Ada",
    kind: "sms",
    alert_kind: "monitor_down",
    number_masked: "+90•••••6869",
    reason: "sent",
    notes: [],
    provider_status: 201,
    provider_code: null,
    provider_ref: null,
    segments: 1,
    attempts: 1,
    created_at: TODAY("11:07:12"),
    updated_at: TODAY("11:07:13"),
    subject: { kind: "monitor", name: "payments-api" },
    service_state: null,
    escalation_step: null,
    recipient_display_name: "Ada Park",
    ...over,
  };
}

// The five rows of mock 3g, newest first as the API returns them.
const MOCK_3G: PhoneDelivery[] = [
  delivery({ id: "d1", incident_id: "inc-1", incident_ref: "X7K2P9QM", created_at: TODAY("11:07:12") }),
  delivery({
    id: "d2",
    incident_id: "inc-2",
    incident_ref: "7MQ4H2ZD",
    alert_kind: "service_health",
    subject: { kind: "service", name: "checkout" },
    service_state: "down",
    created_at: TODAY("11:06:58"),
  }),
  delivery({
    id: "d3",
    incident_id: "inc-3",
    incident_ref: "4RD8N1KC",
    reason: "storm_limited",
    notes: ["storm_per_incident"],
    provider_status: null,
    segments: 0,
    attempts: 0,
    subject: { kind: "monitor", name: "api-gateway" },
    created_at: TODAY("06:41:03"),
  }),
  delivery({ id: "d4", alert_kind: "monitor_burn", created_at: YESTERDAY("15:12:40") }),
  delivery({
    id: "d5",
    incident_id: "inc-5",
    incident_ref: "Q2W9V5TB",
    reason: "not_deliverable",
    alert_kind: "monitor_recovered",
    notes: ["no_verified_number"],
    number_masked: null,
    provider_status: null,
    segments: 0,
    attempts: 0,
    created_at: YESTERDAY("14:58:17"),
  }),
];

const mounted: VueWrapper[] = [];
async function mountPanel(answer: unknown) {
  apiMock.GET.mockReset();
  apiMock.GET.mockResolvedValue(answer);
  const w = mount(ChannelDeliveriesPanel, {
    props: { channelId: "nc-ada", channelName: "SMS — Ada" },
    global: {
      stubs: { RouterLink: { props: ["to"], template: `<a :data-to="JSON.stringify(to)"><slot /></a>` } },
    },
  });
  mounted.push(w);
  await flushPromises();
  return w;
}
const ok = (deliveries: PhoneDelivery[]) => ({ data: { deliveries }, error: undefined, response: { ok: true, status: 200 } });

const norm = (s: string) => s.replace(/\s+/g, " ").trim();
const rows = (w: VueWrapper) => w.findAll('[data-testid="delivery-row"]');
const part = (row: ReturnType<typeof rows>[number], id: string) => norm(row.get(`[data-testid="${id}"]`).text());
/** The second line's spans, one by one (the mock separates them by space, not by a character). */
const detail = (row: ReturnType<typeof rows>[number]) =>
  row
    .findAll('[data-testid="delivery-detail"] > *')
    .map((e) => norm(e.text()))
    .join(" ");

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(NOW);
});
afterEach(() => {
  while (mounted.length) mounted.pop()!.unmount();
  vi.useRealTimers();
});

describe("ChannelDeliveriesPanel — mock 3g", () => {
  it("reads the channel's deliveries from its own endpoint", async () => {
    await mountPanel(ok([]));
    expect(apiMock.GET).toHaveBeenCalledWith("/api/v1/notification-channels/{channelID}/phone-deliveries", {
      params: { path: { channelID: "nc-ada" } },
    });
  });

  it("has the header “Deliveries · <channel>”, the “last 50” chip, and “✕ Close”, which emits close", async () => {
    const w = await mountPanel(ok(MOCK_3G));
    expect(norm(w.get('[data-testid="deliveries-title"]').text())).toBe("Deliveries · SMS — Ada");
    expect(norm(w.get('[data-testid="deliveries-limit"]').text())).toBe("last 50");
    const close = w.get('[data-testid="deliveries-close"]');
    expect(norm(close.text())).toBe("✕ Close");
    await close.trigger("click");
    expect(w.emitted("close")).toHaveLength(1);
  });

  it("keeps the API's order — newest first — and never re-sorts", async () => {
    const shuffled = [MOCK_3G[3], MOCK_3G[0], MOCK_3G[4]];
    const w = await mountPanel(ok(shuffled));
    expect(rows(w).map((r) => r.attributes("data-id"))).toEqual(["d4", "d1", "d5"]);
  });

  it("renders the five 3g rows: chip, label, ref or “no incident”, mask, and second line", async () => {
    const w = await mountPanel(ok(MOCK_3G));
    const r = rows(w);
    expect(r).toHaveLength(5);

    const want = [
      { chip: "Sent", tone: "text-up", label: "Monitor down · payments-api", ref: "X7K2P9QM", mask: "+90•••••6869", l2: "HTTP 201 1 segment 1 attempt" },
      { chip: "Sent", tone: "text-up", label: "Service alert · checkout down", ref: "7MQ4H2ZD", mask: "+90•••••6869", l2: "HTTP 201 1 segment 1 attempt" },
      {
        chip: "Storm limit",
        tone: "text-degraded",
        label: "Monitor down · api-gateway",
        ref: "4RD8N1KC",
        mask: "+90•••••6869",
        l2: "too many messages to this number for this incident in the last hour",
      },
      { chip: "Sent", tone: "text-up", label: "Budget burn · payments-api", ref: null, mask: "+90•••••6869", l2: "HTTP 201 1 segment 1 attempt" },
      {
        chip: "Not deliverable",
        tone: "text-ink-3",
        label: "Monitor recovery · payments-api",
        ref: "Q2W9V5TB",
        mask: null,
        l2: "no_verified_number Ada Park had no verified number when it was sent",
      },
    ];
    want.forEach((x, i) => {
      const row = r[i];
      expect(part(row, "delivery-reason"), `row ${i + 1} chip`).toBe(x.chip);
      expect(row.get('[data-testid="delivery-reason"]').classes(), `row ${i + 1} tone`).toContain(x.tone);
      expect(part(row, "delivery-alert"), `row ${i + 1} label`).toBe(x.label);
      if (x.ref) {
        expect(part(row, "delivery-ref")).toBe(x.ref);
        expect(row.get('[data-testid="delivery-ref"]').classes()).toContain("font-mono");
        expect(row.find('[data-testid="delivery-no-incident"]').exists()).toBe(false);
      } else {
        expect(row.find('[data-testid="delivery-ref"]').exists()).toBe(false);
        expect(part(row, "delivery-no-incident")).toBe("no incident");
      }
      if (x.mask) expect(part(row, "delivery-mask")).toBe(x.mask);
      else expect(row.find('[data-testid="delivery-mask"]').exists(), `row ${i + 1} has no number`).toBe(false);
      expect(detail(row), `row ${i + 1} second line`).toBe(x.l2);
    });

    // The ref opens the incident.
    expect(JSON.parse(r[0].get('[data-testid="delivery-ref"]').attributes("data-to")!)).toEqual({
      name: "incident",
      params: { id: "inc-1" },
    });
    // The cause of `not_deliverable` is the small mono chip.
    expect(r[4].get('[data-testid="delivery-note"]').text()).toBe("no_verified_number");
  });

  it("puts the clock over today / yesterday, with the offset of the instant", async () => {
    const w = await mountPanel(ok(MOCK_3G));
    const r = rows(w);
    for (const [i, clock, day] of [
      [0, "11:07:12", "today"],
      [2, "06:41:03", "today"],
      [3, "15:12:40", "yesterday"],
      [4, "14:58:17", "yesterday"],
    ] as const) {
      expect(part(r[i], "delivery-clock"), `row ${i + 1}`).toBe(clock);
      expect(part(r[i], "delivery-day"), `row ${i + 1}`).toBe(day);
      expect(part(r[i], "delivery-offset"), `row ${i + 1}: the offset in force then`).toMatch(/^UTC[+-]\d\d:\d\d$/);
    }
  });

  it("names a storm limit from notes without a number, and an unreachable provider without “timeout” or “retrying”", async () => {
    const w = await mountPanel(
      ok([
        delivery({ id: "s1", reason: "storm_limited", notes: ["storm_per_number"], provider_status: null, attempts: 0, segments: 0 }),
        delivery({ id: "u1", reason: "provider_unreachable", provider_status: 503, provider_code: "30008", attempts: 3, segments: 0 }),
      ]),
    );
    const [storm, unreachable] = rows(w);
    expect(detail(storm)).toBe("too many messages to this number in the last hour");
    expect(detail(storm)).not.toMatch(/\d/);
    expect(part(unreachable, "delivery-reason")).toBe("Provider unreachable");
    expect(detail(unreachable)).toBe("HTTP 503 · code 30008 3 attempts");
    expect(norm(w.text())).not.toMatch(/timeout|retrying|within 5 s/i);
  });

  it("names a service's recovery as the mock's message table does, and a monitor's as “Monitor recovery”", async () => {
    const w = await mountPanel(
      ok([
        delivery({ id: "r1", alert_kind: "service_recovered", subject: { kind: "service", name: "checkout" } }),
        delivery({ id: "r2", alert_kind: "monitor_recovered" }),
        delivery({ id: "r3", alert_kind: "escalation_step", escalation_step: 2, subject: null }),
      ]),
    );
    expect(rows(w).map((r) => part(r, "delivery-alert"))).toEqual([
      "Service alert · checkout recovered",
      "Monitor recovery · payments-api",
      "Escalation step 2",
    ]);
  });

  it("shows a known link note with its words, and an unknown note as its raw code alone", async () => {
    const w = await mountPanel(ok([delivery({ id: "n1", notes: ["link_omitted", "a_future_note"] })]));
    const notes = rows(w)[0].findAll('[data-testid="delivery-note"]');
    expect(notes.map((n) => n.text())).toEqual(["link_omitted", "a_future_note"]);
    const labels = rows(w)[0].findAll('[data-testid="delivery-note-label"]');
    expect(labels.map((n) => norm(n.text()))).toEqual(["link dropped to fit the message"]);
  });

  it("shows only masked numbers — a full number that reaches the page anyway is never printed", async () => {
    // The API sends no full number; this one is planted where a careless row would pick it up.
    const FULL = "+905551236869";
    const leaky = MOCK_3G.map((d) => ({ ...d, channel_name: FULL, number: FULL }) as PhoneDelivery);
    const w = await mountPanel(ok(leaky));
    const UNMASKED = /\d{7,}/; // a mask keeps at most 4 digits in a row; no other text here has 7
    expect(FULL, "the pattern catches the planted number").toMatch(UNMASKED);
    expect(w.text()).not.toMatch(UNMASKED);
    expect(rows(w)[0].get('[data-testid="delivery-mask"]').text()).toBe("+90•••••6869");
  });

  it("collapses a row to one column on a narrow screen, as the mock's .pr does under 560px", async () => {
    const w = await mountPanel(ok(MOCK_3G));
    expect(rows(w)[0].classes()).toContain("max-[560px]:grid-cols-1");
  });

  it("closes with the 90-day footnote", async () => {
    const w = await mountPanel(ok(MOCK_3G));
    expect(norm(w.get('[data-testid="deliveries-footnote"]').text())).toBe(
      "Older deliveries are kept 90 days and are not listed here.",
    );
  });

  it("empty: one sentence, no footnote", async () => {
    const w = await mountPanel(ok([]));
    expect(norm(w.get('[data-testid="deliveries-empty"]').text())).toBe("No alert has been sent through this channel yet.");
    expect(rows(w)).toHaveLength(0);
    expect(w.find('[data-testid="deliveries-footnote"]').exists()).toBe(false);
  });

  it("a failed read says what it is not, dashed — never the empty sentence", async () => {
    const w = await mountPanel({ data: undefined, error: { error: "boom" }, response: { ok: false, status: 500 } });
    const el = w.get('[data-testid="deliveries-unavailable"]');
    expect(norm(el.text())).toBe("Deliveries could not be read — this is not “nothing was sent”.");
    expect(el.classes()).toContain("border-dashed");
    expect(w.find('[data-testid="deliveries-empty"]').exists()).toBe(false);
  });

  it("a request that throws reads the same as a failed read", async () => {
    apiMock.GET.mockReset();
    apiMock.GET.mockRejectedValue(new Error("network"));
    const w = mount(ChannelDeliveriesPanel, { props: { channelId: "nc-ada", channelName: "SMS — Ada" } });
    mounted.push(w);
    await flushPromises();
    expect(w.find('[data-testid="deliveries-unavailable"]').exists()).toBe(true);
  });
});
