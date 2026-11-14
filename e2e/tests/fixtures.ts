import { test as base, expect, type Browser, type BrowserContext } from "@playwright/test";

// S9b (sec-hardening-2 H-9b): `make dev-test` runs with zero CSP reports. In this release the policy
// is REPORT-ONLY, so a violation blocks nothing and is invisible unless someone listens: every page
// of every context a test opens — the default one and any `browser.newContext()` — subscribes to
// `securitypolicyviolation`, and the test fails on one. Only violations on the cerbix origin count;
// another site's page (the identity provider's sign-in) answers to its own policy.
//
// Every spec imports `test` from here, never from "@playwright/test" directly.

export { expect };

type Violation = { page: string; directive: string; blocked: string; source: string; disposition: string };

const BINDING = "__cerbixCSPViolation";

const LISTENER = `(() => {
  document.addEventListener("securitypolicyviolation", (e) => {
    try {
      window.${BINDING}({
        page: location.href,
        directive: e.effectiveDirective || e.violatedDirective,
        blocked: e.blockedURI,
        source: (e.sourceFile || "") + ":" + e.lineNumber + ":" + e.columnNumber,
        disposition: e.disposition,
      });
    } catch (_) {}
  });
})();`;

async function watch(context: BrowserContext, into: Violation[]) {
  await context.exposeBinding(BINDING, (_source, v: Violation) => {
    into.push(v);
  });
  await context.addInitScript({ content: LISTENER });
}

export const test = base.extend<{ cspGuard: void }>({
  cspGuard: [
    async ({ context, browser }, use, testInfo) => {
      const violations: Violation[] = [];
      await watch(context, violations);
      // Contexts a test opens itself (anonymous visitors, a second user) are watched too.
      const b = browser as Browser;
      const original = b.newContext.bind(b);
      b.newContext = async (...args: Parameters<Browser["newContext"]>) => {
        const created = await original(...args);
        await watch(created, violations);
        return created;
      };
      try {
        await use();
      } finally {
        b.newContext = original;
      }
      const origin = new URL(String(testInfo.project.use.baseURL || "http://localhost:8080")).origin;
      const ours = violations.filter((v) => {
        try {
          return new URL(v.page).origin === origin;
        } catch {
          return true;
        }
      });
      expect(ours, `Content-Security-Policy violations on ${origin}:\n${JSON.stringify(ours, null, 2)}`).toEqual([]);
    },
    { auto: true },
  ],
});
