import { mount } from "@vue/test-utils";
import { describe, expect, it } from "vitest";

import ChannelRouteChip from "@/components/settings/ChannelRouteChip.vue";

// FR-041a §4.3, D31, mock 3e / 3e′ / 3e‴ (and the 3g note): the chip under a phone channel's name.
// A disabled reason is a SOLID chip (the toggle is locked); "Not a route" is a DASHED chip (the
// channel keeps its own switch). `deliverable.reason: disabled` has no chip at all (§14 point 9).
// Names come from `recipient_display_name` (§14 point 1).

const base = {
  id: "nc",
  project_id: "p1",
  config: {},
  enabled: true,
  recipient: "user" as const,
  recipient_user_id: "u-ada",
  recipient_display_name: "Ada Park",
};

type Row = Record<string, unknown>;

function chip(channel: Row, selfUserId = "u-admin") {
  const w = mount(ChannelRouteChip, { props: { channel: { ...base, ...channel } as never, selfUserId } });
  return { w, el: w.find('[data-testid="channel-route-chip"]') };
}

const norm = (s: string) => s.replace(/\s+/g, " ").trim();

describe("ChannelRouteChip — 3e (project admin)", () => {
  it.each([
    ["SMS — Ada, a route", { type: "sms", deliverable: { route: true } }],
    ["Call — Ada, a route", { type: "voice", deliverable: { route: true } }],
    ["oncall-slack, a text channel", { type: "slack", recipient: undefined, deliverable: undefined }],
    ["a phone channel switched off by its own toggle (deliverable.reason: disabled)", { type: "sms", enabled: false, deliverable: { route: false, reason: "disabled" } }],
  ])("%s: no chip", (_l, ch) => {
    expect(chip(ch).el.exists()).toBe(false);
  });

  it.each([
    [
      "Call — Bo",
      { type: "voice", recipient_user_id: "u-bo", recipient_display_name: "Bo Lindqvist", deliverable: { route: false, reason: "no_verified_number" } },
      "Not a route · Bo Lindqvist has no verified number",
    ],
    [
      "SMS — Eli",
      { type: "sms", recipient_user_id: "u-eli", recipient_display_name: "Eli Moreau", deliverable: { route: false, reason: "recipient_not_eligible" } },
      "Not a route · Eli Moreau can no longer open this project",
    ],
  ])("%s: the dashed “Not a route” chip with the recipient's name", (_l, ch, want) => {
    const { el } = chip(ch);
    expect(norm(el.text())).toBe(want);
    expect(el.attributes("data-kind")).toBe("notRoute");
    expect(el.classes()).toContain("border-dashed");
    expect(el.find("b").text()).toBe("Not a route");
  });

  it.each([
    [
      "NOC duty phone",
      {
        type: "sms",
        enabled: false,
        recipient: "number",
        recipient_user_id: undefined,
        recipient_display_name: undefined,
        number_masked: "+44•••••0199",
        number_verified: false,
        disabled_reason: "number_unverified",
        deliverable: { route: false, reason: "disabled" },
      },
      "Disabled · number not confirmed",
    ],
    [
      "SMS — Fay",
      {
        type: "sms",
        enabled: false,
        recipient_user_id: undefined,
        recipient_display_name: null,
        disabled_reason: "recipient_deleted",
        deliverable: { route: false, reason: "disabled" },
      },
      "Disabled · the recipient's account was deleted",
    ],
  ])("%s: the solid disabled-reason chip", (_l, ch, want) => {
    const { el } = chip(ch);
    expect(norm(el.text())).toBe(want);
    expect(el.attributes("data-kind")).toBe("disabled");
    expect(el.classes()).not.toContain("border-dashed");
    expect(el.find("b").text()).toBe("Disabled");
  });
});

describe("ChannelRouteChip — 3e′ (instance-wide reasons)", () => {
  it.each([
    ["phone alerts off, sms", { type: "sms", deliverable: { route: false, reason: "telephony_disabled" } }, "Not a route · phone alerts are off on this instance"],
    ["phone alerts off, voice", { type: "voice", deliverable: { route: false, reason: "telephony_disabled" } }, "Not a route · phone alerts are off on this instance"],
    ["generic HTTP, voice", { type: "voice", deliverable: { route: false, reason: "voice_unsupported_by_provider" } }, "Not a route · the instance's provider cannot place calls"],
  ])("%s", (_l, ch, want) => {
    const { el } = chip(ch);
    expect(norm(el.text())).toBe(want);
    expect(el.classes()).toContain("border-dashed");
  });
});

describe("ChannelRouteChip — 3e‴ (the editor's own channel)", () => {
  const own = {
    type: "voice",
    recipient_user_id: "u-bo",
    recipient_display_name: "Bo Lindqvist",
    deliverable: { route: false, reason: "no_verified_number" },
  };

  it("says “you” and offers “add one”, which asks for Account → Phone", async () => {
    const { w, el } = chip(own, "u-bo");
    expect(norm(el.text())).toBe("Not a route · you have no verified number — add one");
    const link = el.find('[data-testid="channel-route-add-number"]');
    expect(link.exists()).toBe(true);
    expect(link.text()).toBe("add one");
    await link.trigger("click");
    expect(w.emitted("add-number")).toHaveLength(1);
  });

  it("offers no link on someone else's channel", () => {
    const { el } = chip(own, "u-admin");
    expect(norm(el.text())).toBe("Not a route · Bo Lindqvist has no verified number");
    expect(el.find('[data-testid="channel-route-add-number"]').exists()).toBe(false);
  });
});
