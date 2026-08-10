<script setup lang="ts">
import { onMounted, ref } from "vue";
import { useRoute, useRouter } from "vue-router";
import BrandMark from "@/components/BrandMark.vue";
import { useTheme } from "@/composables/useTheme";
import { useBranding } from "@/stores/branding";
import { useSession } from "@/stores/session";

const router = useRouter();
const route = useRoute();
const session = useSession();
const { toggle } = useTheme();
const branding = useBranding(); // loaded app-wide in App.vue (public endpoint)

const email = ref("");
const password = ref("");
const totp = ref("");
const totpRequired = ref(false); // password accepted; awaiting a second factor
const error = ref("");
const loading = ref(false);

// Which sign-in methods this instance offers (from the backend, provider-agnostic).
const methods = ref<{ local: boolean; oidc: boolean; oidcButtonLabel: string; passwordReset: boolean }>({
  local: true,
  oidc: false,
  oidcButtonLabel: "Continue with SSO",
  passwordReset: false,
});
const methodsLoaded = ref(false);

onMounted(async () => {
  try {
    const res = await fetch("/auth/config", { credentials: "include" });
    if (res.ok) {
      const c = await res.json();
      methods.value = {
        local: !!c.local,
        oidc: !!c.oidc,
        oidcButtonLabel: c.oidc_button_label || "Continue with SSO",
        passwordReset: !!c.password_reset,
      };
    }
  } catch {
    /* keep the local-login default */
  } finally {
    methodsLoaded.value = true;
  }
});

async function submitLocal() {
  error.value = "";
  loading.value = true;
  try {
    const body: Record<string, string> = { username: email.value, password: password.value };
    if (totpRequired.value) body.totp = totp.value.trim();
    const res = await fetch("/auth/local/login", {
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    if (res.ok) {
      await session.fetchMe();
      const redirect = (route.query.redirect as string) || "/";
      router.push(redirect);
      return;
    }
    // A 401 flagged totp_required means the password is right but 2FA is on.
    const data = await res.json().catch(() => ({}));
    if (res.status === 401 && data?.totp_required) {
      if (totpRequired.value && totp.value.trim()) {
        error.value = "That code didn't match. Try again, or use a recovery code.";
      }
      totpRequired.value = true;
      totp.value = "";
      return;
    }
    // The instance policy demands 2FA but this account never enrolled: a dead
    // end without guidance — say what the ways out are.
    if (res.status === 401 && data?.totp_setup_required) {
      error.value =
        "Two-factor authentication is now required for your account, but you haven't set it up yet. " +
        "Sign in via SSO if it is enabled (then enroll 2FA under Settings → Security), or ask an administrator to temporarily relax the requirement.";
      return;
    }
    error.value = "Invalid email or password.";
  } catch {
    error.value = "Could not reach the server.";
  } finally {
    loading.value = false;
  }
}

function oidcLogin() {
  window.location.href = "/auth/login?redirect=/";
}
</script>

<template>
  <button
    class="fixed top-[18px] right-[18px] grid h-[34px] w-[34px] place-items-center rounded-sm border border-border bg-surface text-ink-2 hover:border-border-strong hover:text-ink"
    type="button"
    aria-label="Toggle theme"
    @click="toggle"
  >
    <svg viewBox="0 0 24 24" class="h-4 w-4" fill="none" stroke="currentColor" stroke-width="2">
      <circle cx="12" cy="12" r="4" />
      <path d="M12 2v2M12 20v2M2 12h2M20 12h2M5 5l1.5 1.5M17.5 17.5L19 19M19 5l-1.5 1.5M6.5 17.5L5 19" />
    </svg>
  </button>

  <main class="grid min-h-screen place-items-center p-6">
    <div class="w-full max-w-[400px] overflow-hidden rounded border border-border bg-surface shadow-card">
      <div class="flex h-1 gap-[2px]">
        <i v-for="i in 64" :key="i" class="flex-1" :class="i === 50 || i === 51 ? 'bg-degraded' : 'bg-up'"></i>
      </div>

      <div class="px-7 pb-6 pt-7">
        <div class="mb-6 flex items-center gap-[10px]">
          <BrandMark :tile="30" :glyph="17" />
          <span class="font-mono text-[17px] font-semibold tracking-tight">{{ branding.productName }}</span>
        </div>

        <h1 class="mb-1 text-lg font-semibold tracking-tight">Sign in</h1>
        <p v-if="route.query.reset" class="mb-5 rounded-sm border border-up/40 bg-up/10 px-3 py-2 text-[13px] text-up">Password updated — sign in with your new password.</p>
        <p v-else class="mb-5 text-[13px] text-ink-3">Keep watch over every service.</p>

        <!-- local username/password (if enabled) -->
        <form v-if="methods.local" class="flex flex-col gap-[13px]" @submit.prevent="submitLocal">
          <div class="flex flex-col gap-[6px]">
            <label class="text-xs font-semibold text-ink-2" for="email">Email</label>
            <input
              id="email" v-model="email" type="email" autocomplete="username" placeholder="you@company.com"
              :disabled="totpRequired"
              class="h-10 rounded-sm border border-border bg-surface-2 px-3 text-sm text-ink outline-none focus:border-transparent focus:bg-surface focus:outline-2 focus:outline-offset-1 focus:outline-accent disabled:opacity-60"
            />
          </div>
          <div class="flex flex-col gap-[6px]">
            <div class="flex items-baseline">
              <label class="text-xs font-semibold text-ink-2" for="pw">Password</label>
              <RouterLink v-if="methods.passwordReset" class="ml-auto text-xs font-medium text-accent hover:underline" :to="{ name: 'forgot-password' }">Forgot?</RouterLink>
              <span v-else class="ml-auto text-xs text-ink-3" title="Self-service reset needs email configured; ask an administrator.">Forgot? Ask an admin</span>
            </div>
            <input
              id="pw" v-model="password" type="password" autocomplete="current-password" placeholder="••••••••••"
              :disabled="totpRequired"
              class="h-10 rounded-sm border border-border bg-surface-2 px-3 text-sm text-ink outline-none focus:border-transparent focus:bg-surface focus:outline-2 focus:outline-offset-1 focus:outline-accent disabled:opacity-60"
            />
          </div>

          <!-- second factor (shown once the password is accepted and 2FA is on) -->
          <div v-if="totpRequired" class="flex flex-col gap-[6px]">
            <label class="text-xs font-semibold text-ink-2" for="totp">Two-factor code</label>
            <input
              id="totp" v-model="totp" inputmode="text" autocomplete="one-time-code" placeholder="123456 or a recovery code" autofocus
              class="h-10 rounded-sm border border-border bg-surface-2 px-3 font-mono text-sm tracking-[0.2em] text-ink outline-none focus:border-transparent focus:bg-surface focus:outline-2 focus:outline-offset-1 focus:outline-accent"
            />
            <p class="text-[12px] text-ink-3">Enter the code from your authenticator app, or one of your recovery codes.</p>
          </div>

          <p v-if="error" class="text-[13px] text-down">{{ error }}</p>

          <button
            type="submit" :disabled="loading"
            class="flex h-10 items-center justify-center gap-2 rounded-sm bg-accent text-sm font-semibold text-accent-ink hover:bg-accent-2 disabled:opacity-60"
          >
            {{ loading ? "Signing in…" : totpRequired ? "Verify" : "Sign in" }}
          </button>
        </form>

        <!-- divider only when both methods are shown -->
        <div v-if="methods.local && methods.oidc" class="my-[18px] flex items-center gap-3 text-xs text-ink-3 before:h-px before:flex-1 before:bg-border after:h-px after:flex-1 after:bg-border">or</div>

        <!-- OIDC (any provider; label from config) -->
        <button
          v-if="methods.oidc"
          type="button"
          :class="[
            'flex h-10 w-full items-center justify-center gap-2 rounded-sm text-sm font-semibold',
            methods.local
              ? 'border border-border bg-surface text-ink hover:border-border-strong hover:bg-surface-2'
              : 'bg-accent text-accent-ink hover:bg-accent-2',
          ]"
          @click="oidcLogin"
        >
          <svg viewBox="0 0 24 24" class="h-4 w-4" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <rect x="5" y="11" width="14" height="10" rx="2" /><path d="M8 11V7a4 4 0 0 1 8 0v4" />
          </svg>
          {{ methods.oidcButtonLabel }}
        </button>

        <p v-if="methodsLoaded && !methods.local && !methods.oidc" class="text-[13px] text-ink-3">
          No sign-in method is configured. Enable <code class="font-mono">local</code> or set an <code class="font-mono">oidc.issuer</code> in the server config.
        </p>

        <div class="mt-[22px] flex items-center gap-2 border-t border-border pt-4 text-xs text-ink-3">
          <span class="inline-flex items-center gap-[6px] text-up">
            <span class="h-[7px] w-[7px] rounded-full bg-up"></span> All systems operational
          </span>
          <span class="ml-auto font-mono">99.99%</span>
        </div>
      </div>
      <div v-if="branding.footerText || branding.supportUrl" class="mt-4 text-center text-[12px] text-ink-3">
        <p v-if="branding.footerText">{{ branding.footerText }}</p>
        <a v-if="branding.supportUrl" :href="branding.supportUrl" target="_blank" rel="noopener" class="text-ink-2 underline decoration-border-strong underline-offset-2 hover:text-accent">Support</a>
      </div>
    </div>
  </main>
</template>
