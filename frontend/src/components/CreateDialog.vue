<script setup lang="ts">
import { computed, reactive, ref, watch } from "vue";
import { useRouter } from "vue-router";
import { useUi } from "@/stores/ui";
import { useWorkspace } from "@/stores/workspace";

const ui = useUi();
const ws = useWorkspace();
const router = useRouter();

const isOrg = computed(() => ui.createKind === "org");
const form = reactive({ name: "", slug: "" });
const slugTouched = ref(false);
const busy = ref(false);
const error = ref("");

const slugify = (v: string) =>
  v.toLowerCase().trim().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "");

// Reset the form each time the dialog opens.
watch(
  () => ui.createKind,
  (k) => {
    if (k) {
      form.name = "";
      form.slug = "";
      slugTouched.value = false;
      error.value = "";
    }
  },
);
// Auto-derive the slug from the name until the user edits it directly.
watch(
  () => form.name,
  (n) => {
    if (!slugTouched.value) form.slug = slugify(n);
  },
);
function onSlug(e: Event) {
  slugTouched.value = true;
  form.slug = (e.target as HTMLInputElement).value.toLowerCase().replace(/[^a-z0-9-]/g, "");
}

const canCreate = computed(() => !!form.name.trim() && !!form.slug.trim() && !busy.value);

async function submit() {
  if (!canCreate.value) return;
  busy.value = true;
  error.value = "";
  const err = isOrg.value
    ? await ws.createOrg(form.name.trim(), form.slug.trim())
    : await ws.createProject(form.name.trim(), form.slug.trim());
  busy.value = false;
  if (err) {
    error.value = err;
    return;
  }
  ui.closeCreate();
  router.push({ name: "dashboard" });
}
</script>

<template>
  <div
    v-if="ui.createKind"
    class="fixed inset-0 z-50 grid place-items-center bg-[rgba(10,10,20,0.42)] p-5"
    @click.self="ui.closeCreate()"
    @keydown.esc="ui.closeCreate()"
  >
    <div class="w-full max-w-[460px] rounded border border-border-strong bg-surface shadow-lg" role="dialog" aria-modal="true">
      <div class="px-5 pb-1 pt-[18px]">
        <h3 class="text-[16px] font-semibold tracking-tight">{{ isOrg ? "New organization" : "New project" }}</h3>
        <p class="mt-1 text-[12.5px] text-ink-3">
          {{ isOrg
            ? "The top-level tenant. You become its first admin."
            : `A team or app inside ${ws.orgName || "the organization"} — holds its own monitors, channels and members.` }}
        </p>
      </div>

      <div class="flex flex-col gap-[14px] px-5 pb-1 pt-[14px]">
        <div v-if="!isOrg" class="flex flex-col gap-[6px]">
          <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Organization</span>
          <input :value="ws.orgName" disabled class="rounded-sm border border-border bg-surface-2 px-3 py-[9px] text-[13.5px] text-ink-3" />
        </div>
        <label class="flex flex-col gap-[6px]">
          <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Name</span>
          <input
            v-model="form.name"
            :placeholder="isOrg ? 'Acme' : 'API'"
            autocomplete="off"
            class="rounded-sm border border-border bg-surface-2 px-3 py-[9px] text-[13.5px] outline-none focus:border-accent"
            @keydown.enter="submit"
          />
        </label>
        <label class="flex flex-col gap-[6px]">
          <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">
            Slug <span class="font-normal normal-case tracking-normal text-ink-3">— used in URLs, lowercase</span>
          </span>
          <input
            :value="form.slug"
            :placeholder="isOrg ? 'acme' : 'api'"
            autocomplete="off"
            class="rounded-sm border border-border bg-surface-2 px-3 py-[9px] font-mono text-[12.5px] outline-none focus:border-accent"
            @input="onSlug"
            @keydown.enter="submit"
          />
        </label>
        <div v-if="error" class="text-[12.5px] text-down">{{ error }}</div>
      </div>

      <div class="flex items-center gap-2 px-5 pb-[18px] pt-4">
        <span class="flex-1"></span>
        <button type="button" class="h-9 rounded-sm border border-border px-4 text-[13px] text-ink-2 hover:border-border-strong" @click="ui.closeCreate()">Cancel</button>
        <button
          type="button"
          :disabled="!canCreate"
          class="h-9 rounded-sm bg-accent px-4 text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50"
          @click="submit"
        >
          {{ busy ? "Creating…" : isOrg ? "Create organization" : "Create project" }}
        </button>
      </div>
    </div>
  </div>
</template>
