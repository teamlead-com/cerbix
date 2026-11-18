<script setup lang="ts">
// FR-041a §4.2/§4.3, D28 — the part of Settings → Notification channels' form that replaces the
// config fields for an SMS or Voice call channel (mock 3a, 3a′, 3b-1..3, 3c, 3d, 3f′). The view
// keeps the Type select and the Name field above it; this component owns the recipient, its own
// Create / Save, and the fixed-number verification steps.
//
// Rules that shape it:
// - A phone channel's fields are typed (§4.3): no `config` ever leaves this form, and `enabled` is
//   left to the server — a person channel takes its default (on), a number channel is created
//   disabled whatever the request says (§4.2).
// - D28: a project admin names any eligible recipient, or a fixed number; an editor only themselves
//   (the recipients endpoint returns only them) and never a fixed number. An editor edits only a
//   person channel that phones themselves (`canHoldPhoneChannel`); the view offers no Edit on the
//   others, and should the form be reached anyway it offers no Save. A viewer never sees the form.
//   The predicates are the session store's; no role string is compared here.
// - The fixed-number flow (mock decision 11 and 12, "Accepted as drawn"): after Create the form
//   stays open on the new channel; nothing is sent until "Send code"; a confirmed number does not
//   enable the channel by itself ("Enable channel" / "Keep disabled").
// - No number in full, ever (D35, P3): the typed number lives in `typed`, leaves only in the request
//   body, and is cleared once stored and on unmount. The page shows the server's `number_masked`.
// - Every refusal reads the server's `error` sentence under the action that failed (§14 point 9);
//   the mock's 3f / 3f′ lines are the fallbacks for an answer that carries none — the same rule as
//   Account → Phone for `telephony_disabled`. The code states 2c–2f are mock §2's, shared through
//   `codeAnswer` in lib/phone.ts, for the way the last code went.
// - Revision 9, the code by a call: beside Send code is "Call with the code" — the same verify/send
//   with `via: "voice"` — offered only while /me says `telephony_can_call`. No fallback between the
//   two ways: a refused call reads the server's sentence and nothing else is sent.
// - A provider error on a send (502 `provider_rejected` / `provider_unreachable`): the backend has
//   stored a new code, ending the previous one, before it called the provider — so no running code
//   reached the phone. The steps go back to the mock's "1 · Send a code to <mask>" (`codeSent`
//   false), with the server's sentence under the action and no line about an earlier code.
//
// The row's "Verify" (mock 3e, 3b-2 note: "the row offers Verify again") opens this form with
// `verify` on an existing number channel: it starts at 3b-2 on that channel, with the same steps.
//
// Emits: `created` (a channel now exists — add its row), `saved` (a row changed — replace it),
// `cancel` (close the form). A person channel emits `created` then `cancel`; a number channel emits
// `created` and stays open.
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { api } from "@/api/client";
import type { components } from "@/api/schema";
import {
  CODE_SEND_FAILED as SEND_FAILED,
  CODE_SEND_LIMITED,
  CODE_VERIFY_FAILED as VERIFY_FAILED,
  type CodeVia,
  type PhoneChannelType,
  canHoldPhoneChannel,
  codeAnswer,
  phoneErrorMessage,
  phoneErrorReason,
} from "@/lib/phone";
import { useSession } from "@/stores/session";

type Channel = components["schemas"]["NotificationChannel"];
type CreateBody = components["schemas"]["CreateNotificationChannel"];
type UpdateBody = components["schemas"]["UpdateNotificationChannel"];
type Recipient = components["schemas"]["PhoneRecipient"];

const props = defineProps<{
  orgId: string;
  projectId: string;
  type: PhoneChannelType;
  /** The Name field of the view's form. */
  name: string;
  /** The channel being edited (3d); null while creating. */
  channel: Channel | null;
  /** Open on the 3b-2 steps of an existing number channel — the row's "Verify" (mock 3e). */
  verify?: boolean;
}>();
const emit = defineEmits<{ created: [ch: Channel]; saved: [ch: Channel]; cancel: [] }>();

const session = useSession();

// ── The mock's sentences ────────────────────────────────────────────────────────────────────────
// Fallbacks only: the server's `error` is shown when it sent one (§14 point 9; for voice, the owner's
// point that Twilio with a Messaging Service cannot place calls either — the 3f′ line names only
// the generic gateway).
const OFF_CREATE = "Not created: phone alerts are not enabled on this instance.";
const VOICE_CREATE =
  "Not created: this instance sends SMS through a generic HTTP gateway, which cannot place calls. Create an SMS channel instead.";
const CREATE_FAILED = "Could not create the channel.";
const SAVE_FAILED = "Could not save the channel.";
const RECIPIENTS_FAILED = "The people who can be alerted could not be read.";
const ENABLE_FAILED = "The channel could not be enabled.";

// ── Who may do what (D28) ───────────────────────────────────────────────────────────────────────
const isAdmin = computed(() => session.canProjectAdmin(props.orgId, props.projectId));
const isEdit = computed(() => props.channel !== null);
const selfId = computed(() => session.user?.id ?? "");
/** D28 on edit: false for an editor on a number channel or on someone else's — no Save then. */
const canHold = computed(
  () => !props.channel || canHoldPhoneChannel(props.channel, { projectAdmin: isAdmin.value, selfUserId: selfId.value }),
);

// ── Form state ──────────────────────────────────────────────────────────────────────────────────
const mode = ref<"user" | "number">(props.channel?.recipient === "number" ? "number" : "user");
/** The number being typed — component-local only (P3). */
const typed = ref("");
const recipients = ref<Recipient[]>([]);
const recipientsLoaded = ref(false);
const recipientsError = ref("");
/** The admin's pick; "" while nothing is picked (or the stored recipient was deleted). */
const recipientId = ref(props.channel?.recipient_user_id ?? "");
const busy = ref(false);
const error = ref("");

/** The row's Verify on an existing number channel: the steps, without "Created". */
const verifyExisting = !!props.verify && props.channel?.recipient === "number";
/** "form" (3a–3d), then for a new number channel "verify" (3b-2) and "verified" (3b-3). */
const stage = ref<"form" | "verify" | "verified">(verifyExisting ? "verify" : "form");
/** The number channel being verified (3b-2 / 3b-3). */
const target = ref<Channel | null>(verifyExisting ? props.channel : null);
const codeSent = ref(false);
/** Revision 9: the instance can speak a code by a call — "Call with the code" is offered only then. */
const canCall = computed(() => session.telephonyCanCall === true);
/** The way the last code sent from this form went (a code is only verified here after one is sent). */
const sentVia = ref<CodeVia>("sms");
/** The way of the send in flight, for its button's label. */
const busyVia = ref<CodeVia | null>(null);
const sentLine = computed(() =>
  sentVia.value === "voice" ? `✓ 1 · Code spoken by a call to ${target.value?.number_masked ?? ""}` : `✓ 1 · Code sent to ${target.value?.number_masked ?? ""}`,
);
const code = ref("");
const codeIncorrect = ref(false);
/** The running code is spent (2d, 2e): Verify waits for a new one. */
const codeDead = ref(false);
const sendError = ref("");
const codeError = ref("");
const enableError = ref("");

const recipientName = (r: Recipient) => r.display_name || r.email;
const verifiedMark = (r: Recipient) => (r.phone_verified ? "✓ verified number" : "no verified number");

/** The admin's options: the eligible recipients, plus the stored one when it is not among them. */
const options = computed(() => {
  const list = recipients.value.map((r) => ({ value: r.user_id, label: `${recipientName(r)} · ${r.email} — ${verifiedMark(r)}` }));
  const stored = props.channel?.recipient_user_id;
  if (stored && recipientsLoaded.value && !recipients.value.some((r) => r.user_id === stored)) {
    list.unshift({ value: stored, label: props.channel?.recipient_display_name || "The current recipient" });
  }
  return list;
});

/** The editor's line (3c): "You (name) — mark". On someone else's channel, that person's name. */
const selfRecipient = computed(() => recipients.value.find((r) => r.user_id === selfId.value) ?? null);
const selfLine = computed(() => {
  if (isEdit.value && props.channel?.recipient_user_id !== selfId.value) {
    // `recipient_display_name` is a name only and may be null or "" for a person who still exists;
    // only a missing `recipient_user_id` means the account was deleted.
    if (!props.channel?.recipient_user_id) return "A deleted account";
    return props.channel.recipient_display_name || "The current recipient";
  }
  const r = selfRecipient.value;
  const name = (r && recipientName(r)) || session.user?.display_name || session.user?.email || "";
  return r ? `You (${name}) — ${verifiedMark(r)}` : `You (${name})`;
});

/** Who a new person channel names: the admin's pick, or the editor themselves. */
const effectiveRecipient = computed(() => (isAdmin.value ? recipientId.value : selfId.value));
const picked = computed(() => recipients.value.find((r) => r.user_id === effectiveRecipient.value) ?? null);

const canSubmit = computed(() => {
  if (!props.name.trim() || busy.value) return false;
  if (isEdit.value) return canHold.value;
  return mode.value === "user" ? effectiveRecipient.value !== "" : typed.value.trim() !== "";
});

async function loadRecipients() {
  if (recipientsLoaded.value) return;
  recipientsError.value = "";
  try {
    const res = await api.GET("/api/v1/projects/{projectID}/phone-recipients", { params: { path: { projectID: props.projectId } } });
    if (res.error || !res.data) {
      recipientsError.value = phoneErrorMessage(res.error, RECIPIENTS_FAILED);
      return;
    }
    recipients.value = res.data;
    recipientsLoaded.value = true;
    // A new channel starts on the first eligible person, as the mock draws it (3a).
    if (!isEdit.value && isAdmin.value && recipientId.value === "" && res.data.length > 0) recipientId.value = res.data[0].user_id;
  } catch {
    recipientsError.value = RECIPIENTS_FAILED;
  }
}

onMounted(() => {
  if (mode.value === "user") void loadRecipients();
});
watch(mode, (m) => {
  error.value = "";
  if (m === "user") void loadRecipients();
});
onBeforeUnmount(() => {
  typed.value = "";
  code.value = "";
});

/** A refusal that says phone alerts are off: every surface re-reads /me (Task 1's rule). */
async function refused(err: unknown, fallback: string): Promise<string> {
  const reason = phoneErrorReason(err);
  if (reason === "telephony_disabled") {
    void session.fetchMe();
    return phoneErrorMessage(err, isEdit.value ? fallback : OFF_CREATE);
  }
  if (reason === "voice_unsupported_by_provider" && !isEdit.value) return phoneErrorMessage(err, VOICE_CREATE);
  return phoneErrorMessage(err, fallback);
}

// ── Create (3a, 3a′, 3b-1, 3c) ──────────────────────────────────────────────────────────────────
async function create() {
  const body: Omit<CreateBody, "enabled"> =
    mode.value === "user"
      ? { type: props.type, name: props.name.trim(), recipient: "user", recipient_user_id: effectiveRecipient.value }
      : { type: props.type, name: props.name.trim(), recipient: "number", number: typed.value.trim() };
  // `enabled` is left out on purpose: the schema marks it required only because it has a default,
  // which the server applies to a person channel and ignores for a number channel (§4.2).
  const res = await api.POST("/api/v1/projects/{projectID}/notification-channels", {
    params: { path: { projectID: props.projectId } },
    body: body as CreateBody,
  });
  if (res.error || !res.data) {
    error.value = await refused(res.error, CREATE_FAILED);
    return;
  }
  typed.value = "";
  emit("created", res.data);
  if (res.data.recipient === "number") {
    target.value = res.data;
    stage.value = "verify";
    return;
  }
  emit("cancel");
}

// ── Save an edit (3d) ───────────────────────────────────────────────────────────────────────────
async function save() {
  const ch = props.channel;
  if (!ch?.id) return;
  const body: UpdateBody = { name: props.name.trim() };
  // A blank number keeps the stored one; an unchanged person is not re-sent, so a rename never
  // re-judges a recipient who has since lost access.
  if (mode.value === "number" && typed.value.trim() !== "") body.number = typed.value.trim();
  if (mode.value === "user" && isAdmin.value && recipientId.value !== "" && recipientId.value !== ch.recipient_user_id) {
    body.recipient_user_id = recipientId.value;
  }
  const res = await api.PATCH("/api/v1/notification-channels/{channelID}", { params: { path: { channelID: ch.id } }, body });
  if (res.error || !res.data) {
    error.value = await refused(res.error, SAVE_FAILED);
    return;
  }
  typed.value = "";
  emit("saved", res.data);
  emit("cancel");
}

async function submit() {
  if (!canSubmit.value) return;
  busy.value = true;
  error.value = "";
  try {
    await (isEdit.value ? save() : create());
  } catch {
    error.value = isEdit.value ? SAVE_FAILED : CREATE_FAILED;
  } finally {
    busy.value = false;
  }
}

// ── 3b-2: Send code (POST …/verify/send — no body by SMS, {via} by a call), then Verify ────────
async function sendCode(via: CodeVia) {
  const id = target.value?.id;
  if (!id || busy.value) return;
  busy.value = true;
  busyVia.value = via;
  sendError.value = "";
  codeError.value = "";
  try {
    const params = { path: { channelID: id } };
    const res =
      via === "voice"
        ? await api.POST("/api/v1/notification-channels/{channelID}/verify/send", { params, body: { via } })
        : await api.POST("/api/v1/notification-channels/{channelID}/verify/send", { params });
    if (res.error || !res.response.ok) {
      const reason = phoneErrorReason(res.error);
      // telephony off, or a call refused while /me said one could be made: re-read /me.
      if (reason === "telephony_disabled" || reason === "voice_unsupported_by_provider") void session.fetchMe();
      if (reason === "provider_rejected" || reason === "provider_unreachable") {
        codeSent.value = false;
        code.value = "";
        codeIncorrect.value = false;
      }
      sendError.value = phoneErrorMessage(res.error, reason === "send_limited" ? CODE_SEND_LIMITED : SEND_FAILED);
      return;
    }
    sentVia.value = via;
    codeSent.value = true;
    codeDead.value = false;
    codeIncorrect.value = false;
    code.value = "";
  } catch {
    sendError.value = SEND_FAILED;
  } finally {
    busy.value = false;
    busyVia.value = null;
  }
}

async function verify() {
  const id = target.value?.id;
  if (!id || busy.value || !codeSent.value || codeDead.value || !code.value.trim()) return;
  busy.value = true;
  sendError.value = "";
  codeError.value = "";
  try {
    const res = await api.POST("/api/v1/notification-channels/{channelID}/verify", {
      params: { path: { channelID: id } },
      body: { code: code.value.trim() },
    });
    if (res.error || !res.data) {
      const reason = phoneErrorReason(res.error);
      const answer = codeAnswer(reason, sentVia.value);
      if (answer) {
        codeError.value = answer.line;
        codeIncorrect.value = answer.incorrect;
        codeDead.value = answer.dead;
      } else {
        if (reason === "telephony_disabled") void session.fetchMe();
        codeError.value = phoneErrorMessage(res.error, VERIFY_FAILED);
      }
      return;
    }
    code.value = "";
    target.value = res.data;
    stage.value = "verified";
    emit("saved", res.data);
  } catch {
    codeError.value = VERIFY_FAILED;
  } finally {
    busy.value = false;
  }
}

// ── 3b-3: Enable channel (PATCH {enabled: true}) / Keep disabled ────────────────────────────────
async function enable() {
  const id = target.value?.id;
  if (!id || busy.value) return;
  busy.value = true;
  enableError.value = "";
  try {
    const res = await api.PATCH("/api/v1/notification-channels/{channelID}", { params: { path: { channelID: id } }, body: { enabled: true } });
    if (res.error || !res.data) {
      if (phoneErrorReason(res.error) === "telephony_disabled") void session.fetchMe();
      enableError.value = phoneErrorMessage(res.error, ENABLE_FAILED);
      return;
    }
    emit("saved", res.data);
    emit("cancel");
  } catch {
    enableError.value = ENABLE_FAILED;
  } finally {
    busy.value = false;
  }
}

// ── Classes (the view's channel form and PhonePanel's) ──────────────────────────────────────────
const LABEL = "text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3";
const FIELD =
  "rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent disabled:opacity-60";
const HINT = "text-[11.5px] leading-[1.45] text-ink-3";
const QUIET = "rounded border border-border bg-surface-2 px-3 py-[9px] text-[12.5px] leading-[1.55] text-ink-2";
const WARN = "rounded border border-degraded/40 bg-degraded-weak px-3 py-[9px] text-[12.5px] leading-[1.55] text-ink-2";
const ERR = "text-[12.5px] leading-[1.5] text-down";
const BTN_PRI =
  "h-[34px] rounded-sm bg-accent px-4 text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50";
const BTN_SEC = "h-[34px] rounded-sm border border-border px-4 text-[13px] text-ink-2 hover:border-border-strong disabled:opacity-50";
const BTN_PRI_SM =
  "h-[30px] rounded-sm bg-accent px-3 text-[12.5px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50";
const BTN_SEC_SM = "h-[30px] rounded-sm border border-border px-3 text-[12.5px] text-ink-2 hover:border-border-strong disabled:opacity-50";
const SEG = "relative cursor-pointer border-r border-border px-3 py-[7px] text-[12.5px] last:border-r-0";
</script>

<template>
  <div class="flex flex-col gap-3" data-testid="phone-channel-form">
    <!-- 3a–3d: the recipient -->
    <template v-if="stage === 'form'">
      <!-- Send to: fixed when the channel is created, so only offered while creating -->
      <div v-if="!isEdit" class="flex flex-col gap-[6px]">
        <span :class="LABEL">Send to</span>
        <div class="inline-flex w-fit overflow-hidden rounded-sm border border-border" role="radiogroup" aria-label="Send to">
          <label :class="[SEG, mode === 'user' ? 'bg-accent-weak font-medium text-accent' : 'bg-surface text-ink-2']">
            <input v-model="mode" type="radio" value="user" class="sr-only" data-testid="phone-send-to-user" />A person
          </label>
          <label
            :class="[
              SEG,
              !isAdmin ? 'cursor-not-allowed bg-surface-2 text-ink-3 line-through decoration-border-strong' : mode === 'number' ? 'bg-accent-weak font-medium text-accent' : 'bg-surface text-ink-2',
            ]"
          >
            <input v-model="mode" type="radio" value="number" :disabled="!isAdmin" class="sr-only" data-testid="phone-send-to-number" />A fixed number
          </label>
        </div>
        <span :class="HINT" data-testid="phone-send-to-hint">{{ isAdmin ? "Fixed when the channel is created." : "A fixed number can be added by a project admin." }}</span>
      </div>

      <!-- A person: the admin's picker, or the editor's own line (3a, 3c, 3d person) -->
      <div v-if="mode === 'user'" class="flex flex-col gap-[6px]">
        <span :class="LABEL">Person</span>
        <select v-if="isAdmin" v-model="recipientId" :class="[FIELD, 'max-w-[480px]']" aria-label="Person" data-testid="phone-recipient-select">
          <option v-if="isEdit && !channel?.recipient_user_id" value="" disabled>A deleted account</option>
          <option v-else-if="!recipientsLoaded && !recipientsError" value="" disabled>Loading…</option>
          <option v-for="o in options" :key="o.value" :value="o.value">{{ o.label }}</option>
        </select>
        <div v-else :class="[FIELD, 'max-w-[480px] cursor-not-allowed opacity-60']" aria-disabled="true" data-testid="phone-recipient-self">{{ selfLine }}</div>
        <span :class="HINT" data-testid="phone-recipient-hint">{{
          isAdmin
            ? "People who can open this project — members, organization members and global admins. No number is shown, not even masked."
            : "As an editor you can create a phone channel only for yourself."
        }}</span>
        <div v-if="recipientsError" :class="ERR" data-testid="phone-recipients-error">{{ recipientsError }}</div>
      </div>
      <div v-if="mode === 'user' && !isEdit && picked && !picked.phone_verified" :class="WARN" data-testid="phone-recipient-unverified">
        <b class="font-semibold text-degraded">{{ recipientName(picked) }} has no verified number.</b> The channel is created, but it is not a route — nothing is sent through it — until {{ recipientName(picked) }} confirms a number under Account → Phone.
      </div>

      <!-- A fixed number: 3b-1 while creating, 3d while editing -->
      <template v-if="mode === 'number'">
        <label class="flex flex-col gap-[6px]">
          <span :class="LABEL">Number</span>
          <input
            v-model="typed"
            type="tel"
            autocomplete="off"
            :placeholder="isEdit ? `${channel?.number_masked ?? ''} — type a new number to replace it` : '+44 7700 900199'"
            :disabled="!canHold"
            :class="[FIELD, 'max-w-[320px] font-mono']"
            data-testid="phone-number-input"
          />
          <span :class="HINT" data-testid="phone-number-hint">{{
            isEdit ? "A new number clears the confirmation and disables the channel until it is confirmed again." : "E.164. Never shown again in full."
          }}</span>
        </label>
        <div v-if="!isEdit" :class="QUIET" data-testid="phone-number-explain">
          A fixed-number channel is created <b class="font-semibold text-ink">disabled</b>. Confirm the number with a code to enable it. A shared phone receives the incident link only — it can never acknowledge, because it names nobody.
        </div>
      </template>

      <div v-if="!canHold" :class="HINT" data-testid="phone-edit-forbidden">As an editor you can edit only your own phone channels.</div>
      <div v-if="error" :class="ERR" data-testid="phone-channel-error">{{ error }}</div>
      <div class="flex items-center gap-2">
        <button v-if="canHold" type="button" :disabled="!canSubmit" :class="BTN_PRI" data-testid="phone-channel-submit" @click="submit">
          {{ busy ? (isEdit ? "Saving…" : "Creating…") : isEdit ? "Save" : "Create" }}
        </button>
        <button type="button" :class="BTN_SEC" data-testid="phone-channel-cancel" @click="emit('cancel')">Cancel</button>
      </div>
    </template>

    <!-- 3b-2: created, verify -->
    <template v-else-if="stage === 'verify'">
      <div class="text-[12.5px] text-ink-3" data-testid="phone-verify-created">{{ verifyExisting ? "" : "Created " }}<span class="font-medium text-ink">{{ target?.name }}</span> — disabled until the number is confirmed.</div>
      <div class="flex flex-col gap-3">
        <div data-testid="phone-verify-step-1">
          <div class="text-[12px] font-semibold" :class="codeSent ? 'text-up' : 'text-ink-2'">
            {{ codeSent ? sentLine : `1 · Send a code to ${target?.number_masked ?? ""}` }}
          </div>
          <div class="mt-[6px] flex flex-wrap items-center gap-2">
            <button type="button" :disabled="busy" :class="BTN_SEC_SM" data-testid="phone-verify-send" @click="sendCode('sms')">{{ codeSent ? "Send a new code" : "Send code" }}</button>
            <button v-if="canCall" type="button" :disabled="busy" :class="BTN_SEC_SM" data-testid="phone-verify-call" @click="sendCode('voice')">{{ busyVia === "voice" ? "Calling…" : codeSent ? "Call with a new code" : "Call with the code" }}</button>
          </div>
          <div v-if="sendError" :class="[ERR, 'mt-[6px]']" data-testid="phone-verify-send-error">{{ sendError }}</div>
        </div>
        <div data-testid="phone-verify-step-2">
          <div class="text-[12px]" :class="codeSent ? 'font-semibold text-ink-2' : 'font-medium text-ink-3'">2 · Enter the 6-digit code</div>
          <input
            v-model="code"
            inputmode="numeric"
            autocomplete="one-time-code"
            placeholder="123456"
            :disabled="!codeSent"
            class="mt-[6px] h-10 w-[180px] rounded-sm border bg-surface-2 px-3 font-mono text-[15px] tracking-[0.3em] outline-none focus:border-accent disabled:opacity-55"
            :class="codeIncorrect ? 'border-down' : 'border-border'"
            data-testid="phone-verify-code"
            @input="codeIncorrect = false"
            @keyup.enter="verify"
          />
          <div v-if="codeError" :class="[ERR, 'mt-[6px]']" data-testid="phone-verify-code-error">{{ codeError }}</div>
        </div>
      </div>
      <div class="flex items-center gap-2">
        <button type="button" :disabled="busy || !codeSent || codeDead || !code.trim()" :class="BTN_PRI_SM" data-testid="phone-verify-submit" @click="verify">{{ busy && busyVia === null ? "Verifying…" : "Verify" }}</button>
        <button type="button" :disabled="busy" :class="BTN_SEC_SM" data-testid="phone-verify-later" @click="emit('cancel')">Later</button>
      </div>
    </template>

    <!-- 3b-3: verified -->
    <template v-else>
      <div class="flex flex-col gap-3">
        <div class="text-[12px] font-semibold text-up" data-testid="phone-verified-step-1">{{ sentLine }}</div>
        <div class="text-[12px] font-semibold text-up" data-testid="phone-verified-step-2">✓ 2 · Number confirmed</div>
      </div>
      <div class="flex items-center gap-2">
        <button type="button" :disabled="busy" :class="BTN_PRI_SM" data-testid="phone-enable" @click="enable">{{ busy ? "Enabling…" : "Enable channel" }}</button>
        <button type="button" :disabled="busy" :class="BTN_SEC_SM" data-testid="phone-keep-disabled" @click="emit('cancel')">Keep disabled</button>
      </div>
      <div v-if="enableError" :class="ERR" data-testid="phone-enable-error">{{ enableError }}</div>
    </template>
  </div>
</template>
