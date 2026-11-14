import { BrowserContext, Page } from "@playwright/test";
import { test, expect } from "./fixtures";
import { apiGet, apiSend, firstProject } from "./helpers";

// S9a (sec-hardening-2 H-9a): no request leaving a public status page carries a Referer. The
// unlisted page's capability is `?token=` in its URL; the server serves the page
// `Referrer-Policy: no-referrer` and the support link carries rel="noopener noreferrer". This is
// asserted in the browser on a real click: the support site's request is intercepted and its
// headers inspected. The login page's support link is checked the same way in an anonymous
// context — no sign-in, no logout, the shared session untouched.

const SUPPORT = "https://support.e2e-referrer.invalid/help";

// clickSupport opens the support link from page and returns the headers the browser sent to it.
async function clickSupport(context: BrowserContext, page: Page): Promise<Record<string, string>> {
  let sent: Record<string, string> | undefined;
  await context.route("https://support.e2e-referrer.invalid/**", async (route) => {
    sent = await route.request().allHeaders();
    await route.fulfill({ status: 200, contentType: "text/plain", body: "support" });
  });
  const [popup] = await Promise.all([
    context.waitForEvent("page"),
    page.getByRole("link", { name: "Support" }).click(),
  ]);
  await expect.poll(() => sent !== undefined, { message: "the support request was never made" }).toBe(true);
  await popup.close();
  await context.unroute("https://support.e2e-referrer.invalid/**");
  return sent!;
}

test("the support link sends no Referer from a public status page or the login page (S9a)", async ({ page }) => {
  const { orgID } = await firstProject(page);
  const slug = "e2e-referrer";
  for (const p of await apiGet(page, `/api/v1/organizations/${orgID}/status-pages`)) {
    if (p.slug === slug) await apiSend(page, "delete", `/api/v1/status-pages/${p.id}`);
  }
  const before = await apiGet(page, "/api/v1/settings/branding");
  expect((await apiSend(page, "put", "/api/v1/settings/branding", { ...before, support_url: SUPPORT })).ok()).toBeTruthy();
  const created = await apiSend(page, "post", `/api/v1/organizations/${orgID}/status-pages`, {
    slug, title: "E2E Referrer", visibility: "unlisted",
  });
  expect(created.status()).toBe(201);
  const sp = await created.json();
  expect(sp.unlisted_token, "the org admin receives the token").toBeTruthy();
  try {
    // The unlisted page, opened by its secret link.
    const response = await page.goto(`/status/${slug}?token=${sp.unlisted_token}`);
    expect.soft(response!.headers()["referrer-policy"]).toBe("no-referrer");
    await expect(page.getByRole("link", { name: "Support" })).toBeVisible();
    const fromStatus = await clickSupport(page.context(), page);
    expect.soft(fromStatus.referer, `the support site received ${fromStatus.referer}`).toBeUndefined();

    // The login page, in an anonymous context.
    const anon = await page.context().browser()!.newContext({ storageState: { cookies: [], origins: [] } });
    try {
      const anonPage = await anon.newPage();
      await anonPage.goto((process.env.CERBIX_URL || "http://localhost:8080") + "/login");
      await expect(anonPage.getByRole("link", { name: "Support" })).toBeVisible();
      const fromLogin = await clickSupport(anon, anonPage);
      expect.soft(fromLogin.referer, `the support site received ${fromLogin.referer}`).toBeUndefined();
    } finally {
      await anon.close();
    }
  } finally {
    await apiSend(page, "delete", `/api/v1/status-pages/${sp.id}`);
    expect((await apiSend(page, "put", "/api/v1/settings/branding", before)).ok()).toBeTruthy();
  }
});
