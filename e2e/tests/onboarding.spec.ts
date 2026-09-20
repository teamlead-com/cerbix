import { expect, test } from "@playwright/test";
import { apiSend, firstProject } from "./helpers";

test.describe("onboarding (FR-036 / NFR-030)", () => {
  test("guides an empty project and stays additive on an existing installation", async ({ page }) => {
    const { orgID } = await firstProject(page);
    const slug = `e2e-onboarding-${Date.now()}`;
    const projectResponse = await apiSend(page, "post", `/api/v1/organizations/${orgID}/projects`, {
      slug,
      name: "E2E Onboarding",
    });
    expect(projectResponse.ok(), "create isolated onboarding project").toBeTruthy();
    const project = await projectResponse.json();

    let monitorID = "";
    try {
      await page.setViewportSize({ width: 430, height: 900 });
      await page.addInitScript(
        ([selectedOrgID, selectedProjectID]) => {
          localStorage.setItem("cerbix.org", selectedOrgID);
          localStorage.setItem("cerbix.project", selectedProjectID);
          for (let index = localStorage.length - 1; index >= 0; index--) {
            const key = localStorage.key(index);
            if (key?.startsWith("cerbix.onboarding.dismissed.")) localStorage.removeItem(key);
          }
        },
        [orgID, project.id] as [string, string],
      );

      await page.goto("/");
      const guide = page.getByTestId("onboarding-guide");
      await expect(guide).toHaveAttribute("data-state", "project_no_monitor");
      await expect(guide.getByRole("heading", { name: "Choose the first real monitor" })).toBeVisible();
      await expect(guide.locator('[aria-current="step"]')).toContainText("Monitor");
      const overflow = await page.evaluate(() => [...document.querySelectorAll<HTMLElement>("body *")]
        .filter((element) => element.getBoundingClientRect().right > window.innerWidth + 1)
        .map((element) => ({
          tag: element.tagName.toLowerCase(),
          className: element.className,
          right: Math.round(element.getBoundingClientRect().right),
        }))
        .slice(0, 10));
      expect(overflow, "narrow guide has no page overflow").toEqual([]);

      await guide.getByRole("link", { name: "Open full monitor form" }).focus();
      await page.keyboard.press("Escape");
      await expect(guide).toHaveCount(0);
      await expect(page.getByTestId("onboarding-entry")).toBeFocused();
      await page.getByTestId("onboarding-entry").click();
      await expect(guide).toHaveAttribute("data-state", "project_no_monitor");
      await guide.getByRole("link", { name: "Open full monitor form" }).click();
      await expect(page).toHaveURL(/\/monitors\/new\?onboarding=1$/);

      const monitorResponse = await apiSend(page, "post", `/api/v1/projects/${project.id}/monitors`, {
        name: `e2e-${slug}`,
        type: "push",
        target: "",
        region: "core",
        interval_seconds: 60,
        timeout_seconds: 10,
        grace_seconds: 120,
      });
      expect(monitorResponse.ok(), "create onboarding monitor").toBeTruthy();
      const monitor = await monitorResponse.json();
      monitorID = monitor.id;
      expect(monitor.push_token, "push token returned once at creation").toBeTruthy();
      expect((await page.request.post(`/api/v1/public/push/${monitor.push_token}`)).ok(), "persist first heartbeat").toBeTruthy();

      await page.goto("/");
      await expect(guide).toHaveCount(0);
      await expect(page.getByText(monitor.name, { exact: true })).toBeVisible();
      await expect(page.getByText("Availability · 30d", { exact: true })).toBeVisible();
      await expect(page.getByText("Monitors up", { exact: true })).toBeVisible();
      await expect(page.getByText("Error budget · 30d", { exact: true })).toBeVisible();
      await expect(page.getByText("P95 latency · 30d", { exact: true })).toBeVisible();
      await expect(page.getByLabel("90-day project availability")).toBeVisible();

      await page.getByTestId("onboarding-entry").click();
      await expect(guide).toHaveAttribute("data-state", "complete_existing");
      await expect(guide.getByRole("heading", { name: "First useful result already exists" })).toBeVisible();
      await expect(page.getByText(monitor.name, { exact: true })).toBeVisible();
      await expect(page.getByLabel("90-day project availability")).toBeVisible();
    } finally {
      if (monitorID) await apiSend(page, "delete", `/api/v1/monitors/${monitorID}`);
      await apiSend(page, "delete", `/api/v1/projects/${project.id}`);
    }
  });
});
