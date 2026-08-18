<script setup lang="ts">
/**
 * The audit row, once. Two panels render audit history — an organization's members/tokens trail and
 * the instance's own — and the mock for the second says its row grammar is the first's, unchanged.
 * A claim like that survives by having one component, not by two implementations agreeing today.
 *
 * Presentational only: the caller owns the fetch, the window and the action vocabulary. What lives
 * here is the shape a reader learns once — a severity dot, the actor, the action in prose, a
 * monospace target, relative time — plus the two states that are easy to get wrong: an actor that
 * cannot be resolved reads as itself, never as a borrowed human name, and an empty trail says so
 * instead of rendering nothing.
 */
import { relTime } from "@/lib/incident";
import type { components } from "@/api/schema";

type AuditEntry = components["schemas"]["AuditEntry"];

const props = defineProps<{
  entries: AuditEntry[];
  /** action → prose, e.g. `{"member.add": "added a member"}`. Unknown actions render verbatim. */
  labels: Record<string, string>;
  /** actions rendered with the destructive dot — removals and deletions. */
  destructive?: string[];
  hasMore?: boolean;
  emptyText?: string;
}>();
defineEmits<{ more: [] }>();

const actor = (e: AuditEntry) => (e.via_token ? "a service token" : e.actor_name || e.actor_email || "machine");
const isDestructive = (e: AuditEntry) => (props.destructive ?? []).some((a) => (e.action ?? "").startsWith(a));
</script>

<template>
  <section class="rounded border border-border bg-surface shadow-card">
    <ul>
      <li
        v-for="e in entries"
        :key="e.id"
        class="flex items-center gap-3 border-b border-border px-4 py-[11px] last:border-b-0"
        data-testid="audit-row"
      >
        <span class="h-[7px] w-[7px] flex-none rounded-full" :class="isDestructive(e) ? 'bg-down' : 'bg-accent'"></span>
        <span class="min-w-0 text-[13px]">
          <b class="font-medium">{{ actor(e) }}</b> {{ labels[e.action ?? ""] || e.action
          }}<span v-if="e.target" class="text-ink-3">
            · <span class="font-mono text-[12px]">{{ e.target }}</span></span
          >
        </span>
        <span class="ml-auto flex-none font-mono text-[11.5px] text-ink-3">{{ relTime(e.created_at) }}</span>
      </li>
      <li v-if="!entries.length" class="px-4 py-6 text-center text-[13px] text-ink-3" data-testid="audit-empty">
        {{ emptyText || "No recorded changes yet." }}
      </li>
      <li v-if="hasMore" class="flex justify-center px-4 py-[10px]">
        <button
          type="button"
          class="h-[32px] rounded-sm border border-border-strong px-[14px] text-[12.5px] text-ink-2 hover:border-accent hover:text-accent"
          data-testid="audit-more"
          @click="$emit('more')"
        >
          Show more ({{ entries.length }} shown)
        </button>
      </li>
    </ul>
  </section>
</template>
