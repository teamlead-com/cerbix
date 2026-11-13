import { flushPromises, mount } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { beforeEach, describe, expect, it, vi } from "vitest";

import UsersPanel from "@/components/settings/UsersPanel.vue";
import { useSession } from "@/stores/session";

// sec-hardening-2 H-1: the instance user list badges client-credentials identities as `machine`
// and does not offer them the global-admin flag (the server refuses it with 400); revoking a flag a
// machine somehow holds stays possible.
const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), PATCH: vi.fn(), PUT: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));

const base = { oidc_sub: "s", created_at: new Date().toISOString(), updated_at: new Date().toISOString(), memberships: [] };
const users = [
  { ...base, id: "me", email: "root@example.com", display_name: "Root", is_global_admin: true, auth_type: "local", kind: "human" },
  { ...base, id: "ada", email: "ada@example.com", display_name: "Ada", is_global_admin: false, auth_type: "oidc", kind: "human" },
  { ...base, id: "ci", email: "9f1c@clients", display_name: "ci-deployer", is_global_admin: false, auth_type: "oidc", kind: "machine" },
  { ...base, id: "old", email: "old@clients", display_name: "legacy-bot", is_global_admin: true, auth_type: "oidc", kind: "machine" },
];

beforeEach(() => {
  setActivePinia(createPinia());
  const s = useSession();
  s.user = { id: "me", email: "root@example.com", is_global_admin: true } as never;
  for (const f of Object.values(apiMock)) f.mockReset();
  apiMock.GET.mockImplementation(async (path: string) => {
    if (path === "/api/v1/admin/users") return { data: users };
    return { data: [] };
  });
});

function rowOf(w: ReturnType<typeof mount>, email: string) {
  const row = w.findAll("tr").find((r) => r.text().includes(email));
  if (!row) throw new Error(`no row for ${email}`);
  return row;
}

describe("UsersPanel kind", () => {
  it("badges machines only", async () => {
    const w = mount(UsersPanel);
    await flushPromises();
    expect(rowOf(w, "9f1c@clients").find('[data-testid="kind-badge"]').text()).toBe("machine");
    expect(rowOf(w, "ada@example.com").find('[data-testid="kind-badge"]').exists()).toBe(false);
  });

  it("does not offer a machine the global-admin flag, but lets one be revoked", async () => {
    const w = mount(UsersPanel);
    await flushPromises();
    const grant = rowOf(w, "9f1c@clients").findAll("button").find((b) => b.text() === "Grant admin")!;
    expect(grant.attributes("disabled")).toBeDefined();
    expect(grant.attributes("title")).toBe("A machine identity cannot be a global admin — use an API token");
    const revoke = rowOf(w, "old@clients").findAll("button").find((b) => b.text() === "Revoke admin")!;
    expect(revoke.attributes("disabled")).toBeUndefined();
    const human = rowOf(w, "ada@example.com").findAll("button").find((b) => b.text() === "Grant admin")!;
    expect(human.attributes("disabled")).toBeUndefined();
  });
});
