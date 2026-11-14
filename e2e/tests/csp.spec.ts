import { test, expect } from "./fixtures";

// S9b (sec-hardening-2 H-9b) on the live stack. The application port serves the policy REPORT-ONLY
// in this release, and the fixture (./fixtures.ts) fails any test during which a cerbix page
// reports a violation. The second test is the fixture's own proof: it commits a violation on
// purpose — an inline script, which `script-src 'self'` refuses — and is expected to FAIL. If the
// policy were missing, or the listener deaf, it would pass, and Playwright reports an expected
// failure that passed as a failure.
test.describe("Content-Security-Policy", () => {
  test("the SPA and a public status page are served with the report-only policy, none enforced", async ({ page }) => {
    for (const path of ["/", "/status/e2e-no-such-page"]) {
      const res = await page.goto(path);
      const headers = res!.headers();
      expect(headers["content-security-policy-report-only"], path).toContain("script-src 'self'");
      expect(headers["content-security-policy"], `${path}: enforcement is the next release`).toBeUndefined();
      expect(headers["x-content-type-options"], path).toBe("nosniff");
    }
  });

  test("a violation on a cerbix page fails the test", async ({ page }) => {
    test.fail(true, "the fixture must fail a test on a report-only violation");
    await page.goto("/");
    await page.evaluate(() => {
      const s = document.createElement("script");
      s.textContent = "window.__cerbixInlineRan = true;";
      document.head.appendChild(s);
    });
    await page.waitForTimeout(500);
  });
});
