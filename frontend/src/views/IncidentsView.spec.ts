import { flushPromises, mount } from "@vue/test-utils";
import { describe, expect, it, vi } from "vitest";

import IncidentsView from "@/views/IncidentsView.vue";

// FR-042 / NFR-035 (spec §4.6, I13; mock §4 rows 5 and 6): the operator list stops printing the raw
// enum. Its card badge is the badge WORD (upper-cased by the existing class, so "NOT SPECIFIED"), the
// selected incident's badge is the PHRASE ("Impact not specified" / "Major impact"), and the card's
// badge row wraps so the longer word and a subject chip still fit the 320 px column (mock D-16).

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));
vi.mock("@/components/AppShell.vue", () => ({
  default: { name: "AppShell", template: "<div><slot name='actions' /><slot /></div>" },
}));
vi.mock("@/stores/session", () => ({ useSession: () => ({ canProjectWrite: () => false }) }));
vi.mock("@/stores/workspace", () => ({
  useWorkspace: () => ({ init: () => Promise.resolve(), orgId: "o1", projectId: "p1", orgName: "Org", projectName: "Project" }),
}));

const RouterLink = { props: ["to"], template: "<a><slot /></a>" };

function inc(id: string, impact: string) {
  return { id, project_id: "p1", title: `Incident ${id}`, status: "investigating", impact, source: "auto", started_at: "2026-11-16T10:00:00Z" };
}

async function mountList(list: unknown[]) {
  apiMock.GET.mockReset();
  apiMock.GET.mockImplementation((path: string) => {
    if (path === "/api/v1/projects/{projectID}/incidents") return Promise.resolve({ data: list });
    if (path.endsWith("/updates")) return Promise.resolve({ data: [] });
    return Promise.resolve({ error: { error: "not found" } });
  });
  const w = mount(IncidentsView, { global: { stubs: { RouterLink } } });
  await flushPromises();
  return w;
}

describe("IncidentsView impact words", () => {
  it("prints none as 'Not specified' on the card and 'Impact not specified' on the selected badge", async () => {
    const w = await mountList([inc("aaaaaaaa1", "none"), inc("bbbbbbbb2", "major")]);
    const cards = w.findAll('[data-testid="incident-list-impact"]').map((b) => b.text());
    expect(cards).toEqual(["Not specified", "Major"]);
    // The first incident is auto-selected.
    expect(w.get('[data-testid="incident-selected-impact"]').text()).toBe("Impact not specified");
    expect(w.text()).not.toMatch(/\bnone\b/);
  });

  it("phrases a rated impact as '<Label> impact' on the selected badge", async () => {
    const w = await mountList([inc("bbbbbbbb2", "major")]);
    expect(w.get('[data-testid="incident-selected-impact"]').text()).toBe("Major impact");
  });

  it("lets the card's badge row wrap", async () => {
    const w = await mountList([inc("aaaaaaaa1", "none")]);
    expect(w.get('[data-testid="incident-list-impact"]').element.parentElement!.classList.contains("flex-wrap")).toBe(true);
  });
});
