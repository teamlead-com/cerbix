import { flushPromises, mount } from "@vue/test-utils";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import LoginView from "@/views/LoginView.vue";

// FR-041a D21, mock 5e: the SSO button carries the page's own `redirect` to the server's SSO start
// instead of the fixed `/`, so a phone link (`/i/:ref`) survives an SSO sign-in. The value is passed
// as is: the server's `sanitizeRedirect` refuses anything that is not a path on this site (5e-2,
// `internal/auth`), so the SPA never filters it — and never builds an absolute URL from it.

const routeState = vi.hoisted(() => ({ query: {} as Record<string, unknown> }));
vi.mock("vue-router", () => ({
  useRoute: () => routeState,
  useRouter: () => ({ push: vi.fn() }),
}));
vi.mock("@/composables/useTheme", () => ({ useTheme: () => ({ toggle: vi.fn() }) }));
vi.mock("@/stores/branding", () => ({ useBranding: () => ({ productName: "cerbix", footerText: "", supportUrl: "" }) }));
vi.mock("@/stores/session", () => ({ useSession: () => ({ fetchMe: vi.fn() }) }));
vi.mock("@/components/BrandMark.vue", () => ({ default: { name: "BrandMark", template: "<span />" } }));

let loc: { href: string };

function mountWith(query: Record<string, unknown>) {
  routeState.query = query;
  return mount(LoginView, { global: { stubs: { RouterLink: { template: "<a><slot /></a>" } } } });
}

async function clickSso(query: Record<string, unknown>) {
  const w = mountWith(query);
  await flushPromises();
  const sso = w.findAll("button").find((b) => b.text().includes("Sign in with SSO"));
  expect(sso, "the SSO button is shown").toBeTruthy();
  await sso!.trigger("click");
  return w;
}

describe("LoginView — SSO start carries the return path (5e)", () => {
  beforeEach(() => {
    loc = { href: "" };
    vi.stubGlobal("location", loc);
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => ({
        ok: true,
        json: async () => ({ local: true, oidc: true, oidc_button_label: "Sign in with SSO", password_reset: false }),
      })),
    );
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("5e-1: ?redirect=/i/X7K2P9QM goes to /auth/login?redirect=%2Fi%2FX7K2P9QM", async () => {
    await clickSso({ redirect: "/i/X7K2P9QM" });
    expect(window.location.href).toBe("/auth/login?redirect=%2Fi%2FX7K2P9QM");
  });

  it("5e-2: an off-site //host value is passed encoded for the server to refuse, never followed by the SPA", async () => {
    await clickSso({ redirect: "//evil.example/i/X" });
    expect(window.location.href).toBe("/auth/login?redirect=%2F%2Fevil.example%2Fi%2FX");
  });

  it("5e-2: a /\\host value is passed encoded as well (the server refuses the backslash)", async () => {
    await clickSso({ redirect: "/\\evil.example" });
    expect(window.location.href).toBe("/auth/login?redirect=%2F%5Cevil.example");
  });

  it("no redirect → %2F, as before", async () => {
    await clickSso({});
    expect(window.location.href).toBe("/auth/login?redirect=%2F");
  });

  it("a repeated redirect parameter uses the first value", async () => {
    await clickSso({ redirect: ["/i/X7K2P9QM", "/settings"] });
    expect(window.location.href).toBe("/auth/login?redirect=%2Fi%2FX7K2P9QM");
  });

  it("a query value that carries no path (?redirect with no value) → %2F", async () => {
    await clickSso({ redirect: null });
    expect(window.location.href).toBe("/auth/login?redirect=%2F");
  });
});
