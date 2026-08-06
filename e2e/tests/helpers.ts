import { expect, Page, APIRequestContext } from "@playwright/test";

export const ADMIN = {
  email: process.env.CERBIX_ADMIN_EMAIL || "admin@cerbix.local",
  password: process.env.CERBIX_ADMIN_PASSWORD || "devpassword123",
};

// Signs in through the real login form and waits for the app shell.
export async function login(page: Page, email = ADMIN.email, password = ADMIN.password) {
  await page.goto("/login");
  await page.fill('input[type="email"]', email);
  await page.fill('input[type="password"]', password);
  await page.click('button[type="submit"]');
  await expect(page.locator("aside")).toBeVisible({ timeout: 15_000 });
}

// The page's request context shares the session cookie — arrange/cleanup via API.
export async function apiGet(page: Page, path: string): Promise<any> {
  const r = await page.request.get(path);
  expect(r.ok(), `GET ${path} -> ${r.status()}`).toBeTruthy();
  const body = await r.json();
  return body ?? []; // Go serializes empty lists as null
}
export async function apiSend(page: Page, method: "post" | "put" | "patch" | "delete", path: string, data?: unknown) {
  const r = await page.request[method](path, data === undefined ? undefined : { data });
  return r;
}

// First org/project of the workspace (the dev stack always has one).
export async function firstProject(page: Page): Promise<{ orgID: string; projectID: string }> {
  const orgs = await apiGet(page, "/api/v1/organizations");
  const orgID = orgs[0].id;
  const projects = await apiGet(page, `/api/v1/organizations/${orgID}/projects`);
  return { orgID, projectID: projects[0].id };
}

// Removes every monitor whose name carries the e2e- prefix (idempotent cleanup).
export async function cleanupMonitors(page: Page, projectID: string) {
  const mons = await apiGet(page, `/api/v1/projects/${projectID}/monitors`);
  for (const m of mons) {
    if ((m.name as string).startsWith("e2e-")) await apiSend(page, "delete", `/api/v1/monitors/${m.id}`);
  }
}
