<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import { api } from "@/api/client";
import type { components } from "@/api/schema";
import { applyCredentialSelection, isDanglingSecretRef } from "@/lib/monitorCredentials";
import {
  applyBindings,
  bindingPlaceholder,
  bindingsFromConfig,
  firstBindingIssue,
  isSecretCapableHeader,
  malformedRefKeys,
  MAX_SCENARIO_BINDINGS,
  testBeforeSaveBlockedReason,
  validateScenarioBindings,
  type ScenarioBinding,
} from "@/lib/scenarioBindings";
import {
  buildCanaryConfig,
  canaryRefusals,
  emptyCanaryForm,
  isCredentialHeader,
  parseCanaryConfig,
  CANARY_COMPLETION_KINDS,
  CANARY_CLEANUP_KINDS,
  CANARY_CORRELATE_SOURCES,
  CANARY_MAX_BINDINGS,
  CANARY_CORRELATION_PLACEHOLDER,
  CANARY_FIXTURES,
  CANARY_SUBMIT_KINDS,
  type CanaryForm,
} from "@/lib/canaryWorkflow";
import AppShell from "@/components/AppShell.vue";
import { useSession } from "@/stores/session";
import { useWorkspace } from "@/stores/workspace";

type MonitorType = "http" | "tcp" | "icmp" | "dns" | "tls" | "grpc" | "composite" | "postgres" | "mysql" | "redis" | "promql" | "rabbitmq" | "websocket" | "ssh" | "synthetic" | "async_canary" | "push";
type CreateMonitor = components["schemas"]["CreateMonitor"];
type UpdateMonitor = components["schemas"]["UpdateMonitor"];
type Channel = components["schemas"]["NotificationChannel"];
type ProjectSecret = components["schemas"]["ProjectSecret"];

const httpMethods = ["GET", "POST", "HEAD", "PUT", "DELETE"];

const ws = useWorkspace();
const router = useRouter();
const route = useRoute();
const editId = (route.params.id as string) || "";
const isEdit = computed(() => !!editId);
// While prefilling an edit, suppress the type watcher that resets conditions.
const prefilling = ref(false);

const types: { key: MonitorType; label: string; hint: string }[] = [
  { key: "http", label: "HTTP(S)", hint: "Status, latency, body" },
  { key: "tcp", label: "TCP", hint: "Port connectivity" },
  { key: "icmp", label: "ICMP", hint: "Ping reachability" },
  { key: "dns", label: "DNS", hint: "Hostname resolves" },
  { key: "tls", label: "TLS cert", hint: "Handshake + expiry" },
  { key: "grpc", label: "gRPC", hint: "Health check" },
  { key: "composite", label: "Group", hint: "Aggregate monitors" },
  { key: "postgres", label: "PostgreSQL", hint: "Connect + query" },
  { key: "mysql", label: "MySQL", hint: "Connect + query" },
  { key: "redis", label: "Redis", hint: "AUTH + PING" },
  { key: "promql", label: "PromQL", hint: "Prometheus query" },
  { key: "rabbitmq", label: "RabbitMQ", hint: "AMQP / mgmt API" },
  { key: "websocket", label: "WebSocket", hint: "Upgrade handshake" },
  { key: "ssh", label: "SSH", hint: "Banner check" },
  { key: "synthetic", label: "Synthetic", hint: "Multi-step HTTP flow" },
  { key: "async_canary", label: "Async canary", hint: "One async API journey" },
  { key: "push", label: "Push", hint: "Dead-man's switch" },
];

const form = reactive<{
  name: string;
  type: MonitorType;
  target: string;
  method: string;
  interval_seconds: number;
  timeout_seconds: number;
  retries: number;
  failure_threshold: number;
  confirm_interval_seconds: number;
  renotify_seconds: number;
  grace_seconds: number;
  enabled: boolean;
  auto_incident: boolean;
  tags: string[];
  region: string;
  escalation_policy_id: string;
}>({
  name: "",
  type: "http",
  target: "",
  method: "GET",
  interval_seconds: 60,
  timeout_seconds: 10,
  retries: 0,
  failure_threshold: 1,
  confirm_interval_seconds: 10,
  renotify_seconds: 0,
  grace_seconds: 0,
  enabled: true,
  auto_incident: true,
  tags: [],
  region: "core",
  escalation_policy_id: "",
});

// Tag chips: type + Enter/comma to add, click × to remove. De-duped case-insensitively.
const tagInput = ref("");
function addTag() {
  const t = tagInput.value.trim();
  tagInput.value = "";
  if (!t || t.length > 40 || form.tags.length >= 20) return;
  if (form.tags.some((x) => x.toLowerCase() === t.toLowerCase())) return;
  form.tags.push(t);
}
function removeTag(t: string) {
  form.tags = form.tags.filter((x) => x !== t);
}

const isComposite = computed(() => form.type === "composite");
const isSynthetic = computed(() => form.type === "synthetic");
const isCanary = computed(() => form.type === "async_canary");

// FR-029 phase F. The workflow is a NESTED TYPED document; this is the flat form behind it, and every
// rule is the server's from `internal/domain/canary.go`, mirrored in `lib/canaryWorkflow.ts` so the
// operator meets each one AT THE FIELD instead of as a 400 after saving.
//
// There is no JSON editor here, and none on the read view either — by contract, on the mock the owner
// approved (D-0224). A textual view of a canonical document is a view somebody edits as text.
const canary = reactive<CanaryForm>(emptyCanaryForm());
const canaryRefusalList = computed(() =>
  isCanary.value ? canaryRefusals(canary, Number(form.timeout_seconds) || 0, projectSecretNames.value) : [],
);
/** The refusal for one field, or "" — so a control can render its own reason beside itself. */
function canaryRefusal(field: string): string {
  return canaryRefusalList.value.find((r) => r.field === field)?.message ?? "";
}
// A MONITOR-level rule, not a workflow one, so `canaryRefusals` does not cover it: one probe may not
// overlap the next, because the in-flight lease would refuse the second run and report
// `already_in_flight` forever. Surfaced at the field that causes it instead of as a 400 after Create.
const canaryCadenceRefusal = computed(() => {
  if (!isCanary.value) return "";
  const iv = Number(form.interval_seconds) || 0;
  const to = Number(form.timeout_seconds) || 0;
  if (iv > 0 && to > 0 && iv < to) {
    return "interval must be at least the timeout — one canary probe may not overlap the next";
  }
  return "";
});
/** Rows the form adds and removes. Kept here rather than in the library: this is view state. */
function addCanaryBinding() {
  if (canary.bindings.length < CANARY_MAX_BINDINGS) canary.bindings.push({ name: "", secret: "" });
}
function addCanaryHeader(rows: { name: string; value: string; secretRef: string }[]) {
  rows.push({ name: "", value: "", secretRef: "" });
}
function addCanaryField(rows: { key: string; value: string; secretRef: string }[]) {
  rows.push({ key: "", value: "", secretRef: "" });
}
const isDB = computed(() => form.type === "postgres" || form.type === "mysql"); // same connection form

// Synthetic scenario — visual step builder. Each step is an HTTP request sharing a
// variable context; extracts feed later steps' {{var}} interpolation, asserts gate pass.
type SynHeader = { k: string; v: string };
type SynExtract = { var: string; from: string; path: string };
type SynAssert = { that: string; op: string; value: string; path: string };
type SynStep = { name: string; method: string; url: string; headers: SynHeader[]; body: string; extract: SynExtract[]; assert: SynAssert[] };
const synMethods = ["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD"];
const synFroms = ["json", "header", "status", "body"];
const synThats = ["status", "body_contains", "json", "latency_ms"];
const synOps = ["eq", "ne", "lt", "gt", "contains"];
function blankStep(): SynStep {
  return { name: "", method: "GET", url: "", headers: [], body: "", extract: [], assert: [{ that: "status", op: "eq", value: "200", path: "" }] };
}
// The default scaffold demonstrates extract → interpolate WITHOUT teaching a credential
// pattern. It used to be `login` → `Authorization: Bearer {{token}}`, which the server refuses
// under D7 — so merely choosing Synthetic opened a form whose own example could not be saved,
// and two of my tests asserted that refusal as the normal initial state (party finding, P1).
// An extracted id used in a later path is what `extract` is actually for, and it is valid.
const scenarioSteps = ref<SynStep[]>([
  { name: "list orders", method: "GET", url: "https://api.internal/orders", headers: [], body: "",
    extract: [{ var: "order_id", from: "json", path: "data.0.id" }], assert: [{ that: "status", op: "eq", value: "200", path: "" }] },
  { name: "read one", method: "GET", url: "https://api.internal/orders/{{order_id}}", headers: [], body: "",
    extract: [], assert: [{ that: "status", op: "eq", value: "200", path: "" }] },
]);
function addStep() { scenarioSteps.value.push(blankStep()); }
function removeStep(i: number) { scenarioSteps.value.splice(i, 1); if (!scenarioSteps.value.length) scenarioSteps.value.push(blankStep()); }

// FR-028 stage 2 — a credential in a scenario is a NAMED BINDING (mock approved 2026-09-03).
// The panel that maps binding → project secret is the feature's whole visible surface; the
// placeholder in the document is what the operator sees in the header field. Every rule these
// drive is the server's, mirrored in `lib/scenarioBindings.ts` so it is met at the field.
const scenarioBindings = ref<ScenarioBinding[]>([]);
const scenarioMalformedRefs = ref<string[]>([]);
const newBinding = ref<ScenarioBinding | null>(null);
const projectSecretNames = computed(() => projectSecrets.value.map((s) => s.name ?? ""));
const bindingIssues = computed(() =>
  validateScenarioBindings(
    scenarioSteps.value.map((st) => ({ url: st.url, headers: st.headers, body: st.body })),
    scenarioBindings.value,
    projectSecretNames.value,
    projectSecretsLoaded.value,
    scenarioMalformedRefs.value,
  ),
);
// A binding is usable only from a step; where it is USED is what the panel reports, and the
// server refuses one that is declared and never sent.
function bindingUses(name: string): string[] {
  const out: string[] = [];
  const ph = bindingPlaceholder(name);
  scenarioSteps.value.forEach((st, si) => {
    for (const h of st.headers) if (h.v.includes(ph)) out.push(`step ${si + 1} · ${h.k.trim().toLowerCase() || "header"}`);
    if (st.body.includes(ph)) out.push(`step ${si + 1} · body`);
  });
  return out;
}
function addBinding() {
  const b = newBinding.value;
  if (!b || !b.name.trim() || !b.secret.trim()) return;
  scenarioBindings.value.push({ name: b.name.trim(), secret: b.secret.trim() });
  newBinding.value = null;
}
function removeBinding(i: number) { scenarioBindings.value.splice(i, 1); }
// Insert the placeholder where the operator is: a credential-bearing header whose value is a
// binding is the shape the server demands, so the control writes it rather than asking them to
// type it correctly.
// The name a header's value already carries, when it is exactly one placeholder — so the
// select shows what is stored rather than resetting the operator's choice on every render.
function headerBindingName(value: string): string {
  const b = scenarioBindings.value.find((x) => value.trim() === bindingPlaceholder(x.name));
  return b ? b.name : "";
}
function useBindingInHeader(si: number, hi: number, name: string) {
  if (!name) return;
  scenarioSteps.value[si].headers[hi].v = bindingPlaceholder(name);
}
// D10 at the button, not as a 400 afterwards: an unsaved test has no envelope to carry a
// binding, so a placeholder would travel to the target as literal text.
const testBlockedByBindings = computed(() => (isSynthetic.value ? testBeforeSaveBlockedReason(scenarioBindings.value) : ""));

// True when an existing synthetic monitor came back without its scenario: the caller cannot
// write it, so the server did not send it (FR-028 stage 1).
const scenarioWithheld = ref(false);
const scenarioError = computed(() => {
  if (!isSynthetic.value) return "";
  if (!scenarioSteps.value.length) return "Add at least one step.";
  for (const [i, s] of scenarioSteps.value.entries()) {
    if (!s.url.trim()) return `Step ${i + 1} needs a URL.`;
    if (s.extract.some((e) => !e.var.trim() || ((e.from === "json" || e.from === "header") && !e.path.trim()))) return `Step ${i + 1}: an extract is missing var/path.`;
    if (s.assert.some((a) => a.that === "json" && !a.path.trim())) return `Step ${i + 1}: a json assert needs a path.`;
  }
  return firstBindingIssue(bindingIssues.value);
});
// Serialize the builder to the compact scenario JSON the backend stores/validates.
function syntheticConfig(): Record<string, string> {
  const steps = scenarioSteps.value.map((s) => {
    const step: Record<string, unknown> = { url: s.url.trim() };
    if (s.name.trim()) step.name = s.name.trim();
    if (s.method && s.method !== "GET") step.method = s.method;
    const headers = Object.fromEntries(s.headers.filter((h) => h.k.trim()).map((h) => [h.k.trim(), h.v]));
    if (Object.keys(headers).length) step.headers = headers;
    if (s.body.trim()) step.body = s.body;
    if (s.extract.length) step.extract = s.extract.map((e) => ({ var: e.var.trim(), from: e.from, ...(e.path.trim() ? { path: e.path.trim() } : {}) }));
    if (s.assert.length) step.assert = s.assert.map((a) => ({ that: a.that, ...(a.op ? { op: a.op } : {}), ...(a.value ? { value: a.value } : {}), ...(a.path.trim() ? { path: a.path.trim() } : {}) }));
    return step;
  });
  return applyBindings({ scenario: JSON.stringify({ steps }) }, scenarioBindings.value);
}
const isRedis = computed(() => form.type === "redis");
const isPromQL = computed(() => form.type === "promql");
const isRabbitMQ = computed(() => form.type === "rabbitmq");

const credentialMode = ref<"value" | "ref">("value");
const initialCredentialMode = ref<"value" | "ref">("value");
const secretRef = ref("");
const projectSecrets = ref<ProjectSecret[]>([]);
const projectSecretsLoaded = ref(false);
// A monitor can outlive the secret it points at: the reference is a NAME, and the guards
// only refuse a delete while a reference exists — a bundle-managed rename, a restore from
// an older file, or a project moved between environments can all leave one dangling. The
// <select> below binds a value that is then absent from its options, so without this the
// field renders blank and the operator is told nothing at all.
const danglingSecretRef = computed(() =>
  isDanglingSecretRef(credentialMode.value, secretRef.value, projectSecrets.value, projectSecretsLoaded.value),
);
const secretFeatureDisabled = ref(false);
const tlsSettings = reactive({ enabled: true, skipVerify: false });

// RabbitMQ: AMQP protocol handshake (default) or the management HTTP API. The
// management mode uses basic auth (username + write-only password, reusing `pg`)
// and an optional path.
const rabbitMode = ref<"amqp" | "management">("amqp");
// PromQL basic auth is OPTIONAL (D-0215): `none` is the default and the shape every existing
// monitor has, so the credential block only appears once the operator asks for it.
const promqlAuth = ref<"none" | "basic">("none");
const rabbitPath = ref("");
const credentialRequired = computed(
  () => isDB.value || isRedis.value || (isRabbitMQ.value && rabbitMode.value === "management") || (isPromQL.value && promqlAuth.value === "basic"),
);
function rabbitConfig(): Record<string, string> {
  if (rabbitMode.value !== "management") return { mode: "amqp" };
  const base: Record<string, string> = {
    mode: "management",
    username: pg.username.trim(),
    tls: String(tlsSettings.enabled),
    tls_skip_verify: String(tlsSettings.skipVerify),
  };
  if (rabbitPath.value.trim()) base.path = rabbitPath.value.trim();
  return withSecret(base);
}

// Connection config. `password` is write-only: it's never returned by the API,
// so on edit an empty field means "keep the stored password". Reused across the
// DB types; `query` doubles as the PromQL expression.
const pg = reactive({ database: "", username: "", password: "", sslmode: "require", query: "" });
function withSecret(base: Record<string, string>): Record<string, string> {
  return credentialMode.value === "ref"
    ? applyCredentialSelection(base, { mode: "ref", ref: secretRef.value })
    : applyCredentialSelection(base, { mode: "value", value: pg.password });
}
function pgConfig(): Record<string, string> {
  const base: Record<string, string> = { database: pg.database.trim(), username: pg.username.trim(), query: pg.query.trim() };
  if (form.type === "postgres") base.sslmode = pg.sslmode;
  else {
    base.tls = String(tlsSettings.enabled);
    base.tls_skip_verify = String(tlsSettings.skipVerify);
  }
  return withSecret(base);
}
function redisConfig(): Record<string, string> {
  return withSecret({ username: pg.username.trim(), tls: String(tlsSettings.enabled), tls_skip_verify: String(tlsSettings.skipVerify) });
}
function promqlConfig(): Record<string, string> {
  const base: Record<string, string> = { query: pg.query.trim() };
  if (promqlAuth.value !== "basic") return base;
  // The discriminator is `auth_mode`, not `auth`: a settings key literally named `auth` is
  // refused by the file provider's inline-secret guard, and the schema uses one spelling
  // across every surface.
  return withSecret({ ...base, auth_mode: "basic", username: pg.username.trim() });
}
// Which per-type config to send, if any.
function typeConfig(): Record<string, string> | undefined {
  if (isComposite.value) {
    const cfg: Record<string, string> = { children: [...childIds.value].join(","), mode: mode.value };
    if (mode.value === "quorum") cfg.quorum = String(quorum.value);
    return cfg;
  }
  if (isDB.value) return pgConfig();
  if (isRedis.value) return redisConfig();
  if (isPromQL.value) return promqlConfig();
  if (isRabbitMQ.value) return rabbitConfig();
  if (isSynthetic.value) return syntheticConfig();
  if (isCanary.value) return buildCanaryConfig(canary);
  return undefined;
}

// Composite (group) monitor: aggregate child monitors of this project.
type Monitor = components["schemas"]["Monitor"];
const projectMonitors = ref<Monitor[]>([]);
const childIds = ref<Set<string>>(new Set());

// ---- dependency graph (depends_on) ----------------------------------------
const depIds = ref<Set<string>>(new Set());
function toggleDep(id: string) {
  const s = new Set(depIds.value);
  if (s.has(id)) s.delete(id);
  else s.add(id);
  depIds.value = s;
}
// Transitive client-side cycle probe over the loaded project graph: picking a
// candidate whose ancestry chain reaches this monitor would close a loop.
function reachesMe(fromId: string, visited = new Set<string>()): boolean {
  if (!editId) return false;
  if (fromId === editId) return true;
  if (visited.has(fromId)) return false;
  visited.add(fromId);
  const m = projectMonitors.value.find((x) => x.id === fromId);
  return (m?.depends_on ?? []).some((p) => reachesMe(p, visited));
}
const depCandidates = computed(() =>
  projectMonitors.value
    .filter((m) => m.id !== editId)
    .map((m) => ({ id: m.id ?? "", name: m.name ?? "", type: m.type ?? "", down: m.status === "down", cycle: reachesMe(m.id ?? "") })),
);
function monName(id: string): { name: string; down: boolean } {
  const m = projectMonitors.value.find((x) => x.id === id);
  return { name: m?.name ?? id.slice(0, 8), down: m?.status === "down" };
}

// ---- multi-region set wizard (D-0101) ---------------------------------------
// Pure frontend sugar: expands one spec into N region children ("name @ region",
// no channels) plus a quorum composite that carries the alerting. Backend only
// knows composite mode "quorum".
const multiRegion = ref(false);
const mrRegions = ref<Set<string>>(new Set());
const mrQuorum = ref(2);
function toggleMrRegion(name: string) {
  const s = new Set(mrRegions.value);
  if (s.has(name)) s.delete(name);
  else s.add(name);
  mrRegions.value = s;
  mrQuorum.value = Math.ceil((s.size + 1) / 2); // default: majority
}
const mrEligible = computed(() => !isEdit.value && showTarget.value);
async function createMultiRegionSet(): Promise<void> {
  const regionsPicked = [...mrRegions.value];
  const created: string[] = [];
  const rollback = async () => {
    for (const id of created) await api.DELETE("/api/v1/monitors/{monitorID}", { params: { path: { monitorID: id } } });
  };
  const base: CreateMonitor = {
    name: form.name.trim(),
    type: form.type,
    target: form.target.trim(),
    interval_seconds: form.interval_seconds,
    timeout_seconds: form.timeout_seconds,
    retries: form.retries,
    failure_threshold: form.failure_threshold,
    confirm_interval_seconds: form.confirm_interval_seconds,
    renotify_seconds: form.renotify_seconds,
    grace_seconds: 0,
    enabled: form.enabled,
    auto_incident: false, // children stay quiet; the composite owns incidents/alerts
    tags: form.tags,
  };
  if (form.type === "http") base.method = form.method as CreateMonitor["method"];
  if (showConditions.value) base.conditions = assembled.value;
  { const cfg = typeConfig(); if (cfg) base.config = cfg; }
  for (const r of regionsPicked) {
    const res = await api.POST("/api/v1/projects/{projectID}/monitors", {
      params: { path: { projectID: ws.projectId } },
      body: { ...base, name: `${form.name.trim()} @ ${r}`, region: r },
    });
    if (res.error || !res.data?.id) {
      await rollback();
      throw new Error((res.error as { error?: string })?.error || `Could not create the ${r} member.`);
    }
    created.push(res.data.id);
  }
  const comp = await api.POST("/api/v1/projects/{projectID}/monitors", {
    params: { path: { projectID: ws.projectId } },
    body: {
      name: form.name.trim(),
      type: "composite",
      interval_seconds: form.interval_seconds,
      timeout_seconds: form.timeout_seconds,
      retries: 0,
      failure_threshold: 1,
      confirm_interval_seconds: 0,
      renotify_seconds: form.renotify_seconds,
      grace_seconds: 0,
      enabled: form.enabled,
      auto_incident: form.auto_incident,
      tags: form.tags,
      region: "core",
      escalation_policy_id: form.escalation_policy_id,
      config: { children: created.join(","), mode: "quorum", quorum: String(mrQuorum.value) },
    },
  });
  if (comp.error || !comp.data?.id) {
    await rollback();
    throw new Error((comp.error as { error?: string })?.error || "Could not create the quorum composite.");
  }
  await syncChannels(comp.data.id); // alerts live on the composite
  router.push({ name: "monitor", params: { id: comp.data.id } });
}
const mode = ref<"all" | "any" | "quorum">("all");
const quorum = ref(2); // down-vote threshold for mode "quorum"
function toggleChild(id: string) {
  const s = new Set(childIds.value);
  if (s.has(id)) s.delete(id);
  else s.add(id);
  childIds.value = s;
}
// Candidate children exclude the monitor being edited (no self-reference).
const childCandidates = computed(() => projectMonitors.value.filter((m) => m.id !== editId));

// Escalation policies (project-scoped) selectable for on-call routing of down alerts.
const escalationPolicies = ref<{ id: string; name: string }[]>([]);
// Notification channels (project-scoped) the monitor can be linked to at create/edit.
const channels = ref<Channel[]>([]);
const selectedChannels = ref<Set<string>>(new Set());
const initialChannels = ref<Set<string>>(new Set());
function toggleChannel(id: string) {
  const s = new Set(selectedChannels.value);
  if (s.has(id)) s.delete(id);
  else s.add(id);
  selectedChannels.value = s;
}

// Link newly-checked channels and unlink unchecked ones (create starts empty).
async function syncChannels(monitorID: string) {
  const before = initialChannels.value;
  const target = selectedChannels.value;
  const add = [...target].filter((id) => !before.has(id));
  const remove = [...before].filter((id) => !target.has(id));
  await Promise.all([
    ...add.map((id) =>
      api.POST("/api/v1/monitors/{monitorID}/notifications", { params: { path: { monitorID } }, body: { channel_id: id } }),
    ),
    ...remove.map((id) =>
      api.DELETE("/api/v1/monitors/{monitorID}/notifications/{channelID}", { params: { path: { monitorID, channelID: id } } }),
    ),
  ]);
}

// Structured conditions — serialized to the backend's string[] on submit.
type Cond = { p: string; o: string; v: string };
const placeholders = ["[STATUS]", "[RESPONSE_TIME]", "[CONNECTED]", "[CERT_EXPIRY]", "[RESULT]", "[BODY]"];
const opsNum = ["==", "!=", "<", "<=", ">", ">="];
const opsStr = ["==", "!=", "contains", "matches"];
function opsFor(p: string) {
  return p === "[BODY]" ? opsStr : opsNum;
}
function defaultConds(t: MonitorType): Cond[] {
  if (t === "http") return [{ p: "[STATUS]", o: "==", v: "200" }, { p: "[RESPONSE_TIME]", o: "<", v: "500" }];
  if (t === "tcp" || t === "icmp" || t === "dns" || t === "websocket" || t === "ssh" || t === "rabbitmq") return [{ p: "[CONNECTED]", o: "==", v: "true" }];
  if (t === "tls") return [{ p: "[CERT_EXPIRY]", o: ">", v: "14" }];
  return [];
}
const conds = ref<Cond[]>(defaultConds("http"));

function condString(c: Cond): string {
  const v = c.p === "[BODY]" ? `"${c.v}"` : c.v;
  return `${c.p} ${c.o} ${v}`;
}
function parseCond(s: string): Cond {
  const m = s.match(/^(\[[A-Z_]+\])\s+(\S+)\s+([\s\S]*)$/);
  if (!m) return { p: "[STATUS]", o: "==", v: s };
  let v = m[3].trim();
  if (v.startsWith("\"") && v.endsWith("\"")) v = v.slice(1, -1);
  return { p: m[1], o: m[2], v };
}
const assembled = computed(() => conds.value.map(condString));

function addCond() {
  conds.value.push({ p: "[STATUS]", o: "==", v: "200" });
}
function removeCond(i: number) {
  conds.value.splice(i, 1);
}
function fixOps(c: Cond) {
  if (!opsFor(c.p).includes(c.o)) c.o = opsFor(c.p)[0];
}

// A canary has no `target`: its address is the submit URL inside the workflow, and `NeedsTarget()`
// is false for it server-side. The field is HIDDEN rather than asked for and ignored — the same
// mistake that left a synthetic monitor uncreatable from this form until iter-0167.
const showTarget = computed(() => !["push", "composite", "synthetic", "async_canary"].includes(form.type));
const showConditions = computed(() => !["push", "composite", "postgres", "mysql", "redis", "synthetic", "async_canary"].includes(form.type));
const targetLabel = computed(() =>
  form.type === "http"
    ? "URL"
    : form.type === "promql"
      ? "Prometheus URL"
      : form.type === "websocket"
        ? "WebSocket URL"
        : form.type === "icmp" || form.type === "dns"
          ? "Hostname"
          : "Host : port",
);
const targetHint = computed(() => {
  if (form.type === "http") return "The endpoint to request.";
  if (form.type === "tcp") return "Host and port to connect to.";
  if (form.type === "icmp") return "Host or IP to ping.";
  if (form.type === "dns") return "Hostname to resolve (A/AAAA).";
  if (form.type === "tls") return "TLS endpoint; port defaults to 443.";
  if (form.type === "grpc") return "gRPC endpoint; port defaults to 50051.";
  if (form.type === "postgres") return "PostgreSQL host; port defaults to 5432.";
  if (form.type === "mysql") return "MySQL host; port defaults to 3306.";
  if (form.type === "redis") return "Redis host; port defaults to 6379.";
  if (form.type === "promql") return "Prometheus base URL.";
  if (form.type === "rabbitmq") return rabbitMode.value === "management" ? "Management base URL or host (port defaults to 15672)." : "Broker host; AMQP port defaults to 5672.";
  if (form.type === "websocket") return "ws:// or wss:// endpoint.";
  if (form.type === "ssh") return "SSH host; port defaults to 22.";
  return "cerbix generates a URL; your job POSTs to it.";
});
const targetPlaceholder = computed(() => {
  if (form.type === "http") return "https://api.internal/health";
  if (form.type === "icmp") return "host.internal or 10.0.0.1";
  if (form.type === "dns") return "api.internal";
  if (form.type === "tls") return "api.internal:443";
  if (form.type === "grpc") return "svc.internal:50051";
  if (form.type === "mysql") return "db.internal:3306";
  if (form.type === "redis") return "cache.internal:6379";
  if (form.type === "promql") return "http://prometheus.internal:9090";
  if (form.type === "rabbitmq") return rabbitMode.value === "management" ? "http://rabbit.internal:15672" : "rabbit.internal:5672";
  if (form.type === "websocket") return "wss://api.internal/ws";
  if (form.type === "ssh") return "host.internal:22";
  return "db.internal:5432";
});
const schedTitle = computed(() => (form.type === "push" ? "Heartbeat" : "Schedule"));

// Per-type condition defaults when the type changes (not while editing).
watch(
  () => form.type,
  (t) => {
    if (prefilling.value) return;
    conds.value = defaultConds(t);
  },
);

const submitting = ref(false);
const error = ref("");

const session = useSession();
const writeAllowed = computed(() => session.canProjectWrite(ws.orgId, ws.projectId));
const canSubmit = computed(() => {
  if (!writeAllowed.value || !form.name.trim() || submitting.value) return false;
  if (form.type === "push") return true;
  if (form.type === "composite") return childIds.value.size > 0;
  // A synthetic monitor has NO target — the scenario is what it probes, and `showTarget`
  // hides the field for it — so requiring one below left Create permanently disabled and
  // unexplained: a synthetic monitor could not be created from this form at all. Found by
  // `NewMonitorScenarioBinding.spec.ts` while proving the binding body; the gap survived
  // because no test and no E2E had ever submitted this type (FR-SYN-3's named coverage gap).
  if (isSynthetic.value) return !scenarioError.value;
  // Same shape, same reason: a canary has no target, so requiring one would disable Create for it
  // permanently and without explanation.
  if (isCanary.value) return !canaryCadenceRefusal.value && canaryRefusalList.value.length === 0;
  if (credentialRequired.value) {
    if (credentialMode.value === "ref" && !secretRef.value) return false;
    if (credentialMode.value === "value" && !pg.password && (!isEdit.value || initialCredentialMode.value !== "value")) return false;
  }
  return !!form.target.trim();
});

// live preview
const typeChip = computed<Record<MonitorType, string>>(() => ({ http: "HTTP", tcp: "TCP", icmp: "ICMP", dns: "DNS", tls: "TLS", grpc: "gRPC", composite: "GROUP", postgres: "PG", mysql: "MySQL", redis: "Redis", promql: "PromQL", rabbitmq: "RabbitMQ", websocket: "WS", ssh: "SSH", synthetic: "FLOW", async_canary: "CANARY", push: "PUSH" }));
const previewTarget = computed(() => {
  if (form.type === "push") return "push endpoint · generated on create";
  if (form.type === "composite") return `${childIds.value.size} member${childIds.value.size === 1 ? "" : "s"} · ${mode.value}`;
  // A canary has no target: what it probes is the submit URL inside the workflow.
  if (isCanary.value) return canary.submitURL || "the workflow's submit URL";
  const t = form.target || targetPlaceholder.value;
  return form.type === "http" ? `${form.method} ${t}` : t;
});
const previewConds = computed(() => {
  if (form.type === "push") return [`expected every ${form.interval_seconds}s`];
  if (form.type === "composite") return [mode.value === "all" ? "all members up" : "any member up"];
  if (assembled.value.length) return assembled.value;
  const dflt =
    form.type === "http"
      ? "2xx"
      : form.type === "dns"
        ? "resolves"
        : form.type === "tls"
          ? "handshake + valid cert"
          : form.type === "grpc"
            ? "SERVING"
            : form.type === "postgres" || form.type === "mysql"
              ? "query ok"
              : form.type === "redis"
                ? "PONG"
                : form.type === "promql"
                  ? "result present"
                  : "connect";
  return [`default: ${dflt}`];
});
const summary = computed(() => {
  if (form.type === "push") return `Expects a heartbeat every ${form.interval_seconds}s${form.grace_seconds ? ` (+${form.grace_seconds}s grace)` : ""}.`;
  if (form.type === "composite") return `Up when ${mode.value === "all" ? "all" : "any"} of ${childIds.value.size} member${childIds.value.size === 1 ? "" : "s"} ${mode.value === "all" ? "are" : "is"} up; re-evaluated every ${form.interval_seconds}s.`;
  const r = form.retries;
  return `Checks every ${form.interval_seconds}s, times out after ${form.timeout_seconds}s, ${r} ${r === 1 ? "retry" : "retries"}.`;
});

async function submit() {
  if (!canSubmit.value) return;
  if (!ws.projectId) {
    error.value = "Select a project first.";
    return;
  }
  if (isSynthetic.value && scenarioError.value) {
    error.value = scenarioError.value;
    return;
  }
  submitting.value = true;
  error.value = "";
  try {
    if (isEdit.value) {
      const patch: UpdateMonitor = {
        name: form.name.trim(),
        interval_seconds: form.interval_seconds,
        timeout_seconds: form.timeout_seconds,
        retries: form.retries,
        failure_threshold: form.failure_threshold,
        confirm_interval_seconds: form.confirm_interval_seconds,
        depends_on: [...depIds.value],
        renotify_seconds: form.renotify_seconds,
        enabled: form.enabled,
        auto_incident: form.auto_incident,
        tags: form.tags,
        region: form.type === "composite" ? "core" : form.region.trim() || "core",
        escalation_policy_id: form.escalation_policy_id,
      };
      if (showTarget.value) patch.target = form.target.trim();
      if (form.type === "http") patch.method = form.method as UpdateMonitor["method"];
      if (form.type === "push") patch.grace_seconds = form.grace_seconds;
      if (showConditions.value) patch.conditions = assembled.value;
      { const cfg = typeConfig(); if (cfg) patch.config = cfg; }
      const res = await api.PATCH("/api/v1/monitors/{monitorID}", {
        params: { path: { monitorID: editId } },
        body: patch,
      });
      if (res.error || !res.data) {
        error.value = (res.error as { error?: string })?.error || "Could not update the monitor.";
        return;
      }
      await syncChannels(editId);
      router.push({ name: "monitor", params: { id: editId } });
      return;
    }
    if (multiRegion.value && mrEligible.value) {
      if (mrRegions.value.size < 2) {
        error.value = "Pick at least two regions for a multi-region set.";
        return;
      }
      if (mrQuorum.value < 1 || mrQuorum.value > mrRegions.value.size) {
        error.value = "Quorum must be between 1 and the number of regions.";
        return;
      }
      try {
        await createMultiRegionSet();
      } catch (e) {
        error.value = e instanceof Error ? e.message : "Could not create the multi-region set.";
      }
      return;
    }
    const body: CreateMonitor = {
      name: form.name.trim(),
      type: form.type,
      interval_seconds: form.interval_seconds,
      timeout_seconds: form.timeout_seconds,
      retries: form.retries,
      failure_threshold: form.failure_threshold,
      confirm_interval_seconds: form.confirm_interval_seconds,
      depends_on: [...depIds.value],
      renotify_seconds: form.renotify_seconds,
      grace_seconds: form.grace_seconds,
      enabled: form.enabled,
      auto_incident: form.auto_incident,
      tags: form.tags,
      region: form.type === "composite" ? "core" : form.region.trim() || "core",
      escalation_policy_id: form.escalation_policy_id,
    };
    if (showTarget.value) body.target = form.target.trim();
    if (form.type === "http") body.method = form.method as CreateMonitor["method"];
    if (showConditions.value) body.conditions = assembled.value;
    { const cfg = typeConfig(); if (cfg) body.config = cfg; }
    const res = await api.POST("/api/v1/projects/{projectID}/monitors", {
      params: { path: { projectID: ws.projectId } },
      body,
    });
    if (res.error || !res.data) {
      error.value = (res.error as { error?: string })?.error || "Could not create the monitor.";
      return;
    }
    if (res.data.id) await syncChannels(res.data.id);
    router.push({ name: "monitor", params: { id: res.data.id } });
  } catch {
    error.value = isEdit.value ? "Could not update the monitor." : "Could not create the monitor.";
  } finally {
    submitting.value = false;
  }
}

async function loadForEdit() {
  const res = await api.GET("/api/v1/monitors/{monitorID}", { params: { path: { monitorID: editId } } });
  const m = res.data;
  if (!m) {
    error.value = "Monitor not found.";
    return;
  }
  prefilling.value = true;
  form.name = m.name ?? "";
  form.type = (m.type as MonitorType) ?? "http";
  form.target = m.target ?? "";
  form.method = m.method || "GET";
  form.interval_seconds = m.interval_seconds ?? 60;
  form.timeout_seconds = m.timeout_seconds ?? 10;
  form.retries = m.retries ?? 0;
  form.failure_threshold = m.failure_threshold ?? 1;
  form.confirm_interval_seconds = m.confirm_interval_seconds ?? 10;
  depIds.value = new Set(m.depends_on ?? []);
  form.renotify_seconds = m.renotify_seconds ?? 0;
  form.grace_seconds = m.grace_seconds ?? 0;
  form.enabled = m.enabled ?? true;
  form.auto_incident = m.auto_incident ?? true;
  form.tags = m.tags ?? [];
  form.region = m.region || "core";
  form.escalation_policy_id = m.escalation_policy_id ?? "";
  conds.value = (m.conditions ?? []).map(parseCond);
  // Composite: prefill children + mode from config.
  if (m.type === "composite") {
    childIds.value = new Set((m.config?.children ?? "").split(",").map((s) => s.trim()).filter(Boolean));
  mode.value = (m.config?.mode as "all" | "any" | "quorum") || "all";
  quorum.value = Number(m.config?.quorum ?? 2) || 2;
  }
  // The server withholds a synthetic scenario from a caller who may not write the monitor
  // (FR-028 stage 1). Say so, rather than rendering an empty step builder that looks like a
  // monitor with no scenario — an empty scaffold is a lie a reader would act on.
  scenarioWithheld.value = m.type === "synthetic" && !m.config?.scenario;
  // The reference keys are NOT secret — they hold a name — so they come back on every read and
  // the panel can be rebuilt exactly. A key that does not parse is kept and named rather than
  // dropped: silently discarding it would delete a declaration the operator made.
  // A saved canary reads back into the SAME typed form. The document holds `secret_ref` markers and
  // the flat keys hold the project-secret names; the library recombines the two halves, which is why
  // the read view is complete without a JSON editor.
  if (m.type === "async_canary") {
    const back = parseCanaryConfig((m.config ?? {}) as Record<string, string>);
    if (back) Object.assign(canary, back);
  }
  if (m.type === "synthetic") {
    scenarioBindings.value = bindingsFromConfig(m.config as Record<string, string> | undefined);
    scenarioMalformedRefs.value = malformedRefKeys(m.config as Record<string, string> | undefined);
  }
  // Synthetic: parse the stored scenario JSON back into the visual step builder.
  if (m.type === "synthetic" && m.config?.scenario) {
    try {
      const parsed = JSON.parse(m.config.scenario) as { steps?: Array<Record<string, unknown>> };
      const steps = (parsed.steps ?? []).map((raw): SynStep => ({
        name: (raw.name as string) ?? "",
        method: (raw.method as string) ?? "GET",
        url: (raw.url as string) ?? "",
        headers: Object.entries((raw.headers as Record<string, string>) ?? {}).map(([k, v]) => ({ k, v })),
        body: (raw.body as string) ?? "",
        extract: ((raw.extract as SynExtract[]) ?? []).map((e) => ({ var: e.var ?? "", from: e.from ?? "json", path: e.path ?? "" })),
        assert: ((raw.assert as SynAssert[]) ?? []).map((a) => ({ that: a.that ?? "status", op: a.op ?? "eq", value: a.value ?? "", path: a.path ?? "" })),
      }));
      if (steps.length) scenarioSteps.value = steps;
    } catch {
      /* leave the default scaffold if the stored scenario is unparyable */
    }
  }
  // DB / redis / promql: prefill config (password redacted by the API — left blank).
  if (m.type === "postgres" || m.type === "mysql" || m.type === "redis" || m.type === "promql") {
    pg.database = m.config?.database ?? "";
    pg.username = m.config?.username ?? "";
    pg.sslmode = m.config?.sslmode || "require";
    pg.query = m.config?.query ?? "";
    pg.password = "";
    tlsSettings.enabled = m.config?.tls !== "false";
    tlsSettings.skipVerify = m.config?.tls_skip_verify === "true";
  }
  // RabbitMQ: prefill mode + management basic-auth username/path (password redacted).
  if (m.type === "rabbitmq") {
    rabbitMode.value = m.config?.mode === "management" ? "management" : "amqp";
    promqlAuth.value = m.config?.auth_mode === "basic" ? "basic" : "none";
    rabbitPath.value = m.config?.path ?? "";
    pg.username = m.config?.username ?? "";
    pg.password = "";
    tlsSettings.enabled = m.config?.tls !== "false";
    tlsSettings.skipVerify = m.config?.tls_skip_verify === "true";
  }
  if (credentialRequired.value && m.config?.password_ref) {
    credentialMode.value = "ref";
    initialCredentialMode.value = "ref";
    secretRef.value = m.config.password_ref;
  } else {
    credentialMode.value = "value";
    initialCredentialMode.value = "value";
    secretRef.value = "";
  }
  // Preselect the channels already linked to this monitor.
  const linked = await api.GET("/api/v1/monitors/{monitorID}/notifications", { params: { path: { monitorID: editId } } });
  const ids = new Set((linked.data ?? []).map((c) => c.id!).filter(Boolean));
  selectedChannels.value = new Set(ids);
  initialChannels.value = new Set(ids);
  setTimeout(() => (prefilling.value = false), 0);
}

async function loadChannels() {
  if (!ws.projectId) return;
  const res = await api.GET("/api/v1/projects/{projectID}/notification-channels", { params: { path: { projectID: ws.projectId } } });
  channels.value = res.data ?? [];
}

async function loadEscalationPolicies() {
  if (!ws.projectId) return;
  const res = await api.GET("/api/v1/projects/{projectID}/escalation-policies", { params: { path: { projectID: ws.projectId } } });
  escalationPolicies.value = (res.data ?? []).map((p) => ({ id: p.id ?? "", name: p.name ?? "" }));
}

async function loadProjectMonitors() {
  if (!ws.projectId) return;
  const res = await api.GET("/api/v1/projects/{projectID}/monitors", { params: { path: { projectID: ws.projectId } } });
  projectMonitors.value = res.data ?? [];
}

async function loadProjectSecrets() {
  if (!ws.projectId) return;
  const res = await api.GET("/api/v1/projects/{projectID}/secrets", { params: { path: { projectID: ws.projectId } } });
  const code = (res.error as { error?: string } | undefined)?.error;
  secretFeatureDisabled.value = code === "feature_disabled";
  projectSecrets.value = res.data ?? [];
  projectSecretsLoaded.value = !res.error;
}

type Region = { name: string; live: boolean };
const regions = ref<Region[]>([{ name: "core", live: false }]);
const regionOpen = ref(false);
async function loadRegions() {
  const res = await api.GET("/api/v1/regions");
  if (res.data?.regions?.length) regions.value = res.data.regions as Region[];
}
const filteredRegions = computed(() => {
  const q = form.region.trim().toLowerCase();
  // Show the whole list when the field is empty OR holds an exact region name (a
  // committed selection, not a search) — so opening the picker after choosing "core"
  // still lists geo1/geo2. Filter only while the typed text is a partial query.
  if (!q || regions.value.some((r) => r.name.toLowerCase() === q)) return regions.value;
  return regions.value.filter((r) => r.name.toLowerCase().includes(q));
});
function pickRegion(name: string) {
  form.region = name;
  regionOpen.value = false;
}
function closeRegionSoon() {
  setTimeout(() => (regionOpen.value = false), 120); // let a click on an item register first
}

// ── Test connection (pre-create probe) ────────────────────────────────────
const isTestable = computed(() => form.type !== "push" && form.type !== "composite");
// The test probe is routed to a worker in the selected region; warn when that pool
// has no connected worker (the test would come back as "no worker responded").
const selectedRegionLive = computed(() => {
  const name = form.region.trim() || "core";
  return regions.value.some((r) => r.name === name && r.live);
});
const testing = ref(false);
const testResult = ref<{ up: boolean; latency_ms: number; code: number; msg: string } | null>(null);
const testError = ref("");
async function testConnection() {
  if (!ws.projectId || !isTestable.value) return;
  testing.value = true;
  testError.value = "";
  testResult.value = null;
  try {
    const body: Record<string, unknown> = {
      type: form.type,
      region: form.region.trim() || "core",
      timeout_seconds: form.timeout_seconds,
      interval_seconds: form.interval_seconds,
    };
    if (showTarget.value) body.target = form.target.trim();
    if (form.type === "http") body.method = form.method;
    if (showConditions.value) body.conditions = assembled.value;
    const cfg = typeConfig();
    if (cfg) body.config = cfg;
    const res = await api.POST("/api/v1/projects/{projectID}/monitors/test", {
      params: { path: { projectID: ws.projectId } },
      body: body as never,
    });
    if (res.error) {
      testError.value = (res.error as { error?: string })?.error || "Test failed.";
      return;
    }
    const d = res.data;
    testResult.value = { up: !!d?.up, latency_ms: d?.latency_ms ?? 0, code: d?.code ?? 0, msg: d?.msg ?? "" };
  } catch {
    testError.value = "Test failed.";
  } finally {
    testing.value = false;
  }
}

// Prefill a NEW monitor from the instance defaults (Settings → Monitor
// defaults) — the local literals above are only the pre-fetch fallback.
async function applyInstanceDefaults() {
  const res = await api.GET("/api/v1/monitor-defaults", {});
  const d = res.data;
  if (!d) return;
  if (d.interval_seconds) form.interval_seconds = d.interval_seconds;
  if (d.timeout_seconds) form.timeout_seconds = d.timeout_seconds;
  if (d.retries !== undefined) form.retries = d.retries;
  if (d.failure_threshold) form.failure_threshold = d.failure_threshold;
  if (d.renotify_seconds !== undefined) form.renotify_seconds = d.renotify_seconds;
  if (d.auto_incident !== undefined) form.auto_incident = d.auto_incident;
}

onMounted(async () => {
  await ws.init();
  await Promise.all([loadChannels(), loadEscalationPolicies(), loadProjectMonitors(), loadProjectSecrets(), loadRegions()]);
  if (isEdit.value) await loadForEdit();
  else await applyInstanceDefaults();
});

const inputCls =
  "h-[38px] w-full rounded-sm border border-border bg-surface-2 px-[11px] text-[13.5px] text-ink outline-none focus:border-accent focus:bg-surface";
const selectCls =
  "h-[34px] rounded-sm border border-border bg-surface-2 px-2 text-[13px] text-ink outline-none focus:border-accent";
</script>

<template>
  <AppShell active="monitors" :crumbs="[ws.orgName || 'cerbix', ws.projectName || '…', 'monitors', isEdit ? 'Edit' : 'New']">
    <div class="mx-auto max-w-[1060px] px-[22px] pb-16 pt-6">
      <div class="mb-5">
        <h1 class="text-[21px] font-semibold tracking-tight">{{ isEdit ? "Edit monitor" : "New monitor" }}</h1>
        <p class="mt-[3px] text-[13px] text-ink-3">
          {{ isEdit ? "Update" : "Add" }} a check {{ isEdit ? "in" : "to" }} the <b class="text-ink-2">{{ ws.projectName || "…" }}</b> project.
        </p>
      </div>

      <div class="grid grid-cols-[1fr_340px] items-start gap-5 max-[920px]:grid-cols-1">
        <!-- form -->
        <form class="flex flex-col gap-4" @submit.prevent="submit">
          <!-- basics -->
          <section class="rounded border border-border bg-surface shadow-card">
            <div class="px-4 pt-[13px]"><h2 class="text-[13px] font-semibold">Basics</h2></div>
            <div class="flex flex-col gap-[14px] px-4 pb-4 pt-[14px]">
              <label class="flex flex-col gap-[6px]">
                <span class="text-[12px] font-semibold text-ink-2">Name</span>
                <input v-model="form.name" type="text" placeholder="payments-callback" :class="[inputCls, 'font-mono']" />
              </label>
              <label class="flex flex-col gap-[6px]">
                <span class="text-[12px] font-semibold text-ink-2">Tags <span class="font-normal text-ink-3">· for filtering (env:prod, team:x)</span></span>
                <div class="flex flex-wrap items-center gap-[6px] rounded-sm border border-border bg-surface-2 px-2 py-[6px]">
                  <span v-for="t in form.tags" :key="t" class="inline-flex items-center gap-[5px] rounded-full bg-accent-weak px-[9px] py-[2px] font-mono text-[11.5px] text-accent">
                    {{ t }}
                    <button type="button" class="text-accent/70 hover:text-accent" @click="removeTag(t)">✕</button>
                  </span>
                  <input
                    v-model="tagInput"
                    type="text"
                    placeholder="add tag…"
                    class="min-w-[100px] flex-1 bg-transparent px-1 py-[2px] font-mono text-[13px] outline-none"
                    @keydown.enter.prevent="addTag"
                    @keydown="(e: KeyboardEvent) => { if (e.key === ',') { e.preventDefault(); addTag(); } }"
                    @blur="addTag"
                  />
                </div>
              </label>
              <div class="flex flex-col gap-[6px]">
                <span class="text-[12px] font-semibold text-ink-2">Region <span class="font-normal text-ink-3">· which worker pool probes this monitor</span></span>
                <div class="relative">
                  <input
                    v-model="form.region"
                    type="text"
                    :disabled="isComposite"
                    placeholder="core"
                    autocomplete="off"
                    :class="[inputCls, 'font-mono', isComposite && 'opacity-50']"
                    @focus="regionOpen = true; ($event.target as HTMLInputElement).select()"
                    @input="regionOpen = true"
                    @blur="closeRegionSoon"
                  />
                  <ul
                    v-if="regionOpen && !isComposite && filteredRegions.length"
                    class="absolute z-20 mt-1 max-h-[200px] w-full overflow-auto rounded-sm border border-border bg-surface py-1 shadow-card"
                  >
                    <li
                      v-for="r in filteredRegions"
                      :key="r.name"
                      class="flex cursor-pointer items-center justify-between px-3 py-[6px] text-[13px] hover:bg-surface-2"
                      :class="r.name === form.region.trim() && 'bg-accent-weak'"
                      @mousedown.prevent="pickRegion(r.name)"
                    >
                      <span class="font-mono" :class="r.name === form.region.trim() && 'font-semibold text-accent'">{{ r.name }}</span>
                      <span class="inline-flex items-center gap-[5px] text-[11px]" :class="r.live ? 'text-up' : 'text-ink-3'">
                        <span class="h-[6px] w-[6px] rounded-full" :class="r.live ? 'bg-up' : 'bg-ink-3/50'"></span>
                        {{ r.live ? "worker live" : "no worker" }}
                      </span>
                    </li>
                  </ul>
                </div>
                <span class="text-[11.5px] text-ink-3">
                  <template v-if="isComposite">Composite monitors always run in <code>core</code> (they read the database).</template>
                  <template v-else>Pick a pool or type a new one — a worker started with <code>--region &lt;name&gt;</code> serves it. <span class="text-up">●</span> = worker connected. Default <code>core</code>.</template>
                </span>
              </div>
              <div class="flex flex-col gap-[6px]">
                <span class="text-[12px] font-semibold text-ink-2">
                  Check type <span v-if="isEdit" class="font-normal text-ink-3">· can't be changed</span>
                </span>
                <div class="grid grid-cols-4 gap-[10px] max-[700px]:grid-cols-2 max-[480px]:grid-cols-1">
                  <button
                    v-for="t in types"
                    :key="t.key"
                    type="button"
                    :disabled="isEdit"
                    class="flex flex-col gap-[6px] rounded-sm border p-3 text-left disabled:cursor-not-allowed disabled:opacity-60"
                    :class="form.type === t.key ? 'border-accent bg-accent-weak shadow-[0_0_0_1px_var(--accent-weak)]' : 'border-border bg-surface-2 hover:border-border-strong'"
                    @click="form.type = t.key"
                  >
                    <span
                      class="grid h-[26px] w-[26px] place-items-center rounded-md border"
                      :class="form.type === t.key ? 'border-transparent bg-accent text-accent-ink' : 'border-border bg-surface text-accent'"
                    >
                      <svg viewBox="0 0 24 24" class="h-[15px] w-[15px]" fill="none" stroke="currentColor" stroke-width="2">
                        <template v-if="t.key === 'http'"><circle cx="12" cy="12" r="9" /><path d="M3 12h18M12 3a15 15 0 0 1 0 18M12 3a15 15 0 0 0 0 18" /></template>
                        <template v-else-if="t.key === 'tcp'"><rect x="3" y="10" width="18" height="6" rx="1.5" /><path d="M7 10V7M17 10V7M12 16v3" /></template>
                        <template v-else-if="t.key === 'icmp'"><circle cx="12" cy="12" r="2" /><path d="M8 8a5.5 5.5 0 0 0 0 8M16 8a5.5 5.5 0 0 1 0 8M5 5a10 10 0 0 0 0 14M19 5a10 10 0 0 1 0 14" /></template>
                        <template v-else-if="t.key === 'dns'"><circle cx="12" cy="12" r="9" /><path d="M3 12h18M12 3c2.5 2.5 2.5 15.5 0 18M12 3c-2.5 2.5-2.5 15.5 0 18" /></template>
                        <template v-else-if="t.key === 'tls'"><rect x="5" y="11" width="14" height="9" rx="2" /><path d="M8 11V8a4 4 0 0 1 8 0v3" /></template>
                        <template v-else-if="t.key === 'grpc'"><path d="M4 7h16M4 12h16M4 17h16M8 4v3M16 4v3M8 17v3M16 17v3" /></template>
                        <template v-else-if="t.key === 'composite'"><rect x="3" y="3" width="7" height="7" rx="1.5" /><rect x="14" y="3" width="7" height="7" rx="1.5" /><rect x="8.5" y="14" width="7" height="7" rx="1.5" /></template>
                        <template v-else-if="t.key === 'postgres' || t.key === 'mysql'"><ellipse cx="12" cy="6" rx="7" ry="3" /><path d="M5 6v12c0 1.7 3.1 3 7 3s7-1.3 7-3V6M5 12c0 1.7 3.1 3 7 3s7-1.3 7-3" /></template>
                        <template v-else-if="t.key === 'redis'"><rect x="3" y="4" width="18" height="4" rx="1" /><rect x="3" y="10" width="18" height="4" rx="1" /><rect x="3" y="16" width="18" height="4" rx="1" /></template>
                        <template v-else-if="t.key === 'promql'"><path d="M4 18V6M4 18h16M8 14l3-4 3 3 4-6" /></template>
                        <path v-else d="M3 12h4l2 6 4-14 2 8h6" />
                      </svg>
                    </span>
                    <b class="font-mono text-[13px] font-semibold" :class="form.type === t.key ? 'text-accent' : 'text-ink'">{{ t.label }}</b>
                    <span class="text-[11.5px] text-ink-3">{{ t.hint }}</span>
                  </button>
                </div>
              </div>
            </div>
          </section>

          <!-- target -->
          <section v-if="!isComposite" class="rounded border border-border bg-surface shadow-card">
            <div class="px-4 pt-[13px]">
              <h2 class="text-[13px] font-semibold">Target</h2>
              <p class="mt-[2px] text-[12px] text-ink-3">{{ targetHint }}</p>
            </div>
            <div class="px-4 pb-4 pt-[14px]">
              <div v-if="showTarget" class="flex flex-col gap-[6px]">
                <span class="text-[12px] font-semibold text-ink-2">{{ targetLabel }}</span>
                <div class="flex gap-2">
                  <select v-if="form.type === 'http'" v-model="form.method" :class="[selectCls, 'h-[38px] w-[94px] font-mono']">
                    <option v-for="mth in httpMethods" :key="mth" :value="mth">{{ mth }}</option>
                  </select>
                  <input v-model="form.target" type="text" :placeholder="targetPlaceholder" :class="[inputCls, 'font-mono text-[13px]']" />
                </div>
              </div>
              <div v-else class="flex flex-col gap-[6px]">
                <span class="text-[12px] font-semibold text-ink-2">Push URL</span>
                <div class="flex h-[38px] items-center rounded-sm border border-dashed border-border-strong bg-inset px-[11px] font-mono text-[12.5px] text-ink-3">
                  cerbix.example.com/push/&lt;token&gt;
                </div>
                <span class="text-[11.5px] text-ink-3">POST here on every run; cerbix alerts if it goes silent.</span>
              </div>
            </div>
          </section>

          <!-- DB connection (postgres / mysql) -->
          <section v-if="isDB" class="rounded border border-border bg-surface shadow-card">
            <div class="px-4 pt-[13px]">
              <h2 class="text-[13px] font-semibold">Connection</h2>
              <p class="mt-[2px] text-[12px] text-ink-3">Credentials are encrypted at rest. The password is write-only — leave it blank to keep the stored one.</p>
            </div>
            <div class="flex flex-col gap-[14px] px-4 pb-4 pt-[14px]">
              <div class="grid grid-cols-2 gap-3 max-[560px]:grid-cols-1">
                <label class="flex flex-col gap-[6px]">
                  <span class="text-[12px] font-semibold text-ink-2">Database</span>
                  <input v-model="pg.database" type="text" placeholder="app" :class="[inputCls, 'font-mono text-[13px]']" />
                </label>
                <label class="flex flex-col gap-[6px]">
                  <span class="text-[12px] font-semibold text-ink-2">SSL mode</span>
                  <select v-if="form.type === 'postgres'" v-model="pg.sslmode" :class="[selectCls, 'h-[38px]']">
                    <option value="disable">disable</option>
                    <option value="require">require</option>
                    <option value="verify-ca">verify-ca</option>
                    <option value="verify-full">verify-full</option>
                  </select>
                  <div v-else class="flex h-[38px] items-center gap-4 rounded-sm border border-border bg-surface-2 px-3 text-[12.5px]">
                    <label class="flex items-center gap-2"><input v-model="tlsSettings.enabled" type="checkbox" /> TLS</label>
                    <label class="flex items-center gap-2" :class="!tlsSettings.enabled && 'opacity-50'"><input v-model="tlsSettings.skipVerify" type="checkbox" :disabled="!tlsSettings.enabled" /> Skip verify</label>
                  </div>
                </label>
                <label class="flex flex-col gap-[6px]">
                  <span class="text-[12px] font-semibold text-ink-2">Username</span>
                  <input v-model="pg.username" type="text" placeholder="cerbix" :class="[inputCls, 'font-mono text-[13px]']" />
                </label>
                <div class="flex flex-col gap-[6px]">
                  <span class="text-[12px] font-semibold text-ink-2">Credential</span>
                  <div class="flex gap-3 text-[12px]"><label><input v-model="credentialMode" type="radio" value="value" /> Value</label><label :class="secretFeatureDisabled && 'opacity-50'"><input v-model="credentialMode" type="radio" value="ref" :disabled="secretFeatureDisabled" /> Secret reference</label></div>
                  <select v-if="credentialMode === 'ref'" v-model="secretRef" data-testid="monitor-secret-ref" :class="[selectCls, 'h-[38px]']"><option value="" disabled>Select a project secret</option><option v-for="s in projectSecrets" :key="s.id" :value="s.name">{{ s.name }}</option></select>
                  <p v-if="danglingSecretRef" data-testid="monitor-secret-ref-missing" class="mt-1 text-[12px] text-down">Secret <span class="font-mono">{{ secretRef }}</span> no longer exists in this project. This monitor cannot dispatch until you pick an existing secret.</p>
                  <input v-else v-model="pg.password" type="password" :placeholder="isEdit && initialCredentialMode === 'value' ? '•••••• (unchanged)' : ''" autocomplete="new-password" :class="[inputCls, 'font-mono text-[13px]']" />
                </div>
              </div>
              <label class="flex flex-col gap-[6px]">
                <span class="text-[12px] font-semibold text-ink-2">Query <span class="font-normal text-ink-3">· default SELECT 1</span></span>
                <input v-model="pg.query" type="text" placeholder="SELECT 1" :class="[inputCls, 'font-mono text-[13px]']" />
              </label>
            </div>
          </section>

          <!-- redis auth -->
          <section v-if="isRedis" class="rounded border border-border bg-surface shadow-card">
            <div class="px-4 pt-[13px]">
              <h2 class="text-[13px] font-semibold">Authentication</h2>
              <p class="mt-[2px] text-[12px] text-ink-3">Optional. The password is encrypted at rest and write-only — leave it blank to keep the stored one. cerbix sends AUTH then PING.</p>
            </div>
            <div class="grid grid-cols-2 gap-3 px-4 pb-4 pt-[14px] max-[560px]:grid-cols-1">
              <label class="flex flex-col gap-[6px]">
                <span class="text-[12px] font-semibold text-ink-2">Username <span class="font-normal text-ink-3">· ACL, optional</span></span>
                <input v-model="pg.username" type="text" placeholder="default" :class="[inputCls, 'font-mono text-[13px]']" />
              </label>
              <div class="flex flex-col gap-[6px]">
                <span class="text-[12px] font-semibold text-ink-2">Credential</span>
                <div class="flex gap-3 text-[12px]"><label><input v-model="credentialMode" type="radio" value="value" /> Value</label><label :class="secretFeatureDisabled && 'opacity-50'"><input v-model="credentialMode" type="radio" value="ref" :disabled="secretFeatureDisabled" /> Secret reference</label></div>
                <select v-if="credentialMode === 'ref'" v-model="secretRef" data-testid="monitor-secret-ref" :class="[selectCls, 'h-[38px]']"><option value="" disabled>Select a project secret</option><option v-for="s in projectSecrets" :key="s.id" :value="s.name">{{ s.name }}</option></select>
                  <p v-if="danglingSecretRef" data-testid="monitor-secret-ref-missing" class="mt-1 text-[12px] text-down">Secret <span class="font-mono">{{ secretRef }}</span> no longer exists in this project. This monitor cannot dispatch until you pick an existing secret.</p>
                <input v-else v-model="pg.password" type="password" :placeholder="isEdit && initialCredentialMode === 'value' ? '•••••• (unchanged)' : ''" autocomplete="new-password" :class="[inputCls, 'font-mono text-[13px]']" />
              </div>
              <div class="col-span-2 flex items-center gap-4 text-[12.5px] max-[560px]:col-span-1">
                <label class="flex items-center gap-2"><input v-model="tlsSettings.enabled" type="checkbox" /> TLS (verified)</label>
                <label class="flex items-center gap-2" :class="!tlsSettings.enabled && 'opacity-50'"><input v-model="tlsSettings.skipVerify" type="checkbox" :disabled="!tlsSettings.enabled" /> Skip certificate verification</label>
              </div>
            </div>
          </section>

          <!-- promql query -->
          <section v-if="isPromQL" class="rounded border border-border bg-surface shadow-card">
            <div class="px-4 pt-[13px]">
              <h2 class="text-[13px] font-semibold">PromQL query</h2>
              <p class="mt-[2px] text-[12px] text-ink-3">Runs against the Prometheus instance. Assert a threshold with a <span class="font-mono">[RESULT]</span> condition; with none, up = the query returns a value.</p>
            </div>
            <div class="flex flex-col gap-3 px-4 pb-4 pt-[14px]">
              <input v-model="pg.query" type="text" placeholder="up{job=&quot;api&quot;}" :class="[inputCls, 'font-mono text-[13px]']" />
              <label class="flex flex-col gap-[6px]">
                <span class="text-[12px] font-semibold text-ink-2">Authentication</span>
                <select v-model="promqlAuth" data-testid="promql-auth-mode" :class="[selectCls, 'h-[38px] w-[260px]']">
                  <option value="none">None (Prometheus is reachable without it)</option>
                  <option value="basic">Basic auth</option>
                </select>
              </label>
              <div v-if="promqlAuth === 'basic'" class="grid grid-cols-2 gap-3 max-[560px]:grid-cols-1">
                <label class="flex flex-col gap-[6px]">
                  <span class="text-[12px] font-semibold text-ink-2">Username</span>
                  <input v-model="pg.username" data-testid="promql-username" type="text" placeholder="scanner" :class="[inputCls, 'font-mono text-[13px]']" />
                </label>
              <div class="flex flex-col gap-[6px]">
                <span class="text-[12px] font-semibold text-ink-2">Credential</span>
                <div class="flex gap-3 text-[12px]"><label><input v-model="credentialMode" type="radio" value="value" /> Value</label><label :class="secretFeatureDisabled && 'opacity-50'"><input v-model="credentialMode" type="radio" value="ref" :disabled="secretFeatureDisabled" /> Secret reference</label></div>
                <select v-if="credentialMode === 'ref'" v-model="secretRef" data-testid="monitor-secret-ref" :class="[selectCls, 'h-[38px]']"><option value="" disabled>Select a project secret</option><option v-for="s in projectSecrets" :key="s.id" :value="s.name">{{ s.name }}</option></select>
                <p v-if="danglingSecretRef" data-testid="monitor-secret-ref-missing" class="mt-1 text-[12px] text-down">Secret <span class="font-mono">{{ secretRef }}</span> no longer exists in this project. This monitor cannot dispatch until you pick an existing secret.</p>
                <input v-else v-model="pg.password" type="password" :placeholder="isEdit && initialCredentialMode === 'value' ? '•••••• (unchanged)' : ''" autocomplete="new-password" :class="[inputCls, 'font-mono text-[13px]']" />
              </div>
              </div>
            </div>
          </section>

          <!-- rabbitmq mode + management auth -->
          <section v-if="isRabbitMQ" class="rounded border border-border bg-surface shadow-card">
            <div class="px-4 pt-[13px]">
              <h2 class="text-[13px] font-semibold">RabbitMQ check</h2>
              <p class="mt-[2px] text-[12px] text-ink-3">AMQP handshake needs no credentials; the management API allows richer JSON assertions via <span class="font-mono">[STATUS]</span>/<span class="font-mono">[BODY]</span> conditions.</p>
            </div>
            <div class="flex flex-col gap-3 px-4 pb-4 pt-[14px]">
              <label class="flex flex-col gap-[6px]">
                <span class="text-[12px] font-semibold text-ink-2">Mode</span>
                <select v-model="rabbitMode" :class="[selectCls, 'h-[38px] w-[220px]']">
                  <option value="amqp">AMQP handshake (port 5672)</option>
                  <option value="management">Management API (port 15672)</option>
                </select>
              </label>
              <div v-if="rabbitMode === 'management'" class="grid grid-cols-2 gap-3 max-[560px]:grid-cols-1">
                <label class="flex flex-col gap-[6px]">
                  <span class="text-[12px] font-semibold text-ink-2">Username</span>
                  <input v-model="pg.username" type="text" placeholder="guest" :class="[inputCls, 'font-mono text-[13px]']" />
                </label>
                <div class="flex flex-col gap-[6px]">
                  <span class="text-[12px] font-semibold text-ink-2">Credential</span>
                  <div class="flex gap-3 text-[12px]"><label><input v-model="credentialMode" type="radio" value="value" /> Value</label><label :class="secretFeatureDisabled && 'opacity-50'"><input v-model="credentialMode" type="radio" value="ref" :disabled="secretFeatureDisabled" /> Secret reference</label></div>
                  <select v-if="credentialMode === 'ref'" v-model="secretRef" data-testid="monitor-secret-ref" :class="[selectCls, 'h-[38px]']"><option value="" disabled>Select a project secret</option><option v-for="s in projectSecrets" :key="s.id" :value="s.name">{{ s.name }}</option></select>
                  <p v-if="danglingSecretRef" data-testid="monitor-secret-ref-missing" class="mt-1 text-[12px] text-down">Secret <span class="font-mono">{{ secretRef }}</span> no longer exists in this project. This monitor cannot dispatch until you pick an existing secret.</p>
                  <input v-else v-model="pg.password" type="password" :placeholder="isEdit && initialCredentialMode === 'value' ? '•••••• (unchanged)' : ''" autocomplete="new-password" :class="[inputCls, 'font-mono text-[13px]']" />
                </div>
                <div class="col-span-2 flex items-center gap-4 text-[12.5px] max-[560px]:col-span-1">
                  <label class="flex items-center gap-2"><input v-model="tlsSettings.enabled" type="checkbox" /> TLS (verified)</label>
                  <label class="flex items-center gap-2" :class="!tlsSettings.enabled && 'opacity-50'"><input v-model="tlsSettings.skipVerify" type="checkbox" :disabled="!tlsSettings.enabled" /> Skip certificate verification</label>
                </div>
                <label class="col-span-2 flex flex-col gap-[6px] max-[560px]:col-span-1">
                  <span class="text-[12px] font-semibold text-ink-2">Path <span class="font-normal text-ink-3">· defaults to /api/overview</span></span>
                  <input v-model="rabbitPath" type="text" placeholder="/api/overview" :class="[inputCls, 'font-mono text-[13px]']" />
                </label>
              </div>
            </div>
          </section>

          <!-- synthetic scenario builder -->
          <!-- FR-029 phase F: a TYPED form, five stages plus the bindings that feed them. There is no
               JSON editor here and none on the read view — by contract, on the mock the owner approved
               (D-0224). Every refusal below is the server's rule met at the field. -->
          <section v-if="isCanary" data-testid="canary-workflow" class="rounded border border-border bg-surface shadow-card">
            <div class="flex flex-wrap items-center gap-[10px] border-b border-border px-4 py-[13px]">
              <h2 class="text-[13px] font-semibold">Workflow</h2>
              <span class="rounded-sm border border-border bg-inset px-[7px] py-[2px] font-mono text-[10.5px] text-ink-2">async_transaction_v1</span>
              <span class="flex-1"></span>
              <span class="text-[12px] text-ink-3">5 stages</span>
            </div>
            <div class="flex flex-col gap-[14px] p-4">
              <p v-if="canaryCadenceRefusal" data-testid="canary-refusal-cadence" class="rounded-[6px] border border-down bg-down-weak px-3 py-2 text-[13px] text-down">{{ canaryCadenceRefusal }}</p>

              <!-- 0 · bindings, declared once -->
              <div class="rounded-[7px] border border-border bg-surface-2/50 p-3">
                <div class="flex items-center gap-[9px]">
                  <span class="grid h-5 w-5 place-items-center rounded-full bg-accent-weak text-[11px] font-semibold text-accent">0</span>
                  <b class="text-[13px]">Secrets</b>
                  <span v-if="canary.bindings.length" data-testid="canary-binding-count" class="rounded-sm border border-accent bg-accent-weak px-[7px] py-[2px] font-mono text-[10.5px] text-accent">{{ canary.bindings.length }} of {{ CANARY_MAX_BINDINGS }}</span>
                </div>
                <p class="mt-1 text-[12px] text-ink-3">Declared once. Every position below refers to a binding by name; the project secret is named here and nowhere else, so a rename or a rotation touches one row.</p>
                <div v-for="(b, i) in canary.bindings" :key="i" data-testid="canary-binding" class="mt-2 flex flex-wrap items-start gap-2">
                  <input v-model="b.name" type="text" placeholder="upload" :class="[inputCls, 'font-mono w-[170px]']" :aria-label="'binding name ' + i" />
                  <span class="mt-2 text-[12px] text-ink-3">&rarr;</span>
                  <select v-model="b.secret" :class="[selectCls, 'h-[38px] w-[220px]']" :aria-label="'project secret ' + i">
                    <option value="">choose a project secret…</option>
                    <option v-for="s in projectSecretNames" :key="s" :value="s">{{ s }}</option>
                  </select>
                  <button type="button" class="mt-2 text-[12.5px] text-accent hover:underline" @click="canary.bindings.splice(i, 1)">Remove</button>
                  <p v-if="canaryRefusal('bindings.' + i)" :data-testid="'canary-refusal-bindings.' + i" class="w-full text-[12px] text-down">{{ canaryRefusal("bindings." + i) }}</p>
                </div>
                <button type="button" data-testid="canary-add-binding" class="mt-2 h-[26px] rounded-[6px] border border-border px-[10px] text-[12.5px]" @click="addCanaryBinding">+ Declare a binding</button>
              </div>

              <!-- 1 · submit -->
              <div class="rounded-[7px] border border-border bg-surface-2/50 p-3">
                <div class="flex items-center gap-[9px]">
                  <span class="grid h-5 w-5 place-items-center rounded-full bg-accent-weak text-[11px] font-semibold text-accent">1</span>
                  <b class="text-[13px]">Submit</b>
                </div>
                <div class="mt-2 flex flex-wrap items-start gap-2">
                  <select v-model="canary.submitKind" data-testid="canary-submit-kind" :class="[selectCls, 'h-[38px] w-[190px]']">
                    <option v-for="k in CANARY_SUBMIT_KINDS" :key="k" :value="k">{{ k }}</option>
                  </select>
                  <span :class="[inputCls, 'font-mono w-[90px] text-center leading-[26px] text-ink-3']">POST</span>
                  <input v-model="canary.submitURL" type="text" data-testid="canary-submit-url" placeholder="https://files.example.com/files/upload" :class="[inputCls, 'font-mono flex-1 min-w-[220px]']" />
                </div>
                <p v-if="canaryRefusal('submitURL')" data-testid="canary-refusal-submitURL" class="mt-1 text-[12px] text-down">{{ canaryRefusal("submitURL") }}</p>
                <div class="mt-2 flex flex-wrap gap-3">
                  <label class="flex flex-col gap-[4px]">
                    <span class="text-[12px] font-semibold text-ink-2">Submit timeout (s)</span>
                    <input v-model="canary.submitTimeout" type="text" data-testid="canary-submit-timeout" :class="[inputCls, 'font-mono w-[110px]']" />
                    <span class="text-[11.5px] text-ink-3">1–60, and within the monitor's timeout</span>
                  </label>
                  <label class="flex flex-col gap-[4px]">
                    <span class="text-[12px] font-semibold text-ink-2">Accepted status</span>
                    <input v-model="canary.acceptedStatus" type="text" data-testid="canary-accepted-status" :class="[inputCls, 'font-mono w-[130px]']" />
                    <span class="text-11.5 text-[11.5px] text-ink-3">2xx only</span>
                  </label>
                  <label v-if="canary.submitKind === 'multipart_fixture'" class="flex flex-col gap-[4px]">
                    <span class="text-[12px] font-semibold text-ink-2">Fixture</span>
                    <!-- A select over the REGISTRY, not a text box: the runner carries the bytes and
                         verifies a pinned SHA-256, so a key it does not have is not a fixture. A free
                         text box let `https://evil.example/file.wav` past the form into a 400. -->
                    <select v-model="canary.fixtureRef" data-testid="canary-fixture-ref" :class="[selectCls, 'h-[38px] w-[190px]']">
                      <option value="">choose a fixture…</option>
                      <option v-for="fx in CANARY_FIXTURES" :key="fx" :value="fx">{{ fx }}</option>
                    </select>
                    <span class="text-[11.5px] text-ink-3">a registry key the runner carries, never an upload</span>
                  </label>
                  <label v-if="canary.submitKind === 'multipart_fixture'" class="flex flex-col gap-[4px]">
                    <span class="text-[12px] font-semibold text-ink-2">File field</span>
                    <input v-model="canary.fileField" type="text" :class="[inputCls, 'font-mono w-[120px]']" />
                  </label>
                </div>
                <p v-if="canaryRefusal('submitTimeout')" data-testid="canary-refusal-submitTimeout" class="mt-1 text-[12px] text-down">{{ canaryRefusal("submitTimeout") }}</p>
                <p v-if="canaryRefusal('acceptedStatus')" data-testid="canary-refusal-acceptedStatus" class="mt-1 text-[12px] text-down">{{ canaryRefusal("acceptedStatus") }}</p>
                <p v-if="canaryRefusal('fixtureRef')" data-testid="canary-refusal-fixtureRef" class="mt-1 text-[12px] text-down">{{ canaryRefusal("fixtureRef") }}</p>

                <div class="mt-3 text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Headers</div>
                <div v-for="(h, i) in canary.submitHeaders" :key="i" data-testid="canary-submit-header" class="mt-2 flex flex-wrap items-start gap-2">
                  <input v-model="h.name" type="text" placeholder="authorization" :class="[inputCls, 'font-mono w-[200px]']" :aria-label="'submit header name ' + i" />
                  <span v-if="isCredentialHeader(h.name)" data-testid="canary-credential-header" class="mt-2 rounded-sm border border-degraded bg-degraded-weak px-[7px] py-[2px] text-[11.5px] text-degraded">credential-bearing</span>
                  <!-- D7 taught by the CONTROL: a credential-bearing header offers a binding and
                       nothing else, from the first keystroke of the name. -->
                  <select v-if="isCredentialHeader(h.name)" v-model="h.secretRef" :class="[selectCls, 'h-[38px] w-[200px]']" :aria-label="'submit header binding ' + i">
                    <option value="">choose a binding…</option>
                    <option v-for="b in canary.bindings" :key="b.name" :value="b.name">{{ b.name }}</option>
                  </select>
                  <input v-else v-model="h.value" type="text" placeholder="canary" :class="[inputCls, 'font-mono flex-1 min-w-[160px]']" :aria-label="'submit header value ' + i" />
                  <button type="button" class="mt-2 text-[12.5px] text-accent hover:underline" @click="canary.submitHeaders.splice(i, 1)">Remove</button>
                  <p v-if="canaryRefusal('submitHeaders.' + i)" :data-testid="'canary-refusal-submitHeaders.' + i" class="w-full text-[12px] text-down">{{ canaryRefusal("submitHeaders." + i) }}</p>
                </div>
                <button type="button" data-testid="canary-add-submit-header" class="mt-2 h-[26px] rounded-[6px] border border-border px-[10px] text-[12.5px]" @click="addCanaryHeader(canary.submitHeaders)">+ Header</button>

                <div class="mt-3 text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">{{ canary.submitKind === "multipart_fixture" ? "Multipart fields" : "Body" }}</div>
                <div v-for="(fRow, i) in (canary.submitKind === 'multipart_fixture' ? canary.multipartFields : canary.bodyFields)" :key="i" data-testid="canary-body-field" class="mt-2 flex flex-wrap items-start gap-2">
                  <input v-model="fRow.key" type="text" placeholder="tenant" :class="[inputCls, 'font-mono w-[170px]']" :aria-label="'field key ' + i" />
                  <input v-model="fRow.value" type="text" placeholder="canary" :class="[inputCls, 'font-mono flex-1 min-w-[140px]']" :aria-label="'field value ' + i" />
                  <select v-model="fRow.secretRef" :class="[selectCls, 'h-[38px] w-[170px]']" :aria-label="'field binding ' + i">
                    <option value="">no binding</option>
                    <option v-for="b in canary.bindings" :key="b.name" :value="b.name">{{ b.name }}</option>
                  </select>
                  <button type="button" class="mt-2 text-[12.5px] text-accent hover:underline" @click="(canary.submitKind === 'multipart_fixture' ? canary.multipartFields : canary.bodyFields).splice(i, 1)">Remove</button>
                  <p v-if="canaryRefusal((canary.submitKind === 'multipart_fixture' ? 'multipartFields.' : 'bodyFields.') + i)" :data-testid="'canary-refusal-field-' + i" class="w-full text-[12px] text-down">{{ canaryRefusal((canary.submitKind === "multipart_fixture" ? "multipartFields." : "bodyFields.") + i) }}</p>
                </div>
                <button type="button" data-testid="canary-add-body-field" class="mt-2 h-[26px] rounded-[6px] border border-border px-[10px] text-[12.5px]" @click="addCanaryField(canary.submitKind === 'multipart_fixture' ? canary.multipartFields : canary.bodyFields)">+ Field</button>
                <p v-if="canaryRefusal('bodyFields')" data-testid="canary-refusal-bodyFields" class="mt-1 text-[12px] text-down">{{ canaryRefusal("bodyFields") }}</p>
                <!-- The D3a residual, stated where it applies rather than implied by silence. -->
                <p class="mt-2 rounded-[6px] border border-dashed border-border-strong bg-surface-2 px-3 py-2 text-[12px] text-ink-2">A credential pasted into an ordinary field or a header nobody would call a credential header is <b>not detectable</b> and is not refused. Only the finite credential-bearing header set is enforced.</p>
              </div>

              <!-- 2 · correlate -->
              <div class="rounded-[7px] border border-border bg-surface-2/50 p-3">
                <div class="flex items-center gap-[9px]">
                  <span class="grid h-5 w-5 place-items-center rounded-full bg-accent-weak text-[11px] font-semibold text-accent">2</span>
                  <b class="text-[13px]">Correlate</b>
                </div>
                <div class="mt-2 flex flex-wrap items-start gap-2">
                  <select v-model="canary.correlateSource" data-testid="canary-correlate-source" :class="[selectCls, 'h-[38px] w-[190px]']">
                    <option v-for="k in CANARY_CORRELATE_SOURCES" :key="k" :value="k">{{ k }}</option>
                  </select>
                  <input v-if="canary.correlateSource === 'response_json'" v-model="canary.correlatePath" type="text" data-testid="canary-correlate-path" placeholder="task_id" :class="[inputCls, 'font-mono flex-1 min-w-[200px]']" />
                  <input v-else v-model="canary.correlateHeaderName" type="text" data-testid="canary-correlate-header" placeholder="task-id" :class="[inputCls, 'font-mono flex-1 min-w-[200px]']" />
                </div>
                <p class="mt-1 text-[12px] text-ink-3">Dotted keys and numeric indices only — no expressions.</p>
                <p v-if="canaryRefusal('correlatePath')" data-testid="canary-refusal-correlatePath" class="mt-1 text-[12px] text-down">{{ canaryRefusal("correlatePath") }}</p>
                <p v-if="canaryRefusal('correlateHeaderName')" class="mt-1 text-[12px] text-down">{{ canaryRefusal("correlateHeaderName") }}</p>
              </div>

              <!-- 3 · completion -->
              <div class="rounded-[7px] border border-border bg-surface-2/50 p-3">
                <div class="flex items-center gap-[9px]">
                  <span class="grid h-5 w-5 place-items-center rounded-full bg-accent-weak text-[11px] font-semibold text-accent">3</span>
                  <b class="text-[13px]">Completion</b>
                </div>
                <div class="mt-2 flex flex-wrap items-start gap-2">
                  <select v-model="canary.completionKind" data-testid="canary-completion-kind" :class="[selectCls, 'h-[38px] w-[150px]']">
                    <option v-for="k in CANARY_COMPLETION_KINDS" :key="k" :value="k">{{ k }}</option>
                  </select>
                  <input v-model="canary.completionURL" type="text" data-testid="canary-completion-url" :placeholder="'https://files.example.com/tasks/' + CANARY_CORRELATION_PLACEHOLDER + '/events'" :class="[inputCls, 'font-mono flex-1 min-w-[220px]']" />
                  <label class="flex flex-col gap-[4px]">
                    <span class="text-[12px] font-semibold text-ink-2">Timeout (s)</span>
                    <input v-model="canary.completionTimeout" type="text" data-testid="canary-completion-timeout" :class="[inputCls, 'font-mono w-[110px]']" />
                  </label>
                </div>
                <p class="mt-1 text-[12px] text-ink-3"><span class="font-mono">{{ CANARY_CORRELATION_PLACEHOLDER }}</span> is legal here and nowhere else — nothing has produced an id before this stage. The timeout must fit inside the monitor's.</p>
                <p v-if="canaryRefusal('completionURL')" data-testid="canary-refusal-completionURL" class="mt-1 text-[12px] text-down">{{ canaryRefusal("completionURL") }}</p>
                <p v-if="canaryRefusal('completionTimeout')" data-testid="canary-refusal-completionTimeout" class="mt-1 text-[12px] text-down">{{ canaryRefusal("completionTimeout") }}</p>

                <div class="mt-3 text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Headers <span class="font-normal normal-case tracking-normal">— completion never inherits submit's</span></div>
                <div v-for="(h, i) in canary.completionHeaders" :key="i" data-testid="canary-completion-header" class="mt-2 flex flex-wrap items-start gap-2">
                  <input v-model="h.name" type="text" placeholder="authorization" :class="[inputCls, 'font-mono w-[200px]']" :aria-label="'completion header name ' + i" />
                  <select v-if="isCredentialHeader(h.name)" v-model="h.secretRef" :class="[selectCls, 'h-[38px] w-[200px]']" :aria-label="'completion header binding ' + i">
                    <option value="">choose a binding…</option>
                    <option v-for="b in canary.bindings" :key="b.name" :value="b.name">{{ b.name }}</option>
                  </select>
                  <input v-else v-model="h.value" type="text" :class="[inputCls, 'font-mono flex-1 min-w-[160px]']" :aria-label="'completion header value ' + i" />
                  <button type="button" class="mt-2 text-[12.5px] text-accent hover:underline" @click="canary.completionHeaders.splice(i, 1)">Remove</button>
                  <p v-if="canaryRefusal('completionHeaders.' + i)" class="w-full text-[12px] text-down">{{ canaryRefusal("completionHeaders." + i) }}</p>
                </div>
                <button type="button" data-testid="canary-add-completion-header" class="mt-2 h-[26px] rounded-[6px] border border-border px-[10px] text-[12.5px]" @click="addCanaryHeader(canary.completionHeaders)">+ Header</button>
                <p v-if="canaryRefusal('completionHeaders')" data-testid="canary-refusal-completionHeaders" class="mt-1 text-[12px] text-down">{{ canaryRefusal("completionHeaders") }}</p>

                <template v-if="canary.completionKind === 'sse'">
                  <div class="mt-3 flex flex-wrap gap-3">
                    <label class="flex flex-col gap-[4px]">
                      <span class="text-[12px] font-semibold text-ink-2">Success event</span>
                      <input v-model="canary.sseSuccessEvent" type="text" data-testid="canary-sse-success" placeholder="task.completed" :class="[inputCls, 'font-mono w-[180px]']" />
                    </label>
                    <label class="flex flex-col gap-[4px]">
                      <span class="text-[12px] font-semibold text-ink-2">Failure events</span>
                      <input v-model="canary.sseFailureEvents" type="text" placeholder="task.failed" :class="[inputCls, 'font-mono w-[180px]']" />
                    </label>
                    <label class="flex flex-col gap-[4px]">
                      <span class="text-[12px] font-semibold text-ink-2">Required fields</span>
                      <input v-model="canary.sseRequiredFields" type="text" placeholder="s3_path, byte_size" :class="[inputCls, 'font-mono w-[220px]']" />
                    </label>
                  </div>
                  <p v-if="canaryRefusal('sseSuccessEvent')" data-testid="canary-refusal-sseSuccessEvent" class="mt-1 text-[12px] text-down">{{ canaryRefusal("sseSuccessEvent") }}</p>
                  <p v-if="canaryRefusal('sseRequiredFields')" data-testid="canary-refusal-sseRequiredFields" class="mt-1 text-[12px] text-down">{{ canaryRefusal("sseRequiredFields") }}</p>
                  <p v-if="canaryRefusal('sseFailureEvents')" class="mt-1 text-[12px] text-down">{{ canaryRefusal("sseFailureEvents") }}</p>
                </template>
                <template v-else>
                  <div class="mt-3 flex flex-wrap gap-3">
                    <label class="flex flex-col gap-[4px]">
                      <span class="text-[12px] font-semibold text-ink-2">Interval (s)</span>
                      <input v-model="canary.pollInterval" type="text" data-testid="canary-poll-interval" :class="[inputCls, 'font-mono w-[100px]']" />
                    </label>
                    <label class="flex flex-col gap-[4px]">
                      <span class="text-[12px] font-semibold text-ink-2">Max attempts</span>
                      <input v-model="canary.pollMaxAttempts" type="text" data-testid="canary-poll-attempts" :class="[inputCls, 'font-mono w-[120px]']" />
                    </label>
                    <label class="flex flex-col gap-[4px]">
                      <span class="text-[12px] font-semibold text-ink-2">Success path</span>
                      <input v-model="canary.pollSuccessPath" type="text" :class="[inputCls, 'font-mono w-[150px]']" />
                    </label>
                    <label class="flex flex-col gap-[4px]">
                      <span class="text-[12px] font-semibold text-ink-2">Success value</span>
                      <input v-model="canary.pollSuccessValue" type="text" :class="[inputCls, 'font-mono w-[150px]']" />
                    </label>
                  </div>
                  <p class="mt-1 text-[12px] text-ink-3">Interval &times; attempts must fit inside the completion timeout.</p>
                  <p v-if="canaryRefusal('pollMaxAttempts')" data-testid="canary-refusal-pollMaxAttempts" class="mt-1 text-[12px] text-down">{{ canaryRefusal("pollMaxAttempts") }}</p>
                  <p v-if="canaryRefusal('pollInterval')" class="mt-1 text-[12px] text-down">{{ canaryRefusal("pollInterval") }}</p>
                  <p v-if="canaryRefusal('pollFailurePath')" data-testid="canary-refusal-pollFailurePath" class="mt-1 text-[12px] text-down">{{ canaryRefusal("pollFailurePath") }}</p>
                  <p v-if="canaryRefusal('pollFailureValues')" data-testid="canary-refusal-pollFailureValues" class="mt-1 text-[12px] text-down">{{ canaryRefusal("pollFailureValues") }}</p>
                </template>
              </div>

              <!-- 4 · result -->
              <div class="rounded-[7px] border border-border bg-surface-2/50 p-3">
                <div class="flex items-center gap-[9px]">
                  <span class="grid h-5 w-5 place-items-center rounded-full bg-accent-weak text-[11px] font-semibold text-accent">4</span>
                  <b class="text-[13px]">Result</b>
                </div>
                <div class="mt-2 flex flex-wrap gap-3">
                  <label class="flex flex-col gap-[4px]">
                    <span class="text-[12px] font-semibold text-ink-2">Max latency (s)</span>
                    <input v-model="canary.maxLatency" type="text" data-testid="canary-max-latency" :class="[inputCls, 'font-mono w-[120px]']" />
                    <span class="text-[11.5px] text-ink-3">the promise; the monitor timeout is the limit</span>
                  </label>
                  <label class="flex flex-1 flex-col gap-[4px]">
                    <span class="text-[12px] font-semibold text-ink-2">Required JSON fields</span>
                    <input v-model="canary.resultRequiredFields" type="text" data-testid="canary-required-fields" placeholder="s3_path, byte_size, media_type" :class="[inputCls, 'font-mono min-w-[220px]']" />
                  </label>
                  <label class="flex flex-col gap-[4px]">
                    <span class="text-[12px] font-semibold text-ink-2">Lifecycle path</span>
                    <input v-model="canary.lifecyclePath" type="text" data-testid="canary-lifecycle-path" placeholder="s3_path" :class="[inputCls, 'font-mono w-[150px]']" />
                  </label>
                </div>
                <p v-if="canaryRefusal('maxLatency')" data-testid="canary-refusal-maxLatency" class="mt-1 text-[12px] text-down">{{ canaryRefusal("maxLatency") }}</p>
                <p v-if="canaryRefusal('resultRequiredFields')" data-testid="canary-refusal-resultRequiredFields" class="mt-1 text-[12px] text-down">{{ canaryRefusal("resultRequiredFields") }}</p>
                <p v-if="canaryRefusal('lifecyclePath')" data-testid="canary-refusal-lifecyclePath" class="mt-1 text-[12px] text-down">{{ canaryRefusal("lifecyclePath") }}</p>
              </div>

              <!-- 5 · cleanup -->
              <div class="rounded-[7px] border border-border bg-surface-2/50 p-3">
                <div class="flex items-center gap-[9px]">
                  <span class="grid h-5 w-5 place-items-center rounded-full bg-accent-weak text-[11px] font-semibold text-accent">5</span>
                  <b class="text-[13px]">Cleanup</b>
                </div>
                <div class="mt-2 flex flex-wrap items-start gap-2">
                  <select v-model="canary.cleanupKind" data-testid="canary-cleanup-kind" :class="[selectCls, 'h-[38px] w-[190px]']">
                    <option v-for="k in CANARY_CLEANUP_KINDS" :key="k" :value="k">{{ k }}</option>
                  </select>
                  <input v-if="canary.cleanupKind === 'lifecycle_prefix'" v-model="canary.cleanupPrefix" type="text" data-testid="canary-cleanup-prefix" placeholder="canary/" :class="[inputCls, 'font-mono flex-1 min-w-[160px]']" />
                  <label v-else class="mt-2 flex items-center gap-2 text-[13px]">
                    <input v-model="canary.cleanupAcknowledged" type="checkbox" data-testid="canary-cleanup-ack" />
                    <span>I accept that nothing sweeps what this canary creates</span>
                  </label>
                </div>
                <p class="mt-2 rounded-[6px] border border-dashed border-border-strong bg-surface-2 px-3 py-2 text-[12px] text-ink-2">Validation only. cerbix has no rights on the object store and <b>never deletes what it did not create</b> — this checks that the returned path begins with the prefix. Reaping is the operator's policy on the target side.</p>
                <p v-if="canaryRefusal('cleanupPrefix')" data-testid="canary-refusal-cleanupPrefix" class="mt-1 text-[12px] text-down">{{ canaryRefusal("cleanupPrefix") }}</p>
                <p v-if="canaryRefusal('cleanupAcknowledged')" data-testid="canary-refusal-cleanupAcknowledged" class="mt-1 text-[12px] text-down">{{ canaryRefusal("cleanupAcknowledged") }}</p>
              </div>
            </div>
          </section>

          <section v-if="isSynthetic && scenarioWithheld" data-testid="scenario-withheld" class="rounded border border-border bg-surface p-4 text-[13px] text-ink-2 shadow-card">
            The scenario is not shown: it can carry credentials, so it is returned only to someone who may edit this
            monitor. Ask for editor rights on this project to view or change it.
          </section>
          <!-- FR-028: binding → project secret, always paired and never a binding name alone -->
          <section v-if="isSynthetic && !scenarioWithheld" data-testid="scenario-secrets" class="rounded border border-border bg-surface shadow-card">
            <div class="flex flex-wrap items-center gap-[10px] px-4 pt-[13px]">
              <h2 class="text-[13px] font-semibold">Scenario secrets</h2>
              <span v-if="scenarioBindings.length" class="rounded-sm border border-accent bg-accent-weak px-[7px] py-[2px] font-mono text-[10.5px] text-accent">{{ scenarioBindings.length }} of {{ MAX_SCENARIO_BINDINGS }} bindings</span>
              <span class="flex-1"></span>
              <RouterLink :to="{ name: 'settings' }" class="text-[12px] text-accent hover:underline">Manage project secrets</RouterLink>
            </div>
            <div class="flex flex-col gap-3 p-4">
              <p class="text-[12px] text-ink-3">
                A credential in a scenario is a named binding: the step carries <code v-pre>{{secret:name}}</code>, and the
                project secret's NAME is stored beside it. The value is resolved when a check is dispatched — it never
                reaches this page, and rotating the secret needs no edit here.
              </p>

              <div v-for="(b, bi) in scenarioBindings" :key="bi" data-testid="scenario-binding" class="rounded-sm border border-border bg-surface-2/50 p-3">
                <div class="flex flex-wrap items-center gap-2">
                  <span class="inline-flex items-center gap-[6px] rounded-sm border border-accent bg-accent-weak px-2 py-[2px] font-mono text-[11.5px] text-accent">
                    <span class="opacity-70">binding</span>{{ b.name }}
                  </span>
                  <span class="text-[12px] text-ink-3">→</span>
                  <select v-model="b.secret" :class="[selectCls, 'h-[30px] max-w-[260px]']" data-testid="scenario-binding-secret">
                    <option value="" disabled>Select a project secret</option>
                    <option v-for="s in projectSecrets" :key="s.id" :value="s.name">{{ s.name }}</option>
                  </select>
                  <span class="flex-1"></span>
                  <button type="button" class="text-[11.5px] text-ink-3 hover:text-down" @click="removeBinding(bi)">remove</button>
                </div>
                <div class="mt-[6px] flex flex-wrap items-center gap-[6px] text-[11.5px] text-ink-3">
                  <span>used in</span>
                  <span v-for="(u, ui) in bindingUses(b.name)" :key="ui" class="rounded-sm border border-border bg-surface px-[6px] py-[1px] font-mono text-[10.5px]">{{ u }}</span>
                  <span v-if="!bindingUses(b.name).length" class="italic">nowhere yet</span>
                  <span class="flex-1"></span>
                  <span class="font-mono text-[10.5px]">scenario_secret_{{ b.name }}_ref</span>
                </div>
                <p v-if="bindingIssues.bindingErrors[b.name]" class="mt-[6px] text-[12px] text-down">{{ bindingIssues.bindingErrors[b.name] }}</p>
              </div>

              <div v-if="newBinding" class="rounded-sm border border-border-strong bg-surface-2 p-3">
                <div class="mb-[9px] text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">New binding</div>
                <div class="grid grid-cols-2 gap-3 max-[560px]:grid-cols-1">
                  <label class="flex flex-col gap-[5px]">
                    <span class="text-[12px] font-semibold text-ink-2">Project secret</span>
                    <select v-model="newBinding.secret" :class="[selectCls, 'h-[34px]']" data-testid="new-binding-secret">
                      <option value="" disabled>Select a project secret</option>
                      <option v-for="s in projectSecrets" :key="s.id" :value="s.name">{{ s.name }}</option>
                    </select>
                  </label>
                  <label class="flex flex-col gap-[5px]">
                    <span class="text-[12px] font-semibold text-ink-2">Binding name</span>
                    <input v-model="newBinding.name" data-testid="new-binding-name" placeholder="login" class="h-[34px] rounded-sm border border-border bg-surface px-2 font-mono text-[12.5px]" />
                    <span class="text-[11.5px] text-ink-3">lower-case letters, digits, <code>_</code> and <code>-</code>, starting with a letter</span>
                  </label>
                </div>
                <div class="mt-[10px] flex flex-wrap items-center gap-[10px]">
                  <button type="button" data-testid="add-binding" class="rounded-sm bg-accent px-3 py-[6px] text-[12.5px] font-semibold text-accent-ink hover:bg-accent-2 disabled:opacity-50" :disabled="!newBinding.name.trim() || !newBinding.secret.trim()" @click="addBinding">Add binding</button>
                  <button type="button" class="text-[12.5px] text-ink-3 hover:text-ink" @click="newBinding = null">Cancel</button>
                  <span v-if="newBinding.name.trim()" class="font-mono text-[11px] text-ink-3">the step gets {{ bindingPlaceholder(newBinding.name.trim()) }}</span>
                </div>
              </div>
              <button v-else type="button" data-testid="new-binding" class="self-start rounded-sm border border-border bg-surface px-3 py-[7px] text-[12.5px] font-medium text-ink-2 hover:border-border-strong" @click="newBinding = { name: '', secret: '' }">+ binding</button>

              <p v-for="(e, ei) in bindingIssues.errors" :key="ei" class="text-[12.5px] text-down">{{ e }}</p>
            </div>
          </section>

          <section v-if="isSynthetic && !scenarioWithheld" class="rounded border border-border bg-surface shadow-card">
            <div class="px-4 pt-[13px]"><h2 class="text-[13px] font-semibold">Scenario</h2></div>
            <div class="flex flex-col gap-3 p-4">
              <p class="text-[12px] text-ink-3">
                Ordered HTTP steps sharing a variable context: <code>extract</code> pulls a value into a var, later steps interpolate it as
                <code v-pre>{{var}}</code>; each step's asserts must pass. The whole scenario runs within the timeout below.
              </p>
              <div v-for="(st, si) in scenarioSteps" :key="si" class="rounded-sm border border-border bg-surface-2/50 p-3">
                <div class="flex items-center gap-2">
                  <span class="grid h-[20px] w-[20px] place-items-center rounded-full bg-accent-weak text-[11px] font-semibold text-accent">{{ si + 1 }}</span>
                  <input v-model="st.name" placeholder="step name (optional)" class="h-[30px] flex-1 rounded-sm border border-border bg-surface px-2 text-[13px]" />
                  <button type="button" class="text-[11.5px] text-ink-3 hover:text-down" @click="removeStep(si)">remove</button>
                </div>
                <div class="mt-2 flex gap-2">
                  <select v-model="st.method" :class="selectCls"><option v-for="m in synMethods" :key="m" :value="m">{{ m }}</option></select>
                  <input v-model="st.url" placeholder="https://api.internal/…  (supports {{var}})" class="h-[34px] flex-1 rounded-sm border border-border bg-surface px-2 font-mono text-[12.5px]" :class="bindingIssues.urlErrors[si] && 'border-down bg-down-weak'" />
                </div>
                <p v-if="bindingIssues.urlErrors[si]" class="mt-[3px] text-[12px] text-down">{{ bindingIssues.urlErrors[si] }}</p>
                <!-- headers -->
                <div class="mt-2">
                  <div v-for="(hd, hi) in st.headers" :key="hi" class="mb-1">
                    <div class="flex items-center gap-2">
                      <input v-model="hd.k" placeholder="Header" class="h-[30px] w-[38%] rounded-sm border border-border bg-surface px-2 text-[12.5px]" />
                      <!-- A credential-bearing header stops being free text: D7 is taught by the
                           control, not by a refusal after saving. -->
                      <!-- Once the header NAME is credential-bearing the value is a binding and
                           nothing else, so the control is a selector even before the first binding
                           exists: it is then empty and disabled, and says what to do. Rendering a
                           free-text box until a binding existed invited exactly the literal D7
                           refuses (party finding, P1). -->
                      <select
                        v-if="isSecretCapableHeader(hd.k)"
                        :value="headerBindingName(hd.v)"
                        data-testid="header-binding"
                        :disabled="!scenarioBindings.length"
                        :class="[selectCls, 'h-[30px] flex-1', !scenarioBindings.length && 'opacity-60']"
                        @change="useBindingInHeader(si, hi, ($event.target as HTMLSelectElement).value)"
                      >
                        <option v-if="!scenarioBindings.length" value="">Add a binding first</option>
                        <option v-else value="" disabled>Select a binding</option>
                        <option v-for="b in scenarioBindings" :key="b.name" :value="b.name">{{ b.name }} → {{ b.secret || "no secret" }}</option>
                      </select>
                      <input v-else v-model="hd.v" placeholder="value (supports {{var}})" class="h-[30px] flex-1 rounded-sm border border-border bg-surface px-2 font-mono text-[12.5px]" :class="bindingIssues.headerErrors[si + ':' + hi] && 'border-down bg-down-weak'" />
                      <button type="button" class="text-[11.5px] text-ink-3 hover:text-down" @click="st.headers.splice(hi, 1)">×</button>
                    </div>
                    <p v-if="bindingIssues.headerErrors[si + ':' + hi]" class="mt-[3px] text-[12px] text-down">{{ bindingIssues.headerErrors[si + ':' + hi] }}</p>
                    <p v-else-if="isSecretCapableHeader(hd.k) && !scenarioBindings.length" class="mt-[3px] text-[12px] text-ink-3">
                      <code>{{ hd.k.trim().toLowerCase() }}</code> is a credential-bearing header: its value must be a binding, not a token. Declare one in <b>Scenario secrets</b> above.
                    </p>
                    <!-- The residual, said rather than implied: cerbix cannot tell a credential
                         from data here, so nothing refuses it (D7). -->
                    <p v-else-if="bindingIssues.residualHints[si + ':' + hi]" class="mt-[3px] text-[12px] text-degraded">{{ bindingIssues.residualHints[si + ':' + hi] }}</p>
                  </div>
                  <button type="button" class="text-[12px] text-accent hover:underline" @click="st.headers.push({ k: '', v: '' })">+ header</button>
                </div>
                <textarea v-if="['POST','PUT','PATCH'].includes(st.method)" v-model="st.body" rows="2" placeholder="request body (supports {{var}})" class="mt-2 w-full rounded-sm border border-border bg-surface px-2 py-1 font-mono text-[12px]"></textarea>
                <!-- extracts -->
                <div class="mt-2">
                  <div class="mb-1 text-[11.5px] font-medium uppercase tracking-[0.05em] text-ink-3">Extract → variables</div>
                  <div v-for="(ex, ei) in st.extract" :key="ei" class="mb-1 flex items-center gap-2 text-[12.5px]">
                    <input v-model="ex.var" placeholder="var" class="h-[30px] w-[22%] rounded-sm border border-border bg-surface px-2 font-mono" />
                    <span class="text-ink-3">=</span>
                    <select v-model="ex.from" :class="selectCls"><option v-for="f in synFroms" :key="f" :value="f">{{ f }}</option></select>
                    <input v-if="ex.from === 'json' || ex.from === 'header'" v-model="ex.path" :placeholder="ex.from === 'json' ? 'data.token' : 'Header-Name'" class="h-[30px] flex-1 rounded-sm border border-border bg-surface px-2 font-mono" />
                    <button type="button" class="text-[11.5px] text-ink-3 hover:text-down" @click="st.extract.splice(ei, 1)">×</button>
                  </div>
                  <button type="button" class="text-[12px] text-accent hover:underline" @click="st.extract.push({ var: '', from: 'json', path: '' })">+ extract</button>
                </div>
                <!-- asserts -->
                <div class="mt-2">
                  <div class="mb-1 text-[11.5px] font-medium uppercase tracking-[0.05em] text-ink-3">Assert</div>
                  <div v-for="(av, ai) in st.assert" :key="ai" class="mb-1 flex items-center gap-2 text-[12.5px]">
                    <select v-model="av.that" :class="selectCls"><option v-for="w in synThats" :key="w" :value="w">{{ w }}</option></select>
                    <input v-if="av.that === 'json'" v-model="av.path" placeholder="ok" class="h-[30px] w-[24%] rounded-sm border border-border bg-surface px-2 font-mono" />
                    <select v-if="av.that !== 'body_contains'" v-model="av.op" :class="selectCls"><option v-for="o in synOps" :key="o" :value="o">{{ o }}</option></select>
                    <input v-model="av.value" placeholder="value" class="h-[30px] flex-1 rounded-sm border border-border bg-surface px-2 font-mono" />
                    <button type="button" class="text-[11.5px] text-ink-3 hover:text-down" @click="st.assert.splice(ai, 1)">×</button>
                  </div>
                  <button type="button" class="text-[12px] text-accent hover:underline" @click="st.assert.push({ that: 'status', op: 'eq', value: '200', path: '' })">+ assert</button>
                </div>
              </div>
              <button type="button" class="self-start rounded-sm border border-border bg-surface px-3 py-[7px] text-[12.5px] font-medium text-ink-2 hover:border-border-strong" @click="addStep">+ step</button>
              <p v-if="scenarioError" class="text-[12.5px] text-down">{{ scenarioError }}</p>
            </div>
          </section>

          <!-- schedule -->
          <section class="rounded border border-border bg-surface shadow-card">
            <div class="px-4 pt-[13px]"><h2 class="text-[13px] font-semibold">{{ schedTitle }}</h2></div>
            <div class="px-4 pb-4 pt-[14px]">
              <label v-if="isComposite" class="flex flex-col gap-[6px]">
                <span class="text-[12px] font-semibold text-ink-2">Re-evaluate every</span>
                <span class="relative block max-w-[220px]">
                  <input v-model.number="form.interval_seconds" type="number" min="5" :class="[inputCls, 'pr-[38px] font-mono']" />
                  <span class="pointer-events-none absolute right-[11px] top-1/2 -translate-y-1/2 font-mono text-[12px] text-ink-3">sec</span>
                </span>
                <span class="text-[11.5px] text-ink-3">The group's status is recomputed from its members on this cadence.</span>
              </label>
              <div v-else-if="form.type !== 'push'" class="grid grid-cols-3 gap-3 max-[560px]:grid-cols-1">
                <label class="flex flex-col gap-[6px]">
                  <span class="text-[12px] font-semibold text-ink-2">Interval</span>
                  <span class="relative">
                    <input v-model.number="form.interval_seconds" type="number" min="5" data-testid="monitor-interval" :class="[inputCls, 'pr-[38px] font-mono']" />
                    <span class="pointer-events-none absolute right-[11px] top-1/2 -translate-y-1/2 font-mono text-[12px] text-ink-3">sec</span>
                  </span>
                </label>
                <label class="flex flex-col gap-[6px]">
                  <span class="text-[12px] font-semibold text-ink-2">Timeout</span>
                  <span class="relative">
                    <input v-model.number="form.timeout_seconds" type="number" min="1" data-testid="monitor-timeout" :class="[inputCls, 'pr-[38px] font-mono']" />
                    <span class="pointer-events-none absolute right-[11px] top-1/2 -translate-y-1/2 font-mono text-[12px] text-ink-3">sec</span>
                  </span>
                </label>
                <label class="flex flex-col gap-[6px]">
                  <span class="text-[12px] font-semibold text-ink-2">Retries</span>
                  <span class="relative">
                    <input v-model.number="form.retries" type="number" min="0" :class="[inputCls, 'pr-[38px] font-mono']" />
                    <span class="pointer-events-none absolute right-[11px] top-1/2 -translate-y-1/2 font-mono text-[12px] text-ink-3">×</span>
                  </span>
                </label>
                <label class="flex flex-col gap-[6px]">
                  <span class="text-[12px] font-semibold text-ink-2">Confirmations</span>
                  <span class="relative">
                    <input v-model.number="form.failure_threshold" type="number" min="1" :class="[inputCls, 'pr-[44px] font-mono']" />
                    <span class="pointer-events-none absolute right-[11px] top-1/2 -translate-y-1/2 font-mono text-[12px] text-ink-3">fail</span>
                  </span>
                  <span class="text-[11.5px] text-ink-3">Consecutive failed checks before "down" (and alerting).</span>
                </label>
                <label class="flex flex-col gap-[6px]" :class="form.failure_threshold > 1 ? '' : 'opacity-50'">
                  <span class="text-[12px] font-semibold text-ink-2">Confirm interval</span>
                  <span class="relative">
                    <input v-model.number="form.confirm_interval_seconds" type="number" min="0" :disabled="form.failure_threshold <= 1" :class="[inputCls, 'pr-[38px] font-mono']" />
                    <span class="pointer-events-none absolute right-[11px] top-1/2 -translate-y-1/2 font-mono text-[12px] text-ink-3">sec</span>
                  </span>
                  <span class="text-[11.5px] text-ink-3">Faster probes while confirming a failure. 0 = off; min 5s; needs confirmations &gt; 1.</span>
                </label>
                <label class="flex flex-col gap-[6px]">
                  <span class="text-[12px] font-semibold text-ink-2">Re-notify</span>
                  <span class="relative">
                    <input v-model.number="form.renotify_seconds" type="number" min="0" step="60" :class="[inputCls, 'pr-[38px] font-mono']" />
                    <span class="pointer-events-none absolute right-[11px] top-1/2 -translate-y-1/2 font-mono text-[12px] text-ink-3">sec</span>
                  </span>
                  <span class="text-[11.5px] text-ink-3">Re-send the alert every N sec while down (0 = off).</span>
                </label>
              </div>
              <div v-else class="grid grid-cols-2 gap-3 max-[560px]:grid-cols-1">
                <label class="flex flex-col gap-[6px]">
                  <span class="text-[12px] font-semibold text-ink-2">Expected every</span>
                  <span class="relative">
                    <input v-model.number="form.interval_seconds" type="number" min="5" :class="[inputCls, 'pr-[38px] font-mono']" />
                    <span class="pointer-events-none absolute right-[11px] top-1/2 -translate-y-1/2 font-mono text-[12px] text-ink-3">sec</span>
                  </span>
                  <span class="text-[11.5px] text-ink-3">Alert if the service does not check in within this period.</span>
                </label>
                <label class="flex flex-col gap-[6px]">
                  <span class="text-[12px] font-semibold text-ink-2">Grace period</span>
                  <span class="relative">
                    <input v-model.number="form.grace_seconds" type="number" min="0" :class="[inputCls, 'pr-[38px] font-mono']" />
                    <span class="pointer-events-none absolute right-[11px] top-1/2 -translate-y-1/2 font-mono text-[12px] text-ink-3">sec</span>
                  </span>
                  <span class="text-[11.5px] text-ink-3">Extra tolerance beyond the interval before marking down.</span>
                </label>
              </div>
            </div>
          </section>

          <!-- conditions -->
          <section v-if="showConditions" class="rounded border border-border bg-surface shadow-card">
            <div class="px-4 pt-[13px]">
              <h2 class="text-[13px] font-semibold">Conditions</h2>
              <p class="mt-[2px] text-[12px] text-ink-3">Up when every condition passes. No conditions → 2xx (HTTP) or connect (TCP/ICMP).</p>
            </div>
            <div class="flex flex-col gap-[14px] px-4 pb-4 pt-[14px]">
              <div class="flex flex-col gap-2">
                <div
                  v-for="(c, i) in conds"
                  :key="i"
                  class="grid grid-cols-[1.2fr_.9fr_1.3fr_auto] items-center gap-2 max-[560px]:grid-cols-[1fr_1fr]"
                >
                  <select v-model="c.p" :class="selectCls" @change="fixOps(c)">
                    <option v-for="p in placeholders" :key="p" :value="p">{{ p }}</option>
                  </select>
                  <select v-model="c.o" :class="selectCls">
                    <option v-for="o in opsFor(c.p)" :key="o" :value="o">{{ o }}</option>
                  </select>
                  <input v-model="c.v" :class="[inputCls, 'h-[34px] font-mono text-[13px]']" />
                  <button
                    type="button"
                    class="grid h-[34px] w-[34px] place-items-center rounded-sm border border-border bg-surface text-ink-3 hover:border-down hover:text-down max-[560px]:col-span-2"
                    aria-label="Remove condition"
                    @click="removeCond(i)"
                  >
                    <svg viewBox="0 0 24 24" class="h-[15px] w-[15px]" fill="none" stroke="currentColor" stroke-width="2"><path d="M6 6l12 12M18 6L6 18" /></svg>
                  </button>
                </div>
              </div>
              <button
                type="button"
                class="inline-flex h-[34px] w-fit items-center gap-[7px] rounded-sm border border-dashed border-border-strong px-3 text-[13px] text-ink-2 hover:border-accent hover:text-accent"
                @click="addCond"
              >
                <svg viewBox="0 0 24 24" class="h-[15px] w-[15px]" fill="none" stroke="currentColor" stroke-width="2.2"><path d="M12 5v14M5 12h14" /></svg>
                Add condition
              </button>
              <div>
                <div class="mb-[7px] text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-3">Assembled</div>
                <div class="flex flex-wrap gap-[6px]">
                  <span v-for="(s, i) in assembled" :key="i" class="rounded-xs border border-border bg-inset px-2 py-[3px] font-mono text-[12px] text-ink-2">{{ s }}</span>
                  <span v-if="!assembled.length" class="text-[12.5px] text-ink-3">No conditions — using the default.</span>
                </div>
              </div>
            </div>
          </section>

          <!-- group members (composite) -->
          <section v-if="isComposite" class="rounded border border-border bg-surface shadow-card">
            <div class="px-4 pt-[13px]">
              <h2 class="text-[13px] font-semibold">Group members</h2>
              <p class="mt-[2px] text-[12px] text-ink-3">Aggregate other monitors in this project.</p>
            </div>
            <div class="flex flex-col gap-[14px] px-4 pb-4 pt-[14px]">
              <div class="flex flex-col gap-[6px]">
                <span class="text-[12px] font-semibold text-ink-2">Up when</span>
                <div class="inline-flex w-fit overflow-hidden rounded-sm border border-border">
                  <button type="button" class="px-[13px] py-[6px] text-[12.5px]" :class="mode === 'all' ? 'bg-accent-weak font-medium text-accent' : 'bg-surface-2 text-ink-2'" @click="mode = 'all'">All members up</button>
                  <button type="button" class="border-l border-border px-[13px] py-[6px] text-[12.5px]" :class="mode === 'any' ? 'bg-accent-weak font-medium text-accent' : 'bg-surface-2 text-ink-2'" @click="mode = 'any'">Any member up</button>
                  <button type="button" class="border-l border-border px-[13px] py-[6px] text-[12.5px]" :class="mode === 'quorum' ? 'bg-accent-weak font-medium text-accent' : 'bg-surface-2 text-ink-2'" @click="mode = 'quorum'">Quorum</button>
                </div>
                <div v-if="mode === 'quorum'" class="flex items-center gap-[8px] text-[13px] text-ink-2">
                  Down when ≥
                  <input v-model.number="quorum" type="number" min="1" :max="childIds.size || 1" class="w-[56px] rounded-sm border border-border-strong bg-surface-2 px-2 py-[4px] text-right font-mono text-[12.5px] outline-none focus:border-accent" />
                  of <b class="font-mono">{{ childIds.size }}</b> members report down
                </div>
              </div>
              <div class="flex flex-col gap-2">
                <span class="text-[12px] font-semibold text-ink-2">Members <span class="font-mono text-ink-3">{{ childIds.size }}</span></span>
                <label
                  v-for="m in childCandidates"
                  :key="m.id"
                  class="flex cursor-pointer items-center gap-3 rounded-sm border border-border bg-surface-2 px-3 py-[9px] hover:border-border-strong"
                >
                  <input type="checkbox" class="h-4 w-4 accent-accent" :checked="childIds.has(m.id ?? '')" @change="toggleChild(m.id ?? '')" />
                  <span class="font-mono text-[13px]">{{ m.name }}</span>
                  <span class="ml-auto rounded-xs border border-border px-[6px] py-px font-mono text-[10.5px] uppercase tracking-[0.04em] text-ink-3">{{ m.type }}</span>
                </label>
                <p v-if="!childCandidates.length" class="text-[12.5px] text-ink-3">No other monitors in this project yet.</p>
              </div>
            </div>
          </section>

          <!-- notifications -->
          <section class="rounded border border-border bg-surface shadow-card">
            <div class="px-4 pt-[13px]">
              <h2 class="text-[13px] font-semibold">Notifications</h2>
              <p class="mt-[2px] text-[12px] text-ink-3">Alert these channels when the status changes.</p>
            </div>
            <div class="flex flex-col gap-2 px-4 pb-4 pt-[14px]">
              <label
                v-for="ch in channels"
                :key="ch.id"
                class="flex cursor-pointer items-center gap-3 rounded-sm border border-border bg-surface-2 px-3 py-[9px] hover:border-border-strong"
              >
                <input type="checkbox" class="h-4 w-4 accent-accent" :checked="selectedChannels.has(ch.id ?? '')" @change="toggleChannel(ch.id ?? '')" />
                <span class="text-[13px]">{{ ch.name }}</span>
                <span class="ml-auto rounded-xs border border-border px-[6px] py-px font-mono text-[10.5px] uppercase tracking-[0.04em] text-ink-3">{{ ch.type }}</span>
              </label>
              <p v-if="!channels.length" class="text-[12.5px] text-ink-3">
                No channels in this project yet — add them in
                <RouterLink :to="{ name: 'settings' }" class="text-accent hover:underline">Settings</RouterLink>.
              </p>
            </div>
          </section>

          <!-- enabled -->
          <section class="rounded border border-border bg-surface shadow-card">
            <div class="flex items-center gap-3 px-4 py-[15px]">
              <div class="flex flex-col">
                <b class="text-[13px] font-semibold">Enabled</b>
                <span class="text-[12px] text-ink-3">Start checking as soon as it's created.</span>
              </div>
              <button
                type="button"
                role="switch"
                :aria-checked="form.enabled"
                class="relative ml-auto h-[22px] w-[38px] flex-none rounded-full transition-colors"
                :class="form.enabled ? 'bg-accent' : 'bg-border-strong'"
                @click="form.enabled = !form.enabled"
              >
                <span class="absolute left-[2px] top-[2px] h-[18px] w-[18px] rounded-full bg-white transition-transform" :class="form.enabled ? 'translate-x-4' : ''"></span>
              </button>
            </div>
          </section>

          <!-- auto-incident -->
          <section class="rounded border border-border bg-surface shadow-card">
            <div class="flex items-center gap-3 px-4 py-[15px]">
              <div class="flex flex-col">
                <b class="text-[13px] font-semibold">Auto-incident</b>
                <span class="text-[12px] text-ink-3">Open an incident automatically when this monitor goes down. An already-open one still resolves on recovery even if you turn this off.</span>
              </div>
              <button
                type="button"
                role="switch"
                :aria-checked="form.auto_incident"
                class="relative ml-auto h-[22px] w-[38px] flex-none rounded-full transition-colors"
                :class="form.auto_incident ? 'bg-accent' : 'bg-border-strong'"
                @click="form.auto_incident = !form.auto_incident"
              >
                <span class="absolute left-[2px] top-[2px] h-[18px] w-[18px] rounded-full bg-white transition-transform" :class="form.auto_incident ? 'translate-x-4' : ''"></span>
              </button>
            </div>
            <div v-if="form.auto_incident" class="border-t border-border px-4 py-[13px]">
              <label class="mb-1 block text-[12px] font-medium text-ink-2">On-call escalation policy</label>
              <select v-model="form.escalation_policy_id" :class="selectCls" class="w-full">
                <option value="">Flat — notify all linked channels</option>
                <option v-for="p in escalationPolicies" :key="p.id" :value="p.id">{{ p.name }}</option>
              </select>
              <p class="mt-[6px] text-[12px] text-ink-3">
                With a policy, down alerts follow the on-call ladder (and its acknowledge/stop) instead of notifying every channel at once. Manage policies in
                <RouterLink :to="{ name: 'escalation' }" class="text-accent hover:underline">Escalation</RouterLink>.
              </p>
            </div>
            <!-- Dependency graph: while any (transitive) parent is down, this monitor's alerts stay quiet -->
            <div class="border-t border-border px-4 py-[13px]">
              <label class="mb-1 block text-[12px] font-medium text-ink-2">Depends on <span class="font-mono text-ink-3">{{ depIds.size }}</span></label>
              <div v-if="depIds.size" class="mb-2 flex flex-wrap gap-[6px]">
                <span v-for="id in depIds" :key="id" class="inline-flex items-center gap-[6px] rounded-full bg-accent-weak py-[3px] pl-[10px] pr-[6px] text-[12px] font-medium text-accent">
                  <span class="h-[6px] w-[6px] rounded-full" :class="monName(id).down ? 'bg-down' : 'bg-up'"></span>
                  {{ monName(id).name }}
                  <button type="button" class="px-[3px] opacity-70 hover:opacity-100" title="Remove" @click="toggleDep(id)">✕</button>
                </span>
              </div>
              <div class="max-h-[150px] overflow-auto rounded-sm border border-border bg-surface-2">
                <div v-if="!depCandidates.length" class="px-3 py-3 text-[12.5px] text-ink-3">No other monitors in this project yet.</div>
                <button
                  v-for="c in depCandidates" :key="c.id" type="button"
                  class="flex w-full items-center gap-[9px] px-3 py-[7px] text-left text-[12.5px] hover:bg-accent-weak disabled:cursor-not-allowed disabled:opacity-45"
                  :disabled="c.cycle"
                  :title="c.cycle ? 'Would create a dependency cycle' : ''"
                  @click="toggleDep(c.id)"
                >
                  <span class="h-[7px] w-[7px] rounded-full" :class="c.down ? 'bg-down' : 'bg-up'"></span>
                  <span class="font-mono">{{ c.name }}</span>
                  <span v-if="depIds.has(c.id)" class="text-accent">✓</span>
                  <span class="ml-auto rounded-xs border border-border px-[4px] py-px font-mono text-[9.5px] uppercase tracking-[0.03em] text-ink-3">{{ c.type }}</span>
                </button>
              </div>
              <p class="mt-[6px] text-[12px] text-ink-3">While any parent is down, this monitor's alerts are suppressed (data keeps recording). Cycles are rejected.</p>
            </div>
          </section>

          <!-- Multi-region set wizard (quorum composite over N region children) -->
          <section v-if="mrEligible" class="rounded border border-border bg-surface shadow-card">
            <div class="flex items-center gap-[10px] px-4 py-[13px]">
              <div>
                <h2 class="text-[13px] font-semibold">Multi-region set</h2>
                <p class="mt-[2px] text-[12px] text-ink-3">Probe this target from several regions; alert by quorum.</p>
              </div>
              <button type="button" class="ml-auto h-[22px] w-[38px] flex-none rounded-full transition-colors" :class="multiRegion ? 'bg-accent' : 'bg-inset'" role="switch" :aria-checked="multiRegion" @click="multiRegion = !multiRegion">
                <span class="block h-[18px] w-[18px] rounded-full bg-white transition-transform" :class="multiRegion ? 'translate-x-[18px]' : 'translate-x-[2px]'"></span>
              </button>
            </div>
            <div v-if="multiRegion" class="border-t border-border px-4 py-[13px]">
              <span class="text-[12px] font-semibold text-ink-2">Regions <span class="font-mono text-ink-3">{{ mrRegions.size }}</span></span>
              <div class="mt-2 flex flex-wrap gap-[8px]">
                <button
                  v-for="r in regions" :key="r.name" type="button"
                  class="inline-flex items-center gap-[7px] rounded-sm border px-3 py-[6px] text-[12.5px] disabled:cursor-not-allowed disabled:opacity-45"
                  :class="mrRegions.has(r.name) ? 'border-accent bg-accent-weak font-medium text-accent' : 'border-border-strong bg-surface-2 text-ink-2'"
                  :disabled="!r.live"
                  :title="r.live ? '' : 'No live worker in this region'"
                  @click="toggleMrRegion(r.name)"
                >
                  <span class="h-[6px] w-[6px] rounded-full" :class="r.live ? 'bg-up' : 'bg-pending'"></span>{{ r.name }}
                </button>
              </div>
              <div class="mt-[10px] flex items-center gap-[8px] text-[13px] text-ink-2">
                Down when ≥
                <input v-model.number="mrQuorum" type="number" min="1" :max="mrRegions.size || 1" class="w-[56px] rounded-sm border border-border-strong bg-surface-2 px-2 py-[4px] text-right font-mono text-[12.5px] outline-none focus:border-accent" />
                of <b class="font-mono">{{ mrRegions.size }}</b> regions report down
              </div>
              <p v-if="mrRegions.size >= 2" class="mt-[8px] rounded-sm border border-dashed border-border-strong bg-surface-2 px-3 py-[8px] text-[12px] text-ink-2">
                Will create: <span v-for="(r, i) in [...mrRegions]" :key="r" class="font-mono">{{ form.name || "monitor" }} @ {{ r }}<template v-if="i < mrRegions.size - 1"> · </template></span>
                (no channels) + composite <span class="font-mono">{{ form.name || "monitor" }}</span> (quorum {{ mrQuorum }}/{{ mrRegions.size }} — it carries the alerts)
              </p>
              <p class="mt-[6px] text-[11.5px] text-ink-3">The target must be reachable from every picked region. Default quorum = majority.</p>
            </div>
          </section>

          <div v-if="!writeAllowed" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] text-ink-3">You don't have permission to write in this project.</div>
          <div v-if="error" class="rounded-sm border border-down/40 bg-down-weak px-3 py-2 text-[13px] text-down">{{ error }}</div>

          <!-- test connection result -->
          <div v-if="testResult" class="flex items-start gap-[10px] rounded-sm border px-3 py-[10px] text-[13px]" :class="testResult.up ? 'border-up/40 bg-up-weak' : 'border-down/40 bg-down-weak'">
            <span class="mt-[1px] font-semibold" :class="testResult.up ? 'text-up' : 'text-down'">{{ testResult.up ? "UP" : "DOWN" }}</span>
            <span class="flex-1 text-ink-2">
              <span class="font-mono">{{ testResult.latency_ms }}ms<template v-if="testResult.code"> · code {{ testResult.code }}</template></span>
              <span v-if="testResult.msg" class="block text-[12px] text-ink-3">{{ testResult.msg }}</span>
            </span>
          </div>
          <div v-if="testError" class="rounded-sm border border-down/40 bg-down-weak px-3 py-2 text-[13px] text-down">{{ testError }}</div>
          <!-- D10 stated where the click happens: a scenario with bindings is deliberately not
               testable before it is saved, so the form says so and offers the way forward. -->
          <div v-if="testBlockedByBindings" data-testid="test-blocked" class="rounded-sm border border-border-strong bg-surface-2 px-3 py-2 text-[12.5px] text-ink-2">{{ testBlockedByBindings }}</div>
          <div v-if="isTestable && !selectedRegionLive" class="rounded-sm border border-degraded/40 bg-degraded-weak px-3 py-2 text-[12.5px] text-ink-2">
            Region <code>{{ form.region.trim() || "core" }}</code> has no connected worker — the probe runs in that region, so the test will report “no worker responded” until one is started with <code>--region {{ form.region.trim() || "core" }}</code>.
          </div>

          <div class="flex items-center justify-end gap-[10px]">
            <button
              v-if="isTestable"
              type="button"
              data-testid="test-connection"
              :disabled="testing || !writeAllowed || !!testBlockedByBindings || (showTarget && !form.target.trim())"
              class="mr-auto inline-flex h-[38px] items-center gap-[7px] rounded-sm border border-border bg-surface px-[15px] text-[13.5px] font-semibold text-ink-2 hover:border-border-strong disabled:opacity-50"
              @click="testConnection"
            >
              <svg viewBox="0 0 24 24" class="h-[15px] w-[15px]" fill="none" stroke="currentColor" stroke-width="2.2"><path d="M13 2 3 14h7l-1 8 10-12h-7z" /></svg>
              {{ testing ? "Testing…" : "Test connection" }}
            </button>
            <RouterLink :to="{ name: 'monitors' }" class="inline-flex h-[38px] items-center rounded-sm border border-border bg-surface px-[15px] text-[13.5px] font-semibold text-ink hover:border-border-strong">Cancel</RouterLink>
            <button type="submit" :disabled="!canSubmit" class="inline-flex h-[38px] items-center gap-[7px] rounded-sm bg-accent px-[15px] text-[13.5px] font-semibold text-accent-ink hover:bg-accent-2 disabled:opacity-50">
              <svg viewBox="0 0 24 24" class="h-[15px] w-[15px]" fill="none" stroke="currentColor" stroke-width="2.2"><path d="M5 12l5 5L20 7" /></svg>
              {{ submitting ? (isEdit ? "Saving…" : "Creating…") : isEdit ? "Save changes" : "Create monitor" }}
            </button>
          </div>
        </form>

        <!-- live preview -->
        <aside class="sticky top-[76px] max-[920px]:static">
          <div class="mb-[10px] text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Live preview</div>
          <div class="flex flex-col gap-3 rounded border border-border bg-surface p-4 shadow-card">
            <div class="flex items-center gap-[9px]">
              <span class="font-mono text-[13.5px] font-semibold">{{ form.name || "unnamed" }}</span>
              <span class="rounded-xs border border-border px-[5px] py-px font-mono text-[10.5px] uppercase tracking-[0.04em] text-ink-3">{{ typeChip[form.type] }}</span>
              <span class="ml-auto inline-flex h-[24px] items-center gap-[6px] rounded-full bg-pending-weak pl-[7px] pr-[9px] text-[12px] font-medium text-ink-3">
                <span class="h-[7px] w-[7px] rounded-full bg-pending"></span>Pending
              </span>
            </div>
            <div class="break-all font-mono text-[12px] text-ink-2">{{ previewTarget }}</div>
            <div class="flex flex-wrap gap-[6px]">
              <span v-for="(s, i) in previewConds" :key="i" class="rounded-xs border border-border bg-inset px-[7px] py-[2px] font-mono text-[11.5px] text-ink-2">{{ s }}</span>
            </div>
            <div class="border-t border-border pt-[10px] text-[12.5px] text-ink-3">{{ summary }}</div>
          </div>
          <p class="mt-3 text-[12px] leading-relaxed text-ink-3">
            The monitor starts <span class="font-mono">pending</span> and turns
            <span class="text-up">operational</span> or <span class="text-down">down</span> after its first check.
          </p>
        </aside>
      </div>
    </div>
  </AppShell>
</template>
