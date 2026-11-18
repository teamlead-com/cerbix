<script setup lang="ts">
// FR-041a D21, mock §5: `/i/:ref` — the short link in a phone message. The reference is normalised
// here exactly as the server does it (`normalizeIncidentRef`); a malformed one gets the 5d page with
// no request at all. Otherwise `GET /api/v1/incidents/by-ref/{ref}` answers:
//   200 → switch the workspace to the incident's organization and project (the `SearchBox.vue go()`
//         pattern), leave the 5a-2 notice when either changed, and REPLACE the URL with the
//         incident's, so Back does not land on this page again;
//   403 → 5b, the workspace is not switched (the answer names no project, organization or title);
//   404 → 5d — the same words for an unknown reference and for one in an invisible organization.
import { computed, onBeforeUnmount, ref, watch } from "vue";
import { isNavigationFailure, useRoute, useRouter } from "vue-router";
import { api } from "@/api/client";
import AppShell from "@/components/AppShell.vue";
import { failureOf, transportFailure } from "@/lib/gate";
import { normalizeIncidentRef } from "@/lib/phone";
import { useUi } from "@/stores/ui";
import { useWorkspace } from "@/stores/workspace";

type State = "resolving" | "notFound" | "forbidden" | "failed";

const route = useRoute();
const router = useRouter();
const ws = useWorkspace();
const ui = useUi();

const raw = computed(() => {
  const p = route.params.ref;
  const v = Array.isArray(p) ? p[0] : p;
  return typeof v === "string" ? v : "";
});

const state = ref<State>("resolving");
// The reference as the page names it: normalised when it is one, the raw text when it is not (5d).
const shown = ref("");
const failure = ref("");

// A later resolution (another reference in the same view) or leaving the page voids an earlier one:
// a late answer must neither switch the workspace nor navigate.
let ticket = 0;
onBeforeUnmount(() => {
  ticket++;
});

function fail(message: string) {
  failure.value = message;
  state.value = "failed";
}

async function resolve() {
  const mine = ++ticket;
  const live = () => mine === ticket;
  failure.value = "";

  const normalized = normalizeIncidentRef(raw.value);
  if (!normalized) {
    shown.value = raw.value;
    state.value = "notFound";
    return;
  }
  shown.value = normalized;
  state.value = "resolving";

  let res;
  try {
    res = await api.GET("/api/v1/incidents/by-ref/{ref}", { params: { path: { ref: normalized } } });
  } catch (e) {
    if (live()) fail(transportFailure(e));
    return;
  }
  if (!live()) return;
  if (res.error || !res.data) {
    const f = failureOf(res);
    if (f.status === 404) state.value = "notFound";
    else if (f.status === 403) state.value = "forbidden";
    else fail(f.code || "The link could not be resolved.");
    return;
  }
  const target = res.data;

  // The workspace must be read before it is compared: a fresh tab arrives here with none loaded.
  try {
    await ws.init();
  } catch {
    /* selectOrg below says whether the incident's organization can be opened */
  }
  if (!live()) return;

  // The context the person had: the notice says "Switched" only when it differs from the incident's.
  // An empty project is no context (a failed or empty organization), so it alone is not a switch.
  const orgBefore = ws.orgId;
  const projectBefore = ws.projectId;

  // Another organization, or init() failed reading the incident's OWN organization's projects: the
  // store then names that org with no projects and a standing error, and only selectOrg's retry
  // path reads it again (a plain init() keeps throwing that error, so "Try again" would be stuck).
  const needsOrg = () =>
    target.org_id !== ws.orgId || (!!ws.transitionError && ws.transition.failedOrgId === target.org_id);
  if (needsOrg()) {
    // An organization missing from the loaded list (granted after it was read): read the list again
    // once, so the switch publishes its name too.
    if (!ws.orgs.some((o) => o.id === target.org_id)) {
      try {
        await ws.init(true);
      } catch {
        /* selectOrg below decides */
      }
      if (!live()) return;
    }
    if (needsOrg()) {
      const selected = await ws.selectOrg(target.org_id);
      if (!live()) return;
      if (!selected) {
        fail(ws.transitionError || "Could not switch to the incident's organization.");
        return;
      }
    }
  }
  if (target.project_id !== ws.projectId) {
    let selected = ws.selectProject(target.project_id);
    if (!selected && !ws.transitionPending) {
      // The project list predates the person's membership: read it again once.
      try {
        await ws.loadProjects();
      } catch {
        /* the failure below names it */
      }
      if (!live()) return;
      selected = ws.selectProject(target.project_id);
    }
    if (!selected) {
      fail(ws.transitionError || "Could not switch to the incident's project.");
      return;
    }
  }
  const changed = orgBefore !== target.org_id || (projectBefore !== "" && projectBefore !== target.project_id);
  if (changed) ui.setIncidentNotice(`Switched to ${ws.orgName} / ${ws.projectName} to open this incident.`);

  // A navigation that does not happen must not leave the notice for the next incident page opened.
  let opened = false;
  try {
    opened = !isNavigationFailure(await router.replace({ name: "incident", params: { id: target.incident_id } }));
  } catch {
    if (live()) fail("Could not open the incident page.");
  } finally {
    if (changed && !opened) ui.takeIncidentNotice();
  }
}

watch(raw, () => void resolve(), { immediate: true });

const CARD = "mx-auto flex w-full max-w-[520px] gap-[10px] rounded border border-border bg-surface p-[22px] shadow-card";
const BTN_SEC =
  "inline-flex h-[32px] items-center justify-center rounded-sm border border-border px-[13px] text-[13px] font-medium text-ink-2 hover:border-border-strong hover:text-ink";
</script>

<template>
  <AppShell
    active="incidents"
    :crumbs="[{ label: ws.orgName || 'cerbix' }, { label: ws.projectName || '…', to: { name: 'dashboard' } }, { label: 'Incidents', to: { name: 'incidents' } }, { label: shown || '…' }]"
  >
    <div class="mx-auto max-w-[900px] px-[22px] pb-16 pt-10">
      <!-- 5a-1 -->
      <div v-if="state === 'resolving'" :class="CARD" class="items-center" role="status" data-testid="incident-ref-resolving">
        <span class="h-[14px] w-[14px] flex-none animate-spin rounded-full border-2 border-border-strong border-t-accent motion-reduce:animate-none" aria-hidden="true"></span>
        <p class="text-[13px] leading-[1.55] text-ink-2">Opening incident <span class="font-mono">{{ shown }}</span>…</p>
      </div>

      <!-- 5b -->
      <div v-else-if="state === 'forbidden'" :class="CARD" class="flex-col" data-testid="incident-ref-forbidden">
        <h1 class="text-[16px] font-semibold">You can't open this incident</h1>
        <p class="text-[13px] leading-[1.55] text-ink-2">Incident <span class="font-mono">{{ shown }}</span> is in a project you are not a member of. Ask an admin of that project to add you.</p>
        <div class="flex gap-2"><RouterLink :to="{ name: 'incidents' }" :class="BTN_SEC">Go to incidents</RouterLink></div>
      </div>

      <!-- 5d -->
      <div v-else-if="state === 'notFound'" :class="CARD" class="flex-col" data-testid="incident-ref-not-found">
        <h1 class="text-[16px] font-semibold">No incident found</h1>
        <p class="text-[13px] leading-[1.55] text-ink-2">Nothing matches <span class="break-all font-mono">{{ shown }}</span>. Check the link in the message — a reference is 8 letters and digits.</p>
        <div class="flex gap-2"><RouterLink :to="{ name: 'incidents' }" :class="BTN_SEC">Go to incidents</RouterLink></div>
      </div>

      <!-- Not drawn: the answer could not be read, or the workspace could not be switched. -->
      <div v-else :class="CARD" class="flex-col" role="alert" data-testid="incident-ref-failed">
        <h1 class="text-[16px] font-semibold">Could not open incident <span class="font-mono">{{ shown }}</span></h1>
        <p class="text-[13px] leading-[1.55] text-ink-2">{{ failure }}</p>
        <div class="flex gap-2">
          <button type="button" :class="BTN_SEC" @click="resolve">Try again</button>
          <RouterLink :to="{ name: 'incidents' }" :class="BTN_SEC">Go to incidents</RouterLink>
        </div>
      </div>
    </div>
  </AppShell>
</template>
