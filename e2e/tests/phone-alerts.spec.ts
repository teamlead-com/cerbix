import { test, expect } from "./fixtures";
import type { APIRequestContext, APIResponse, Page } from "@playwright/test";
import { E2E_WORKSPACE } from "./helpers";

// FR-041a (phone alerts, part a) against the live dev stack, through the SPA.
//
// NO REAL PROVIDER IS EVER CALLED. The stack's telephony is pointed at the provider stub of
// docker/phone-stub (service `phonestub`, started by `make dev-up`): a generic HTTP SMS gateway that
// records every message and answers 201. beforeAll REFUSES to run when the stack's telephony is on
// with anything else, or names any other gateway URL, so a developer's real provider is never
// overwritten or called. afterAll puts telephony back OFF.
//
// The suite's one sign-in is reused: the tests use the stored admin session, the hooks an API
// context built from the same stored state. Test 6 signs in through Keycloak in a context of its own
// (as oidc.spec.ts does) and never touches the shared session; nothing here calls /auth/logout.
//
// Codes: a run asks for two (the admin's number, a fixed-number channel); the hourly limit is 3 per
// requester, so `make dev-ready-single` resets the admin's code rows (e2e/seed/e2e-phone-reset.sql)
// and every run uses fresh random numbers for the per-number daily limit.

const BASE = process.env.CERBIX_URL || "http://localhost:8080";
const STUB = process.env.PHONESTUB_URL || "http://localhost:8026";
const STUB_IN_STACK = "http://phonestub:8026/sms";
const ADMIN_SESSION = ".auth/admin.json";

const stamp = Date.now();
const randomNumber = () => `+1555${String(Math.floor(Math.random() * 1e7)).padStart(7, "0")}`;
const ADMIN_NUMBER = randomNumber();
let FIXED_NUMBER = randomNumber();
while (FIXED_NUMBER === ADMIN_NUMBER) FIXED_NUMBER = randomNumber();
const RUN_NUMBERS = [ADMIN_NUMBER, FIXED_NUMBER];

// The header value is a run-unique marker, so "it is not in the page" can actually fail.
const HEADER_SECRET = `e2e-stub-secret-${stamp}`;
const BODY_TEMPLATE = '{"to": "{{to}}", "from": "{{from}}", "text": "{{text}}"}';

const SMS_NAME = `e2e-phone-sms-${stamp}`;
const VOICE_NAME = `e2e-phone-voice-${stamp}`;
const FIXED_NAME = `e2e-phone-fixed-${stamp}`;
const ROUTE_NAME = `e2e-phone-route-${stamp}`;
const MON_NAME = `e2e-phone-down-${stamp}`;
const NOPANEL_TITLE = `e2e-phone-nopanel-${stamp}`;
const PROJECT_SLUG = `e2e-phone-${stamp}`;
const SERVICE_SLUG = `e2e-phone-svc-${stamp}`;

const CODE_TEXT = /^\[cerbix\] Your verification code is (\d{6})\. It expires in 10 minutes\.$/;
const TEST_TEXT = "[cerbix] Test message: phone alerts are working.";
const VOICE_REFUSAL = "the telephony provider in force sends SMS only; a voice channel cannot be created or enabled";

type StubMessage = { seq: number; method: string; path: string; content_type: string; header_names: string[]; to: string | null; text: string | null; body: string };

// ── state shared by the serial tests ────────────────────────────────────────────────────────────
let api: APIRequestContext;
let original: any = null; // the stack's telephony view before the run
let guardPassed = false; // the stack's telephony was the stub's or unconfigured: this run may write it
let adminID = "";
let orgID = "";
let harnessID = "";
let phoneProjectID = "";
let adminMask = "";
let smsChannelID = "";
let monitorID = "";
let downIncident: { id: string; ref: string } | null = null;
let nopanelIncidentID = "";
let serviceID = "";

async function readJSON(r: APIResponse) {
  const text = await r.text();
  return text ? JSON.parse(text) : null;
}
async function aget(path: string): Promise<any> {
  const r = await api.get(path);
  expect(r.ok(), `GET ${path} -> ${r.status()} ${await r.text()}`).toBeTruthy();
  return (await readJSON(r)) ?? [];
}
function asend(method: "POST" | "PUT" | "PATCH" | "DELETE", path: string, data?: unknown) {
  return api.fetch(path, { method, data });
}

async function stubMessages(to?: string): Promise<StubMessage[]> {
  const r = await api.get(`${STUB}/_stub/messages${to ? `?to=${encodeURIComponent(to)}` : ""}`);
  expect(r.ok(), "the phone stub answers").toBeTruthy();
  return (await r.json()) as StubMessage[];
}

// The newest verification code sent to `to` after stub sequence number `after`.
async function codeFor(to: string, after = 0): Promise<{ code: string; seq: number }> {
  let found = null as { code: string; seq: number } | null;
  await expect
    .poll(async () => {
      const hit = (await stubMessages(to)).filter((m) => m.seq > after && CODE_TEXT.test(m.text ?? "")).pop();
      found = hit ? { code: CODE_TEXT.exec(hit.text!)![1], seq: hit.seq } : null;
      return found !== null;
    }, { message: `a verification code SMS reaches the stub for the run's number`, timeout: 20_000 })
    .toBe(true);
  return found!;
}

function wrongCode(code: string) {
  return code.slice(0, 5) + String((Number(code[5]) + 1) % 10);
}

function channelRow(page: Page, name: string) {
  return page.locator(`[data-testid="channel-row"][data-name="${name}"]`);
}

async function openChannels(page: Page, projectID: string) {
  // The workspace is remembered per browser; point it at the project whose channels are wanted.
  await page.goto("/settings?tab=channels");
  await page.evaluate(
    ([o, p]) => {
      localStorage.setItem("cerbix.org", o);
      localStorage.setItem("cerbix.project", p);
    },
    [orgID, projectID] as [string, string],
  );
  await page.goto("/settings?tab=channels");
  await expect(page.getByRole("button", { name: "Add channel" })).toBeVisible();
}

async function startChannel(page: Page, type: "sms" | "voice", name: string) {
  await page.getByRole("button", { name: "Add channel" }).click();
  await expect(page.getByTestId("channel-form")).toBeVisible();
  await page.getByTestId("channel-type").selectOption(type);
  await page.getByTestId("channel-name").fill(name);
  await expect(page.getByTestId("phone-channel-form")).toBeVisible();
}

async function pickAdmin(page: Page) {
  const select = page.getByTestId("phone-recipient-select");
  await expect(select.locator(`option[value="${adminID}"]`)).toHaveCount(1);
  await select.selectOption(adminID);
}

const isPost = (path: string) => (r: { url(): string; request(): { method(): string } }) =>
  r.request().method() === "POST" && new URL(r.url()).pathname === path;

async function deleteLeftovers(projectID: string) {
  for (const c of await aget(`/api/v1/projects/${projectID}/notification-channels`)) {
    if (String(c.name).startsWith("e2e-phone-")) await asend("DELETE", `/api/v1/notification-channels/${c.id}`);
  }
  for (const m of await aget(`/api/v1/projects/${projectID}/monitors`)) {
    if (String(m.name).startsWith("e2e-phone-")) await asend("DELETE", `/api/v1/monitors/${m.id}`);
  }
  for (const i of await aget(`/api/v1/projects/${projectID}/incidents`)) {
    if (String(i.title ?? "").includes("e2e-phone-") && i.status !== "resolved") {
      await asend("POST", `/api/v1/incidents/${i.id}/updates`, { status: "resolved", body: "e2e cleanup" });
    }
  }
}

test.describe.serial("phone alerts (FR-041a) — through the provider stub", () => {
  test.beforeAll(async ({ playwright }) => {
    api = await playwright.request.newContext({ baseURL: BASE, storageState: ADMIN_SESSION });

    const health = await api.get(`${STUB}/_stub/health`).catch(() => null);
    if (!health || health.status() !== 200) {
      throw new Error("phone stub not running — `make dev-up` starts it (service phonestub, :8026)");
    }

    // Never overwrite or call a developer's real provider.
    original = await aget("/api/v1/settings/telephony");
    const url = String(original?.generic_http?.url ?? "");
    const atStub = url.startsWith("http://phonestub:8026/");
    if (original?.enabled && !(original.provider === "generic_http" && atStub)) {
      throw new Error(`refusing to run: telephony on this stack is ON with provider "${original.provider}" — not the phone stub`);
    }
    if (url !== "" && !atStub) {
      throw new Error(`refusing to run: telephony on this stack names another gateway (${url}); only ${STUB_IN_STACK} is allowed`);
    }

    guardPassed = true;

    const me = await aget("/api/v1/me");
    adminID = me.user.id;
    expect(adminID, "the signed-in admin").toBeTruthy();

    const orgs = await aget("/api/v1/organizations");
    const org = orgs.find((o: any) => o.slug === E2E_WORKSPACE.orgSlug);
    expect(org, "the E2E organization from global setup").toBeTruthy();
    orgID = org.id;
    const projects = await aget(`/api/v1/organizations/${orgID}/projects`);
    harnessID = projects.find((p: any) => p.slug === E2E_WORKSPACE.projectSlug)?.id;
    expect(harnessID, "the E2E project from global setup").toBeTruthy();

    // A clean slate: the stub, the admin's phone, leftovers of an aborted run.
    expect((await api.delete(`${STUB}/_stub/messages`)).ok()).toBeTruthy();
    const del = await asend("DELETE", "/api/v1/me/phone");
    expect([204, 404], `DELETE /me/phone -> ${del.status()}`).toContain(del.status());
    await deleteLeftovers(harnessID);
    for (const p of projects) {
      if (String(p.slug).startsWith("e2e-phone-")) {
        await deleteLeftovers(p.id);
        await asend("DELETE", `/api/v1/projects/${p.id}`);
      }
    }

    const created = await asend("POST", `/api/v1/organizations/${orgID}/projects`, { slug: PROJECT_SLUG, name: PROJECT_SLUG });
    expect(created.status(), await created.text()).toBe(201);
    phoneProjectID = (await created.json()).id;
  });

  test.afterAll(async () => {
    if (!api) return;
    if (!guardPassed) {
      // Refused (or never reached the guard): this run wrote nothing, so it restores nothing.
      await api.dispose();
      return;
    }
    const problems: string[] = [];
    const tryStep = async (what: string, fn: () => Promise<unknown>) => {
      try {
        await fn();
      } catch (e) {
        problems.push(`${what}: ${e}`);
      }
    };
    // 1. Telephony OFF first, so nothing more can be sent; secrets omitted = kept, and the e2e header
    //    is removed when the stack had none. "Absent" cannot be restored through the API: a stack that
    //    was never configured stays configured:true, enabled:false.
    await tryStep("restore telephony", async () => {
      const o = original ?? {};
      const g = o.generic_http ?? {};
      const body: any = {
        enabled: false,
        provider: o.provider ?? "",
        twilio: { account_sid: o.twilio?.account_sid ?? "", from: o.twilio?.from ?? "" },
        generic_http: {
          method: g.method ?? "", url: g.url ?? "", content_type: g.content_type ?? "",
          body_template: g.body_template ?? "", from: g.from ?? "", response_code_field: g.response_code_field ?? "",
          ...(g.headers_set ? {} : { headers_clear: true }),
        },
        public_base_url: o.public_base_url ?? "",
        voice_language: o.voice_language ?? "",
        sms_encoding: o.sms_encoding ?? "",
        sms_max_segments: o.sms_max_segments ?? 0,
        storm: o.storm ?? { per_incident_per_hour: 0, per_number_per_hour: 0 },
        confirm_unroutable: true,
      };
      const r = await asend("PUT", "/api/v1/settings/telephony", body);
      if (!r.ok()) throw new Error(`PUT telephony -> ${r.status()} ${await r.text()}`);
    });
    // 2. The run's entities.
    for (const id of [downIncident?.id, nopanelIncidentID]) {
      if (id) await tryStep(`resolve ${id}`, () => asend("POST", `/api/v1/incidents/${id}/updates`, { status: "resolved", body: "e2e cleanup" }));
    }
    if (monitorID) await tryStep("delete monitor", () => asend("DELETE", `/api/v1/monitors/${monitorID}`));
    if (harnessID) await tryStep("harness leftovers", () => deleteLeftovers(harnessID));
    if (phoneProjectID) {
      await tryStep("phone project leftovers", () => deleteLeftovers(phoneProjectID));
      if (serviceID) await tryStep("delete service", () => asend("DELETE", `/api/v1/projects/${phoneProjectID}/services/${serviceID}`));
      await tryStep("delete phone project", () => asend("DELETE", `/api/v1/projects/${phoneProjectID}`));
    }
    await tryStep("delete my phone", () => asend("DELETE", "/api/v1/me/phone"));
    // 3. The stub held only what this run sent, to this run's numbers — then it is cleared.
    await tryStep("stub recipients", async () => {
      const all = await stubMessages();
      const strangers = all.filter((m) => !RUN_NUMBERS.includes(String(m.to)));
      console.log(`[phone-alerts] the stub recorded ${all.length} message(s), ${strangers.length} outside the run's two numbers`);
      if (strangers.length) throw new Error(`the stub received ${strangers.length} message(s) to numbers outside the run`);
    });
    await tryStep("clear stub", () => api.delete(`${STUB}/_stub/messages`));
    await api.dispose();
    expect(problems, "afterAll cleanup").toEqual([]);
  });

  test("1 — telephony settings: a generic HTTP gateway saved through the form (1b, 1c)", async ({ page }) => {
    await page.goto("/settings?tab=telephony");
    const panel = page.getByTestId("telephony-panel");
    await expect(panel).toBeVisible();

    await page.getByTestId("telephony-enabled").check();
    await page.getByTestId("telephony-provider-generic").check();
    await expect(page.getByTestId("telephony-gateway-note")).toContainText("SMS only.");
    await page.getByTestId("generic-url").fill(STUB_IN_STACK);
    await page.getByTestId("generic-method").selectOption("POST");
    await page.getByTestId("generic-content-type").selectOption("application/json");
    await page.getByTestId("generic-headers").fill(`X-E2E-Stub: ${HEADER_SECRET}`);
    await page.getByTestId("generic-body-template").fill(BODY_TEMPLATE);
    await page.getByTestId("generic-from").fill("CERBIX");
    await page.getByTestId("generic-response-code-field").fill("error_code");
    // Messages block
    await page.getByTestId("telephony-public-base-url").fill("http://localhost:8080");
    await page.getByTestId("telephony-voice-language").fill("en-US");
    await page.getByTestId("telephony-encoding-transliterate").check();
    await page.getByTestId("telephony-max-segments").selectOption("2");
    await page.getByTestId("telephony-storm-incident").fill("5");
    await page.getByTestId("telephony-storm-number").fill("20");

    const put = page.waitForResponse((r) => r.request().method() === "PUT" && r.url().endsWith("/api/v1/settings/telephony"));
    await page.getByTestId("telephony-save").click();
    expect((await put).status(), "the settings PUT").toBe(200);
    await expect(page.getByTestId("telephony-saved")).toHaveText("✓ Saved");
    await expect(page.getByTestId("telephony-pill")).toHaveText("On · Generic HTTP");

    // Re-open: the headers are stored, and never shown back.
    await page.reload();
    await expect(page.getByTestId("telephony-pill")).toHaveText("On · Generic HTTP");
    await expect(page.getByTestId("generic-headers-stored")).toHaveText("✓ stored");
    await expect(page.getByTestId("generic-url")).toHaveValue(STUB_IN_STACK);
    expect(await page.content(), "the header value is never in the page").not.toContain(HEADER_SECRET);

    const view = await aget("/api/v1/settings/telephony");
    expect(view.enabled).toBe(true);
    expect(view.provider).toBe("generic_http");
    expect(view.generic_http.url).toBe(STUB_IN_STACK);
    expect(view.generic_http.headers_set).toBe(true);
    expect(JSON.stringify(view), "the view carries no secret").not.toContain(HEADER_SECRET);
  });

  test("2 — Account → Phone: send, a wrong code, the right code (2a → 2b → 2c → 2g)", async ({ page }) => {
    await page.goto("/settings?tab=phone");
    await expect(page.getByTestId("phone-panel")).toBeVisible();
    await expect(page.getByTestId("phone-explanation")).toBeVisible();
    // Revision 9: generic_http cannot place calls, so /me says telephony_can_call false.
    expect((await aget("/api/v1/me")).telephony_can_call).toBe(false);
    await expect(page.getByTestId("phone-send")).toBeVisible();
    await expect(page.getByTestId("phone-send-call")).toHaveCount(0);

    await page.getByTestId("phone-number-input").fill(ADMIN_NUMBER);
    const put = page.waitForResponse((r) => r.request().method() === "PUT" && r.url().endsWith("/api/v1/me/phone"));
    await page.getByTestId("phone-send").click();
    expect((await put).status(), "PUT /me/phone").toBe(200);

    // 2b
    await expect(page.getByTestId("phone-pill")).toHaveText("Not verified");
    await expect(page.getByTestId("phone-step-1")).toContainText("✓ 1 · Code sent by SMS");
    await expect(page.getByTestId("phone-resend")).toBeVisible();
    await expect(page.getByTestId("phone-resend-call")).toHaveCount(0);

    const { code } = await codeFor(ADMIN_NUMBER);
    const sms = (await stubMessages(ADMIN_NUMBER)).filter((m) => CODE_TEXT.test(m.text ?? ""));
    expect(sms, "exactly one code SMS to the number").toHaveLength(1);
    expect(sms[0].path).toBe("/sms");
    expect(sms[0].header_names, "the stored header reaches the gateway").toContain("x-e2e-stub");
    expect(JSON.parse(sms[0].body).from).toBe("CERBIX");

    // 2c
    await page.getByTestId("phone-code-input").fill(wrongCode(code));
    await page.getByTestId("phone-verify").click();
    await expect(page.getByTestId("phone-code-error")).toHaveText("That code is not right. Check the most recent SMS and try again.");

    // 2g
    await page.getByTestId("phone-code-input").fill(code);
    await page.getByTestId("phone-verify").click();
    await expect(page.getByTestId("phone-pill")).toHaveText("Verified");
    await expect(page.getByTestId("phone-masked")).toHaveText(new RegExp(`${ADMIN_NUMBER.slice(-4)}$`));
    const html = await page.content();
    expect(html, "the full number is nowhere in the page").not.toContain(ADMIN_NUMBER);
    expect(html).not.toContain(ADMIN_NUMBER.slice(1));

    const me = await aget("/api/v1/me");
    expect(me.phone.verified).toBe(true);
    adminMask = me.phone.number_masked;
    expect(adminMask.endsWith(ADMIN_NUMBER.slice(-4))).toBe(true);
  });

  test("3 — Send test SMS reaches the stub (1i)", async ({ page }) => {
    expect(adminMask, "test 2 verified the admin's number").toBeTruthy();
    await page.goto("/settings?tab=telephony");
    const send = page.getByTestId("telephony-test-send");
    await expect(send).toHaveText(`Send test SMS to ${adminMask}`);
    await expect(send).toBeEnabled();
    const post = page.waitForResponse(isPost("/api/v1/settings/telephony/test-sms"));
    await send.click();
    expect((await post).status()).toBe(200);
    await expect(page.getByTestId("telephony-test-result")).toContainText("✓ Accepted by the provider · HTTP 201.");
    await expect
      .poll(async () => (await stubMessages(ADMIN_NUMBER)).filter((m) => m.text === TEST_TEXT).length, { timeout: 10_000 })
      .toBe(1);
  });

  test("4 — channels: a person, a refused voice call, a fixed number verified and enabled (3a, 3f′, 3b, 3e)", async ({ page }) => {
    await openChannels(page, harnessID);

    // 3a: SMS to a person (the admin, whose number is verified) — a route, so no chip.
    await startChannel(page, "sms", SMS_NAME);
    await expect(page.getByTestId("phone-send-to-user")).toBeChecked();
    await pickAdmin(page);
    let created = page.waitForResponse(isPost(`/api/v1/projects/${harnessID}/notification-channels`));
    await page.getByTestId("phone-channel-submit").click();
    expect((await created).status(), "create the person channel").toBe(201);
    await expect(page.getByTestId("channel-form")).toBeHidden();
    const smsRow = channelRow(page, SMS_NAME);
    await expect(smsRow).toBeVisible();
    await expect(smsRow.getByTestId("channel-recipient")).toContainText("you");
    await expect(smsRow.getByTestId("channel-route-chip")).toHaveCount(0);

    // 3f′: a voice channel under the generic gateway is refused with the server's sentence.
    await startChannel(page, "voice", VOICE_NAME);
    await pickAdmin(page);
    created = page.waitForResponse(isPost(`/api/v1/projects/${harnessID}/notification-channels`));
    await page.getByTestId("phone-channel-submit").click();
    expect((await created).status(), "voice under generic_http").toBe(400);
    await expect(page.getByTestId("phone-channel-error")).toContainText(VOICE_REFUSAL);
    await page.getByTestId("phone-channel-cancel").click();
    await expect(channelRow(page, VOICE_NAME)).toHaveCount(0);

    // 3b-1 → 3b-2 → 3b-3: a fixed number.
    await startChannel(page, "sms", FIXED_NAME);
    await page.locator("label", { has: page.getByTestId("phone-send-to-number") }).click();
    await expect(page.getByTestId("phone-number-explain")).toBeVisible();
    await page.getByTestId("phone-number-input").fill(FIXED_NUMBER);
    created = page.waitForResponse(isPost(`/api/v1/projects/${harnessID}/notification-channels`));
    await page.getByTestId("phone-channel-submit").click();
    const fixed = await created;
    expect(fixed.status(), "create the number channel").toBe(201);
    const fixedID = (await fixed.json()).id as string;

    await expect(page.getByTestId("phone-verify-created")).toHaveText(`Created ${FIXED_NAME} — disabled until the number is confirmed.`);
    await expect(page.getByTestId("phone-verify-send")).toHaveText("Send code");
    await expect(page.getByTestId("phone-verify-call"), "revision 9: no call under generic_http").toHaveCount(0);
    const sent = page.waitForResponse(isPost(`/api/v1/notification-channels/${fixedID}/verify/send`));
    await page.getByTestId("phone-verify-send").click();
    expect((await sent).status(), "verify/send").toBe(204);
    await expect(page.getByTestId("phone-verify-call")).toHaveCount(0);
    const { code } = await codeFor(FIXED_NUMBER);
    await page.getByTestId("phone-verify-code").fill(code);
    await page.getByTestId("phone-verify-submit").click();
    await expect(page.getByTestId("phone-verified-step-2")).toHaveText("✓ 2 · Number confirmed");
    const patched = page.waitForResponse((r) => r.request().method() === "PATCH" && r.url().endsWith(`/api/v1/notification-channels/${fixedID}`));
    await page.getByTestId("phone-enable").click();
    expect((await patched).status(), "enable the number channel").toBe(200);
    await expect(page.getByTestId("channel-form")).toBeHidden();

    const fixedRow = channelRow(page, FIXED_NAME);
    await expect(fixedRow.getByTestId("channel-route-chip")).toHaveCount(0);
    await expect(fixedRow.getByTestId("channel-verify")).toHaveCount(0);
    await expect(fixedRow.getByTestId("channel-toggle-locked")).toHaveCount(0);
    await expect(fixedRow.getByRole("button", { name: "Disable channel" })).toBeVisible();
    await expect(fixedRow.getByTestId("channel-recipient")).toContainText(FIXED_NUMBER.slice(-4));
    expect(await page.content(), "the fixed number is never shown in full").not.toContain(FIXED_NUMBER.slice(1));

    const listed = await aget(`/api/v1/projects/${harnessID}/notification-channels`);
    const f = listed.find((c: any) => c.id === fixedID);
    expect(f.enabled).toBe(true);
    expect(f.number_verified).toBe(true);
    expect(listed.some((c: any) => c.name === VOICE_NAME), "the refused voice channel was not created").toBe(false);
    smsChannelID = listed.find((c: any) => c.name === SMS_NAME).id;
  });

  test("5 — DOWN → an SMS → the incident's Phone panel → the channel's deliveries → /i/:ref (4a, 3g, 5a, 5d)", async ({ page }) => {
    test.setTimeout(4 * 60_000);
    expect(smsChannelID, "test 4 created the person channel").toBeTruthy();

    // Created paused, linked, then resumed — so the first (failing) check cannot run before the link.
    const created = await asend("POST", `/api/v1/projects/${harnessID}/monitors`, {
      name: MON_NAME, type: "tcp", target: "127.0.0.1:1", region: "core",
      interval_seconds: 30, timeout_seconds: 5, retries: 0, failure_threshold: 1,
      confirm_interval_seconds: 0, auto_incident: true, renotify_seconds: 0, enabled: false,
    });
    expect(created.status(), await created.text()).toBe(201);
    monitorID = (await created.json()).id;
    const link = await asend("POST", `/api/v1/monitors/${monitorID}/notifications`, { channel_id: smsChannelID });
    expect(link.status(), await link.text()).toBe(204);
    const resumed = await asend("PATCH", `/api/v1/monitors/${monitorID}`, { enabled: true });
    expect(resumed.ok(), await resumed.text()).toBeTruthy();

    await expect.poll(async () => (await aget(`/api/v1/monitors/${monitorID}`)).status, { message: "the monitor goes down", timeout: 120_000, intervals: [2_000] }).toBe("down");
    await expect
      .poll(async () => {
        const inc = (await aget(`/api/v1/projects/${harnessID}/incidents`)).find((i: any) => i.monitor_id === monitorID && i.status !== "resolved");
        if (inc?.ref) downIncident = { id: inc.id, ref: inc.ref };
        return !!downIncident;
      }, { message: "the auto-incident opens with a ref", timeout: 60_000, intervals: [2_000] })
      .toBe(true);
    const { id: incidentID, ref } = downIncident!;
    expect(ref).toMatch(/^[0-9A-HJKMNP-TV-Z]{8}$/);

    // The SMS: with the incident's reference and link, or (D32) sent before the incident opened.
    const head = `[cerbix] DOWN ${MON_NAME} (${E2E_WORKSPACE.projectName})`;
    let down = undefined as StubMessage | undefined;
    await expect
      .poll(async () => {
        down = (await stubMessages(ADMIN_NUMBER)).find((m) => (m.text ?? "").startsWith(`[cerbix] DOWN ${MON_NAME}`));
        return !!down;
      }, { message: "the DOWN SMS reaches the stub", timeout: 60_000, intervals: [2_000] })
      .toBe(true);
    expect([head, `${head} - incident ${ref} ${BASE}/i/${ref}`]).toContain(down!.text);
    console.log(`[phone-alerts] DOWN SMS ${down!.text === head ? "sent before the incident opened (no_incident_ref form)" : "carries the incident ref and link"}`);

    // The delivery row is attached to the incident.
    await expect
      .poll(async () => (await aget(`/api/v1/incidents/${incidentID}/phone-activity`)).deliveries.filter((d: any) => d.reason === "sent").length,
        { message: "the incident's phone activity lists the sent SMS", timeout: 30_000 })
      .toBeGreaterThan(0);

    // 4a: the incident page's Phone panel.
    await page.goto(`/incidents/${incidentID}`);
    const panel = page.getByTestId("incident-phone-panel");
    await expect(panel).toBeVisible();
    await expect(panel.getByTestId("incident-phone-public")).toHaveText("not on the status page");
    const row = panel.getByTestId("delivery-row").filter({ has: page.getByTestId("delivery-channel").filter({ hasText: SMS_NAME }) }).first();
    await expect(row.getByTestId("delivery-reason")).toContainText("Sent");
    await expect(row.getByTestId("delivery-mask")).toHaveText(adminMask);
    await expect(row.getByTestId("delivery-kind")).toContainText("SMS");
    await expect(row.getByTestId("delivery-detail")).toContainText("HTTP 201");
    expect(await page.content(), "no full number on the incident page").not.toContain(ADMIN_NUMBER.slice(1));

    // 3g: the channel's deliveries name the incident by its ref.
    await openChannels(page, harnessID);
    await channelRow(page, SMS_NAME).getByTestId("channel-deliveries-open").click();
    const deliveries = page.getByTestId("channel-deliveries");
    await expect(deliveries.getByTestId("deliveries-title")).toHaveText(`Deliveries · ${SMS_NAME}`);
    const drow = deliveries.getByTestId("delivery-row").filter({ has: page.getByTestId("delivery-ref").filter({ hasText: ref }) });
    await expect(drow).toHaveCount(1);
    await expect(drow.getByTestId("delivery-reason")).toContainText("Sent");
    await expect(drow.getByTestId("delivery-mask")).toHaveText(adminMask);

    // 5a: the short link, in any case, opens the incident.
    await page.goto(`/i/${ref.toLowerCase()}`);
    await expect(page).toHaveURL(new RegExp(`/incidents/${incidentID}$`), { timeout: 15_000 });

    // 5d: a malformed ref asks nothing; a well-formed unknown one is not found.
    const byRef: string[] = [];
    page.on("request", (r) => {
      if (r.url().includes("/api/v1/incidents/by-ref/")) byRef.push(r.url());
    });
    await page.goto("/i/abc");
    await expect(page.getByTestId("incident-ref-not-found")).toContainText("No incident found");
    expect(byRef, "no lookup for a malformed ref").toEqual([]);
    await page.goto("/i/00000000");
    await expect(page.getByTestId("incident-ref-not-found")).toContainText("No incident found");
    expect(byRef.length, "a well-formed ref is looked up").toBe(1);
  });

  test("6 — an SSO sign-in started from /i/:ref returns to it (5e-1)", async ({ browser }) => {
    expect(downIncident, "test 5 opened an incident").toBeTruthy();
    const { id, ref } = downIncident!;
    const context = await browser.newContext({ baseURL: BASE, storageState: { cookies: [], origins: [] } });
    try {
      const page = await context.newPage();
      await page.goto(`/i/${ref}`);
      await expect(page).toHaveURL(/\/login\?/);
      expect(new URL(page.url()).searchParams.get("redirect")).toBe(`/i/${ref}`);
      const sso = page.locator("button", { hasText: /SSO|Keycloak/ });
      await expect(sso, "single-stack E2E requires the configured Keycloak profile").toBeVisible({ timeout: 15_000 });
      await sso.click();
      await page.waitForURL(/realms\/cerbix/, { timeout: 15_000 });
      await page.fill("#username", "testuser@example.com");
      await page.fill("#password", "password");
      await page.click("#kc-login");
      await page.waitForURL((u) => !u.href.includes("realms") && !u.pathname.startsWith("/auth/"), { timeout: 20_000 });
      const path = new URL(page.url()).pathname;
      expect(path, "back at the link, not the dashboard").not.toBe("/");
      expect([`/i/${ref}`, `/incidents/${id}`]).toContain(path);
      console.log(`[phone-alerts] SSO sign-in from /i/:ref returned to ${path === `/i/${ref}` ? "/i/<ref>" : "/incidents/<id>"}`);
      if (path === `/i/${ref}`) {
        await expect(page.getByTestId("incident-ref-not-found").or(page.getByTestId("incident-ref-forbidden"))).toBeVisible();
      }
    } finally {
      await context.close();
    }
  });

  test("7 — route loss and telephony off (1f, 3e′, 4g-1, 4g-2, 2j′)", async ({ page }) => {
    expect(downIncident, "test 5 opened an incident").toBeTruthy();
    // The project's only channel: SMS to the admin. A service that owns paging routes through it.
    const ch = await asend("POST", `/api/v1/projects/${phoneProjectID}/notification-channels`, {
      type: "sms", name: ROUTE_NAME, recipient: "user", recipient_user_id: adminID,
    });
    expect(ch.status(), await ch.text()).toBe(201);
    const svc = await asend("POST", `/api/v1/projects/${phoneProjectID}/services`, { slug: SERVICE_SLUG, name: `E2E phone ${stamp}` });
    expect(svc.status(), await svc.text()).toBe(201);
    serviceID = (await aget(`/api/v1/projects/${phoneProjectID}/services`)).find((s: any) => s.service.slug === SERVICE_SLUG).service.id;
    const owns = await asend("PATCH", `/api/v1/projects/${phoneProjectID}/services/${serviceID}/alerting`, { owns_paging: true });
    expect(owns.status(), await owns.text()).toBe(200);

    // 1f: turning phone alerts off would leave the service with no route.
    await page.goto("/settings?tab=telephony");
    await expect(page.getByTestId("telephony-pill")).toHaveText("On · Generic HTTP");
    await page.getByTestId("telephony-enabled").uncheck();
    let put = page.waitForResponse((r) => r.request().method() === "PUT" && r.url().endsWith("/api/v1/settings/telephony"));
    await page.getByTestId("telephony-save").click();
    expect((await put).status(), "the unconfirmed switch-off").toBe(409);
    const warning = page.getByTestId("telephony-unroutable");
    await expect(warning).toContainText("Not saved yet —");
    await expect(warning).toContainText("would be left with no route");
    const group = warning.getByTestId("telephony-unroutable-group").filter({ hasText: SERVICE_SLUG });
    await expect(group).toContainText(`· project ${PROJECT_SLUG}`);
    await expect(group).toContainText("service");
    expect((await aget("/api/v1/settings/telephony")).enabled, "nothing was saved by the 409").toBe(true);
    await expect(page.getByTestId("telephony-unroutable-confirm")).toHaveText("Turn off anyway");
    put = page.waitForResponse((r) => r.request().method() === "PUT" && r.url().endsWith("/api/v1/settings/telephony"));
    await page.getByTestId("telephony-unroutable-confirm").click();
    expect((await put).status(), "the confirmed switch-off").toBe(200);
    await expect(page.getByTestId("telephony-saved")).toHaveText("✓ Saved");
    await expect(page.getByTestId("telephony-pill")).toHaveText("Off");

    // 4g-2: a trace is never hidden because the switch is off now. (The incident page opens an
    // incident of the current project only, so this runs while the workspace is still the harness.)
    await page.goto(`/incidents/${downIncident!.id}`);
    await expect(page.getByTestId("incident-phone-panel")).toBeVisible();
    await expect(page.getByTestId("incident-phone-panel").getByTestId("delivery-row").first()).toBeVisible();

    // 3e′: the channel stays, and says why it is not a route. From here the workspace is the
    // route-loss project.
    await openChannels(page, phoneProjectID);
    await expect(channelRow(page, ROUTE_NAME).getByTestId("channel-route-chip")).toHaveText("Not a route · phone alerts are off on this instance");

    // 4g-1: no deliveries and telephony off — no panel at all.
    const inc = await asend("POST", `/api/v1/projects/${phoneProjectID}/incidents`, { title: NOPANEL_TITLE, impact: "minor", body: "e2e" });
    expect(inc.status(), await inc.text()).toBe(201);
    nopanelIncidentID = (await inc.json()).id;
    const activity = page.waitForResponse((r) => r.url().endsWith(`/api/v1/incidents/${nopanelIncidentID}/phone-activity`));
    await page.goto(`/incidents/${nopanelIncidentID}`);
    expect((await activity).status()).toBe(200);
    await expect(page.getByText(NOPANEL_TITLE).first()).toBeVisible();
    await expect(page.getByTestId("incident-phone-panel")).toHaveCount(0);

    // 2j′: the number stays verified; Replace needs a code, so it is disabled.
    await page.goto("/settings?tab=phone");
    await expect(page.getByTestId("phone-pill")).toHaveText("Verified");
    await expect(page.getByTestId("phone-off-kept")).toBeVisible();
    await expect(page.getByTestId("phone-replace")).toBeDisabled();
    await expect(page.getByTestId("phone-remove")).toBeEnabled();
    await expect(page.getByTestId("phone-send-call")).toHaveCount(0);
  });
});
