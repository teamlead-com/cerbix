import { test, expect } from "@playwright/test";
import * as crypto from "crypto";
import { ADMIN, apiGet, apiSend, firstProject, cleanupMonitors } from "./helpers";

// RFC 6238 TOTP (SHA-1, 6 digits, 30s period) — enough to act as the
// authenticator app in the enroll/login flow.
function totpCode(secretB32: string, at = Date.now()): string {
  const clean = secretB32.replace(/=+$/, "").toUpperCase();
  const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567";
  let bits = "";
  for (const ch of clean) bits += alphabet.indexOf(ch).toString(2).padStart(5, "0");
  const bytes = Buffer.from((bits.match(/.{8}/g) ?? []).map((b) => parseInt(b, 2)));
  const counter = Buffer.alloc(8);
  counter.writeBigUInt64BE(BigInt(Math.floor(at / 1000 / 30)));
  const h = crypto.createHmac("sha1", bytes).update(counter).digest();
  const off = h[h.length - 1] & 0x0f;
  const code = ((h.readUInt32BE(off) & 0x7fffffff) % 1_000_000).toString().padStart(6, "0");
  return code;
}

test.describe("time-based flows", () => {
  test("confirm phase accelerates time-to-down", async ({ page }) => {
    test.setTimeout(120_000);
    const { projectID } = await firstProject(page);
    const r = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
      name: "e2e-confirm", type: "tcp", target: "localhost:9", region: "core",
      interval_seconds: 60, timeout_seconds: 3, failure_threshold: 3,
      confirm_interval_seconds: 5, auto_incident: false,
    });
    expect(r.status(), await r.text()).toBe(201);
    const mon = await r.json();
    try {
      // With a 60s interval and threshold 3, flat probing needs ~3 minutes;
      // the confirm phase re-probes every 5s, so down must arrive far sooner.
      const started = Date.now();
      const deadline = started + 100_000;
      let status = "";
      while (Date.now() < deadline) {
        status = (await apiGet(page, `/api/v1/monitors/${mon.id}`)).status;
        if (status === "down") break;
        await page.waitForTimeout(2000);
      }
      const took = (Date.now() - started) / 1000;
      expect(status, `still "${status}" after ${took.toFixed(0)}s`).toBe("down");
      expect(took, "confirm phase must beat flat 3×60s probing").toBeLessThan(100);
    } finally {
      await cleanupMonitors(page, projectID);
    }
  });

  test("TOTP: enroll → login demands a code → code works → disable", async ({ page }) => {
    const enroll = await apiSend(page, "post", "/api/v1/me/totp/enroll");
    expect(enroll.ok(), await enroll.text()).toBeTruthy();
    const { secret } = await enroll.json();
    expect(secret).toBeTruthy();
    try {
      const enable = await apiSend(page, "post", "/api/v1/me/totp/enable", { code: totpCode(secret) });
      expect(enable.ok(), await enable.text()).toBeTruthy();

      const anon = await page.context().browser()!.newContext({ storageState: { cookies: [], origins: [] } });
      // Password alone → 401 with the totp_required discriminator.
      const bare = await anon.request.post("/auth/local/login", { data: { username: ADMIN.email, password: ADMIN.password } });
      expect(bare.status()).toBe(401);
      expect((await bare.json()).totp_required).toBeTruthy();
      // Password + a live code → session.
      const withCode = await anon.request.post("/auth/local/login", {
        data: { username: ADMIN.email, password: ADMIN.password, totp: totpCode(secret) },
      });
      expect(withCode.ok(), await withCode.text()).toBeTruthy();
      await anon.close();
    } finally {
      const disable = await apiSend(page, "post", "/api/v1/me/totp/disable", { password: ADMIN.password });
      expect(disable.ok(), await disable.text()).toBeTruthy();
    }
  });
});
