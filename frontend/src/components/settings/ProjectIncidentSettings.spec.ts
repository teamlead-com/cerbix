import { flushPromises, mount } from "@vue/test-utils";
import { beforeEach, describe, expect, it, vi } from "vitest";

import ProjectIncidentSettings from "@/components/settings/ProjectIncidentSettings.vue";

// FR-042 / NFR-035 (D4, D19; mock §2, D-8/D-9): the project's default impact for monitor
// auto-incidents. Save is disabled until the draft differs from what is STORED, so the form never
// sends the server's no-op; "was <Old>" shows only while dirty; the server's own refusal renders.
const apiMock = vi.hoisted(() => ({ GET: vi.fn(), PATCH: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));

const EXPLANATION =
  "The impact a monitor's automatic incident opens with. It is shown on status pages and in incident lists, and it does not change while the incident is open. Changing it here does not touch incidents that are already open — it applies from the next one. Incidents a service opens take their impact from that service's alerting settings instead.";

function mountWith(opts: { canManage: boolean; stored?: string; patch?: (body: Record<string, unknown>) => unknown }) {
  apiMock.GET.mockReset();
  apiMock.PATCH.mockReset();
  apiMock.GET.mockResolvedValue({ data: { id: "p1", default_incident_impact: opts.stored ?? "major" } });
  apiMock.PATCH.mockImplementation(async (_p: string, req: { body: Record<string, unknown> }) =>
    opts.patch ? opts.patch(req.body) : { data: { id: "p1", ...req.body } },
  );
  return mount(ProjectIncidentSettings, { props: { projectId: "p1", canManage: opts.canManage } });
}
const t = (w: ReturnType<typeof mountWith>, id: string) => w.find(`[data-testid="project-incident-${id}"]`);

beforeEach(() => {
  apiMock.GET.mockReset();
  apiMock.PATCH.mockReset();
});

describe("ProjectIncidentSettings with project:manage", () => {
  it("reads the stored value, titles the card and explains it", async () => {
    const w = mountWith({ canManage: true });
    await flushPromises();
    expect(apiMock.GET).toHaveBeenCalledWith("/api/v1/projects/{projectID}", { params: { path: { projectID: "p1" } } });
    expect(w.get("h2").text()).toBe("Incidents");
    expect(w.text()).toContain("Impact of monitor incidents");
    expect(t(w, "explanation").text()).toBe(EXPLANATION);
    const select = t(w, "impact-select");
    expect((select.element as HTMLSelectElement).value).toBe("major");
    expect(select.findAll("option").map((o) => [o.attributes("value"), o.text()])).toEqual([
      ["none", "Not specified"],
      ["minor", "Minor"],
      ["major", "Major"],
      ["critical", "Critical"],
    ]);
  });

  it("keeps Save disabled until the value differs, and says what it was while dirty", async () => {
    const w = mountWith({ canManage: true });
    await flushPromises();
    expect(t(w, "save").attributes("disabled")).toBeDefined();
    expect(t(w, "was").exists()).toBe(false);

    await t(w, "impact-select").setValue("critical");
    expect(t(w, "save").attributes("disabled")).toBeUndefined();
    expect(t(w, "was").text()).toBe("was Major");

    await t(w, "impact-select").setValue("major");
    expect(t(w, "save").attributes("disabled")).toBeDefined();
    expect(t(w, "was").exists()).toBe(false);
  });

  it("sends only the default, adopts the echo, and says Saved", async () => {
    const w = mountWith({ canManage: true });
    await flushPromises();
    await t(w, "impact-select").setValue("none");
    expect(t(w, "was").text()).toBe("was Major");
    await t(w, "save").trigger("click");
    await flushPromises();

    expect(apiMock.PATCH).toHaveBeenCalledWith("/api/v1/projects/{projectID}", {
      params: { path: { projectID: "p1" } },
      body: { default_incident_impact: "none" },
    });
    expect(t(w, "saved").text()).toBe("✓ Saved");
    expect(t(w, "was").exists(), "what was saved is no longer a change").toBe(false);
    expect(t(w, "save").attributes("disabled")).toBeDefined();
    expect((t(w, "impact-select").element as HTMLSelectElement).value).toBe("none");

    // A further change is a new edit, measured against what is now stored.
    await t(w, "impact-select").setValue("minor");
    expect(t(w, "saved").exists()).toBe(false);
    expect(t(w, "was").text()).toBe("was Not specified");
  });

  it("renders the server's own refusal in the down hue", async () => {
    const w = mountWith({ canManage: true, patch: () => ({ error: { error: "forbidden: project:manage required" } }) });
    await flushPromises();
    await t(w, "impact-select").setValue("critical");
    await t(w, "save").trigger("click");
    await flushPromises();
    const err = t(w, "error");
    expect(err.text()).toBe("forbidden: project:manage required");
    expect(err.classes()).toContain("text-down");
    expect(t(w, "saved").exists()).toBe(false);
  });

  it("says so when the project could not be read, instead of offering a default", async () => {
    apiMock.GET.mockResolvedValue({ error: { error: "boom" } });
    const w = mount(ProjectIncidentSettings, { props: { projectId: "p1", canManage: true } });
    await flushPromises();
    expect(t(w, "impact-select").exists()).toBe(false);
    expect(t(w, "save").exists()).toBe(false);
    expect(t(w, "unavailable").text()).toBe("Could not load the project's incident settings.");
  });
});

describe("ProjectIncidentSettings without project:manage", () => {
  it("shows the value as the incident-page badge and who can change it", async () => {
    const w = mountWith({ canManage: false, stored: "none" });
    await flushPromises();
    expect(t(w, "impact-select").exists()).toBe(false);
    expect(t(w, "save").exists()).toBe(false);
    const badge = t(w, "impact-badge");
    expect(badge.text()).toBe("Not specified");
    expect(badge.classes()).toEqual(expect.arrayContaining(["text-ink-3", "bg-surface-2"]));
    expect(t(w, "explanation").text()).toBe(EXPLANATION);
    expect(t(w, "read-only").text()).toBe("Only a project admin can change this.");
  });
});
