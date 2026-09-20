import type { components } from "@/api/schema";

export type OnboardingStep = "org" | "project" | "monitor" | "result";

export type OnboardingKind =
  | "loading"
  | "failed_read"
  | "fresh_no_org"
  | "org_no_project"
  | "project_no_monitor"
  | "forbidden_handoff"
  | "waiting_for_result"
  | "worker_unavailable"
  | "scheduler_unknown"
  | "first_result_up"
  | "first_result_down"
  | "complete_existing";

export type Monitor = components["schemas"]["Monitor"];
export type Heartbeat = components["schemas"]["Heartbeat"];

export interface MonitorEvidence {
  monitor: Monitor;
  heartbeat: Heartbeat | null;
}

export interface OnboardingInput {
  loading?: boolean;
  readError?: string;
  orgCount: number;
  orgId: string;
  projectId: string;
  canCreateOrg: boolean;
  canCreateProject: boolean;
  canCreateMonitor: boolean;
  evidence: MonitorEvidence[];
  candidateId?: string;
  journeyActive?: boolean;
  regionLive?: boolean;
  now?: number;
}

export interface OnboardingSnapshot {
  kind: OnboardingKind;
  current: OnboardingStep;
  done: OnboardingStep[];
  candidate: Monitor | null;
  heartbeat: Heartbeat | null;
  error: string;
  handoffStep: OnboardingStep | null;
}

const pullBasedTypes = new Set<Monitor["type"]>([
  "http",
  "tcp",
  "icmp",
  "dns",
  "tls",
  "grpc",
  "postgres",
  "mysql",
  "redis",
  "promql",
  "rabbitmq",
  "websocket",
  "ssh",
  "synthetic",
  "async_canary",
]);

function snapshot(
  kind: OnboardingKind,
  current: OnboardingStep,
  done: OnboardingStep[],
  candidate: Monitor | null = null,
  heartbeat: Heartbeat | null = null,
  error = "",
  handoffStep: OnboardingStep | null = null,
): OnboardingSnapshot {
  return { kind, current, done, candidate, heartbeat, error, handoffStep };
}

function newestCandidate(evidence: MonitorEvidence[], preferredId?: string): MonitorEvidence | null {
  const preferred = evidence.find((item) => item.monitor.id === preferredId);
  if (preferred) return preferred;
  return [...evidence].sort((a, b) => {
    const enabled = Number(b.monitor.enabled !== false) - Number(a.monitor.enabled !== false);
    if (enabled) return enabled;
    return Date.parse(b.monitor.created_at ?? "") - Date.parse(a.monitor.created_at ?? "");
  })[0] ?? null;
}

function newestObserved(evidence: MonitorEvidence[]): MonitorEvidence | null {
  return [...evidence]
    .filter((item) => item.heartbeat)
    .sort((a, b) => Date.parse(b.heartbeat?.ts ?? "") - Date.parse(a.heartbeat?.ts ?? ""))[0] ?? null;
}

function waitExpired(monitor: Monitor, now: number): boolean {
  const created = Date.parse(monitor.created_at ?? "");
  if (!Number.isFinite(created)) return false;
  const expectedSeconds = Math.max(30, (monitor.interval_seconds ?? 60) + (monitor.timeout_seconds ?? 10) + 15);
  return now - created >= expectedSeconds * 1000;
}

export function resolveOnboarding(input: OnboardingInput): OnboardingSnapshot {
  if (input.loading) return snapshot("loading", "org", []);
  if (input.readError) return snapshot("failed_read", "result", [], null, null, input.readError);
  if (!input.orgCount) {
    if (!input.canCreateOrg) return snapshot("forbidden_handoff", "org", [], null, null, "", "org");
    return snapshot("fresh_no_org", "org", []);
  }
  if (!input.orgId || !input.projectId) {
    if (!input.canCreateProject) return snapshot("forbidden_handoff", "project", ["org"], null, null, "", "project");
    return snapshot("org_no_project", "project", ["org"]);
  }
  if (!input.evidence.length) {
    if (!input.canCreateMonitor) return snapshot("forbidden_handoff", "monitor", ["org", "project"], null, null, "", "monitor");
    return snapshot("project_no_monitor", "monitor", ["org", "project"]);
  }

  const selected = newestCandidate(input.evidence, input.candidateId);
  const observed = input.journeyActive
    ? selected?.heartbeat
      ? selected
      : null
    : newestObserved(input.evidence);
  if (observed?.heartbeat) {
    const kind = input.journeyActive
      ? observed.heartbeat.up
        ? "first_result_up"
        : "first_result_down"
      : "complete_existing";
    return snapshot(kind, "result", ["org", "project", "monitor", "result"], observed.monitor, observed.heartbeat);
  }

  if (!selected) return snapshot("failed_read", "result", ["org", "project"], null, null, "No readable monitor was found.");
  const monitor = selected.monitor;
  if (pullBasedTypes.has(monitor.type) && input.regionLive === false) {
    return snapshot("worker_unavailable", "result", ["org", "project", "monitor"], monitor);
  }
  if (monitor.type !== "push" && waitExpired(monitor, input.now ?? Date.now())) {
    return snapshot("scheduler_unknown", "result", ["org", "project", "monitor"], monitor);
  }
  return snapshot("waiting_for_result", "result", ["org", "project", "monitor"], monitor);
}

export function onboardingDismissalKey(userId: string, orgId: string, projectId: string): string {
  const scope = projectId ? `project.${projectId}` : orgId ? `org.${orgId}` : "account";
  return `cerbix.onboarding.dismissed.${userId || "unknown"}.${scope}`;
}

export function isOnboardingDismissed(key: string, storage: Pick<Storage, "getItem"> = localStorage): boolean {
  try {
    return storage.getItem(key) === "1";
  } catch {
    return false;
  }
}

export function dismissOnboarding(key: string, storage: Pick<Storage, "setItem"> = localStorage): void {
  try {
    storage.setItem(key, "1");
  } catch {
    return;
  }
}

export function createdMonitorDestination(id: string, type: Monitor["type"], onboarding: boolean) {
  if (!onboarding) return { name: "monitor", params: { id } };
  if (type === "push") return { name: "monitor", params: { id }, query: { onboarding: "1" } };
  return { name: "dashboard", query: { onboarding: "1", monitor: id } };
}
