import { flushPromises, mount } from "@vue/test-utils";
import { describe, expect, it, vi } from "vitest";

import SearchBox from "@/components/SearchBox.vue";

// A search hit carries its own org and project, and every hit type must switch the workspace to
// them before navigating.
//
// It used to switch for PROJECT hits alone. A monitor or incident hit from another workspace then
// landed on a detail page whose reads and permission checks still ran against the previous tenant:
// the successor and convert pickers listed a stranger's services, and `canWrite` compared the
// workspace's org against the loaded subject's project, so a legitimate editor of the target saw
// acknowledge, resolve and postmortem disappear. Two entrances, one defect.

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), PATCH: vi.fn(), PUT: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));

const router = vi.hoisted(() => ({ push: vi.fn() }));
vi.mock("vue-router", () => ({ useRouter: () => router }));

const ws = vi.hoisted(() => ({
  orgId: "org-a",
  projectId: "proj-a",
  selectOrg: vi.fn(async (id: string) => {
    ws.orgId = id;
    return true;
  }),
  selectProject: vi.fn((id: string) => {
    ws.projectId = id;
    return true;
  }),
}));
vi.mock("@/stores/workspace", () => ({ useWorkspace: () => ws }));

async function pickFirstHit(hit: Record<string, unknown>) {
  ws.orgId = "org-a";
  ws.projectId = "proj-a";
  ws.selectOrg.mockClear();
  ws.selectProject.mockClear();
  router.push.mockClear();
  apiMock.GET.mockReset();
  apiMock.GET.mockResolvedValue({ data: { hits: [hit] } });

  const w = mount(SearchBox);
  const input = w.get("input");
  await input.setValue("check");
  await input.trigger("input");
  // The box debounces; the timer is real and short, so wait for it rather than faking the clock.
  await new Promise((r) => setTimeout(r, 260));
  await flushPromises();
  const buttons = w.findAll("button");
  expect(buttons.length).toBeGreaterThan(0);
  await buttons[0].trigger("click");
  await flushPromises();
  return w;
}

type Deferred<T> = {
  promise: Promise<T>;
  resolve: (value: T) => void;
  reject: (reason?: unknown) => void;
};

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

async function startSearch(wrapper: ReturnType<typeof mount>, term: string) {
  const input = wrapper.get("input");
  await input.setValue(term);
  await input.trigger("input");
  await new Promise((resolve) => setTimeout(resolve, 260));
  await flushPromises();
  return input;
}

describe("a search hit brings its tenant with it", () => {
  it("switches org and project for a MONITOR hit in another workspace", async () => {
    await pickFirstHit({ type: "monitor", id: "mon-b", name: "checkout", org_id: "org-b", project_id: "proj-b" });

    expect(ws.selectOrg).toHaveBeenCalledWith("org-b");
    expect(ws.selectProject).toHaveBeenCalledWith("proj-b");
    expect(router.push).toHaveBeenCalledWith({ name: "monitor", params: { id: "mon-b" } });
  });

  it("switches org and project for an INCIDENT hit in another workspace", async () => {
    await pickFirstHit({ type: "incident", id: "inc-b", name: "outage", org_id: "org-b", project_id: "proj-b" });

    expect(ws.selectOrg).toHaveBeenCalledWith("org-b");
    expect(ws.selectProject).toHaveBeenCalledWith("proj-b");
    expect(router.push).toHaveBeenCalledWith({ name: "incident", params: { id: "inc-b" } });
  });

  it("does not churn the workspace for a hit already in it", async () => {
    await pickFirstHit({ type: "monitor", id: "mon-a", name: "checkout", org_id: "org-a", project_id: "proj-a" });

    expect(ws.selectOrg).not.toHaveBeenCalled();
    expect(ws.selectProject).not.toHaveBeenCalled();
    expect(router.push).toHaveBeenCalledWith({ name: "monitor", params: { id: "mon-a" } });
  });

  it("does not navigate when the organization transition is rejected", async () => {
    ws.orgId = "org-a";
    ws.projectId = "proj-a";
    ws.selectOrg.mockClear().mockResolvedValueOnce(false);
    ws.selectProject.mockClear();
    router.push.mockClear();
    apiMock.GET.mockResolvedValue({
      data: { hits: [{ type: "monitor", id: "mon-b", name: "checkout", org_id: "org-b", project_id: "proj-b" }] },
    });

    const wrapper = mount(SearchBox, { attachTo: document.body });
    await wrapper.get("input").setValue("check");
    await wrapper.get("input").trigger("input");
    await new Promise((resolve) => setTimeout(resolve, 260));
    await flushPromises();
    await wrapper.findAll("button")[0].trigger("click");
    await flushPromises();

    expect(ws.selectProject).not.toHaveBeenCalled();
    expect(router.push).not.toHaveBeenCalled();
    expect(document.activeElement).toBe(wrapper.get("input").element);
    wrapper.unmount();
  });
});

describe("search combobox semantics", () => {
  it("exposes combobox, listbox, option, and live-region semantics", async () => {
    apiMock.GET.mockReset();
    apiMock.GET.mockResolvedValue({
      data: { hits: [{ type: "monitor", id: "mon-a", label: "Checkout" }] },
    });

    const wrapper = mount(SearchBox);
    const input = await startSearch(wrapper, "check");

    expect(input.attributes("role")).toBe("combobox");
    expect(input.attributes("aria-label")).toBe("Search");
    expect(input.attributes("aria-expanded")).toBe("true");
    expect(input.attributes("aria-controls")).toBe("search-results");
    expect(input.attributes("aria-autocomplete")).toBe("list");
    expect(wrapper.get('[role="listbox"]').exists()).toBe(true);
    expect(wrapper.get('[role="option"]').attributes("id")).toMatch(/^search-option-/);
    expect(wrapper.get('[role="option"]').attributes("aria-selected")).toBe("false");
    expect(wrapper.get('[role="option"]').attributes("tabindex")).toBe("-1");
    expect(wrapper.findAll('[aria-live="polite"]')).toHaveLength(1);

    wrapper.unmount();
  });

  it("moves the active option with ArrowDown and ArrowUp and activates it with Enter", async () => {
    ws.orgId = "org-a";
    ws.projectId = "proj-a";
    router.push.mockClear();
    apiMock.GET.mockReset();
    apiMock.GET.mockResolvedValue({
      data: {
        hits: [
          { type: "monitor", id: "mon-a", label: "Checkout", org_id: "org-a", project_id: "proj-a" },
          { type: "incident", id: "inc-a", label: "Checkout outage", org_id: "org-a", project_id: "proj-a" },
        ],
      },
    });

    const wrapper = mount(SearchBox);
    const input = await startSearch(wrapper, "check");
    const options = () => wrapper.findAll('[role="option"]');
    const selected = () => options().filter((option) => option.attributes("aria-selected") === "true");

    await input.trigger("keydown", { key: "ArrowDown" });
    expect(input.attributes("aria-activedescendant")).toBe(options()[0].attributes("id"));
    expect(selected()).toHaveLength(1);
    expect(selected()[0].attributes("id")).toBe(options()[0].attributes("id"));

    await input.trigger("keydown", { key: "ArrowDown" });
    expect(input.attributes("aria-activedescendant")).toBe(options()[1].attributes("id"));
    expect(selected()).toHaveLength(1);
    expect(selected()[0].attributes("id")).toBe(options()[1].attributes("id"));

    await input.trigger("keydown", { key: "ArrowUp" });
    expect(input.attributes("aria-activedescendant")).toBe(options()[0].attributes("id"));
    expect(selected()).toHaveLength(1);
    expect(selected()[0].attributes("id")).toBe(options()[0].attributes("id"));

    await input.trigger("keydown", { key: "Enter" });
    await flushPromises();
    expect(router.push).toHaveBeenCalledWith({ name: "monitor", params: { id: "mon-a" } });

    wrapper.unmount();
  });

  it("wraps ArrowUp from no selection to the last option", async () => {
    apiMock.GET.mockReset();
    apiMock.GET.mockResolvedValue({
      data: {
        hits: [
          { type: "monitor", id: "mon-a", label: "A" },
          { type: "monitor", id: "mon-b", label: "B" },
          { type: "monitor", id: "mon-c", label: "C" },
        ],
      },
    });

    const wrapper = mount(SearchBox);
    const input = await startSearch(wrapper, "check");
    const options = wrapper.findAll('[role="option"]');

    await input.trigger("keydown", { key: "ArrowUp" });

    expect(input.attributes("aria-activedescendant")).toBe(options[2].attributes("id"));
    expect(options.filter((option) => option.attributes("aria-selected") === "true")).toHaveLength(1);
    expect(options[2].attributes("aria-selected")).toBe("true");

    wrapper.unmount();
  });

  it("keeps option IDs unique for empty and sanitization-colliding identities", async () => {
    apiMock.GET.mockReset();
    apiMock.GET.mockResolvedValue({
      data: {
        hits: [
          { type: "monitor", id: "a/b", label: "Slash" },
          { type: "monitor", id: "a-b", label: "Dash" },
          { type: "monitor", id: "", label: "Empty" },
          { type: "monitor", id: "///", label: "Punctuation" },
        ],
      },
    });

    const wrapper = mount(SearchBox);
    await startSearch(wrapper, "check");
    const ids = wrapper.findAll('[role="option"]').map((option) => option.attributes("id"));

    expect(ids.every((id) => /^search-option-/.test(id ?? ""))).toBe(true);
    expect(new Set(ids).size).toBe(ids.length);
    expect(ids[2]).toMatch(/-2(?:-\d+)?$/);

    wrapper.unmount();
  });

  it("closes on Escape, restores focus, and clears when Escape is pressed again", async () => {
    apiMock.GET.mockReset();
    apiMock.GET.mockResolvedValue({
      data: { hits: [{ type: "monitor", id: "mon-a", label: "Checkout" }] },
    });

    const wrapper = mount(SearchBox, { attachTo: document.body });
    const input = await startSearch(wrapper, "check");
    const other = document.createElement("button");
    document.body.append(other);
    other.focus();

    await input.trigger("keydown", { key: "Escape" });
    expect(wrapper.find('[role="listbox"]').exists()).toBe(false);
    expect(input.attributes("aria-expanded")).toBe("false");
    expect(document.activeElement).toBe(input.element);
    expect((input.element as HTMLInputElement).value).toBe("check");

    await input.trigger("keydown", { key: "Escape" });
    expect((input.element as HTMLInputElement).value).toBe("");
    expect(wrapper.find('[role="option"]').exists()).toBe(false);

    other.remove();
    wrapper.unmount();
  });

  it("clears the results and closes the popup for a short query", async () => {
    apiMock.GET.mockReset();
    apiMock.GET.mockResolvedValue({
      data: { hits: [{ type: "monitor", id: "mon-a", label: "Checkout" }] },
    });

    const wrapper = mount(SearchBox);
    const input = await startSearch(wrapper, "check");
    expect(wrapper.get('[role="option"]').exists()).toBe(true);

    await input.setValue("x");
    await input.trigger("input");
    expect(wrapper.find('[role="listbox"]').exists()).toBe(false);
    expect(input.attributes("aria-expanded")).toBe("false");
    expect(wrapper.find('[role="option"]').exists()).toBe(false);

    wrapper.unmount();
  });

  it("announces searching and no matches through one polite live region", async () => {
    const request = deferred<{ data: { hits: [] } }>();
    apiMock.GET.mockReset();
    apiMock.GET.mockReturnValue(request.promise);

    const wrapper = mount(SearchBox);
    const input = wrapper.get("input");
    await input.setValue("check");
    await input.trigger("input");
    await new Promise((resolve) => setTimeout(resolve, 260));
    await flushPromises();

    const live = wrapper.get('[aria-live="polite"]');
    expect(wrapper.findAll('[aria-live="polite"]')).toHaveLength(1);
    expect(live.text()).toBe("Searching…");

    request.resolve({ data: { hits: [] } });
    await flushPromises();
    expect(live.text()).toContain("No matches");

    wrapper.unmount();
  });

  it("announces a specific request error through the polite live region", async () => {
    apiMock.GET.mockReset();
    apiMock.GET.mockResolvedValue({ error: { error: "search unavailable" }, response: { ok: false } });

    const wrapper = mount(SearchBox);
    const input = await startSearch(wrapper, "check");

    expect(wrapper.get('[aria-live="polite"]').text()).toBe("Search failed. Please try again.");
    expect(input.attributes("aria-expanded")).toBe("true");

    wrapper.unmount();
  });

  it("renders the newest query and ignores a stale response and stale navigation", async () => {
    const requestA = deferred<{ data: { hits: Array<Record<string, string>> } }>();
    const requestB = deferred<{ data: { hits: Array<Record<string, string>> } }>();
    apiMock.GET.mockReset();
    apiMock.GET.mockImplementationOnce(() => requestA.promise).mockImplementationOnce(() => requestB.promise);
    router.push.mockClear();

    const wrapper = mount(SearchBox);
    const input = wrapper.get("input");
    await input.setValue("alpha");
    await input.trigger("input");
    await new Promise((resolve) => setTimeout(resolve, 260));
    await flushPromises();
    await input.setValue("beta");
    await input.trigger("input");
    await new Promise((resolve) => setTimeout(resolve, 260));
    await flushPromises();
    expect(apiMock.GET).toHaveBeenCalledTimes(2);

    requestB.resolve({ data: { hits: [{ type: "project", id: "project-b", label: "Beta" }] } });
    await flushPromises();
    expect(wrapper.get('[role="option"]').text()).toContain("Beta");
    expect(wrapper.find('[role="option"]').text()).not.toContain("Alpha");
    expect(router.push).not.toHaveBeenCalled();

    requestA.resolve({ data: { hits: [{ type: "monitor", id: "monitor-a", label: "Alpha" }] } });
    await flushPromises();
    expect(wrapper.get('[role="option"]').text()).toContain("Beta");
    expect(wrapper.find('[role="option"]').text()).not.toContain("Alpha");
    expect(router.push).not.toHaveBeenCalled();

    wrapper.unmount();
  });

  it("does not let a stale request finalizer clear the current loading state", async () => {
    const requestA = deferred<{ data: { hits: Array<Record<string, string>> } }>();
    const requestB = deferred<{ data: { hits: Array<Record<string, string>> } }>();
    apiMock.GET.mockReset();
    apiMock.GET.mockImplementationOnce(() => requestA.promise).mockImplementationOnce(() => requestB.promise);

    const wrapper = mount(SearchBox);
    const input = wrapper.get("input");
    await input.setValue("alpha");
    await input.trigger("input");
    await new Promise((resolve) => setTimeout(resolve, 260));
    await flushPromises();
    await input.setValue("beta");
    await input.trigger("input");
    await new Promise((resolve) => setTimeout(resolve, 260));
    await flushPromises();

    requestA.resolve({ data: { hits: [{ type: "monitor", id: "monitor-a", label: "Alpha" }] } });
    await flushPromises();
    expect(wrapper.get('[aria-live="polite"]').text()).toBe("Searching…");
    expect(wrapper.get('[role="listbox"]').text()).toContain("Searching…");

    requestB.resolve({ data: { hits: [{ type: "monitor", id: "monitor-b", label: "Beta" }] } });
    await flushPromises();
    expect(wrapper.get('[role="option"]').text()).toContain("Beta");

    wrapper.unmount();
  });
});
