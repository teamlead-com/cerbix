import { flushPromises, mount } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { nextTick } from "vue";
import { beforeEach, describe, expect, it, vi } from "vitest";

import SecretsPanel from "@/components/settings/SecretsPanel.vue";
import { useSession } from "@/stores/session";
import { useWorkspace } from "@/stores/workspace";

const apiMock = vi.hoisted(() => ({
  GET: vi.fn(),
  POST: vi.fn(),
  PATCH: vi.fn(),
  DELETE: vi.fn(),
}));

vi.mock("@/api/client", () => ({ api: apiMock }));

type Deferred<T> = {
  promise: Promise<T>;
  resolve: (value: T) => void;
};

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

function mountPanel(globalAdmin = true) {
  const pinia = createPinia();
  setActivePinia(pinia);
  const ws = useWorkspace();
  ws.orgId = "org-a";
  ws.projectId = "project-a";
  const session = useSession();
  session.user = { id: "user-a", is_global_admin: globalAdmin } as typeof session.user;
  session.memberships = globalAdmin
    ? []
    : ([{ org_id: "org-a", project_id: "project-a", role: "viewer" }] as typeof session.memberships);
  const wrapper = mount(SecretsPanel, { global: { plugins: [pinia] } });
  return { wrapper, ws };
}

describe("SecretsPanel tenant context", () => {
  beforeEach(() => {
    for (const method of Object.values(apiMock)) method.mockReset();
    vi.stubGlobal("confirm", vi.fn(() => true));
  });

  it("ignores a deferred project-A load after switching to project B", async () => {
    const loadA = deferred<{ data: Array<{ id: string; name: string }> }>();
    apiMock.GET.mockImplementation((_path: string, options: { params: { path: { projectID: string } } }) => {
      if (options.params.path.projectID === "project-a") return loadA.promise;
      return Promise.resolve({ data: [{ id: "b-id", name: "b-secret" }] });
    });

    const { wrapper, ws } = mountPanel();
    await nextTick();
    const requestA = apiMock.GET.mock.calls[0]?.[1] as { signal: AbortSignal };

    ws.projectId = "project-b";
    await nextTick();
    await flushPromises();
    expect(requestA.signal.aborted).toBe(true);
    expect(wrapper.text()).toContain("b-secret");

    loadA.resolve({ data: [{ id: "a-id", name: "a-secret" }] });
    await flushPromises();
    expect(wrapper.text()).toContain("b-secret");
    expect(wrapper.text()).not.toContain("a-secret");
  });

  it("aborts a project-A create and never clears or applies project-B plaintext state", async () => {
    const createA = deferred<{ data: { id: string; name: string } }>();
    apiMock.GET.mockResolvedValue({ data: [] });
    apiMock.POST.mockReturnValue(createA.promise);

    const { wrapper, ws } = mountPanel();
    await flushPromises();
    await wrapper.get('[data-testid="secret-add-name"]').setValue("a-secret");
    await wrapper.get('[data-testid="secret-add-value"]').setValue("a-plaintext");
    await wrapper.get('[data-testid="secret-add-submit"]').trigger("click");
    const requestA = apiMock.POST.mock.calls[0]?.[1] as { signal: AbortSignal };

    ws.projectId = "project-b";
    await nextTick();
    await flushPromises();
    expect(requestA.signal.aborted).toBe(true);
    expect((wrapper.get('[data-testid="secret-add-name"]').element as HTMLInputElement).value).toBe("");
    expect((wrapper.get('[data-testid="secret-add-value"]').element as HTMLInputElement).value).toBe("");

    await wrapper.get('[data-testid="secret-add-name"]').setValue("b-secret");
    await wrapper.get('[data-testid="secret-add-value"]').setValue("b-plaintext");
    createA.resolve({ data: { id: "a-id", name: "a-secret" } });
    await flushPromises();

    expect((wrapper.get('[data-testid="secret-add-name"]').element as HTMLInputElement).value).toBe("b-secret");
    expect((wrapper.get('[data-testid="secret-add-value"]').element as HTMLInputElement).value).toBe("b-plaintext");
    expect(wrapper.find('[data-testid="secret-action-error"]').exists()).toBe(false);
  });

  it("shows the inventory read-only to a viewer", async () => {
    apiMock.GET.mockResolvedValue({ data: [{ id: "s-id", name: "shared-secret" }] });
    const { wrapper } = mountPanel(false);
    await flushPromises();

    expect(wrapper.text()).toContain("shared-secret");
    expect(wrapper.text()).toContain("Read-only access");
    expect(wrapper.find('[data-testid="secret-add-submit"]').exists()).toBe(false);
    expect(wrapper.text()).not.toContain("Edit");
    expect(wrapper.text()).not.toContain("Delete");
  });
});
