import { describe, expect, it } from "vitest";

import {
  type RouteChip,
  ALERT_KIND_LABEL,
  CHANNEL_TYPE_LABEL,
  DELIVERABLE_REASONS,
  DELIVERY_REASONS,
  DELIVERY_TONE_CLASS,
  NOTE_LABEL,
  PHONE_NOTES,
  TRANSPORT_CLASS_LABEL,
  VOICE_LANGUAGES,
  alertKindLabel,
  alertSummary,
  canHoldPhoneChannel,
  channelRecipientLabel,
  codeAnswer,
  CODE_SEND_LIMITED,
  deliveryDetail,
  deliveryNotes,
  deliveryReasonLabel,
  deliveryReasonTone,
  deliverySecondLine,
  isPhoneChannel,
  kindLabel,
  normalizeIncidentRef,
  parseHeaderLines,
  phoneErrorMessage,
  phoneErrorReason,
  routeChip,
  shortProviderRef,
  testSmsMessage,
  type PhoneDelivery,
  type PhoneDeliveryReason,
  type RouteChannel,
} from "@/lib/phone";

// FR-041a SPA Task 1 — the vocabulary every phone surface shares (mock 1i, 3e–3g, 4a–4b, 5d), with
// the owner's rulings of spec revision 7/8 §14: a delivery row never says "timeout", "5 s" or
// "retrying"; storm_limited names the limit from notes, never its value; one label for a monitor's
// recovery; names come from recipient_display_name and a missing name reads neutrally.

function delivery(over: Partial<PhoneDelivery> = {}): PhoneDelivery {
  return {
    id: "d1",
    channel_id: "c1",
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
    created_at: "2026-11-18T14:06:58Z",
    updated_at: "2026-11-18T14:06:58Z",
    subject: null,
    service_state: null,
    escalation_step: null,
    recipient_display_name: null,
    ...over,
  };
}

const FORBIDDEN = [/timeout/i, /5\s*s\b/, /retrying/i, /within/i];

describe("delivery reasons — the closed set of §4.5 part (a)", () => {
  const table: [PhoneDeliveryReason, string, string][] = [
    ["sending", "Sending", "pending"],
    ["sent", "Sent", "ok"],
    ["provider_unreachable", "Provider unreachable", "warn"],
    ["provider_rejected", "Rejected by provider", "bad"],
    ["outcome_unknown", "Outcome unknown", "warn"],
    ["not_deliverable", "Not deliverable", "config"],
    ["voice_unsupported_by_provider", "Calls not supported", "config"],
    ["telephony_disabled", "Phone alerts off", "config"],
    ["storm_limited", "Storm limit", "warn"],
  ];

  // An independent copy of the closed set — Go's `phoneReasons` (internal/domain/phone.go) and the
  // schema enum of PhoneDelivery.reason; DELIVERY_REASONS is a literal tuple in phone.ts that vue-tsc
  // holds equal to the schema enum, so a reason added to openapi breaks the type-check and this test.
  const CLOSED_SET = [
    "sending",
    "sent",
    "provider_unreachable",
    "provider_rejected",
    "outcome_unknown",
    "not_deliverable",
    "voice_unsupported_by_provider",
    "telephony_disabled",
    "storm_limited",
  ];

  it("covers exactly the nine reasons of the closed set", () => {
    expect([...DELIVERY_REASONS].sort()).toEqual([...CLOSED_SET].sort());
    expect(table.map((r) => r[0]).sort()).toEqual([...CLOSED_SET].sort());
  });

  it.each(table)("%s has its label, tone, a tone class and a non-empty second line", (reason, label, tone) => {
    expect(deliveryReasonLabel(reason)).toBe(label);
    expect(deliveryReasonTone(reason)).toBe(tone);
    expect(DELIVERY_TONE_CLASS[deliveryReasonTone(reason)]).toBeTruthy();
    const line = deliverySecondLine(delivery({ reason, provider_status: reason === "sent" ? 201 : null }));
    expect(line).not.toBe("");
    for (const re of FORBIDDEN) expect(line, `${reason}: ${line}`).not.toMatch(re);
  });

  it("an in-flight row is dashed, a sent row green, a rejected row red", () => {
    expect(DELIVERY_TONE_CLASS.pending).toContain("border-dashed");
    expect(DELIVERY_TONE_CLASS.ok).toContain("text-up");
    expect(DELIVERY_TONE_CLASS.bad).toContain("text-down");
    expect(DELIVERY_TONE_CLASS.warn).toContain("text-degraded");
    expect(DELIVERY_TONE_CLASS.config).not.toContain("border-dashed");
  });
});

describe("deliverySecondLine — mock 4b, with revision 7/8's rulings", () => {
  it("sent: status, segments, attempts and a shortened reference", () => {
    const d = delivery({ provider_status: 201, segments: 1, attempts: 1, provider_ref: "SM3f00000000000000000000000000a91c" });
    expect(deliverySecondLine(d)).toBe("HTTP 201 · 1 segment · 1 attempt · ref SM3f…a91c");
    const parts = deliveryDetail(d).parts;
    expect(parts.map((p) => p.style)).toEqual(["mono", "plain", "plain", "mono"]);
  });

  it("plural segments and attempts", () => {
    expect(deliverySecondLine(delivery({ segments: 2, attempts: 3 }))).toBe("HTTP 201 · 2 segments · 3 attempts");
  });

  it("a voice row never shows segments", () => {
    expect(deliverySecondLine(delivery({ kind: "voice", segments: 1, attempts: 1 }))).toBe("HTTP 201 · 1 attempt");
  });

  it("a reference of 10 characters or fewer is shown whole", () => {
    expect(shortProviderRef("SM12345678")).toBe("SM12345678");
    expect(shortProviderRef("SM123456789")).toBe("SM12…6789");
  });

  it("provider_rejected: status and code; status alone without a code; a 3xx is a redirect not followed", () => {
    expect(deliverySecondLine(delivery({ reason: "provider_rejected", provider_status: 400, provider_code: "21215" }))).toBe(
      "HTTP 400 · code 21215 · 1 attempt",
    );
    expect(deliveryDetail(delivery({ reason: "provider_rejected", provider_status: 400, provider_code: "21215" })).parts[0]).toEqual({
      text: "HTTP 400 · code 21215",
      style: "mono",
    });
    expect(deliverySecondLine(delivery({ reason: "provider_rejected", provider_status: 400 }))).toBe("HTTP 400 · 1 attempt");
    expect(deliverySecondLine(delivery({ reason: "provider_rejected", provider_status: 302 }))).toBe(
      "HTTP 302 · redirect not followed · 1 attempt",
    );
  });

  it("provider_unreachable: the status and the attempts — never 'retrying' or a transport class", () => {
    const line = deliverySecondLine(delivery({ reason: "provider_unreachable", provider_status: 503, attempts: 3 }));
    expect(line).toBe("HTTP 503 · 3 attempts");
    expect(deliverySecondLine(delivery({ reason: "provider_unreachable", provider_status: null, attempts: 2 }))).toBe("2 attempts");
    expect(deliverySecondLine(delivery({ reason: "provider_unreachable", provider_status: 429, provider_code: "20429", attempts: 1 }))).toBe(
      "HTTP 429 · code 20429 · 1 attempt",
    );
  });

  it("outcome_unknown: attempts, then the fixed sentence as the why", () => {
    const det = deliveryDetail(delivery({ reason: "outcome_unknown", provider_status: null, attempts: 1 }));
    expect(det.parts.map((p) => p.text)).toEqual(["1 attempt"]);
    expect(det.why).toBe(
      "The process sending it stopped before the provider's answer was stored. It may have arrived; cerbix did not send it again, so it is not counted as delivered.",
    );
  });

  it("not_deliverable names the person from recipient_display_name, and is neutral without one", () => {
    const named = delivery({ reason: "not_deliverable", provider_status: null, attempts: 0, number_masked: null, notes: ["no_verified_number"], recipient_display_name: "Ada Park" });
    expect(deliveryDetail(named).parts).toEqual([
      { text: "no_verified_number", style: "note" },
      { text: "Ada Park had no verified number when it was sent", style: "plain" },
    ]);
    const elig = delivery({ reason: "not_deliverable", provider_status: null, attempts: 0, notes: ["recipient_not_eligible"], recipient_display_name: "Eli Moreau" });
    expect(deliverySecondLine(elig)).toBe("recipient_not_eligible · Eli Moreau could no longer open this project when it was sent");
    const anon = delivery({ reason: "not_deliverable", provider_status: null, attempts: 0, notes: ["no_verified_number"], recipient_display_name: null });
    expect(deliverySecondLine(anon)).toBe("no_verified_number · The recipient had no verified number when it was sent");
    const anonElig = delivery({ reason: "not_deliverable", provider_status: null, attempts: 0, notes: ["recipient_not_eligible"] });
    expect(deliverySecondLine(anonElig)).toBe("recipient_not_eligible · The recipient could no longer open this project when it was sent");
  });

  it("voice_unsupported_by_provider does not blame the generic HTTP gateway alone", () => {
    const line = deliverySecondLine(delivery({ reason: "voice_unsupported_by_provider", kind: "voice", provider_status: null, attempts: 0 }));
    expect(line).toBe("The instance's provider cannot place calls.");
  });

  it("telephony_disabled", () => {
    expect(deliverySecondLine(delivery({ reason: "telephony_disabled", provider_status: null, attempts: 0 }))).toBe(
      "Phone alerts were switched off on this instance.",
    );
  });

  it("storm_limited names the limit from notes and never a number", () => {
    const inc = deliverySecondLine(delivery({ reason: "storm_limited", provider_status: null, attempts: 0, notes: ["storm_per_incident"] }));
    expect(inc).toBe("too many messages to this number for this incident in the last hour");
    const num = deliverySecondLine(delivery({ reason: "storm_limited", provider_status: null, attempts: 0, notes: ["storm_per_number"] }));
    expect(num).toBe("too many messages to this number in the last hour");
    for (const s of [inc, num]) expect(s).not.toMatch(/\d/);
  });

  it("sending", () => {
    expect(deliverySecondLine(delivery({ reason: "sending", provider_status: null, attempts: 1 }))).toBe("waiting for the provider's answer");
  });
});

describe("notes", () => {
  it("every link note maps to its sentence", () => {
    expect(NOTE_LABEL).toEqual({
      no_incident_ref: "sent before the incident opened — no link",
      no_public_url: "sent without a link",
      link_omitted: "link dropped to fit the message",
    });
  });

  it("deliveryNotes returns the link notes in order with their words, an unknown note raw; causes and storm limits are the second line's", () => {
    const d = delivery({ notes: ["link_omitted", "no_verified_number", "storm_per_number", "no_incident_ref", "something_new"] });
    expect(deliveryNotes(d)).toEqual([
      { code: "link_omitted", label: "link dropped to fit the message" },
      { code: "no_incident_ref", label: "sent before the incident opened — no link" },
      { code: "something_new", label: "" },
    ]);
  });

  // The closed set of notes lives only in Go (`PhoneNote*`, internal/domain/phone.go): the schema
  // types `notes` as string[]. Every one must be either a link note with words or consumed by the
  // second line of the reason that carries it — none may vanish.
  const GO_NOTES = [
    "no_incident_ref",
    "no_public_url",
    "link_omitted",
    "no_verified_number",
    "recipient_not_eligible",
    "storm_per_incident",
    "storm_per_number",
  ];

  it("PHONE_NOTES is Go's closed set of notes", () => {
    expect([...PHONE_NOTES].sort()).toEqual([...GO_NOTES].sort());
  });

  it.each(GO_NOTES)("note %s is shown — as a link note, or by the second line of its reason", (note) => {
    if (Object.hasOwn(NOTE_LABEL, note)) {
      expect(deliveryNotes(delivery({ notes: [note] }))).toEqual([{ code: note, label: NOTE_LABEL[note as keyof typeof NOTE_LABEL] }]);
      return;
    }
    expect(deliveryNotes(delivery({ notes: [note] })), "consumed notes are not repeated as chips").toEqual([]);
    const reason: PhoneDeliveryReason = note.startsWith("storm_") ? "storm_limited" : "not_deliverable";
    const without = deliverySecondLine(delivery({ reason, provider_status: null, attempts: 0, notes: [] }));
    const withNote = deliverySecondLine(delivery({ reason, provider_status: null, attempts: 0, notes: [note] }));
    expect(withNote, note).not.toBe(without);
  });
});

describe("labels", () => {
  it("channel types, kinds and phone predicate", () => {
    expect(CHANNEL_TYPE_LABEL).toEqual({ sms: "SMS", voice: "Voice call" });
    expect(kindLabel("sms")).toBe("SMS");
    expect(kindLabel("voice")).toBe("Call");
    expect(isPhoneChannel("sms")).toBe(true);
    expect(isPhoneChannel("voice")).toBe(true);
    expect(isPhoneChannel("slack")).toBe(false);
    expect(isPhoneChannel(undefined)).toBe(false);
  });

  it("alert kinds: seven labels, one for a monitor's recovery, the raw string for an unknown kind", () => {
    expect(Object.keys(ALERT_KIND_LABEL).sort()).toEqual(
      ["escalation_step", "monitor_burn", "monitor_down", "monitor_recovered", "service_burn", "service_health", "service_recovered"].sort(),
    );
    expect(alertKindLabel("monitor_recovered")).toBe("Monitor recovery");
    expect(alertKindLabel("monitor_down")).toBe("Monitor down");
    expect(alertKindLabel("service_health")).toBe("Service alert");
    expect(alertKindLabel("escalation_step")).toBe("Escalation step");
    expect(alertKindLabel("service_recovered"), "a service's recovery reads as the mock's message table").toBe("Service alert");
    expect(alertKindLabel("monitor_burn")).toBe("Budget burn");
    expect(alertKindLabel("a_kind_from_the_future")).toBe("a_kind_from_the_future");
    expect(alertKindLabel("")).toBe("");
  });

  it("alertSummary composes subject, state and step", () => {
    const mon = { kind: "monitor" as const, name: "payments-api" };
    const svc = { kind: "service" as const, name: "checkout" };
    expect(alertSummary(delivery({ alert_kind: "monitor_down", subject: mon }))).toBe("Monitor down · payments-api");
    expect(alertSummary(delivery({ alert_kind: "monitor_down", subject: mon }), { withSubject: false })).toBe("Monitor down");
    expect(alertSummary(delivery({ alert_kind: "service_health", subject: svc, service_state: "down" }))).toBe("Service alert · checkout down");
    expect(alertSummary(delivery({ alert_kind: "service_health", subject: svc, service_state: "down" }), { withSubject: false })).toBe(
      "Service alert · down",
    );
    expect(alertSummary(delivery({ alert_kind: "monitor_recovered", subject: mon }))).toBe("Monitor recovery · payments-api");
    expect(alertSummary(delivery({ alert_kind: "monitor_recovered", subject: mon }), { withSubject: false })).toBe("Monitor recovery");
    expect(alertSummary(delivery({ alert_kind: "service_recovered", subject: svc, service_state: null }))).toBe(
      "Service alert · checkout recovered",
    );
    expect(alertSummary(delivery({ alert_kind: "service_recovered", subject: svc, service_state: null }), { withSubject: false })).toBe(
      "Service alert · recovered",
    );
    expect(alertSummary(delivery({ alert_kind: "service_recovered", subject: null, service_state: null }))).toBe("Service alert · recovered");
    expect(alertSummary(delivery({ alert_kind: "monitor_burn", subject: mon }))).toBe("Budget burn · payments-api");
    expect(alertSummary(delivery({ alert_kind: "escalation_step", escalation_step: 2 }))).toBe("Escalation step 2");
    expect(alertSummary(delivery({ alert_kind: "escalation_step", escalation_step: 2, subject: mon }))).toBe("Escalation step 2 · payments-api");
    expect(alertSummary(delivery({ alert_kind: "monitor_down", subject: null }))).toBe("Monitor down");
    expect(alertSummary(delivery({ alert_kind: "weird", subject: mon }))).toBe("weird · payments-api");
  });
});

describe("routeChip — mock 3e, 3e′, 3e‴", () => {
  const base: RouteChannel = {
    type: "sms",
    recipient: "user",
    recipient_user_id: "u-bo",
    recipient_display_name: "Bo Lindqvist",
    deliverable: { route: true },
  };

  it("a route, or a non-phone channel, has no chip", () => {
    expect(routeChip(base, { selfUserId: "u-ada" })).toEqual({ kind: "none" });
    expect(routeChip({ ...base, type: "slack", deliverable: { route: false, reason: "telephony_disabled" } }, { selfUserId: null })).toEqual({
      kind: "none",
    });
  });

  it("disabled_reason wins over deliverable, with the tooltip that unlocks the toggle", () => {
    expect(
      routeChip(
        { ...base, recipient: "number", recipient_user_id: undefined, disabled_reason: "number_unverified", deliverable: { route: false, reason: "telephony_disabled" } },
        { selfUserId: null },
      ),
    ).toEqual({ kind: "disabled", text: "Disabled · number not confirmed", unlock: "Confirm the number to enable this channel" });
    expect(
      routeChip({ ...base, recipient_display_name: null, disabled_reason: "recipient_deleted", deliverable: { route: false, reason: "recipient_deleted" } }, { selfUserId: null }),
    ).toEqual({
      kind: "disabled",
      text: "Disabled · the recipient's account was deleted",
      unlock: "The recipient was deleted — pick another person or delete the channel",
    });
  });

  it("the channel's own switch (reason disabled) is no chip", () => {
    expect(routeChip({ ...base, deliverable: { route: false, reason: "disabled" } }, { selfUserId: null })).toEqual({ kind: "none" });
  });

  it("not a route: instance-wide reasons", () => {
    expect(routeChip({ ...base, deliverable: { route: false, reason: "telephony_disabled" } }, { selfUserId: null })).toEqual({
      kind: "notRoute",
      text: "Not a route · phone alerts are off on this instance",
    });
    expect(routeChip({ ...base, type: "voice", deliverable: { route: false, reason: "voice_unsupported_by_provider" } }, { selfUserId: null })).toEqual({
      kind: "notRoute",
      text: "Not a route · the instance's provider cannot place calls",
    });
  });

  it("not a route: the recipient, by name for another person and as 'you' for oneself", () => {
    const nv = { ...base, deliverable: { route: false, reason: "no_verified_number" as const } };
    expect(routeChip(nv, { selfUserId: "u-ada" })).toEqual({ kind: "notRoute", text: "Not a route · Bo Lindqvist has no verified number" });
    expect(routeChip(nv, { selfUserId: "u-bo" })).toEqual({ kind: "notRoute", text: "Not a route · you have no verified number — add one" });
    expect(routeChip({ ...nv, recipient_display_name: null }, { selfUserId: "u-ada" })).toEqual({
      kind: "notRoute",
      text: "Not a route · the recipient has no verified number",
    });
    const ne = { ...base, recipient_display_name: "Eli Moreau", deliverable: { route: false, reason: "recipient_not_eligible" as const } };
    expect(routeChip(ne, { selfUserId: "u-ada" })).toEqual({ kind: "notRoute", text: "Not a route · Eli Moreau can no longer open this project" });
    expect(routeChip({ ...ne, recipient_display_name: null }, { selfUserId: "u-ada" })).toEqual({
      kind: "notRoute",
      text: "Not a route · the recipient can no longer open this project",
    });
  });
});

describe("routeChip — every deliverable.reason of the closed set", () => {
  // Independent copy of the schema enum ChannelDeliverable.reason (= Go's R11 set).
  const CLOSED: [string, RouteChip["kind"], string][] = [
    ["disabled", "none", ""],
    ["telephony_disabled", "notRoute", "Not a route · phone alerts are off on this instance"],
    ["voice_unsupported_by_provider", "notRoute", "Not a route · the instance's provider cannot place calls"],
    ["number_unverified", "notRoute", "Not a route · number not confirmed"],
    ["no_verified_number", "notRoute", "Not a route · Bo Lindqvist has no verified number"],
    ["recipient_not_eligible", "notRoute", "Not a route · Bo Lindqvist can no longer open this project"],
    ["recipient_deleted", "notRoute", "Not a route · the recipient's account was deleted"],
  ];

  it("DELIVERABLE_REASONS is the closed set", () => {
    expect([...DELIVERABLE_REASONS].sort()).toEqual(CLOSED.map((r) => r[0]).sort());
  });

  it.each(CLOSED)("%s → %s", (reason, kind, text) => {
    const chip = routeChip(
      { type: "sms", recipient: "user", recipient_user_id: "u-bo", recipient_display_name: "Bo Lindqvist", deliverable: { route: false, reason: reason as never } },
      { selfUserId: "u-ada" },
    );
    expect(chip.kind).toBe(kind);
    if (chip.kind === "notRoute") expect(chip.text).toBe(text);
  });
});

describe("normalizeIncidentRef — domain.NormalizeIncidentRef", () => {
  it("upper-cases and maps the aliases", () => {
    expect(normalizeIncidentRef("x7k2p9qm")).toBe("X7K2P9QM");
    expect(normalizeIncidentRef("ILOoil00")).toBe("11001100");
    expect(normalizeIncidentRef("X7K2P9QM")).toBe("X7K2P9QM");
  });

  it("refuses wrong length, U, and anything outside the alphabet", () => {
    expect(normalizeIncidentRef("X7K2P9Q")).toBeNull();
    expect(normalizeIncidentRef("X7K2P9QMA")).toBeNull();
    expect(normalizeIncidentRef("X7K2P9QU")).toBeNull();
    expect(normalizeIncidentRef("X7K2P9Q-")).toBeNull();
    expect(normalizeIncidentRef(" X7K2P9Q")).toBeNull();
    expect(normalizeIncidentRef("X7K2P9Qé")).toBeNull();
    // The cases of Go's own test: non-ASCII letters that upper-case INTO the alphabet — dotless ı
    // becomes I (→ 1), long ſ becomes S. Only the non-ASCII guard refuses them.
    expect(normalizeIncidentRef("ABCDEFGı")).toBeNull();
    expect(normalizeIncidentRef("ABCDEFGſ")).toBeNull();
    expect(normalizeIncidentRef("ıııııııı")).toBeNull();
    expect(normalizeIncidentRef("")).toBeNull();
  });
});

describe("parseHeaderLines", () => {
  it("parses Name: value per non-empty line, trimming values", () => {
    expect(parseHeaderLines("Authorization: Bearer abc\n\nX-Api-Key:  k1 \n")).toEqual({
      ok: true,
      headers: { Authorization: "Bearer abc", "X-Api-Key": "k1" },
    });
  });

  it("empty text is an empty map", () => {
    expect(parseHeaderLines("")).toEqual({ ok: true, headers: {} });
    expect(parseHeaderLines("  \n \n")).toEqual({ ok: true, headers: {} });
  });

  it("a bad line reports its 1-based number", () => {
    expect(parseHeaderLines("A: 1\n\nno colon here")).toEqual({ ok: false, line: 3 });
    expect(parseHeaderLines(": value")).toEqual({ ok: false, line: 1 });
    expect(parseHeaderLines("Bad Name: v")).toEqual({ ok: false, line: 1 });
  });

  it("a value may hold a colon", () => {
    expect(parseHeaderLines("X-Url: https://a.example:8443/x")).toEqual({ ok: true, headers: { "X-Url": "https://a.example:8443/x" } });
  });
});

describe("testSmsMessage — mock 1i, revision 8 §4.1", () => {
  it("accepted", () => {
    const m = testSmsMessage({ outcome: "sent", http_status: 201, provider_code: "" }, "twilio", "");
    expect(m.tone).toBe("ok");
    expect(m.text).toBe("✓ Accepted by the provider · HTTP 201. Check your phone.");
  });

  it("rejected with a code", () => {
    const m = testSmsMessage({ outcome: "rejected", http_status: 400, provider_code: "21211" }, "twilio", "");
    expect(m.tone).toBe("bad");
    expect(m.status).toBe("HTTP 400 · code 21211");
    expect(m.text).toBe("Rejected by the provider · HTTP 400 · code 21211. Check the From number and the account.");
  });

  it("rejected by a generic gateway with no error-code field", () => {
    const m = testSmsMessage({ outcome: "rejected", http_status: 400, provider_code: "" }, "generic_http", "");
    expect(m.text).toBe("Rejected by the gateway · HTTP 400. Set “Error code field” to see the gateway's own code.");
  });

  it("rejected by a generic gateway that has an error-code field", () => {
    const m = testSmsMessage({ outcome: "rejected", http_status: 400, provider_code: "E42" }, "generic_http", "error_code");
    expect(m.text).toBe("Rejected by the gateway · HTTP 400 · code E42. Check the From number and the account.");
  });

  it("a redirect", () => {
    const m = testSmsMessage({ outcome: "rejected", http_status: 302, provider_code: "" }, "generic_http", "");
    expect(m.text).toBe("Rejected · HTTP 302. A redirect is not followed; put the final URL in the settings.");
  });

  it("unreachable shows the transport class when present, never a budget", () => {
    const m = testSmsMessage({ outcome: "unreachable", http_status: 0, provider_code: "", transport_class: "timeout" }, "twilio", "");
    expect(m.tone).toBe("warn");
    expect(m.transportClass).toBe("timeout");
    expect(m.text).toBe("No answer from the provider (timeout). The message may or may not have been sent.");
    expect(m.text).not.toMatch(/5\s*s\b|within/);

    const egress = testSmsMessage(
      { outcome: "unreachable", http_status: 0, provider_code: "", transport_class: "destination refused by the delivery egress policy" },
      "generic_http",
      "",
    );
    expect(egress.transportClass).toBe(TRANSPORT_CLASS_LABEL["destination refused by the delivery egress policy"]);

    expect(testSmsMessage({ outcome: "unreachable", http_status: 0, provider_code: "" }, "twilio", "").text).toBe(
      "No answer from the provider. The message may or may not have been sent.",
    );
    expect(testSmsMessage({ outcome: "unreachable", http_status: 503, provider_code: "" }, "twilio", "").text).toBe(
      "No usable answer from the provider · HTTP 503. The message may or may not have been sent.",
    );
  });

  it("every transport class has a short label", () => {
    for (const [k, v] of Object.entries(TRANSPORT_CLASS_LABEL)) {
      expect(v, k).toBeTruthy();
      expect(v.length, k).toBeLessThanOrEqual(k.length);
      expect(v.length, k).toBeLessThanOrEqual(24);
    }
  });
});

describe("errors", () => {
  it("phoneErrorReason reads a ReasonError's reason, or empty", () => {
    expect(phoneErrorReason({ error: "x", reason: "number_in_use" })).toBe("number_in_use");
    expect(phoneErrorReason({ error: "x" })).toBe("");
    expect(phoneErrorReason(undefined)).toBe("");
    expect(phoneErrorReason("boom")).toBe("");
    expect(phoneErrorReason({ reason: 7 })).toBe("");
  });

  it("phoneErrorMessage is the server's sentence as it is, or the fallback", () => {
    expect(phoneErrorMessage({ error: "The body template is invalid: unknown placeholder {{x}}.", reason: "template_invalid" }, "Could not save.")).toBe(
      "The body template is invalid: unknown placeholder {{x}}.",
    );
    expect(phoneErrorMessage({ error: "" }, "Could not save.")).toBe("Could not save.");
    expect(phoneErrorMessage(undefined, "Could not save.")).toBe("Could not save.");
  });
});

describe("VOICE_LANGUAGES", () => {
  it("twenty BCP-47 tags matching domain.phoneLanguageRe, en-US first, no duplicates", () => {
    expect(VOICE_LANGUAGES).toHaveLength(20);
    expect(VOICE_LANGUAGES[0]).toBe("en-US");
    expect(new Set(VOICE_LANGUAGES).size).toBe(VOICE_LANGUAGES.length);
    for (const t of VOICE_LANGUAGES) expect(t).toMatch(/^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$/);
  });
});

describe("codeAnswer — the code states of mock §2 (2c–2e), for a code by SMS or by a call (revision 9)", () => {
  it("code_incorrect points at the latest message of the way the code came, and leaves Verify usable", () => {
    expect(codeAnswer("code_incorrect", "sms")).toEqual({
      line: "That code is not right. Check the most recent SMS and try again.",
      incorrect: true,
      dead: false,
    });
    expect(codeAnswer("code_incorrect", "voice")).toEqual({
      line: "That code is not right. Check the code from the most recent call and try again.",
      incorrect: true,
      dead: false,
    });
  });

  it("code_expired and code_attempts_exhausted spend the code, whatever way it came", () => {
    for (const via of ["sms", "voice"] as const) {
      expect(codeAnswer("code_expired", via)).toEqual({
        line: "This code has expired — codes work for 10 minutes. Send a new one.",
        incorrect: false,
        dead: true,
      });
      expect(codeAnswer("code_attempts_exhausted", via)).toEqual({
        line: "Too many wrong codes — this code no longer works. Send a new one.",
        incorrect: false,
        dead: true,
      });
    }
  });

  it("a call's lines never say SMS; any other reason is not a code state", () => {
    for (const r of ["code_incorrect", "code_expired", "code_attempts_exhausted"] as const) {
      expect(codeAnswer(r, "voice")!.line).not.toMatch(/SMS/);
    }
    expect(codeAnswer("send_limited", "sms")).toBeNull();
    expect(codeAnswer("", "sms")).toBeNull();
    expect(CODE_SEND_LIMITED).toBe(
      "No code sent: too many codes were requested. You can get 3 codes an hour, and a number gets at most 10 a day. Try again later.",
    );
    expect(CODE_SEND_LIMITED).not.toMatch(/SMS/);
  });

  it("a code whose way is not known (sent before a reload, while calls are possible) names neither way", () => {
    expect(codeAnswer("code_incorrect", null)).toEqual({
      line: "That code is not right. Check the most recent code and try again.",
      incorrect: true,
      dead: false,
    });
    for (const r of ["code_incorrect", "code_expired", "code_attempts_exhausted"] as const) {
      expect(codeAnswer(r, null)!.line).not.toMatch(/SMS|call/);
    }
  });
});

describe("canHoldPhoneChannel — D28, judged against the channel as stored", () => {
  const ctx = (projectAdmin: boolean) => ({ projectAdmin, selfUserId: "u-bo" });
  it.each([
    ["admin, someone's person channel", { recipient: "user", recipient_user_id: "u-ada" }, true, true],
    ["admin, a number channel", { recipient: "number" }, true, true],
    ["editor, their own person channel", { recipient: "user", recipient_user_id: "u-bo" }, false, true],
    ["editor, someone else's person channel", { recipient: "user", recipient_user_id: "u-ada" }, false, false],
    ["editor, a deleted recipient's channel", { recipient: "user" }, false, false],
    ["editor, a number channel", { recipient: "number" }, false, false],
  ] as const)("%s", (_l, ch, admin, want) => {
    expect(canHoldPhoneChannel(ch, ctx(admin))).toBe(want);
  });

  it("an editor with no user id holds nothing", () => {
    expect(canHoldPhoneChannel({ recipient: "user" }, { projectAdmin: false, selfUserId: "" })).toBe(false);
    expect(canHoldPhoneChannel({ recipient: "user", recipient_user_id: "" }, { projectAdmin: false, selfUserId: "" })).toBe(false);
  });
});

describe("channelRecipientLabel — the row's “to …” line (mock 3e, 3e‴; §14 point 1)", () => {
  const self = { selfUserId: "u-bo" };
  it.each([
    ["a person, by name", { type: "sms", recipient: "user", recipient_user_id: "u-ada", recipient_display_name: "Ada Park" }, { text: "Ada Park", style: "plain" }],
    ["the reader themselves", { type: "voice", recipient: "user", recipient_user_id: "u-bo", recipient_display_name: "Bo Lindqvist" }, { text: "you", style: "plain" }],
    ["a deleted account (no recipient_user_id)", { type: "sms", recipient: "user", recipient_display_name: null }, { text: "a deleted account", style: "deleted" }],
    ["a living person with no name is not called deleted — the chip's wording, “the recipient”", { type: "sms", recipient: "user", recipient_user_id: "u-x", recipient_display_name: "" }, { text: "the recipient", style: "plain" }],
    ["a fixed number, by its mask", { type: "sms", recipient: "number", number_masked: "+44•••••0199" }, { text: "+44•••••0199", style: "mono" }],
  ] as const)("%s", (_l, ch, want) => {
    expect(channelRecipientLabel(ch, self)).toEqual(want);
  });

  it("is null for a text channel and for a number channel without a mask", () => {
    expect(channelRecipientLabel({ type: "slack" }, self)).toBeNull();
    expect(channelRecipientLabel({ type: "sms", recipient: "number" }, self)).toBeNull();
  });
});
