import { expect, Page } from "@playwright/test";

export const ADMIN = {
  email: process.env.CERBIX_ADMIN_EMAIL || "admin@cerbix.local",
  password: process.env.CERBIX_ADMIN_PASSWORD || "devpassword123",
};

export const E2E_WORKSPACE = {
  orgSlug: "e2e-harness",
  orgName: "E2E Harness",
  projectSlug: "e2e-harness",
  projectName: "E2E Harness",
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

// Creates the one tenant owned by the browser harness. A fresh Compose database
// bootstraps only the admin user; tests must never borrow or clean an arbitrary
// user-created tenant merely because it happens to be first in a response.
export async function ensureE2EWorkspace(page: Page): Promise<{ orgID: string; projectID: string }> {
  const browserRequest = async (method: "GET" | "POST", path: string, data?: unknown) =>
    page.evaluate(async ({ method, path, data }) => {
      const response = await fetch(path, {
        method,
        credentials: "same-origin",
        headers: data === undefined ? undefined : { "Content-Type": "application/json" },
        body: data === undefined ? undefined : JSON.stringify(data),
      });
      return { status: response.status, body: await response.json() };
    }, { method, path, data });

  const listedOrgs = await browserRequest("GET", "/api/v1/organizations");
  expect(listedOrgs.status, "list organizations after login").toBe(200);
  const orgs = Array.isArray(listedOrgs.body) ? listedOrgs.body : [];
  let org = orgs.find((candidate: any) => candidate.slug === E2E_WORKSPACE.orgSlug);
  if (!org) {
    const created = await browserRequest("POST", "/api/v1/organizations", {
      slug: E2E_WORKSPACE.orgSlug,
      name: E2E_WORKSPACE.orgName,
    });
    expect(created.status, "create dedicated E2E organization").toBe(201);
    org = created.body;
  }

  const listedProjects = await browserRequest("GET", `/api/v1/organizations/${org.id}/projects`);
  expect(listedProjects.status, "list dedicated E2E projects").toBe(200);
  const projects = Array.isArray(listedProjects.body) ? listedProjects.body : [];
  let project = projects.find((candidate: any) => candidate.slug === E2E_WORKSPACE.projectSlug);
  if (!project) {
    const created = await browserRequest("POST", `/api/v1/organizations/${org.id}/projects`, {
      slug: E2E_WORKSPACE.projectSlug,
      name: E2E_WORKSPACE.projectName,
    });
    expect(created.status, "create dedicated E2E project").toBe(201);
    project = created.body;
  }
  return { orgID: org.id, projectID: project.id };
}

// Exact org/project owned by the test harness (never response element zero).
export async function firstProject(page: Page): Promise<{ orgID: string; projectID: string }> {
  const orgs = await apiGet(page, "/api/v1/organizations");
  const org = orgs.find((candidate: any) => candidate.slug === E2E_WORKSPACE.orgSlug);
  expect(org, "dedicated E2E organization from global setup").toBeTruthy();
  const projects = await apiGet(page, `/api/v1/organizations/${org.id}/projects`);
  const project = projects.find((candidate: any) => candidate.slug === E2E_WORKSPACE.projectSlug);
  expect(project, "dedicated E2E project from global setup").toBeTruthy();
  return { orgID: org.id, projectID: project.id };
}

// Removes every monitor whose name carries the e2e- prefix (idempotent cleanup).
export async function cleanupMonitors(page: Page, projectID: string) {
  const mons = await apiGet(page, `/api/v1/projects/${projectID}/monitors`);
  for (const m of mons) {
    if ((m.name as string).startsWith("e2e-")) await apiSend(page, "delete", `/api/v1/monitors/${m.id}`);
  }
}
