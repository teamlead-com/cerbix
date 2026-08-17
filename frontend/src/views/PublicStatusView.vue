<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { useRoute } from "vue-router";
import { api } from "@/api/client";
import type { components } from "@/api/schema";
import { useTheme } from "@/composables/useTheme";
import { componentMeta, summaryHeadline, withheldText } from "@/lib/statuspage";
import { useBranding } from "@/stores/branding";
import { impactBadge, relTime, statusBadge } from "@/lib/incident";
import { renderSections } from "@/lib/postmortem";

type Render = components["schemas"]["StatusPageRender"];
type ComponentView = components["schemas"]["ComponentView"];
type ComponentDay = components["schemas"]["ComponentDay"];

const route = useRoute();
const { toggle } = useTheme();
const branding = useBranding(); // loaded app-wide in App.vue (public endpoint)
const slug = route.params.slug as string;
const token = (route.query.token as string) || "";
const previewID = (route.query.preview as string) || "";
const internalPreview = ref(false);

const loading = ref(true);
const notFound = ref(false);
const page = ref<Render | null>(null);

async function load() {
  loading.value = true;
  const res = await api.GET("/api/v1/public/status-pages/{slug}", {
    params: { path: { slug }, query: token ? { token } : {} },
  });
  if (res.error || !res.data) {
    // Internal pages 404 publicly by design; signed-in members preview them
    // through the authed render endpoint when the editor link carries the id.
    if (previewID) {
      const auth = await api.GET("/api/v1/status-pages/{pageID}/render", {
        params: { path: { pageID: previewID } },
      });
      if (!auth.error && auth.data) {
        page.value = auth.data;
        internalPreview.value = true;
        loading.value = false;
        return;
      }
    }
    notFound.value = true;
  } else {
    page.value = res.data;
  }
  loading.value = false;
}

function pct(n?: number | null) {
  return n === null || n === undefined ? null : `${n.toFixed(2)}%`;
}
// In internal-preview mode the public feed 404s — use the authed feed endpoint.
const feed = (fmt: string) =>
  internalPreview.value && previewID
    ? `/api/v1/status-pages/${previewID}/feed?format=${fmt}`
    : `/api/v1/public/status-pages/${slug}/feed?format=${fmt}${token ? "&token=" + token : ""}`;

// Banner sub-line derived from the page summary (no dedicated backend copy field).
//
// The unmeasured half is stated FIRST when it exists, because "all monitored services are running
// normally" beside two components nobody measured is the sentence FR-021 §17 set out to remove.
const summarySub = computed(() => {
  const unmeasured = page.value?.unmeasured_count ?? 0;
  switch (page.value?.summary_state) {
    case "empty":
      return "This page has no components yet, so there is nothing to report.";
    case "no_data":
      return unmeasured === 1
        ? "The one component on this page has no measurement yet."
        : "None of the components on this page have a measurement yet.";
  }
  const tail = unmeasured > 0
    ? ` ${unmeasured} component${unmeasured === 1 ? "" : "s"} on this page ${unmeasured === 1 ? "has" : "have"} no measurement.`
    : "";
  switch (page.value?.summary) {
    case "operational":
      return "All measured services are running normally." + tail;
    case "degraded":
      return "Some services are experiencing elevated latency. We’re on it." + tail;
    case "partial_outage":
      return "Some services are partially unavailable." + tail;
    case "major_outage":
      return "A major outage is affecting one or more services." + tail;
    case "maintenance":
      return "Scheduled maintenance is in progress." + tail;
    default:
      return "Live status of every monitored service." + tail;
  }
});
const updatedUTC = computed(() => {
  if (!page.value?.updated_at) return "";
  return new Date(page.value.updated_at).toISOString().slice(11, 16) + " UTC";
});

// Past-incidents accordion: which incident is expanded.
const expanded = ref<Set<string>>(new Set());
function toggleInc(id?: string) {
  if (!id) return;
  const s = new Set(expanded.value);
  if (s.has(id)) s.delete(id);
  else s.add(id);
  expanded.value = s;
}
// Latest (most recent) update of an incident — its current communicated state.
type Upd = { id?: string; status?: string; body?: string; created_at?: string };
function lastUpdate(updates?: Upd[]): Upd | null {
  return updates && updates.length ? updates[updates.length - 1] : null;
}
// Human-readable incident duration (started → resolved).
function duration(startISO?: string | null, endISO?: string | null): string {
  if (!startISO || !endISO) return "";
  const ms = new Date(endISO).getTime() - new Date(startISO).getTime();
  if (!(ms > 0)) return "";
  const mins = Math.round(ms / 60000);
  if (mins < 60) return `${mins} min`;
  const h = Math.floor(mins / 60);
  const rm = mins % 60;
  if (h < 24) return rm ? `${h}h ${rm}m` : `${h}h`;
  const d = Math.floor(h / 24);
  const rh = h % 24;
  return rh ? `${d}d ${rh}h` : `${d}d`;
}

// Group components by their `group` field, preserving first-seen order.
const groups = computed(() => {
  const order: string[] = [];
  const map = new Map<string, ComponentView[]>();
  for (const c of page.value?.components ?? []) {
    const key = c.group || "Services";
    if (!map.has(key)) {
      map.set(key, []);
      order.push(key);
    }
    map.get(key)!.push(c);
  }
  return order.map((name) => ({ name, comps: map.get(name)! }));
});

// Solid strip / meter color by status (a status color, not a *-weak token).
function meterColor(s?: string) {
  const m = componentMeta(s);
  return m.dot; // bg-up / bg-degraded / bg-down / bg-maint
}
function incidentStrip(s?: string) {
  return { investigating: "bg-degraded", identified: "bg-degraded", monitoring: "bg-maint", resolved: "bg-up" }[s ?? ""] || "bg-degraded";
}

// Build a 90-slot strip (oldest→newest) from a component's sparse daily rows.
function strip(daily?: ComponentDay[]): { pct: number | null; label: string }[] {
  const byDay = new Map<string, ComponentDay>();
  for (const d of daily ?? []) if (d.day) byDay.set(d.day.slice(0, 10), d);
  const out: { pct: number | null; label: string }[] = [];
  const today = new Date();
  for (let i = 89; i >= 0; i--) {
    const dt = new Date(today);
    dt.setUTCDate(today.getUTCDate() - i);
    const key = dt.toISOString().slice(0, 10);
    const d = byDay.get(key);
    out.push({ pct: d && d.total ? (d.uptime_percent ?? 0) : null, label: key });
  }
  return out;
}
function daySegClass(p: number | null): string {
  if (p === null) return "bg-inset";
  if (p >= 99.5) return "bg-up";
  if (p >= 95) return "bg-degraded";
  return "bg-down";
}
function dayTitle(d: { pct: number | null; label: string }): string {
  return d.pct === null ? `${d.label} · no data` : `${d.label} · ${d.pct.toFixed(2)}%`;
}
function fmtDay(ts?: string): string {
  return ts ? new Date(ts).toISOString().slice(0, 10) : "";
}
function maintState(m: components["schemas"]["MaintenanceWindow"]): string {
  const now = Date.now();
  if (m.starts_at && new Date(m.starts_at).getTime() > now) return "Scheduled";
  return "In progress";
}

// Email subscription.
const subEmail = ref("");
const subscribing = ref(false);
const subMsg = ref("");
const subErr = ref("");
const banner = ref("");

async function subscribe() {
  const email = subEmail.value.trim();
  if (!email) return;
  subscribing.value = true;
  subErr.value = "";
  subMsg.value = "";
  try {
    const res = await api.POST("/api/v1/public/status-pages/{slug}/subscribers", {
      params: { path: { slug }, query: token ? { token } : {} },
      body: { email },
    });
    if (res.error) {
      subErr.value = (res.error as { error?: string })?.error || "Could not subscribe.";
      return;
    }
    subMsg.value = "Check your inbox to confirm your subscription.";
    subEmail.value = "";
  } finally {
    subscribing.value = false;
  }
}

// Handle confirm / unsubscribe links landing back on this page.
async function handleLinkActions() {
  const confirm = route.query.confirm as string | undefined;
  const unsub = route.query.unsubscribe as string | undefined;
  if (confirm) {
    const res = await api.POST("/api/v1/public/subscriptions/{token}/confirm", { params: { path: { token: confirm } } });
    banner.value = res.error ? "This confirmation link is invalid or expired." : "Subscription confirmed — you’ll get status updates by email.";
  } else if (unsub) {
    const res = await api.DELETE("/api/v1/public/subscriptions/{token}", { params: { path: { token: unsub } } });
    banner.value = res.error ? "This unsubscribe link is invalid." : "You’ve been unsubscribed.";
  }
}

onMounted(async () => {
  await load();
  await handleLinkActions();
});
</script>

<template>
  <div class="min-h-screen bg-bg text-ink">
    <!-- sticky public header -->
    <header class="sticky top-0 z-10 border-b border-border bg-surface/80 backdrop-blur">
      <div class="mx-auto flex h-[60px] max-w-[820px] items-center gap-3 px-5">
        <img v-if="branding.logoUrl" :src="branding.logoUrl" alt="" class="h-[28px] w-[28px] rounded-md object-contain" />
        <span v-else class="grid h-[28px] w-[28px] place-items-center rounded-md bg-accent text-accent-ink">
          <svg viewBox="0 0 24 24" class="h-4 w-4" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 3l7 3v5c0 4.5-3 7.5-7 9-4-1.5-7-4.5-7-9V6z" /><path d="M8.5 12l2 2 4.5-4.5" /></svg>
        </span>
        <b class="text-[15px] font-semibold tracking-tight">{{ page?.title || "Status" }}</b>
        <div class="ml-auto flex items-center gap-2">
          <button class="grid h-[34px] w-[34px] place-items-center rounded-sm border border-border bg-surface text-ink-2 hover:border-border-strong hover:text-ink" type="button" aria-label="Toggle theme" @click="toggle">
            <svg viewBox="0 0 24 24" class="h-4 w-4" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="4" /><path d="M12 2v2M12 20v2M2 12h2M20 12h2M5 5l1.5 1.5M17.5 17.5L19 19M19 5l-1.5 1.5M6.5 17.5L5 19" /></svg>
          </button>
          <a :href="feed('rss')" class="inline-flex h-[34px] items-center gap-[7px] rounded-sm border border-border bg-surface px-[13px] text-[13px] font-medium text-ink hover:border-border-strong">
            <svg viewBox="0 0 24 24" class="h-[15px] w-[15px]" fill="none" stroke="currentColor" stroke-width="2"><path d="M4 11a9 9 0 0 1 9 9M4 4a16 16 0 0 1 16 16" /><circle cx="5" cy="19" r="1.4" fill="currentColor" /></svg>
            RSS
          </a>
        </div>
      </div>
    </header>

    <main class="mx-auto max-w-[820px] px-5 pb-16 pt-[26px]">
      <div v-if="internalPreview" class="mb-4 flex items-center gap-2 rounded border border-degraded/40 bg-degraded-weak px-4 py-2 text-[12.5px] font-medium text-degraded">
        🔒 Internal page — visible to signed-in members only; anonymous visitors get 404.
      </div>
      <div v-if="loading" class="text-[13px] text-ink-3">Loading…</div>

      <div v-else-if="notFound" class="rounded-lg border border-border bg-surface p-10 text-center shadow-card">
        <p class="text-[15px] font-medium">This status page is not available.</p>
        <p class="mt-1 text-[13px] text-ink-3">It may be private, or the link may be incorrect.</p>
      </div>

      <template v-else-if="page">
        <div v-if="banner" class="mb-[14px] rounded-lg border border-accent bg-accent-weak px-[18px] py-[13px] text-[13.5px] text-accent">{{ banner }}</div>

        <!-- overall summary banner -->
        <div class="mb-[14px] flex flex-wrap items-center gap-4 rounded-lg border p-[22px] shadow-card" :class="[componentMeta(page.summary).band, 'border-border']">
          <span class="grid h-[42px] w-[42px] flex-none place-items-center rounded-[11px] text-white" :class="meterColor(page.summary)">
            <svg v-if="page.summary === 'operational' && !(page.unmeasured_count ?? 0)" viewBox="0 0 24 24" class="h-[22px] w-[22px]" fill="none" stroke="currentColor" stroke-width="2.4"><path d="M20 6L9 17l-5-5" /></svg>
            <svg v-else viewBox="0 0 24 24" class="h-[22px] w-[22px]" fill="none" stroke="currentColor" stroke-width="2.2"><path d="M12 8v5M12 16h.01" /><circle cx="12" cy="12" r="9" /></svg>
          </span>
          <div class="min-w-0">
            <h1 class="m-0 text-[20px] font-semibold tracking-tight" :class="componentMeta(page.summary).text">{{ summaryHeadline(page.summary, page.summary_state, page.unmeasured_count) }}</h1>
            <div class="mt-[2px] text-[13.5px] text-ink-2">{{ summarySub }}</div>
          </div>
          <div class="ml-auto text-right font-mono text-[12px] text-ink-3">updated {{ relTime(page.updated_at) }}<br />{{ updatedUTC }}</div>
        </div>

        <!-- active incidents -->
        <div v-for="inc in page.active_incidents || []" :key="inc.id" class="mb-[14px] overflow-hidden rounded-lg border border-border bg-surface shadow-card">
          <div class="h-[3px]" :class="incidentStrip(inc.status)"></div>
          <div class="px-[18px] py-[15px]">
            <div class="mb-[6px] flex flex-wrap items-center gap-[9px]">
              <h3 class="m-0 text-[15px] font-semibold">{{ inc.title }}</h3>
              <span class="inline-flex items-center gap-[6px] rounded-full px-[9px] py-[2px] text-[11.5px] font-semibold" :class="statusBadge(inc.status).cls"><span class="h-[7px] w-[7px] rounded-full" :class="meterColor(inc.status)"></span>{{ statusBadge(inc.status).label }}</span>
              <span class="rounded-full px-[9px] py-[2px] text-[11.5px] font-semibold" :class="impactBadge(inc.impact).cls">{{ impactBadge(inc.impact).label }}</span>
            </div>
            <div class="font-mono text-[12px] text-ink-3">opened {{ relTime(inc.started_at) }} · {{ inc.source }}</div>

            <!-- latest update (current communicated state) -->
            <div v-if="lastUpdate(inc.updates)" class="mt-3 rounded-md border border-border bg-surface-2 px-3 py-[10px]">
              <div class="flex items-center gap-2">
                <span class="rounded-full px-[8px] py-[1px] text-[11px] font-semibold" :class="statusBadge(lastUpdate(inc.updates)!.status).cls">{{ statusBadge(lastUpdate(inc.updates)!.status).label }}</span>
                <span class="font-mono text-[11px] text-ink-3">{{ relTime(lastUpdate(inc.updates)!.created_at) }}</span>
              </div>
              <p v-if="lastUpdate(inc.updates)!.body" class="mt-[4px] whitespace-pre-wrap text-[13px] text-ink-2">{{ lastUpdate(inc.updates)!.body }}</p>
            </div>

            <!-- expand full timeline -->
            <button v-if="inc.updates && inc.updates.length > 1" type="button" class="mt-2 text-[12px] font-medium text-accent hover:underline" @click="toggleInc(inc.id)">
              {{ inc.id && expanded.has(inc.id) ? "Hide timeline" : `Show full timeline (${inc.updates.length})` }}
            </button>
            <div v-if="inc.id && expanded.has(inc.id)" class="mt-3 border-t border-border pt-3">
              <div v-for="u in inc.updates" :key="u.id" class="border-l-2 border-border pb-[11px] pl-3 last:pb-0">
                <div class="flex items-center gap-2">
                  <span class="rounded-full px-[8px] py-[1px] text-[11px] font-semibold" :class="statusBadge(u.status).cls">{{ statusBadge(u.status).label }}</span>
                  <span class="font-mono text-[11px] text-ink-3">{{ relTime(u.created_at) }}</span>
                </div>
                <p v-if="u.body" class="mt-[3px] whitespace-pre-wrap text-[13px] text-ink-2">{{ u.body }}</p>
              </div>
            </div>
          </div>
        </div>

        <!-- scheduled / active maintenance -->
        <div v-if="page.maintenance && page.maintenance.length" class="mb-[14px] overflow-hidden rounded-lg border border-border bg-surface shadow-card">
          <div class="border-b border-border px-[18px] py-[11px] text-[12px] font-semibold text-ink-2">Maintenance</div>
          <div v-for="m in page.maintenance" :key="m.id" class="flex items-center gap-3 border-b border-border px-[18px] py-[13px] last:border-b-0">
            <span class="grid h-[30px] w-[30px] flex-none place-items-center rounded-full bg-maint-weak text-maint">
              <svg viewBox="0 0 24 24" class="h-[15px] w-[15px]" fill="none" stroke="currentColor" stroke-width="2"><path d="M14.7 6.3a4 4 0 0 0-5.4 5.4L4 17v3h3l5.3-5.3a4 4 0 0 0 5.4-5.4l-2.3 2.3-2-2 2.3-2.3z" /></svg>
            </span>
            <div class="min-w-0">
              <div class="text-[13.5px] font-medium">{{ m.reason || "Scheduled maintenance" }}</div>
              <div class="font-mono text-[11.5px] text-ink-3">{{ fmtDay(m.starts_at) }} → {{ fmtDay(m.ends_at) }}</div>
            </div>
            <span class="ml-auto rounded-full bg-maint-weak px-[9px] py-[2px] text-[11.5px] font-medium text-maint">{{ maintState(m) }}</span>
          </div>
        </div>

        <!-- components by group -->
        <div class="mb-[10px] mt-[26px] flex items-center gap-[10px]">
          <h2 class="m-0 text-[13px] font-semibold">Current status by service</h2>
          <span class="ml-auto text-[12px] text-ink-3">90-day uptime</span>
        </div>

        <div v-for="g in groups" :key="g.name" class="mb-3 overflow-hidden rounded-lg border border-border bg-surface shadow-card">
          <div class="border-b border-border px-[18px] py-[11px] text-[12px] font-semibold text-ink-2">{{ g.name }}</div>
          <div v-for="c in g.comps" :key="c.id" class="border-b border-border px-[18px] py-[14px] last:border-b-0">
            <div class="mb-[9px] flex items-center gap-[10px]">
              <span class="text-[14px] font-medium">{{ c.name }}</span>
              <span v-if="c.description" class="min-w-0 truncate text-[12px] text-ink-3">{{ c.description }}</span>
              <span v-if="c.unavailable" class="ml-auto inline-flex items-center gap-[7px] text-[12.5px] font-semibold text-degraded" data-testid="component-unavailable">
                <svg viewBox="0 0 24 24" class="h-[13px] w-[13px]" fill="none" stroke="currentColor" stroke-width="2.2"><path d="M12 8v5M12 16h.01" /><circle cx="12" cy="12" r="9" /></svg>
                Status unavailable
              </span>
              <span v-else class="ml-auto inline-flex items-center gap-[7px] text-[12.5px] font-semibold" :class="componentMeta(c.status).text"><span class="h-[8px] w-[8px] rounded-full" :class="componentMeta(c.status).dot"></span>{{ componentMeta(c.status).label }}</span>
            </div>
            <!-- per-day 90-day availability strip (aggregate meter fallback for manual components) -->
            <div v-if="c.daily && c.daily.length" class="flex h-[26px] items-stretch gap-[2px]">
              <span v-for="(d, i) in strip(c.daily)" :key="i" class="min-w-0 flex-1 rounded-[2px]" :class="daySegClass(d.pct)" :title="dayTitle(d)"></span>
            </div>
            <div v-else-if="c.uptime_90d != null" class="h-[8px] overflow-hidden rounded-[3px] bg-inset">
              <i class="block h-full rounded-[3px]" :class="meterColor(c.status)" :style="{ width: Math.max(0, Math.min(100, c.uptime_90d ?? 0)) + '%' }"></i>
            </div>
            <!-- No history at all: an empty rail under "90 days ago … today" would claim a
                 90-day record that reads as flawless. The absence is stated, WITH its reason when
                 the server gave one — a missing number without a reason is indistinguishable from
                 one nobody computed. -->
            <p v-else class="m-0 font-mono text-[11.5px] text-ink-3" data-testid="no-history">{{ withheldText(c.withheld_reason) }}</p>
            <div v-if="(c.daily && c.daily.length) || c.uptime_90d != null" class="mt-[7px] flex justify-between font-mono text-[11.5px] text-ink-3">
              <span>90 days ago</span>
              <span v-if="pct(c.uptime_90d)"><b class="font-semibold text-ink-2">{{ pct(c.uptime_90d) }}</b> uptime</span>
              <span>today</span>
            </div>
          </div>
        </div>
        <p v-if="!groups.length" class="rounded-lg border border-border bg-surface px-4 py-6 text-center text-[13px] text-ink-3 shadow-card">No components on this page.</p>

        <!-- incident history (resolved, last 90 days) -->
        <template v-if="page.recent_incidents && page.recent_incidents.length">
          <div class="mb-[10px] mt-[26px] flex items-center gap-[10px]">
            <h2 class="m-0 text-[13px] font-semibold">Past incidents</h2>
            <span class="ml-auto text-[12px] text-ink-3">last 90 days</span>
          </div>
          <div class="overflow-hidden rounded-lg border border-border bg-surface shadow-card">
            <div v-for="inc in page.recent_incidents" :key="inc.id" class="border-b border-border last:border-b-0">
              <!-- collapsed row (click to expand) -->
              <button type="button" class="flex w-full items-center gap-3 px-[18px] py-[13px] text-left hover:bg-surface-2" @click="toggleInc(inc.id)">
                <span class="h-[8px] w-[8px] flex-none rounded-full bg-up"></span>
                <div class="min-w-0">
                  <div class="text-[13.5px] font-medium">{{ inc.title }}</div>
                  <div class="font-mono text-[11.5px] text-ink-3">
                    resolved {{ relTime(inc.resolved_at ?? undefined) }}<template v-if="duration(inc.started_at, inc.resolved_at)"> · lasted {{ duration(inc.started_at, inc.resolved_at) }}</template> · {{ impactBadge(inc.impact).label.toLowerCase() }} impact<template v-if="inc.postmortem"> · <span class="text-accent">postmortem</span></template>
                  </div>
                </div>
                <span class="ml-auto rounded-full px-[9px] py-[2px] text-[11.5px] font-semibold" :class="statusBadge(inc.status).cls">{{ statusBadge(inc.status).label }}</span>
                <svg viewBox="0 0 24 24" class="h-[15px] w-[15px] flex-none text-ink-3 transition-transform" :class="inc.id && expanded.has(inc.id) ? 'rotate-180' : ''" fill="none" stroke="currentColor" stroke-width="2"><path d="M6 9l6 6 6-6" /></svg>
              </button>
              <!-- expanded: postmortem if published, else the update timeline -->
              <div v-if="inc.id && expanded.has(inc.id)" class="border-t border-border bg-surface-2 px-[18px] py-[15px]">
                <template v-if="inc.postmortem && renderSections(inc.postmortem.body).length">
                  <div class="mb-[10px] text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-3">Postmortem</div>
                  <div v-for="sec in renderSections(inc.postmortem.body)" :key="sec.heading" class="mb-3 last:mb-0">
                    <h4 class="mb-1 text-[12px] font-semibold uppercase tracking-[0.05em] text-ink-3">{{ sec.heading }}</h4>
                    <p class="whitespace-pre-wrap text-[13px] leading-relaxed text-ink-2">{{ sec.content }}</p>
                  </div>
                </template>
                <template v-else-if="inc.updates && inc.updates.length">
                  <div class="mb-[10px] text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-3">Timeline</div>
                  <div v-for="u in inc.updates" :key="u.id" class="border-l-2 border-border pb-[11px] pl-3 last:pb-0">
                    <div class="flex items-center gap-2">
                      <span class="rounded-full px-[8px] py-[1px] text-[11px] font-semibold" :class="statusBadge(u.status).cls">{{ statusBadge(u.status).label }}</span>
                      <span class="font-mono text-[11px] text-ink-3">{{ relTime(u.created_at) }}</span>
                    </div>
                    <p v-if="u.body" class="mt-[3px] whitespace-pre-wrap text-[13px] text-ink-2">{{ u.body }}</p>
                  </div>
                </template>
                <p v-else class="text-[13px] text-ink-3">No further details were published.</p>
              </div>
            </div>
          </div>
        </template>

        <!-- subscribe + feeds -->
        <div class="mt-[30px] flex flex-col gap-4 rounded-lg border border-border bg-surface px-5 py-[18px] shadow-card">
          <div class="flex flex-wrap items-center gap-4">
            <div class="min-w-0">
              <b class="text-[14px]">Subscribe to updates</b>
              <span class="block text-[12.5px] text-ink-3">Get an email when an incident is opened, updated, or resolved.</span>
            </div>
            <form class="ml-auto flex gap-2 max-[520px]:w-full" @submit.prevent="subscribe">
              <input v-model="subEmail" type="email" required placeholder="you@company.com" class="h-[38px] w-[220px] rounded-sm border border-border bg-surface-2 px-[11px] text-[13px] outline-none focus:border-accent max-[520px]:w-full" />
              <button type="submit" :disabled="subscribing || !subEmail.trim()" class="h-[38px] shrink-0 rounded-sm bg-accent px-[15px] text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50">
                {{ subscribing ? "…" : "Subscribe" }}
              </button>
            </form>
          </div>
          <p v-if="subMsg" class="text-[12.5px] text-up">{{ subMsg }}</p>
          <p v-if="subErr" class="text-[12.5px] text-down">{{ subErr }}</p>
          <div class="flex gap-4 border-t border-border pt-3 font-mono text-[12.5px] text-ink-3">
            <span>Also by feed:</span>
            <a :href="feed('rss')" class="hover:text-accent">RSS</a>
            <a :href="feed('atom')" class="hover:text-accent">Atom</a>
            <a :href="feed('json')" class="hover:text-accent">JSON</a>
          </div>
        </div>
        <div class="mt-[22px] text-center text-[12px] text-ink-3">
          <p v-if="branding.footerText" class="mb-[6px]">{{ branding.footerText }}</p>
          <a v-if="branding.supportUrl" :href="branding.supportUrl" target="_blank" rel="noopener" class="mb-[6px] inline-block text-ink-2 underline decoration-border-strong underline-offset-2 hover:text-accent">Support</a>
          <div>
            Powered by
            <span class="inline-flex translate-y-[3px] text-accent"><svg viewBox="0 0 24 24" class="h-[14px] w-[14px]" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 3l7 3v5c0 4.5-3 7.5-7 9-4-1.5-7-4.5-7-9V6z" /><path d="M8.5 12l2 2 4.5-4.5" /></svg></span>
            <b class="text-ink-2">cerbix</b>
          </div>
        </div>
      </template>
    </main>
  </div>
</template>
