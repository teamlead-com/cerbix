<script setup lang="ts">
// FR-024 / AC-0163-8, built to the APPROVED mock (docs/design/mock-reliability-gate.html, screens
// 1 "empty", 2 "editor", 3 "decision & override", 6 "unknown") as D-0207 reads it into the product.
//
// Four rules this card keeps, each of which it would otherwise get wrong in a way an operator
// would believe:
//
//   * it NEVER asks the gate (D-0207 item 4, §9). The latest decision is the newest LEDGER row for
//     this service over the last 30 days — an explicit half-open RFC3339 range, `limit=1`, then the
//     full immutable record by id. Opening a page never creates a decision, a rate token or a metric.
//   * the budget figure is the one THE DECISION quoted — a budget clause's `reasons[].value` — and
//     is omitted when no budget clause is in `reasons[]`. It never fetches a fresher number from the
//     report path to stand beside a decision it did not belong to.
//   * every mutation carries the revision the operator SAW (`expected_revision`, `policy_revision`,
//     the override's own id), and a 409 preserves what was typed and blocks every further mutation
//     until an explicit Reload re-reads the server's state.
//   * a control a role cannot use is NOT rendered (never a button that can only 403). The two
//     flags come from the view, from the two central session predicates; no role string is
//     compared here. A file-managed service does NOT make the gate read-only (D13).
//
// Transport discipline as ServiceAlerting.vue: one generation per context, an AbortController per
// request, a deadline raced against the transport, and every response — read OR write — dropped
// when it arrives after the context it belongs to has gone.
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from "vue";

import { api } from "@/api/client";
import type { components } from "@/api/schema";
import {
  ASSIGNMENTS,
  CHIP_ACC,
  CHIP_BASE,
  CHIP_DORM,
  CHIP_DOWN,
  CHIP_FILE,
  CHIP_PLAIN,
  CHIP_WARN,
  CLAUSE_DESCRIPTIONS,
  CLI_EXITS,
  GATE_CLAUSES,
  GATE_SCHEMA_VERSION,
  LEGEND,
  NO_DECISION_PILL,
  PILL_BASE,
  PILL_DOT,
  UNKNOWN_BEHAVIOR_LABELS,
  assignmentClass,
  budgetOfDecision,
  cliCommand,
  createTemplate,
  dateLabel,
  defaultOverrideUntil,
  describeFailure,
  draftFromPolicy,
  draftToBody,
  durationShort,
  exitCode,
  failureOf,
  fmtPercent,
  isConflict,
  isPast,
  ledgerRange,
  maxOverrideUntil,
  preciseLabel,
  reasonChip,
  reasonIncidentId,
  reasonKind,
  reasonValueLabel,
  sealStaleReason,
  shortId,
  statePill,
  templateWindow,
  transportFailureOf,
  untilLabel,
  validateDraft,
  validateOverride,
  windowsOf,
  type GateFailure,
  type GateWindow,
  type PolicyDraft,
  type ServiceSLATarget,
  type StatePill,
} from "@/lib/gate";
import { relTime } from "@/lib/incident";
import { sealedLabel } from "@/lib/services";

type GatePolicy = components["schemas"]["GatePolicy"];
type GateOverride = components["schemas"]["GateOverride"];
type GateDecision = components["schemas"]["GateDecision"];
type GateDecisionList = components["schemas"]["GateDecisionList"];

const props = defineProps<{
  projectId: string;
  serviceId: string;
  /** The service's slug, for the delete dialog's title. */
  serviceSlug?: string;
  /**
   * The service's SLO target inventory from the service detail (`sla_targets`, canonical order):
   * the editor offers EXACTLY these windows, the empty state lists them. Nothing is derived here.
   */
  slaTargets?: ServiceSLATarget[];
  /** The file provider owning the service, or "" — shown as a chip; it does NOT gate anything (D13). */
  managedBy?: string;
  /** `gate:policy:write` — editor and above (`session.canProjectWrite`). */
  canPolicyWrite: boolean;
  /** `gate:override` — project_admin and above (`session.canProjectAdmin`). */
  canOverride: boolean;
}>();

// ── Transport ─────────────────────────────────────────────────────────────────────────────────
// A lax response shape shared by every boundary: openapi-fetch's `{ data, error, response }`,
// with every member optional so a rejected transport and a test fixture fit the same reader.
type Res<T> = {
  data?: T;
  error?: unknown;
  response?: { status?: number; headers?: { get?: (name: string) => string | null } };
};
type Outcome<T> = { kind: "ok"; data: T | undefined } | { kind: "stale" } | { kind: "failed"; failure: GateFailure };

const TIMEOUT_MS = 10_000;
let generation = 0;
const inflight = new Set<AbortController>();

function abortAll() {
  for (const c of inflight) c.abort();
  inflight.clear();
}

/**
 * ONE request under the context generation captured by the caller: aborted with the context,
 * raced against a deadline, and reported as `stale` — never applied — when the context moved on
 * while it was in flight. Reads and writes go through the same door.
 */
async function request<T>(gen: number, run: (signal: AbortSignal) => Promise<Res<T>>): Promise<Outcome<T>> {
  const controller = new AbortController();
  inflight.add(controller);
  let deadline: ReturnType<typeof setTimeout> | undefined;
  try {
    const timeout = new Promise<never>((_, reject) => {
      deadline = setTimeout(() => {
        controller.abort();
        reject(new Error("the request timed out"));
      }, TIMEOUT_MS);
    });
    const res = (await Promise.race([run(controller.signal), timeout])) as Res<T>;
    if (gen !== generation) return { kind: "stale" };
    const status = res.response?.status;
    if (res.error !== undefined || (typeof status === "number" && status >= 400)) {
      return { kind: "failed", failure: failureOf(res) };
    }
    return { kind: "ok", data: res.data };
  } catch (e) {
    if (gen !== generation) return { kind: "stale" };
    return { kind: "failed", failure: transportFailureOf(e) };
  } finally {
    if (deadline) clearTimeout(deadline);
    inflight.delete(controller);
  }
}

const path = computed(() => ({ projectID: props.projectId, serviceID: props.serviceId }));
/** The clock every relative label on the card is read against; refreshed on every (re)load. */
const now = ref(new Date());

// ── The policy ────────────────────────────────────────────────────────────────────────────────
type PolicyStatus = "loading" | "ok" | "none" | "unavailable";
const policy = ref<GatePolicy | null>(null);
const policyStatus = ref<PolicyStatus>("loading");
/** The one line for a refusal other than `not_configured`: 401/403, a foreign service, a network failure. */
const policyUnavailable = ref("");

async function loadPolicy(gen: number) {
  policyStatus.value = "loading";
  policyUnavailable.value = "";
  const out = await request<GatePolicy>(gen, (signal) =>
    api.GET("/api/v1/projects/{projectID}/services/{serviceID}/gate/policy", {
      params: { path: path.value },
      signal,
    }),
  );
  if (out.kind === "stale") return;
  if (out.kind === "failed") {
    policy.value = null;
    if (out.failure.code === "not_configured") {
      policyStatus.value = "none";
      return;
    }
    policyStatus.value = "unavailable";
    policyUnavailable.value = describeFailure(out.failure, {
      notFound: "This service does not exist, or you cannot see it.",
      denied: "You cannot read this service's gate.",
      fallback: "Could not read the gate policy.",
    });
    return;
  }
  if (!out.data) {
    policy.value = null;
    policyStatus.value = "unavailable";
    policyUnavailable.value = "The policy read returned nothing.";
    return;
  }
  policy.value = out.data;
  policyStatus.value = "ok";
}

// ── The latest decision (the ledger, never the gate) ──────────────────────────────────────────
type LatestStatus = "loading" | "ok" | "empty" | "error";
const latest = ref<GateDecision | null>(null);
const latestStatus = ref<LatestStatus>("loading");
const latestError = ref("");

async function loadLatest(gen: number) {
  latestStatus.value = "loading";
  latestError.value = "";
  const { from, to } = ledgerRange(now.value);
  const page = await request<GateDecisionList>(gen, (signal) =>
    api.GET("/api/v1/projects/{projectID}/gate/decisions", {
      params: { path: { projectID: props.projectId }, query: { from, to, service_id: props.serviceId, limit: 1 } },
      signal,
    }),
  );
  if (page.kind === "stale") return;
  if (page.kind === "failed") {
    latestStatus.value = "error";
    latestError.value = describeFailure(page.failure, {
      context: "ledger",
      notFound: "The decision ledger is not visible for this project.",
      fallback: "Could not read the decision ledger.",
    });
    return;
  }
  const head = page.data?.items?.[0];
  if (!head) {
    latest.value = null;
    latestStatus.value = "empty";
    return;
  }
  // The listing is a projection; the card renders the EVIDENCE, so the full immutable record is
  // read by id — still the ledger, still a read.
  const full = await request<GateDecision>(gen, (signal) =>
    api.GET("/api/v1/projects/{projectID}/gate/decisions/{decisionID}", {
      params: { path: { projectID: props.projectId, decisionID: head.decision_id } },
      signal,
    }),
  );
  if (full.kind === "stale") return;
  if (full.kind === "failed" || !full.data) {
    latestStatus.value = "error";
    latestError.value =
      full.kind === "failed"
        ? describeFailure(full.failure, {
            context: "ledger",
            notFound: "The decision is no longer retained in the ledger.",
            fallback: "Could not read the decision.",
          })
        : "The decision read returned nothing.";
    return;
  }
  latest.value = full.data;
  latestStatus.value = "ok";
}

// ── The active override ───────────────────────────────────────────────────────────────────────
type OverrideStatus = "loading" | "active" | "none" | "error";
const override = ref<GateOverride | null>(null);
const overrideStatus = ref<OverrideStatus>("none");
const overrideReadError = ref("");

async function loadOverride(gen: number) {
  overrideStatus.value = "loading";
  overrideReadError.value = "";
  const out = await request<GateOverride>(gen, (signal) =>
    api.GET("/api/v1/projects/{projectID}/services/{serviceID}/gate/override", {
      params: { path: path.value },
      signal,
    }),
  );
  if (out.kind === "stale") return;
  if (out.kind === "failed") {
    override.value = null;
    if (out.failure.code === "none_active") {
      overrideStatus.value = "none";
      return;
    }
    overrideStatus.value = "error";
    overrideReadError.value = describeFailure(out.failure, {
      notFound: "This service does not exist, or you cannot see it.",
      fallback: "Could not read the active override.",
    });
    return;
  }
  override.value = out.data ?? null;
  overrideStatus.value = out.data ? "active" : "none";
}

// ── Windows with a target ─────────────────────────────────────────────────────────────────────
// The service's EXISTING target inventory, as the service detail reports it (`sla_targets`,
// canonical order, always an array). The editor offers exactly these windows and the empty state
// lists them; nothing is derived or synthesised here, and nothing can fail here — a detail that
// could not be read never renders this card at all.
const targets = computed<ServiceSLATarget[]>(() => props.slaTargets ?? []);
const windowsWithTarget = computed<GateWindow[]>(() => windowsOf(targets.value));

// ── Context lifecycle ─────────────────────────────────────────────────────────────────────────
function resetAll() {
  policy.value = null;
  policyStatus.value = "loading";
  policyUnavailable.value = "";
  latest.value = null;
  latestStatus.value = "loading";
  latestError.value = "";
  override.value = null;
  overrideStatus.value = "none";
  overrideReadError.value = "";
  legendOpen.value = false;
  editing.value = false;
  draft.value = createTemplate([]);
  baseRevision.value = null;
  saving.value = false;
  policyError.value = "";
  conflict.value = "";
  deleteOpen.value = false;
  deleting.value = false;
  deleteError.value = "";
  ovReason.value = "";
  ovUntil.value = defaultOverrideUntil(now.value);
  ovTouched.value = false;
  ovCreating.value = false;
  ovError.value = "";
  revokeConfirming.value = false;
  revoking.value = false;
}

/** The dependent panels, after the policy's state is known. */
async function loadDependents(gen: number) {
  const tasks: Promise<void>[] = [];
  if (policyStatus.value !== "unavailable") tasks.push(loadLatest(gen));
  if (policyStatus.value === "ok") tasks.push(loadOverride(gen));
  else {
    override.value = null;
    overrideStatus.value = "none";
  }
  await Promise.all(tasks);
}

async function load() {
  abortAll();
  const gen = ++generation;
  now.value = new Date();
  resetAll();
  await loadPolicy(gen);
  if (gen !== generation) return;
  await loadDependents(gen);
}

/**
 * The explicit Reload after a 409 (and the one way out of the blocked state): re-reads everything,
 * clears the conflict, and — when the editor is open — re-prefills it with the server's CURRENT
 * policy (or the create template when it is gone) so what is on screen is what can be saved. The
 * override form's inputs are kept: they are the operator's words, not the server's state.
 */
async function reload() {
  abortAll();
  const gen = ++generation;
  now.value = new Date();
  conflict.value = "";
  policyError.value = "";
  deleteError.value = "";
  ovError.value = "";
  deleteOpen.value = false;
  revokeConfirming.value = false;
  saving.value = false;
  deleting.value = false;
  ovCreating.value = false;
  revoking.value = false;
  await loadPolicy(gen);
  if (gen !== generation) return;
  await loadDependents(gen);
  if (gen !== generation) return;
  if (editing.value) {
    if (policyStatus.value === "unavailable") editing.value = false;
    else prefill();
  }
}

onMounted(load);
watch(() => [props.projectId, props.serviceId], load);
onBeforeUnmount(() => {
  generation++;
  abortAll();
  document.removeEventListener("keydown", onDialogKey);
});

// ── Header ────────────────────────────────────────────────────────────────────────────────────
const legendOpen = ref(false);

/** NOW: `not configured` without a policy; the latest decision's state with one; nothing until known. */
const headerPill = computed<StatePill | null>(() => {
  if (policyStatus.value === "none") return statePill("NOT_CONFIGURED");
  if (policyStatus.value === "ok" && latestStatus.value === "ok" && latest.value && latest.value.state !== "NOT_CONFIGURED") {
    return statePill(latest.value.state);
  }
  return null;
});

// ── The editor ────────────────────────────────────────────────────────────────────────────────
const editing = ref(false);
const draft = ref<PolicyDraft>(createTemplate([]));
/** `expected_revision` for the save: the revision the operator SAW, or null for "nothing configured". */
const baseRevision = ref<number | null>(null);
const saving = ref(false);
/** The policy panel's banner (`gate-policy-error`): a refusal verbatim, or the 409 text with Reload. */
const policyError = ref("");
/**
 * Which panel shows the Reload button; ANY value blocks every MUTATION on the card (Save, Delete,
 * Create, Revoke) until Reload. Inputs stay editable: what was typed is the operator's, not the
 * server's, and it is preserved through the refusal.
 */
const conflict = ref<"" | "policy" | "delete" | "override">("");
const blocked = computed(() => conflict.value !== "");

function prefill() {
  if (policy.value) {
    draft.value = draftFromPolicy(policy.value);
    baseRevision.value = policy.value.revision;
  } else {
    draft.value = createTemplate(windowsWithTarget.value);
    baseRevision.value = null;
  }
}

function openEditor() {
  if (!props.canPolicyWrite || blocked.value) return;
  policyError.value = "";
  prefill();
  editing.value = true;
}

// A create form whose inventory changes while it is open (the detail re-read) picks the template's window.
watch(windowsWithTarget, (ws) => {
  if (editing.value && !policy.value && (!draft.value.window || !ws.includes(draft.value.window))) {
    draft.value.window = templateWindow(ws);
  }
});

function discard() {
  editing.value = false;
  policyError.value = "";
  if (conflict.value === "policy" || conflict.value === "delete") conflict.value = "";
  prefill();
}

const draftErrors = computed(() => validateDraft(draft.value, windowsWithTarget.value, policy.value?.window ?? ""));
/** The stored window lost its target since the policy was saved (reviewer check 2). */
const windowLost = computed(
  () =>
    editing.value &&
    !!policy.value &&
    draft.value.window === policy.value.window &&
    !windowsWithTarget.value.includes(policy.value.window),
);
/** The select offers the inventory; a stored window outside it is listed once, disabled, so the field reads. */
const windowOptions = computed<{ value: GateWindow; label: string; disabled: boolean }[]>(() => {
  const opts = targets.value.map((t) => ({ value: t.window, label: `${t.window} · objective ${fmtPercent(t.objective)}`, disabled: false }));
  const w = draft.value.window;
  if (w && !windowsWithTarget.value.includes(w)) opts.push({ value: w, label: `${w} · no target any more`, disabled: true });
  return opts;
});
const canSave = computed(
  () => editing.value && !saving.value && !blocked.value && Object.keys(draftErrors.value).length === 0,
);

async function save() {
  if (!canSave.value) return;
  const gen = generation;
  saving.value = true;
  policyError.value = "";
  const body = draftToBody(draft.value, baseRevision.value);
  const out = await request<{ revision: number }>(gen, (signal) =>
    api.PUT("/api/v1/projects/{projectID}/services/{serviceID}/gate/policy", {
      params: { path: path.value },
      body,
      signal,
    }),
  );
  if (out.kind === "stale") return;
  saving.value = false;
  if (out.kind === "failed") {
    // The draft is PRESERVED. A 409 additionally blocks every mutation until Reload.
    policyError.value = describeFailure(out.failure, {
      context: "save",
      notFound: "This service does not exist any more, or you cannot see it.",
      fallback: "Could not save the policy.",
    });
    if (isConflict(out.failure)) conflict.value = "policy";
    return;
  }
  editing.value = false;
  // Re-read rather than trust the echo: `updated_at`/`updated_by` and — a real change revokes
  // the active override — the override panel.
  await loadPolicy(gen);
  if (gen !== generation) return;
  await loadDependents(gen);
}

// ── Delete ────────────────────────────────────────────────────────────────────────────────────
const deleteOpen = ref(false);
const deleting = ref(false);
/** The dialog's own banner (`gate-delete-error`), with Reload on a 409. */
const deleteError = ref("");
const deleteCancelBtn = ref<HTMLButtonElement | null>(null);

function onDialogKey(e: KeyboardEvent) {
  if (e.key === "Escape" && deleteOpen.value && !deleting.value) closeDelete();
}
watch(deleteOpen, (open) => {
  if (open) document.addEventListener("keydown", onDialogKey);
  else document.removeEventListener("keydown", onDialogKey);
});

function openDelete() {
  if (!policy.value || !props.canPolicyWrite || blocked.value) return;
  deleteError.value = "";
  deleteOpen.value = true;
  void nextTick(() => deleteCancelBtn.value?.focus());
}
function closeDelete() {
  if (deleting.value) return;
  deleteOpen.value = false;
  deleteError.value = "";
}

async function confirmDelete() {
  const p = policy.value;
  if (!p || deleting.value || blocked.value) return;
  const gen = generation;
  deleting.value = true;
  deleteError.value = "";
  const out = await request<unknown>(gen, (signal) =>
    api.DELETE("/api/v1/projects/{projectID}/services/{serviceID}/gate/policy", {
      params: { path: path.value, query: { expected_revision: p.revision } },
      signal,
    }),
  );
  if (out.kind === "stale") return;
  deleting.value = false;
  if (out.kind === "failed") {
    deleteError.value = describeFailure(out.failure, {
      context: "delete",
      notFound: "This service does not exist any more, or you cannot see it.",
      fallback: "Could not delete the policy.",
    });
    if (isConflict(out.failure) || out.failure.code === "not_configured") conflict.value = "delete";
    return;
  }
  deleteOpen.value = false;
  editing.value = false;
  await loadPolicy(gen);
  if (gen !== generation) return;
  await loadDependents(gen);
}

// ── The override panel ────────────────────────────────────────────────────────────────────────
const ovReason = ref("");
const ovUntil = ref(defaultOverrideUntil());
const ovTouched = ref(false);
const ovCreating = ref(false);
/** The panel's one error line (`gate-override-error`), with Reload on a 409. */
const ovError = ref("");
const revokeConfirming = ref(false);
const revoking = ref(false);

const ovMax = computed(() => maxOverrideUntil(now.value));
const ovErrors = computed(() => validateOverride(ovReason.value, ovUntil.value));
/** One override at a time (D9): the form stays visible but Create is disabled while one is active. */
const ovBlockedByActive = computed(() => overrideStatus.value === "active");
const canCreate = computed(
  () =>
    props.canOverride &&
    policyStatus.value === "ok" &&
    overrideStatus.value !== "loading" &&
    !ovBlockedByActive.value &&
    !ovCreating.value &&
    !blocked.value &&
    !ovErrors.value.reason &&
    !ovErrors.value.until,
);

async function createOverride() {
  ovTouched.value = true;
  const p = policy.value;
  if (!canCreate.value || !p) return;
  const gen = generation;
  ovCreating.value = true;
  ovError.value = "";
  const out = await request<{ id: string }>(gen, (signal) =>
    api.POST("/api/v1/projects/{projectID}/services/{serviceID}/gate/override", {
      params: { path: path.value },
      body: { policy_revision: p.revision, reason: ovReason.value.trim(), expires_at: new Date(ovUntil.value).toISOString() },
      signal,
    }),
  );
  if (out.kind === "stale") return;
  ovCreating.value = false;
  if (out.kind === "failed") {
    // The reason and the expiry stay on screen; a 409 blocks every mutation until Reload.
    ovError.value = describeFailure(out.failure, {
      context: "override",
      notFound: "This service does not exist any more, or you cannot see it.",
      fallback: "Could not create the override.",
    });
    if (isConflict(out.failure)) conflict.value = "override";
    return;
  }
  ovReason.value = "";
  ovUntil.value = defaultOverrideUntil();
  ovTouched.value = false;
  await Promise.all([loadOverride(gen), loadLatest(gen)]);
}

async function revoke() {
  const o = override.value;
  if (!o || !props.canOverride || revoking.value || blocked.value) return;
  const gen = generation;
  revoking.value = true;
  ovError.value = "";
  // By the override's own id, never "the current one" (D13a).
  const out = await request<unknown>(gen, (signal) =>
    api.DELETE("/api/v1/projects/{projectID}/services/{serviceID}/gate/overrides/{overrideID}", {
      params: { path: { ...path.value, overrideID: o.id } },
      signal,
    }),
  );
  if (out.kind === "stale") return;
  revoking.value = false;
  revokeConfirming.value = false;
  if (out.kind === "failed") {
    ovError.value = describeFailure(out.failure, {
      context: "revoke",
      notFound: "This override does not exist, or you cannot see it.",
      fallback: "Could not revoke the override.",
    });
    if (isConflict(out.failure)) conflict.value = "override";
    return;
  }
  await loadOverride(gen);
}

// ── The latest-decision card's derived facts ──────────────────────────────────────────────────
const budget = computed(() => (latest.value ? budgetOfDecision(latest.value.reasons) : null));
const budgetRemaining = computed(() => (budget.value?.percent != null ? Math.max(0, 100 - budget.value.percent) : 0));
const budgetClass = computed(() => {
  const a = budget.value?.reason.assignment;
  if (budget.value?.percent == null) return "text-ink-3";
  if (reasonKind(budget.value.reason) !== "matched") return "text-ink";
  return a === "block" ? "text-down" : a === "warn" ? "text-degraded" : "text-ink";
});
const sealStale = computed(
  () => !!latest.value && latest.value.seal_lag != null && latest.value.max_seal_lag_seconds != null && latest.value.seal_lag > latest.value.max_seal_lag_seconds,
);
const staleReason = computed(() => (latest.value ? sealStaleReason(latest.value.reasons) : undefined));
/** The header chip beside the big pill: an override in force, or an action that differs from the state. */
const actionChip = computed<{ text: string; cls: string } | null>(() => {
  const d = latest.value;
  if (!d || !d.action) return null;
  if (d.override) return { text: `override applied → action ${d.action}`, cls: chipAcc };
  if (d.action !== d.state) {
    const tone = d.action === "BLOCK" ? CHIP_DOWN : d.action === "WARN" ? CHIP_WARN : "border-up bg-up-weak text-up";
    return { text: `action ${d.action} · exit ${exitCode(d.action)}`, cls: `${CHIP_BASE} ${tone}` };
  }
  return null;
});

// ── The CLI card ──────────────────────────────────────────────────────────────────────────────
const origin = typeof window !== "undefined" && window.location ? window.location.origin : "";
const command = computed(() => cliCommand(origin, props.projectId, props.serviceId));

// ── Shared classes (token-mapped; see tailwind.config.js) ─────────────────────────────────────
const BTN = "inline-flex h-[30px] items-center gap-[6px] rounded-sm border border-border bg-surface px-3 text-[13px] text-ink hover:border-border-strong disabled:cursor-not-allowed disabled:opacity-50";
const BTN_PRI = "inline-flex h-[30px] items-center gap-[6px] rounded-sm border border-accent bg-accent px-3 text-[13px] font-medium text-accent-ink disabled:cursor-not-allowed disabled:opacity-50";
const BTN_DANGER = "inline-flex h-[30px] items-center rounded-sm border border-down bg-surface px-3 text-[13px] text-down disabled:cursor-not-allowed disabled:opacity-50";
const BTN_SM = "inline-flex h-[26px] items-center rounded-sm border border-border bg-surface px-[10px] text-[12.5px] text-ink-2 hover:border-border-strong hover:text-ink disabled:cursor-not-allowed disabled:opacity-50";
const BTN_SM_DANGER = "inline-flex h-[26px] items-center rounded-sm border border-down bg-surface px-[10px] text-[12.5px] text-down disabled:cursor-not-allowed disabled:opacity-50";
// The chip and pill classes are lib/gate.ts's (shared with the ledger views); these are the compositions this card uses.
const chipPlain = `${CHIP_BASE} ${CHIP_PLAIN}`;
const chipMono = `${chipPlain} font-mono text-[10.5px]`;
const chipDorm = `${CHIP_BASE} ${CHIP_DORM}`;
const chipFile = `${CHIP_BASE} ${CHIP_FILE}`;
const chipAcc = `${CHIP_BASE} ${CHIP_ACC}`;
const reasonChipCls = "font-mono text-[10.5px]";
const INPUT = "h-[30px] rounded-sm border border-border bg-surface px-[10px] text-[13px] text-ink outline-none focus:border-accent disabled:opacity-60";
const LBL = "text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3";
const PILL_BIG = "inline-flex h-[30px] items-center gap-[6px] rounded-full pl-[9px] pr-3 text-[13.5px] font-semibold";
const PILL_DOT_BIG = `${PILL_DOT} h-[9px] w-[9px]`;
const ERR_BOX = "flex items-start gap-[9px] rounded-sm border border-down bg-down-weak px-3 py-[9px] text-[13px] text-down";
const WARN_BOX = "flex items-start gap-[9px] rounded-sm border border-degraded bg-degraded-weak px-3 py-[9px] text-[13px] text-degraded";
const INFO_BOX = "flex items-start gap-[9px] rounded-sm border border-border-strong bg-surface-2 px-3 py-[9px] text-[13px] text-ink-2";
const ROW = "flex flex-wrap items-center gap-[9px] border-b border-border px-4 py-[11px] last:border-b-0";
const SEG = "inline-flex overflow-hidden rounded-sm border border-border";
const SEG_ITEM = "border-r border-border px-[10px] py-[3px] text-[11.5px] last:border-r-0 disabled:cursor-not-allowed";
</script>

<template>
  <section class="mb-4 overflow-hidden rounded border border-border bg-surface shadow-card" data-testid="service-gate">
    <header class="flex flex-wrap items-center gap-[10px] border-b border-border px-4 py-[10px]">
      <h2 class="text-[13.5px] font-semibold">Release gate</h2>
      <span v-if="headerPill" :class="[PILL_BASE, headerPill.cls]" data-testid="gate-state">
        <span :class="[PILL_DOT, headerPill.dot]"></span>{{ headerPill.label }}
      </span>
      <div class="flex-1"></div>
      <span v-if="policyStatus === 'ok' && policy" :class="chipMono" data-testid="gate-policy-chip">revision {{ policy.revision }}</span>
      <span v-else-if="policyStatus === 'none'" :class="chipDorm" data-testid="gate-policy-chip">no policy</span>
      <button
        type="button"
        :class="BTN_SM"
        :aria-pressed="legendOpen"
        aria-controls="gate-legend"
        data-testid="gate-legend-toggle"
        @click="legendOpen = !legendOpen"
      >
        the five answers
      </button>
    </header>

    <!-- The five answers: no new colour, status never colour alone. -->
    <div
      v-if="legendOpen"
      id="gate-legend"
      class="grid gap-[10px] border-b border-border px-4 py-3 [grid-template-columns:repeat(auto-fit,minmax(190px,1fr))]"
      data-testid="gate-legend"
    >
      <div v-for="l in LEGEND" :key="l.state" class="flex flex-col gap-[7px] rounded-[7px] border border-border p-[11px] text-[12.5px] text-ink-2">
        <span :class="[PILL_BASE, statePill(l.state).cls]">
          <span :class="[PILL_DOT, statePill(l.state).dot]"></span>{{ statePill(l.state).label }}
        </span>
        <span>{{ l.text }}</span>
      </div>
    </div>

    <p v-if="policyStatus === 'loading'" class="px-4 py-6 text-[13px] text-ink-3" data-testid="gate-loading">Loading…</p>

    <!-- A refusal other than not_configured: one line, no controls, nothing else asked. -->
    <p v-else-if="policyStatus === 'unavailable'" class="px-4 py-6 text-[13px] text-ink-3" data-testid="gate-unavailable">
      {{ policyUnavailable }}
    </p>

    <template v-else>
      <!-- ── Screen 1: the empty state ─────────────────────────────────────────────────── -->
      <div v-if="policyStatus === 'none' && !editing" class="flex flex-col gap-[14px] p-4" data-testid="gate-empty">
        <p class="max-w-[62ch] text-[13px] text-ink-2">
          Nothing asks this service before a release yet. A gate policy names <b>one SLO window</b> of this service and
          says, for each release-risk fact, whether it <b>blocks</b>, <b>warns</b> or is <b>ignored</b>. A pipeline then
          asks <span class="font-mono">cerbix gate check</span> immediately before the protected step and gets one of
          <span class="font-mono">ALLOW · WARN · BLOCK · UNKNOWN</span>, with the evidence it rests on.
        </p>
        <div class="flex flex-wrap items-center gap-[9px] rounded-[7px] border border-border px-[13px] py-[11px]" data-testid="gate-windows">
          <span :class="LBL" class="min-w-[140px]">Windows with a target</span>
          <span v-for="t in targets" :key="t.window" :class="chipMono" data-testid="gate-window-chip" :data-window="t.window">
            {{ t.window }} · {{ fmtPercent(t.objective) }}
          </span>
          <span v-if="!targets.length" class="text-[12.5px] text-ink-3" data-testid="gate-windows-none">
            none yet — set an objective on a window in the reliability report above; a policy needs one
          </span>
          <span class="flex-1"></span>
          <span v-if="targets.length" :class="chipPlain">a policy picks exactly one</span>
        </div>
        <div v-if="canPolicyWrite" class="flex flex-wrap items-center gap-[10px]">
          <button
            type="button"
            :class="BTN_PRI"
            :disabled="!windowsWithTarget.length || blocked"
            :title="!windowsWithTarget.length ? 'no window has a target yet' : undefined"
            data-testid="gate-configure"
            @click="openEditor"
          >
            Configure gate
          </button>
          <span v-if="!windowsWithTarget.length" class="text-[12px] text-ink-3">a policy needs a window with a target</span>
        </div>
      </div>

      <!-- ── The policy: read-only rendering, or the editor (screen 2) ─────────────────── -->
      <div v-else class="flex flex-col gap-[14px] p-4">
        <div class="flex flex-wrap items-center gap-[10px]">
          <h3 class="text-[13px] font-semibold">Gate policy</h3>
          <span :class="chipMono">schema_version {{ policy?.schema_version ?? GATE_SCHEMA_VERSION }}</span>
          <span v-if="policy" :class="chipMono">revision {{ policy.revision }}</span>
          <span v-else :class="chipDorm">new policy</span>
          <div class="flex-1"></div>
          <span v-if="managedBy" :class="chipFile" :title="'Owned by the file provider ' + managedBy">file-managed service</span>
          <span v-if="managedBy" :class="chipAcc">gate owned here</span>
          <button v-if="canPolicyWrite && !editing" type="button" :class="BTN_SM" :disabled="blocked" data-testid="gate-configure" @click="openEditor">
            Configure
          </button>
        </div>

        <!-- Read-only: what every role sees. The segmented control is drawn, not interactive. -->
        <div v-if="!editing && policy" class="flex flex-col gap-[14px]" data-testid="gate-policy-readonly">
          <div class="grid grid-cols-2 gap-[14px] max-[760px]:grid-cols-1">
            <div class="flex flex-col gap-[5px]">
              <span :class="LBL">SLO window</span>
              <span class="font-mono text-[13px]" data-testid="gate-readonly-window">{{ policy.window }}</span>
              <span class="text-[12px] text-ink-3">every budget and burn fact is judged against this window's target</span>
            </div>
            <div class="flex flex-col gap-[5px]">
              <span :class="LBL">When a fact is unavailable</span>
              <span class="text-[13px]" data-testid="gate-readonly-unknown-behavior">{{ UNKNOWN_BEHAVIOR_LABELS[policy.unknown_behavior] }}</span>
              <span class="text-[12px] text-ink-3"><span class="font-mono">unknown_behavior</span>: <b>warn</b> lets a release through with the reasons printed; <b>block</b> holds it</span>
            </div>
          </div>
          <div>
            <div :class="LBL" class="mb-2">Release-risk facts</div>
            <div class="rounded-[7px] border border-border">
              <div
                v-for="c in GATE_CLAUSES"
                :key="c"
                :class="ROW"
                :data-testid="`gate-readonly-clause-${c}`"
                :data-assignment="policy.clauses[c]"
              >
                <span class="font-mono text-[13.5px] font-medium">{{ c }}</span>
                <span v-if="c === 'budget_consumed'" class="font-mono text-[12.5px] text-ink-2">≥ {{ policy.budget_consumed_percent }} % burned</span>
                <span class="flex-1"></span>
                <span :class="SEG" role="group" :aria-label="`${c} assignment`">
                  <span
                    v-for="a in ASSIGNMENTS"
                    :key="a"
                    :class="[SEG_ITEM, assignmentClass(a, policy.clauses[c] === a)]"
                    :aria-current="policy.clauses[c] === a ? 'true' : undefined"
                  >{{ a }}</span>
                </span>
                <span class="basis-full text-[12.5px] text-ink-3">{{ CLAUSE_DESCRIPTIONS[c] }}</span>
              </div>
            </div>
          </div>
          <div class="grid grid-cols-2 gap-[14px] max-[760px]:grid-cols-1">
            <div class="flex flex-col gap-[5px]">
              <span :class="LBL">Trust sealed facts up to</span>
              <span class="font-mono text-[13px]" data-testid="gate-readonly-seal-lag">{{ durationShort(policy.max_seal_lag_seconds) }} behind now</span>
              <span class="text-[12px] text-ink-3">past it every budget fact is <b>unavailable</b> (<span class="font-mono">seal_stale</span>)</span>
            </div>
            <div class="flex flex-col gap-[5px]">
              <span :class="LBL">Revision</span>
              <span class="font-mono text-[13px]" data-testid="gate-readonly-revision">
                {{ policy.revision }} · updated {{ sealedLabel(policy.updated_at) }} · by {{ policy.updated_by }}
              </span>
            </div>
          </div>
        </div>

        <!-- The editor: the whole document, every field explicit, one revision. -->
        <form v-else-if="editing" class="flex flex-col gap-[14px]" data-testid="gate-policy-form" novalidate @submit.prevent="save">
          <div class="grid grid-cols-2 gap-[14px] max-[760px]:grid-cols-1">
            <div class="flex flex-col gap-[5px]">
              <label class="text-[12.5px] font-medium text-ink-2" for="gate-window">SLO window</label>
              <select
                id="gate-window"
                v-model="draft.window"
                :class="[INPUT, 'font-mono', draftErrors.window ? 'border-down' : '']"
                :disabled="saving"
                data-testid="gate-window"
              >
                <option v-if="!draft.window" value="" disabled>pick a window</option>
                <option v-for="o in windowOptions" :key="o.value" :value="o.value" :disabled="o.disabled">{{ o.label }}</option>
              </select>
              <span class="text-[12px] text-ink-3">only windows this service has a target for · the budget below is this window's</span>
              <span v-if="windowLost" class="text-[12px] text-degraded" data-testid="gate-window-stale">
                The stored window <span class="font-mono">{{ policy?.window }}</span> no longer has a target; the gate answers UNKNOWN (<span class="font-mono">window_target_missing</span>) until this is fixed.
              </span>
              <span v-if="draftErrors.window" class="text-[12px] text-down" data-testid="gate-field-error-window">{{ draftErrors.window }}</span>
            </div>
            <div class="flex flex-col gap-[5px]">
              <label class="text-[12.5px] font-medium text-ink-2" for="gate-unknown-behavior">When a fact is unavailable</label>
              <select id="gate-unknown-behavior" v-model="draft.unknown_behavior" :class="INPUT" :disabled="saving" data-testid="gate-unknown-behavior">
                <option value="warn">{{ UNKNOWN_BEHAVIOR_LABELS.warn }}</option>
                <option value="block">{{ UNKNOWN_BEHAVIOR_LABELS.block }}</option>
              </select>
              <span class="text-[12px] text-ink-3"><span class="font-mono">unknown_behavior</span>: <b>warn</b> lets a release through with the reasons printed; <b>block</b> holds it</span>
            </div>
          </div>

          <div>
            <div :class="LBL" class="mb-2">Release-risk facts · each must be assigned</div>
            <div class="rounded-[7px] border border-border">
              <div v-for="c in GATE_CLAUSES" :key="c" :class="ROW">
                <span class="font-mono text-[13.5px] font-medium">{{ c }}</span>
                <label v-if="c === 'budget_consumed'" class="inline-flex h-[26px] items-center gap-[6px] rounded-sm border bg-surface px-[8px] font-mono text-[12.5px]" :class="draftErrors.threshold ? 'border-down' : 'border-border'">
                  <input
                    v-model="draft.threshold"
                    type="number"
                    inputmode="numeric"
                    min="1"
                    max="100"
                    step="1"
                    class="w-[44px] border-0 bg-transparent text-right font-mono text-[12.5px] text-ink outline-none"
                    aria-label="threshold percent"
                    :disabled="saving"
                    data-testid="gate-threshold"
                  />
                  <span class="text-[12px] text-ink-3">% burned</span>
                </label>
                <span class="flex-1"></span>
                <span :class="SEG" role="group" :aria-label="`${c} assignment`">
                  <button
                    v-for="a in ASSIGNMENTS"
                    :key="a"
                    type="button"
                    :class="[SEG_ITEM, assignmentClass(a, draft.clauses[c] === a)]"
                    :aria-pressed="draft.clauses[c] === a"
                    :disabled="saving"
                    :data-testid="`gate-clause-${c}-${a}`"
                    @click="draft.clauses[c] = a"
                  >{{ a }}</button>
                </span>
                <span class="basis-full text-[12.5px] text-ink-3">{{ CLAUSE_DESCRIPTIONS[c] }}</span>
                <span v-if="c === 'budget_consumed' && draftErrors.threshold" class="basis-full text-[12px] text-down" data-testid="gate-field-error-threshold">{{ draftErrors.threshold }}</span>
              </div>
            </div>
            <p v-if="draftErrors.clauses" class="mt-2 text-[12px] text-down" data-testid="gate-field-error-clauses">{{ draftErrors.clauses }}</p>
          </div>

          <div class="grid grid-cols-2 gap-[14px] max-[760px]:grid-cols-1">
            <div class="flex flex-col gap-[5px]">
              <label class="text-[12.5px] font-medium text-ink-2" for="gate-seal-lag">Trust sealed facts up to</label>
              <span class="inline-flex h-[30px] w-[220px] items-center gap-[8px] rounded-sm border bg-surface px-[10px] font-mono text-[13px]" :class="draftErrors['seal-lag'] ? 'border-down' : 'border-border'">
                <input
                  id="gate-seal-lag"
                  v-model="draft.sealLagMinutes"
                  type="number"
                  inputmode="numeric"
                  min="5"
                  max="1440"
                  step="1"
                  class="w-[52px] border-0 bg-transparent text-right font-mono text-[13px] text-ink outline-none"
                  aria-label="max seal lag minutes"
                  :disabled="saving"
                  data-testid="gate-seal-lag-minutes"
                />
                <span class="text-[12px] text-ink-3">minutes behind now</span>
              </span>
              <span class="text-[12px] text-ink-3"><span class="font-mono">max_seal_lag_seconds</span> · whole minutes, 5 min – 24 h · past it every budget fact is <b>unavailable</b> (<span class="font-mono">seal_stale</span>)</span>
              <span v-if="draftErrors['seal-lag']" class="text-[12px] text-down" data-testid="gate-field-error-seal-lag">{{ draftErrors['seal-lag'] }}</span>
            </div>
            <div :class="INFO_BOX" class="self-end text-[12.5px]">
              A healthy service seals about 3 minutes behind now (bucket 60 s + late-arrival grace 120 s). The floor of 5 minutes is
              that plus two buckets of headroom — a bound below it would say <span class="font-mono">seal_stale</span> about a system doing exactly what it should.
            </div>
          </div>

          <!-- Refused by the server: verbatim, and a 409 with the way out. -->
          <div v-if="policyError" :class="ERR_BOX" role="alert">
            <span aria-hidden="true">⚠</span>
            <span class="flex-1" data-testid="gate-policy-error">{{ policyError }}</span>
            <button v-if="conflict === 'policy'" type="button" class="flex-none rounded-[5px] border border-down bg-surface px-2 py-[2px] text-[11.5px]" data-testid="gate-reload" @click="reload">Reload</button>
          </div>

          <div class="flex flex-wrap items-center gap-[10px] border-t border-border pt-[14px]">
            <button type="submit" :class="BTN_PRI" :disabled="!canSave" data-testid="gate-save">{{ saving ? "Saving…" : "Save policy" }}</button>
            <button type="button" :class="BTN" :disabled="saving" data-testid="gate-discard" @click="discard">Discard changes</button>
            <span class="flex-1"></span>
            <button v-if="policy" type="button" :class="BTN_DANGER" :disabled="saving || blocked" data-testid="gate-delete" @click="openDelete">Delete policy…</button>
          </div>
        </form>
      </div>

      <!-- ── Screen 3/6: the latest decision — the ledger, never the gate ────────────────── -->
      <div class="border-t border-border p-4" data-testid="gate-latest">
        <div class="flex flex-wrap items-center gap-[10px]">
          <h3 class="text-[13px] font-semibold">Latest decision <span class="font-normal text-ink-3">· last 30 days</span></h3>
          <template v-if="latestStatus === 'ok' && latest">
            <span :class="[PILL_BIG, statePill(latest.state).cls]" data-testid="gate-latest-state">
              <span :class="[PILL_DOT_BIG, statePill(latest.state).dot]"></span>{{ statePill(latest.state).label }}
            </span>
            <span v-if="actionChip" :class="actionChip.cls" data-testid="gate-latest-action">{{ actionChip.text }}</span>
          </template>
          <div class="flex-1"></div>
          <template v-if="latestStatus === 'ok' && latest">
            <span :class="chipMono" :title="latest.decision_id" data-testid="gate-latest-id">{{ shortId(latest.decision_id) }}</span>
            <span v-if="latest.policy_revision != null" :class="chipMono" data-testid="gate-latest-revision">policy rev {{ latest.policy_revision }}</span>
          </template>
          <RouterLink :to="{ path: '/gate/decisions', query: { service: serviceId } }" class="font-mono text-[11.5px] text-accent hover:underline" data-testid="gate-all-decisions">
            all decisions →
          </RouterLink>
        </div>

        <p v-if="latestStatus === 'loading'" class="mt-3 text-[13px] text-ink-3" data-testid="gate-latest-loading">Loading…</p>
        <p v-else-if="latestStatus === 'error'" class="mt-3 text-[13px] text-down" data-testid="gate-latest-error">{{ latestError }}</p>
        <div v-else-if="latestStatus === 'empty'" class="mt-3 flex flex-wrap items-center gap-3 text-[13px] text-ink-3" data-testid="gate-latest-empty">
          <span :class="[PILL_BASE, NO_DECISION_PILL.cls]">
            <span :class="[PILL_DOT, NO_DECISION_PILL.dot]"></span>{{ NO_DECISION_PILL.label }}
          </span>
          <span>No pipeline has asked in the last 30 days. This card reads the project ledger; it never asks the gate itself.</span>
        </div>

        <div v-else-if="latest" class="mt-3 flex flex-col gap-[14px]">
          <div class="grid grid-cols-2 gap-[14px] max-[760px]:grid-cols-1">
            <dl class="grid grid-cols-[max-content_1fr] gap-x-[18px] gap-y-[6px] text-[13px]">
              <dt class="text-ink-3">Evaluated</dt>
              <dd class="font-mono" data-testid="gate-latest-evaluated">{{ preciseLabel(latest.evaluated_at) }}</dd>

              <dt class="text-ink-3">Facts sealed through</dt>
              <dd v-if="latest.sealed_through" class="font-mono" data-testid="gate-latest-sealed">
                {{ sealedLabel(latest.sealed_through) }}
                <span v-if="latest.seal_lag != null" :class="sealStale ? 'text-down' : 'text-ink-3'" data-testid="gate-latest-seal-lag" :data-stale="sealStale ? 'true' : 'false'">
                  · seal lag {{ durationShort(latest.seal_lag) }}<template v-if="latest.max_seal_lag_seconds != null">{{ sealStale ? ` — over the ${durationShort(latest.max_seal_lag_seconds)} allowed` : ` of ${durationShort(latest.max_seal_lag_seconds)} allowed` }}</template>
                </span>
              </dd>
              <dd v-else class="text-ink-3" data-testid="gate-latest-never-sealed">not yet — no fact of this service has been sealed, so there is no watermark to show</dd>

              <template v-if="latest.window">
                <dt class="text-ink-3">Window</dt>
                <dd class="font-mono" data-testid="gate-latest-window">
                  {{ latest.window }}<template v-if="latest.objective != null"> · objective {{ fmtPercent(latest.objective) }}</template><template v-if="latest.objective_updated_at"> · target updated {{ dateLabel(latest.objective_updated_at) }}</template><template v-if="latest.objective == null"> · <span class="text-ink-3">no target for this window</span></template>
                </dd>
              </template>

              <template v-if="latest.facts_fresh_until">
                <dt class="text-ink-3">Facts fresh until</dt>
                <dd class="font-mono" data-testid="gate-latest-fresh-until" :data-past="isPast(latest.facts_fresh_until, now) ? 'true' : 'false'">
                  {{ sealedLabel(latest.facts_fresh_until) }}
                  <span :class="isPast(latest.facts_fresh_until, now) ? 'text-degraded' : 'text-ink-3'"> · {{ untilLabel(latest.facts_fresh_until, now) }}</span>
                </dd>
              </template>
            </dl>

            <!-- The budget the DECISION quoted, or its withholding — never a fresher number from elsewhere. -->
            <div v-if="budget">
              <div :class="LBL">Error budget<template v-if="latest.window"> · {{ latest.window }}</template></div>
              <div v-if="budget.percent != null" class="font-mono text-[26px] font-medium tracking-tight" :class="budgetClass" data-testid="gate-latest-budget">
                {{ fmtPercent(budget.percent, 1) }}<small class="ml-[6px] text-[12px] font-normal tracking-normal text-ink-3">burned · {{ fmtPercent(budgetRemaining, 1) }} remaining</small>
              </div>
              <div v-else class="font-mono text-[26px] font-medium text-ink-3" data-testid="gate-latest-budget-withheld">
                —<small class="ml-[6px] text-[12px] font-normal text-ink-3">withheld · {{ budget.reason.code }}</small>
              </div>
              <div class="mt-[6px] text-[12px] text-ink-3">the number this decision quoted — one owner, one snapshot</div>
            </div>
          </div>

          <div>
            <div :class="LBL" class="mb-2">Reasons · every clause that matched or was unavailable</div>
            <ul class="rounded-[7px] border border-border" data-testid="gate-latest-reasons">
              <li
                v-for="(r, i) in latest.reasons"
                :key="i"
                :class="ROW"
                class="text-[13px]"
                data-testid="gate-reason"
                :data-code="r.code"
                :data-clause="r.clause ?? ''"
                :data-assignment="r.assignment ?? ''"
              >
                <span :class="[CHIP_BASE, reasonChipCls, reasonChip(r).cls]">{{ reasonChip(r).label }}</span>
                <span class="font-mono font-medium">{{ r.clause ?? r.code }}</span>
                <span v-if="reasonKind(r) === 'unavailable'" class="font-mono text-ink-2">{{ r.code }}</span>
                <RouterLink v-if="reasonIncidentId(r)" :to="{ name: 'incident', params: { id: reasonIncidentId(r) } }" class="font-mono text-accent hover:underline">
                  incident {{ shortId(reasonIncidentId(r)) }}
                </RouterLink>
                <span v-else-if="reasonValueLabel(r)" class="font-mono text-ink-2">{{ reasonValueLabel(r) }}</span>
                <span class="flex-1"></span>
                <span v-if="reasonKind(r) === 'matched' && latest.policy_revision != null" class="text-[12px] text-ink-3">assigned <b>{{ r.assignment }}</b> in revision {{ latest.policy_revision }}</span>
                <span v-if="r.source" class="text-[12px] text-ink-3" :title="'the owner the fact came from'">{{ r.source }}</span>
                <a v-if="r.docs" :href="r.docs" class="text-[12px] text-accent hover:underline" target="_blank" rel="noreferrer">docs</a>
              </li>
              <li v-if="!latest.reasons.length" class="px-4 py-[11px] text-[12.5px] text-ink-3" data-testid="gate-latest-no-reasons">Nothing matched and nothing was unavailable.</li>
            </ul>
          </div>

          <div v-if="staleReason && latest.seal_lag != null" :class="WARN_BOX" data-testid="gate-latest-stale-warning">
            <span aria-hidden="true">⚠</span>
            <span>
              <b>Facts are {{ durationShort(latest.seal_lag) }} behind.</b> The service's materializer is not sealing; the gate keeps answering UNKNOWN
              until it catches up<template v-if="latest.max_seal_lag_seconds != null"> to within {{ durationShort(latest.max_seal_lag_seconds) }}</template>.
              Deficient evidence is never rewritten as a known block.
            </span>
          </div>

          <div v-if="latest.override" :class="INFO_BOX" data-testid="gate-latest-override">
            <span aria-hidden="true">ℹ</span>
            <span>
              <b>Without the override the pipeline would have been told <span data-testid="gate-latest-unoverridden">{{ latest.unoverridden_action ?? "—" }}</span></b>
              (<span class="font-mono">unoverridden_action</span>). The override changes the action only: state, reasons and the ledger row say
              <span class="font-mono">{{ latest.state }}</span>. By <span class="font-mono">{{ latest.override.actor_label }}</span> —
              “{{ latest.override.reason }}” — until {{ sealedLabel(latest.override.expires_at) }}.
            </span>
          </div>
        </div>
      </div>

      <!-- ── Screen 3: the override panel — only when a policy exists ─────────────────────── -->
      <div v-if="policyStatus === 'ok'" class="flex flex-col gap-3 border-t border-border p-4" data-testid="gate-override-panel">
        <div class="flex flex-wrap items-center gap-[10px]">
          <h3 class="text-[13px] font-semibold">Override</h3>
          <span class="text-[12px] text-ink-3">turns a BLOCK into ALLOW and leaves WARN and ALLOW alone; the state and the reasons never change</span>
          <div class="flex-1"></div>
          <RouterLink :to="{ path: `/services/${serviceId}/gate/overrides` }" class="font-mono text-[11.5px] text-accent hover:underline" data-testid="gate-override-history">
            override history →
          </RouterLink>
        </div>

        <p v-if="overrideStatus === 'loading'" class="text-[13px] text-ink-3" data-testid="gate-override-loading">Loading…</p>
        <p v-else-if="overrideStatus === 'error'" class="text-[13px] text-down" data-testid="gate-override-read-error">{{ overrideReadError }}</p>

        <div v-if="override" class="rounded-[7px] border border-border" data-testid="gate-override-active">
          <div class="flex flex-wrap items-center gap-[10px] border-b border-border px-4 py-[10px]">
            <h4 class="text-[13px] font-semibold">Active override</h4>
            <span :class="chipAcc">in force</span>
            <div class="flex-1"></div>
            <span :class="chipMono">bound to rev {{ override.policy_revision }}</span>
          </div>
          <dl class="grid grid-cols-[max-content_1fr] gap-x-[18px] gap-y-[6px] p-4 text-[13px]">
            <dt class="text-ink-3">Reason</dt>
            <dd data-testid="gate-override-reason">{{ override.reason }}</dd>
            <dt class="text-ink-3">Created by</dt>
            <dd class="font-mono" data-testid="gate-override-actor">{{ override.actor_label }}</dd>
            <dt class="text-ink-3">Created</dt>
            <dd class="font-mono" data-testid="gate-override-created">{{ sealedLabel(override.created_at) }} <span class="text-ink-3">· {{ relTime(override.created_at) }}</span></dd>
            <dt class="text-ink-3">Expires</dt>
            <dd class="font-mono" data-testid="gate-override-expires">{{ sealedLabel(override.expires_at) }} <span class="text-ink-3">· {{ untilLabel(override.expires_at, now) }}</span></dd>
          </dl>
          <div v-if="canOverride" class="flex flex-wrap items-center gap-[10px] border-t border-border px-4 py-[10px]">
            <template v-if="!revokeConfirming">
              <button type="button" :class="BTN_SM_DANGER" :disabled="revoking || blocked" data-testid="gate-override-revoke" @click="revokeConfirming = true">
                Revoke this override
              </button>
            </template>
            <template v-else>
              <span class="text-[12.5px] text-ink-2">Revoke it? The next <span class="font-mono">gate check</span> is answered without it.</span>
              <button type="button" :class="BTN_SM_DANGER" :disabled="revoking || blocked" data-testid="gate-override-revoke-confirm" @click="revoke">
                {{ revoking ? "Revoking…" : "Revoke" }}
              </button>
              <button type="button" :class="BTN_SM" :disabled="revoking" data-testid="gate-override-revoke-cancel" @click="revokeConfirming = false">Keep it</button>
            </template>
          </div>
        </div>

        <div v-if="ovError" :class="ERR_BOX" role="alert">
          <span aria-hidden="true">⚠</span>
          <span class="flex-1" data-testid="gate-override-error">{{ ovError }}</span>
          <button v-if="conflict === 'override'" type="button" class="flex-none rounded-[5px] border border-down bg-surface px-2 py-[2px] text-[11.5px]" data-testid="gate-reload" @click="reload">Reload</button>
        </div>

        <!-- Add an override: project_admin only. An editor is told who can; a viewer sees nothing. -->
        <form v-if="canOverride" class="flex flex-col gap-3 rounded-[7px] border border-border p-4" data-testid="gate-override-form" novalidate @submit.prevent="createOverride">
          <div class="flex flex-wrap items-center gap-[10px]">
            <h4 class="text-[13px] font-semibold">Add an override</h4>
            <div class="flex-1"></div>
            <span :class="chipMono">bound to rev {{ policy?.revision }}</span>
          </div>
          <div class="flex flex-col gap-[5px]">
            <label class="text-[12.5px] font-medium text-ink-2" for="gate-override-reason">Why the next BLOCK may pass</label>
            <textarea
              id="gate-override-reason"
              v-model="ovReason"
              rows="2"
              maxlength="500"
              class="rounded-sm border bg-surface px-[10px] py-[7px] text-[13px] text-ink outline-none focus:border-accent disabled:opacity-60"
              :class="ovTouched && ovErrors.reason ? 'border-down' : 'border-border'"
              placeholder="Required. Recorded on every decision it changes and in the audit log."
              :disabled="ovCreating"
              data-testid="gate-override-input-reason"
              @input="ovTouched = true"
            ></textarea>
            <span v-if="ovTouched && ovErrors.reason" class="text-[12px] text-down" data-testid="gate-override-field-error-reason">{{ ovErrors.reason }}</span>
          </div>
          <div class="flex flex-col gap-[5px]">
            <label class="text-[12.5px] font-medium text-ink-2" for="gate-override-until">Until</label>
            <span class="flex flex-wrap items-center gap-[8px]">
              <input
                id="gate-override-until"
                v-model="ovUntil"
                type="datetime-local"
                :max="ovMax"
                :class="[INPUT, 'font-mono', ovTouched && ovErrors.until ? 'border-down' : '']"
                :disabled="ovCreating"
                data-testid="gate-override-input-until"
                @input="ovTouched = true"
              />
              <span class="text-[12px] text-ink-3">· max 7 days · a hard maximum, no default</span>
            </span>
            <span v-if="ovTouched && ovErrors.until" class="text-[12px] text-down" data-testid="gate-override-field-error-until">{{ ovErrors.until }}</span>
          </div>
          <div v-if="ovBlockedByActive && override" :class="WARN_BOX" data-testid="gate-override-blocked">
            <span aria-hidden="true">⚠</span>
            <span>
              <b>One override at a time.</b> An active override exists (<span class="font-mono">{{ override.actor_label }}</span>, until
              {{ sealedLabel(override.expires_at) }}). Revoke it first — creating another returns <span class="font-mono">409 override_active</span>.
            </span>
          </div>
          <div>
            <button type="submit" :class="BTN_PRI" :disabled="!canCreate" data-testid="gate-override-create">{{ ovCreating ? "Creating…" : "Create override" }}</button>
          </div>
        </form>
        <p v-else-if="canPolicyWrite" class="text-[12.5px] text-ink-3" data-testid="gate-override-absent">Overrides are created by project admins.</p>
      </div>

      <!-- ── What a pipeline sees: the exact command for THIS service, by id ─────────────── -->
      <div class="flex flex-col gap-3 border-t border-border p-4" data-testid="gate-cli">
        <div class="flex flex-wrap items-center gap-[10px]">
          <h3 class="text-[13px] font-semibold">What a pipeline sees</h3>
          <div class="flex-1"></div>
          <span v-if="policyStatus === 'none'" :class="chipMono">exit 4 today</span>
        </div>
        <pre class="tnum overflow-x-auto rounded-sm border border-border bg-inset px-[14px] py-3 font-mono text-[12.5px] leading-[1.55] text-ink" data-testid="gate-cli-command">{{ command }}</pre>
        <p class="text-[12px] text-ink-3">
          <span class="font-mono">CERBIX_TOKEN</span> is an API token with a role on this project, from the environment only — never a flag.
          The stdout line is <span class="font-mono">state=… [action=…] [override=…] decision=…</span>; reasons go to stderr;
          <span class="font-mono">--json</span> emits the API response verbatim.
        </p>
        <div class="grid gap-[8px] [grid-template-columns:repeat(auto-fit,minmax(190px,1fr))]">
          <div v-for="e in CLI_EXITS" :key="e.code" class="flex flex-col gap-[4px] rounded-[7px] border border-border p-[10px] text-[12.5px] text-ink-2">
            <span class="font-mono text-[12px] text-ink">{{ e.code }}</span>
            <span>{{ e.text }}</span>
          </div>
        </div>
      </div>
    </template>

    <!-- ── The delete confirmation (screen 2): what changes, what is kept, the numbers ────── -->
    <div
      v-if="deleteOpen && policy"
      class="fixed inset-0 z-50 grid place-items-center bg-[rgba(10,10,20,0.5)] p-5"
      @click.self="closeDelete"
      @keydown.esc="closeDelete"
    >
      <div class="w-full max-w-[560px] overflow-hidden rounded border border-border-strong bg-surface shadow-lg" role="dialog" aria-modal="true" aria-labelledby="gate-delete-title" data-testid="gate-delete-dialog">
        <div class="flex flex-wrap items-center gap-[10px] border-b border-border px-4 py-3">
          <h3 id="gate-delete-title" class="text-[14px] font-semibold">Delete the gate policy<template v-if="serviceSlug"> for <span class="font-mono">{{ serviceSlug }}</span></template>?</h3>
          <div class="flex-1"></div>
          <span :class="chipMono">DELETE …/gate/policy?expected_revision={{ policy.revision }}</span>
        </div>
        <div class="flex flex-col gap-[14px] p-4">
          <p class="text-[13px] text-ink-2">This is what changes, in one transaction:</p>
          <div class="rounded-[7px] border border-border text-[13px]">
            <div :class="ROW"><span :class="chipDorm">then</span><span>Every later <span class="font-mono">gate check</span> answers <b>NOT_CONFIGURED</b> (exit 4) until a policy is saved again.</span></div>
            <div :class="ROW">
              <span :class="chipDorm">then</span>
              <span v-if="override">The active override by <span class="font-mono">{{ override.actor_label }}</span> is closed as <span class="font-mono">policy_deleted</span> and reads as <b>inert</b> in the history.</span>
              <span v-else>No override is active, so none is closed.</span>
            </div>
            <div :class="ROW"><span :class="chipDorm">kept</span><span>Every decision already in the ledger stays exactly as recorded.</span></div>
            <div :class="ROW">
              <span :class="chipDorm">kept</span>
              <span>The revision count continues: this delete makes the tombstone <b>revision {{ policy.revision + 1 }}</b>, and the next saved policy is <b>revision {{ policy.revision + 2 }}</b> — never 1 again.</span>
            </div>
          </div>
          <div v-if="deleteError" :class="ERR_BOX" role="alert">
            <span aria-hidden="true">⚠</span>
            <span class="flex-1" data-testid="gate-delete-error">{{ deleteError }}</span>
            <button v-if="conflict === 'delete'" type="button" class="flex-none rounded-[5px] border border-down bg-surface px-2 py-[2px] text-[11.5px]" data-testid="gate-reload" @click="reload">Reload</button>
          </div>
          <div class="flex items-center gap-[10px]">
            <button type="button" :class="BTN_DANGER" :disabled="deleting || blocked" data-testid="gate-delete-confirm" @click="confirmDelete">{{ deleting ? "Deleting…" : "Delete policy" }}</button>
            <button ref="deleteCancelBtn" type="button" :class="BTN" :disabled="deleting" data-testid="gate-delete-cancel" @click="closeDelete">Keep it</button>
          </div>
        </div>
      </div>
    </div>
  </section>
</template>
