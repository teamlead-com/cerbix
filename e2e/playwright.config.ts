import { defineConfig } from "@playwright/test";

// The suite runs against a LIVE cerbix stack (docker compose --profile single;
// add --profile sso for the OIDC spec). It creates its own entities (prefixed
// e2e-) and cleans them up, but point it at a dev instance, never production.
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
      testIgnore: /auth\.setup\.ts/,
    },
  ],
});
