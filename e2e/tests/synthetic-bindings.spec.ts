import { test, expect } from "@playwright/test";
import { apiGet, apiSend, ensureE2EWorkspace, cleanupMonitors } from "./helpers";

// FR-028 stage 2 in the editor, against a live stack. Two things make this spec worth its
// runtime: it is the FIRST E2E anywhere that touches a synthetic monitor — FR-SYN-3's named
// gap, recorded in docs/status.md — and it proves the three properties the owner approved the
// mock for, on the surface an operator actually uses:
//
//   1. the binding → inventory mapping is on screen, and it round-trips through a save;
//   2. a credential-bearing header cannot hold a literal (D7), and the refusal names it;
//   3. a scenario carrying a binding is NOT testable before it is saved (D10), said at the
//      button rather than discovered as a 400.
//
// Everything is prefixed `e2e-` and cleaned up, per the suite's contract with dev stacks.
test.describe("synthetic secret bindings", () => {
  const SECRET = "e2e-scenario-token";

  test.afterEach(async ({ page }) => {
    const { projectID } = await ensureE2EWorkspace(page);
    await cleanupMonitors(page, projectID);
    await apiSend(page, "delete", `/api/v1/projects/${projectID}/secrets/${SECRET}`);
  });

  test("a credential in a scenario is a named binding, and the editor says so", async ({ page }) => {
    await page.goto("/");
    const { projectID } = await ensureE2EWorkspace(page);

    // A binding resolves a project secret by NAME, so the inventory entry comes first.
    await apiSend(page, "delete", `/api/v1/projects/${projectID}/secrets/${SECRET}`);
    const made = await apiSend(page, "post", `/api/v1/projects/${projectID}/secrets`, {
      name: SECRET,
      value: "e2e-scenario-secret-value",
    });
    expect(made.status(), "the secret the binding will point at").toBe(201);

    await page.goto(`/monitors/new?project=${projectID}`);
    await page.locator("button", { hasText: "Synthetic" }).click();
    await expect(page.getByTestId("scenario-secrets")).toBeVisible();
    // The form opens VALID: the default scaffold demonstrates extract → interpolate and puts no
    // credential anywhere, so there is no D7 refusal to start with (party finding, P1 — it used
    // to open with `Authorization: Bearer {{token}}`, which the server refuses).
    await expect(page.locator("text=must be exactly")).toHaveCount(0);

    // ── The mapping, declared where the operator is ─────────────────────────────────────
    await page.getByTestId("new-binding").click();
    await page.getByTestId("new-binding-name").fill("login");
    await page.getByTestId("new-binding-secret").selectOption(SECRET);
    await page.getByTestId("add-binding").click();

    const binding = page.getByTestId("scenario-binding").first();
    await expect(binding).toBeVisible();
    // The panel never shows a binding name alone: the secret it resolves to is on the row.
    await expect(binding).toContainText("login");
    await expect(binding.getByTestId("scenario-binding-secret")).toHaveValue(SECRET);
    // Declared and unused is refused by the server, so the editor says it before a save.
    await expect(binding).toContainText("never used");

    await page.getByPlaceholder("payments-callback").fill("e2e-synthetic-binding");

    // ── D7 at the header field ──────────────────────────────────────────────────────────
    // Add a credential-bearing header the way an operator does. Its value control is a binding
    // selector from the first keystroke — never a free-text box, which is what invited a pasted
    // token in the first place.
    await page.locator("button", { hasText: "+ header" }).first().click();
    await page.getByPlaceholder("Header").first().fill("authorization");
    const picker = page.getByTestId("header-binding").first();
    await expect(picker).toBeVisible();
    await picker.selectOption("login");
    await expect(page.locator("text=must be exactly")).toHaveCount(0);
    await expect(binding).not.toContainText("never used");

    // ── D10 at the button ───────────────────────────────────────────────────────────────
    await expect(page.getByTestId("test-blocked")).toContainText("Save the monitor before testing it");
    await expect(page.getByTestId("test-connection")).toBeDisabled();

    // ── Save, and the wire shape is the flat key plus the placeholder ────────────────────
    await page.locator("button", { hasText: /Create monitor/ }).click();
    // Poll the API rather than the URL: `/monitors/new` itself matches any sane "back to the
    // list" pattern, so a URL wait here returns instantly and reads the list before the POST
    // has landed. It passed alone and failed in the full suite, which is the signature of a
    // test that waited on the wrong thing.
    let saved: any;
    await expect
      .poll(
        async () => {
          const list = await apiGet(page, `/api/v1/projects/${projectID}/monitors`);
          saved = (list as any[]).find((m) => m.name === "e2e-synthetic-binding");
          return !!saved;
        },
        { timeout: 15_000, message: "the monitor was created" },
      )
      .toBe(true);
    expect(saved.config.scenario_secret_login_ref).toBe(SECRET);
    expect(saved.config.scenario, "the document carries the placeholder, never the value").toContain(
      "{{secret:login}}",
    );
    expect(JSON.stringify(saved)).not.toContain("e2e-scenario-secret-value");

    // ── And it round-trips: the panel rebuilds from the stored keys ──────────────────────
    await page.goto(`/monitors/${saved.id}/edit`);
    const reloaded = page.getByTestId("scenario-binding").first();
    await expect(reloaded).toBeVisible();
    await expect(reloaded.getByTestId("scenario-binding-secret")).toHaveValue(SECRET);
    await expect(page.getByTestId("test-blocked")).toBeVisible();

    // The secret is now referenced, so the inventory refuses to strand the monitor — the same
    // guard `password_ref` has, reached through a scenario binding.
    const refused = await apiSend(page, "delete", `/api/v1/projects/${projectID}/secrets/${SECRET}`);
    expect(refused.status(), "deleting a bound secret is refused").toBe(409);
  });
});
