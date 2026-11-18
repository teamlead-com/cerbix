// FR-041a — the vocabulary every phone surface shares: Settings → Telephony (mock 1i), Account →
// Phone (2), the channel editor and list (3), the incident's Phone panel (4) and `/i/:ref` (5).
// Pure: no Vue, no store, no request — the components take their words from here so the same
// delivery reads the same in the channel's list and on the incident.
//
// Rulings of the owner that shape it (spec revision 7/8, §14 "SPA plan of FR-041a"):
//   * a delivery row shows the provider's HTTP status, its code and the number of attempts, and
//     never says "timeout", "within 5 s" or "retrying" — the record holds neither (§4.5);
//   * a `storm_limited` row names the limit from `notes`, never its value (point 3);
//   * names come from `recipient_display_name`; without one the text is neutral (points 1, 2);
//   * a monitor's recovery has ONE label in every list ("Monitor recovery");
//   * the test SMS alone shows the class of a transport failure (`transport_class`, revision 8).

import type { components } from "@/api/schema";
import { CHIP_DORM, CHIP_DOWN, CHIP_WARN } from "@/lib/gate";

type Schemas = components["schemas"];

export type PhoneDelivery = Schemas["PhoneDelivery"];
export type PhoneDeliveryReason = PhoneDelivery["reason"];
export type UserPhone = Schemas["UserPhone"];
export type TelephonyView = Schemas["TelephonySettingsView"];
export type TelephonyTestSMSResult = Schemas["TelephonyTestSMSResult"];
export type TransportClass = NonNullable<TelephonyTestSMSResult["transport_class"]>;
export type ChannelDeliverable = Schemas["ChannelDeliverable"];
export type NotificationChannel = Schemas["NotificationChannel"];
export type ChannelType = NonNullable<NotificationChannel["type"]>;
export type PhoneChannelType = "sms" | "voice";
export type TelephonyProvider = NonNullable<TelephonyView["provider"]>;
export type PhoneReason = NonNullable<Schemas["ReasonError"]["reason"]>;
export type DeliverableReason = NonNullable<ChannelDeliverable["reason"]>;

/** Compile-time proof that a literal tuple lists exactly the members of a schema enum: a member
 *  added to openapi (and to schema.d.ts) without being listed here fails vue-tsc. */
type Same<A, B> = [A] extends [B] ? ([B] extends [A] ? true : false) : false;

// ── Channel types ───────────────────────────────────────────────────────────────────────────────

export function isPhoneChannel(type: string | null | undefined): type is PhoneChannelType {
  return type === "sms" || type === "voice";
}

/** The type select's words (mock 3a); the table's Type column keeps the raw `sms` / `voice` (3g). */
export const CHANNEL_TYPE_LABEL: Record<PhoneChannelType, string> = { sms: "SMS", voice: "Voice call" };

/** A delivery's kind as the Phone panel prints it (mock 4a). */
export function kindLabel(kind: PhoneDelivery["kind"]): string {
  return kind === "voice" ? "Call" : "SMS";
}

// ── Delivery reasons (§4.5, part a: the whole closed set, P6) ───────────────────────────────────

export type DeliveryTone = "ok" | "warn" | "bad" | "config" | "pending";

const REASON_LABEL: Record<PhoneDeliveryReason, string> = {
  sending: "Sending",
  sent: "Sent",
  provider_unreachable: "Provider unreachable",
  provider_rejected: "Rejected by provider",
  outcome_unknown: "Outcome unknown",
  not_deliverable: "Not deliverable",
  voice_unsupported_by_provider: "Calls not supported",
  telephony_disabled: "Phone alerts off",
  storm_limited: "Storm limit",
};

/** The nine reasons of part (a), in the order of §4.5's table — a literal list, held equal to the
 *  schema enum by the type below. */
export const DELIVERY_REASONS = [
  "sending",
  "sent",
  "provider_unreachable",
  "provider_rejected",
  "outcome_unknown",
  "not_deliverable",
  "voice_unsupported_by_provider",
  "telephony_disabled",
  "storm_limited",
] as const;
const deliveryReasonsComplete: Same<(typeof DELIVERY_REASONS)[number], PhoneDeliveryReason> = true;
void deliveryReasonsComplete;

/** The reason chip's words (mock decision 14: reasons read in words). Unknown → the raw value. */
export function deliveryReasonLabel(reason: PhoneDeliveryReason): string {
  return REASON_LABEL[reason] ?? String(reason);
}

/** The chip's hue (mock 4 note): green sent; amber unreachable / unknown / storm; red rejected;
 *  grey the configuration reasons; dashed while sending. */
export function deliveryReasonTone(reason: PhoneDeliveryReason): DeliveryTone {
  switch (reason) {
    case "sent":
      return "ok";
    case "provider_unreachable":
    case "outcome_unknown":
    case "storm_limited":
      return "warn";
    case "provider_rejected":
      return "bad";
    case "not_deliverable":
    case "voice_unsupported_by_provider":
    case "telephony_disabled":
      return "config";
    case "sending":
      return "pending";
    default: {
      const unreachable: never = reason;
      void unreachable;
      return "config";
    }
  }
}

/** Colour classes per tone, on the tokens of style.css (both themes); combine with CHIP_BASE. */
export const DELIVERY_TONE_CLASS: Record<DeliveryTone, string> = {
  ok: "border-up bg-up-weak text-up",
  warn: CHIP_WARN,
  bad: CHIP_DOWN,
  config: "border-border bg-surface text-ink-3",
  pending: CHIP_DORM,
};

// ── The second line of a delivery (mock 4b) ─────────────────────────────────────────────────────

/** One span of the second line: `mono` for status, code and reference; `note` for a stored enum
 *  shown as the small mono chip (the cause of `not_deliverable`); `plain` for words. */
export interface DeliveryLinePart {
  text: string;
  style: "plain" | "mono" | "note";
}

export interface DeliveryDetail {
  parts: DeliveryLinePart[];
  /** A full sentence under the line — only `outcome_unknown` has one. */
  why: string | null;
}

const OUTCOME_UNKNOWN_WHY =
  "The process sending it stopped before the provider's answer was stored. It may have arrived; cerbix did not send it again, so it is not counted as delivered.";

/** A provider reference longer than 10 characters is shortened to its first 4 and last 4. */
export function shortProviderRef(ref: string): string {
  return ref.length > 10 ? `${ref.slice(0, 4)}…${ref.slice(-4)}` : ref;
}

function plural(n: number, one: string): string {
  return `${n} ${one}${n === 1 ? "" : "s"}`;
}

function statusText(status: number | null, code: string | null): string {
  const bits: string[] = [];
  if (status !== null && status > 0) bits.push(`HTTP ${status}`);
  if (code) bits.push(`code ${code}`);
  return bits.join(" · ");
}

function attemptsPart(d: PhoneDelivery): DeliveryLinePart[] {
  return d.attempts > 0 ? [{ text: plural(d.attempts, "attempt"), style: "plain" }] : [];
}

// ── Notes: the closed set of Go's `PhoneNote*` (internal/domain/phone.go) ─────────────────────────
// The schema types `notes` as string[], so the set is pinned here and in the spec.

export const LINK_NOTES = ["no_incident_ref", "no_public_url", "link_omitted"] as const;
export const CAUSE_NOTES = ["no_verified_number", "recipient_not_eligible"] as const;
export const STORM_NOTES = ["storm_per_incident", "storm_per_number"] as const;
export const PHONE_NOTES = [...LINK_NOTES, ...CAUSE_NOTES, ...STORM_NOTES] as const;
export type LinkNote = (typeof LINK_NOTES)[number];
export type CauseNote = (typeof CAUSE_NOTES)[number];
export type StormNote = (typeof STORM_NOTES)[number];
export type PhoneNote = (typeof PHONE_NOTES)[number];

const CAUSE_TEXT: Record<CauseNote, string> = {
  no_verified_number: "had no verified number when it was sent",
  recipient_not_eligible: "could no longer open this project when it was sent",
};

const STORM_TEXT: Record<StormNote, string> = {
  storm_per_incident: "too many messages to this number for this incident in the last hour",
  storm_per_number: "too many messages to this number in the last hour",
};

function firstOf<T extends string>(notes: string[], set: readonly T[]): T | null {
  return (notes.find((n) => (set as readonly string[]).includes(n)) as T | undefined) ?? null;
}

function notDeliverableParts(d: PhoneDelivery): DeliveryLinePart[] {
  const cause = firstOf(d.notes, CAUSE_NOTES);
  if (!cause) return [{ text: "The recipient could not be reached when it was sent", style: "plain" }];
  return [
    { text: cause, style: "note" },
    { text: `${d.recipient_display_name || "The recipient"} ${CAUSE_TEXT[cause]}`, style: "plain" },
  ];
}

function stormText(d: PhoneDelivery): string {
  const limit = firstOf(d.notes, STORM_NOTES);
  return limit ? STORM_TEXT[limit] : "refused by the storm limit";
}

/** The second line of a delivery row, as parts (for rendering) and its one sentence of why. */
export function deliveryDetail(d: PhoneDelivery): DeliveryDetail {
  switch (d.reason) {
    case "sent": {
      const parts: DeliveryLinePart[] = [];
      const st = statusText(d.provider_status, null);
      if (st) parts.push({ text: st, style: "mono" });
      if (d.kind === "sms" && d.segments > 0) parts.push({ text: plural(d.segments, "segment"), style: "plain" });
      parts.push(...attemptsPart(d));
      if (d.provider_ref) parts.push({ text: `ref ${shortProviderRef(d.provider_ref)}`, style: "mono" });
      return { parts, why: null };
    }
    case "provider_rejected": {
      const s = d.provider_status;
      const head =
        s !== null && s >= 300 && s < 400 ? `HTTP ${s} · redirect not followed` : statusText(s, d.provider_code);
      return { parts: [...(head ? [{ text: head, style: "mono" as const }] : []), ...attemptsPart(d)], why: null };
    }
    case "provider_unreachable": {
      const head = statusText(d.provider_status, d.provider_code);
      const parts = [...(head ? [{ text: head, style: "mono" as const }] : []), ...attemptsPart(d)];
      return { parts: parts.length ? parts : [{ text: "no answer from the provider", style: "plain" }], why: null };
    }
    case "outcome_unknown":
      return { parts: attemptsPart(d), why: OUTCOME_UNKNOWN_WHY };
    case "not_deliverable":
      return { parts: notDeliverableParts(d), why: null };
    case "voice_unsupported_by_provider":
      return { parts: [{ text: "The instance's provider cannot place calls.", style: "plain" }], why: null };
    case "telephony_disabled":
      return { parts: [{ text: "Phone alerts were switched off on this instance.", style: "plain" }], why: null };
    case "storm_limited":
      return { parts: [{ text: stormText(d), style: "plain" }], why: null };
    case "sending":
      return { parts: [{ text: "waiting for the provider's answer", style: "plain" }], why: null };
    default: {
      const unreachable: never = d.reason;
      return { parts: [{ text: String(unreachable), style: "mono" }], why: null };
    }
  }
}

/** The second line as one string: parts joined by " · ", then the why (tests, titles, aria). */
export function deliverySecondLine(d: PhoneDelivery): string {
  const det = deliveryDetail(d);
  const line = det.parts.map((p) => p.text).join(" · ");
  if (!det.why) return line;
  return line ? `${line} — ${det.why}` : det.why;
}

// ── Notes (mock 4b "Notes, beside any reason") ──────────────────────────────────────────────────

/** The link notes and their words. The causes of `not_deliverable` and the storm limits are part of
 *  the second line (deliveryDetail), not separate notes. */
export const NOTE_LABEL: Record<LinkNote, string> = {
  no_incident_ref: "sent before the incident opened — no link",
  no_public_url: "sent without a link",
  link_omitted: "link dropped to fit the message",
};

/** A delivery's notes as chips, in stored order: the mono chip (`code`) and its words (`label`).
 *  Cause and storm notes are left out — the second line says them. A note outside the closed set
 *  is shown raw with no words rather than dropped: a trace is never hidden (P7). */
export function deliveryNotes(d: Pick<PhoneDelivery, "notes">): { code: string; label: string }[] {
  const consumed: readonly string[] = [...CAUSE_NOTES, ...STORM_NOTES];
  return d.notes
    .filter((n) => !consumed.includes(n))
    .map((n) => ({ code: n, label: Object.hasOwn(NOTE_LABEL, n) ? NOTE_LABEL[n as LinkNote] : "" }));
}

// ── What a delivery was about ───────────────────────────────────────────────────────────────────

/** The seven alert kinds (`domain.PhoneAlert*`). A monitor's recovery is "Monitor recovery" in
 *  every list; a service's recovery reads as the mock's message table does, "Service alert ·
 *  recovered" (alertSummary adds the word). */
export const ALERT_KIND_LABEL: Record<string, string> = {
  monitor_down: "Monitor down",
  monitor_recovered: "Monitor recovery",
  service_health: "Service alert",
  service_recovered: "Service alert",
  escalation_step: "Escalation step",
  monitor_burn: "Budget burn",
  service_burn: "Budget burn",
};

/** The alert kind's words; an unknown kind is shown raw, never a crash. */
export function alertKindLabel(kind: string): string {
  return Object.hasOwn(ALERT_KIND_LABEL, kind) ? ALERT_KIND_LABEL[kind] : kind;
}

/**
 * What a delivery was about, as one label: the kind (with the escalation step's number), then the
 * subject's name and a service alert's state — "Monitor down · payments-api", "Service alert ·
 * checkout down", "Service alert · checkout recovered", "Escalation step 2". `withSubject: false` drops the name where the page already
 * names the subject (the incident's Phone panel: "Service alert · down").
 */
export function alertSummary(
  d: Pick<PhoneDelivery, "alert_kind" | "subject" | "service_state" | "escalation_step">,
  opts: { withSubject?: boolean } = {},
): string {
  let head = alertKindLabel(d.alert_kind);
  if (d.alert_kind === "escalation_step" && d.escalation_step !== null && d.escalation_step !== undefined) {
    head = `${head} ${d.escalation_step}`;
  }
  const tail: string[] = [];
  if (opts.withSubject !== false && d.subject?.name) tail.push(d.subject.name);
  if (d.service_state) tail.push(d.service_state);
  else if (d.alert_kind === "service_recovered") tail.push("recovered"); // service_state is null for it
  return tail.length ? `${head} · ${tail.join(" ")}` : head;
}

// ── A phone channel's route state (mock 3e, 3e′, 3e‴) ───────────────────────────────────────────

/** The closed set of `deliverable.reason`, held equal to the schema enum. */
export const DELIVERABLE_REASONS = [
  "disabled",
  "telephony_disabled",
  "voice_unsupported_by_provider",
  "number_unverified",
  "no_verified_number",
  "recipient_not_eligible",
  "recipient_deleted",
] as const;
const deliverableReasonsComplete: Same<(typeof DELIVERABLE_REASONS)[number], DeliverableReason> = true;
void deliverableReasonsComplete;

export type RouteChannel = Pick<
  NotificationChannel,
  "type" | "recipient" | "recipient_user_id" | "recipient_display_name" | "disabled_reason" | "deliverable"
>;

export type RouteChip =
  | { kind: "none" }
  /** Solid chip; the toggle is locked and `unlock` is its tooltip. */
  | { kind: "disabled"; text: string; unlock: string }
  /** Dashed chip; the channel keeps its own switch. */
  | { kind: "notRoute"; text: string };

/**
 * The chip under a channel's name. `disabled_reason` wins; then a `deliverable.route === false`
 * whose reason is not the channel's own switch (`disabled` needs no mark, ruling 9); else none.
 * `selfUserId` is the signed-in user, whose own channel reads "you … — add one".
 */
export function routeChip(ch: RouteChannel, ctx: { selfUserId: string | null | undefined }): RouteChip {
  if (!isPhoneChannel(ch.type)) return { kind: "none" };
  if (ch.disabled_reason === "number_unverified") {
    return { kind: "disabled", text: "Disabled · number not confirmed", unlock: "Confirm the number to enable this channel" };
  }
  if (ch.disabled_reason === "recipient_deleted") {
    return {
      kind: "disabled",
      text: "Disabled · the recipient's account was deleted",
      unlock: "The recipient was deleted — pick another person or delete the channel",
    };
  }
  const dv = ch.deliverable;
  if (!dv || dv.route !== false || dv.reason === "disabled") return { kind: "none" };
  const self = ch.recipient === "user" && !!ctx.selfUserId && ch.recipient_user_id === ctx.selfUserId;
  const name = ch.recipient_display_name || "";
  const reason = dv.reason;
  const why = ((): string => {
    if (reason === undefined) return "";
    switch (reason) {
      case "telephony_disabled":
        return "phone alerts are off on this instance";
      case "voice_unsupported_by_provider":
        return "the instance's provider cannot place calls";
      case "no_verified_number":
        if (self) return "you have no verified number — add one";
        return name ? `${name} has no verified number` : "the recipient has no verified number";
      case "recipient_not_eligible":
        return name ? `${name} can no longer open this project` : "the recipient can no longer open this project";
      case "number_unverified":
        return "number not confirmed";
      case "recipient_deleted":
        return "the recipient's account was deleted";
      default: {
        const unreachable: never = reason;
        return String(unreachable);
      }
    }
  })();
  return { kind: "notRoute", text: why ? `Not a route · ${why}` : "Not a route" };
}

/** Whom a phone channel sends to, for the row's "to …" line (mock 3e): the mask of a number
 *  (`mono`), "you" for the reader's own channel, the recipient's name (§14 point 1), or "a deleted
 *  account" (`deleted`, drawn in italics) once the recipient is gone. Only a missing
 *  `recipient_user_id` means deleted — a living person may have an empty name. Null for a text
 *  channel. */
export function channelRecipientLabel(
  ch: Pick<NotificationChannel, "type" | "recipient" | "recipient_user_id" | "recipient_display_name" | "number_masked">,
  ctx: { selfUserId: string | null | undefined },
): { text: string; style: "plain" | "mono" | "deleted" } | null {
  if (!isPhoneChannel(ch.type)) return null;
  if (ch.recipient === "number") return ch.number_masked ? { text: ch.number_masked, style: "mono" } : null;
  if (!ch.recipient_user_id) return { text: "a deleted account", style: "deleted" };
  if (ctx.selfUserId && ch.recipient_user_id === ctx.selfUserId) return { text: "you", style: "plain" };
  // A living person with no name reads as the route chip under it does: "the recipient" (review M-5).
  return { text: ch.recipient_display_name || "the recipient", style: "plain" };
}

// ── Incident references (D21) ───────────────────────────────────────────────────────────────────

const CROCKFORD = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";

/** Mirrors `domain.NormalizeIncidentRef`: exactly 8 characters, any case, I/L → 1 and O → 0, the
 *  Crockford alphabet only. `null` → the "No incident found" page without a request (mock 5d). */
export function normalizeIncidentRef(s: string): string | null {
  if (s.length !== 8) return null;
  let out = "";
  for (const raw of s) {
    if (raw.charCodeAt(0) >= 0x80) return null;
    let ch = raw.toUpperCase();
    if (ch === "I" || ch === "L") ch = "1";
    else if (ch === "O") ch = "0";
    if (!CROCKFORD.includes(ch)) return null;
    out += ch;
  }
  return out.length === 8 ? out : null;
}

// ── generic_http headers ────────────────────────────────────────────────────────────────────────

const TOKEN_RE = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/;

export type HeaderParse = { ok: true; headers: Record<string, string> } | { ok: false; line: number };

/** "Name: value" per non-empty line; the name is an HTTP token, the value is trimmed. A line that
 *  is not one reports its 1-based number. Empty text is an empty map. */
export function parseHeaderLines(text: string): HeaderParse {
  const headers: Record<string, string> = {};
  const lines = text.split(/\r?\n/);
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i].trim();
    if (line === "") continue;
    const at = line.indexOf(":");
    if (at <= 0) return { ok: false, line: i + 1 };
    const name = line.slice(0, at).trim();
    if (!TOKEN_RE.test(name)) return { ok: false, line: i + 1 };
    headers[name] = line.slice(at + 1).trim();
  }
  return { ok: true, headers };
}

// ── The test SMS (mock 1i, revision 8 §4.1) ─────────────────────────────────────────────────────

/** Short words for the closed set of `transport_class`. */
export const TRANSPORT_CLASS_LABEL: Record<TransportClass, string> = {
  "destination refused by the delivery egress policy": "egress policy refused",
  "certificate not accepted": "certificate not accepted",
  "the server does not speak TLS": "no TLS",
  "TLS handshake failed": "TLS handshake failed",
  timeout: "timeout",
  "DNS lookup failed": "DNS lookup failed",
  "connection refused": "connection refused",
  "connection failed": "connection failed",
};

export interface TestSmsMessage {
  tone: "ok" | "bad" | "warn";
  /** "✓ Accepted by the provider", "Rejected by the gateway", … */
  lead: string;
  /** The mono part: "HTTP 201", "HTTP 400 · code 21211", or "". */
  status: string;
  /** The short label of `transport_class`, when the provider was not reached. */
  transportClass: string | null;
  /** What to do next: "Check your phone.", … */
  advice: string;
  /** The whole row as one sentence. */
  text: string;
}

function testMsg(tone: TestSmsMessage["tone"], lead: string, status: string, transportClass: string | null, advice: string): TestSmsMessage {
  const text = `${lead}${status ? ` · ${status}` : ""}${transportClass ? ` (${transportClass})` : ""}. ${advice}`;
  return { tone, lead, status, transportClass, advice, text };
}

/** The 1i row for a 200 answer of `POST /settings/telephony/test-sms` (a refusal is a 200 too). */
export function testSmsMessage(
  r: TelephonyTestSMSResult,
  provider: TelephonyProvider | string,
  responseCodeField: string,
): TestSmsMessage {
  const gateway = provider === "generic_http";
  const status = r.http_status > 0 ? `HTTP ${r.http_status}` : "";
  switch (r.outcome) {
    case "sent":
      return testMsg("ok", "✓ Accepted by the provider", status, null, "Check your phone.");
    case "rejected": {
      if (r.http_status >= 300 && r.http_status < 400) {
        return testMsg("bad", "Rejected", status, null, "A redirect is not followed; put the final URL in the settings.");
      }
      const lead = gateway ? "Rejected by the gateway" : "Rejected by the provider";
      const full = statusText(r.http_status, r.provider_code || null);
      if (gateway && !r.provider_code && !responseCodeField.trim()) {
        return testMsg("bad", lead, full, null, "Set “Error code field” to see the gateway's own code.");
      }
      return testMsg("bad", lead, full, null, "Check the From number and the account.");
    }
    case "unreachable":
    default: {
      const tc = r.transport_class ? (TRANSPORT_CLASS_LABEL[r.transport_class] ?? r.transport_class) : null;
      const lead = r.http_status > 0 ? "No usable answer from the provider" : "No answer from the provider";
      return testMsg("warn", lead, statusText(r.http_status, r.provider_code || null), tc, "The message may or may not have been sent.");
    }
  }
}

// ── Errors (ReasonError: branch on `reason`, show `error`) ──────────────────────────────────────

/** The `reason` of a ReasonError, or "" — branch on it, never on the text or the status alone. */
export function phoneErrorReason(err: unknown): PhoneReason | "" {
  if (err && typeof err === "object" && "reason" in err) {
    const r = (err as { reason?: unknown }).reason;
    if (typeof r === "string") return r as PhoneReason;
  }
  return "";
}

/** The server's sentence as it is (rulings 8 and 9), or the fallback when it sent none. */
export function phoneErrorMessage(err: unknown, fallback: string): string {
  if (err && typeof err === "object" && "error" in err) {
    const e = (err as { error?: unknown }).error;
    if (typeof e === "string" && e !== "") return e;
  }
  return fallback;
}

// ── Code states (mock §2, 2c–2f) — Account → Phone and a number channel's verification (3b-2) ────

/** The way a code was sent: by SMS, or spoken by a call (spec revision 9, `via`). */
export type CodeVia = "sms" | "voice";

/**
 * What the last send of a code from Account → Phone did: it went by SMS or by a call, or the
 * provider refused or could not be reached ("undelivered", a 502). The backend stores the new code
 * (ending the previous one) before it calls the provider, so after a 502 no running code reached
 * the phone.
 */
export type PhoneCodeSend = CodeVia | "undelivered";

/** What a Verify refusal does to the form: its line, a red code field, or a spent code. */
export interface CodeAnswer {
  line: string;
  /** 2c: the field turns red; Verify stays usable. */
  incorrect: boolean;
  /** 2d / 2e: the code is spent; Verify waits for a new one. */
  dead: boolean;
}

/**
 * The mock's line for a code state, or null when `reason` is not one (read the server's sentence).
 * `via` is the way the running code went; null when the page does not know it (a code sent before a
 * reload while calls are possible), and the line then names neither way.
 */
export function codeAnswer(reason: PhoneReason | "", via: CodeVia | null): CodeAnswer | null {
  switch (reason) {
    case "code_incorrect":
      return {
        line:
          via === "voice"
            ? "That code is not right. Check the code from the most recent call and try again."
            : via === "sms"
              ? "That code is not right. Check the most recent SMS and try again."
              : "That code is not right. Check the most recent code and try again.",
        incorrect: true,
        dead: false,
      };
    case "code_expired":
      return { line: "This code has expired — codes work for 10 minutes. Send a new one.", incorrect: false, dead: true };
    case "code_attempts_exhausted":
      return { line: "Too many wrong codes — this code no longer works. Send a new one.", incorrect: false, dead: true };
    default:
      return null;
  }
}

/** 2f, the fallback under a refused send when the server's `send_limited` carries no sentence. */
export const CODE_SEND_LIMITED =
  "No code sent: too many codes were requested. You can get 3 codes an hour, and a number gets at most 10 a day. Try again later.";
/** Fallbacks for an answer with no `error` sentence (a bare 501, a dropped connection). */
export const CODE_SEND_FAILED = "No code sent.";
export const CODE_VERIFY_FAILED = "The code could not be checked.";

// ── D28: who may edit, switch or delete a phone channel ─────────────────────────────────────────

/**
 * Whether the caller may update or delete this phone channel, judged against the channel as stored
 * (D28; `mayHold` in internal/api/handlers_phonechannels.go): a project admin any; an editor only a
 * person channel that phones themselves — never a number channel, another person's, or a deleted
 * recipient's. Callers check `canProjectWrite` first; this decides only among writers.
 */
export function canHoldPhoneChannel(
  ch: Pick<NotificationChannel, "recipient" | "recipient_user_id">,
  ctx: { projectAdmin: boolean; selfUserId: string | null | undefined },
): boolean {
  if (ctx.projectAdmin) return true;
  return ch.recipient === "user" && !!ctx.selfUserId && ch.recipient_user_id === ctx.selfUserId;
}

// ── Account → Phone (mock decision 9) ───────────────────────────────────────────────────────────

/** The fixed list of alert languages; each matches `domain.phoneLanguageRe`. */
export const VOICE_LANGUAGES: readonly string[] = [
  "en-US",
  "en-GB",
  "de-DE",
  "fr-FR",
  "es-ES",
  "it-IT",
  "nl-NL",
  "pt-BR",
  "pt-PT",
  "pl-PL",
  "tr-TR",
  "ru-RU",
  "uk-UA",
  "sv-SE",
  "da-DK",
  "nb-NO",
  "fi-FI",
  "ja-JP",
  "ko-KR",
  "zh-CN",
];
