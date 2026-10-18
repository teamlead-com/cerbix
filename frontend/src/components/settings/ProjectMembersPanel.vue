<script setup lang="ts">
// Project members (iter-0206, FR-040, func-project-members.md; mock docs/design/mock-project-members.html).
// A project admin manages the members of the CURRENT project: its project-scoped memberships
// (role, remove) and an invite form fixed to this project and to project roles. Who reaches the
// project through the organization is shown read-only. The organization Members tab is separate.
import { computed, onMounted, reactive, ref, watch } from "vue";
import { api } from "@/api/client";
import type { components } from "@/api/schema";
import { relTime } from "@/lib/incident";
import { useSession } from "@/stores/session";
import { useWorkspace } from "@/stores/workspace";

type Member = components["schemas"]["Member"];
type InheritedMember = components["schemas"]["InheritedMember"];
type ProjectRole = components["schemas"]["ProjectRole"];
type Role = components["schemas"]["Role"];

// Emitted after the signed-in user changed or removed THEIR OWN project membership: the parent
// refreshes the session and leaves the tab if they no longer manage the project.
const emit = defineEmits<{ (e: "self-changed"): void }>();

const ws = useWorkspace();
const session = useSession();

const loading = ref(true);
const members = ref<Member[]>([]);
const inherited = ref<InheritedMember[]>([]);
const error = ref("");
const actionError = ref("");
const confirmRemoveId = ref("");

// Only project roles: a project admin can never grant an organization-wide role.
const projectRoles: { key: ProjectRole; label: string }[] = [
  { key: "project_admin", label: "Project Admin" },
  { key: "editor", label: "Editor" },
  { key: "viewer", label: "Viewer" },
];
const allRoleLabels: Record<string, string> = {
  org_admin: "Org Admin",
  project_admin: "Project Admin",
  editor: "Editor",
  viewer: "Viewer",
};
const roleLabel = (r?: Role) => (r ? allRoleLabels[r] ?? r : "—");
const roleColor: Record<string, string> = {
  org_admin: "#7c5cff",
  project_admin: "#3a7de5",
  editor: "#8a8a9d",
  viewer: "#8a8a9d",
};
const isAdminRole = (r?: Role) => r === "org_admin" || r === "project_admin";

const avatarPalette = ["#5854f2", "#e0399f", "#0d9aa8", "#b97800", "#7c5cff", "#12a05c"];
function avatarColor(seed?: string): string {
  if (!seed) return avatarPalette[0];
  let h = 0;
  for (const ch of seed) h = (h + ch.charCodeAt(0)) % avatarPalette.length;
  return avatarPalette[h];
}
function initials(name?: string, email?: string): string {
  const n = (name || email || "").trim();
  if (!n) return "?";
  const parts = n.split(/\s+/).filter(Boolean);
  if (parts.length >= 2) return (parts[0][0] + parts[1][0]).toUpperCase();
  return n.slice(0, 2).toUpperCase();
}
const lastActive = (ts?: string | null) => (ts ? relTime(ts) : "never");
const isSelf = (m: Member) => m.user_id === session.user?.id;
const projectLabel = computed(() => ws.projectName || "this project");

function apiError(err: unknown, fallback: string): string {
  return (err as { error?: string } | undefined)?.error || fallback;
}

async function load() {
  loading.value = true;
  error.value = "";
  try {
    await ws.init();
    if (!ws.projectId) {
      members.value = [];
      inherited.value = [];
      return;
    }
    const res = await api.GET("/api/v1/projects/{projectID}/members", {
      params: { path: { projectID: ws.projectId } },
    });
    if (res.error || !res.data) {
      error.value = "You need project-admin rights to manage this project's members.";
      members.value = [];
      inherited.value = [];
      return;
    }
    members.value = res.data.members ?? [];
    inherited.value = res.data.inherited ?? [];
  } catch {
    error.value = "Could not load the project's members.";
  } finally {
    loading.value = false;
  }
}

async function changeRole(m: Member, role: ProjectRole) {
  if (role === m.role) return;
  actionError.value = "";
  const res = await api.PATCH("/api/v1/projects/{projectID}/members/{membershipID}", {
    params: { path: { projectID: ws.projectId, membershipID: m.id! } },
    body: { role },
  });
  if (res.error || !res.data) {
    actionError.value = apiError(res.error, "Could not change the role.");
    await load(); // revert the <select> to server truth
    return;
  }
  if (isSelf(m)) {
    emit("self-changed");
    return;
  }
  const idx = members.value.findIndex((x) => x.id === m.id);
  if (idx >= 0) members.value[idx].role = res.data.role;
}

async function removeMember(m: Member) {
  actionError.value = "";
  const res = await api.DELETE("/api/v1/projects/{projectID}/members/{membershipID}", {
    params: { path: { projectID: ws.projectId, membershipID: m.id! } },
  });
  confirmRemoveId.value = "";
  if (res.error) {
    actionError.value = apiError(res.error, "Could not remove the member.");
    return;
  }
  if (isSelf(m)) {
    emit("self-changed");
    return;
  }
  members.value = members.value.filter((x) => x.id !== m.id);
}

const showInvite = ref(false);
const invite = reactive<{ email: string; role: ProjectRole }>({ email: "", role: "editor" });
const inviting = ref(false);
const inviteError = ref("");
const canInvite = computed(() => !!invite.email.trim() && !inviting.value);

async function sendInvite() {
  if (!canInvite.value || !ws.projectId) return;
  inviting.value = true;
  inviteError.value = "";
  try {
    const res = await api.POST("/api/v1/projects/{projectID}/members", {
      params: { path: { projectID: ws.projectId } },
      body: { email: invite.email.trim(), role: invite.role },
    });
    if (res.error || !res.data) {
      inviteError.value = apiError(res.error, "Could not add the member.");
      return;
    }
    invite.email = "";
    invite.role = "editor";
    showInvite.value = false;
    await load();
  } catch {
    inviteError.value = "Could not add the member.";
  } finally {
    inviting.value = false;
  }
}

onMounted(load);
watch(() => ws.projectId, load);
</script>

<template>
  <div>
    <div class="mb-4 flex flex-wrap items-start gap-[14px]">
      <div>
        <h2 class="text-[15px] font-semibold tracking-tight">Members</h2>
        <p class="mt-[3px] text-[13px] text-ink-3">People with access to <b class="font-semibold text-ink-2">{{ projectLabel }}</b>.</p>
      </div>
      <button
        v-if="!error"
        type="button"
        data-testid="project-invite-toggle"
        class="ml-auto inline-flex h-[34px] items-center gap-[7px] rounded-sm bg-accent px-[13px] text-[13px] font-medium text-accent-ink hover:bg-accent-2"
        @click="showInvite = !showInvite"
      >
        <svg viewBox="0 0 24 24" class="h-[15px] w-[15px]" fill="none" stroke="currentColor" stroke-width="2.2"><path d="M12 5v14M5 12h14" /></svg>
        Invite to project
      </button>
    </div>

    <section v-if="showInvite && !error" class="mb-4 rounded border border-border bg-surface shadow-card" data-testid="project-invite">
      <div class="grid grid-cols-[2fr_1fr_auto] items-end gap-3 p-4 max-[640px]:grid-cols-1">
        <label class="flex flex-col gap-[6px]">
          <span class="text-[11.5px] font-semibold text-ink-2">Email</span>
          <input v-model="invite.email" type="email" placeholder="teammate@example.com" data-testid="project-invite-email" class="h-[38px] rounded-sm border border-border bg-surface-2 px-[11px] font-mono text-[13px] outline-none focus:border-accent focus:bg-surface" />
        </label>
        <label class="flex flex-col gap-[6px]">
          <span class="text-[11.5px] font-semibold text-ink-2">Role</span>
          <select v-model="invite.role" data-testid="project-invite-role" class="h-[38px] rounded-sm border border-border bg-surface-2 px-[11px] text-[13px] outline-none focus:border-accent focus:bg-surface">
            <option v-for="r in projectRoles" :key="r.key" :value="r.key">{{ r.label }}</option>
          </select>
        </label>
        <button type="button" :disabled="!canInvite" data-testid="project-invite-send" class="inline-flex h-[38px] items-center gap-[7px] rounded-sm bg-accent px-[15px] text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50" @click="sendInvite">
          {{ inviting ? "Adding…" : "Add to project" }}
        </button>
        <p class="col-span-full -mt-1 text-[12px] text-ink-3">Grants access to <b class="text-ink-2">{{ projectLabel }}</b> only. Organization-wide roles are managed by an Org Admin.</p>
      </div>
      <div v-if="inviteError" class="border-t border-border px-4 py-2 text-[12.5px] text-down" data-testid="project-invite-error">{{ inviteError }}</div>
    </section>

    <div v-if="error" class="mb-4 rounded border border-border bg-surface p-4 text-[13px] text-ink-3 shadow-card">{{ error }}</div>
    <div v-if="actionError" class="mb-4 rounded-sm border border-down/40 bg-down-weak px-4 py-2 text-[13px] text-down">{{ actionError }}</div>

    <template v-if="!error">
      <section class="rounded border border-border bg-surface shadow-card" data-testid="project-members">
        <div class="flex items-center gap-[10px] px-4 pb-3 pt-[14px]">
          <h3 class="text-[13px] font-semibold">Project members</h3>
          <span class="font-mono text-[12px] text-ink-3">{{ members.length }}</span>
        </div>
        <div class="overflow-x-auto">
          <table class="w-full text-[13px]">
            <thead>
              <tr class="text-[10.5px] uppercase tracking-[0.06em] text-ink-3">
                <th class="border-b border-border px-4 py-[10px] text-left">Member</th>
                <th class="border-b border-border px-4 py-[10px] text-left">Role</th>
                <th class="border-b border-border px-4 py-[10px] text-left">Last active</th>
                <th class="border-b border-border px-4 py-[10px] text-right">Actions</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="m in members" :key="m.id" class="hover:bg-surface-2" :data-testid="`project-member-${m.email}`">
                <td class="border-b border-border px-4 py-[11px]">
                  <div class="flex items-center gap-[11px]">
                    <span class="grid h-8 w-8 flex-none place-items-center rounded-full text-[11.5px] font-semibold text-white" :style="{ background: avatarColor(m.user_id) }">{{ initials(m.display_name, m.email) }}</span>
                    <span class="flex flex-col leading-tight">
                      <b class="flex items-center gap-[6px] text-[13px]">{{ m.display_name || m.email }}<span v-if="isSelf(m)" class="rounded-full bg-accent-weak px-[6px] py-px text-[10px] font-bold uppercase tracking-[0.04em] text-accent">you</span></b>
                      <span class="font-mono text-[11px] text-ink-3">{{ m.email }}</span>
                    </span>
                  </div>
                </td>
                <td class="border-b border-border px-4 py-[11px]">
                  <select
                    class="h-[30px] rounded-sm border border-border bg-surface px-[9px] text-[12.5px] outline-none focus:border-accent"
                    :value="m.role"
                    data-testid="project-member-role"
                    @change="changeRole(m, ($event.target as HTMLSelectElement).value as ProjectRole)"
                  >
                    <option v-for="r in projectRoles" :key="r.key" :value="r.key">{{ r.label }}</option>
                  </select>
                </td>
                <td class="border-b border-border px-4 py-[11px] font-mono text-[12.5px] text-ink-3">{{ lastActive(m.last_active_at) }}</td>
                <td class="border-b border-border px-4 py-[11px] text-right">
                  <template v-if="confirmRemoveId === m.id">
                    <span class="mr-2 text-[12px] text-ink-3">{{ isSelf(m) ? "Remove yourself? You will no longer manage this project's members." : `Remove from ${projectLabel}? The account is kept.` }}</span>
                    <button type="button" data-testid="project-member-confirm-remove" class="mr-1 h-[26px] rounded-sm bg-down px-[9px] text-[12px] font-medium text-white hover:opacity-90" @click="removeMember(m)">Remove</button>
                    <button type="button" class="h-[26px] rounded-sm border border-border px-[9px] text-[12px] text-ink-2 hover:border-border-strong" @click="confirmRemoveId = ''">Cancel</button>
                  </template>
                  <button
                    v-else
                    type="button"
                    class="text-ink-3 hover:text-down"
                    aria-label="Remove from the project"
                    data-testid="project-member-remove"
                    :title="`Remove from ${projectLabel} — only the project membership is revoked`"
                    @click="confirmRemoveId = m.id ?? ''"
                  >
                    <svg viewBox="0 0 24 24" class="h-[16px] w-[16px]" fill="none" stroke="currentColor" stroke-width="2"><path d="M4 7h16M9 7V5a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v2M6 7l1 13a1 1 0 0 0 1 1h8a1 1 0 0 0 1-1l1-13" /></svg>
                  </button>
                </td>
              </tr>
              <tr v-if="!members.length && !loading">
                <td colspan="4" class="px-4 py-10 text-center text-[13px] text-ink-3">No project members yet.</td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>

      <section class="mt-4 rounded border border-border bg-surface shadow-card" data-testid="project-inherited">
        <div class="flex items-center gap-[10px] px-4 pb-1 pt-[14px]">
          <h3 class="text-[13px] font-semibold">Access through the organization</h3>
          <span class="font-mono text-[12px] text-ink-3">{{ inherited.length }}</span>
          <span class="ml-auto text-[12px] text-ink-3">read-only</span>
        </div>
        <p class="px-4 pb-3 text-[12px] text-ink-3">These people reach every project in <b class="text-ink-2">{{ ws.orgName || "the organization" }}</b>. Their access is managed by an Org Admin in Organization → Members.</p>
        <div class="overflow-x-auto">
          <table class="w-full text-[13px]">
            <thead>
              <tr class="text-[10.5px] uppercase tracking-[0.06em] text-ink-3">
                <th class="border-b border-border px-4 py-[10px] text-left">Member</th>
                <th class="border-b border-border px-4 py-[10px] text-left">Organization role</th>
              </tr>
            </thead>
            <tbody>
              <!-- Two OIDC users may share an email (spec, "Known exposure"): the index keeps keys unique. -->
              <tr v-for="(im, i) in inherited" :key="`${i}:${im.email}:${im.role}`" class="bg-surface-2">
                <td class="border-b border-border px-4 py-[11px]">
                  <div class="flex items-center gap-[11px]">
                    <span class="grid h-8 w-8 flex-none place-items-center rounded-full text-[11.5px] font-semibold text-white" :style="{ background: avatarColor(im.email) }">{{ initials(im.display_name, im.email) }}</span>
                    <span class="flex flex-col leading-tight">
                      <b class="text-[13px]">{{ im.display_name || im.email }}</b>
                      <span class="font-mono text-[11px] text-ink-3">{{ im.email }}</span>
                    </span>
                  </div>
                </td>
                <td class="border-b border-border px-4 py-[11px]">
                  <span class="inline-flex h-[26px] items-center gap-[7px] rounded-full border border-border px-[10px] text-[12.5px] font-medium" :style="isAdminRole(im.role) ? { color: roleColor[im.role], borderColor: roleColor[im.role] + '4d' } : {}">
                    <span class="h-2 w-2 rounded-[2px]" :style="{ background: roleColor[im.role] || '#8a8a9d' }"></span>
                    {{ roleLabel(im.role) }}
                  </span>
                </td>
              </tr>
              <tr v-if="!inherited.length && !loading">
                <td colspan="2" class="px-4 py-6 text-center text-[13px] text-ink-3">No organization-wide members.</td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>
    </template>
  </div>
</template>
