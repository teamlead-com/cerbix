<script setup lang="ts">
import { onMounted, reactive, ref } from "vue";
import { api } from "@/api/client";
import type { components } from "@/api/schema";

type AgentToken = components["schemas"]["AgentToken"];

const loading = ref(true);
const tokens = ref<AgentToken[]>([]);
const error = ref("");
const actionError = ref("");
// The plaintext secret exists only in the create response — shown once.
const revealed = ref<{ name: string; token: string } | null>(null);
const copied = ref(false);

const form = reactive({ name: "", region: "" });
const creating = ref(false);

async function load() {
  loading.value = true;
  error.value = "";
  try {
    const res = await api.GET("/api/v1/agent-tokens", {});
    if (res.error) {
      error.value = "You need global-admin rights to manage agent tokens.";
      tokens.value = [];
      return;
    }
    tokens.value = res.data ?? [];
  } catch {
    error.value = "Could not load agent tokens.";
  } finally {
    loading.value = false;
  }
}

async function issue() {
  if (!form.name.trim() || !form.region.trim() || creating.value) return;
  creating.value = true;
  actionError.value = "";
  try {
    const res = await api.POST("/api/v1/agent-tokens", {
      body: { name: form.name.trim(), region: form.region.trim() },
    });
    if (res.error || !res.data) {
      actionError.value = (res.error as { error?: string })?.error || "Could not issue the token.";
      return;
    }
    revealed.value = { name: res.data.name ?? "", token: res.data.token ?? "" };
    form.name = "";
    form.region = "";
    await load();
  } finally {
    creating.value = false;
  }
}

async function revoke(t: AgentToken) {
  actionError.value = "";
  const res = await api.DELETE("/api/v1/agent-tokens/{tokenID}", { params: { path: { tokenID: t.id! } } });
  if (res.error) {
    actionError.value = (res.error as { error?: string })?.error || "Could not revoke the token.";
    return;
  }
  await load(); // revoked rows stay listed, dimmed
}

async function copy() {
  if (!revealed.value) return;
  try {
    await navigator.clipboard.writeText(revealed.value.token);
    copied.value = true;
    setTimeout(() => (copied.value = false), 1500);
  } catch {
    /* clipboard blocked; the value is shown for manual copy */
  }
}
const fmtDate = (ts?: string) => (ts ? new Date(ts).toISOString().slice(0, 10) : "—");

onMounted(load);
</script>

<template>
  <div>
    <div class="mb-4">
      <h2 class="text-[15px] font-semibold tracking-tight">Agent tokens</h2>
      <p class="mt-[3px] text-[13px] text-ink-3">Bearer tokens for HTTP-pull agents (<code class="font-mono text-[12px]">--role agent</code>) in broker-less regions. Each token authorizes exactly one region.</p>
    </div>

    <div v-if="revealed" class="mb-4 rounded border border-accent/40 bg-accent-weak p-4">
      <div class="text-[12px] font-semibold text-accent">Agent token "{{ revealed.name }}" — shown once, copy it now</div>
      <div class="mt-2 flex items-center gap-2">
        <code class="min-w-0 flex-1 truncate rounded-sm border border-border bg-surface px-3 py-2 font-mono text-[12.5px]">{{ revealed.token }}</code>
        <button type="button" class="h-[34px] rounded-sm border border-border px-3 text-[13px] text-ink-2 hover:border-border-strong" @click="copy">{{ copied ? "Copied ✓" : "Copy" }}</button>
        <button type="button" class="h-[34px] rounded-sm border border-border px-3 text-[13px] text-ink-2 hover:border-border-strong" @click="revealed = null">Dismiss</button>
      </div>
      <p class="mt-2 text-[12px] text-ink-3">Configure it as the agent's <code class="font-mono">pull.token</code>.</p>
    </div>

    <div v-if="error" class="mb-4 rounded border border-border bg-surface p-4 text-[13px] text-ink-3 shadow-card">{{ error }}</div>
    <div v-if="actionError" class="mb-4 rounded-sm border border-down/40 bg-down-weak px-4 py-2 text-[13px] text-down">{{ actionError }}</div>

    <section v-if="!error" class="overflow-hidden rounded border border-border bg-surface shadow-card">
      <table class="w-full text-[13px]">
        <thead>
          <tr class="text-[10.5px] uppercase tracking-[0.06em] text-ink-3">
            <th class="border-b border-border px-4 py-[10px] text-left">Name</th>
            <th class="border-b border-border px-4 py-[10px] text-left">Region</th>
            <th class="border-b border-border px-4 py-[10px] text-left">Created</th>
            <th class="border-b border-border px-4 py-[10px] text-left">Status</th>
            <th class="border-b border-border px-4 py-[10px]"></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="t in tokens" :key="t.id" class="hover:bg-surface-2" :class="t.revoked_at ? 'text-ink-3' : ''">
            <td class="border-b border-border px-4 py-[11px] font-mono text-[12.5px]">{{ t.name }}</td>
            <td class="border-b border-border px-4 py-[11px] font-mono text-[12.5px]">{{ t.region }}</td>
            <td class="border-b border-border px-4 py-[11px] font-mono text-[12px]">{{ fmtDate(t.created_at) }}</td>
            <td class="border-b border-border px-4 py-[11px]">
              <span v-if="t.revoked_at" class="rounded-full bg-down-weak px-[9px] py-[2px] text-[11px] font-semibold text-down">revoked {{ fmtDate(t.revoked_at) }}</span>
              <span v-else class="text-ink-2">active</span>
            </td>
            <td class="border-b border-border px-4 py-[11px] text-right">
              <button v-if="!t.revoked_at" type="button" class="text-[12.5px] text-down hover:underline" @click="revoke(t)">Revoke</button>
            </td>
          </tr>
          <tr v-if="!tokens.length && !loading"><td colspan="5" class="px-4 py-10 text-center text-[13px] text-ink-3">No agent tokens yet.</td></tr>
        </tbody>
      </table>
      <div class="flex flex-wrap items-end gap-3 border-t border-border p-4">
        <label class="flex flex-col gap-[6px]">
          <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Name</span>
          <input v-model="form.name" type="text" placeholder="geo2-dc" class="w-[170px] rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent" />
        </label>
        <label class="flex flex-col gap-[6px]">
          <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Region</span>
          <input v-model="form.region" type="text" placeholder="geo2" class="w-[130px] rounded-sm border border-border bg-surface-2 px-3 py-2 font-mono text-[13px] outline-none focus:border-accent" />
        </label>
        <button type="button" :disabled="creating || !form.name.trim() || !form.region.trim()" class="h-[38px] rounded-sm bg-accent px-[15px] text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50" @click="issue">{{ creating ? "Issuing…" : "Issue token" }}</button>
      </div>
    </section>
  </div>
</template>
