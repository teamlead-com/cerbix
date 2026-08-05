<script setup lang="ts">
import { computed, ref } from "vue";
import { RouterLink, useRoute, useRouter } from "vue-router";
import { useTheme } from "@/composables/useTheme";

const { toggle } = useTheme();
const route = useRoute();
const router = useRouter();
const token = computed(() => (route.query.token as string) || "");

const next = ref("");
const confirm = ref("");
const loading = ref(false);
const error = ref("");

async function submit() {
  error.value = "";
  if (next.value.length < 8) {
    error.value = "New password must be at least 8 characters.";
    return;
  }
  if (next.value !== confirm.value) {
    error.value = "Passwords don't match.";
    return;
  }
  loading.value = true;
  try {
    const res = await fetch("/auth/local/reset/confirm", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ token: token.value, new_password: next.value }),
    });
    if (res.status === 204) {
      router.push({ name: "login", query: { reset: "1" } });
      return;
    }
    const data = await res.json().catch(() => ({}));
    error.value = data?.error || "This reset link is invalid or has expired.";
  } catch {
    error.value = "Could not reach the server. Try again.";
  } finally {
    loading.value = false;
  }
}
</script>

<template>
  <button
    class="fixed top-[18px] right-[18px] grid h-[34px] w-[34px] place-items-center rounded-sm border border-border bg-surface text-ink-2 hover:border-border-strong hover:text-ink"
    type="button" aria-label="Toggle theme" @click="toggle"
  >
    <svg viewBox="0 0 24 24" class="h-4 w-4" fill="none" stroke="currentColor" stroke-width="2">
      <circle cx="12" cy="12" r="4" />
      <path d="M12 2v2M12 20v2M2 12h2M20 12h2M5 5l1.5 1.5M17.5 17.5L19 19M19 5l-1.5 1.5M6.5 17.5L5 19" />
    </svg>
  </button>

  <main class="grid min-h-screen place-items-center p-6">
    <div class="w-full max-w-[400px] overflow-hidden rounded border border-border bg-surface shadow-card">
      <div class="flex h-1 gap-[2px]"><i v-for="i in 64" :key="i" class="flex-1 bg-up"></i></div>
      <div class="px-7 pb-6 pt-7">
        <div class="mb-6 flex items-center gap-[10px]">
          <span class="grid h-[30px] w-[30px] place-items-center rounded-sm bg-accent text-accent-ink">
            <svg viewBox="0 0 24 24" class="h-[17px] w-[17px]" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round">
              <path d="M12 3l7 3v5c0 4.5-3 7.5-7 9-4-1.5-7-4.5-7-9V6z" /><path d="M8.5 12l2 2 4.5-4.5" />
            </svg>
          </span>
          <span class="font-mono text-[17px] font-semibold tracking-tight">cerbix</span>
        </div>

        <h1 class="mb-1 text-lg font-semibold tracking-tight">Choose a new password</h1>

        <template v-if="!token">
          <p class="mb-5 text-[13px] text-down">This reset link is missing its token. Request a new one.</p>
          <RouterLink :to="{ name: 'forgot-password' }" class="text-xs font-medium text-accent hover:underline">Request a reset link</RouterLink>
        </template>
        <form v-else class="flex flex-col gap-[13px]" @submit.prevent="submit">
          <p class="mb-1 text-[13px] text-ink-3">At least 8 characters.</p>
          <div class="flex flex-col gap-[6px]">
            <label class="text-xs font-semibold text-ink-2" for="np">New password</label>
            <input id="np" v-model="next" type="password" autocomplete="new-password" required
              class="h-10 rounded-sm border border-border bg-surface-2 px-3 text-sm text-ink outline-none focus:border-transparent focus:bg-surface focus:outline-2 focus:outline-offset-1 focus:outline-accent" />
          </div>
          <div class="flex flex-col gap-[6px]">
            <label class="text-xs font-semibold text-ink-2" for="cp">Confirm password</label>
            <input id="cp" v-model="confirm" type="password" autocomplete="new-password" required
              class="h-10 rounded-sm border border-border bg-surface-2 px-3 text-sm text-ink outline-none focus:border-transparent focus:bg-surface focus:outline-2 focus:outline-offset-1 focus:outline-accent" />
          </div>
          <p v-if="error" class="text-[13px] text-down">{{ error }}</p>
          <button type="submit" :disabled="loading" class="flex h-10 items-center justify-center rounded-sm bg-accent text-sm font-semibold text-accent-ink hover:bg-accent-2 disabled:opacity-60">
            {{ loading ? "Saving…" : "Set new password" }}
          </button>
        </form>

        <div class="mt-[22px] border-t border-border pt-4 text-xs">
          <RouterLink :to="{ name: 'login' }" class="font-medium text-accent hover:underline">← Back to sign in</RouterLink>
        </div>
      </div>
    </div>
  </main>
</template>
