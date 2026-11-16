import { test, expect } from "./fixtures";
import type { Page } from "@playwright/test";
import { apiGet, apiSend, firstProject } from "./helpers";

// FR-043 / NFR-036 (docs/specs/func-recovery-threshold.md revision 6, §7 row "E2E"): one monitor at
// recovery_threshold 2 against a target that fails, succeeds once, fails, then succeeds twice gives
// ONE recovery notification and ONE incident — the lucky single success in the middle announces
// nothing, resolves nothing and starts a run that the next failure breaks (§4.1, §4.2). The
// confirmed recovery's notification and the auto-incident's resolving update carry the same
// sentence, `Recovered: 2/2 consecutive checks passed; first OK at <RFC 3339 Z>` (§4.6), whose
// instant is the first success of the run that COMPLETED (D6), not of the broken one.
//
// The controllable target is cerbix's own public status-page endpoint: an http monitor probes
// `GET /api/v1/public/status-pages/<slug>` over the compose network, and the test flips the page's
// visibility — `public` answers 200 (up), `internal` answers 404 (down). The visibility gate is read
// from the database on every request ahead of the render cache (handlers_statuspage.go
// publicStatusPage), so a flip takes effect on the very next check, and the MONITOR's configuration
// never changes during the scenario — the qualifier NFR-036 states. Notifications go to an email
// channel delivering to the Mailpit sidecar, exactly as mail.spec.ts reads it, under a recipient
// unique to this run.
//
// The one time-sensitive step is "succeed ONCE": the test watches the run counter and flips the page
// back to `internal` as soon as it reads 1, inside one check interval. The interval is 10 s rather
// than the 5 s probers.spec.ts uses so that the flip AND the monitor-page load that reads the
// "recovering 1/2" chip both fit comfortably before the next check. Expected run time ≈ 1.5–2 min.
//
// API-shaped, on the stored session (local logins are rate-limited); everything is prefixed
// `e2e-recovery` and removed in `finally`.

const MAILPIT = process.env.MAILPIT_URL || "http://localhost:8025";
const MON = "e2e-recovery-threshold";
const SLUG = "e2e-recovery-threshold";
const CHANNEL = "e2e-recovery-mail";
const INTERVAL_S = 10;
const STEP_MS = 4 * INTERVAL_S * 1000; // one state change: a check interval plus scheduler/snapshot slack, ×4

const SENTENCE = /Recovered: 2\/2 consecutive checks passed; first OK at (\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z)/;

// A Go RFC 3339 timestamp (microseconds, any offset) in epoch ms.
function ms(ts: string): number {
  return Date.parse(ts.replace(/(\.\d{3})\d+/, "$1"));
}
// The format §4.6 fixes for the first-OK instant: whole seconds, truncated, `Z`.
function rfc3339Seconds(ts: string): string {
  return new Date(Math.floor(ms(ts) / 1000) * 1000).toISOString().replace(/\.\d{3}Z$/, "Z");
}

async function until<T>(page: Page, label: string, read: () => Promise<T>, ok: (v: T) => boolean, timeoutMs: number, everyMs = 1000): Promise<T> {
  const deadline = Date.now() + timeoutMs;
  let last: T = await read();
  while (!ok(last)) {
    if (Date.now() >= deadline) throw new Error(`${label}: not reached within ${timeoutMs} ms; last seen ${JSON.stringify(last)}`);
    await page.waitForTimeout(everyMs);
    last = await read();
  }
  return last;
}

type Mail = { id: string; text: string };

async function mailpitUp(page: Page): Promise<boolean> {
  try {
    return (await page.request.get(`${MAILPIT}/api/v1/messages`)).ok();
  } catch {
    return false;
  }
}

// Every message Mailpit holds for the recipient, with its decoded plain-text body.
async function mailsTo(page: Page, rcpt: string): Promise<Mail[]> {
  const list = await (await page.request.get(`${MAILPIT}/api/v1/messages?limit=500`)).json();
  const mine = (list.messages ?? []).filter((m: any) => m.To?.some((t: any) => t.Address === rcpt));
  const out: Mail[] = [];
  for (const m of mine) {
    const full = await (await page.request.get(`${MAILPIT}/api/v1/message/${m.ID}`)).json();
    out.push({ id: m.ID, text: String(full.Text ?? "").trim() });
  }
  return out;
}

test.describe("recovery threshold (FR-043)", () => {
  test.beforeEach(async ({ page }) => {
    // The target is cerbix's own public status page at http://cerbix:8080, a host name that exists
    // only in the single topology's compose network; the other topologies have no target to probe.
    test.skip((process.env.CERBIX_TOPOLOGY ?? "single") !== "single", "the probe target exists in the single topology only");
    const available = await mailpitUp(page);
    expect(available, "single-stack E2E requires the Mailpit profile").toBeTruthy();
  });

  test("threshold 2: fail → one success → fail → two successes gives one recovery notification and one incident", async ({ page }) => {
    test.setTimeout(6 * 60_000);
    const { orgID, projectID } = await firstProject(page);
    const rcpt = `e2e-recovery-${Date.now()}@example.com`;
    const recoveredLine = `🟢 ${MON} recovered — it is back UP.`;
    const downLine = `🔴 ${MON} is DOWN.`;

    // Leftovers of an aborted earlier run.
    for (const p of await apiGet(page, `/api/v1/organizations/${orgID}/status-pages`)) {
      if (p.slug === SLUG) await apiSend(page, "delete", `/api/v1/status-pages/${p.id}`);
    }
    for (const m of await apiGet(page, `/api/v1/projects/${projectID}/monitors`)) {
      if (m.name === MON) await apiSend(page, "delete", `/api/v1/monitors/${m.id}`);
    }
    for (const c of await apiGet(page, `/api/v1/projects/${projectID}/notification-channels`)) {
      if (c.name === CHANNEL) await apiSend(page, "delete", `/api/v1/notification-channels/${c.id}`);
    }

    let pageID = "";
    let monitorID = "";
    let channelID = "";
    try {
      // ── The target: a public status page answers 200, an internal one 404.
      const sp = await apiSend(page, "post", `/api/v1/organizations/${orgID}/status-pages`, {
        slug: SLUG, title: "E2E Recovery Threshold", visibility: "public",
      });
      expect(sp.status(), await sp.text()).toBe(201);
      pageID = (await sp.json()).id;
      const setTarget = async (healthy: boolean) => {
        const r = await apiSend(page, "patch", `/api/v1/status-pages/${pageID}`, { visibility: healthy ? "public" : "internal" });
        expect(r.ok(), await r.text()).toBeTruthy();
        // The same gate the monitor's check meets, verified from outside.
        expect((await page.request.get(`/api/v1/public/status-pages/${SLUG}`)).status(), `target healthy=${healthy}`).toBe(healthy ? 200 : 404);
      };
      await setTarget(true);

      // ── The channel the test can read: email to the Mailpit sidecar, a recipient unique to this run.
      const ch = await apiSend(page, "post", `/api/v1/projects/${projectID}/notification-channels`, {
        type: "email", name: CHANNEL,
        config: { to: rcpt, from: "cerbix@e2e.local", smtp_host: "mailpit", smtp_port: "1025" },
      });
      expect(ch.status(), await ch.text()).toBe(201);
      channelID = (await ch.json()).id;

      // ── The monitor: threshold 2 up, 1 down, an auto-incident, no reminders, no confirm phase,
      // no in-check retries — so every check is exactly one verdict.
      const created = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
        name: MON, type: "http", target: `http://cerbix:8080/api/v1/public/status-pages/${SLUG}`, region: "core",
        interval_seconds: INTERVAL_S, timeout_seconds: 5, retries: 0,
        failure_threshold: 1, confirm_interval_seconds: 0, recovery_threshold: 2,
        auto_incident: true, renotify_seconds: 0,
      });
      expect(created.status(), await created.text()).toBe(201);
      const mon = await created.json();
      monitorID = mon.id;
      expect(mon.recovery_threshold, "the threshold is stored as written").toBe(2);
      // pending → up is never notified (MonitorTransition.ShouldNotify), so linking now adds no mail.
      const link = await apiSend(page, "post", `/api/v1/monitors/${monitorID}/notifications`, { channel_id: channelID });
      expect(link.status(), await link.text()).toBe(204);

      const monitor = () => apiGet(page, `/api/v1/monitors/${monitorID}`);
      const heartbeats = async (): Promise<any[]> =>
        (await apiGet(page, `/api/v1/monitors/${monitorID}/heartbeats?limit=200`)).slice().sort((a: any, b: any) => ms(a.ts) - ms(b.ts));
      const incidents = async (): Promise<any[]> =>
        (await apiGet(page, `/api/v1/projects/${projectID}/incidents`)).filter((i: any) => i.monitor_id === monitorID);
      const newestHeartbeat = async () => { const all = await heartbeats(); return all[all.length - 1]; };

      // ── 0. Healthy: the first check takes the new monitor straight to up (D7).
      await until(page, "first check → up", monitor, (m) => m.status === "up", STEP_MS + 15_000);

      // ── 1. FAIL: down on the first failure, the auto-incident opens.
      await setTarget(false);
      await until(page, "fail → down", monitor, (m) => m.status === "down", STEP_MS);
      const opened = await until(page, "auto-incident opened", incidents, (l) => l.length === 1, 15_000);
      expect(opened[0].status, "the incident is open").not.toBe("resolved");

      // ── 2. SUCCEED ONCE: still down, a run of 1, no recovery. The page goes back to internal the
      // moment the counter reads 1, so the next check fails.
      await setTarget(true);
      const firstRun = await until(page, "one success → run 1", monitor, (m) => m.consecutive_successes >= 1 || m.status !== "down", STEP_MS, 250);
      await setTarget(false);
      expect(firstRun.status, "one success at threshold 2 leaves the monitor down").toBe("down");
      expect(firstRun.consecutive_successes, "the run counts the single success").toBe(1);
      expect(firstRun.recovery_started_at, "the run's first success is stamped").toBeTruthy();
      const lucky = await newestHeartbeat();
      expect(lucky?.up, "the newest check is the lucky success").toBe(true);

      // The monitor page shows the run next to the Down pill (§8) — as of page load (D13), which is
      // why this read happens inside the interval, before the next (failing) check.
      await page.goto(`/monitors/${monitorID}`);
      const chip = page.getByTestId("monitor-recovering");
      await expect(chip).toHaveText("recovering 1/2", { timeout: 7_000 });
      await expect(chip).toHaveAttribute("title", /1 of 2 consecutive successful checks/);

      expect((await incidents()).map((i) => i.status), "the incident stays open during the run").not.toContain("resolved");
      expect((await mailsTo(page, rcpt)).filter((m) => m.text.includes(recoveredLine)), "no recovery notification for one success").toHaveLength(0);

      // ── 3. FAIL: the run breaks; still down, counter and stamp cleared, still no recovery.
      const broken = await until(page, "failure after the lucky success", newestHeartbeat,
        (hb) => !!hb && ms(hb.ts) > ms(lucky.ts) && hb.up === false, STEP_MS, 500);
      expect(broken.up).toBe(false);
      const afterBreak = await monitor();
      expect(afterBreak.status, "a broken run stays down").toBe("down");
      expect(afterBreak.consecutive_successes, "a failure ends the run").toBe(0);
      expect(afterBreak.recovery_started_at, "a broken run clears its first-OK stamp").toBeNull();
      // The DOWN alert of step 1 has had time to arrive; no recovery mail ever has.
      const beforeRecovery = await until(page, "the DOWN mail", () => mailsTo(page, rcpt),
        (l) => l.some((m) => m.text.includes(downLine)), 45_000);
      expect(beforeRecovery.filter((m) => m.text.includes(recoveredLine)), "still no recovery notification").toHaveLength(0);
      const stillOpen = await incidents();
      expect(stillOpen.length, "the broken run opened no second incident").toBe(1);
      expect(stillOpen[0].status, "the incident is still open").not.toBe("resolved");

      // ── 4. SUCCEED TWICE: the second run completes at 2/2.
      await setTarget(true);
      const secondRun = await until(page, "second run → 1", monitor, (m) => m.consecutive_successes >= 1 || m.status !== "down", STEP_MS, 500);
      expect(secondRun.status, "the first success of the second run leaves the monitor down").toBe("down");
      expect(secondRun.consecutive_successes).toBe(1);
      const secondStart = secondRun.recovery_started_at as string;
      expect(secondStart, "the second run is stamped afresh").toBeTruthy();
      expect(ms(secondStart), "the stamp belongs to the run after the break").toBeGreaterThan(ms(firstRun.recovery_started_at));

      const recovered = await until(page, "second success → up", monitor, (m) => m.status === "up", STEP_MS);
      expect(recovered.consecutive_successes, "the counter is zero once up").toBe(0);
      expect(recovered.recovery_started_at, "the stamp is cleared by the confirming transaction").toBeNull();

      // The checks really were fail, ONE success, fail, two successes (as run lengths).
      const runs: { up: boolean; n: number }[] = [];
      for (const hb of await heartbeats()) {
        const last = runs[runs.length - 1];
        if (last && last.up === hb.up) last.n++;
        else runs.push({ up: hb.up, n: 1 });
      }
      const shape = runs.map((r) => `${r.up ? "ok" : "fail"}×${r.n}`).join(" ");
      expect(runs.length, `heartbeat runs: ${shape}`).toBe(5);
      expect(runs.map((r) => r.up), `heartbeat runs: ${shape}`).toEqual([true, false, true, false, true]);
      expect(runs[2].n, `exactly one lucky success; heartbeat runs: ${shape}`).toBe(1);
      expect(runs[4].n, `the completed run; heartbeat runs: ${shape}`).toBeGreaterThanOrEqual(2);

      // ── One recovery notification, with the sentence; one DOWN notification. A short grace after
      // the first recovery mail lets a duplicate show up if there were one.
      await until(page, "the recovery mail", () => mailsTo(page, rcpt), (l) => l.some((m) => m.text.includes(recoveredLine)), 60_000);
      await page.waitForTimeout(5_000);
      const mails = await mailsTo(page, rcpt);
      const recoveryMails = mails.filter((m) => m.text.includes(recoveredLine));
      const downMails = mails.filter((m) => m.text.includes(downLine));
      expect(recoveryMails, `exactly one recovery notification; got ${JSON.stringify(mails)}`).toHaveLength(1);
      expect(downMails, `exactly one DOWN notification; got ${JSON.stringify(mails)}`).toHaveLength(1);
      const match = recoveryMails[0].text.match(SENTENCE);
      expect(match, `the recovery text carries the sentence: ${recoveryMails[0].text}`).not.toBeNull();
      const sentence = match![0];
      expect(recoveryMails[0].text, "one space after today's text, then the sentence").toBe(`${recoveredLine} ${sentence}`);
      expect(match![1], "first OK is the first success of the run that completed (D6)").toBe(rfc3339Seconds(secondStart));

      // ── One incident over the whole scenario, resolved by the confirmed recovery with the same sentence.
      const finalIncidents = await until(page, "incident resolved", incidents, (l) => l.length >= 1 && l.every((i) => i.status === "resolved"), 15_000);
      expect(finalIncidents, "exactly one incident for the monitor").toHaveLength(1);
      const inc = finalIncidents[0];
      expect(inc.resolved_at, "resolved_at is set").toBeTruthy();
      expect(ms(match![1]) + 999, "first OK is not earlier than the incident's start (one ingest consumer, D6)").toBeGreaterThanOrEqual(ms(inc.started_at));
      const updates: any[] = await apiGet(page, `/api/v1/incidents/${inc.id}/updates`);
      const resolving = updates.filter((u) => u.status === "resolved");
      expect(resolving, `one resolving update; got ${JSON.stringify(updates)}`).toHaveLength(1);
      expect(resolving[0].body, "the resolving update carries the notification's sentence (I14)").toBe(`Monitor recovered — automatically resolved. ${sentence}`);
    } finally {
      if (monitorID) {
        for (const i of await apiGet(page, `/api/v1/projects/${projectID}/incidents`)) {
          if (i.monitor_id === monitorID && i.status !== "resolved") {
            await apiSend(page, "post", `/api/v1/incidents/${i.id}/updates`, { status: "resolved", body: "cleanup" });
          }
        }
        await apiSend(page, "delete", `/api/v1/monitors/${monitorID}`);
      }
      if (channelID) await apiSend(page, "delete", `/api/v1/notification-channels/${channelID}`);
      if (pageID) await apiSend(page, "delete", `/api/v1/status-pages/${pageID}`);
    }
  });
});
