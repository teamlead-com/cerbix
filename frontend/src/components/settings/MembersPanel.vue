<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from "vue";
import { api } from "@/api/client";
import type { components } from "@/api/schema";
import { useSession } from "@/stores/session";
import { useWorkspace } from "@/stores/workspace";
import { relTime } from "@/lib/incident";

type Member = components["schemas"]["Member"];
type Role = components["schemas"]["Role"];
type AuditEntry = components["schemas"]["AuditEntry"];

const ws = useWorkspace();
const session = useSession();

const loading = ref(true);
const members = ref<Member[]>([]);
const error = ref("");
const actionError = ref("");
const query = ref("");
const confirmRemoveId = ref("");

const canManage = computed(() => session.isOrgAdmin(ws.orgId));

const roles: { key: Role; label: string }[] = [
  { key: "org_admin", label: "Org Admin" },
  { key: "project_admin", label: "Project Admin" },
  { key: "editor", label: "Editor" },
  { key: "viewer", label: "Viewer" },
];
const roleLabel = (r?: Role) => roles.find((x) => x.key === r)?.label ?? r ?? "—";

// Role ramp — cool admin tiers, distinct from status colors (mirrors the design system).
const roleColor: Record<string, string> = {
  org_admin: "#7c5cff",
  project_admin: "#3a7de5",
  editor: "#8a8a9d",
  viewer: "#8a8a9d",
};
const isAdminRole = (r?: Role) => r === "org_admin" || r === "project_admin";

// Static capability reference — mirrors the internal/authz matrix.
const permCols = ["Viewer", "Editor", "Project Admin", "Org Admin", "Global Admin"];
const permColColor = ["#8a8a9d", "#55556a", "#3a7de5", "#7c5cff", "#5854f2"];
const caps: { label: string; grants: boolean[] }[] = [
  { label: "View monitors, SLA & incidents", grants: [true, true, true, true, true] },
  { label: "Create, edit & delete monitors", grants: [false, true, true, true, true] },
  { label: "Manage maintenance & notifications", grants: [false, true, true, true, true] },
  { label: "Manage project members & settings", grants: [false, false, true, true, true] },
  { label: "Create/delete projects, manage org members", grants: [false, false, false, true, true] },
  { label: "Manage organizations & global admin", grants: [false, false, false, false, true] },
];

const avatarPalette = ["#5854f2", "#e0399f", "#0d9aa8", "#b97800", "#7c5cff", "#12a05c"];
function avatarColor(id?: string): string {
  if (!id) return avatarPalette[0];
  let h = 0;
  for (const ch of id) h = (h + ch.charCodeAt(0)) % avatarPalette.length;
  return avatarPalette[h];
}
function initials(m: Member): string {
  const n = (m.display_name || m.email || "").trim();
  if (!n) return "?";
  const parts = n.split(/\s+/).filter(Boolean);
  if (parts.length >= 2) return (parts[0][0] + parts[1][0]).toUpperCase();
  return n.slice(0, 2).toUpperCase();
}
const memberName = (m: Member) => m.display_name || m.email || m.user_id || "unknown";
const lastActive = (ts?: string | null) => (ts ? relTime(ts) : "never");

// A membership may only hold roles valid for its scope (org vs project).
function roleOptions(m: Member): { key: Role; label: string }[] {
  return m.project_id
    ? roles.filter((r) => r.key !== "org_admin")
    : roles.filter((r) => r.key !== "project_admin");
}

async function changeRole(m: Member, role: Role) {
  if (role === m.role) return;
  actionError.value = "";
  const res = await api.PATCH("/api/v1/organizations/{orgID}/members/{membershipID}", {
    params: { path: { orgID: ws.orgId, membershipID: m.id! } },
    body: { role },
  });
  if (res.error || !res.data) {
    actionError.value = (res.error as { error?: string })?.error || "Could not change the role.";
    await load(); // revert the <select> to server truth
    return;
  }
  const idx = members.value.findIndex((x) => x.id === m.id);
  if (idx >= 0) members.value[idx].role = res.data.role;
}

async function removeMember(m: Member) {
  actionError.value = "";
  const res = await api.DELETE("/api/v1/organizations/{orgID}/members/{membershipID}", {
    params: { path: { orgID: ws.orgId, membershipID: m.id! } },
  });
  if (res.error) {
    actionError.value = (res.error as { error?: string })?.error || "Could not remove the member.";
  } else {
    members.value = members.value.filter((x) => x.id !== m.id);
  }
  confirmRemoveId.value = "";
}
function projectScope(m: Member): { label: string; sub: string; dashed: boolean } {
  if (!m.project_id) return { label: ws.orgName || "Organization", sub: "org-wide", dashed: false };
  const p = ws.projects.find((x) => x.id === m.project_id);
  return { label: p?.name || p?.slug || "project", sub: roleLabel(m.role), dashed: !p };
}
function fmtDate(ts?: string): string {
  return ts ? new Date(ts).toISOString().slice(0, 10) : "—";
}

const filtered = computed(() => {
  const q = query.value.trim().toLowerCase();
  if (!q) return members.value;
  return members.value.filter(
    (m) => memberName(m).toLowerCase().includes(q) || (m.email ?? "").toLowerCase().includes(q) || roleLabel(m.role).toLowerCase().includes(q),
  );
});

async function load() {
  loading.value = true;
  error.value = "";
  try {
    await ws.init();
    if (!ws.orgId) {
      members.value = [];
      return;
    }
    const res = await api.GET("/api/v1/organizations/{orgID}/members", {
      params: { path: { orgID: ws.orgId } },
    });
    if (res.error) {
      error.value = "You need org-admin rights to view members.";
      members.value = [];
      return;
    }
    members.value = res.data ?? [];
    if (canManage.value) {
      const a = await api.GET("/api/v1/organizations/{orgID}/audit", { params: { path: { orgID: ws.orgId }, query: { limit: 30 } } });
      audit.value = a.data ?? [];
    } else {
      audit.value = [];
    }
  } catch {
    error.value = "Could not load members.";
  } finally {
    loading.value = false;
  }
}

// Audit trail (org admin): recent member/token changes.
const audit = ref<AuditEntry[]>([]);
const actionLabel: Record<string, string> = {
  "member.add": "added a member",
  "member.role_change": "changed a member's role",
  "member.remove": "removed a member",
  "token.create": "created an API token",
  "token.delete": "revoked an API token",
};
const auditActor = (e: AuditEntry) => (e.via_token ? "a service token" : e.actor_name || e.actor_email || "someone");

// Invite by email — the user must have signed in at least once so it resolves to a real account.
const showInvite = ref(false);
const invite = reactive<{ email: string; project_id: string; role: Role }>({
  email: "",
  project_id: "",
  role: "editor",
});
const inviting = ref(false);
const inviteError = ref("");
const canInvite = computed(() => !!invite.email.trim() && !inviting.value);

async function sendInvite() {
  if (!canInvite.value || !ws.orgId) return;
  inviting.value = true;
  inviteError.value = "";
  const body: components["schemas"]["AddMember"] = { email: invite.email.trim(), role: invite.role };
  if (invite.project_id) body.project_id = invite.project_id;
  try {
    const res = await api.POST("/api/v1/organizations/{orgID}/members", {
      params: { path: { orgID: ws.orgId } },
      body,
    });
    if (res.error || !res.data) {
      inviteError.value = (res.error as { error?: string })?.error || "Could not add the member.";
      return;
    }
    invite.email = "";
    invite.project_id = "";
    invite.role = "editor";
    showInvite.value = false;
    await load(); // refresh with the enriched member row (identity + last active)
  } catch {
    inviteError.value = "Could not add the member.";
  } finally {
    inviting.value = false;
  }
}

onMounted(load);
watch(() => ws.orgId, load);
</script>

<template>
  <div>
    <!-- panel head -->
    <div class="mb-4 flex flex-wrap items-start gap-[14px]">
      <div>
        <h2 class="text-[15px] font-semibold tracking-tight">Members</h2>
        <p class="mt-[3px] text-[13px] text-ink-3">People with access to <b class="font-semibold text-ink-2">{{ ws.orgName || "this organization" }}</b> and its projects.</p>
      </div>
      <button
        v-if="canManage"
        type="button"
        class="ml-auto inline-flex h-[34px] items-center gap-[7px] rounded-sm bg-accent px-[13px] text-[13px] font-medium text-accent-ink hover:bg-accent-2"
        @click="showInvite = !showInvite"
      >
        <svg viewBox="0 0 24 24" class="h-[15px] w-[15px]" fill="none" stroke="currentColor" stroke-width="2.2"><path d="M12 5v14M5 12h14" /></svg>
        Invite member
      </button>
    </div>

    <!-- invite panel -->
    <section v-if="showInvite && canManage" class="mb-4 rounded border border-border bg-surface shadow-card">
      <div class="grid grid-cols-[2fr_1fr_1fr_auto] items-end gap-3 p-4 max-[900px]:grid-cols-1">
        <label class="flex flex-col gap-[6px]">
          <span class="text-[11.5px] font-semibold text-ink-2">Email</span>
          <input v-model="invite.email" type="email" placeholder="teammate@example.com" class="h-[38px] rounded-sm border border-border bg-surface-2 px-[11px] font-mono text-[13px] outline-none focus:border-accent focus:bg-surface" />
        </label>
        <label class="flex flex-col gap-[6px]">
          <span class="text-[11.5px] font-semibold text-ink-2">Scope</span>
          <select v-model="invite.project_id" class="h-[38px] rounded-sm border border-border bg-surface-2 px-[11px] text-[13px] outline-none focus:border-accent focus:bg-surface">
            <option value="">Organization</option>
            <option v-for="p in ws.projects" :key="p.id" :value="p.id">{{ p.name || p.slug }}</option>
          </select>
        </label>
        <label class="flex flex-col gap-[6px]">
          <span class="text-[11.5px] font-semibold text-ink-2">Role</span>
          <select v-model="invite.role" class="h-[38px] rounded-sm border border-border bg-surface-2 px-[11px] text-[13px] outline-none focus:border-accent focus:bg-surface">
            <option v-for="r in roles" :key="r.key" :value="r.key">{{ r.label }}</option>
          </select>
        </label>
        <button type="button" :disabled="!canInvite" class="inline-flex h-[38px] items-center gap-[7px] rounded-sm bg-accent px-[15px] text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50" @click="sendInvite">
          <svg viewBox="0 0 24 24" class="h-[15px] w-[15px]" fill="none" stroke="currentColor" stroke-width="2"><path d="M4 4l16 8-16 8 4-8-4-8z" /></svg>
          {{ inviting ? "Sending…" : "Send invite" }}
        </button>
      </div>
      <div v-if="inviteError" class="border-t border-border px-4 py-2 text-[12.5px] text-down">{{ inviteError }}</div>
    </section>

    <div v-if="error" class="mb-4 rounded border border-border bg-surface p-4 text-[13px] text-ink-3 shadow-card">{{ error }}</div>
    <div v-if="actionError" class="mb-4 rounded-sm border border-down/40 bg-down-weak px-4 py-2 text-[13px] text-down">{{ actionError }}</div>

    <!-- members table -->
    <section v-if="!error" class="rounded border border-border bg-surface shadow-card">
      <div class="flex items-center gap-[10px] px-4 pb-3 pt-[14px]">
        <h3 class="text-[13px] font-semibold">Members</h3>
        <span class="font-mono text-[12px] text-ink-3">{{ members.length }}</span>
        <div class="relative ml-auto">
          <svg viewBox="0 0 24 24" class="pointer-events-none absolute left-[9px] top-1/2 h-[15px] w-[15px] -translate-y-1/2 text-ink-3" fill="none" stroke="currentColor" stroke-width="2"><circle cx="11" cy="11" r="7" /><path d="M21 21l-4-4" /></svg>
          <input v-model="query" type="text" placeholder="Search members" class="h-[32px] w-[200px] rounded-sm border border-border bg-surface pl-[30px] pr-[10px] text-[13px] outline-none focus:border-accent max-[640px]:w-[130px]" />
        </div>
      </div>
      <div class="overflow-x-auto">
        <table class="w-full text-[13px]">
          <thead>
            <tr class="text-[10.5px] uppercase tracking-[0.06em] text-ink-3">
              <th class="border-b border-border px-4 py-[10px] text-left">Member</th>
              <th class="border-b border-border px-4 py-[10px] text-left">Role</th>
              <th class="border-b border-border px-4 py-[10px] text-left">Project access</th>
              <th class="border-b border-border px-4 py-[10px] text-left">Last active</th>
              <th v-if="canManage" class="border-b border-border px-4 py-[10px] text-right">Actions</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="m in filtered" :key="m.id" class="hover:bg-surface-2">
              <td class="border-b border-border px-4 py-[11px]">
                <div class="flex items-center gap-[11px]">
                  <span class="grid h-8 w-8 flex-none place-items-center rounded-full text-[11.5px] font-semibold text-white" :style="{ background: avatarColor(m.user_id) }">{{ initials(m) }}</span>
                  <span class="flex flex-col leading-tight">
                    <b class="flex items-center gap-[6px] text-[13px]">{{ memberName(m) }}<span v-if="m.user_id === session.user?.id" class="rounded-full bg-accent-weak px-[6px] py-px text-[10px] font-bold uppercase tracking-[0.04em] text-accent">you</span></b>
                    <span class="font-mono text-[11px] text-ink-3">{{ m.email || m.user_id }}</span>
                  </span>
                </div>
              </td>
              <td class="border-b border-border px-4 py-[11px]">
                <select
                  v-if="canManage"
                  class="h-[30px] rounded-sm border border-border bg-surface px-[9px] text-[12.5px] outline-none focus:border-accent"
                  :value="m.role"
                  @change="changeRole(m, ($event.target as HTMLSelectElement).value as Role)"
                >
                  <option v-for="r in roleOptions(m)" :key="r.key" :value="r.key">{{ r.label }}</option>
                </select>
                <span v-else class="inline-flex h-[26px] items-center gap-[7px] rounded-full border border-border px-[10px] text-[12.5px] font-medium" :style="isAdminRole(m.role) ? { color: roleColor[m.role ?? ''], borderColor: roleColor[m.role ?? ''] + '4d' } : {}">
                  <span class="h-2 w-2 rounded-[2px]" :style="{ background: roleColor[m.role ?? ''] || '#8a8a9d' }"></span>
                  {{ roleLabel(m.role) }}
                </span>
              </td>
              <td class="border-b border-border px-4 py-[11px]">
                <span class="inline-block rounded-xs border border-border bg-inset px-[7px] py-[2px] font-mono text-[11px] text-ink-2" :class="projectScope(m).dashed ? 'border-dashed' : ''">
                  <b class="font-semibold text-ink">{{ projectScope(m).label }}</b> · {{ projectScope(m).sub }}
                </span>
              </td>
              <td class="border-b border-border px-4 py-[11px] font-mono text-[12.5px] text-ink-3" :title="'added ' + fmtDate(m.created_at)">{{ lastActive(m.last_active_at) }}</td>
              <td v-if="canManage" class="border-b border-border px-4 py-[11px] text-right">
                <template v-if="confirmRemoveId === m.id">
                  <span class="mr-2 text-[12px] text-ink-3">Remove?</span>
                  <button type="button" class="mr-1 h-[26px] rounded-sm bg-down px-[9px] text-[12px] font-medium text-white hover:opacity-90" @click="removeMember(m)">Confirm</button>
                  <button type="button" class="h-[26px] rounded-sm border border-border px-[9px] text-[12px] text-ink-2 hover:border-border-strong" @click="confirmRemoveId = ''">Cancel</button>
                </template>
                <button v-else type="button" class="text-ink-3 hover:text-down" aria-label="Remove member" @click="confirmRemoveId = m.id ?? ''">
                  <svg viewBox="0 0 24 24" class="h-[16px] w-[16px]" fill="none" stroke="currentColor" stroke-width="2"><path d="M4 7h16M9 7V5a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v2M6 7l1 13a1 1 0 0 0 1 1h8a1 1 0 0 0 1-1l1-13" /></svg>
                </button>
              </td>
            </tr>
            <tr v-if="!filtered.length && !loading">
              <td :colspan="canManage ? 5 : 4" class="px-4 py-10 text-center text-[13px] text-ink-3">{{ members.length ? "No members match your search." : "No members yet." }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>

    <!-- role permissions matrix -->
    <div class="mt-7">
      <div class="mb-3 flex items-center gap-[10px]">
        <h3 class="text-[13px] font-semibold">Role permissions</h3>
        <span class="font-mono text-[12px] text-ink-3">reference</span>
      </div>
      <section class="overflow-x-auto rounded border border-border bg-surface shadow-card">
        <table class="w-full text-[13px]">
          <thead>
            <tr>
              <th class="border-b border-border px-3 py-[11px] text-left text-[10.5px] font-semibold uppercase tracking-[0.06em] text-ink-3">Capability</th>
              <th v-for="(c, i) in permCols" :key="c" class="border-b border-border px-3 py-[11px] text-center text-[11.5px] font-semibold">
                <span class="inline-flex items-center gap-[6px]"><i class="h-2 w-2 rounded-[2px]" :style="{ background: permColColor[i] }"></i>{{ c }}</span>
              </th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="row in caps" :key="row.label" class="hover:bg-surface-2">
              <td class="border-b border-border px-3 py-[10px] text-ink-2">{{ row.label }}</td>
              <td v-for="(g, i) in row.grants" :key="i" class="border-b border-border px-3 py-[10px] text-center">
                <svg v-if="g" class="mx-auto h-[15px] w-[15px] text-up" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.6"><path d="M5 12l5 5L20 7" /></svg>
                <span v-else class="text-border-strong">–</span>
              </td>
            </tr>
          </tbody>
        </table>
      </section>
      <p class="mt-3 text-[12px] leading-relaxed text-ink-3">Roles can be granted at the <b class="text-ink-2">organization</b> scope (all projects) or on a <b class="text-ink-2">single project</b>. <span class="font-mono">Global Admin</span> is a user-level flag with full access everywhere.</p>
    </div>

    <!-- audit trail (org admin) -->
    <div v-if="canManage" class="mt-7">
      <div class="mb-3 flex items-center gap-[10px]">
        <h3 class="text-[13px] font-semibold">Audit log</h3>
        <span class="font-mono text-[12px] text-ink-3">member & token changes</span>
      </div>
      <section class="rounded border border-border bg-surface shadow-card">
        <ul>
          <li v-for="e in audit" :key="e.id" class="flex items-center gap-3 border-b border-border px-4 py-[11px] last:border-b-0">
            <span class="h-[7px] w-[7px] flex-none rounded-full" :class="e.action?.startsWith('member.remove') || e.action?.startsWith('token.delete') ? 'bg-down' : 'bg-accent'"></span>
            <span class="min-w-0 text-[13px]">
              <b class="font-medium">{{ auditActor(e) }}</b> {{ actionLabel[e.action ?? ""] || e.action }}<span v-if="e.target" class="text-ink-3"> · <span class="font-mono text-[12px]">{{ e.target }}</span></span>
            </span>
            <span class="ml-auto flex-none font-mono text-[11.5px] text-ink-3">{{ relTime(e.created_at) }}</span>
          </li>
          <li v-if="!audit.length" class="px-4 py-6 text-center text-[13px] text-ink-3">No recorded changes yet.</li>
        </ul>
      </section>
    </div>
  </div>
</template>
