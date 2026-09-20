import { createPinia, setActivePinia } from "pinia";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { useWorkspace } from "@/stores/workspace";

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));

beforeEach(() => {
  setActivePinia(createPinia());
  localStorage.clear();
  apiMock.GET.mockReset();
});

describe("workspace read failures", () => {
  it("does not render a failed organization read as an empty tenant", async () => {
    apiMock.GET.mockResolvedValue({ error: { error: "database unavailable" }, response: { ok: false } });
    const workspace = useWorkspace();
    await expect(workspace.init()).rejects.toThrow("database unavailable");
    expect(workspace.loaded).toBe(false);
  });

  it("does not render a failed project read as an empty organization", async () => {
    apiMock.GET
      .mockResolvedValueOnce({ data: [{ id: "o1", name: "Acme" }], response: { ok: true } })
      .mockResolvedValueOnce({ error: { error: "project read failed" }, response: { ok: false } });
    const workspace = useWorkspace();
    await expect(workspace.init()).rejects.toThrow("project read failed");
    expect(workspace.orgId).toBe("o1");
    expect(workspace.projects).toEqual([]);
    expect(workspace.loaded).toBe(false);
  });
});
