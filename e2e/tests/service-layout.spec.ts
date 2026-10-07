import { test, expect } from "@playwright/test";
import { apiGet, apiSend, ensureE2EWorkspace } from "./helpers";

// iter-0202 BUG-0202-3: the paging-ownership card's escalation-policy row sat OUTSIDE the card's
// padded body, so it ran to the card's left edge, touched its bottom and used the page's larger
// default type. The row is part of the card, so it lines up with the body and uses its type.

test.describe("service paging card layout", () => {
  test.afterEach(async ({ page }) => {
    const { projectID } = await ensureE2EWorkspace(page);
    for (const s of await apiGet(page, `/api/v1/projects/${projectID}/services`)) {
      if ((s.service.slug as string).startsWith("e2e-layout")) {
        await apiSend(page, "delete", `/api/v1/projects/${projectID}/services/${s.service.id}`);
      }
    }
  });

  test("the escalation-policy row lines up with the card body", async ({ page }) => {
    await page.goto("/");
    const { projectID } = await ensureE2EWorkspace(page);
    const created = await apiSend(page, "post", `/api/v1/projects/${projectID}/services`, {
      slug: "e2e-layout-paging", name: "E2E Layout Paging",
    });
    expect(created.status()).toBe(201);
    const svc = await created.json();

    await page.goto(`/services/${svc.id}`);
    const card = page.getByTestId("service-alerting");
    const row = page.getByTestId("alerting-escalation");
    await expect(row).toBeVisible();
    const label = row.locator("span").first();
    // The body's first line of text is the reference for both edge and type.
    const body = page.getByTestId("alerting-body");
    await expect(body).toBeVisible();

    const cardBox = (await card.boundingBox())!;
    const bodyBox = (await body.boundingBox())!;
    const bodyPadLeft = await body.evaluate((el) => parseFloat(getComputedStyle(el).paddingLeft));
    const labelBox = (await label.boundingBox())!;
    const rowBox = (await row.boundingBox())!;

    expect(labelBox.x, "the label starts at the card's edge instead of the body's padding")
      .toBeCloseTo(bodyBox.x + bodyPadLeft, 0);
    expect(cardBox.y + cardBox.height - (rowBox.y + rowBox.height), "the row touches the card's bottom edge")
      .toBeGreaterThanOrEqual(0);
    const rowContentBottom = await row.evaluate((el) => {
      const r = el.getBoundingClientRect();
      return r.bottom - parseFloat(getComputedStyle(el).paddingBottom);
    });
    expect(rowBox.y + rowBox.height - rowContentBottom, "the row has no bottom padding").toBeGreaterThanOrEqual(8);
    const [bodySize, rowSize] = await Promise.all([
      body.evaluate((el) => getComputedStyle(el).fontSize),
      row.evaluate((el) => getComputedStyle(el).fontSize),
    ]);
    expect(rowSize, "the row does not use the card body's type size").toBe(bodySize);
  });
});
