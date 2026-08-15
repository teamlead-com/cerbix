import { defineConfig } from "@playwright/test";

// The suite runs against a LIVE local Cerbix stack. The canonical single gate
// is `make dev-up && make dev-test`; it requires the SSO and mail profiles.
// Tests create their own e2e-prefixed entities, but never point this at prod.
//
// Sessions: the setup project signs in ONCE and every spec reuses the stored
// state — local logins are rate-limited (login_rate_limit_per_minute), so
// per-test logins would trip the limiter and flake.
export default defineConfig({
  testDir: "./tests",
  timeout: 60_000,
  workers: 1, // one shared instance — serialize to keep state deterministic
  retries: 0,
  reporter: [["list"]],
  use: {
    baseURL: process.env.CERBIX_URL || "http://localhost:8080",
    screenshot: "only-on-failure",
    trace: "retain-on-failure",
  },
  projects: [
    { name: "setup", testMatch: /auth\.setup\.ts/ },
    {
      name: "chromium",
      dependencies: ["setup"],
      use: { storageState: ".auth/admin.json" },
      testIgnore: process.env.CERBIX_TOPOLOGY === "geo"
        ? /auth\.setup\.ts/
        : /auth\.setup\.ts|topology-geo\.spec\.ts/,
    },
  ],
});
