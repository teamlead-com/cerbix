<script setup lang="ts">
// FR-025 D-0210 item 3, mock screen 3 ("Before and after"): the SLI on each side of ONE change's
// instant, from sealed minutes only — the reliability page's own number (D8, NFR-020).
//
// The comparison is addressed by the change's IDENTITY `(source, external_id)` and a horizon —
// there is no by-identity group route, so this view knows the group only through the compare
// response (kind, ref, terminal phase, T). Each side is a figure, or WITHHELD with the page's own
// reason word, or PENDING with `sealed_through` stated — EITHER side may be pending (D-0211) — and
// never a partial number; `delta` exists only when both sides are figures. Nothing here is causal.
//
// The horizon lives in the URL (`?horizon=`) so a row's link and a shared link mean the same
// thing; the control writes it back with `router.replace`, and the route watcher re-reads. An
// unfamiliar horizon is SENT as given and the server's `horizon_invalid` is rendered — never
// silently corrected into a comparison the link did not ask for.
//
// Read-only for viewer+ (`project:read`): no role is consulted, nothing is written. Concurrency
// discipline as GateOverridesView.vue / ServiceGate.vue: one generation and one AbortController
// per load; a route or workspace change and unmount abort what is in flight; a late answer is
// dropped, never applied.
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { RouterLink, useRoute, useRouter } from "vue-router";

import { api } from "@/api/client";
import type { components } from "@/api/schema";
import AppShell from "@/components/AppShell.vue";
import {
  DEFAULT_HORIZON,
  HORIZONS,
  WITHHELD_HINT,
  clockLabel,
  deltaChip,
  deltaValueClass,
  describeChangeFailure,
  durationsLine,
  formatCompareSide,
  horizonLabel,
  kindClip,
  kindLabel,
  secondsLabel,
  sideBar,
  windowLabel,
  withheldLabel,
  type ChangeCompare,
  type ChangeCompareSide,
  type ChangeHorizon,
  type CompareSideView,
  requestScope,
} from "@/lib/changes";
import { CHIP_BASE, CHIP_PLAIN, describeFailure, failureOf, isAbort, transportFailure } from "@/lib/gate";
import { sealedLabel } from "@/lib/services";
import { useWorkspace } from "@/stores/workspace";

type Detail = components["schemas"]["ServiceDetail"];

const ws = useWorkspace();
const route = useRoute();
const router = useRouter();

const q = (v: unknown): string => (typeof v === "string" ? v : Array.isArray(v) && typeof v[0] === "string" ? v[0] : "");
const serviceId = computed(() => String(route.params.id || ""));
const source = computed(() => q(route.query.source));
const externalId = computed(() => q(route.query.external_id));
/** The query's horizon VERBATIM, `1h` when absent — an unfamiliar value is the server's 400 to render. */
const horizon = computed(() => q(route.query.horizon) || DEFAULT_HORIZON);

const service = ref<Detail | null>(null);
const cmp = ref<ChangeCompare | null>(null);
const loading = ref(true);
const error = ref("");
// Which failure the error line describes: an HTTP status, 0 for the transport, "client" for a
// link this view refused before any request. Exposed as data-status so a test can tell them apart.
const errorStatus = ref<number | "client" | null>(null);

// The generation guard, the abort set and the 10 s DEADLINE all come from the shared scope. This
// view is the third caller (the card and the timeline are the others), and it was left out when
// the scope moved into `lib/changes.ts` — so a service that answered and a comparison that never
// settled held this page on "Loading…" for good, while the commit that moved the scope claimed
// every comparison was bounded in time. Review [42].
const scope = requestScope();
const stale = (mine: number) => scope.stale(mine);
onBeforeUnmount(() => scope.close());

const SERVICE_GONE = "This service does not exist, or you cannot see it.";
const CHANGE_GONE = "This change does not exist on this service, or you cannot see it.";
const LINK_INCOMPLETE = "This link names no change — it needs a source and an external_id.";

async function load() {
  const mine = scope.begin();
  loading.value = true;
  error.value = "";
  errorStatus.value = null;
  cmp.value = null;
  try {
    await ws.init();
    if (stale(mine)) return;
    if (!ws.projectId || !serviceId.value) return;
    const path = { projectID: ws.projectId, serviceID: serviceId.value };
    const svcReq = scope.request<Detail>(mine, (signal: AbortSignal) =>
      api.GET("/api/v1/projects/{projectID}/services/{serviceID}", { params: { path }, signal }),
    );
    if (!source.value || !externalId.value) {
      // Refused here, before any comparison is asked; the service is still read for the header.
      error.value = LINK_INCOMPLETE;
      errorStatus.value = "client";
      const svc = await svcReq;
      if (svc.kind === "stale") return;
      if (svc.kind === "ok" && svc.data) service.value = svc.data;
      return;
    }
    const cmpReq = scope.request<ChangeCompare>(mine, (signal: AbortSignal) =>
      api.GET("/api/v1/projects/{projectID}/services/{serviceID}/changes/compare", {
        // The cast sends the query's value as given (see `horizon` above).
        params: { path, query: { source: source.value, external_id: externalId.value, horizon: horizon.value as ChangeHorizon } },
        signal,
      }),
    );
    // Both requests are already in flight; what follows decides only the ORDER OF DECISION, and it
    // is the subject that decides. `await Promise.all([svcReq, cmpReq])` made the page wait for an
    // ANCILLARY answer before acting on the authoritative one, so a comparison that never settles —
    // a blackholed transport, not merely a slow one — held `loading` true forever while a 404 or a
    // 403 for the service was already in hand and would never be rendered (review [31]).
    const svc = await svcReq;
    if (svc.kind === "stale") return;
    if (svc.kind === "failed" || !svc.data) {
      // The service is the subject; without it the comparison has no header to sit under, so stop
      // waiting for one. `scope.abort()` abandons it AT ONCE — not at its deadline — while leaving
      // this generation live, so the 404 below is still rendered and `loading` still clears.
      scope.abort();
      const f = svc.kind === "failed" ? svc.failure : failureOf({});
      error.value = describeFailure(f, { notFound: SERVICE_GONE, denied: "You cannot see this service." });
      errorStatus.value = f.status;
      return;
    }
    service.value = svc.data;
    const res = await cmpReq;
    if (res.kind === "stale") return;
    if (res.kind === "failed" || !res.data) {
      const f = res.kind === "failed" ? res.failure : failureOf({});
      error.value = describeChangeFailure(f, { notFound: CHANGE_GONE, denied: "You cannot read this service's changes." });
      errorStatus.value = f.status;
      return;
    }
    cmp.value = res.data;
  } catch (e) {
    if (stale(mine) || isAbort(e)) return;
    error.value = transportFailure(e);
    errorStatus.value = 0;
  } finally {
    if (!stale(mine)) loading.value = false;
  }
}

/** The horizon control writes the URL; the route watcher below re-reads under a new generation. */
function setHorizon(h: ChangeHorizon) {
  if (h === horizon.value) return;
  void router.replace({ query: { ...route.query, horizon: h } });
}

onMounted(load);
watch(() => [route.params.id, route.query.source, route.query.external_id, route.query.horizon, ws.projectId], load);

// ── Derived ───────────────────────────────────────────────────────────────────────────────────
const name = computed(() => service.value?.service.name || "");
const title = computed(() => cmp.value?.ref || externalId.value || "");

interface SideCell {
  key: "before" | "after";
  label: string;
  order: number;
  side: ChangeCompareSide;
  view: CompareSideView;
  bar: { good: number; bad: number; unknown: number } | null;
  durations: string;
}
const sides = computed<SideCell[]>(() => {
  const c = cmp.value;
  if (!c) return [];
  const cell = (key: SideCell["key"], label: string, order: number, side: ChangeCompareSide): SideCell => {
    const view = formatCompareSide(side);
    return {
      key,
      label,
      order,
      side,
      view,
      bar: view.kind === "figure" ? sideBar(view) : null,
      durations: view.kind === "figure" ? durationsLine(view) : "",
    };
  };
  return [cell("before", "Before", 1, c.before), cell("after", "After", 3, c.after)];
});
const delta = computed(() => deltaChip(cmp.value?.delta));
const bothFigures = computed(() => sides.value.length === 2 && sides.value.every((s) => s.view.kind === "figure"));
const anyPending = computed(() => sides.value.some((s) => s.view.kind === "pending"));
const timelinePath = computed(() => `/services/${serviceId.value}/changes`);

// ── Shared classes (token-mapped; see tailwind.config.js) ─────────────────────────────────────
const chipMono = `${CHIP_BASE} ${CHIP_PLAIN} font-mono text-[10.5px]`;
const LBL = "text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3";
const ERR_BOX = "flex items-start gap-[9px] rounded-sm border border-down bg-down-weak px-3 py-[9px] text-[13px] text-down";
const INFO_BOX = "flex items-start gap-[9px] rounded-sm border border-border-strong bg-surface-2 px-3 py-[9px] text-[13px] text-ink-2";
const SEG = "inline-flex overflow-hidden rounded-sm border border-border";
const SEG_ITEM = "border-r border-border px-[10px] py-[3px] text-[11.5px] last:border-r-0";
const SEG_ON = "bg-accent-weak font-semibold text-accent";
const SEG_OFF = "text-ink-3 hover:text-ink";
const SIDE = "flex flex-col gap-[6px] rounded-[7px] border p-[12px]";
const KPI = "font-mono text-[26px] font-medium tracking-tight";
const KPI_SMALL = "ml-[6px] text-[12px] font-normal tracking-normal text-ink-3";
</script>

<template>
  <AppShell active="services" :crumbs="[ws.orgName || 'cerbix', ws.projectName || '…', 'Services', name || '…', 'before and after']">
    <div class="mx-auto max-w-[1180px] px-[22px] pb-16 pt-[26px]" data-testid="change-compare" :data-source="source" :data-external-id="externalId" :data-horizon="horizon">
      <RouterLink :to="{ name: 'service', params: { id: serviceId } }" class="text-[12.5px] text-ink-3 hover:text-accent" data-testid="compare-back">← back to the service</RouterLink>

      <div class="mb-[22px] mt-[10px]">
        <h1 class="text-[21px] font-semibold tracking-tight">
          {{ name || "…" }} · <span class="font-mono">{{ title || "…" }}</span> · before and after
        </h1>
        <p class="mt-[3px] text-[13px] text-ink-3">
          The SLI on each side of the change's instant, from sealed minutes only — the reliability page's own number ·
          <span class="font-mono">GET …/services/{s}/changes/compare?source&amp;external_id&amp;horizon</span>
        </p>
      </div>

      <section class="overflow-hidden rounded border border-border bg-surface shadow-card">
        <header class="flex flex-wrap items-center gap-[10px] border-b border-border px-4 py-[13px]">
          <!-- Kind: shape + text in the accent, never a state hue. -->
          <span v-if="cmp" class="inline-flex items-center gap-[6px] text-[12.5px] font-medium text-ink" data-testid="compare-kind" :data-kind="cmp.kind">
            <i class="inline-block h-[10px] w-[10px] flex-none bg-accent" :style="{ clipPath: kindClip(cmp.kind) }" aria-hidden="true"></i>{{ kindLabel(cmp.kind) }}
          </span>
          <h2 class="font-mono text-[14px] font-semibold" data-testid="compare-ref">{{ title || "…" }}</h2>
          <span v-if="cmp" :class="chipMono" data-testid="compare-terminal" :data-phase="cmp.terminal_phase" :title="`the terminal phase row ${cmp.change_id}`">{{ cmp.terminal_phase }}</span>
          <span v-if="cmp" :class="chipMono" data-testid="compare-t" :title="'T is the terminal phase’s occurred_at floored to the canonical minute'">T = {{ sealedLabel(cmp.t) }}</span>
          <span :class="chipMono" data-testid="compare-identity">{{ source || "?" }} · {{ externalId || "?" }}</span>
          <div class="flex-1"></div>
          <span :class="LBL">horizon</span>
          <span :class="SEG" role="group" aria-label="horizon" data-testid="compare-horizon" :data-horizon="horizon">
            <button
              v-for="h in HORIZONS"
              :key="h"
              type="button"
              :class="[SEG_ITEM, h === horizon ? SEG_ON : SEG_OFF]"
              :aria-pressed="h === horizon"
              :data-testid="`compare-horizon-${h}`"
              :data-horizon="h"
              @click="setHorizon(h)"
            >{{ horizonLabel(h) }}</button>
          </span>
        </header>

        <div class="flex flex-col gap-[14px] p-4">
          <div v-if="error" :class="ERR_BOX" role="alert" data-testid="compare-error" :data-status="errorStatus ?? undefined">
            <span aria-hidden="true">⚠</span>
            <span class="flex-1">{{ error }}</span>
          </div>
          <p v-else-if="loading" class="text-[13px] text-ink-3" data-testid="compare-loading">Loading…</p>

          <template v-else-if="cmp">
            <div class="grid gap-[14px] [grid-template-columns:1fr_auto_1fr] max-[760px]:grid-cols-1" data-testid="compare-sides">
              <!-- Each side EXACTLY one of three shapes; the figure is plain ink — only the Δ carries a sign's hue. -->
              <div
                v-for="s in sides"
                :key="s.key"
                :class="[SIDE, s.view.kind === 'figure' ? 'border-border' : 'border-dashed border-border-strong']"
                :style="{ order: s.order }"
                :data-testid="`compare-${s.key}`"
                :data-kind="s.view.kind"
                :data-withheld="s.view.kind === 'withheld' ? s.view.reason : undefined"
                :data-pending="s.view.kind === 'pending' ? 'true' : undefined"
              >
                <div :class="LBL" :title="`${sealedLabel(s.side.from)} → ${sealedLabel(s.side.to)}`">{{ s.label }} · {{ windowLabel(s.side.from, s.side.to) }}</div>

                <template v-if="s.view.kind === 'figure'">
                  <div :class="KPI" :data-testid="`compare-${s.key}-figure`">
                    {{ s.view.text }}<small :class="KPI_SMALL">{{ s.view.buckets }} min · bad {{ secondsLabel(s.view.bad) }}</small>
                  </div>
                  <div v-if="s.bar" class="flex h-[8px] overflow-hidden rounded-[4px] bg-inset" role="img" :aria-label="s.durations" :data-testid="`compare-${s.key}-bar`">
                    <span class="h-full bg-up" :style="{ width: s.bar.good + '%' }"></span>
                    <span class="h-full bg-down" :style="{ width: s.bar.bad + '%' }"></span>
                    <span class="h-full bg-ink-3 opacity-50" :style="{ width: s.bar.unknown + '%' }"></span>
                  </div>
                  <div class="text-[12px] text-ink-3" :data-testid="`compare-${s.key}-durations`">{{ s.durations }}</div>
                </template>

                <template v-else-if="s.view.kind === 'pending'">
                  <div :class="[KPI, 'text-ink-3']" :data-testid="`compare-${s.key}-figure`">
                    pending<small v-if="s.view.sealedThrough" :class="KPI_SMALL">sealed through {{ sealedLabel(s.view.sealedThrough) }}</small>
                  </div>
                  <div class="text-[12px] text-ink-3">the seal must pass {{ clockLabel(s.side.to) }} before this side is quoted; nothing partial is shown</div>
                </template>

                <template v-else>
                  <div :class="[KPI, 'text-ink-3']" :data-testid="`compare-${s.key}-figure`">
                    —<small :class="KPI_SMALL">withheld · {{ withheldLabel(s.view.reason) }}</small>
                  </div>
                  <div class="text-[12px] text-ink-3">{{ s.view.detail || WITHHELD_HINT[s.view.reason] || "the page would not quote this range" }}</div>
                </template>
              </div>

              <!-- Δ: present ONLY when both sides are figures; coloured by its sign and by nothing else. -->
              <div class="flex min-w-[110px] flex-col items-center justify-center gap-[4px]" style="order: 2" data-testid="compare-delta" :data-sign="delta ? delta.sign : undefined" :data-present="delta ? 'true' : 'false'">
                <div :class="LBL">Δ availability</div>
                <div v-if="delta" class="font-mono text-[22px] font-semibold tracking-tight" :class="deltaValueClass(delta.sign)">{{ delta.text }}</div>
                <div v-else class="font-mono text-[22px] font-semibold text-ink-3">—</div>
                <div class="text-[12px] text-ink-3">{{ delta ? "points" : "both sides must be figures" }}</div>
              </div>
            </div>

            <div :class="INFO_BOX" data-testid="compare-info">
              <span aria-hidden="true">ℹ</span>
              <div>
                <b class="font-semibold">These are the minutes the reliability page shows for the same range.</b>
                <template v-if="bothFigures && cmp.sealed_through"> Both sides are fully sealed (<span class="font-mono">sealed_through {{ sealedLabel(cmp.sealed_through) }}</span>).</template>
                <template v-else-if="anyPending && cmp.sealed_through"> The seal stands at <span class="font-mono">{{ sealedLabel(cmp.sealed_through) }}</span>; a pending side is quoted once it has passed that side's end — at a shorter horizon both sides may already be sealed.</template>
                <template v-else-if="cmp.sealed_through"> <span class="font-mono">sealed_through {{ sealedLabel(cmp.sealed_through) }}</span>.</template>
                <template v-else> Nothing is sealed for this service yet.</template>
                Correlation is not causation: the card says <b class="font-semibold">preceded</b>, and only that — the incidents this change preceded are on the
                <RouterLink :to="timelinePath" class="text-accent hover:underline" data-testid="compare-timeline-link">timeline</RouterLink>.
                Snapshot <span class="font-mono">{{ sealedLabel(cmp.as_of) }}</span>.
              </div>
            </div>
          </template>
        </div>
      </section>
    </div>
  </AppShell>
</template>
