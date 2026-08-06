<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { api } from "@/api/client";
import type { components } from "@/api/schema";
import { useSession } from "@/stores/session";
import { useWorkspace } from "@/stores/workspace";
import { relTime } from "@/lib/incident";

type AdminUser = components["schemas"]["AdminUser"];
type Role = components["schemas"]["Role"];

const ws = useWorkspace();
const session = useSession();

const loading = ref(true);
const users = ref<AdminUser[]>([]);
const error = ref("");
const actionError = ref("");
const query = ref("");
const confirmDeleteId = ref("");
const busyId = ref("");

const avatarPalette = ["#5854f2", "#e0399f", "#0d9aa8", "#b97800", "#7c5cff", "#12a05c"];
function avatarColor(id?: string): string {
  if (!id) return avatarPalette[0];
  let h = 0;
  for (const ch of id) h = (h + ch.charCodeAt(0)) % avatarPalette.length;
  return avatarPalette[h];
}
function initials(u: AdminUser): string {
  const n = (u.display_name || u.email || "").trim();
  if (!n) return "?";
  const parts = n.split(/\s+/).filter(Boolean);
  if (parts.length >= 2) return (parts[0][0] + parts[1][0]).toUpperCase();
  return n.slice(0, 2).toUpperCase();
}
const userName = (u: AdminUser) => u.display_name || u.email || u.id || "unknown";
const isSelf = (u: AdminUser) => u.id === session.user?.id;
const lastActive = (ts?: string | null) => (ts ? relTime(ts) : "never");
// local | oidc → one pill each; both → two pills.
function authPills(u: AdminUser): string[] {
  if (u.auth_type === "both") return ["local", "OIDC"];
  if (u.auth_type === "oidc") return ["OIDC"];
  if (u.auth_type === "local") return ["local"];
  return [];
}
function membershipLabel(m: NonNullable<AdminUser["memberships"]>[number]): string {
  const base = `${m.org_name || m.org_id} · ${m.role}`;
  return m.project_name ? `${base} (${m.project_name})` : base;
}

const filtered = computed(() => {
  const q = query.value.trim().toLowerCase();
  if (!q) return users.value;
  return users.value.filter(
    (u) => userName(u).toLowerCase().includes(q) || (u.email ?? "").toLowerCase().includes(q),
  );
});

async function load() {
  loading.value = true;
  error.value = "";
  try {
    await ws.init(); // org list for the "Add to org" picker (global admin sees all orgs)
    const res = await api.GET("/api/v1/admin/users", {});
    if (res.error) {
      error.value = "You need global-admin rights to view users.";
      users.value = [];
      return;
    }
    users.value = res.data ?? [];
  } catch {
    error.value = "Could not load users.";
  } finally {
    loading.value = false;
  }
}

async function toggleAdmin(u: AdminUser) {
  actionError.value = "";
  busyId.value = u.id ?? "";
  try {
    const res = await api.PATCH("/api/v1/admin/users/{userID}", {
      params: { path: { userID: u.id! } },
      body: { is_global_admin: !u.is_global_admin },
    });
    if (res.error || !res.data) {
      actionError.value = (res.error as { error?: string })?.error || "Could not change the global-admin flag.";
      return;
    }
    const idx = users.value.findIndex((x) => x.id === u.id);
    if (idx >= 0) users.value[idx].is_global_admin = res.data.is_global_admin;
  } finally {
    busyId.value = "";
  }
}

async function removeUser(u: AdminUser) {
  actionError.value = "";
  busyId.value = u.id ?? "";
  try {
    const res = await api.DELETE("/api/v1/admin/users/{userID}", {
      params: { path: { userID: u.id! } },
    });
    if (res.error) {
      actionError.value = (res.error as { error?: string })?.error || "Could not delete the user.";
    } else {
      users.value = users.value.filter((x) => x.id !== u.id);
    }
  } finally {
    busyId.value = "";
    confirmDeleteId.value = "";
  }
}

// "Add to org" grants org-scoped memberships, so the role choices are the
// org-scope set (project_admin is granted from Members with a project scope).
// The form is an inline expansion row under the user (a floating popover would
// be clipped by the table's overflow container): chips pick the organizations,
// the role is set per organization in the picked list — no ordering, no
// "default role" mode. Submit is one membership call per org, each with its
// own role, reporting per-org failures.
const orgRoles: Role[] = ["viewer", "editor", "org_admin"];
const addTarget = ref<AdminUser | null>(null);
const picks = ref<{ org_id: string; role: Role }[]>([]);
const bulkRole = ref(""); // "Set all to" — an explicit bulk action, resets to the placeholder
const adding = ref(false);
function openAdd(u: AdminUser) {
  if (addTarget.value?.id === u.id) {
    addTarget.value = null;
    return;
  }
  addTarget.value = u;
  picks.value = [];
  bulkRole.value = "";
  actionError.value = "";
}
// Orgs where the user already holds an org-level grant — offered as disabled chips.
const memberOrgIds = computed(() => {
  const u = addTarget.value;
  return new Set((u?.memberships ?? []).filter((m) => !m.project_id).map((m) => m.org_id));
});
const isMemberOrg = (id?: string) => !!id && memberOrgIds.value.has(id);
const isPickedOrg = (id?: string) => !!id && picks.value.some((p) => p.org_id === id);
function toggleOrg(id?: string) {
  if (!id) return;
  const i = picks.value.findIndex((p) => p.org_id === id);
  if (i >= 0) picks.value.splice(i, 1);
  else picks.value.push({ org_id: id, role: "editor" });
}
function setAllRoles() {
  if (!bulkRole.value) return;
  for (const p of picks.value) p.role = bulkRole.value as Role;
  bulkRole.value = "";
}
function orgLabel(id: string): string {
  const o = ws.orgs.find((x) => x.id === id);
  return o?.name || o?.slug || id;
}
async function submitAdd() {
  const u = addTarget.value;
  if (!u || !picks.value.length || adding.value) return;
  adding.value = true;
  actionError.value = "";
  const failedPicks: { org_id: string; role: Role }[] = [];
  try {
    for (const p of picks.value) {
      const res = await api.POST("/api/v1/organizations/{orgID}/members", {
        params: { path: { orgID: p.org_id } },
        body: { user_id: u.id, role: p.role },
      });
      if (res.error || !res.data) failedPicks.push(p);
    }
    await load(); // refresh membership chips (partial success included)
    if (failedPicks.length) {
      actionError.value = `Could not add the user to: ${failedPicks.map((p) => orgLabel(p.org_id)).join(", ")}.`;
      // Keep the form open with only the failed picks; re-point at the fresh row.
      picks.value = failedPicks;
      addTarget.value = users.value.find((x) => x.id === u.id) ?? null;
    } else {
      addTarget.value = null;
    }
  } finally {
    adding.value = false;
  }
}

onMounted(load);
</script>

<template>
  <div>
    <!-- panel head -->
    <div class="mb-4 flex flex-wrap items-start gap-[14px]">
      <div>
        <h2 class="text-[15px] font-semibold tracking-tight">Users</h2>
        <p class="mt-[3px] text-[13px] text-ink-3">Every user of this instance, including users outside any organization.</p>
      </div>
      <div class="relative ml-auto">
        <svg viewBox="0 0 24 24" class="pointer-events-none absolute left-[9px] top-1/2 h-[15px] w-[15px] -translate-y-1/2 text-ink-3" fill="none" stroke="currentColor" stroke-width="2"><circle cx="11" cy="11" r="7" /><path d="M21 21l-4-4" /></svg>
        <input v-model="query" type="text" placeholder="Search by email or name" class="h-[32px] w-[220px] rounded-sm border border-border bg-surface pl-[30px] pr-[10px] text-[13px] outline-none focus:border-accent max-[640px]:w-[140px]" />
      </div>
    </div>

    <div v-if="error" class="mb-4 rounded border border-border bg-surface p-4 text-[13px] text-ink-3 shadow-card">{{ error }}</div>
    <div v-if="actionError" class="mb-4 rounded-sm border border-down/40 bg-down-weak px-4 py-2 text-[13px] text-down">{{ actionError }}</div>


    <section v-if="!error" class="rounded border border-border bg-surface shadow-card">
      <div class="flex items-center gap-[10px] px-4 pb-3 pt-[14px]">
        <h3 class="text-[13px] font-semibold">Users</h3>
        <span class="font-mono text-[12px] text-ink-3">{{ users.length }}</span>
      </div>
      <div class="overflow-x-auto">
        <table class="w-full text-[13px]">
          <thead>
            <tr class="text-[10.5px] uppercase tracking-[0.06em] text-ink-3">
              <th class="border-b border-border px-4 py-[10px] text-left">User</th>
              <th class="border-b border-border px-4 py-[10px] text-left">Sign-in</th>
              <th class="border-b border-border px-4 py-[10px] text-left">Organizations</th>
              <th class="border-b border-border px-4 py-[10px] text-left">Last active</th>
              <th class="border-b border-border px-4 py-[10px] text-right">Actions</th>
            </tr>
          </thead>
          <tbody>
            <template v-for="u in filtered" :key="u.id">
            <tr class="hover:bg-surface-2">
              <td class="border-b border-border px-4 py-[11px]">
                <div class="flex items-center gap-[11px]">
                  <span class="grid h-8 w-8 flex-none place-items-center rounded-full text-[11.5px] font-semibold text-white" :style="{ background: avatarColor(u.id) }">{{ initials(u) }}</span>
                  <span class="flex min-w-0 flex-col leading-tight">
                    <b class="flex flex-wrap items-center gap-[6px] text-[13px]">
                      {{ userName(u) }}
                      <span v-if="u.is_global_admin" class="rounded-full bg-degraded-weak px-[7px] py-px text-[10px] font-bold uppercase tracking-[0.04em] text-degraded">★ Global admin</span>
                      <span v-if="isSelf(u)" class="rounded-full bg-accent-weak px-[6px] py-px text-[10px] font-bold uppercase tracking-[0.04em] text-accent">you</span>
                    </b>
                    <span class="font-mono text-[11px] text-ink-3">{{ u.email }}</span>
                  </span>
                </div>
              </td>
              <td class="border-b border-border px-4 py-[11px]">
                <span v-for="p in authPills(u)" :key="p" class="mr-1 inline-block rounded-full px-[9px] py-[2px] text-[11.5px] font-medium" :class="p === 'OIDC' ? 'bg-accent-weak text-accent' : 'bg-maint-weak text-maint'">{{ p }}</span>
              </td>
              <td class="border-b border-border px-4 py-[11px]">
                <span v-if="!u.memberships?.length" class="inline-flex items-center gap-[5px] rounded-full bg-down-weak px-[9px] py-[2px] text-[11.5px] font-medium text-down">⚠ no organization</span>
                <template v-else>
                  <span v-for="m in u.memberships" :key="m.org_id + (m.project_id ?? '')" class="mb-[2px] mr-1 inline-block rounded-full border border-border bg-surface-2 px-[9px] py-[2px] font-mono text-[11px] text-ink-2">{{ membershipLabel(m) }}</span>
                </template>
              </td>
              <td class="border-b border-border px-4 py-[11px] font-mono text-[12.5px] text-ink-3">{{ lastActive(u.last_active_at) }}</td>
              <td class="border-b border-border px-4 py-[11px] text-right">
                <template v-if="confirmDeleteId === u.id">
                  <span class="mr-2 text-[12px] text-ink-3">Delete user?</span>
                  <button type="button" class="mr-1 h-[26px] rounded-sm bg-down px-[9px] text-[12px] font-medium text-white hover:opacity-90 disabled:opacity-50" :disabled="busyId === u.id" @click="removeUser(u)">Confirm</button>
                  <button type="button" class="h-[26px] rounded-sm border border-border px-[9px] text-[12px] text-ink-2 hover:border-border-strong" @click="confirmDeleteId = ''">Cancel</button>
                </template>
                <div v-else class="inline-flex flex-wrap items-center justify-end gap-[6px]">
                  <button
                    type="button"
                    class="h-[26px] rounded-sm border border-border px-[9px] text-[12px] text-ink-2 hover:border-border-strong disabled:cursor-not-allowed disabled:opacity-45"
                    :disabled="isSelf(u) || busyId === u.id"
                    :title="isSelf(u) ? 'You cannot change your own global-admin flag' : ''"
                    @click="toggleAdmin(u)"
                  >{{ u.is_global_admin ? "Revoke admin" : "Grant admin" }}</button>
                  <button
                    type="button"
                    class="h-[26px] rounded-sm bg-accent px-[9px] text-[12px] font-medium text-accent-ink hover:bg-accent-2"
                    @click="openAdd(u)"
                  >{{ addTarget?.id === u.id ? "Add to org ▴" : "Add to org" }}</button>
                  <button
                    type="button"
                    class="h-[26px] rounded-sm border border-down/50 px-[9px] text-[12px] text-down hover:bg-down-weak disabled:cursor-not-allowed disabled:opacity-45"
                    :disabled="isSelf(u)"
                    :title="isSelf(u) ? 'You cannot delete your own account' : ''"
                    @click="confirmDeleteId = u.id ?? ''"
                  >Delete</button>
                </div>
              </td>
            </tr>

            <!-- inline add-to-orgs expansion -->
            <tr v-if="addTarget?.id === u.id">
              <td colspan="5" class="border-b border-l-[3px] border-border border-l-accent bg-inset px-4 pb-4 pt-[14px]">
                <div class="mb-[10px] flex flex-wrap items-baseline gap-2">
                  <b class="text-[13px] font-semibold">Add to organizations</b>
                  <span class="font-mono text-[12px] text-ink-3">{{ u.email }}</span>
                </div>
                <div class="mb-[6px] text-[11px] font-semibold uppercase tracking-[0.05em] text-ink-2">
                  Organizations <span class="font-normal normal-case tracking-normal text-ink-3">— pick one or more, then set a role per organization below</span>
                </div>
                <div class="flex min-h-[32px] flex-wrap items-center gap-[6px]">
                  <button
                    v-for="o in ws.orgs"
                    :key="o.id"
                    type="button"
                    class="inline-flex h-[28px] items-center gap-[6px] rounded-full border px-[11px] text-[12.5px] transition-colors disabled:cursor-not-allowed disabled:opacity-45"
                    :class="isPickedOrg(o.id) ? 'border-accent bg-accent-weak font-medium text-accent' : 'border-border-strong bg-surface text-ink-2 hover:border-ink-3'"
                    :disabled="isMemberOrg(o.id)"
                    :title="isMemberOrg(o.id) ? 'Already a member of this organization' : ''"
                    @click="toggleOrg(o.id)"
                  >
                    <svg v-if="isPickedOrg(o.id)" viewBox="0 0 24 24" class="h-[12px] w-[12px]" fill="none" stroke="currentColor" stroke-width="3"><path d="M5 12l5 5L20 7" /></svg>
                    {{ o.name || o.slug }}
                  </button>
                  <span v-if="!ws.orgs.length" class="text-[12.5px] text-ink-3">No organizations yet.</span>
                </div>

                <div class="mt-3 border-t border-dashed border-border-strong pt-[10px]">
                  <div v-if="!picks.length" class="py-[6px] text-[12.5px] text-ink-3">Pick organizations above — each appears here with its own role.</div>
                  <template v-else>
                    <div class="flex items-center gap-3 pb-2">
                      <span class="text-[11px] font-semibold uppercase tracking-[0.05em] text-ink-2">Roles</span>
                      <span class="ml-auto flex items-center gap-[6px] text-[11px] text-ink-3">
                        Set all to:
                        <select v-model="bulkRole" class="h-[28px] rounded-sm border border-border-strong bg-surface px-[6px] text-[12px] outline-none focus:border-accent" @change="setAllRoles">
                          <option value="" disabled>…</option>
                          <option v-for="r in orgRoles" :key="r" :value="r">{{ r }}</option>
                        </select>
                      </span>
                    </div>
                    <div v-for="p in picks" :key="p.org_id" class="flex items-center gap-3 py-[5px]">
                      <span class="min-w-[120px] text-[13px] font-medium">{{ orgLabel(p.org_id) }}</span>
                      <select v-model="p.role" class="h-[30px] w-[150px] rounded-sm border border-border-strong bg-surface px-[8px] text-[12.5px] outline-none focus:border-accent">
                        <option v-for="r in orgRoles" :key="r" :value="r">{{ r }}</option>
                      </select>
                      <button type="button" class="ml-auto px-1 text-[14px] text-ink-3 hover:text-down" title="Remove from the selection" @click="toggleOrg(p.org_id)">✕</button>
                    </div>
                  </template>
                </div>

                <div class="mt-3 flex justify-end gap-2">
                  <button type="button" class="h-[34px] rounded-sm border border-border px-[14px] text-[13px] text-ink-2 hover:border-border-strong" @click="addTarget = null">Cancel</button>
                  <button type="button" :disabled="!picks.length || adding" class="h-[34px] rounded-sm bg-accent px-[15px] text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50" @click="submitAdd">
                    {{ adding ? "Adding…" : picks.length > 1 ? `Add to ${picks.length} orgs` : "Add" }}
                  </button>
                </div>
              </td>
            </tr>
            </template>

            <tr v-if="!filtered.length && !loading">
              <td colspan="5" class="px-4 py-10 text-center text-[13px] text-ink-3">{{ users.length ? "No users match your search." : "No users yet." }}</td>
            </tr>
          </tbody>
        </table>
      </div>
      <p class="border-t border-border px-4 py-[10px] text-[12px] leading-relaxed text-ink-3">
        Your own row is locked; the last global admin cannot be demoted or deleted. Deleting a user removes their memberships and sessions. <b class="font-medium text-ink-2">Add to org</b> grants an organization-scope role — project-scoped roles (incl. Project Admin) are granted from <b class="font-medium text-ink-2">Members</b> with a project scope.
      </p>
    </section>
  </div>
</template>
