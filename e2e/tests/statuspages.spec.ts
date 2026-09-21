import { test, expect } from "@playwright/test";
import { apiGet, apiSend, firstProject } from "./helpers";

test.describe("status pages", () => {
  test("full lifecycle: page, grouped component, preview, feeds, subscribers", async ({ page }) => {
    const { orgID } = await firstProject(page);
    const slug = "e2e-status";
    // Fresh start (idempotent re-runs).
    const existing = await apiGet(page, `/api/v1/organizations/${orgID}/status-pages`);
    for (const p of existing) if (p.slug === slug) await apiSend(page, "delete", `/api/v1/status-pages/${p.id}`);

    const created = await apiSend(page, "post", `/api/v1/organizations/${orgID}/status-pages`, {
      slug, title: "E2E Status", visibility: "public",
    });
    expect(created.status()).toBe(201);
    const sp = await created.json();
    try {
      // Component with the once-curl-only fields (D-0114).
      const comp = await apiSend(page, "post", `/api/v1/status-pages/${sp.id}/components`, {
        name: "e2e-comp", group: "E2E group", description: "component description", position: 5, manual_status: "operational",
      });
      expect(comp.status()).toBe(201);

      // Public render shows the group heading and the description.
      await page.goto(`/status/${slug}`);
      await expect(page.locator("text=E2E group")).toBeVisible();
      await expect(page.locator("text=component description")).toBeVisible();

      // Feeds answer for a public page.
      const rss = await page.request.get(`/api/v1/public/status-pages/${slug}/feed?format=rss`);
      expect(rss.status()).toBe(200);

      // Internal visibility: public 404s, ?preview renders with the banner (D-0114),
      // and the authed feed answers while the public one refuses.
      expect((await apiSend(page, "patch", `/api/v1/status-pages/${sp.id}`, { title: "E2E Status", visibility: "internal" })).ok()).toBeTruthy();
      const pub = await page.request.get(`/api/v1/public/status-pages/${slug}`);
      expect(pub.status()).toBe(404);
      await page.goto(`/status/${slug}?preview=${sp.id}`);
      await expect(page.getByText(/Internal page/)).toBeVisible();
      expect((await page.request.get(`/api/v1/status-pages/${sp.id}/feed?format=rss`)).status()).toBe(200);

      // Back to public: the anonymous subscribe form creates a pending subscriber
      // the owner sees in the editor (D-0116).
      await apiSend(page, "patch", `/api/v1/status-pages/${sp.id}`, { title: "E2E Status", visibility: "public" });
      const sub = await page.request.post(`/api/v1/public/status-pages/${slug}/subscribers`, { data: { email: "e2e-sub@example.com" } });
      expect(sub.ok()).toBeTruthy();
      const subs = await apiGet(page, `/api/v1/status-pages/${sp.id}/subscribers`);
      const mine = subs.find((s: any) => s.email === "e2e-sub@example.com");
      expect(mine).toBeTruthy();
      expect(mine.confirmed_at ?? null).toBeNull();
      expect((await apiSend(page, "delete", `/api/v1/status-pages/${sp.id}/subscribers/${mine.id}`)).ok()).toBeTruthy();
    } finally {
      await apiSend(page, "delete", `/api/v1/status-pages/${sp.id}`);
    }
  });

  test("incident-aware hero matches on public and authenticated preview at desktop and 430px", async ({
    page,
  }) => {
    const { orgID } = await firstProject(page);
    const slug = "e2e-status-incident-aware";
    const projectResponse = await apiSend(page, "post", `/api/v1/organizations/${orgID}/projects`, {
      slug: `e2e-status-incident-aware-${Date.now()}`,
      name: "E2E Incident-aware Status",
    });
    expect(projectResponse.status()).toBe(201);
    const project = await projectResponse.json();
    const projectID = project.id as string;
    for (const existing of await apiGet(
      page,
      `/api/v1/organizations/${orgID}/status-pages`,
    )) {
      if (existing.slug === slug) {
        await apiSend(page, "delete", `/api/v1/status-pages/${existing.id}`);
      }
    }

    const statusPageResponse = await apiSend(
      page,
      "post",
      `/api/v1/organizations/${orgID}/status-pages`,
      {
        slug,
        title: "E2E Incident-aware Status",
        visibility: "public",
        project_id: projectID,
      },
    );
    expect(statusPageResponse.status()).toBe(201);
    const statusPage = await statusPageResponse.json();
    let incident: any = null;
    let monitor: any = null;

    const assertHero = async () => {
      const hero = page.getByTestId("overall-status");
      await expect(
        page.getByRole("heading", { level: 1, name: "1 active incident" }),
      ).toBeVisible();
      await expect(
        page.getByTestId("overall-status-supporting"),
      ).toHaveText(
        "Major impact. All measured services are currently operational. 1 component on this page has no measurement.",
      );
      await expect(hero).not.toContainText("All systems operational");
      await expect(hero).toHaveAttribute("data-visual", "warning");
      await expect(hero).not.toHaveClass(/bg-up-weak/);
      await expect(page.getByTestId("overall-status-icon")).toHaveAttribute(
        "data-icon",
        "alert",
      );
      await expect(page.getByTestId("overall-status-check-icon")).toHaveCount(
        0,
      );
      await expect(page.getByTestId("overall-status-alert-icon")).toBeVisible();
    };

    const assertNoOverflow = async () => {
      const overflow = await page.evaluate(() => ({
        viewport: window.innerWidth,
        document: document.documentElement.scrollWidth,
        body: document.body.scrollWidth,
      }));
      expect(overflow.document).toBeLessThanOrEqual(overflow.viewport);
      expect(overflow.body).toBeLessThanOrEqual(overflow.viewport);
    };

    try {
      const monitorResponse = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
        name: "e2e-status-incident-aware-monitor", type: "http",
        target: "https://example.com/status-incident-aware", interval_seconds: 300,
        timeout_seconds: 5, region: "core", enabled: false,
      });
      expect(monitorResponse.status()).toBe(201);
      monitor = await monitorResponse.json();
      const componentResponse = await apiSend(
        page,
        "post",
        `/api/v1/status-pages/${statusPage.id}/components`,
        {
          name: "Checkout API",
          group: "Customer services",
          description: "Public checkout traffic",
          position: 1,
          manual_status: "operational",
        },
      );
      expect(componentResponse.status()).toBe(201);
      const incidentComponentResponse = await apiSend(
        page,
        "post",
        `/api/v1/status-pages/${statusPage.id}/components`,
        {
          name: "Incident signal",
          group: "Customer services",
          position: 2,
          monitor_id: monitor.id,
        },
      );
      expect(incidentComponentResponse.status()).toBe(201);
      const incidentResponse = await apiSend(
        page,
        "post",
        `/api/v1/projects/${projectID}/incidents`,
        {
          title: "e2e incident-aware major",
          impact: "major",
          monitor_id: monitor.id,
          body: "Checkout requests are degraded while recovery proceeds.",
        },
      );
      expect(incidentResponse.status()).toBe(201);
      incident = await incidentResponse.json();

      await page.setViewportSize({ width: 1280, height: 900 });
      await page.goto(`/status/${slug}`);
      await assertHero();
      const publicComposition = await page
        .getByTestId("overall-status")
        .evaluate((hero) => ({
          headline: hero.querySelector("h1")?.textContent,
          supporting: hero.querySelector('[data-testid="overall-status-supporting"]')?.textContent,
          classes: hero.className,
          visual: hero.getAttribute("data-visual"),
          icon: hero
            .querySelector('[data-testid="overall-status-icon"]')
            ?.getAttribute("data-icon"),
        }));

      await page.setViewportSize({ width: 430, height: 932 });
      await page.reload();
      await assertHero();
      await assertNoOverflow();

      const internalResponse = await apiSend(
        page,
        "patch",
        `/api/v1/status-pages/${statusPage.id}`,
        {
          title: "E2E Incident-aware Status",
          visibility: "internal",
        },
      );
      expect(internalResponse.ok()).toBeTruthy();

      await page.setViewportSize({ width: 1280, height: 900 });
      await page.goto(`/status/${slug}?preview=${statusPage.id}`);
      await expect(
        page.getByText("Internal page — visible to signed-in members only", {
          exact: false,
        }),
      ).toBeVisible();
      await assertHero();
      const previewComposition = await page
        .getByTestId("overall-status")
        .evaluate((hero) => ({
          headline: hero.querySelector("h1")?.textContent,
          supporting: hero.querySelector('[data-testid="overall-status-supporting"]')?.textContent,
          classes: hero.className,
          visual: hero.getAttribute("data-visual"),
          icon: hero
            .querySelector('[data-testid="overall-status-icon"]')
            ?.getAttribute("data-icon"),
        }));
      expect(previewComposition).toEqual(publicComposition);

      await page.setViewportSize({ width: 430, height: 932 });
      await page.reload();
      await assertHero();
      await assertNoOverflow();
    } finally {
      if (incident) {
        await apiSend(page, "post", `/api/v1/incidents/${incident.id}/updates`, {
          status: "resolved",
          body: "e2e cleanup",
        });
      }
      await apiSend(page, "delete", `/api/v1/status-pages/${statusPage.id}`);
      if (monitor) await apiSend(page, "delete", `/api/v1/monitors/${monitor.id}`);
      await apiSend(page, "delete", `/api/v1/projects/${projectID}`);
    }
  });

  // iter-0181, reported from production: adding a component whose source is a SERVICE answered
  // `400 Bad Request` with `invalid JSON body`. The handler did not name `service_id` while the
  // contract declared it and the SPA sent it, and `DisallowUnknownFields` turned the request into a
  // malformed-JSON report about JSON that was correct. Every unit-level double already implemented
  // the derivation the handler lacked, so nothing below this line could fail either — which is why
  // the live assertion is worth its seconds.
  test("a component can be bound to a service", async ({ page }) => {
    const { orgID, projectID } = await firstProject(page);
    const slug = "e2e-status-svc";
    const svcSlug = "e2e-status-svc-binding";

    for (const p of await apiGet(page, `/api/v1/organizations/${orgID}/status-pages`)) {
      if (p.slug === slug) await apiSend(page, "delete", `/api/v1/status-pages/${p.id}`);
    }
    for (const s of await apiGet(page, `/api/v1/projects/${projectID}/services`)) {
      if (s.service.slug === svcSlug) await apiSend(page, "delete", `/api/v1/projects/${projectID}/services/${s.service.id}`);
    }

    const svcRes = await apiSend(page, "post", `/api/v1/projects/${projectID}/services`, {
      slug: svcSlug, name: "E2E Status Binding",
    });
    expect(svcRes.status()).toBe(201);
    const svc = await svcRes.json();
    let sp: { id: string } | null = null;
    try {
      const pageRes = await apiSend(page, "post", `/api/v1/organizations/${orgID}/status-pages`, {
        slug, title: "E2E Service Status", visibility: "public",
      });
      expect(pageRes.status()).toBe(201);
      sp = await pageRes.json();
      const comp = await apiSend(page, "post", `/api/v1/status-pages/${sp.id}/components`, {
        name: "e2e-service-comp", service_id: svc.id, group: "Services",
      });
      // The reported symptom, named: a 400 here is the defect returning, not a bad fixture.
      expect(await comp.text()).not.toContain("invalid JSON body");
      expect(comp.status()).toBe(201);
      const created = await comp.json();
      // The source is DERIVED from the binding; the caller never sends it.
      expect(created.source).toBe("service");
      expect(created.service_id).toBe(svc.id);
    } finally {
      if (sp) await apiSend(page, "delete", `/api/v1/status-pages/${sp.id}`);
      await apiSend(page, "delete", `/api/v1/projects/${projectID}/services/${svc.id}`);
    }
  });

  test("service-first incident density stays navigable on desktop and 430px", async ({ page }) => {
    const { orgID } = await firstProject(page);
    const slug = "e2e-status-density";
    const monitorName = "e2e-status-density-monitor";
    const incidentPrefix = "e2e-status-density-";

    const projectResponse = await apiSend(page, "post", `/api/v1/organizations/${orgID}/projects`, {
      slug: `e2e-status-density-${Date.now()}`,
      name: "E2E Status Density",
    });
    expect(projectResponse.status()).toBe(201);
    const project = await projectResponse.json();
    const projectID = project.id as string;

    for (const statusPage of await apiGet(page, `/api/v1/organizations/${orgID}/status-pages`)) {
      if (statusPage.slug === slug) await apiSend(page, "delete", `/api/v1/status-pages/${statusPage.id}`);
    }

    const monitorResponse = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
      name: monitorName, type: "http", target: "https://example.com/status-density",
      interval_seconds: 300, timeout_seconds: 5, region: "core", enabled: false,
    });
    expect(monitorResponse.status()).toBe(201);
    const monitor = await monitorResponse.json();
    const statusPageResponse = await apiSend(page, "post", `/api/v1/organizations/${orgID}/status-pages`, {
      slug, title: "E2E Incident Density", visibility: "public",
    });
    expect(statusPageResponse.status()).toBe(201);
    const statusPage = await statusPageResponse.json();
    const createdIncidents: any[] = [];

    try {
      const componentResponse = await apiSend(page, "post", `/api/v1/status-pages/${statusPage.id}/components`, {
        name: "Checkout API", group: "Customer services", description: "Public checkout traffic",
        position: 1, monitor_id: monitor.id,
      });
      expect(componentResponse.status()).toBe(201);

      for (const [index, impact] of ["minor", "critical", "none", "major"].entries()) {
        const incidentResponse = await apiSend(page, "post", `/api/v1/projects/${projectID}/incidents`, {
          title: `${incidentPrefix}${impact}`, impact, monitor_id: monitor.id,
          body: index === 1
            ? "Checkout requests are timing out for a subset of customers while the recovery proceeds. This text proves the compact preview and expanded timeline remain readable."
            : `Public update for ${impact} impact`,
        });
        expect(incidentResponse.status()).toBe(201);
        createdIncidents.push(await incidentResponse.json());
      }

      await page.setViewportSize({ width: 1280, height: 900 });
      await page.goto(`/status/${slug}`);
      await expect(page.getByRole("heading", { name: "Current status by service" })).toBeVisible();
      await expect(page.getByRole("heading", { name: "Active incidents (4)" })).toBeVisible();
      await expect(page.getByTestId("active-incident-header")).toHaveCount(4);
      await expect(page.getByText("auto", { exact: true })).toHaveCount(0);
      expect(await page.locator("[data-section]").evaluateAll((sections) => sections.map((section) => section.getAttribute("data-section")))).toEqual([
        "overall", "components", "active-incidents", "subscribe",
      ]);

      const componentLink = page.getByTestId("component-incident-link").first();
      await expect(componentLink).toHaveText("4 active incidents");
      const firstIncident = page.getByTestId("active-incident-header").first();
      await expect(firstIncident).toHaveAttribute("aria-expanded", "false");
      await firstIncident.focus();
      await page.keyboard.press("Enter");
      await expect(firstIncident).toHaveAttribute("aria-expanded", "true");
      await page.keyboard.press("Space");
      await expect(firstIncident).toHaveAttribute("aria-expanded", "false");
      await componentLink.click();
      await expect(firstIncident).toHaveAttribute("aria-expanded", "true");
      await expect(firstIncident).toBeFocused();
      await expect(page.getByTestId("active-incident-panel").first()).toContainText("Latest");
      expect(page.url()).toBe(`${process.env.CERBIX_URL || "http://localhost:8080"}/status/${slug}`);
      expect(page.url()).not.toContain(monitor.id);
      for (const incident of createdIncidents) expect(page.url()).not.toContain(incident.id);

      await page.setViewportSize({ width: 430, height: 932 });
      await page.reload();
      await expect(page.getByRole("heading", { name: "Active incidents (4)" })).toBeVisible();
      await expect(page.getByTestId("latest-update-preview").first()).toBeVisible();
      const overflow = await page.evaluate(() => ({
        viewport: window.innerWidth,
        document: document.documentElement.scrollWidth,
        body: document.body.scrollWidth,
      }));
      expect(overflow.document).toBeLessThanOrEqual(overflow.viewport);
      expect(overflow.body).toBeLessThanOrEqual(overflow.viewport);
    } finally {
      for (const incident of createdIncidents) {
        await apiSend(page, "post", `/api/v1/incidents/${incident.id}/updates`, { status: "resolved", body: "e2e cleanup" });
      }
      await apiSend(page, "delete", `/api/v1/status-pages/${statusPage.id}`);
      await apiSend(page, "delete", `/api/v1/monitors/${monitor.id}`);
      await apiSend(page, "delete", `/api/v1/projects/${projectID}`);
    }
  });
});
