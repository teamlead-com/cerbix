import { flushPromises, mount } from "@vue/test-utils";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import IncidentDetailView from "@/views/IncidentDetailView.vue";
import { SYSTEM_NOTE_MARKERS } from "@/lib/incident";

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));
vi.mock("vue-router", () => ({
  useRoute: () => ({ params: { id: "inc1" } }),
  RouterLink: { props: ["to"], template: '<a :data-to="JSON.stringify(to)"><slot /></a>' },
}));
vi.mock("@/components/AppShell.vue", () => ({ default: { name: "AppShell", template: "<div><slot name='actions' /><slot /></div>" } }));
vi.mock("@/stores/session", () => ({ useSession: () => ({ canProjectWrite: () => true }) }));
vi.mock("@/stores/workspace", () => ({ useWorkspace: () => ({ init: () => Promise.resolve(), orgId: "o1", projectId: "p1" }) }));

// FR-025 D-0210 item 4, mock screen 4: the incident page's `Preceded by` section and the
// `🚀 Changes:` system note. What is proven:
//
//   * the section is asked for a SERVICE incident only — a monitor incident asks nothing, because
//     the correlation runs for service auto-incidents (D7);
//   * a row is the phase KNOWN AT THE OPEN with its COPIED lag: the anchor and the lag come from the
//     link, the group's phases TODAY are shown beside them and never rewrite the anchor;
//   * an upstream row is named a probable root and points at the service the change was recorded on;
//   * the row links to the comparison under the CHANGE's own service, by identity;
//   * a group with no terminal phase gets the dashed "unavailable" cell, not a comparison link;
//   * the word is `preceded`, never "caused", and the hint says the window is fixed at the open;
//   * the `🚀 Changes:` note is detected by its PREFIX (the server's own idempotency marker) and
//     rendered as a system note like `⚡ Context:`, with the change glyph in the gutter.

const OWN_SERVICE = "0191c2a4-7f3e-4c1b-9a2d-00000000000f";
const UPSTREAM_SERVICE = "0191c2a4-7f3e-4c1b-9a2d-0000000000aa";

const INCIDENT = {
  id: "inc1",
  project_id: "p1",
  title: "checkout is down",
  status: "investigating",
  impact: "major",
  source: "auto",
  started_at: "2026-08-30T14:31:00Z",
  service_id: OWN_SERVICE,
};

const phase = (id: string, name: string, at: string) => ({
  id,
  phase: name,
  occurred_at: at,
  ref: "v4.2.1",
  url: "",
  actor_label: "token:ci",
  actor_user_id: null,
  via_token: true,
  recorded_at: at,
});

function link(over: Record<string, unknown> = {}) {
  return {
    change: {
      ...phase("p-started", "started", "2026-08-30T14:00:00Z"),
      service_id: OWN_SERVICE,
      source: "github-actions",
      external_id: "run-1",
      kind: "deploy",
    },
    role: "own_service",
    occurred_at: "2026-08-30T14:00:00Z",
    lag_seconds: 1860,
    computed_at: "2026-08-30T14:31:02Z",
    phases: [phase("p-started", "started", "2026-08-30T14:00:00Z"), phase("p-done", "succeeded", "2026-08-30T14:05:00Z")],
    ...over,
  };
}

type Res = { data?: unknown; error?: unknown; response?: Response };
const ok = (data: unknown): Res => ({ data, response: new Response(null, { status: 200 }) });
const refused = (status: number, code: string): Res => ({ error: { error: code }, response: new Response(null, { status }) });

function serve(opts: { incident?: Record<string, unknown>; changes?: Res | (() => Promise<Res>); updates?: unknown[] } = {}) {
  const incident = { ...INCIDENT, ...(opts.incident ?? {}) };
  apiMock.GET.mockImplementation((path: string) => {
    if (path.endsWith("/incidents/{incidentID}/changes")) {
      const c = opts.changes ?? ok({ items: [] });
      return Promise.resolve(typeof c === "function" ? c() : c);
    }
    if (path.endsWith("/updates")) return Promise.resolve(ok(opts.updates ?? []));
    if (path.endsWith("/postmortem")) return Promise.resolve(refused(404, "not found"));
    if (path.endsWith("/services/{serviceID}")) return Promise.resolve(ok({ service: { id: OWN_SERVICE, slug: "checkout", name: "Checkout" } }));
    if (path.endsWith("/monitors/{monitorID}")) return Promise.resolve(refused(404, "not found"));
    return Promise.resolve(ok(incident));
  });
}

function mountView() {
  return mount(IncidentDetailView, {
    global: { stubs: { RouterLink: { props: ["to"], template: '<a :data-to="JSON.stringify(to)"><slot /></a>' } } },
  });
}
async function settle() {
  await flushPromises();
  await flushPromises();
  await flushPromises();
}

type W = ReturnType<typeof mountView>;
const t = (w: W, id: string) => w.find(`[data-testid="${id}"]`);
const has = (w: W, id: string) => t(w, id).exists();
const changeCalls = () => apiMock.GET.mock.calls.filter((c) => String(c[0]).endsWith("/incidents/{incidentID}/changes"));

beforeEach(() => {
  apiMock.GET.mockReset();
  apiMock.POST.mockReset();
});
afterEach(() => {
  expect(apiMock.POST, "opening an incident records no change").not.toHaveBeenCalled();
});

describe("IncidentDetailView — Preceded by (D-0210 item 4, D7)", () => {
  it("a SERVICE incident asks for its change links; the row carries the anchored phase, the copied lag and the phases today", async () => {
    serve({ changes: ok({ items: [link()] }) });
    const w = mountView();
    await settle();
    expect(changeCalls()).toHaveLength(1);
    expect(changeCalls()[0][1].params.path).toEqual({ projectID: "p1", incidentID: "inc1" });
    expect(has(w, "incident-preceded")).toBe(true);
    expect(t(w, "incident-preceded-count").text()).toBe("1 change");

    const row = t(w, "incident-preceded-row");
    expect(row.attributes("data-role")).toBe("own_service");
    expect(row.attributes("data-source")).toBe("github-actions");
    expect(row.attributes("data-external-id")).toBe("run-1");
    expect(row.attributes("data-kind")).toBe("deploy");
    expect(t(w, "incident-preceded-role").text()).toBe("own service");
    expect(t(w, "incident-preceded-kind").text()).toBe("deploy");
    expect(t(w, "incident-preceded-ref").text()).toBe("v4.2.1");
    expect(t(w, "incident-preceded-anchor").text(), "the phase KNOWN at the open, not the latest one").toBe("started 14:00");
    expect(t(w, "incident-preceded-lag").text()).toBe("−31 m");
    expect(t(w, "incident-preceded-lag").attributes("title")).toBe("1860 s before the open");
    expect(t(w, "incident-preceded-sub").text(), "the group's phases TODAY, beside the anchor").toContain("started 08-30 14:00 → succeeded 14:05");
    expect(t(w, "incident-preceded-sub").text()).toContain("anchored at the started phase known at 14:31");
    expect(w.text(), "preceded is the whole claim").not.toContain("caused");
    expect(t(w, "incident-preceded-hint").text()).toContain("the window is fixed at open");
  });

  it("the row links to the comparison under the CHANGE's own service, by identity", async () => {
    serve({ changes: ok({ items: [link()] }) });
    const w = mountView();
    await settle();
    expect(JSON.parse(t(w, "incident-preceded-compare").attributes("data-to")!)).toEqual({
      path: `/services/${OWN_SERVICE}/changes/compare`,
      query: { source: "github-actions", external_id: "run-1" },
    });
  });

  it("an upstream link is a probable root, named on the service the change was recorded on", async () => {
    const upstream = link({
      role: "upstream",
      change: { ...link().change, service_id: UPSTREAM_SERVICE, source: "argo", external_id: "flip-9", kind: "flag" },
      lag_seconds: 45,
    });
    serve({ changes: ok({ items: [upstream] }) });
    const w = mountView();
    await settle();
    expect(t(w, "incident-preceded-role").text()).toBe("upstream · probable root");
    expect(t(w, "incident-preceded-lag").text()).toBe("−45 s");
    expect(t(w, "incident-preceded-sub").text()).toContain("which the impact graph marks as a probable root of this incident");
    expect(JSON.parse(t(w, "incident-preceded-compare").attributes("data-to")!).path).toBe(`/services/${UPSTREAM_SERVICE}/changes/compare`);
  });

  it("a group with no terminal phase gets the dashed cell, never a comparison link", async () => {
    serve({ changes: ok({ items: [link({ phases: [phase("p-started", "started", "2026-08-30T14:00:00Z")] })] }) });
    const w = mountView();
    await settle();
    expect(has(w, "incident-preceded-compare")).toBe(false);
    expect(t(w, "incident-preceded-no-terminal").text()).toBe("before/after unavailable until a terminal phase");
  });

  it("no links: the section says so rather than disappearing; a refusal renders in one line", async () => {
    serve({ changes: ok({ items: [] }) });
    const w = mountView();
    await settle();
    expect(t(w, "incident-preceded-count").text()).toBe("0 changes");
    expect(t(w, "incident-preceded-empty").text()).toContain("No change was recorded on this service or its probable-root upstreams before the open.");

    apiMock.GET.mockReset();
    serve({ changes: refused(403, "") });
    const w2 = mountView();
    await settle();
    expect(t(w2, "incident-preceded-error").text()).toBe("You cannot see this incident's changes.");
    expect(t(w2, "incident-preceded-error").attributes("data-status")).toBe("403");
    expect(has(w2, "incident-preceded-row")).toBe(false);
  });

  it("a MONITOR incident has no service and asks for nothing — the section is absent", async () => {
    serve({ incident: { service_id: undefined, monitor_id: "mon1" } });
    const w = mountView();
    await settle();
    expect(has(w, "incident-preceded")).toBe(false);
    expect(changeCalls(), "no service, no correlation, no request").toHaveLength(0);
  });
});

describe("IncidentDetailView — the `🚀 Changes:` note (D-0210 item 4)", () => {
  it("is detected by its PREFIX and rendered as a system note, like `⚡ Context:`", async () => {
    serve({
      updates: [
        { status: "investigating", body: `${SYSTEM_NOTE_MARKERS.changes} deploy v4.2.1 (github-actions/run-1) 31 m before the open`, created_at: "2026-08-30T14:31:02Z", author: "" },
        { status: "investigating", body: `${SYSTEM_NOTE_MARKERS.context} availability 91.20 % over 30d`, created_at: "2026-08-30T14:31:01Z", author: "" },
        { status: "investigating", body: "a human wrote this one", created_at: "2026-08-30T14:35:00Z", author: "alice@example.com" },
      ],
    });
    const w = mountView();
    await settle();
    const notes = w.findAll('[data-testid="incident-update-system-note"]');
    expect(notes.map((n) => n.attributes("data-note")), "the human note is not a system note").toEqual(["changes", "context"]);
    expect(notes[0].text()).toContain("deploy v4.2.1 (github-actions/run-1) 31 m before the open");
    // The changes note is the only one whose gutter carries a kind SHAPE rather than a status dot.
    expect(notes[0].find("span.bg-accent").exists(), "the change glyph in the gutter").toBe(true);
    expect(notes[1].find("span.bg-accent").exists()).toBe(false);
  });
});
