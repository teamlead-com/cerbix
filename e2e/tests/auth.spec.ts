import { test, expect } from "./fixtures";
import { ADMIN, desktopNavAside, login } from "./helpers";

test.use({ storageState: { cookies: [], origins: [] } });

test.describe("authentication", () => {
  test("rejects a wrong password", async ({ page }) => {
    await page.goto("/login");
    await page.fill('input[type="email"]', ADMIN.email);
    await page.fill('input[type="password"]', "definitely-wrong");
    await page.click('button[type="submit"]');
    await expect(page.locator("text=Invalid email or password")).toBeVisible();
  });

  test("signs in locally and shows the build version", async ({ page }) => {
    await login(page);
    const navigation = desktopNavAside(page);
    await expect(navigation.getByRole("link", { name: "Settings" })).toBeVisible();
    // Sidebar footer carries the running build (iter D-0105).
    await expect(navigation.getByText(/cerbix (dev|v)/)).toBeVisible();
  });

  // Signed in in this test's OWN anonymous context (test.use above), never the shared session.
  // sec-hardening-2 H-9b: POST /auth/logout answers 200 {"redirect_to": …} and the SPA navigates
  // itself — signing out through the menu must land on the login page with the session gone.
  test("signing out through the menu ends the session", async ({ page }) => {
    await login(page);
    // The SPA navigates away the moment the answer arrives, after which the browser no longer holds
    // the response body; the route reads the server's real answer and passes it through unchanged.
    let answer: { status: number; body: unknown } | undefined;
    await page.route("**/auth/logout", async (route) => {
      const res = await route.fetch();
      answer = { status: res.status(), body: await res.json() };
      await route.fulfill({ response: res });
    });
    await page.getByRole("button", { name: "Account menu" }).click();
    await page.getByRole("menuitem", { name: "Sign out" }).click();
    await expect(page).toHaveURL(/\/login/);
    expect(answer, "POST /auth/logout was sent").toBeDefined();
    expect(answer!.status).toBe(200);
    expect(answer!.body).toEqual({ redirect_to: "/" });
    expect((await page.request.get("/api/v1/me")).status(), "the session survived sign-out").toBe(401);
  });

  test("unauthenticated API access is rejected", async ({ page }) => {
    const r = await page.request.get("/api/v1/organizations");
    expect(r.status()).toBe(401);
  });
});
