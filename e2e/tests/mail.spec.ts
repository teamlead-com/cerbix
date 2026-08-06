import { test, expect } from "@playwright/test";
import { ADMIN, apiGet, apiSend, firstProject } from "./helpers";

// Mail flows through the Mailpit sidecar (compose profile `mail`). Skips
// cleanly when Mailpit is unreachable. The SMTP settings are switched to the
// sidecar for the duration and restored afterwards.
const MAILPIT = process.env.MAILPIT_URL || "http://localhost:8025";

// The settings PUT is strict — send only real fields, never the GET view's
// derived ones (deliverable/smtp_password_set/configured).
function mailBody(v: any) {
  return {
    enabled: v.enabled ?? false,
    smtp_host: v.smtp_host ?? "",
    smtp_port: v.smtp_port ?? 0,
    smtp_username: v.smtp_username ?? "",
    smtp_password: "", // blank preserves the stored secret
    from: v.from ?? "",
    public_base_url: v.public_base_url ?? "",
  };
}

async function mailpitUp(page: any): Promise<boolean> {
  try {
    const r = await page.request.get(`${MAILPIT}/api/v1/messages`);
    return r.ok();
  } catch {
    return false;
  }
}
async function lastMailTo(page: any, rcpt: string, timeoutMs = 15_000): Promise<string> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const list = await (await page.request.get(`${MAILPIT}/api/v1/messages`)).json();
    const msg = (list.messages ?? []).find((m: any) => m.To?.some((t: any) => t.Address === rcpt));
    if (msg) {
      const full = await (await page.request.get(`${MAILPIT}/api/v1/message/${msg.ID}`)).json();
      return `${full.Text ?? ""}\n${full.HTML ?? ""}`;
    }
    await page.waitForTimeout(1000);
  }
  throw new Error(`no mail for ${rcpt} within ${timeoutMs}ms`);
}
const firstUrl = (body: string) => body.match(/https?:\/\/[^\s"'<>]+/)?.[0];

test.describe("mail flows (mailpit profile)", () => {
  test.beforeEach(async ({ page }) => {
    test.skip(!(await mailpitUp(page)), "mailpit is not running (--profile mail)");
  });

  test("password reset: request → letter → confirm → sign in → restore", async ({ page }) => {
    const before = await apiGet(page, "/api/v1/settings/mail");
    const set = {
      ...mailBody(before), enabled: true, smtp_host: "mailpit", smtp_port: 1025,
      smtp_username: "", // no auth: Go's smtp refuses PlainAuth over plaintext
      from: "cerbix@e2e.local", public_base_url: process.env.CERBIX_URL || "http://localhost:8080",
    };
    const put = await apiSend(page, "put", "/api/v1/settings/mail", set);
    expect(put.ok(), await put.text()).toBeTruthy();
    try {
      await page.request.delete(`${MAILPIT}/api/v1/messages`);
      const req = await page.request.post("/auth/local/reset/request", { data: { email: ADMIN.email } });
      expect(req.ok()).toBeTruthy();

      const body = await lastMailTo(page, ADMIN.email);
      const link = firstUrl(body);
      expect(link, "reset link in the letter").toBeTruthy();
      const token = new URL(link!).searchParams.get("token") ?? link!.split("/").pop();
      expect(token).toBeTruthy();

      const tempPass = "e2e-temporary-pass-123";
      const confirm = await page.request.post("/auth/local/reset/confirm", { data: { token, new_password: tempPass } });
      expect(confirm.status(), await confirm.text()).toBe(204);

      // The new password signs in (fresh context — never touch the shared session).
      const anon = await page.context().browser()!.newContext({ storageState: { cookies: [], origins: [] } });
      const login = await anon.request.post("/auth/local/login", { data: { username: ADMIN.email, password: tempPass } });
      expect(login.ok(), await login.text()).toBeTruthy();
      await anon.close();

      // Restore the well-known dev password through the still-valid admin session.
      const restore = await apiSend(page, "post", "/api/v1/me/password", {
        current_password: tempPass, new_password: ADMIN.password,
      });
      expect(restore.ok(), await restore.text()).toBeTruthy();
    } finally {
      await apiSend(page, "put", "/api/v1/settings/mail", mailBody(before));
    }
  });

  test("subscriber confirm: subscribe → letter → link → confirmed", async ({ page }) => {
    const before = await apiGet(page, "/api/v1/settings/mail");
    const set = {
      ...mailBody(before), enabled: true, smtp_host: "mailpit", smtp_port: 1025,
      smtp_username: "", // no auth: Go's smtp refuses PlainAuth over plaintext
      from: "cerbix@e2e.local", public_base_url: process.env.CERBIX_URL || "http://localhost:8080",
    };
    const put = await apiSend(page, "put", "/api/v1/settings/mail", set);
    expect(put.ok(), await put.text()).toBeTruthy();
    const { orgID } = await firstProject(page);
    const created = await apiSend(page, "post", `/api/v1/organizations/${orgID}/status-pages`, {
      slug: "e2e-mail-status", title: "E2E Mail", visibility: "public",
    });
    const sp = await created.json();
    try {
      await page.request.delete(`${MAILPIT}/api/v1/messages`);
      const rcpt = "e2e-confirm@example.com";
      const sub = await page.request.post(`/api/v1/public/status-pages/e2e-mail-status/subscribers`, { data: { email: rcpt } });
      expect(sub.ok(), await sub.text()).toBeTruthy();

      const body = await lastMailTo(page, rcpt, 45_000); // outbox delivery + backoff headroom
      const link = firstUrl(body);
      expect(link, "confirm link in the letter").toBeTruthy();
      // The link lands on the public status page (?confirm=<token>) — the SPA
      // performs the confirmation, so it must run in a real browser page.
      const anon = await page.context().browser()!.newContext({ storageState: { cookies: [], origins: [] } });
      const anonPage = await anon.newPage();
      await anonPage.goto(link!);
      await anonPage.waitForTimeout(1500);
      await anon.close();

      const subs = await apiGet(page, `/api/v1/status-pages/${sp.id}/subscribers`);
      const mine = subs.find((s: any) => s.email === rcpt);
      expect(mine?.confirmed_at, "subscriber confirmed after the link visit").toBeTruthy();
    } finally {
      await apiSend(page, "delete", `/api/v1/status-pages/${sp.id}`);
      await apiSend(page, "put", "/api/v1/settings/mail", mailBody(before));
    }
  });
});
