<script setup lang="ts">
// The incident history of a status page (func-status-pages-incidents.md §13.5, FR-039, D-0270),
// built to the approved mock `docs/design/mock-status-incident-history.html`. The page itself lists
// at most ten past incidents; this view pages through the 90-day history by UTC month, at most 50
// per request, with *Show more* appending in place. Only `month` goes into the URL — never the
// cursor — so a month link can be shared and always opens the month from its start.
import { computed, nextTick, onBeforeUnmount, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import { api } from "@/api/client";
import type { components } from "@/api/schema";
import PastIncidentRow from "@/components/PastIncidentRow.vue";
import PublicStatusHeader from "@/components/PublicStatusHeader.vue";
import { utcDayLabel, utcMonthLabel } from "@/lib/wallclock";

type IncidentHistory = components["schemas"]["IncidentHistory"];
type IncidentDetail = components["schemas"]["IncidentDetail"];

const route = useRoute();
const router = useRouter();
const slug = route.params.slug as string;
const token = (route.query.token as string) || "";
const previewID = (route.query.preview as string) || "";

const loading = ref(true);
const notFound = ref(false);
const refusal = ref("");
const internalPreview = ref(false);
const history = ref<IncidentHistory | null>(null);
const incidents = ref<IncidentDetail[]>([]);
const loadingMore = ref(false);
const moreError = ref("");
const loadError = ref("");
const heading = ref<HTMLElement | null>(null);

// Accordion state keyed by page-local row positions, never written into the URL.
const expanded = ref<Set<string>>(new Set());
function toggle(key: string) {
  const s = new Set(expanded.value);
  if (s.has(key)) s.delete(key);
  else s.add(key);
  expanded.value = s;
}

function requestedMonth(): string {
  return typeof route.query.month === "string" ? route.query.month : "";
}

// ── One guard for every read (review R1-2), as GateDecisionsView.vue does ──────────────────
// The first page of a month and every Show more share ONE generation counter and ONE
// AbortController. A month change or an unmount bumps the generation and aborts what is in flight,
// and an answer that lands after its generation has passed is dropped — rows, errors and spinners
// alike — so a late answer can never mix months or overwrite the month the URL names.
let generation = 0;
let inflight: AbortController | undefined;
function begin(): { signal: AbortSignal; mine: number } {
  inflight?.abort();
  inflight = new AbortController();
  return { signal: inflight.signal, mine: ++generation };
}
function stale(mine: number): boolean {
  return mine !== generation;
}
function isAbort(e: unknown): boolean {
  return (e as { name?: string } | null)?.name === "AbortError";
}
onBeforeUnmount(() => {
  generation++;
  inflight?.abort();
});

// One request, public first; an internal page answers 404 publicly, and a signed-in member
// previewing it reaches the authenticated route through the page id the editor link carries.
// A transport failure REJECTS (openapi-fetch does not turn it into `error`); callers catch it.
// It writes NO state: which route answered comes back as `authed`, and the caller applies it only
// after its generation check (review round 2) — a late authenticated answer's mode is dropped with
// its rows.
async function fetchHistory(month: string, cursor: string, signal: AbortSignal) {
  const query: Record<string, string> = {};
  if (month) query.month = month;
  if (cursor) query.cursor = cursor;
  if (!internalPreview.value) {
    const res = await api.GET("/api/v1/public/status-pages/{slug}/history", {
      params: { path: { slug }, query: token ? { ...query, token } : query },
      signal,
    });
    if (res.data) return { data: res.data, authed: false };
    if (res.response?.status !== 404 || !previewID) return { error: res.error, status: res.response?.status };
  }
  const auth = await api.GET("/api/v1/status-pages/{pageID}/history", {
    params: { path: { pageID: previewID }, query },
    signal,
  });
  if (auth.data) return { data: auth.data, authed: true };
  return { error: auth.error, status: auth.response?.status };
}

function errorText(err: unknown): string {
  return (err as { error?: string } | undefined)?.error ?? "";
}

async function load(focus: boolean) {
  const { signal, mine } = begin();
  loading.value = true;
  loadingMore.value = false;
  notFound.value = false;
  refusal.value = "";
  loadError.value = "";
  moreError.value = "";
  expanded.value = new Set();
  try {
    const res = await fetchHistory(requestedMonth(), "", signal);
    if (stale(mine)) return;
    if (res.data) internalPreview.value = res.authed;
    if (!res.data) {
      if (res.status === 400) {
        refusal.value = errorText(res.error).startsWith("month_outside_history")
          ? "That month is outside the 90-day incident history."
          : "That month is not a valid month.";
      } else if (res.status === 404) {
        notFound.value = true;
      } else {
        loadError.value = "Could not load the incident history.";
      }
      history.value = null;
      incidents.value = [];
      return;
    }
    history.value = res.data;
    incidents.value = [...res.data.incidents];
    if (focus) {
      await nextTick();
      if (!stale(mine)) heading.value?.focus();
    }
  } catch (e) {
    if (stale(mine) || isAbort(e)) return;
    history.value = null;
    incidents.value = [];
    loadError.value = "Could not load the incident history.";
  } finally {
    if (!stale(mine)) loading.value = false;
  }
}

async function showMore() {
  const h = history.value;
  if (!h?.next_cursor || loading.value || loadingMore.value) return;
  const { signal, mine } = begin();
  loadingMore.value = true;
  moreError.value = "";
  try {
    const res = await fetchHistory(h.month, h.next_cursor, signal);
    if (stale(mine)) return;
    if (res.data) internalPreview.value = res.authed;
    if (!res.data) {
      moreError.value = "Could not load more incidents. Try again.";
      return;
    }
    incidents.value = [...incidents.value, ...res.data.incidents];
    history.value = { ...h, next_cursor: res.data.next_cursor };
  } catch (e) {
    if (stale(mine) || isAbort(e)) return;
    moreError.value = "Could not load more incidents. Try again.";
  } finally {
    if (!stale(mine)) loadingMore.value = false;
  }
}

watch(() => route.query.month, () => load(true));
void load(false);


// Months are UTC calendar months (§13.2); the label says so beside the navigator. Every date on
// this page goes through lib/wallclock, the one owner of how cerbix writes time (NFR-025b).
function countLabel(n: number): string {
  return `${n} incident${n === 1 ? "" : "s"}`;
}
const months = computed(() => history.value?.months ?? []);
// A 90-day window touches three to five UTC months; one row of tiles at desktop width.
const monthColumns = computed(() =>
  months.value.length >= 5 ? "grid-cols-5" : months.value.length === 3 ? "grid-cols-3" : "grid-cols-4",
);
const currentIndex = computed(() => months.value.findIndex((m) => m.month === history.value?.month));
const currentCount = computed(() => months.value[currentIndex.value]?.count ?? 0);
const newer = computed(() => (currentIndex.value > 0 ? months.value[currentIndex.value - 1].month : ""));
const older = computed(() =>
  currentIndex.value >= 0 && currentIndex.value < months.value.length - 1 ? months.value[currentIndex.value + 1].month : "",
);
// The oldest offered month starts where the history starts, not on its first day (§13.2).
const partialFrom = computed(() => {
  const h = history.value;
  if (!h) return "";
  const [y, m] = h.month.split("-").map(Number);
  if (Date.parse(h.from) <= Date.UTC(y, m - 1, 1)) return "";
  return utcDayLabel(h.from);
});

function withAccess(q: URLSearchParams): string {
  if (token) q.set("token", token);
  if (previewID) q.set("preview", previewID);
  const qs = q.toString();
  return qs ? `?${qs}` : "";
}
function monthHref(month: string): string {
  return `/status/${encodeURIComponent(slug)}/history${withAccess(new URLSearchParams({ month }))}`;
}
const pageHref = computed(() => `/status/${encodeURIComponent(slug)}${withAccess(new URLSearchParams())}`);
const currentMonthHref = computed(() => `/status/${encodeURIComponent(slug)}/history${withAccess(new URLSearchParams())}`);
function goMonth(month: string) {
  if (!month) return;
  void router.push({ query: { ...route.query, month } });
}
const feedHref = computed(() =>
  internalPreview.value && previewID
    ? `/api/v1/status-pages/${previewID}/feed?format=rss`
    : `/api/v1/public/status-pages/${slug}/feed?format=rss${token ? "&token=" + token : ""}`,
);
</script>

<template>
  <div class="min-h-screen bg-bg text-ink">
    <PublicStatusHeader :title="history?.title || 'Status'" :feed-href="feedHref" />

    <main class="mx-auto max-w-[820px] px-5 pb-16 pt-[26px]">
      <div
        v-if="internalPreview"
        class="mb-4 flex items-center gap-2 rounded border border-degraded/40 bg-degraded-weak px-4 py-2 text-[12.5px] font-medium text-degraded"
      >
        🔒 Internal page — visible to signed-in members only; anonymous visitors get 404.
      </div>

      <a :href="pageHref" class="inline-flex items-center gap-[6px] text-[13px] text-ink-2 hover:text-ink" data-testid="history-back">
        <span aria-hidden="true">←</span> Current status
      </a>
      <div class="mt-3">
        <h1 class="text-[20px] font-semibold tracking-tight">Incident history</h1>
        <p class="mt-[3px] text-[12.5px] text-ink-3">Resolved incidents from the last 90 days.</p>
      </div>

      <div v-if="loading && !history" class="mt-6 text-[13px] text-ink-3">Loading…</div>

      <div
        v-else-if="notFound"
        class="mt-6 rounded-lg border border-border bg-surface p-10 text-center shadow-card"
        data-testid="history-not-found"
      >
        <p class="text-[15px] font-medium">This status page is not available.</p>
        <p class="mt-1 text-[13px] text-ink-3">It may be private, or the link may be incorrect.</p>
      </div>

      <div
        v-else-if="loadError"
        class="mt-6 rounded-lg border border-border bg-surface p-6 text-[13px] shadow-card"
        data-testid="history-load-error"
      >
        <p>{{ loadError }}</p>
        <button
          type="button"
          class="mt-2 font-medium text-accent"
          data-testid="history-retry"
          @click="load(false)"
        >
          Try again
        </button>
      </div>

      <div
        v-else-if="refusal"
        class="mt-6 rounded-lg border border-border bg-surface p-6 text-[13px] shadow-card"
        data-testid="history-refusal"
      >
        <p>{{ refusal }}</p>
        <a :href="currentMonthHref" class="mt-2 inline-block font-medium text-accent">Show the current month</a>
      </div>

      <template v-else-if="history">
        <nav class="mt-[18px] flex items-stretch gap-2" aria-label="Incident history months" data-testid="history-months">
          <button
            type="button"
            class="grid w-[34px] flex-none place-items-center rounded-md border border-border bg-surface text-ink-2 hover:border-border-strong disabled:cursor-default disabled:opacity-40"
            aria-label="Newer month"
            :disabled="!newer"
            data-testid="history-newer"
            @click="goMonth(newer)"
          >
            ←
          </button>
          <ol
            class="m-0 grid min-w-0 flex-1 list-none gap-[6px] p-0 max-[520px]:grid-cols-2"
            :class="monthColumns"
          >
            <li v-for="m in months" :key="m.month" class="min-w-0">
              <a
                :href="monthHref(m.month)"
                class="flex min-w-0 flex-col gap-px rounded-md border px-[10px] py-[7px]"
                :class="
                  m.month === history.month
                    ? 'border-accent bg-accent-weak'
                    : 'border-border bg-surface hover:border-border-strong'
                "
                :aria-current="m.month === history.month ? 'page' : undefined"
                data-testid="history-month"
                :data-month="m.month"
                @click.prevent="goMonth(m.month)"
              >
                <span class="text-[13px] font-medium" :class="m.month === history.month ? 'text-accent' : ''">{{
                  utcMonthLabel(m.month)
                }}</span>
                <span class="whitespace-nowrap font-mono text-[11px] text-ink-3">{{ countLabel(m.count) }}</span>
              </a>
            </li>
          </ol>
          <button
            type="button"
            class="grid w-[34px] flex-none place-items-center rounded-md border border-border bg-surface text-ink-2 hover:border-border-strong disabled:cursor-default disabled:opacity-40"
            aria-label="Older month"
            :disabled="!older"
            data-testid="history-older"
            @click="goMonth(older)"
          >
            →
          </button>
        </nav>
        <p class="mt-2 text-[11.5px] text-ink-3">Months are calendar months in UTC. Times are shown in your time zone.</p>

        <section aria-labelledby="history-month-heading" data-testid="history-month-section">
          <div class="mb-[10px] mt-[26px] flex items-center gap-[10px]">
            <h2 id="history-month-heading" ref="heading" tabindex="-1" class="m-0 text-[13px] font-semibold outline-none">
              {{ utcMonthLabel(history.month) }}
            </h2>
            <span class="ml-auto text-[12px] text-ink-3">{{ countLabel(currentCount) }}</span>
          </div>
          <p
            v-if="partialFrom"
            class="mb-[10px] rounded-md border border-border bg-inset px-3 py-[9px] text-[12.5px] text-ink-2"
            data-testid="history-partial"
          >
            History covers the last 90 days; this month is shown from <b>{{ partialFrom }}</b>.
          </p>
          <div class="overflow-hidden rounded-lg border border-border bg-surface shadow-card">
            <PastIncidentRow
              v-for="(inc, i) in incidents"
              :key="`hist-${i}`"
              :inc="inc"
              :header-id="`history-incident-header-${i + 1}`"
              :panel-id="`history-incident-panel-${i + 1}`"
              :expanded="expanded.has(`hist-${i}`)"
              @toggle="toggle(`hist-${i}`)"
            />
            <p v-if="!incidents.length" class="px-[18px] py-[30px] text-center text-[13px] text-ink-3" data-testid="history-empty">
              No incidents were resolved in {{ utcMonthLabel(history.month) }}.
            </p>
          </div>
          <div v-if="history.next_cursor || moreError" class="mt-3 flex flex-col items-center gap-[6px]">
            <button
              v-if="history.next_cursor"
              type="button"
              class="h-9 rounded-md border border-border bg-surface px-4 text-[13px] font-medium hover:border-border-strong disabled:opacity-50"
              :disabled="loadingMore"
              data-testid="history-show-more"
              @click="showMore"
            >
              {{ loadingMore ? "Loading…" : "Show more" }}
            </button>
            <span v-if="history.next_cursor" class="font-mono text-[11.5px] text-ink-3"
              >showing {{ incidents.length }} of {{ currentCount }}</span
            >
            <p v-if="moreError" class="text-[12.5px] text-down">{{ moreError }}</p>
          </div>
        </section>
      </template>
    </main>
  </div>
</template>
