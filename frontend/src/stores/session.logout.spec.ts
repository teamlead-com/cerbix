import { createPinia, setActivePinia } from "pinia";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { useSession } from "@/stores/session";

vi.mock("@/api/client", () => ({ api: { GET: vi.fn() } }));

// sec-hardening-2 H-9b: POST /auth/logout answers 200 {"redirect_to": …} instead of a 302. fetch
// would follow a 302 to an external post_logout_url — a cross-origin request `connect-src 'self'`
// blocks — so the SPA reads the destination and navigates there itself, with
// window.location.assign. The server validates the value; the SPA navigates to "/" whenever the
// answer carries no destination.

describe("session.logout", () => {
  let assign: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    setActivePinia(createPinia());
    assign = vi.fn();
    vi.stubGlobal("window", { location: { assign } });
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  function answer(status: number, body: unknown) {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(body === undefined ? null : JSON.stringify(body), {
        status,
        headers: { "Content-Type": "application/json" },
      }),
    );
    vi.stubGlobal("fetch", fetchMock);
    return fetchMock;
  }

  it("posts, clears the session, and navigates to the redirect_to the server returned", async () => {
    const fetchMock = answer(200, { redirect_to: "https://idp.example/logout?x=1" });
    const s = useSession();
    s.user = { id: "u1", email: "u@example.com" } as typeof s.user;
    await s.logout();
    expect(fetchMock).toHaveBeenCalledWith("/auth/logout", expect.objectContaining({ method: "POST", credentials: "include" }));
    expect(fetchMock.mock.calls[0][1]?.redirect, "the SPA never lets fetch follow a redirect").toBe("manual");
    expect(s.user).toBeNull();
    expect(assign).toHaveBeenCalledWith("https://idp.example/logout?x=1");
  });

  it("drops the phone state with the user (FR-041a)", async () => {
    answer(200, { redirect_to: "/" });
    const s = useSession();
    s.user = { id: "u1", email: "u@example.com" } as typeof s.user;
    s.phone = { number_masked: "+90•••••6869", verified: true, language: null };
    s.telephonyEnabled = true;
    s.telephonyVoiceLanguage = "en-US";
    await s.logout();
    expect(s.phone).toBeNull();
    expect(s.telephonyEnabled).toBe(false);
    expect(s.telephonyVoiceLanguage).toBeNull();
  });

  it("navigates to / when the server names no destination", async () => {
    answer(200, { redirect_to: "/" });
    await useSession().logout();
    expect(assign).toHaveBeenCalledWith("/");
  });

  it("navigates to / when the answer is not the JSON it expects, or the request fails", async () => {
    answer(500, undefined);
    await useSession().logout();
    expect(assign).toHaveBeenLastCalledWith("/");

    answer(200, { redirect_to: 42 });
    await useSession().logout();
    expect(assign).toHaveBeenLastCalledWith("/");

    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new TypeError("network")));
    await useSession().logout();
    expect(assign).toHaveBeenLastCalledWith("/");
  });
});
