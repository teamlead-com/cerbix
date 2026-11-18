<script setup lang="ts">
// FR-041a §4.1 — Settings → Administration → Telephony (mock §1: 1a–1i, 1c′, 1c″). Global admin
// only; the panel loads itself. The card follows the Email (SMTP) card's grammar: a status pill in
// the title row, stored secrets marked "✓ stored" with an "(unchanged)" placeholder, blank keeps.
//
// Rulings that shape it (spec revision 7/8, §14): the server's `error` sentence is shown as it is —
// there is no client-side template check (8), and an answer the mock has no text for reads that
// sentence under the action that failed (9); the test SMS names `transport_class` when the answer
// carries one and never assumes a timeout (revision 8, §4.1).
import { computed, onMounted, reactive, ref, watch } from "vue";
import { api } from "@/api/client";
import type { components } from "@/api/schema";
import {
  type TelephonyTestSMSResult,
  type TelephonyView,
  type TestSmsMessage,
  parseHeaderLines,
  phoneErrorMessage,
  phoneErrorReason,
  testSmsMessage,
} from "@/lib/phone";
import { useSession } from "@/stores/session";

type SetBody = components["schemas"]["SetTelephonySettings"];
type ServiceWithoutRoute = components["schemas"]["ServiceWithoutRoute"];
type Provider = "twilio" | "generic_http";
type Method = "POST" | "PUT";
type ContentType = "application/json" | "application/x-www-form-urlencoded";
type Encoding = "transliterate" | "unicode";

const emit = defineEmits<{ (e: "open-tab", key: string): void }>();

const session = useSession();

// ── Words of the mock that more than one place uses ─────────────────────────────────────────────
const KEY_MISSING_LEAD = "Phone alerts need an encryption key, and this server has none.";
const KEY_MISSING_REST =
  "They store a provider credential and phone numbers, and cerbix keeps those only encrypted. Set security.encryption_key in the server configuration and restart; this form opens once the key is there.";
const TEMPLATE_NOTE = "Not saved: the body template has a placeholder outside a value.";
const HEADERS_CONFLICT_NOTE =
  "Not saved: remove the stored headers or type new ones, not both. To replace them, type the new headers and leave “Remove stored headers” off.";
const TEST_NO_NUMBER = "You have no verified number.";
const TEST_UNSAVED = "Save first — the test uses the saved settings.";
const TEST_OFF = "Turn phone alerts on and save to send a test.";
const TEST_READY = "Sends a fixed text to your own verified number, with the saved settings.";

// A Messaging Service SID as Twilio's From sends SMS only (D3, revision 8): voice is unavailable
// exactly as under the generic HTTP gateway. The server's own rule is `^MG[0-9a-fA-F]{32}$`.
const MESSAGING_SERVICE_RE = /^MG[0-9a-fA-F]{32}$/;
const isMessagingService = (from: string) => MESSAGING_SERVICE_RE.test(from.trim());

// ── State ───────────────────────────────────────────────────────────────────────────────────────
interface Form {
  enabled: boolean;
  provider: Provider;
  twilio: { account_sid: string; auth_token: string; from: string };
  generic: {
    method: Method;
    url: string;
    content_type: ContentType;
    headers: string;
    headers_clear: boolean;
    body_template: string;
    from: string;
    response_code_field: string;
  };
  public_base_url: string;
  voice_language: string;
  sms_encoding: Encoding;
  sms_max_segments: number;
  storm: { per_incident_per_hour: number; per_number_per_hour: number };
}

/** The form a view opens as: §4.1's defaults fill what the view leaves empty (mock 1a). */
function formFromView(v: TelephonyView | null): Form {
  const tw = v?.twilio ?? {};
  const g = v?.generic_http ?? {};
  return {
    enabled: v?.enabled === true,
    provider: v?.provider === "generic_http" ? "generic_http" : "twilio",
    twilio: { account_sid: tw.account_sid ?? "", auth_token: "", from: tw.from ?? "" },
    generic: {
      method: g.method === "PUT" ? "PUT" : "POST",
      url: g.url ?? "",
      content_type: g.content_type === "application/x-www-form-urlencoded" ? g.content_type : "application/json",
      headers: "",
      headers_clear: false,
      body_template: g.body_template ?? "",
      from: g.from ?? "",
      response_code_field: g.response_code_field ?? "",
    },
    public_base_url: v?.public_base_url ?? "",
    voice_language: v?.voice_language || "en-US",
    sms_encoding: v?.sms_encoding === "unicode" ? "unicode" : "transliterate",
    sms_max_segments: v?.sms_max_segments && v.sms_max_segments >= 1 && v.sms_max_segments <= 4 ? v.sms_max_segments : 2,
    storm: {
      per_incident_per_hour: v?.storm?.per_incident_per_hour || 5,
      per_number_per_hour: v?.storm?.per_number_per_hour || 20,
    },
  };
}

const loading = ref(true);
const loadError = ref("");
const view = ref<TelephonyView | null>(null);
const form = reactive<Form>(formFromView(null));
// The form as the last read or save left it: "unsaved changes" is the form differing from it.
const baseline = ref(JSON.stringify(formFromView(null)));
const mailURL = ref("");
const mailKnown = ref(false);

const saving = ref(false);
const saved = ref(false);
const saveNote = ref(""); // a refusal under Save (1c″, 1d, 1e, or the server's sentence)
const saveNoteIsKey = ref(false);
const templateError = ref("");
const headersError = ref("");

type Cause = "off" | "gateway" | "messaging" | "other";
interface Unroutable {
  services: ServiceWithoutRoute[];
  total: number;
  cause: Cause;
  body: SetBody;
  formKey: string;
  /** The server's `error`: the explanation of a 409 whose cause the form cannot tell (ruling 9). */
  serverSentence: string;
}
const unroutable = ref<Unroutable | null>(null);

const testing = ref(false);
const testResult = ref<TestSmsMessage | null>(null);
const testError = ref("");
// A 409 of the test that a pre-state already has words for (§4.1): shown as that pre-state even
// when the session or the view had not caught up yet.
const testRefusal = ref<"" | "off" | "no_number">("");

// ── Derived ─────────────────────────────────────────────────────────────────────────────────────
const keyMissing = computed(() => view.value?.encryption_key_configured === false);
const headersStored = computed(() => view.value?.generic_http?.headers_set === true);
const tokenStored = computed(() => view.value?.twilio?.auth_token_set === true);
// A secret the configured keys cannot open is stored but useless: every send fails. The label slot
// says so instead of "✓ stored" (the server's flag, `*_unreadable`).
const tokenUnreadable = computed(() => view.value?.twilio?.auth_token_unreadable === true);
const headersUnreadable = computed(() => view.value?.generic_http?.headers_unreadable === true);
const UNREADABLE_LABEL = "stored · cannot be read";
const UNREADABLE_PLACEHOLDER = "stored, but unreadable — type it again";
const tokenPlaceholder = computed(() =>
  tokenUnreadable.value ? UNREADABLE_PLACEHOLDER : tokenStored.value ? "•••••••• (unchanged)" : "auth token",
);
const headersPlaceholder = computed(() =>
  form.generic.headers_clear
    ? "Headers will be removed"
    : headersUnreadable.value
      ? UNREADABLE_PLACEHOLDER
      : headersStored.value
        ? "•••••••• (unchanged)"
        : "Authorization: Bearer …",
);
const dirty = computed(() => JSON.stringify(form) !== baseline.value);

const pill = computed(() => {
  const v = view.value;
  if (!v?.configured) return { text: "Off · not configured", on: false };
  if (!v.enabled) return { text: "Off", on: false };
  return { text: v.provider === "generic_http" ? "On · Generic HTTP" : "On · Twilio", on: true };
});

const mailHint = computed(() => mailURL.value.trim());
const noLinkAnywhere = computed(() => mailKnown.value && !form.public_base_url.trim() && !mailURL.value.trim());

const masked = computed(() => session.phone?.number_masked ?? "");
type TestState = "sending" | "no_number" | "unsaved" | "off" | "ready";
const testState = computed<TestState>(() => {
  if (testing.value) return "sending";
  if (!session.phone?.verified || testRefusal.value === "no_number") return "no_number";
  if (dirty.value) return "unsaved";
  if (view.value?.enabled !== true || testRefusal.value === "off") return "off";
  return "ready";
});
const testLabel = computed(() => {
  if (testState.value === "sending") return "Sending…";
  if (testState.value === "no_number" || !masked.value) return "Send test SMS";
  return `Send test SMS to ${masked.value}`;
});

// ── Load ────────────────────────────────────────────────────────────────────────────────────────
function applyView(v: TelephonyView) {
  view.value = v;
  Object.assign(form, formFromView(v));
  baseline.value = JSON.stringify(form);
}

async function load() {
  loading.value = true;
  loadError.value = "";
  try {
    const [tel, mail] = await Promise.all([
      api.GET("/api/v1/settings/telephony"),
      api.GET("/api/v1/settings/mail").catch(() => null),
    ]);
    if (!tel.data) {
      loadError.value = phoneErrorMessage(tel.error, "Could not load the telephony settings.");
      return;
    }
    applyView(tel.data);
    if (mail?.data) {
      mailURL.value = mail.data.public_base_url ?? "";
      mailKnown.value = true;
    }
  } catch {
    loadError.value = "Could not load the telephony settings.";
  } finally {
    loading.value = false;
  }
}

// ── Save ────────────────────────────────────────────────────────────────────────────────────────
/** The whole group, from the form. Both provider blocks always travel, so switching provider never
 *  wipes the other's stored fields; a secret is sent only when typed (blank keeps, §4.1). The
 *  gateway's headers act only while the gateway is the selected provider: a removal or a typed map
 *  left behind in a hidden block is neither sent nor judged. */
function buildBody(headers: Record<string, string>): SetBody {
  const twilio: NonNullable<SetBody["twilio"]> = { account_sid: form.twilio.account_sid, from: form.twilio.from };
  if (form.twilio.auth_token.trim()) twilio.auth_token = form.twilio.auth_token;
  const g = form.generic;
  const generic: NonNullable<SetBody["generic_http"]> = {
    method: g.method,
    url: g.url,
    content_type: g.content_type,
    body_template: g.body_template,
    from: g.from,
    response_code_field: g.response_code_field,
  };
  if (form.provider === "generic_http") {
    if (g.headers_clear) generic.headers_clear = true;
    else if (Object.keys(headers).length) generic.headers = headers;
  }
  return {
    enabled: form.enabled,
    provider: form.provider,
    twilio,
    generic_http: generic,
    public_base_url: form.public_base_url,
    voice_language: form.voice_language,
    sms_encoding: form.sms_encoding,
    sms_max_segments: Number(form.sms_max_segments),
    storm: {
      per_incident_per_hour: Number(form.storm.per_incident_per_hour),
      per_number_per_hour: Number(form.storm.per_number_per_hour),
    },
  };
}

/** Which change the 409 answers (mock 1f/1g): switching off dominates when both changed. */
function causeOf(): Cause {
  const v = view.value;
  if (v?.enabled && !form.enabled) return "off";
  if (form.provider === "generic_http" && v?.provider !== "generic_http") return "gateway";
  if (
    form.provider === "twilio" &&
    isMessagingService(form.twilio.from) &&
    !(v?.provider === "twilio" && isMessagingService(v.twilio?.from ?? ""))
  ) {
    return "messaging";
  }
  return "other";
}

function clearNotes() {
  saved.value = false;
  saveNote.value = "";
  saveNoteIsKey.value = false;
  templateError.value = "";
  headersError.value = "";
}

async function save() {
  if (keyMissing.value || saving.value) return;
  clearNotes();
  unroutable.value = null;
  let headers: Record<string, string> = {};
  if (form.provider === "generic_http" && !form.generic.headers_clear) {
    const parsed = parseHeaderLines(form.generic.headers);
    if (!parsed.ok) {
      headersError.value = `Line ${parsed.line} is not a header: write one “Name: value” per line.`;
      return;
    }
    headers = parsed.headers;
  }
  // The form as judged, taken before the request: an edit made meanwhile withdraws the warning.
  await send(buildBody(headers), causeOf(), JSON.stringify(form));
}

async function send(body: SetBody, cause: Cause, formKey: string) {
  saving.value = true;
  try {
    const res = await api.PUT("/api/v1/settings/telephony", { body });
    if (res.data) {
      unroutable.value = null;
      testRefusal.value = "";
      testResult.value = null;
      testError.value = "";
      applyView(res.data);
      saved.value = true;
      await session.fetchMe();
      return;
    }
    const err = res.error as unknown;
    switch (phoneErrorReason(err) as string) {
      case "services_without_route": {
        const e = err as components["schemas"]["ServicesWithoutRouteError"];
        unroutable.value = {
          services: e.services_without_route ?? [],
          total: e.services_without_route_total ?? e.services_without_route?.length ?? 0,
          cause,
          body,
          formKey,
          serverSentence: phoneErrorMessage(err, ""),
        };
        if (JSON.stringify(form) !== formKey) unroutable.value = null;
        return;
      }
      case "encryption_key_missing":
        saveNote.value = `${KEY_MISSING_LEAD} ${KEY_MISSING_REST}`;
        saveNoteIsKey.value = true;
        return;
      case "template_invalid":
        templateError.value = phoneErrorMessage(err, "The body template was refused.");
        saveNote.value = TEMPLATE_NOTE;
        return;
      case "headers_conflict":
        saveNote.value = HEADERS_CONFLICT_NOTE;
        return;
      default:
        saveNote.value = phoneErrorMessage(err, "Could not save.");
    }
  } catch {
    saveNote.value = "Could not save.";
  } finally {
    saving.value = false;
  }
}

/** "Turn off anyway" / "Switch anyway": the refused body again, confirmed (§4.1). */
async function confirmUnroutable() {
  const u = unroutable.value;
  if (!u || saving.value) return;
  clearNotes();
  await send({ ...u.body, confirm_unroutable: true }, u.cause, u.formKey);
}

/** "Keep phone alerts on" / "Keep Twilio": undo the change the warning is about; nothing is sent. */
function keepUnroutable() {
  const u = unroutable.value;
  if (!u) return;
  const v = view.value;
  if (u.cause === "off") form.enabled = true;
  else if (u.cause === "gateway") form.provider = "twilio";
  else if (u.cause === "messaging") form.twilio.from = v?.twilio?.from ?? "";
  unroutable.value = null;
}

const unroutableGroups = computed(() => {
  const groups: { project: string; services: ServiceWithoutRoute[] }[] = [];
  for (const s of unroutable.value?.services ?? []) {
    const last = groups[groups.length - 1];
    if (last && last.project === s.project) last.services.push(s);
    else groups.push({ project: s.project, services: [s] });
  }
  return groups;
});
const unroutableMore = computed(() => {
  const u = unroutable.value;
  return u ? Math.max(0, u.total - u.services.length) : 0;
});
const unroutableHead = computed(() => {
  // A 409 whose cause the form cannot tell (a view another admin changed meanwhile) has no mock
  // text: the server's sentence is its whole explanation (ruling 9).
  if (unroutable.value?.cause === "other" && unroutable.value.serverSentence) return unroutable.value.serverSentence;
  const n = unroutable.value?.total ?? 0;
  return `Not saved yet — ${n} ${n === 1 ? "service" : "services"} would be left with no route.`;
});
const unroutableText = computed(() => {
  switch (unroutable.value?.cause) {
    case "off":
      return "Phone channels stay, but they would not be called or texted and would stop counting as routes. These services' incidents would not be announced, and their members' own alerts page as usual:";
    case "gateway":
      return "The generic HTTP gateway cannot place calls: Voice call channels stay, but they would not be called and would stop counting as routes. SMS channels are not affected.";
    case "messaging":
      return "A Messaging Service cannot place calls: Voice call channels stay, but they would not be called and would stop counting as routes. SMS channels are not affected.";
    default:
      return "";
  }
});
const unroutableButtons = computed(() => {
  switch (unroutable.value?.cause) {
    case "off":
      return { confirm: "Turn off anyway", keep: "Keep phone alerts on", danger: true };
    case "gateway":
      return { confirm: "Switch anyway", keep: "Keep Twilio", danger: false };
    case "messaging":
      return { confirm: "Switch anyway", keep: "Keep the From number", danger: false };
    default:
      return { confirm: "Save anyway", keep: "Cancel", danger: false };
  }
});

// A warning answers ONE body: any change of the form withdraws it, so a confirm never sends a body
// the server did not judge. Every change also clears "✓ Saved".
watch(
  form,
  () => {
    if (unroutable.value && JSON.stringify(form) !== unroutable.value.formKey) unroutable.value = null;
    if (saved.value && dirty.value) saved.value = false;
  },
  { deep: true },
);
watch(() => form.generic.body_template, () => (templateError.value = ""));
watch(() => form.generic.headers, () => (headersError.value = ""));

function removeHeaders() {
  form.generic.headers = "";
  form.generic.headers_clear = true;
}
function undoRemoveHeaders() {
  form.generic.headers_clear = false;
}

// ── Test SMS (mock 1i) ──────────────────────────────────────────────────────────────────────────
async function sendTest() {
  if (testState.value !== "ready") return;
  testing.value = true;
  testResult.value = null;
  testError.value = "";
  try {
    const res = await api.POST("/api/v1/settings/telephony/test-sms");
    if (res.data) {
      testResult.value = testSmsMessage(
        res.data as TelephonyTestSMSResult,
        view.value?.provider ?? "",
        view.value?.generic_http?.response_code_field ?? "",
      );
      return;
    }
    const err = res.error as unknown;
    const reason = phoneErrorReason(err);
    if (reason === "telephony_disabled") {
      testRefusal.value = "off";
      await session.fetchMe();
    } else if (reason === "no_verified_number") {
      testRefusal.value = "no_number";
      await session.fetchMe();
    } else {
      testError.value = phoneErrorMessage(err, "Could not send the test SMS.");
    }
  } catch {
    testError.value = "Could not send the test SMS.";
  } finally {
    testing.value = false;
  }
}

function openPhoneTab() {
  emit("open-tab", "phone");
}

onMounted(load);

// ── Classes (the Email card's) ──────────────────────────────────────────────────────────────────
const LABEL = "text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3";
const INPUT =
  "rounded-sm border border-border bg-surface-2 px-3 py-2 font-mono text-[12.5px] outline-none focus:border-accent disabled:opacity-55";
const SELECT =
  "rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent disabled:opacity-55";
const HINT = "text-[11.5px] leading-[1.45] text-ink-3";
const SUBHEAD = "border-t border-border pt-[14px] text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-2";
const BTN_SEC =
  "h-[30px] rounded-sm border border-border px-[12px] text-[12.5px] text-ink-2 hover:border-border-strong disabled:opacity-50";
</script>

<template>
  <div v-if="loading" class="text-[13px] text-ink-3">Loading…</div>
  <div v-else-if="loadError" class="rounded border border-down/40 bg-down-weak p-4 text-[13px] text-down" data-testid="telephony-load-error">{{ loadError }}</div>
  <section v-else class="flex flex-col gap-4 rounded border border-border bg-surface p-[18px] shadow-card" data-testid="telephony-panel">
    <div class="flex items-center justify-between gap-3">
      <h2 class="text-[14px] font-semibold">Phone alerts (SMS and voice calls)</h2>
      <span class="rounded-full px-[10px] py-[3px] text-[11.5px] font-medium" :class="pill.on ? 'bg-up-weak text-up' : 'bg-inset text-ink-3'" data-testid="telephony-pill">{{ pill.text }}</span>
    </div>
    <p class="text-[12.5px] leading-[1.5] text-ink-3">Sends alerts as SMS and voice calls through your own provider account. Phone channels can be created only while this is on. Changes apply immediately to every process that delivers alerts.</p>

    <!-- 1e -->
    <div v-if="keyMissing" class="rounded border border-down/40 bg-down-weak px-[13px] py-3 text-[12.5px] leading-[1.5] text-ink-2" data-testid="telephony-key-missing">
      <b class="font-semibold text-down">{{ KEY_MISSING_LEAD }}</b>
      They store a provider credential and phone numbers, and cerbix keeps those only encrypted. Set <span class="font-mono">security.encryption_key</span> in the server configuration and restart; this form opens once the key is there.
    </div>

    <fieldset :disabled="keyMissing || saving" class="flex min-w-0 flex-col gap-4">
      <label class="inline-flex cursor-pointer items-center gap-[8px] text-[13px]"><input v-model="form.enabled" type="checkbox" class="h-[15px] w-[15px] accent-accent" data-testid="telephony-enabled" />Enable phone alerts</label>

      <div class="flex flex-col gap-[6px]">
        <span :class="LABEL">Provider</span>
        <div class="flex flex-wrap gap-x-6 gap-y-2">
          <label class="inline-flex cursor-pointer items-start gap-[8px] text-[13px]">
            <input v-model="form.provider" type="radio" name="telephony-provider" value="twilio" class="mt-[3px] accent-accent" data-testid="telephony-provider-twilio" />
            <span>Twilio<span class="block text-[11.5px] text-ink-3">SMS and voice calls</span></span>
          </label>
          <label class="inline-flex cursor-pointer items-start gap-[8px] text-[13px]">
            <input v-model="form.provider" type="radio" name="telephony-provider" value="generic_http" class="mt-[3px] accent-accent" data-testid="telephony-provider-generic" />
            <span>Generic HTTP gateway<span class="block text-[11.5px] text-ink-3">SMS only, through a request you define — no voice calls</span></span>
          </label>
        </div>
      </div>

      <!-- Twilio (1a/1b) -->
      <template v-if="form.provider === 'twilio'">
        <div class="grid grid-cols-3 gap-4 max-[760px]:grid-cols-1">
          <label class="flex flex-col gap-[6px]"><span :class="LABEL">Account SID</span>
            <input v-model="form.twilio.account_sid" type="text" autocomplete="off" placeholder="AC…" :class="INPUT" data-testid="twilio-account-sid" /></label>
          <label class="flex flex-col gap-[6px]"><span :class="LABEL">Auth token<span v-if="tokenUnreadable" class="ml-1 font-normal normal-case text-down" data-testid="twilio-auth-token-stored">{{ UNREADABLE_LABEL }}</span><span v-else-if="tokenStored" class="ml-1 font-normal normal-case text-up" data-testid="twilio-auth-token-stored">✓ stored</span></span>
            <input v-model="form.twilio.auth_token" type="password" autocomplete="new-password" :placeholder="tokenPlaceholder" :class="INPUT" data-testid="twilio-auth-token" /></label>
          <label class="flex flex-col gap-[6px]"><span :class="LABEL">From</span>
            <input v-model="form.twilio.from" type="text" autocomplete="off" placeholder="+15550100 or MG…" :class="INPUT" data-testid="twilio-from" />
            <span :class="HINT">An E.164 number or a Messaging Service SID.</span></label>
        </div>
        <div v-if="isMessagingService(form.twilio.from)" class="rounded bg-inset px-[13px] py-[10px] text-[12.5px] leading-[1.5] text-ink-3" data-testid="telephony-messaging-note">
          <b class="font-semibold text-ink">SMS only.</b> A Messaging Service cannot place calls: a Voice call channel cannot be created, and existing ones are not called while it is the From.
        </div>
      </template>

      <!-- Generic HTTP gateway (1c, 1c′, 1d) -->
      <template v-else>
        <div class="rounded bg-inset px-[13px] py-[10px] text-[12.5px] leading-[1.5] text-ink-3" data-testid="telephony-gateway-note">
          <b class="font-semibold text-ink">SMS only.</b> A gateway cannot place calls: a Voice call channel cannot be created, and existing ones are not called while this provider is in use.
        </div>
        <div class="grid grid-cols-[2fr_1fr] gap-4 max-[560px]:grid-cols-1">
          <label class="flex flex-col gap-[6px]"><span :class="LABEL">URL</span>
            <input v-model="form.generic.url" type="text" autocomplete="off" placeholder="https://sms-gw.example.net/v2/messages" :class="INPUT" data-testid="generic-url" /></label>
          <label class="flex flex-col gap-[6px]"><span :class="LABEL">Method</span>
            <select v-model="form.generic.method" :class="SELECT" data-testid="generic-method"><option value="POST">POST</option><option value="PUT">PUT</option></select></label>
        </div>
        <label class="flex flex-col gap-[6px]"><span :class="LABEL">Content type</span>
          <select v-model="form.generic.content_type" :class="[SELECT, 'max-w-[320px]']" data-testid="generic-content-type">
            <option value="application/json">application/json</option>
            <option value="application/x-www-form-urlencoded">application/x-www-form-urlencoded</option>
          </select>
          <span :class="HINT">Fixes how placeholders are encoded: JSON string escaping, or form percent-encoding for <span class="font-mono">application/x-www-form-urlencoded</span>.</span></label>

        <div class="flex flex-col gap-[6px]">
          <label for="telephony-generic-headers" :class="LABEL">Headers<span v-if="headersUnreadable" class="ml-1 font-normal normal-case text-down" data-testid="generic-headers-stored">{{ UNREADABLE_LABEL }}</span><span v-else-if="headersStored" class="ml-1 font-normal normal-case text-up" data-testid="generic-headers-stored">✓ stored</span></label>
          <textarea
            id="telephony-generic-headers"
            v-model="form.generic.headers"
            rows="3"
            autocomplete="off"
            spellcheck="false"
            :disabled="form.generic.headers_clear || keyMissing"
            :placeholder="headersPlaceholder"
            :class="[INPUT, 'min-h-[60px]', headersError ? 'border-down' : '']"
            data-testid="generic-headers"
          />
          <span v-if="headersError" class="text-[12px] text-down" data-testid="generic-headers-error">{{ headersError }}</span>
          <template v-if="form.generic.headers_clear">
            <span class="flex flex-wrap items-center gap-2">
              <span class="rounded bg-degraded-weak px-[10px] py-[4px] text-[12px] text-ink-2" data-testid="generic-headers-removal"><b class="font-semibold text-degraded">Removed when you save.</b> The gateway will be called with no extra headers.</span>
              <button type="button" :class="BTN_SEC" data-testid="generic-headers-undo" @click="undoRemoveHeaders">Undo</button>
            </span>
          </template>
          <template v-else>
            <span :class="HINT">Headers can hold credentials, so they are never shown back. Leave empty to keep the stored set; type headers here to replace all of them.</span>
            <span v-if="headersStored" class="flex flex-wrap items-center gap-2">
              <button type="button" class="h-[26px] rounded-sm border border-down/50 px-[10px] text-[12px] text-down hover:bg-down-weak disabled:opacity-50" data-testid="generic-headers-remove" @click="removeHeaders">Remove stored headers</button>
              <span :class="HINT">for a gateway that no longer needs any</span>
            </span>
          </template>
        </div>

        <label class="flex flex-col gap-[6px]"><span :class="LABEL">Body template</span>
          <textarea v-model="form.generic.body_template" rows="3" spellcheck="false" placeholder='{"to": "{{to}}", "from": "{{from}}", "message": "{{text}}"}' :class="[INPUT, 'min-h-[60px]', templateError ? 'border-down' : '']" data-testid="generic-body-template" />
          <span v-if="templateError" class="text-[12px] text-down" data-testid="generic-template-error">{{ templateError }}</span>
          <span :class="HINT">Placeholders <span class="font-mono" v-text="'{{to}}'" />, <span class="font-mono" v-text="'{{from}}'" />, <span class="font-mono" v-text="'{{text}}'" /> are inserted as values, encoded for the content type. They must sit inside a JSON string or a form value.</span></label>

        <div class="grid grid-cols-2 gap-4 max-[560px]:grid-cols-1">
          <label class="flex flex-col gap-[6px]"><span :class="LABEL">From</span>
            <input v-model="form.generic.from" type="text" autocomplete="off" placeholder="CERBIX" :class="INPUT" data-testid="generic-from" /></label>
          <label class="flex flex-col gap-[6px]"><span :class="LABEL">Error code field<span class="ml-1 font-normal normal-case text-ink-3">(optional)</span></span>
            <input v-model="form.generic.response_code_field" type="text" autocomplete="off" placeholder="error_code" :class="INPUT" data-testid="generic-response-code-field" /></label>
        </div>
        <p :class="HINT">The top-level key of the gateway's JSON response that holds its own error code. cerbix keeps that code (letters, digits, <span class="font-mono">_ . -</span>, up to 32) and never the response text, which can quote the number. <b class="font-semibold text-ink-2">Without it, a refusal is recorded as its HTTP status alone</b> — a bad template and an empty balance then look the same. Any 2xx answer counts as sent: a gateway that reports failure inside a 200 is not supported, and a redirect is treated as a refusal.</p>
      </template>

      <!-- Messages (1b, 1h) -->
      <div :class="SUBHEAD">Messages</div>
      <div class="grid grid-cols-[2fr_1fr] gap-4 max-[560px]:grid-cols-1">
        <label class="flex flex-col gap-[6px]"><span :class="LABEL">Public base URL<span class="ml-1 font-normal normal-case text-ink-3">(for the incident link)</span></span>
          <input v-model="form.public_base_url" type="text" autocomplete="off" :placeholder="mailHint || 'https://cerbix.example.com'" :class="INPUT" data-testid="telephony-public-base-url" />
          <span :class="HINT" data-testid="telephony-public-url-hint"><template v-if="mailHint">Empty: links use <span class="font-mono">{{ mailHint }}</span> from Email settings. Set it here when the phone link must differ. </template><span class="font-mono">https</span> unless the host is loopback.</span></label>
        <label class="flex flex-col gap-[6px]"><span :class="LABEL">Voice language</span>
          <input v-model="form.voice_language" type="text" autocomplete="off" placeholder="en-US" :class="INPUT" data-testid="telephony-voice-language" />
          <span :class="HINT">BCP-47 tag spoken in calls. A person can choose their own under Account → Phone.</span></label>
      </div>
      <div v-if="noLinkAnywhere" class="rounded bg-degraded-weak px-[13px] py-[10px] text-[12.5px] leading-[1.5] text-ink-2" data-testid="telephony-no-link-warning">
        <b class="font-semibold text-degraded">Messages will carry no link.</b> Neither this field nor Email → Public base URL is set. Messages still name the incident by its short reference (<span class="font-mono">incident X7K2P9QM</span>), and each delivery records <span class="font-mono">no_public_url</span>.
      </div>
      <div class="grid grid-cols-[2fr_1fr] gap-4 max-[560px]:grid-cols-1">
        <div class="flex flex-col gap-[6px]">
          <span :class="LABEL">Names in SMS</span>
          <label class="inline-flex cursor-pointer items-start gap-[8px] text-[13px]">
            <input v-model="form.sms_encoding" type="radio" name="telephony-encoding" value="transliterate" class="mt-[3px] accent-accent" data-testid="telephony-encoding-transliterate" />
            <span>Transliterate to Latin<span class="block text-[11.5px] text-ink-3">Names become ASCII; a message stays at 160 characters a segment. Characters with no Latin form become “?”.</span></span>
          </label>
          <label class="inline-flex cursor-pointer items-start gap-[8px] text-[13px]">
            <input v-model="form.sms_encoding" type="radio" name="telephony-encoding" value="unicode" class="mt-[3px] accent-accent" data-testid="telephony-encoding-unicode" />
            <span>Send as written (Unicode)<span class="block text-[11.5px] text-ink-3">Names keep their script; the whole message then costs 70 characters a segment.</span></span>
          </label>
        </div>
        <label class="flex flex-col gap-[6px]"><span :class="LABEL">Longest SMS</span>
          <select v-model.number="form.sms_max_segments" :class="SELECT" data-testid="telephony-max-segments">
            <option v-for="n in [1, 2, 3, 4]" :key="n" :value="n">{{ n }} {{ n === 1 ? "segment" : "segments" }}</option>
          </select>
          <span :class="HINT">1–4. Longer names are shortened to fit; the link is never cut.</span></label>
      </div>

      <!-- Storm limits -->
      <div :class="SUBHEAD">Storm limits</div>
      <p class="-mt-2 text-[12.5px] leading-[1.5] text-ink-3">A delivery past a limit is not sent; it is recorded on the incident's Phone panel and in the channel's deliveries as “Storm limit”.</p>
      <div class="grid grid-cols-2 gap-4 max-[560px]:grid-cols-1">
        <label class="flex flex-col gap-[6px]"><span :class="LABEL">Per number, per incident, per hour</span>
          <input v-model.number="form.storm.per_incident_per_hour" type="number" min="1" :class="INPUT" data-testid="telephony-storm-incident" /></label>
        <label class="flex flex-col gap-[6px]"><span :class="LABEL">Per number, per hour</span>
          <input v-model.number="form.storm.per_number_per_hour" type="number" min="1" :class="INPUT" data-testid="telephony-storm-number" /></label>
      </div>
    </fieldset>

    <!-- Save (1a, 1b, 1c″, 1d, 1e) -->
    <div class="flex flex-wrap items-center gap-3 border-t border-border pt-[14px]">
      <button type="button" :disabled="keyMissing || saving" class="h-[36px] rounded-sm bg-accent px-[16px] text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50" data-testid="telephony-save" @click="save">{{ saving ? "Saving…" : "Save phone settings" }}</button>
      <span v-if="saved" class="text-[12.5px] text-up" data-testid="telephony-saved">✓ Saved</span>
      <span v-else-if="keyMissing" class="text-[12.5px] text-ink-3" data-testid="telephony-save-note">Needs the encryption key.</span>
      <span v-else-if="saveNote" class="min-w-0 flex-1 text-[12.5px] text-down" data-testid="telephony-save-note"><b v-if="saveNoteIsKey" class="font-semibold">{{ KEY_MISSING_LEAD }}</b> {{ saveNoteIsKey ? KEY_MISSING_REST : saveNote }}</span>
      <span v-else-if="!view?.configured && !unroutable" class="text-[12.5px] text-ink-3" data-testid="telephony-save-note">Nothing is stored until you save.</span>
    </div>

    <!-- 1f / 1g: the save's own 409 -->
    <template v-if="unroutable">
      <div class="rounded bg-degraded-weak px-[13px] py-3 text-[12.5px] leading-[1.5] text-ink-2" data-testid="telephony-unroutable">
        <b class="font-semibold text-degraded">{{ unroutableHead }}</b><template v-if="unroutableText"> {{ unroutableText }}</template>
        <div v-for="(g, gi) in unroutableGroups" :key="`${gi}/${g.project}`" class="mt-2 flex flex-wrap items-center gap-[6px]" data-testid="telephony-unroutable-group">
          <span v-for="(s, si) in g.services" :key="`${si}/${s.slug}`" class="inline-flex items-center gap-[5px] rounded-[5px] border border-border bg-surface px-[7px] py-[2px] text-[11.5px] text-ink-2" :title="s.service"><span class="text-ink-3">service</span><span class="font-mono">{{ s.slug }}</span></span>
          <span class="text-ink-3">· project <b class="font-medium text-ink-2">{{ g.project }}</b></span>
        </div>
        <div v-if="unroutableMore > 0" class="mt-2 font-medium text-ink-2" data-testid="telephony-unroutable-more">… and {{ unroutableMore }} more.</div>
      </div>
      <div class="flex flex-wrap items-center gap-2">
        <button
          type="button"
          :disabled="saving"
          class="h-[34px] rounded-sm px-[14px] text-[13px] font-medium disabled:opacity-50"
          :class="unroutableButtons.danger ? 'bg-down text-white hover:opacity-90' : 'bg-accent text-accent-ink hover:bg-accent-2'"
          data-testid="telephony-unroutable-confirm"
          @click="confirmUnroutable"
        >{{ unroutableButtons.confirm }}</button>
        <button type="button" :disabled="saving" class="h-[34px] rounded-sm border border-border px-[14px] text-[13px] text-ink-2 hover:border-border-strong disabled:opacity-50" data-testid="telephony-unroutable-keep" @click="keepUnroutable">{{ unroutableButtons.keep }}</button>
      </div>
    </template>

    <!-- 1i: Test -->
    <div :class="SUBHEAD">Test</div>
    <div class="flex flex-wrap items-center gap-3" data-testid="telephony-test">
      <button type="button" :disabled="testState !== 'ready'" :class="BTN_SEC" data-testid="telephony-test-send" @click="sendTest">{{ testLabel }}</button>
      <span v-if="testState === 'no_number'" class="text-[12.5px] text-ink-3" data-testid="telephony-test-note">{{ TEST_NO_NUMBER }} <a href="/settings?tab=phone" class="text-accent hover:underline" data-testid="telephony-test-phone-link" @click.prevent="openPhoneTab">Add one under Account → Phone</a>.</span>
      <span v-else-if="testState === 'unsaved'" class="text-[12.5px] text-ink-3" data-testid="telephony-test-note">{{ TEST_UNSAVED }}</span>
      <span v-else-if="testState === 'off'" class="text-[12.5px] text-ink-3" data-testid="telephony-test-note">{{ TEST_OFF }}</span>
      <span v-else-if="testState === 'ready' && testResult" class="text-[12.5px]" :class="testResult.tone === 'ok' ? 'text-up' : 'text-down'" data-testid="telephony-test-result">{{ testResult.lead }}<template v-if="testResult.status"> · <span class="font-mono">{{ testResult.status }}</span></template><template v-if="testResult.transportClass"> (<span class="font-mono">{{ testResult.transportClass }}</span>)</template>. {{ testResult.advice }}</span>
      <span v-else-if="testState === 'ready' && testError" class="text-[12.5px] text-down" data-testid="telephony-test-error">{{ testError }}</span>
      <span v-else-if="testState === 'ready'" class="text-[12.5px] text-ink-3" data-testid="telephony-test-note">{{ TEST_READY }}</span>
    </div>
  </section>
</template>
