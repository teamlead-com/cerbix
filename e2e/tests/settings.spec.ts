import { test, expect } from "@playwright/test";
import { apiGet, apiSend, firstProject } from "./helpers";

test.describe("settings", () => {
  test("branding round-trips and renders on the login page", async ({ page }) => {
    const before = await apiGet(page, "/api/v1/settings/branding");
    const set = { ...before, footer_text: "e2e footer", support_url: "https://support.e2e.example" };
    expect((await apiSend(page, "put", "/api/v1/settings/branding", set)).ok()).toBeTruthy();
    try {
      // A fresh anonymous context views the login page — never log the shared
      // session out (specs share one server-side session; see the config note).
      const anon = await page.context().browser()!.newContext({ storageState: { cookies: [], origins: [] } });
      const anonPage = await anon.newPage();
      await anonPage.goto((process.env.CERBIX_URL || "http://localhost:8080") + "/login");
      await expect(anonPage.locator("text=e2e footer")).toBeVisible();
      await expect(anonPage.locator('a[href="https://support.e2e.example"]')).toBeVisible();
      await anon.close();
    } finally {
      const restore = await apiSend(page, "put", "/api/v1/settings/branding", before);
      expect(restore.ok()).toBeTruthy();
    }
  });

  test("global silence keeps its expiry across the toggle (D-0112)", async ({ page }) => {
    const until = new Date(Date.now() + 3600_000).toISOString();
    expect((await apiSend(page, "put", "/api/v1/settings/alerting", { global_silence: { enabled: true, until } })).ok()).toBeTruthy();
    try {
      const got = await apiGet(page, "/api/v1/settings/alerting");
      expect(got.global_silence.until).toBeTruthy();
      await page.goto("/settings?tab=alerting");
      await expect(page.locator('input[type="datetime-local"]')).toBeVisible();
      await expect(page.getByText(/silenced instance-wide until/)).toBeVisible();
    } finally {
      await apiSend(page, "put", "/api/v1/settings/alerting", { global_silence: { enabled: false } });
    }
  });

  test("minimum password length is live policy (D-0108)", async ({ page }) => {
    const before = await apiGet(page, "/api/v1/settings/auth-policy");
    expect((await apiSend(page, "put", "/api/v1/settings/auth-policy", { ...before, min_password_len: 16 })).ok()).toBeTruthy();
    try {
      const r = await apiSend(page, "post", "/api/v1/me/password", {
        current_password: process.env.CERBIX_ADMIN_PASSWORD || "devpassword123",
        new_password: "only12charsX",
      });
      expect(r.status()).toBe(400);
    } finally {
      await apiSend(page, "put", "/api/v1/settings/auth-policy", before);
    }
  });

  test("api tokens: issue shows the secret once and lists the issuer", async ({ page }) => {
    const { orgID } = await firstProject(page);
    // Leftovers from interrupted runs would trip strict-mode locators.
    for (const t of await apiGet(page, `/api/v1/organizations/${orgID}/tokens`)) {
      if ((t.name as string).startsWith("e2e-")) await apiSend(page, "delete", `/api/v1/tokens/${t.id}`);
    }
    const r = await apiSend(page, "post", `/api/v1/organizations/${orgID}/tokens`, { name: "e2e-token", role: "viewer" });
    expect(r.status()).toBe(201);
    // The create response is {token: <plaintext>, api_token: {...}} — the row
    // id lives in api_token (a wrong path here once produced DELETE /tokens/undefined).
    const tok = (await r.json()).api_token;
    expect(tok.id).toBeTruthy();
    try {
      await page.goto("/settings?tab=tokens");
      const row = page.locator("tr", { hasText: "e2e-token" }).first();
      await expect(row).toBeVisible();
      // Created-by column resolves the issuer's email (D-0109).
      await expect(row.getByText(/@/).first()).toBeVisible();
    } finally {
      await apiSend(page, "delete", `/api/v1/tokens/${tok.id}`);
    }
  });

  test("webhooks: pause/resume switch (D-0113)", async ({ page }) => {
    const { orgID } = await firstProject(page);
    const r = await apiSend(page, "post", `/api/v1/organizations/${orgID}/webhooks`, { url: "https://ops.example.com/e2e-hook" });
    const hook = await r.json();
    try {
      await page.goto("/settings?tab=webhooks");
      const row = page.locator("tr", { hasText: "e2e-hook" });
      const toggle = row.locator("button.rounded-full").first();
      await toggle.click();
      await page.waitForTimeout(600);
      const after = await apiGet(page, `/api/v1/organizations/${orgID}/webhooks`);
      expect(after.find((h: any) => h.id === hook.id).enabled).toBeFalsy();
      await toggle.click();
      await page.waitForTimeout(600);
      const restored = await apiGet(page, `/api/v1/organizations/${orgID}/webhooks`);
      expect(restored.find((h: any) => h.id === hook.id).enabled).toBeTruthy();
    } finally {
      await apiSend(page, "delete", `/api/v1/webhooks/${hook.id}`);
    }
  });

  test("agent tokens: issue (secret shown once) and revoke", async ({ page }) => {
    // Revoked rows stay listed as history — a unique name isolates this run.
    const name = `e2e-agent-${Date.now()}`;
    await page.goto("/settings?tab=agenttokens");
    await page.fill('input[placeholder="geo2-dc"]', name);
    await page.fill('input[placeholder="geo2"]', "e2e-region");
    await page.locator("button", { hasText: "Issue token" }).click();
    await expect(page.locator("text=shown once")).toBeVisible();
    await page.locator("button", { hasText: "Dismiss" }).click();
    const row = page.locator("tr", { hasText: name });
    await row.locator("button", { hasText: "Revoke" }).click();
    await expect(row.getByText(/revoked/)).toBeVisible();
  });
});

  // The instance audit trail (iter-0155): a global admin's own actions were recorded for months and
  // shown nowhere. Two things are worth a browser: that the panel exists where the mock put it and
  // renders real rows from the server, and that the endpoint behind it is a DISTINCT read — the split
  // the whole design rests on, and the one an authz slip would erase.
  test("the instance audit panel shows the installation's own history", async ({ page }) => {
    // Make sure there IS an instance-level entry to render: toggling global admin on a user writes
    // `user.global_admin` with org_id NULL, which is exactly what this panel is for.
    const users = await apiGet(page, "/api/v1/admin/users");
    const other = (users as { id: string; email: string; is_global_admin?: boolean }[]).find(
      (u) => !u.email.startsWith("admin@"),
    );
    if (other) {
      await apiSend(page, "patch", `/api/v1/admin/users/${other.id}`, { is_global_admin: false });
    }

    const entries = (await apiGet(page, "/api/v1/admin/audit?limit=30")) as { org_id?: string; action?: string }[];
    // The PATCH above wrote `user.global_admin` with no organization, so this assertion is not
    // conditional: there IS an instance entry, and an "or the empty state" escape hatch here would
    // make the test pass on a panel that renders nothing.
    expect(entries.length).toBeGreaterThan(0);
    // Instance entries carry no organization — an org-scoped row reaching this listing is the leak the
    // whole split exists to prevent.
    for (const e of entries) {
      expect(e.org_id ?? "").toBe("");
    }

    await page.goto("/settings?tab=instanceaudit");
    await expect(page.getByTestId("instance-chip")).toHaveText("instance");
    await expect(page.getByTestId("audit-row").first()).toBeVisible();
    await expect(page.getByTestId("audit-empty")).toHaveCount(0);
    // The row reads as prose about an actor and an action, not as a raw action key.
    await expect(page.getByTestId("audit-row").first()).toContainText(/granted or revoked global admin|deleted a user|replayed/);

    // The tab lives in the Administration group, where "instance, not tenant" already means something.
    await expect(page.locator("text=Administration").first()).toBeVisible();
  });

  // Editing a notification channel through the UI. The property worth a live run is the
  // one no unit test can reach: the server never sends a secret out, so the form leaves
  // it blank, and the channel must still hold the bot token it was created with. The
  // proof is the PATCH at the end — the server merges a config patch onto the STORED
  // config and validates the result, so a telegram channel whose bot_token had been
  // blanked by the edit would answer 400 there instead of 200.
  test("a notification channel is edited in place, and its secret survives", async ({ page }) => {
    const { projectID } = await firstProject(page);
    const name = "e2e-channel-edit";
    const created = await apiSend(page, "post", `/api/v1/projects/${projectID}/notification-channels`, {
      type: "telegram",
      name,
      config: { bot_token: "e2e-secret-token", chat_id: "1000" },
    });
    expect(created.ok(), `create channel -> ${created.status()}`).toBeTruthy();
    const channelID = (await created.json()).id as string;

    try {
      await page.goto("/settings?tab=channels");
      const row = page.locator("tr", { hasText: name });
      await expect(row).toBeVisible();
      await row.getByRole("button", { name: "Edit" }).click();

      const form = page.getByTestId("channel-form");
      await expect(form).toBeVisible();
      // The type is frozen and the secret is blank: the page cannot show what it was never given.
      await expect(page.getByTestId("channel-type")).toBeDisabled();
      await expect(page.getByTestId("channel-field-bot_token")).toHaveValue("");
      await expect(page.getByTestId("channel-field-chat_id")).toHaveValue("1000");

      await page.getByTestId("channel-name").fill(`${name}-renamed`);
      await page.getByTestId("channel-field-chat_id").fill("2000");
      await form.getByRole("button", { name: "Save" }).click();

      // The list shows the edit without a reload, and it survives one.
      await expect(page.locator("tr", { hasText: `${name}-renamed` })).toBeVisible();
      await page.reload();
      await expect(page.locator("tr", { hasText: `${name}-renamed` })).toBeVisible();

      const listed = (await apiGet(page, `/api/v1/projects/${projectID}/notification-channels`)) as {
        id: string;
        name: string;
        config: Record<string, string>;
      }[];
      const got = listed.find((c) => c.id === channelID)!;
      expect(got.name).toBe(`${name}-renamed`);
      expect(got.config.chat_id).toBe("2000");
      expect(got.config.bot_token, "a listed channel never carries its secret").toBe("");

      // The blank the form sent did NOT blank the stored token: this patch validates the
      // merged config, and telegram without a bot_token is a 400.
      const merged = await apiSend(page, "patch", `/api/v1/notification-channels/${channelID}`, {
        config: { chat_id: "3000" },
      });
      expect(merged.status(), "the stored bot_token still satisfies telegram's contract").toBe(200);
    } finally {
      await apiSend(page, "delete", `/api/v1/notification-channels/${channelID}`);
    }
  });
