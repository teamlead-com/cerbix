<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from "vue";
import { useRoute } from "vue-router";
import { api } from "@/api/client";
import type { components } from "@/api/schema";
import AppShell from "@/components/AppShell.vue";
import MembersPanel from "@/components/settings/MembersPanel.vue";
import UsersPanel from "@/components/settings/UsersPanel.vue";
import { useBranding } from "@/stores/branding";
import { useSession } from "@/stores/session";
import { useWorkspace } from "@/stores/workspace";

type Channel = components["schemas"]["NotificationChannel"];
type ChannelType = NonNullable<Channel["type"]>;
type ApiToken = components["schemas"]["ApiToken"];
type Webhook = components["schemas"]["Webhook"];
type Role = components["schemas"]["Role"];

const ws = useWorkspace();
const session = useSession();
// Channels are project-write; API tokens and webhooks are org-manage.
const canWriteChannels = computed(() => session.canProjectWrite(ws.orgId, ws.projectId));
const canManageOrg = computed(() => session.isOrgAdmin(ws.orgId));

type Tab =
  | "channels"
  | "incoming"
  | "authentication"
  | "branding"
  | "alerting"
  | "monitordefaults"
  | "email"
  | "users"
  | "members"
  | "tokens"
  | "webhooks"
  | "security";
const instanceTabs: { key: Tab; label: string; scope: string }[] = [
  { key: "users", label: "Users", scope: "instance" },
  { key: "authentication", label: "Authentication", scope: "instance" },
  { key: "branding", label: "Branding", scope: "instance" },
  { key: "alerting", label: "Alerting", scope: "instance" },
  { key: "monitordefaults", label: "Monitor defaults", scope: "instance" },
  { key: "email", label: "Email", scope: "instance" },
];
const tabs = computed<{ key: Tab; label: string; scope: string }[]>(() => [
  { key: "channels", label: "Notification channels", scope: "project" },
  { key: "incoming", label: "Incoming alerts", scope: "project" },
  // Instance-wide settings — global-admin only.
  ...(session.isGlobalAdmin ? instanceTabs : []),
  { key: "members", label: "Members", scope: "org" },
  { key: "tokens", label: "API tokens", scope: "org" },
  { key: "webhooks", label: "Webhooks", scope: "org" },
  { key: "security", label: "Security", scope: "account" },
]);
const tab = ref<Tab>("channels");

// Deep links: /settings?tab=members. Invalid or forbidden keys fall back to the default.
const route = useRoute();
const queryTab = route.query.tab;
if (typeof queryTab === "string" && tabs.value.some((t) => t.key === queryTab)) {
  tab.value = queryTab as Tab;
}

// Group the nav by scope so the (now ~10) settings don't sprawl across one row.
const scopeLabels: Record<string, string> = {
  project: "Project",
  instance: "Administration",
  org: "Organization",
  account: "Account",
};
const tabGroups = computed(() =>
  ["project", "org", "instance", "account"]
    .map((s) => ({ scope: s, label: scopeLabels[s], items: tabs.value.filter((t) => t.scope === s) }))
    .filter((g) => g.items.length),
);

// Alertmanager receiver URL for the current project (absolute, so it's paste-ready
// into an Alertmanager webhook_config).
const receiverUrl = computed(() =>
  ws.projectId ? `${window.location.origin}/api/v1/projects/${ws.projectId}/alerts/alertmanager` : "",
);

// ── Authentication / OIDC (instance-wide, global-admin only) ──────────────
const oidcRedirectDefault = `${window.location.origin}/auth/callback`;
const oidc = reactive({
  loaded: false,
  configured: false,
  active: false,
  clientSecretSet: false,
  reloadError: "",
  saving: false,
  error: "",
  scopeInput: "",
  adminInput: "",
  form: {
    enabled: false,
    issuer: "",
    client_id: "",
    client_secret: "",
    redirect_url: oidcRedirectDefault,
    scopes: ["openid", "email", "profile"] as string[],
    post_logout_url: "",
    button_label: "",
    bootstrap_admins: [] as string[],
  },
});

async function loadOIDC() {
  oidc.error = "";
  const res = await api.GET("/api/v1/settings/oidc");
  if (res.error) {
    oidc.error = "You need global-admin rights to manage authentication.";
    oidc.loaded = true;
    return;
  }
  const d = res.data!;
  oidc.configured = !!d.configured;
  oidc.active = !!d.active;
  oidc.clientSecretSet = !!d.client_secret_set;
  oidc.reloadError = d.reload_error || "";
  oidc.form.enabled = !!d.enabled;
  oidc.form.issuer = d.issuer || "";
  oidc.form.client_id = d.client_id || "";
  oidc.form.client_secret = "";
  oidc.form.redirect_url = d.redirect_url || oidcRedirectDefault;
  oidc.form.scopes = d.scopes?.length ? [...d.scopes] : ["openid", "email", "profile"];
  oidc.form.post_logout_url = d.post_logout_url || "";
  oidc.form.button_label = d.button_label || "";
  oidc.form.bootstrap_admins = d.bootstrap_admins ? [...d.bootstrap_admins] : [];
  oidc.loaded = true;
}

function addChip(list: string[], value: string) {
  const v = value.trim();
  if (v && !list.includes(v)) list.push(v);
}
function removeChip(list: string[], i: number) {
  list.splice(i, 1);
}

async function saveOIDC() {
  oidc.saving = true;
  oidc.error = "";
  try {
    const body: Record<string, unknown> = {
      enabled: oidc.form.enabled,
      issuer: oidc.form.issuer,
      client_id: oidc.form.client_id,
      redirect_url: oidc.form.redirect_url,
      scopes: oidc.form.scopes,
      post_logout_url: oidc.form.post_logout_url,
      button_label: oidc.form.button_label,
      bootstrap_admins: oidc.form.bootstrap_admins,
    };
    // Send the secret only when the admin typed a new one (blank preserves it).
    if (oidc.form.client_secret) body.client_secret = oidc.form.client_secret;
    const res = await api.PUT("/api/v1/settings/oidc", { body: body as never });
    if (res.error) {
      oidc.error = (res.error as { error?: string })?.error || "Could not save the configuration.";
      return;
    }
    const d = res.data!;
    oidc.configured = true;
    oidc.active = !!d.active;
    oidc.clientSecretSet = !!d.client_secret_set;
    oidc.reloadError = d.reload_error || "";
    oidc.form.client_secret = "";
  } finally {
    oidc.saving = false;
  }
}

// ── Instance settings groups (branding / alerting / monitor defaults) ──────
const brand = reactive({
  loaded: false, saving: false, error: "", saved: false,
  form: { product_name: "", accent_color: "", logo_url: "", footer_text: "", support_url: "",
    announcement: { enabled: false, text: "", level: "info" } },
});
const alerting = reactive({ loaded: false, saving: false, saved: false, enabled: false });
const monDefaults = reactive({
  loaded: false, saving: false, error: "", saved: false,
  form: { interval_seconds: 60, timeout_seconds: 10, retries: 0, failure_threshold: 1, renotify_seconds: 0, auto_incident: true },
});

function flash(o: { saved: boolean }) {
  o.saved = true;
  setTimeout(() => (o.saved = false), 1600);
}

async function loadBranding() {
  const res = await api.GET("/api/v1/settings/branding");
  const d = res.data;
  if (d) {
    brand.form.product_name = d.product_name || "";
    brand.form.accent_color = d.accent_color || "";
    brand.form.logo_url = d.logo_url || "";
    brand.form.footer_text = d.footer_text || "";
    brand.form.support_url = d.support_url || "";
    brand.form.announcement = { enabled: d.announcement?.enabled ?? false, text: d.announcement?.text || "", level: d.announcement?.level || "info" };
  }
  brand.loaded = true;
}
async function saveBranding() {
  brand.saving = true; brand.error = "";
  try {
    const res = await api.PUT("/api/v1/settings/branding", { body: brand.form as never });
    if (res.error) { brand.error = (res.error as { error?: string })?.error || "Could not save."; return; }
    await useBranding().load(); // re-apply live theming
    flash(brand);
  } finally { brand.saving = false; }
}

async function loadAlerting() {
  const res = await api.GET("/api/v1/settings/alerting");
  alerting.enabled = res.data?.global_silence?.enabled ?? false;
  alerting.loaded = true;
}
async function saveAlerting(enabled: boolean) {
  alerting.saving = true;
  try {
    const res = await api.PUT("/api/v1/settings/alerting", { body: { global_silence: { enabled } } as never });
    if (!res.error) { alerting.enabled = res.data?.global_silence?.enabled ?? enabled; flash(alerting); }
  } finally { alerting.saving = false; }
}

async function loadMonDefaults() {
  const res = await api.GET("/api/v1/settings/monitor-defaults");
  const d = res.data;
  if (d) {
    monDefaults.form = {
      interval_seconds: d.interval_seconds ?? 60, timeout_seconds: d.timeout_seconds ?? 10,
      retries: d.retries ?? 0, failure_threshold: d.failure_threshold ?? 1,
      renotify_seconds: d.renotify_seconds ?? 0, auto_incident: d.auto_incident ?? true,
    };
  }
  monDefaults.loaded = true;
}
async function saveMonDefaults() {
  monDefaults.saving = true; monDefaults.error = "";
  try {
    const f = monDefaults.form;
    const body = {
      interval_seconds: Number(f.interval_seconds), timeout_seconds: Number(f.timeout_seconds),
      retries: Number(f.retries), failure_threshold: Number(f.failure_threshold),
      renotify_seconds: Number(f.renotify_seconds), auto_incident: f.auto_incident,
    };
    const res = await api.PUT("/api/v1/settings/monitor-defaults", { body: body as never });
    if (res.error) { monDefaults.error = (res.error as { error?: string })?.error || "Could not save."; return; }
    flash(monDefaults);
  } finally { monDefaults.saving = false; }
}

// ── Auth policy (in the Authentication tab) ───────────────────────────────
const authPolicy = reactive({
  loaded: false, saving: false, error: "", saved: false,
  form: { min_password_len: 8, session_ttl_seconds: 86400, require_totp: "none", allowed_email_domains: [] as string[] },
  domainInput: "",
});
async function loadAuthPolicy() {
  const res = await api.GET("/api/v1/settings/auth-policy");
  const d = res.data;
  if (d) {
    authPolicy.form = {
      min_password_len: d.min_password_len ?? 8, session_ttl_seconds: d.session_ttl_seconds ?? 86400,
      require_totp: d.require_totp || "none", allowed_email_domains: d.allowed_email_domains ? [...d.allowed_email_domains] : [],
    };
  }
  authPolicy.loaded = true;
}
async function saveAuthPolicy() {
  authPolicy.saving = true; authPolicy.error = "";
  try {
    const f = authPolicy.form;
    const body = {
      min_password_len: Number(f.min_password_len), session_ttl_seconds: Number(f.session_ttl_seconds),
      require_totp: f.require_totp, allowed_email_domains: f.allowed_email_domains,
    };
    const res = await api.PUT("/api/v1/settings/auth-policy", { body: body as never });
    if (res.error) { authPolicy.error = (res.error as { error?: string })?.error || "Could not save."; return; }
    flash(authPolicy);
  } finally { authPolicy.saving = false; }
}

// ── Email / SMTP (Email tab) ──────────────────────────────────────────────
const mailS = reactive({
  loaded: false, saving: false, error: "", saved: false, passwordSet: false, deliverable: false,
  form: { enabled: false, smtp_host: "", smtp_port: 587, smtp_username: "", smtp_password: "", from: "", public_base_url: "" },
});
async function loadMail() {
  const res = await api.GET("/api/v1/settings/mail");
  const d = res.data;
  if (d) {
    mailS.passwordSet = d.smtp_password_set ?? false;
    mailS.deliverable = d.deliverable ?? false;
    mailS.form = {
      enabled: d.enabled ?? false, smtp_host: d.smtp_host || "", smtp_port: d.smtp_port || 587,
      smtp_username: d.smtp_username || "", smtp_password: "", from: d.from || "", public_base_url: d.public_base_url || "",
    };
  }
  mailS.loaded = true;
}
async function saveMail() {
  mailS.saving = true; mailS.error = "";
  try {
    const f = mailS.form;
    const body: Record<string, unknown> = {
      enabled: f.enabled, smtp_host: f.smtp_host, smtp_port: Number(f.smtp_port),
      smtp_username: f.smtp_username, from: f.from, public_base_url: f.public_base_url,
    };
    if (f.smtp_password) body.smtp_password = f.smtp_password;
    const res = await api.PUT("/api/v1/settings/mail", { body: body as never });
    if (res.error) { mailS.error = (res.error as { error?: string })?.error || "Could not save."; return; }
    const d = res.data;
    mailS.passwordSet = d?.smtp_password_set ?? mailS.passwordSet;
    mailS.deliverable = d?.deliverable ?? false;
    mailS.form.smtp_password = "";
    flash(mailS);
  } finally { mailS.saving = false; }
}

const roles: { key: Role; label: string }[] = [
  { key: "org_admin", label: "Org Admin" },
  { key: "project_admin", label: "Project Admin" },
  { key: "editor", label: "Editor" },
  { key: "viewer", label: "Viewer" },
];
const roleLabel = (r?: Role) => roles.find((x) => x.key === r)?.label ?? r ?? "—";
const projectName = (id?: string | null) =>
  id ? (ws.projects.find((p) => p.id === id)?.name ?? "project") : "org-wide";

// ── Notification channels (project-scoped) ────────────────────────────────
const channels = ref<Channel[]>([]);
const channelsError = ref("");
const channelTypes: { key: ChannelType; label: string }[] = [
  { key: "webhook", label: "Webhook" },
  { key: "slack", label: "Slack" },
  { key: "telegram", label: "Telegram" },
  { key: "email", label: "Email" },
];
// Config fields per channel type (mirrors internal/domain/notification.go Validate).
const channelFields: Record<ChannelType, { key: string; label: string; placeholder?: string; optional?: boolean }[]> = {
  webhook: [{ key: "url", label: "URL", placeholder: "https://hooks.example/incoming" }],
  slack: [{ key: "url", label: "Slack webhook URL", placeholder: "https://hooks.slack.com/services/…" }],
  telegram: [
    { key: "bot_token", label: "Bot token" },
    { key: "chat_id", label: "Chat ID" },
  ],
  email: [
    { key: "to", label: "To (comma-separated)", placeholder: "oncall@x, team@x" },
    { key: "from", label: "From", placeholder: "cerbix@x" },
    { key: "smtp_host", label: "SMTP host" },
    { key: "smtp_port", label: "SMTP port", placeholder: "587", optional: true },
    { key: "smtp_username", label: "SMTP username", optional: true },
    { key: "smtp_password", label: "SMTP password", optional: true },
  ],
};
const showChannelAdd = ref(false);
const channelForm = reactive<{ type: ChannelType; name: string; config: Record<string, string> }>({
  type: "webhook",
  name: "",
  config: {},
});
const channelBusy = ref(false);
const channelFormError = ref("");

async function loadChannels() {
  channelsError.value = "";
  if (!ws.projectId) {
    channels.value = [];
    channelsError.value = "Select a project to manage its notification channels.";
    return;
  }
  const res = await api.GET("/api/v1/projects/{projectID}/notification-channels", {
    params: { path: { projectID: ws.projectId } },
  });
  if (res.error) {
    channelsError.value = "You need editor rights on this project to view channels.";
    channels.value = [];
    return;
  }
  channels.value = res.data ?? [];
}

async function addChannel() {
  if (!ws.projectId || channelBusy.value) return;
  channelBusy.value = true;
  channelFormError.value = "";
  const config: Record<string, string> = {};
  for (const f of channelFields[channelForm.type]) {
    const v = (channelForm.config[f.key] ?? "").trim();
    if (v) config[f.key] = v;
  }
  const body: components["schemas"]["CreateNotificationChannel"] = {
    type: channelForm.type,
    name: channelForm.name.trim(),
    config,
    enabled: true,
  };
  try {
    const res = await api.POST("/api/v1/projects/{projectID}/notification-channels", {
      params: { path: { projectID: ws.projectId } },
      body,
    });
    if (res.error || !res.data) {
      channelFormError.value = (res.error as { error?: string })?.error || "Could not create the channel.";
      return;
    }
    channels.value.push(res.data);
    channelForm.name = "";
    channelForm.config = {};
    showChannelAdd.value = false;
  } finally {
    channelBusy.value = false;
  }
}

async function deleteChannel(c: Channel) {
  if (!c.id || !confirm(`Delete channel "${c.name}"? Monitors linked to it stop notifying here.`)) return;
  const res = await api.DELETE("/api/v1/notification-channels/{channelID}", {
    params: { path: { channelID: c.id } },
  });
  if (!res.error) channels.value = channels.value.filter((x) => x.id !== c.id);
}

// ── API tokens (org-scoped) ───────────────────────────────────────────────
const tokens = ref<ApiToken[]>([]);
const tokensError = ref("");
const showTokenAdd = ref(false);
const tokenForm = reactive<{ name: string; role: Role; project_id: string }>({ name: "", role: "viewer", project_id: "" });
const tokenBusy = ref(false);
const tokenFormError = ref("");
const revealedSecret = ref<{ kind: string; value: string } | null>(null);

async function loadTokens() {
  tokensError.value = "";
  if (!ws.orgId) return;
  const res = await api.GET("/api/v1/organizations/{orgID}/tokens", {
    params: { path: { orgID: ws.orgId } },
  });
  if (res.error) {
    tokensError.value = "You need org-admin rights to manage API tokens.";
    tokens.value = [];
    return;
  }
  tokens.value = res.data ?? [];
}

async function addToken() {
  if (!ws.orgId || !tokenForm.name.trim() || tokenBusy.value) return;
  tokenBusy.value = true;
  tokenFormError.value = "";
  const body: components["schemas"]["CreateApiToken"] = { name: tokenForm.name.trim(), role: tokenForm.role };
  if (tokenForm.project_id) body.project_id = tokenForm.project_id;
  try {
    const res = await api.POST("/api/v1/organizations/{orgID}/tokens", {
      params: { path: { orgID: ws.orgId } },
      body,
    });
    if (res.error || !res.data) {
      tokenFormError.value = (res.error as { error?: string })?.error || "Could not create the token.";
      return;
    }
    if (res.data.api_token) tokens.value.unshift(res.data.api_token);
    if (res.data.token) revealedSecret.value = { kind: "API token", value: res.data.token };
    tokenForm.name = "";
    tokenForm.project_id = "";
    tokenForm.role = "viewer";
    showTokenAdd.value = false;
  } finally {
    tokenBusy.value = false;
  }
}

async function deleteToken(t: ApiToken) {
  if (!t.id || !confirm(`Revoke token "${t.name}"? Callers using it lose access immediately.`)) return;
  const res = await api.DELETE("/api/v1/tokens/{tokenID}", { params: { path: { tokenID: t.id } } });
  if (!res.error) tokens.value = tokens.value.filter((x) => x.id !== t.id);
}

// ── Webhooks (org-scoped) ─────────────────────────────────────────────────
const webhooks = ref<Webhook[]>([]);
const webhooksError = ref("");
const showWebhookAdd = ref(false);
const webhookForm = reactive<{ url: string; project_id: string }>({ url: "", project_id: "" });
const webhookBusy = ref(false);
const webhookFormError = ref("");

async function loadWebhooks() {
  webhooksError.value = "";
  if (!ws.orgId) return;
  const res = await api.GET("/api/v1/organizations/{orgID}/webhooks", {
    params: { path: { orgID: ws.orgId } },
  });
  if (res.error) {
    webhooksError.value = "You need org-admin rights to manage webhooks.";
    webhooks.value = [];
    return;
  }
  webhooks.value = res.data ?? [];
}

async function addWebhook() {
  if (!ws.orgId || !webhookForm.url.trim() || webhookBusy.value) return;
  webhookBusy.value = true;
  webhookFormError.value = "";
  const body: components["schemas"]["CreateWebhook"] = { url: webhookForm.url.trim(), enabled: true };
  if (webhookForm.project_id) body.project_id = webhookForm.project_id;
  try {
    const res = await api.POST("/api/v1/organizations/{orgID}/webhooks", {
      params: { path: { orgID: ws.orgId } },
      body,
    });
    if (res.error || !res.data) {
      webhookFormError.value = (res.error as { error?: string })?.error || "Could not create the webhook.";
      return;
    }
    webhooks.value.unshift(res.data);
    if (res.data.secret) revealedSecret.value = { kind: "Webhook signing secret", value: res.data.secret };
    webhookForm.url = "";
    webhookForm.project_id = "";
    showWebhookAdd.value = false;
  } finally {
    webhookBusy.value = false;
  }
}

async function deleteWebhook(h: Webhook) {
  if (!h.id || !confirm(`Delete webhook to ${h.url}?`)) return;
  const res = await api.DELETE("/api/v1/webhooks/{webhookID}", { params: { path: { webhookID: h.id } } });
  if (!res.error) webhooks.value = webhooks.value.filter((x) => x.id !== h.id);
}

// ── Two-factor auth (account-scoped, local users only) ────────────────────
type TOTPStep = "idle" | "enrolling" | "confirming";
const totpStep = ref<TOTPStep>("idle");
const totpSecret = ref("");
const totpUri = ref("");
const totpCode = ref("");
const totpBusy = ref(false);
const totpError = ref("");
const recoveryCodes = ref<string[]>([]);
const disablePassword = ref("");
const showDisable = ref(false);
const isLocalAccount = computed(() => !session.user?.oidc_sub);

// ── Change password (local accounts) ──────────────────────────────────────
const pw = reactive({ current: "", next: "", confirm: "" });
const pwBusy = ref(false);
const pwError = ref("");
const pwDone = ref(false);
async function changePassword() {
  pwError.value = "";
  pwDone.value = false;
  if (pw.next.length < 8) {
    pwError.value = "New password must be at least 8 characters.";
    return;
  }
  if (pw.next !== pw.confirm) {
    pwError.value = "New password and confirmation don't match.";
    return;
  }
  pwBusy.value = true;
  try {
    const res = await api.POST("/api/v1/me/password", {
      body: { current_password: pw.current, new_password: pw.next },
    });
    if (res.error) {
      pwError.value = (res.error as { error?: string })?.error || "Could not change the password.";
      return;
    }
    pw.current = pw.next = pw.confirm = "";
    pwDone.value = true;
  } catch {
    pwError.value = "Could not change the password.";
  } finally {
    pwBusy.value = false;
  }
}

// otpauth:// URI encoded as a QR the authenticator app can scan (rendered via a
// data: image built from a tiny inline generator would be heavy; we show the URI
// and the manual secret, which every TOTP app accepts).
async function startEnroll() {
  totpBusy.value = true;
  totpError.value = "";
  try {
    const res = await api.POST("/api/v1/me/totp/enroll");
    if (res.error || !res.data) {
      totpError.value = (res.error as { error?: string })?.error || "Could not start enrollment.";
      return;
    }
    totpSecret.value = res.data.secret ?? "";
    totpUri.value = res.data.uri ?? "";
    totpCode.value = "";
    totpStep.value = "confirming";
  } finally {
    totpBusy.value = false;
  }
}

async function confirmEnable() {
  if (!totpCode.value.trim() || totpBusy.value) return;
  totpBusy.value = true;
  totpError.value = "";
  try {
    const res = await api.POST("/api/v1/me/totp/enable", { body: { code: totpCode.value.trim() } });
    if (res.error || !res.data) {
      totpError.value = (res.error as { error?: string })?.error || "That code didn't match.";
      return;
    }
    recoveryCodes.value = res.data.recovery_codes ?? [];
    session.totpEnabled = true;
    totpStep.value = "idle";
    totpSecret.value = "";
    totpUri.value = "";
    totpCode.value = "";
  } finally {
    totpBusy.value = false;
  }
}

function cancelEnroll() {
  totpStep.value = "idle";
  totpSecret.value = "";
  totpUri.value = "";
  totpCode.value = "";
  totpError.value = "";
}

async function disableTotp() {
  if (!disablePassword.value || totpBusy.value) return;
  totpBusy.value = true;
  totpError.value = "";
  try {
    const res = await api.POST("/api/v1/me/totp/disable", { body: { password: disablePassword.value } });
    if (res.error) {
      totpError.value = (res.error as { error?: string })?.error || "Could not disable two-factor.";
      return;
    }
    session.totpEnabled = false;
    showDisable.value = false;
    disablePassword.value = "";
    recoveryCodes.value = [];
  } finally {
    totpBusy.value = false;
  }
}

// ── loading ───────────────────────────────────────────────────────────────
const loading = ref(true);

async function loadActive() {
  revealedSecret.value = null;
  if (tab.value === "channels") await loadChannels();
  else if (tab.value === "authentication") await Promise.all([loadOIDC(), loadAuthPolicy()]);
  else if (tab.value === "branding") await loadBranding();
  else if (tab.value === "alerting") await loadAlerting();
  else if (tab.value === "monitordefaults") await loadMonDefaults();
  else if (tab.value === "email") await loadMail();
  else if (tab.value === "tokens") await loadTokens();
  else if (tab.value === "webhooks") await loadWebhooks();
  // "security" reads from the session store; "members" is a self-loading panel.
}

async function load() {
  loading.value = true;
  try {
    await ws.init();
    await loadActive();
  } finally {
    loading.value = false;
  }
}

const activeError = computed(() =>
  tab.value === "channels"
    ? channelsError.value
    : tab.value === "tokens"
      ? tokensError.value
      : tab.value === "webhooks"
        ? webhooksError.value
        : "",
);

async function copy(v: string) {
  try {
    await navigator.clipboard.writeText(v);
  } catch {
    /* clipboard blocked; the value is shown for manual copy */
  }
}

onMounted(load);
watch(() => ws.orgId, load);
watch(() => ws.projectId, () => tab.value === "channels" && loadChannels());
watch(tab, loadActive);
</script>

<template>
  <AppShell active="settings" :crumbs="[ws.orgName || 'cerbix', 'Settings']">
    <div class="mx-auto max-w-[1180px] px-[22px] pb-16 pt-[26px]">
      <div class="mb-5">
        <h1 class="text-[21px] font-semibold tracking-tight">Settings</h1>
        <p class="mt-[3px] text-[13px] text-ink-3">Project, organization and instance configuration.</p>
      </div>

      <div class="grid grid-cols-[210px_1fr] gap-7 max-[820px]:grid-cols-1 max-[820px]:gap-4">
        <!-- grouped settings nav -->
        <nav class="flex flex-col gap-[18px] self-start max-[820px]:sticky max-[820px]:top-0">
          <div v-for="g in tabGroups" :key="g.scope">
            <div class="mb-[5px] px-[10px] text-[10.5px] font-semibold uppercase tracking-[0.07em] text-ink-3">{{ g.label }}</div>
            <div class="flex flex-col gap-[2px]">
              <button
                v-for="t in g.items"
                :key="t.key"
                type="button"
                class="rounded-[6px] px-[10px] py-[7px] text-left text-[13px] transition-colors"
                :class="tab === t.key ? 'bg-accent-weak font-medium text-accent' : 'text-ink-2 hover:bg-surface-2 hover:text-ink'"
                @click="tab = t.key"
              >{{ t.label }}</button>
            </div>
          </div>
        </nav>

        <!-- content column -->
        <div class="min-w-0">

      <!-- once-shown secret banner -->
      <div v-if="revealedSecret" class="mb-4 rounded border border-accent/40 bg-accent-weak p-4">
        <div class="text-[12px] font-semibold text-accent">{{ revealedSecret.kind }} — shown once, copy it now</div>
        <div class="mt-2 flex items-center gap-2">
          <code class="min-w-0 flex-1 truncate rounded-sm border border-border bg-surface px-3 py-2 font-mono text-[12.5px]">{{ revealedSecret.value }}</code>
          <button type="button" class="h-[34px] rounded-sm border border-border px-3 text-[13px] text-ink-2 hover:border-border-strong" @click="copy(revealedSecret.value)">Copy</button>
          <button type="button" class="h-[34px] rounded-sm border border-border px-3 text-[13px] text-ink-2 hover:border-border-strong" @click="revealedSecret = null">Dismiss</button>
        </div>
      </div>

      <div v-if="activeError" class="mb-5 rounded border border-border bg-surface p-4 text-[13px] text-ink-3 shadow-card">{{ activeError }}</div>

      <!-- ── Channels ── -->
      <template v-if="tab === 'channels' && !activeError">
        <div class="mb-3 flex items-center justify-between">
          <div class="text-[13px] text-ink-3">{{ ws.projectName }} · {{ channels.length }} channel(s)</div>
          <button v-if="canWriteChannels" type="button" class="h-[32px] rounded-sm bg-accent px-[13px] text-[13px] font-medium text-accent-ink hover:bg-accent-2" @click="showChannelAdd = !showChannelAdd">Add channel</button>
        </div>
        <div v-if="showChannelAdd && canWriteChannels" class="mb-5 flex flex-col gap-3 rounded border border-border bg-surface p-4 shadow-card">
          <div class="grid grid-cols-[200px_1fr] gap-3 max-[720px]:grid-cols-1">
            <label class="flex flex-col gap-[6px]">
              <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Type</span>
              <select v-model="channelForm.type" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent">
                <option v-for="ct in channelTypes" :key="ct.key" :value="ct.key">{{ ct.label }}</option>
              </select>
            </label>
            <label class="flex flex-col gap-[6px]">
              <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Name</span>
              <input v-model="channelForm.name" type="text" placeholder="e.g. oncall-slack" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent" />
            </label>
          </div>
          <div class="grid grid-cols-2 gap-3 max-[720px]:grid-cols-1">
            <label v-for="f in channelFields[channelForm.type]" :key="f.key" class="flex flex-col gap-[6px]">
              <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">{{ f.label }}<span v-if="f.optional" class="ml-1 normal-case text-ink-3/70">(optional)</span></span>
              <input v-model="channelForm.config[f.key]" type="text" :placeholder="f.placeholder" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent" />
            </label>
          </div>
          <div v-if="channelFormError" class="text-[12.5px] text-down">{{ channelFormError }}</div>
          <div class="flex items-center gap-2">
            <button type="button" :disabled="!channelForm.name.trim() || channelBusy" class="h-[34px] rounded-sm bg-accent px-4 text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50" @click="addChannel">{{ channelBusy ? "Creating…" : "Create" }}</button>
            <button type="button" class="h-[34px] rounded-sm border border-border px-4 text-[13px] text-ink-2 hover:border-border-strong" @click="showChannelAdd = false">Cancel</button>
          </div>
        </div>
        <section class="overflow-hidden rounded border border-border bg-surface shadow-card">
          <table class="w-full text-[13px]">
            <thead>
              <tr class="text-[10.5px] uppercase tracking-[0.06em] text-ink-3">
                <th class="border-b border-border px-4 py-[10px] text-left">Name</th>
                <th class="border-b border-border px-4 py-[10px] text-left">Type</th>
                <th class="border-b border-border px-4 py-[10px] text-left">Status</th>
                <th class="border-b border-border px-4 py-[10px]"></th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="c in channels" :key="c.id" class="hover:bg-surface-2">
                <td class="border-b border-border px-4 py-[11px] font-medium">{{ c.name }}</td>
                <td class="border-b border-border px-4 py-[11px] text-ink-2">{{ c.type }}</td>
                <td class="border-b border-border px-4 py-[11px]"><span class="text-ink-2">{{ c.enabled ? "enabled" : "disabled" }}</span></td>
                <td class="border-b border-border px-4 py-[11px] text-right"><button v-if="canWriteChannels" type="button" class="text-[12.5px] text-down hover:underline" @click="deleteChannel(c)">Delete</button></td>
              </tr>
              <tr v-if="!channels.length && !loading"><td colspan="4" class="px-4 py-10 text-center text-[13px] text-ink-3">No channels yet. Link one to a monitor from its detail page.</td></tr>
            </tbody>
          </table>
        </section>
      </template>

      <!-- ── Incoming alerts (Alertmanager receiver) ── -->
      <template v-else-if="tab === 'incoming' && !activeError">
        <div class="mb-3 text-[13px] text-ink-3">{{ ws.projectName }} · Alertmanager receiver</div>
        <section class="flex flex-col gap-4 rounded border border-border bg-surface p-[18px] shadow-card">
          <div>
            <h2 class="text-[14px] font-semibold">Prometheus Alertmanager</h2>
            <p class="mt-[3px] text-[13px] leading-[1.5] text-ink-3">
              Point an Alertmanager <code class="rounded-xs bg-inset px-[4px] py-px font-mono text-[12px]">webhook_config</code> at the URL below.
              A <b class="text-ink-2">firing</b> alert opens an incident (idempotent per fingerprint); a <b class="text-ink-2">resolved</b> alert closes it.
            </p>
          </div>

          <div>
            <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Receiver URL</span>
            <div class="mt-[6px] flex gap-2 max-[560px]:flex-col">
              <input :value="receiverUrl" readonly class="flex-1 rounded-sm border border-border bg-surface-2 px-3 py-2 font-mono text-[12.5px] text-ink-2 outline-none" @focus="($event.target as HTMLInputElement).select()" />
              <button type="button" class="h-[38px] shrink-0 rounded-sm border border-border px-4 text-[13px] text-ink-2 hover:border-border-strong" @click="copy(receiverUrl)">Copy</button>
            </div>
          </div>

          <div class="rounded border border-border bg-inset/40 p-3">
            <div class="mb-[6px] text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Authentication</div>
            <p class="text-[13px] leading-[1.5] text-ink-3">
              The receiver requires a bearer <b class="text-ink-2">service-account token</b> with Editor rights.
              Issue one under
              <button type="button" class="font-medium text-accent hover:underline" @click="tab = 'tokens'">API tokens</button>,
              then send it in Alertmanager's <code class="rounded-xs bg-inset px-[4px] py-px font-mono text-[12px]">http_config</code>.
            </p>
          </div>

          <div>
            <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Alertmanager config</span>
            <pre class="mt-[6px] overflow-x-auto rounded-sm border border-border bg-surface-2 p-3 font-mono text-[12px] leading-[1.55] text-ink-2"><code>receivers:
  - name: cerbix
    webhook_configs:
      - url: {{ receiverUrl || 'https://cerbix.example/api/v1/projects/&lt;id&gt;/alerts/alertmanager' }}
        http_config:
          authorization:
            type: Bearer
            credentials: &lt;service-account-token&gt;</code></pre>
          </div>
        </section>
      </template>

      <!-- ── Authentication (instance-wide OIDC) ── -->
      <template v-else-if="tab === 'authentication'">
        <div v-if="oidc.error" class="rounded border border-down/40 bg-down-weak p-4 text-[13px] text-down">{{ oidc.error }}</div>
        <section v-else-if="oidc.loaded" class="flex flex-col gap-4 rounded border border-border bg-surface p-[18px] shadow-card">
          <div>
            <h2 class="text-[14px] font-semibold">Single sign-on (OIDC)</h2>
            <p class="mt-[3px] text-[13px] leading-[1.5] text-ink-3">
              Instance-wide. Saving here <b class="text-ink-2">replaces the config file's</b> <code class="rounded-xs bg-inset px-[4px] py-px font-mono text-[12px]">oidc:</code> block. Local login stays available as a fallback.
            </p>
          </div>

          <!-- status banner -->
          <div
            class="flex items-center gap-[10px] rounded-[9px] border border-transparent px-[13px] py-[10px] text-[13px]"
            :class="oidc.active ? 'bg-up-weak text-ink-2' : oidc.form.enabled ? 'bg-degraded-weak text-ink-2' : 'bg-inset text-ink-3'"
          >
            <span
              class="rounded-[5px] border border-border bg-surface px-[8px] py-[2px] font-mono text-[11px] font-semibold"
              :class="oidc.active ? 'text-up' : oidc.form.enabled ? 'text-degraded' : 'text-ink-3'"
            >{{ oidc.active ? "ACTIVE" : oidc.form.enabled ? "PENDING" : "OFF" }}</span>
            <span v-if="oidc.active">Provider is live — the SSO button shows on the login page.</span>
            <span v-else-if="oidc.reloadError">Saved, but the provider could not be built: <span class="font-mono text-[12px] text-down">{{ oidc.reloadError }}</span> — retrying.</span>
            <span v-else-if="oidc.form.enabled">Enabled — save to build the provider.</span>
            <span v-else>SSO is off. {{ oidc.configured ? "" : "Currently using the config-file bootstrap (if any)." }}</span>
          </div>

          <label class="grid grid-cols-[210px_1fr] items-center gap-4 border-b border-border py-[12px] max-[640px]:grid-cols-1 max-[640px]:gap-2">
            <span class="text-[12.5px] font-semibold text-ink">Enable SSO<span class="mt-[3px] block text-[11.5px] font-normal text-ink-3">Show the OIDC button on the login page.</span></span>
            <span class="inline-flex cursor-pointer items-center gap-[10px]">
              <input v-model="oidc.form.enabled" type="checkbox" class="h-[16px] w-[16px] accent-accent" />
              <span class="text-[13px] text-ink-2">{{ oidc.form.enabled ? "Enabled" : "Disabled" }}</span>
            </span>
          </label>

          <label class="grid grid-cols-[210px_1fr] items-start gap-4 border-b border-border py-[12px] max-[640px]:grid-cols-1 max-[640px]:gap-2">
            <span class="pt-[8px] text-[12.5px] font-semibold text-ink">Button label</span>
            <input v-model="oidc.form.button_label" type="text" placeholder="Continue with Keycloak" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent" />
          </label>

          <label class="grid grid-cols-[210px_1fr] items-start gap-4 border-b border-border py-[12px] max-[640px]:grid-cols-1 max-[640px]:gap-2">
            <span class="pt-[8px] text-[12.5px] font-semibold text-ink">Issuer URL<span class="mt-[3px] block text-[11.5px] font-normal text-ink-3">Base provider URL (before /.well-known).</span></span>
            <input v-model="oidc.form.issuer" type="text" placeholder="https://keycloak.example/realms/team" class="rounded-sm border border-border bg-surface-2 px-3 py-2 font-mono text-[12.5px] outline-none focus:border-accent" />
          </label>

          <label class="grid grid-cols-[210px_1fr] items-start gap-4 border-b border-border py-[12px] max-[640px]:grid-cols-1 max-[640px]:gap-2">
            <span class="pt-[8px] text-[12.5px] font-semibold text-ink">Client ID</span>
            <input v-model="oidc.form.client_id" type="text" placeholder="cerbix" class="rounded-sm border border-border bg-surface-2 px-3 py-2 font-mono text-[12.5px] outline-none focus:border-accent" />
          </label>

          <label class="grid grid-cols-[210px_1fr] items-start gap-4 border-b border-border py-[12px] max-[640px]:grid-cols-1 max-[640px]:gap-2">
            <span class="pt-[8px] text-[12.5px] font-semibold text-ink">Client secret<span class="mt-[3px] block text-[11.5px] font-normal text-ink-3">Stored encrypted. Leave blank to keep the current one.</span></span>
            <div>
              <input v-model="oidc.form.client_secret" type="password" :placeholder="oidc.clientSecretSet ? '•••••••• (unchanged)' : 'client secret'" autocomplete="new-password" class="w-full rounded-sm border border-border bg-surface-2 px-3 py-2 font-mono text-[12.5px] outline-none focus:border-accent" />
              <div v-if="oidc.clientSecretSet" class="mt-[6px] text-[12px] text-up">✓ A secret is stored</div>
            </div>
          </label>

          <div class="grid grid-cols-[210px_1fr] items-start gap-4 border-b border-border py-[12px] max-[640px]:grid-cols-1 max-[640px]:gap-2">
            <span class="pt-[8px] text-[12.5px] font-semibold text-ink">Redirect URL<span class="mt-[3px] block text-[11.5px] font-normal text-ink-3">Register this in the provider's client.</span></span>
            <div class="flex gap-2 max-[560px]:flex-col">
              <input v-model="oidc.form.redirect_url" type="text" class="flex-1 rounded-sm border border-border bg-surface-2 px-3 py-2 font-mono text-[12.5px] outline-none focus:border-accent" />
              <button type="button" class="h-[38px] shrink-0 rounded-sm border border-border px-4 text-[13px] text-ink-2 hover:border-border-strong" @click="copy(oidc.form.redirect_url)">Copy</button>
            </div>
          </div>

          <div class="grid grid-cols-[210px_1fr] items-start gap-4 border-b border-border py-[12px] max-[640px]:grid-cols-1 max-[640px]:gap-2">
            <span class="pt-[8px] text-[12.5px] font-semibold text-ink">Scopes</span>
            <div class="flex flex-wrap items-center gap-[6px]">
              <span v-for="(s, i) in oidc.form.scopes" :key="s" class="inline-flex items-center gap-[6px] rounded-md bg-inset px-[9px] py-[4px] font-mono text-[11.5px] text-ink-2">{{ s }}<button type="button" class="text-ink-3 hover:text-down" @click="removeChip(oidc.form.scopes, i)">×</button></span>
              <input v-model="oidc.scopeInput" type="text" placeholder="+ scope" class="w-[110px] rounded-sm border border-dashed border-border-strong bg-transparent px-[8px] py-[3px] font-mono text-[11.5px] outline-none focus:border-accent" @keydown.enter.prevent="addChip(oidc.form.scopes, oidc.scopeInput); oidc.scopeInput = ''" />
            </div>
          </div>

          <div class="grid grid-cols-[210px_1fr] items-start gap-4 border-b border-border py-[12px] max-[640px]:grid-cols-1 max-[640px]:gap-2">
            <span class="pt-[8px] text-[12.5px] font-semibold text-ink">Bootstrap admin emails<span class="mt-[3px] block text-[11.5px] font-normal text-ink-3">Promoted to global-admin on first login.</span></span>
            <div class="flex flex-wrap items-center gap-[6px]">
              <span v-for="(e, i) in oidc.form.bootstrap_admins" :key="e" class="inline-flex items-center gap-[6px] rounded-md bg-inset px-[9px] py-[4px] font-mono text-[11.5px] text-ink-2">{{ e }}<button type="button" class="text-ink-3 hover:text-down" @click="removeChip(oidc.form.bootstrap_admins, i)">×</button></span>
              <input v-model="oidc.adminInput" type="text" placeholder="+ email" class="w-[150px] rounded-sm border border-dashed border-border-strong bg-transparent px-[8px] py-[3px] font-mono text-[11.5px] outline-none focus:border-accent" @keydown.enter.prevent="addChip(oidc.form.bootstrap_admins, oidc.adminInput); oidc.adminInput = ''" />
            </div>
          </div>

          <label class="grid grid-cols-[210px_1fr] items-start gap-4 py-[12px] max-[640px]:grid-cols-1 max-[640px]:gap-2">
            <span class="pt-[8px] text-[12.5px] font-semibold text-ink">Post-logout redirect<span class="mt-[3px] block text-[11.5px] font-normal text-ink-3">Optional.</span></span>
            <input v-model="oidc.form.post_logout_url" type="text" placeholder="https://cerbix.example/login" class="rounded-sm border border-border bg-surface-2 px-3 py-2 font-mono text-[12.5px] outline-none focus:border-accent" />
          </label>

          <div class="flex items-center gap-3 border-t border-border pt-[16px]">
            <button type="button" :disabled="oidc.saving" class="h-[36px] rounded-sm bg-accent px-[16px] text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50" @click="saveOIDC">{{ oidc.saving ? "Saving…" : "Save & apply" }}</button>
            <span class="text-[12.5px] text-ink-3">Saving rebuilds the provider immediately.</span>
          </div>
        </section>
        <div v-else class="text-[13px] text-ink-3">Loading…</div>

        <!-- auth policy -->
        <section v-if="authPolicy.loaded" class="mt-5 flex flex-col gap-4 rounded border border-border bg-surface p-[18px] shadow-card">
          <h2 class="text-[14px] font-semibold">Login policy</h2>
          <div class="grid grid-cols-2 gap-4 max-[560px]:grid-cols-1">
            <label class="flex flex-col gap-[6px]"><span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Min password length</span>
              <input v-model.number="authPolicy.form.min_password_len" type="number" min="6" max="256" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent" /></label>
            <label class="flex flex-col gap-[6px]"><span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Session TTL (seconds)</span>
              <input v-model.number="authPolicy.form.session_ttl_seconds" type="number" min="300" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent" /></label>
          </div>
          <label class="flex flex-col gap-[6px]"><span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Require two-factor</span>
            <select v-model="authPolicy.form.require_totp" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent max-w-[240px]">
              <option value="none">Not required</option><option value="admins">Global admins</option><option value="all">Everyone</option>
            </select>
            <span v-if="authPolicy.form.require_totp !== 'none'" class="text-[11.5px] text-degraded">⚠ Users without 2FA set up will be unable to sign in locally.</span>
          </label>
          <div class="flex flex-col gap-[6px]">
            <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Allowed email domains<span class="ml-1 font-normal normal-case text-ink-3">(empty = any)</span></span>
            <div class="flex flex-wrap items-center gap-[6px]">
              <span v-for="(d, i) in authPolicy.form.allowed_email_domains" :key="d" class="inline-flex items-center gap-[6px] rounded-md bg-inset px-[9px] py-[4px] font-mono text-[11.5px] text-ink-2">{{ d }}<button type="button" class="text-ink-3 hover:text-down" @click="removeChip(authPolicy.form.allowed_email_domains, i)">×</button></span>
              <input v-model="authPolicy.domainInput" type="text" placeholder="+ example.com" class="w-[150px] rounded-sm border border-dashed border-border-strong bg-transparent px-[8px] py-[3px] font-mono text-[11.5px] outline-none focus:border-accent" @keydown.enter.prevent="addChip(authPolicy.form.allowed_email_domains, authPolicy.domainInput); authPolicy.domainInput = ''" />
            </div>
          </div>
          <div class="flex items-center gap-3 border-t border-border pt-[14px]">
            <button type="button" :disabled="authPolicy.saving" class="h-[36px] rounded-sm bg-accent px-[16px] text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50" @click="saveAuthPolicy">{{ authPolicy.saving ? "Saving…" : "Save policy" }}</button>
            <span v-if="authPolicy.saved" class="text-[12.5px] text-up">✓ Saved</span>
            <span v-if="authPolicy.error" class="text-[12.5px] text-down">{{ authPolicy.error }}</span>
          </div>
        </section>
      </template>

      <!-- ── Branding ── -->
      <template v-else-if="tab === 'branding'">
        <section v-if="brand.loaded" class="flex flex-col gap-4 rounded border border-border bg-surface p-[18px] shadow-card">
          <h2 class="text-[14px] font-semibold">Branding</h2>
          <div class="grid grid-cols-2 gap-4 max-[560px]:grid-cols-1">
            <label class="flex flex-col gap-[6px]"><span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Product name</span>
              <input v-model="brand.form.product_name" type="text" placeholder="cerbix" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent" /></label>
            <label class="flex flex-col gap-[6px]"><span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Accent color</span>
              <div class="flex items-center gap-2">
                <input v-model="brand.form.accent_color" type="text" placeholder="#3b5bdb" class="w-[120px] rounded-sm border border-border bg-surface-2 px-3 py-2 font-mono text-[12.5px] outline-none focus:border-accent" />
                <span class="h-[26px] w-[26px] rounded-sm border border-border" :style="{ background: /^#[0-9a-fA-F]{6}$/.test(brand.form.accent_color) ? brand.form.accent_color : 'transparent' }"></span>
              </div></label>
          </div>
          <label class="flex flex-col gap-[6px]"><span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Support URL</span>
            <input v-model="brand.form.support_url" type="text" placeholder="https://…" class="rounded-sm border border-border bg-surface-2 px-3 py-2 font-mono text-[12.5px] outline-none focus:border-accent" /></label>
          <label class="flex flex-col gap-[6px]"><span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Footer text</span>
            <input v-model="brand.form.footer_text" type="text" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent" /></label>

          <div class="rounded border border-border bg-surface-2 p-3">
            <label class="mb-2 inline-flex cursor-pointer items-center gap-[8px] text-[13px]"><input v-model="brand.form.announcement.enabled" type="checkbox" class="h-[15px] w-[15px] accent-accent" />Show a global announcement banner</label>
            <div v-if="brand.form.announcement.enabled" class="flex flex-col gap-2">
              <input v-model="brand.form.announcement.text" type="text" placeholder="Scheduled maintenance tonight 22:00–23:00 UTC" class="rounded-sm border border-border bg-surface px-3 py-2 text-[13px] outline-none focus:border-accent" />
              <select v-model="brand.form.announcement.level" class="w-[160px] rounded-sm border border-border bg-surface px-3 py-2 text-[13px] outline-none focus:border-accent">
                <option value="info">Info</option><option value="warning">Warning</option><option value="critical">Critical</option>
              </select>
            </div>
          </div>
          <div class="flex items-center gap-3 border-t border-border pt-[14px]">
            <button type="button" :disabled="brand.saving" class="h-[36px] rounded-sm bg-accent px-[16px] text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50" @click="saveBranding">{{ brand.saving ? "Saving…" : "Save branding" }}</button>
            <span v-if="brand.saved" class="text-[12.5px] text-up">✓ Applied</span>
            <span v-if="brand.error" class="text-[12.5px] text-down">{{ brand.error }}</span>
          </div>
        </section>
        <div v-else class="text-[13px] text-ink-3">Loading…</div>
      </template>

      <!-- ── Alerting ── -->
      <template v-else-if="tab === 'alerting'">
        <section v-if="alerting.loaded" class="flex flex-col gap-4 rounded border border-border bg-surface p-[18px] shadow-card">
          <h2 class="text-[14px] font-semibold">Alerting</h2>
          <div class="flex items-start justify-between gap-4 rounded border border-border bg-surface-2 p-4">
            <div>
              <div class="text-[13.5px] font-semibold">Global silence</div>
              <p class="mt-[3px] text-[12.5px] leading-[1.5] text-ink-3">Suppress all monitor-down and burn-rate notifications instance-wide. Incidents are still recorded — only the outgoing alerts are held.</p>
            </div>
            <button type="button" :disabled="alerting.saving" class="inline-flex items-center gap-[9px]" @click="saveAlerting(!alerting.enabled)">
              <span class="h-[22px] w-[38px] rounded-full transition-colors" :class="alerting.enabled ? 'bg-down' : 'bg-border-strong'">
                <span class="block h-[18px] w-[18px] translate-y-[2px] rounded-full bg-white transition-transform" :class="alerting.enabled ? 'translate-x-[18px]' : 'translate-x-[2px]'"></span>
              </span>
            </button>
          </div>
          <div v-if="alerting.enabled" class="rounded bg-down-weak px-3 py-2 text-[12.5px] text-down">Alerts are currently silenced instance-wide.</div>
          <span v-if="alerting.saved" class="text-[12.5px] text-up">✓ Saved</span>
        </section>
        <div v-else class="text-[13px] text-ink-3">Loading…</div>
      </template>

      <!-- ── Monitor defaults ── -->
      <template v-else-if="tab === 'monitordefaults'">
        <section v-if="monDefaults.loaded" class="flex flex-col gap-4 rounded border border-border bg-surface p-[18px] shadow-card">
          <h2 class="text-[14px] font-semibold">Monitor defaults</h2>
          <p class="text-[12.5px] text-ink-3">Applied to newly-created monitors when a field is left unset.</p>
          <div class="grid grid-cols-3 gap-4 max-[560px]:grid-cols-1">
            <label class="flex flex-col gap-[6px]"><span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Interval (s)</span>
              <input v-model.number="monDefaults.form.interval_seconds" type="number" min="5" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent" /></label>
            <label class="flex flex-col gap-[6px]"><span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Timeout (s)</span>
              <input v-model.number="monDefaults.form.timeout_seconds" type="number" min="1" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent" /></label>
            <label class="flex flex-col gap-[6px]"><span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Retries</span>
              <input v-model.number="monDefaults.form.retries" type="number" min="0" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent" /></label>
            <label class="flex flex-col gap-[6px]"><span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Failure threshold</span>
              <input v-model.number="monDefaults.form.failure_threshold" type="number" min="1" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent" /></label>
            <label class="flex flex-col gap-[6px]"><span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Re-notify (s)</span>
              <input v-model.number="monDefaults.form.renotify_seconds" type="number" min="0" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent" /></label>
            <label class="flex flex-col justify-end gap-[6px] pb-2"><span class="inline-flex cursor-pointer items-center gap-[8px] text-[13px]"><input v-model="monDefaults.form.auto_incident" type="checkbox" class="h-[15px] w-[15px] accent-accent" />Auto-incidents on</span></label>
          </div>
          <div class="flex items-center gap-3 border-t border-border pt-[14px]">
            <button type="button" :disabled="monDefaults.saving" class="h-[36px] rounded-sm bg-accent px-[16px] text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50" @click="saveMonDefaults">{{ monDefaults.saving ? "Saving…" : "Save defaults" }}</button>
            <span v-if="monDefaults.saved" class="text-[12.5px] text-up">✓ Saved</span>
            <span v-if="monDefaults.error" class="text-[12.5px] text-down">{{ monDefaults.error }}</span>
          </div>
        </section>
        <div v-else class="text-[13px] text-ink-3">Loading…</div>
      </template>

      <!-- ── Email / SMTP ── -->
      <template v-else-if="tab === 'email'">
        <section v-if="mailS.loaded" class="flex flex-col gap-4 rounded border border-border bg-surface p-[18px] shadow-card">
          <div class="flex items-center justify-between gap-3">
            <h2 class="text-[14px] font-semibold">Email (SMTP)</h2>
            <span class="rounded-full px-[10px] py-[3px] text-[11.5px] font-medium" :class="mailS.deliverable ? 'bg-up-weak text-up' : 'bg-inset text-ink-3'">{{ mailS.deliverable ? "Deliverable" : "Not sending" }}</span>
          </div>
          <p class="text-[12.5px] leading-[1.5] text-ink-3">Used for password-reset links and status-page subscription email. Changes apply immediately — the mailer re-reads these per send.</p>

          <label class="inline-flex cursor-pointer items-center gap-[8px] text-[13px]"><input v-model="mailS.form.enabled" type="checkbox" class="h-[15px] w-[15px] accent-accent" />Enable outgoing email</label>

          <div class="grid grid-cols-[2fr_1fr] gap-4 max-[560px]:grid-cols-1">
            <label class="flex flex-col gap-[6px]"><span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">SMTP host</span>
              <input v-model="mailS.form.smtp_host" type="text" placeholder="smtp.example.com" class="rounded-sm border border-border bg-surface-2 px-3 py-2 font-mono text-[12.5px] outline-none focus:border-accent" /></label>
            <label class="flex flex-col gap-[6px]"><span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Port</span>
              <input v-model.number="mailS.form.smtp_port" type="number" min="1" max="65535" class="rounded-sm border border-border bg-surface-2 px-3 py-2 font-mono text-[12.5px] outline-none focus:border-accent" /></label>
          </div>
          <div class="grid grid-cols-2 gap-4 max-[560px]:grid-cols-1">
            <label class="flex flex-col gap-[6px]"><span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Username</span>
              <input v-model="mailS.form.smtp_username" type="text" autocomplete="off" class="rounded-sm border border-border bg-surface-2 px-3 py-2 font-mono text-[12.5px] outline-none focus:border-accent" /></label>
            <label class="flex flex-col gap-[6px]"><span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Password<span v-if="mailS.passwordSet" class="ml-1 font-normal normal-case text-up">✓ stored</span></span>
              <input v-model="mailS.form.smtp_password" type="password" autocomplete="new-password" :placeholder="mailS.passwordSet ? '•••••••• (unchanged)' : 'smtp password'" class="rounded-sm border border-border bg-surface-2 px-3 py-2 font-mono text-[12.5px] outline-none focus:border-accent" /></label>
          </div>
          <div class="grid grid-cols-2 gap-4 max-[560px]:grid-cols-1">
            <label class="flex flex-col gap-[6px]"><span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">From address</span>
              <input v-model="mailS.form.from" type="text" placeholder="status@example.com" class="rounded-sm border border-border bg-surface-2 px-3 py-2 font-mono text-[12.5px] outline-none focus:border-accent" /></label>
            <label class="flex flex-col gap-[6px]"><span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Public base URL<span class="ml-1 font-normal normal-case text-ink-3">(for links)</span></span>
              <input v-model="mailS.form.public_base_url" type="text" placeholder="https://cerbix.example" class="rounded-sm border border-border bg-surface-2 px-3 py-2 font-mono text-[12.5px] outline-none focus:border-accent" /></label>
          </div>

          <div class="flex items-center gap-3 border-t border-border pt-[14px]">
            <button type="button" :disabled="mailS.saving" class="h-[36px] rounded-sm bg-accent px-[16px] text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50" @click="saveMail">{{ mailS.saving ? "Saving…" : "Save email settings" }}</button>
            <span v-if="mailS.saved" class="text-[12.5px] text-up">✓ Saved</span>
            <span v-if="mailS.error" class="text-[12.5px] text-down">{{ mailS.error }}</span>
          </div>
        </section>
        <div v-else class="text-[13px] text-ink-3">Loading…</div>
      </template>

      <!-- ── Members (org-scoped, self-loading panel) ── -->
      <MembersPanel v-else-if="tab === 'members'" />

      <!-- ── Users (instance-wide, global admin, self-loading panel) ── -->
      <UsersPanel v-else-if="tab === 'users'" />

      <!-- ── Tokens ── -->
      <template v-else-if="tab === 'tokens' && !activeError">
        <div class="mb-3 flex items-center justify-between">
          <div class="text-[13px] text-ink-3">{{ ws.orgName }} · {{ tokens.length }} token(s)</div>
          <button v-if="canManageOrg" type="button" class="h-[32px] rounded-sm bg-accent px-[13px] text-[13px] font-medium text-accent-ink hover:bg-accent-2" @click="showTokenAdd = !showTokenAdd">Issue token</button>
        </div>
        <div v-if="showTokenAdd && canManageOrg" class="mb-5 flex flex-col gap-3 rounded border border-border bg-surface p-4 shadow-card">
          <div class="grid grid-cols-[1fr_180px_180px] gap-3 max-[720px]:grid-cols-1">
            <label class="flex flex-col gap-[6px]">
              <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Name</span>
              <input v-model="tokenForm.name" type="text" placeholder="e.g. ci-deploy-bot" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent" />
            </label>
            <label class="flex flex-col gap-[6px]">
              <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Scope</span>
              <select v-model="tokenForm.project_id" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent">
                <option value="">Org-wide</option>
                <option v-for="p in ws.projects" :key="p.id" :value="p.id">{{ p.name || p.slug }}</option>
              </select>
            </label>
            <label class="flex flex-col gap-[6px]">
              <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Role</span>
              <select v-model="tokenForm.role" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent">
                <option v-for="r in roles" :key="r.key" :value="r.key">{{ r.label }}</option>
              </select>
            </label>
          </div>
          <div v-if="tokenFormError" class="text-[12.5px] text-down">{{ tokenFormError }}</div>
          <div class="flex items-center gap-2">
            <button type="button" :disabled="!tokenForm.name.trim() || tokenBusy" class="h-[34px] rounded-sm bg-accent px-4 text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50" @click="addToken">{{ tokenBusy ? "Issuing…" : "Issue" }}</button>
            <button type="button" class="h-[34px] rounded-sm border border-border px-4 text-[13px] text-ink-2 hover:border-border-strong" @click="showTokenAdd = false">Cancel</button>
          </div>
        </div>
        <section class="overflow-hidden rounded border border-border bg-surface shadow-card">
          <table class="w-full text-[13px]">
            <thead>
              <tr class="text-[10.5px] uppercase tracking-[0.06em] text-ink-3">
                <th class="border-b border-border px-4 py-[10px] text-left">Name</th>
                <th class="border-b border-border px-4 py-[10px] text-left">Scope</th>
                <th class="border-b border-border px-4 py-[10px] text-left">Role</th>
                <th class="border-b border-border px-4 py-[10px] text-left">Last used</th>
                <th class="border-b border-border px-4 py-[10px]"></th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="t in tokens" :key="t.id" class="hover:bg-surface-2">
                <td class="border-b border-border px-4 py-[11px] font-medium">{{ t.name }}</td>
                <td class="border-b border-border px-4 py-[11px] text-ink-2">{{ projectName(t.project_id) }}</td>
                <td class="border-b border-border px-4 py-[11px]"><span class="rounded-full border border-border px-[9px] py-[2px] text-[11.5px] font-medium text-ink-2">{{ roleLabel(t.role) }}</span></td>
                <td class="border-b border-border px-4 py-[11px] text-ink-3">{{ t.last_used_at ? new Date(t.last_used_at).toLocaleString() : "never" }}</td>
                <td class="border-b border-border px-4 py-[11px] text-right"><button v-if="canManageOrg" type="button" class="text-[12.5px] text-down hover:underline" @click="deleteToken(t)">Revoke</button></td>
              </tr>
              <tr v-if="!tokens.length && !loading"><td colspan="5" class="px-4 py-10 text-center text-[13px] text-ink-3">No tokens yet.</td></tr>
            </tbody>
          </table>
        </section>
      </template>

      <!-- ── Webhooks ── -->
      <template v-else-if="tab === 'webhooks' && !activeError">
        <div class="mb-3 flex items-center justify-between">
          <div class="text-[13px] text-ink-3">{{ ws.orgName }} · {{ webhooks.length }} webhook(s)</div>
          <button v-if="canManageOrg" type="button" class="h-[32px] rounded-sm bg-accent px-[13px] text-[13px] font-medium text-accent-ink hover:bg-accent-2" @click="showWebhookAdd = !showWebhookAdd">Add webhook</button>
        </div>
        <div v-if="showWebhookAdd && canManageOrg" class="mb-5 flex flex-col gap-3 rounded border border-border bg-surface p-4 shadow-card">
          <div class="grid grid-cols-[1fr_220px] gap-3 max-[720px]:grid-cols-1">
            <label class="flex flex-col gap-[6px]">
              <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">URL</span>
              <input v-model="webhookForm.url" type="text" placeholder="https://receiver.example/cerbix" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent" />
            </label>
            <label class="flex flex-col gap-[6px]">
              <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Scope</span>
              <select v-model="webhookForm.project_id" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent">
                <option value="">Org-wide</option>
                <option v-for="p in ws.projects" :key="p.id" :value="p.id">{{ p.name || p.slug }}</option>
              </select>
            </label>
          </div>
          <p class="text-[12px] text-ink-3">A signing secret is generated and shown once; deliveries carry an <code class="font-mono">X-Cerbix-Signature</code> HMAC header.</p>
          <div v-if="webhookFormError" class="text-[12.5px] text-down">{{ webhookFormError }}</div>
          <div class="flex items-center gap-2">
            <button type="button" :disabled="!webhookForm.url.trim() || webhookBusy" class="h-[34px] rounded-sm bg-accent px-4 text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50" @click="addWebhook">{{ webhookBusy ? "Creating…" : "Create" }}</button>
            <button type="button" class="h-[34px] rounded-sm border border-border px-4 text-[13px] text-ink-2 hover:border-border-strong" @click="showWebhookAdd = false">Cancel</button>
          </div>
        </div>
        <section class="overflow-hidden rounded border border-border bg-surface shadow-card">
          <table class="w-full text-[13px]">
            <thead>
              <tr class="text-[10.5px] uppercase tracking-[0.06em] text-ink-3">
                <th class="border-b border-border px-4 py-[10px] text-left">URL</th>
                <th class="border-b border-border px-4 py-[10px] text-left">Scope</th>
                <th class="border-b border-border px-4 py-[10px] text-left">Status</th>
                <th class="border-b border-border px-4 py-[10px]"></th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="h in webhooks" :key="h.id" class="hover:bg-surface-2">
                <td class="border-b border-border px-4 py-[11px] font-mono text-[12px] text-ink-2">{{ h.url }}</td>
                <td class="border-b border-border px-4 py-[11px] text-ink-2">{{ projectName(h.project_id) }}</td>
                <td class="border-b border-border px-4 py-[11px] text-ink-2">{{ h.enabled ? "enabled" : "disabled" }}</td>
                <td class="border-b border-border px-4 py-[11px] text-right"><button v-if="canManageOrg" type="button" class="text-[12.5px] text-down hover:underline" @click="deleteWebhook(h)">Delete</button></td>
              </tr>
              <tr v-if="!webhooks.length && !loading"><td colspan="4" class="px-4 py-10 text-center text-[13px] text-ink-3">No webhooks yet.</td></tr>
            </tbody>
          </table>
        </section>
      </template>

      <!-- ── Security (2FA) ── -->
      <template v-else-if="tab === 'security'">
        <div class="mb-3 text-[13px] text-ink-3">Password and two-factor authentication for your account.</div>

        <!-- not a local account: password + 2FA are handled by the identity provider -->
        <section v-if="!isLocalAccount" class="rounded border border-border bg-surface p-5 text-[13px] text-ink-3 shadow-card">
          You sign in through your organization's identity provider. Your password and two-factor authentication are managed there, not in cerbix.
        </section>

        <template v-else>
          <!-- change password -->
          <section class="mb-4 rounded border border-border bg-surface p-5 shadow-card">
            <h2 class="text-[13px] font-semibold">Change password</h2>
            <p class="mt-[2px] text-[12px] text-ink-3">At least 8 characters. You'll stay signed in on this device.</p>
            <form class="mt-4 flex max-w-[360px] flex-col gap-3" @submit.prevent="changePassword">
              <label class="flex flex-col gap-[6px]">
                <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Current password</span>
                <input v-model="pw.current" type="password" autocomplete="current-password" class="h-10 rounded-sm border border-border bg-surface-2 px-3 text-[13px] outline-none focus:border-accent" />
              </label>
              <label class="flex flex-col gap-[6px]">
                <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">New password</span>
                <input v-model="pw.next" type="password" autocomplete="new-password" class="h-10 rounded-sm border border-border bg-surface-2 px-3 text-[13px] outline-none focus:border-accent" />
              </label>
              <label class="flex flex-col gap-[6px]">
                <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Confirm new password</span>
                <input v-model="pw.confirm" type="password" autocomplete="new-password" class="h-10 rounded-sm border border-border bg-surface-2 px-3 text-[13px] outline-none focus:border-accent" />
              </label>
              <div v-if="pwError" class="text-[12.5px] text-down">{{ pwError }}</div>
              <div v-if="pwDone" class="text-[12.5px] text-up">Password changed.</div>
              <div>
                <button type="submit" :disabled="!pw.current || !pw.next || pwBusy" class="h-[34px] rounded-sm bg-accent px-4 text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50">{{ pwBusy ? "Saving…" : "Update password" }}</button>
              </div>
            </form>
          </section>

          <!-- recovery codes shown once after enabling -->
          <div v-if="recoveryCodes.length" class="mb-4 rounded border border-accent/40 bg-accent-weak p-4">
            <div class="text-[12px] font-semibold text-accent">Recovery codes — shown once, store them safely</div>
            <p class="mt-1 text-[12px] text-ink-2">Each code works a single time if you lose your authenticator. They won't be shown again.</p>
            <div class="mt-3 grid grid-cols-2 gap-2 max-[520px]:grid-cols-1">
              <code v-for="c in recoveryCodes" :key="c" class="rounded-sm border border-border bg-surface px-3 py-[7px] text-center font-mono text-[13px] tracking-[0.15em]">{{ c }}</code>
            </div>
            <div class="mt-3 flex gap-2">
              <button type="button" class="h-[32px] rounded-sm border border-border px-3 text-[13px] text-ink-2 hover:border-border-strong" @click="copy(recoveryCodes.join('\n'))">Copy all</button>
              <button type="button" class="h-[32px] rounded-sm border border-border px-3 text-[13px] text-ink-2 hover:border-border-strong" @click="recoveryCodes = []">I've saved them</button>
            </div>
          </div>

          <section class="rounded border border-border bg-surface p-5 shadow-card">
            <!-- enabled state -->
            <template v-if="session.totpEnabled">
              <div class="flex items-center gap-2">
                <span class="inline-flex items-center gap-[6px] rounded-full border border-up/40 bg-up/10 px-[9px] py-[2px] text-[11.5px] font-medium text-up">
                  <span class="h-[7px] w-[7px] rounded-full bg-up"></span> Two-factor is on
                </span>
              </div>
              <p class="mt-3 text-[13px] text-ink-3">You'll be asked for a code from your authenticator app each time you sign in.</p>
              <div v-if="!showDisable" class="mt-4">
                <button type="button" class="h-[34px] rounded-sm border border-border px-4 text-[13px] text-down hover:border-down/50" @click="showDisable = true; totpError = ''">Disable two-factor</button>
              </div>
              <div v-else class="mt-4 flex max-w-[360px] flex-col gap-2">
                <label class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Confirm your password to disable</label>
                <input v-model="disablePassword" type="password" autocomplete="current-password" class="h-10 rounded-sm border border-border bg-surface-2 px-3 text-[13px] outline-none focus:border-accent" @keyup.enter="disableTotp" />
                <div v-if="totpError" class="text-[12.5px] text-down">{{ totpError }}</div>
                <div class="flex gap-2">
                  <button type="button" :disabled="!disablePassword || totpBusy" class="h-[34px] rounded-sm bg-down px-4 text-[13px] font-medium text-white hover:opacity-90 disabled:opacity-50" @click="disableTotp">{{ totpBusy ? "Disabling…" : "Disable" }}</button>
                  <button type="button" class="h-[34px] rounded-sm border border-border px-4 text-[13px] text-ink-2 hover:border-border-strong" @click="showDisable = false; disablePassword = ''">Cancel</button>
                </div>
              </div>
            </template>

            <!-- disabled → enroll flow -->
            <template v-else>
              <div v-if="totpStep === 'idle'">
                <p class="text-[13px] text-ink-3">Add a second step at sign-in using an authenticator app (Google Authenticator, 1Password, Aegis…).</p>
                <button type="button" :disabled="totpBusy" class="mt-4 h-[34px] rounded-sm bg-accent px-4 text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50" @click="startEnroll">{{ totpBusy ? "Starting…" : "Enable two-factor" }}</button>
              </div>

              <div v-else-if="totpStep === 'confirming'" class="flex max-w-[420px] flex-col gap-3">
                <div>
                  <div class="text-[12px] font-semibold text-ink-2">1 · Add this secret to your authenticator</div>
                  <p class="mt-1 text-[12px] text-ink-3">Scan the setup link or type the secret manually.</p>
                  <div class="mt-2 flex items-center gap-2">
                    <code class="min-w-0 flex-1 truncate rounded-sm border border-border bg-surface-2 px-3 py-2 font-mono text-[13px] tracking-[0.15em]">{{ totpSecret }}</code>
                    <button type="button" class="h-[34px] rounded-sm border border-border px-3 text-[13px] text-ink-2 hover:border-border-strong" @click="copy(totpSecret)">Copy</button>
                  </div>
                  <a :href="totpUri" class="mt-2 inline-block text-[12px] font-medium text-accent hover:underline">Open in authenticator app</a>
                </div>
                <div>
                  <div class="text-[12px] font-semibold text-ink-2">2 · Enter the current 6-digit code</div>
                  <input v-model="totpCode" inputmode="numeric" placeholder="123456" class="mt-2 h-10 w-[180px] rounded-sm border border-border bg-surface-2 px-3 font-mono text-[15px] tracking-[0.3em] outline-none focus:border-accent" @keyup.enter="confirmEnable" />
                </div>
                <div v-if="totpError" class="text-[12.5px] text-down">{{ totpError }}</div>
                <div class="flex gap-2">
                  <button type="button" :disabled="!totpCode.trim() || totpBusy" class="h-[34px] rounded-sm bg-accent px-4 text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50" @click="confirmEnable">{{ totpBusy ? "Verifying…" : "Verify & enable" }}</button>
                  <button type="button" class="h-[34px] rounded-sm border border-border px-4 text-[13px] text-ink-2 hover:border-border-strong" @click="cancelEnroll">Cancel</button>
                </div>
              </div>
            </template>
          </section>
        </template>
      </template>
        </div><!-- /content column -->
      </div><!-- /settings grid -->
    </div>
  </AppShell>
</template>
