<script setup lang="ts">
// FR-041a §4.2 — Settings → Account → Phone (mock §2: 2a–2j, 2j′). Every state is drawn from the
// session's copy of GET /api/v1/me (`phone`, `telephony_enabled`, `telephony_voice_language`); the
// panel makes no request on mount, and writes each answer back with `session.setPhone`.
//
// Rules that shape it:
// - No number in full, ever (D35, P3). The page only has `number_masked`. A typed number lives in
//   this component's `typed` ref, leaves only in a PUT body, and is cleared once it is stored, on
//   "Use another number" / Cancel, and on unmount — never a store, a URL or storage.
// - "Send a new code" is the body-less POST /me/phone/verify/send (§14 SPA point 7): it works after a
//   reload, when nothing typed is held, so the typed number is never kept for it.
// - The code states 2c–2e are the mock's sentences (`codeAnswer` in lib/phone.ts, shared with a number
//   channel's verification). `send_limited` (2f) and "switched off meanwhile" (`telephony_disabled`)
//   read the server's sentence, the mock's line only when it sent none — one rule for
//   `telephony_disabled` on every phone surface. Every answer the mock has no text for reads the
//   server's `error` under the action that failed, no new layout (point 9).
// - Revision 9, the code by a call: beside every send of a code (Send code, Use another number's
//   send, Send code to the new number, Send a new code) is "Call me with the code" — the same
//   request with `via: "voice"` — offered only while /me says `telephony_can_call`. There is no
//   fallback between the two ways: a refused call reads the server's sentence and nothing else is
//   sent. The step and code lines follow the way the last code went (`session.phoneCodeSend`, in
//   memory, so a tab switch keeps it); when the page does not know it (a reload while calls were
//   possible at mount) they name neither way.
// - A provider error on a send (502 `provider_rejected` / `provider_unreachable`): the backend has
//   stored a new code, ending the previous one, before it called the provider — so no running code
//   reached the phone. Step 1 is the mock's own "not sent" state ("1 · Send a code to <mask>"), the
//   server's sentence is under the action, and the code lines point at no particular message.
// - Remove (2i) is offered for any stored number: verified (2g, 2j′) and, by the owner's decision of
//   2026-11-18 (a deviation from the mock), unverified (2b) with phone alerts on or off — the same
//   DELETE, confirmation and wording. It ends the running code and forgets the way it went.
// - Quiet hours belong to FR-041c and are not drawn.
import { computed, onBeforeUnmount, ref, watch } from "vue";
import { api } from "@/api/client";
import {
  CODE_SEND_FAILED as SEND_FAILED,
  CODE_SEND_LIMITED as SEND_LIMITED,
  CODE_VERIFY_FAILED as VERIFY_FAILED,
  type CodeVia,
  type PhoneCodeSend,
  VOICE_LANGUAGES,
  codeAnswer,
  phoneErrorMessage,
  phoneErrorReason,
} from "@/lib/phone";
import { useSession } from "@/stores/session";

const emit = defineEmits<{ (e: "open-tab", key: string): void }>();

const session = useSession();

// ── The mock's sentences ────────────────────────────────────────────────────────────────────────
const OFF_SENTENCE = "Phone alerts are not enabled on this instance, so a number cannot be confirmed now.";
const OFF_TAIL = "An administrator turns them on under Settings → Administration → Telephony.";
const SWITCHED_OFF =
  "No code sent: phone alerts were switched off on this instance. An administrator turns them on under Settings → Administration → Telephony.";
// Fallbacks for an answer that carries no `error` sentence (a bare 501, a dropped connection); the
// code-state lines and the send/verify fallbacks are lib/phone.ts's.
const REMOVE_FAILED = "The number could not be removed.";
const LANGUAGE_FAILED = "The language could not be saved.";

// ── State ───────────────────────────────────────────────────────────────────────────────────────
/** What the person is doing beside the state /me gives: replacing (2h), removing (2i, from 2b or 2g), or entering another number from 2b. */
type Mode = "idle" | "replace" | "remove" | "another";
const mode = ref<Mode>("idle");
/** The number being typed — component-local only (P3). */
const typed = ref("");
const code = ref("");
const busy = ref(false);
/** The line under a code send (2a's Send code, 2b's Send a new code, 2h's Send code to the new number). */
const sendError = ref("");
/** The line under the code field for a Verify answer (2c–2e, or the server's sentence). */
const codeError = ref("");
const codeIncorrect = ref(false);
/** The running code is spent (2d, 2e): Verify waits for a new one. */
const codeDead = ref(false);
const removeError = ref("");
const languageError = ref("");
const languageSaved = ref(false);
const languageBusy = ref(false);

const phone = computed(() => session.phone);
const telephonyOn = computed(() => session.telephonyEnabled);
/** Revision 9: the instance can speak a code by a call — the call actions are offered only then. */
const canCall = computed(() => session.telephonyCanCall === true);
/** What the last send from this page did; null until one is sent in this page's lifetime. */
const lastSend = computed<PhoneCodeSend | null>(() => session.phoneCodeSend ?? null);
/** The last send reached no phone (a 502): step 1 is "not sent". */
const undelivered = computed(() => lastSend.value === "undelivered");
/**
 * Whether a call could be made when the page was opened. With no send known, a code can only have
 * gone by SMS when no call was possible then; a later /me that turns calls off says nothing about
 * the code already running (review m-1), so this is read once.
 */
const couldCallAtMount = session.telephonyCanCall === true;
/** The way of the send in flight, for its button's label. */
const busyVia = ref<CodeVia | null>(null);
/** The way the running code went: the last send, else SMS when no call was possible, else unknown. */
const runningVia = computed<CodeVia | null>(() => {
  const s = lastSend.value;
  if (s === "undelivered") return null;
  return s ?? (couldCallAtMount ? null : "sms");
});
const stepSentLine = computed(() =>
  runningVia.value === "voice" ? "✓ 1 · Code spoken by a call" : runningVia.value === "sms" ? "✓ 1 · Code sent by SMS" : "✓ 1 · Code sent",
);

/** The language select's value: "" is the instance default (`language: null`). */
const language = ref(session.phone?.language ?? "");
watch(
  () => session.phone?.language,
  (l) => {
    language.value = l ?? "";
  },
);

const instanceDefaultLabel = computed(() =>
  session.telephonyVoiceLanguage ? `Instance default (${session.telephonyVoiceLanguage})` : "Instance default",
);
/** The fixed list, plus the stored tag when it is not on it. */
const languageOptions = computed(() => {
  const stored = session.phone?.language;
  return stored && !VOICE_LANGUAGES.includes(stored) ? [...VOICE_LANGUAGES, stored] : [...VOICE_LANGUAGES];
});

function clearLines() {
  sendError.value = "";
  codeError.value = "";
  codeIncorrect.value = false;
  removeError.value = "";
  languageSaved.value = false;
  languageError.value = "";
}

// A state change from elsewhere (a re-read of /me) ends a sub-step that no longer applies.
watch(
  () => [session.phone?.verified, session.phone === null] as const,
  () => {
    if (mode.value === "another" && session.phone?.verified !== false) mode.value = "idle";
    if (mode.value === "replace" && session.phone?.verified !== true) mode.value = "idle";
    if (mode.value === "remove" && !session.phone) mode.value = "idle";
  },
);
watch(telephonyOn, (on) => {
  if (!on && (mode.value === "replace" || mode.value === "another")) {
    mode.value = "idle";
    typed.value = "";
  }
});

onBeforeUnmount(() => {
  typed.value = "";
  code.value = "";
});

/** A code send refused: the line under it, and a re-read of /me when the answer changes the state. */
async function sendRefused(error: unknown) {
  const reason = phoneErrorReason(error);
  if (reason === "telephony_disabled") {
    sendError.value = phoneErrorMessage(error, SWITCHED_OFF);
    typed.value = "";
    await session.fetchMe();
    return;
  }
  sendError.value = reason === "send_limited" ? phoneErrorMessage(error, SEND_LIMITED) : phoneErrorMessage(error, SEND_FAILED);
  // A call refused while /me said one could be made: the page's state is stale, so re-read /me
  // (the call action goes when it says so). Nothing was stored; the typed number stays.
  if (reason === "voice_unsupported_by_provider") {
    await session.fetchMe();
    return;
  }
  // The number is stored unverified though the code was not sent (502), or the stored state is not
  // what this page holds (409 no_number / number_already_verified): re-read /me.
  if (reason === "provider_rejected" || reason === "provider_unreachable") {
    typed.value = "";
    mode.value = "idle";
    await session.fetchMe();
    // After the re-read: it may bring the number just stored, which drops the previous record.
    session.setPhoneCodeSend("undelivered");
  } else if (reason === "no_number" || reason === "number_already_verified") {
    await session.fetchMe();
  }
}

// ── 2a / 2h / Use another number: PUT /me/phone — by SMS ({number}) or by a call (+ via) ────────
async function sendCode(via: CodeVia) {
  if (busy.value || !typed.value.trim()) return;
  busy.value = true;
  busyVia.value = via;
  clearLines();
  try {
    const body = via === "voice" ? { number: typed.value, via } : { number: typed.value };
    const res = await api.PUT("/api/v1/me/phone", { body });
    if (res.error || !res.data) {
      await sendRefused(res.error);
      return;
    }
    typed.value = "";
    code.value = "";
    codeDead.value = false;
    mode.value = "idle";
    // The number first: a new number drops the old record, then this send is recorded.
    session.setPhone(res.data);
    session.setPhoneCodeSend(via);
  } catch {
    sendError.value = SEND_FAILED;
  } finally {
    busy.value = false;
    busyVia.value = null;
  }
}

// ── 2b: Send a new code (POST /me/phone/verify/send — no body by SMS, {via} by a call) ──────────
async function resendCode(via: CodeVia) {
  if (busy.value) return;
  busy.value = true;
  busyVia.value = via;
  clearLines();
  try {
    const res =
      via === "voice"
        ? await api.POST("/api/v1/me/phone/verify/send", { body: { via } })
        : await api.POST("/api/v1/me/phone/verify/send");
    if (res.error || !res.response.ok) {
      await sendRefused(res.error);
      return;
    }
    code.value = "";
    codeDead.value = false;
    session.setPhoneCodeSend(via);
  } catch {
    sendError.value = SEND_FAILED;
  } finally {
    busy.value = false;
    busyVia.value = null;
  }
}

// ── 2b: Verify (POST /me/phone/verify) ──────────────────────────────────────────────────────────
async function verify() {
  if (busy.value || codeDead.value || !code.value.trim()) return;
  busy.value = true;
  clearLines();
  try {
    const res = await api.POST("/api/v1/me/phone/verify", { body: { code: code.value.trim() } });
    if (res.error || !res.data) {
      const answer = codeAnswer(phoneErrorReason(res.error), runningVia.value);
      if (answer) {
        codeError.value = answer.line;
        codeIncorrect.value = answer.incorrect;
        codeDead.value = answer.dead;
      } else {
        codeError.value = phoneErrorMessage(res.error, VERIFY_FAILED);
      }
      return;
    }
    code.value = "";
    session.setPhone(res.data);
  } catch {
    codeError.value = VERIFY_FAILED;
  } finally {
    busy.value = false;
  }
}

function useAnotherNumber() {
  clearLines();
  typed.value = "";
  mode.value = "another";
}
function startReplace() {
  clearLines();
  typed.value = "";
  mode.value = "replace";
}
function startRemove() {
  clearLines();
  mode.value = "remove";
}
function cancel() {
  clearLines();
  typed.value = "";
  mode.value = "idle";
}

// ── 2i: Remove (DELETE /me/phone) — from 2g/2j′ and from 2b ────────────────────────────────────
async function removeNumber() {
  if (busy.value) return;
  busy.value = true;
  removeError.value = "";
  try {
    const res = await api.DELETE("/api/v1/me/phone");
    if (res.error || !res.response.ok) {
      removeError.value = phoneErrorMessage(res.error, REMOVE_FAILED);
      return;
    }
    mode.value = "idle";
    // The running code goes with the number: its field, its answers, and the way it went.
    code.value = "";
    codeDead.value = false;
    codeIncorrect.value = false;
    codeError.value = "";
    sendError.value = "";
    session.setPhone(null);
    session.setPhoneCodeSend(null);
  } catch {
    removeError.value = REMOVE_FAILED;
  } finally {
    busy.value = false;
  }
}

// ── 2g: Voice language (PATCH /me/phone/preferences) — saved as soon as it is picked ────────────
async function saveLanguage() {
  if (languageBusy.value) return;
  const chosen = language.value === "" ? null : language.value;
  languageBusy.value = true;
  languageSaved.value = false;
  languageError.value = "";
  try {
    const res = await api.PATCH("/api/v1/me/phone/preferences", { body: { language: chosen } });
    if (res.error || !res.data) {
      languageError.value = phoneErrorMessage(res.error, LANGUAGE_FAILED);
      language.value = session.phone?.language ?? "";
      return;
    }
    session.setPhone(res.data);
    languageSaved.value = true;
  } catch {
    languageError.value = LANGUAGE_FAILED;
    language.value = session.phone?.language ?? "";
  } finally {
    languageBusy.value = false;
  }
}

function openTelephony() {
  emit("open-tab", "telephony");
}

// ── Classes (the Security tab's and the Email card's) ───────────────────────────────────────────
const LABEL = "text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3";
const INPUT =
  "h-10 max-w-[320px] rounded-sm border border-border bg-surface-2 px-3 font-mono text-[13px] outline-none focus:border-accent disabled:opacity-55";
const HINT = "text-[11.5px] leading-[1.45] text-ink-3";
const BTN_PRI =
  "h-[32px] rounded-sm bg-accent px-[13px] text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50";
const BTN_SEC =
  "h-[32px] rounded-sm border border-border px-[13px] text-[13px] text-ink-2 hover:border-border-strong disabled:opacity-50";
const BTN_DANGERLINE =
  "h-[32px] rounded-sm border border-border px-[13px] text-[13px] text-down hover:border-down/50 disabled:opacity-50";
const BTN_DANGER = "h-[32px] rounded-sm bg-down px-[13px] text-[13px] font-medium text-white hover:opacity-90 disabled:opacity-50";
const QUIET = "rounded border border-border bg-surface-2 px-3 py-[9px] text-[12.5px] leading-[1.55] text-ink-2";
const WARN = "rounded border border-degraded/40 bg-degraded-weak px-3 py-[9px] text-[12.5px] leading-[1.55] text-ink-2";
const PILL = "inline-flex items-center rounded-full px-[10px] py-[3px] text-[11.5px] font-medium";
const ERR = "text-[12.5px] leading-[1.5] text-down";
</script>

<template>
  <section class="flex max-w-[640px] flex-col gap-4 rounded border border-border bg-surface p-5 shadow-card" data-testid="phone-panel">
    <h2 class="text-[13px] font-semibold" data-testid="phone-title">Phone for alerts</h2>

    <!-- 2a / 2j: no number -->
    <template v-if="!phone">
      <p v-if="telephonyOn" class="-mt-2 text-[12.5px] leading-[1.5] text-ink-3" data-testid="phone-explanation">Phone channels can text or call you at this number. cerbix sends nothing to it until you confirm it with a code.</p>
      <div v-else :class="QUIET" data-testid="phone-off-notice">
        {{ OFF_SENTENCE }}
        <template v-if="session.isGlobalAdmin"><a href="/settings?tab=telephony" class="text-accent hover:underline" data-testid="phone-off-link" @click.prevent="openTelephony">Turn them on under Administration → Telephony</a>.</template>
        <template v-else>{{ OFF_TAIL }}</template>
      </div>
      <label class="flex flex-col gap-[6px]"><span :class="LABEL">Mobile number</span>
        <input v-model="typed" type="tel" autocomplete="off" placeholder="+90 532 000 0000" :disabled="!telephonyOn" :class="INPUT" data-testid="phone-number-input" @keyup.enter="sendCode('sms')" />
        <span v-if="telephonyOn" :class="HINT" data-testid="phone-number-hint">International format with the country code (E.164).</span></label>
      <div class="flex flex-wrap items-center gap-2">
        <button type="button" :disabled="!telephonyOn || busy || !typed.trim()" :class="BTN_PRI" data-testid="phone-send" @click="sendCode('sms')">{{ busyVia === "sms" ? "Sending…" : "Send code" }}</button>
        <button v-if="canCall" type="button" :disabled="!telephonyOn || busy || !typed.trim()" :class="BTN_SEC" data-testid="phone-send-call" @click="sendCode('voice')">{{ busyVia === "voice" ? "Calling…" : "Call me with the code" }}</button>
      </div>
      <div v-if="sendError" :class="ERR" data-testid="phone-send-error">{{ sendError }}</div>
    </template>

    <!-- A stored number, verified or not -->
    <template v-else>
      <div class="flex flex-wrap items-center gap-[10px]">
        <span class="font-mono text-[13px]" data-testid="phone-masked">{{ phone.number_masked }}</span>
        <span :class="[PILL, phone.verified ? 'bg-up-weak text-up' : 'bg-degraded-weak text-degraded']" data-testid="phone-pill">{{ phone.verified ? "Verified" : "Not verified" }}</span>
      </div>

      <!-- 2i: removing the number (from 2g/2j′, and from 2b by the owner's decision of 2026-11-18) -->
      <template v-if="mode === 'remove'">
        <div :class="WARN" data-testid="phone-remove-warning"><b class="font-semibold text-degraded">Remove this number?</b> Phone channels that alert you stay, but they are not routes until you add and confirm a number again.</div>
        <div class="flex flex-wrap items-center gap-2">
          <button type="button" :disabled="busy" :class="BTN_DANGER" data-testid="phone-remove-confirm" @click="removeNumber">{{ busy ? "Removing…" : "Remove number" }}</button>
          <button type="button" :disabled="busy" :class="BTN_SEC" data-testid="phone-remove-cancel" @click="cancel">Cancel</button>
        </div>
        <div v-if="removeError" :class="ERR" data-testid="phone-remove-error">{{ removeError }}</div>
      </template>

      <!-- 2b: stored unverified (also what a reload shows) -->
      <template v-else-if="!phone.verified">
        <div v-if="!telephonyOn" :class="QUIET" data-testid="phone-off-notice">
          {{ OFF_SENTENCE }}
          <template v-if="session.isGlobalAdmin"><a href="/settings?tab=telephony" class="text-accent hover:underline" data-testid="phone-off-link" @click.prevent="openTelephony">Turn them on under Administration → Telephony</a>.</template>
          <template v-else>{{ OFF_TAIL }}</template>
        </div>

        <!-- Use another number: the 2a field again -->
        <template v-if="mode === 'another'">
          <label class="flex flex-col gap-[6px]"><span :class="LABEL">Mobile number</span>
            <input v-model="typed" type="tel" autocomplete="off" placeholder="+90 532 000 0000" :class="INPUT" data-testid="phone-number-input" @keyup.enter="sendCode('sms')" />
            <span :class="HINT" data-testid="phone-number-hint">International format with the country code (E.164).</span></label>
          <div class="flex flex-wrap items-center gap-2">
            <button type="button" :disabled="busy || !typed.trim()" :class="BTN_PRI" data-testid="phone-send" @click="sendCode('sms')">{{ busyVia === "sms" ? "Sending…" : "Send code" }}</button>
            <button v-if="canCall" type="button" :disabled="busy || !typed.trim()" :class="BTN_SEC" data-testid="phone-send-call" @click="sendCode('voice')">{{ busyVia === "voice" ? "Calling…" : "Call me with the code" }}</button>
            <button type="button" :disabled="busy" :class="BTN_SEC" data-testid="phone-another-cancel" @click="cancel">Cancel</button>
          </div>
          <div v-if="sendError" :class="ERR" data-testid="phone-send-error">{{ sendError }}</div>
        </template>

        <template v-else>
          <div class="flex flex-col gap-3">
            <div data-testid="phone-step-1">
              <div v-if="undelivered" class="text-[12px] font-semibold text-ink-2">1 · Send a code to {{ phone.number_masked }}</div>
              <template v-else>
                <div class="text-[12px] font-semibold text-up">{{ stepSentLine }}</div>
                <p :class="[HINT, 'mt-[2px]']">It works for 10 minutes. The number is shown masked, here as everywhere.</p>
              </template>
            </div>
            <div data-testid="phone-step-2">
              <div class="text-[12px] font-semibold text-ink-2">2 · Enter the 6-digit code</div>
              <input
                v-model="code"
                inputmode="numeric"
                autocomplete="one-time-code"
                placeholder="123456"
                :disabled="!telephonyOn"
                class="mt-[6px] h-10 w-[180px] rounded-sm border bg-surface-2 px-3 font-mono text-[15px] tracking-[0.3em] outline-none focus:border-accent disabled:opacity-55"
                :class="codeIncorrect ? 'border-down' : 'border-border'"
                data-testid="phone-code-input"
                @input="codeIncorrect = false"
                @keyup.enter="verify"
              />
              <div v-if="codeError" :class="[ERR, 'mt-[6px]']" data-testid="phone-code-error">{{ codeError }}</div>
              <div v-if="sendError" :class="[ERR, 'mt-[6px]']" data-testid="phone-send-error">{{ sendError }}</div>
            </div>
          </div>
          <div class="flex flex-wrap items-center gap-2">
            <button type="button" :disabled="!telephonyOn || busy || codeDead || !code.trim()" :class="BTN_PRI" data-testid="phone-verify" @click="verify">{{ busy && busyVia === null ? "Verifying…" : "Verify" }}</button>
            <button type="button" :disabled="!telephonyOn || busy" :class="BTN_SEC" data-testid="phone-resend" @click="resendCode('sms')">{{ busyVia === "sms" ? "Sending…" : "Send a new code" }}</button>
            <button v-if="canCall" type="button" :disabled="!telephonyOn || busy" :class="BTN_SEC" data-testid="phone-resend-call" @click="resendCode('voice')">{{ busyVia === "voice" ? "Calling…" : "Call me with a new code" }}</button>
            <button type="button" :disabled="!telephonyOn || busy" class="text-[12.5px] text-accent hover:underline disabled:opacity-50 disabled:no-underline" data-testid="phone-use-another" @click="useAnotherNumber">Use another number</button>
            <button type="button" :disabled="busy" :class="BTN_DANGERLINE" data-testid="phone-remove" @click="startRemove">Remove</button>
          </div>
        </template>
      </template>

      <!-- 2g / 2h / 2j′: verified -->
      <template v-else>
        <!-- 2h: replacing the number -->
        <template v-if="mode === 'replace' && telephonyOn">
          <label class="flex flex-col gap-[6px]"><span :class="LABEL">New mobile number</span>
            <input v-model="typed" type="tel" autocomplete="off" placeholder="+90 532 000 0000" :class="INPUT" data-testid="phone-replace-input" @keyup.enter="sendCode('sms')" /></label>
          <div :class="WARN" data-testid="phone-replace-warning"><b class="font-semibold text-degraded">Your current number stops working for alerts now.</b> Until you confirm the new one, phone channels that alert you are not routes, and nothing is sent to either number.</div>
          <div class="flex flex-wrap items-center gap-2">
            <button type="button" :disabled="busy || !typed.trim()" :class="BTN_PRI" data-testid="phone-replace-send" @click="sendCode('sms')">{{ busyVia === "sms" ? "Sending…" : "Send code to the new number" }}</button>
            <button v-if="canCall" type="button" :disabled="busy || !typed.trim()" :class="BTN_SEC" data-testid="phone-replace-send-call" @click="sendCode('voice')">{{ busyVia === "voice" ? "Calling…" : "Call me with the code" }}</button>
            <button type="button" :disabled="busy" :class="BTN_SEC" data-testid="phone-replace-cancel" @click="cancel">Cancel</button>
          </div>
          <div v-if="sendError" :class="ERR" data-testid="phone-send-error">{{ sendError }}</div>
        </template>

        <!-- 2g / 2j′ -->
        <template v-else>
          <div v-if="!telephonyOn" :class="QUIET" data-testid="phone-off-kept">Phone alerts are switched off on this instance, so nothing is sent to this number for now. It stays verified.</div>
          <div class="flex flex-wrap items-center gap-2">
            <button
              type="button"
              :disabled="!telephonyOn"
              :title="telephonyOn ? undefined : 'Replacing needs a code, and no SMS can be sent'"
              :class="BTN_SEC"
              data-testid="phone-replace"
              @click="startReplace"
            >Replace number</button>
            <button type="button" :class="BTN_DANGERLINE" data-testid="phone-remove" @click="startRemove">Remove</button>
          </div>
          <p v-if="!telephonyOn" :class="HINT" data-testid="phone-replace-reason">Replacing needs a code, and no SMS can be sent, so Replace is disabled. Remove and the language work as usual.</p>

          <label class="flex flex-col gap-[6px]"><span :class="LABEL">Voice language</span>
            <select
              v-model="language"
              :disabled="languageBusy"
              class="h-10 max-w-[320px] rounded-sm border border-border bg-surface-2 px-3 text-[13px] outline-none focus:border-accent disabled:opacity-55"
              data-testid="phone-language"
              @change="saveLanguage"
            >
              <option value="">{{ instanceDefaultLabel }}</option>
              <option v-for="tag in languageOptions" :key="tag" :value="tag">{{ tag }}</option>
            </select>
            <span :class="HINT" data-testid="phone-language-hint">The language cerbix speaks when it calls you. SMS text is always in English. Saved as soon as you pick it.</span></label>
          <span v-if="languageSaved" class="text-[12.5px] text-up" data-testid="phone-language-saved">✓ Saved</span>
          <div v-if="languageError" :class="ERR" data-testid="phone-language-error">{{ languageError }}</div>
        </template>
      </template>
    </template>
  </section>
</template>
