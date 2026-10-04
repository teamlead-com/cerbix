import { test, expect } from "@playwright/test";
import { desktopNavAside } from "./helpers";

test.use({ storageState: { cookies: [], origins: [] } });

test("desktop navigation is distinct from a preceding contextual aside", async ({ page }) => {
  await page.setContent(`
    <div data-testid="app-shell">
      <main><aside><a href="/wrong">Settings</a><div>cerbix v9.9.9</div></aside></main>
      <aside><nav><a href="/settings">Settings</a></nav><div>cerbix dev</div></aside>
    </div>
  `);

  const navigation = desktopNavAside(page);
  await expect(navigation).toBeVisible();
  await expect(navigation.getByRole("link", { name: "Settings" })).toHaveAttribute("href", "/settings");
  await expect(navigation.getByText("cerbix dev")).toBeVisible();
});

test("a contextual aside alone is not desktop navigation", async ({ page }) => {
  await page.setContent(`
    <div data-testid="app-shell">
      <main><aside><a href="/wrong">Settings</a><div>cerbix v9.9.9</div></aside></main>
    </div>
  `);

  await expect(desktopNavAside(page)).toHaveCount(0);
});
