import { test, expect, type Page } from "@playwright/test";
import { apiGet, apiSend, firstProject } from "./helpers";

// iter-0203 (func-status-pages-incidents.md §13, FR-039 / NFR-033): the live page lists at most ten
// past incidents and links to a month-paged history. Everything here is real: a fresh project, a
// public status page, and incidents opened and resolved through the API. The product cannot
// backdate a resolution, so every incident lands in the current UTC month; month boundaries, the
// keyset and the partial oldest month's SQL are proved in internal/store against PostgreSQL.

const SLUG = "e2e-status-history";
const PAST = 52;

async function noHorizontalOverflow(page: Page) {
  const [scroll, inner] = await page.evaluate(() => [document.documentElement.scrollWidth, window.innerWidth]);
  expect(scroll, "the page scrolls sideways").toBeLessThanOrEqual(inner);
}

test.describe("status page incident history", () => {
  test("ten on the page, a history paged by month with Show more, at desktop and 430 px", async ({ page }) => {
    test.setTimeout(180_000);
    await page.goto("/");
    const { orgID } = await firstProject(page);
    for (const existing of await apiGet(page, `/api/v1/organizations/${orgID}/status-pages`)) {
      if (existing.slug === SLUG) await apiSend(page, "delete", `/api/v1/status-pages/${existing.id}`);
    }
    const projectRes = await apiSend(page, "post", `/api/v1/organizations/${orgID}/projects`, {
      slug: `e2e-status-history-${Date.now()}`, name: "E2E Status History",
    });
    expect(projectRes.status()).toBe(201);
    const projectID = (await projectRes.json()).id as string;
    const spRes = await apiSend(page, "post", `/api/v1/organizations/${orgID}/status-pages`, {
      slug: SLUG, title: "E2E Status History", visibility: "public", project_id: projectID,
    });
    expect(spRes.status()).toBe(201);
    const sp = await spRes.json();

    try {
      for (let i = 0; i < PAST; i++) {
        const inc = await apiSend(page, "post", `/api/v1/projects/${projectID}/incidents`, {
          title: `e2e history ${String(i).padStart(2, "0")}`, impact: "minor", body: "opened",
        });
        expect(inc.status(), `open incident ${i}`).toBe(201);
        const id = (await inc.json()).id as string;
        const res = await apiSend(page, "post", `/api/v1/incidents/${id}/updates`, { status: "resolved", body: "fixed" });
        expect(res.ok(), `resolve incident ${i}`).toBeTruthy();
      }

      // The page: ten rows, the scope says so, and the link to the rest.
      await page.setViewportSize({ width: 1280, height: 900 });
      await page.goto(`/status/${SLUG}`);
      await expect(page.getByTestId("past-incident-row")).toHaveCount(10);
      await expect(page.getByTestId("past-incidents-scope")).toHaveText("latest 10 · last 90 days");
      await expect(page.getByTestId("past-incident-row").first()).toContainText(`e2e history ${PAST - 1}`);
      const link = page.getByTestId("past-incidents-history-link");
      await expect(link).toBeVisible();
      await link.click();

      // The history: the current UTC month, 50 rows, Show more for the rest — in place, URL unchanged.
      await expect(page).toHaveURL(new RegExp(`/status/${SLUG}/history$`));
      await expect(page.getByRole("heading", { level: 1, name: "Incident history" })).toBeVisible();
      const current = page.locator('[data-testid="history-month"][aria-current="page"]');
      await expect(current).toContainText(`${PAST} incidents`);
      await expect(page.getByTestId("past-incident-row")).toHaveCount(50);
      await expect(page.getByText(`showing 50 of ${PAST}`)).toBeVisible();
      const urlBefore = page.url();
      await page.getByTestId("history-show-more").click();
      await expect(page.getByTestId("past-incident-row")).toHaveCount(PAST);
      await expect(page.getByTestId("history-show-more")).toHaveCount(0);
      expect(page.url(), "Show more put something into the URL").toBe(urlBefore);
      const titles = await page.getByTestId("past-incident-row").locator("button .text-\\[13\\.5px\\]").allTextContents();
      expect(new Set(titles).size, "an incident appears twice across the two pages").toBe(PAST);

      // An older month: empty, said in words; the oldest one says where history begins.
      const months = page.getByTestId("history-month");
      const count = await months.count();
      expect(count, "a 90-day history touches three or four months").toBeGreaterThanOrEqual(3);
      await page.getByTestId("history-older").click();
      await expect(page).toHaveURL(/month=\d{4}-\d{2}/);
      await expect(page.getByTestId("history-empty")).toContainText("No incidents were resolved in");
      await expect(page.locator("#history-month-heading")).toBeFocused();
      await months.nth(count - 1).click();
      await expect(page.getByTestId("history-partial")).toContainText("History covers the last 90 days");
      await expect(page.getByTestId("history-older")).toBeDisabled();

      // 430 px: the navigator wraps, nothing scrolls sideways.
      await page.setViewportSize({ width: 430, height: 932 });
      await months.first().click();
      await expect(page.getByTestId("past-incident-row")).toHaveCount(50);
      await noHorizontalOverflow(page);
      await page.getByTestId("past-incident-row").first().locator("button").click();
      await expect(page.locator("#history-incident-panel-1")).toBeVisible();
      await noHorizontalOverflow(page);

      // An unlisted page: the link and the history keep the token; without it the history is 404.
      const patched = await apiSend(page, "patch", `/api/v1/status-pages/${sp.id}`, { visibility: "unlisted" });
      expect(patched.ok()).toBeTruthy();
      const token = (await patched.json()).unlisted_token as string;
      expect(token, "an unlisted page has a token").toBeTruthy();
      await page.setViewportSize({ width: 1280, height: 900 });
      await page.goto(`/status/${SLUG}?token=${token}`);
      await page.getByTestId("past-incidents-history-link").click();
      await expect(page).toHaveURL(new RegExp(`/status/${SLUG}/history\\?token=`));
      await expect(page.getByTestId("past-incident-row")).toHaveCount(50);
      const anon = await page.request.get(`/api/v1/public/status-pages/${SLUG}/history`);
      expect(anon.status(), "the unlisted history answered without its token").toBe(404);
    } finally {
      await apiSend(page, "delete", `/api/v1/status-pages/${sp.id}`);
      await apiSend(page, "delete", `/api/v1/projects/${projectID}`);
    }
  });
});
