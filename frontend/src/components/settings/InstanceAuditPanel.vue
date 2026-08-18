<script setup lang="ts">
/**
 * The instance's own audit trail — what a GLOBAL admin's actions leave behind.
 *
 * These entries are stored with no organization (`org_id IS NULL`) and, until iter-0155, appeared in
 * no listing at all: the members panel's trail is org-scoped by construction, so the installation
 * recorded its own history and could not read it. The API keeps the split that this panel depends on —
 * `GET /api/v1/admin/audit` is a distinct read, not the org listing with a wider filter, so no authz
 * slip can widen one into the other.
 *
 * Deliberately absent: org-scoped rows. Mixing them in would make this the "everything" view and
 * answer, in one glance, a question this panel is not allowed to answer — who did what inside a
 * tenant. Also absent: any way to delete a row. An audit trail with a delete button is not one.
 */
import { computed, onMounted, ref } from "vue";

import AuditRows from "@/components/settings/AuditRows.vue";
import { api } from "@/api/client";
import type { components } from "@/api/schema";

type AuditEntry = components["schemas"]["AuditEntry"];

const entries = ref<AuditEntry[]>([]);
const error = ref("");
const loading = ref(true);
// The window widens on demand (30 → 100 → 500, the server's cap) rather than a silent cut.
const limit = ref(30);
const STEPS = [30, 100, 500];

async function load() {
  const r = await api.GET("/api/v1/admin/audit", { params: { query: { limit: limit.value } } });
  if (r.error) {
    // A failed read is stated, never rendered as "nothing happened here".
    error.value = "Could not load the instance audit trail.";
    return;
  }
  error.value = "";
  entries.value = r.data ?? [];
}

onMounted(async () => {
  try {
    await load();
  } finally {
    loading.value = false;
  }
});

const hasMore = computed(() => entries.value.length >= limit.value && limit.value < 500);
async function more() {
  limit.value = STEPS[Math.min(STEPS.indexOf(limit.value) + 1, STEPS.length - 1)];
  await load();
}

// Instance-level vocabulary. Unknown actions render verbatim rather than being hidden.
const labels: Record<string, string> = {
  "user.global_admin": "granted or revoked global admin",
  "user.delete": "deleted a user",
  "provider.reload": "reloaded a file provider",
  "provider.disable": "disabled a file provider",
  "outbox.replay": "replayed a dead outbox event",
  "outbox.replay_all": "replayed every dead outbox event",
  "agenttoken.create": "issued an agent token",
  "agenttoken.revoke": "revoked an agent token",
};
const destructive = ["user.delete", "provider.disable", "agenttoken.revoke"];
</script>

<template>
  <div>
    <div class="mb-3 flex items-center gap-[10px]">
      <h3 class="text-[13px] font-semibold">Instance audit</h3>
      <span class="font-mono text-[12px] text-ink-3">global-admin actions · no organization</span>
      <span
        class="ml-auto rounded-full border border-dashed border-border-strong px-[9px] py-[2px] text-[11.5px] text-ink-2"
        data-testid="instance-chip"
        >instance</span
      >
    </div>
    <p v-if="error" class="mb-3 text-[13px] text-down" data-testid="instance-audit-error">{{ error }}</p>
    <p v-else-if="loading" class="text-[13px] text-ink-3">Loading…</p>
    <AuditRows
      v-else
      :entries="entries"
      :labels="labels"
      :destructive="destructive"
      :has-more="hasMore"
      empty-text="No instance-level actions recorded yet."
      @more="more"
    />
  </div>
</template>
