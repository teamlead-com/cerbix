import { test, expect } from "@playwright/test";
import { apiGet, apiSend, ensureE2EWorkspace, cleanupMonitors } from "./helpers";

// FR-029 phase F against a live stack. The library's rules are unit tested and the form is tested
// against the real component; what only a live stack can prove is the two things that matter here:
//
//   1. a canary declared entirely through the TYPED FORM is accepted by the server — the client's
//      idea of a valid document and the server's are the same idea, on the real write path;
//   2. it round-trips: reopening the saved monitor rebuilds the same typed form, with no JSON
//      editor anywhere, which is the contract the owner approved the mock for.
//
// Everything is prefixed `e2e-` and cleaned up, per the suite's contract with dev stacks.
test.describe("async canary form", () => {
  const SECRET = "e2e-canary-token";

  test.afterEach(async ({ page }) => {
    const { projectID } = await ensureE2EWorkspace(page);
    await cleanupMonitors(page, projectID);
    await apiSend(page, "delete", `/api/v1/projects/${projectID}/secrets/${SECRET}`);
  });

  test("a canary is declared through the typed form, accepted, and reads back", async ({ page }) => {
    await page.goto("/");
    const { projectID } = await ensureE2EWorkspace(page);

    await apiSend(page, "delete", `/api/v1/projects/${projectID}/secrets/${SECRET}`);
    const made = await apiSend(page, "post", `/api/v1/projects/${projectID}/secrets`, {
      name: SECRET,
      value: "e2e-canary-secret-value",
    });
    expect(made.status(), "the secret the binding will point at").toBe(201);

    await page.goto(`/monitors/new?project=${projectID}`);
    await page.locator("button", { hasText: "Async canary" }).click();
    await expect(page.getByTestId("canary-workflow")).toBeVisible();

    // The contract the mock was approved for, asserted on the real page: no JSON editor anywhere.
    await expect(page.locator("textarea")).toHaveCount(0);

    await page.getByPlaceholder("payments-callback").fill("e2e-canary-form");
    // Several workflow bounds are expressed against the monitor's own timeout, and a canary's
    // interval may not be shorter than it.
    await page.getByTestId("monitor-timeout").fill("300");
    await page.getByTestId("monitor-interval").fill("300");

    // ── the binding, declared once ───────────────────────────────────────────────────────
    await page.getByTestId("canary-add-binding").click();
    await page.getByLabel("binding name 0").fill("upload");
    await page.getByLabel("project secret 0").selectOption(SECRET);

    // ── submit ───────────────────────────────────────────────────────────────────────────
    await page.getByTestId("canary-submit-url").fill("https://files.example.invalid/files/upload");
    await page.getByTestId("canary-submit-timeout").fill("30");
    await page.getByTestId("canary-accepted-status").fill("202");
    await page.getByTestId("canary-add-body-field").click();
    await page.getByLabel("field key 0").fill("tenant");
    await page.getByLabel("field value 0").fill("canary");

    // D7 taught by the control: naming a credential-bearing header replaces the value box with the
    // binding picker, and the refusal for a literal is never reachable from here.
    await page.getByTestId("canary-add-submit-header").click();
    await page.getByLabel("submit header name 0").fill("authorization");
    await expect(page.getByTestId("canary-credential-header")).toBeVisible();
    await expect(page.getByLabel("submit header value 0")).toHaveCount(0);
    await page.getByLabel("submit header binding 0").selectOption("upload");

    // ── the placeholder is legal in exactly one field ────────────────────────────────────
    await page.getByTestId("canary-submit-url").fill("https://files.example.invalid/files/{{ correlation_id }}/upload");
    await expect(page.getByTestId("canary-refusal-submitURL")).toContainText("only legal in the completion URL");
    await page.getByTestId("canary-submit-url").fill("https://files.example.invalid/files/upload");
    await expect(page.getByTestId("canary-refusal-submitURL")).toHaveCount(0);

    // ── correlate, completion, result, cleanup ───────────────────────────────────────────
    await page.getByTestId("canary-correlate-path").fill("task_id");
    await page.getByTestId("canary-completion-url").fill("https://files.example.invalid/tasks/{{ correlation_id }}");
    await page.getByTestId("canary-completion-timeout").fill("240");
    await page.getByTestId("canary-poll-interval").fill("5");
    await page.getByTestId("canary-poll-attempts").fill("48");
    await page.getByTestId("canary-max-latency").fill("240");
    await page.getByTestId("canary-required-fields").fill("s3_path, byte_size");
    await page.getByTestId("canary-lifecycle-path").fill("s3_path");
    await page.getByTestId("canary-cleanup-prefix").fill("canary/");

    await page.locator("button", { hasText: "Create monitor" }).click();

    // The server ACCEPTED what the form built. This is the assertion the whole spec exists for:
    // two implementations of one contract agreeing on the real write path.
    let monitorID = "";
    await expect
      .poll(
        async () => {
          const list = (await apiGet(page, `/api/v1/projects/${projectID}/monitors`)) as any[];
          const found = list.find((m) => m.name === "e2e-canary-form");
          monitorID = found?.id ?? "";
          return found?.type ?? "";
        },
        { timeout: 20_000, message: "the canary the form built was never created" },
      )
      .toBe("async_canary");

    // The wire shape: the document plus the flat ref key, and no project-secret name in the document.
    const saved = (await apiGet(page, `/api/v1/monitors/${monitorID}`)) as any;
    expect(saved.config.canary_secret_upload_ref).toBe(SECRET);
    expect(saved.config.workflow, "the document is stored").toBeTruthy();
    expect(saved.config.workflow).not.toContain(SECRET);
    expect(saved.config.workflow).toContain('"secret_ref":"upload"');
    // No value of the secret anywhere in the read model.
    expect(JSON.stringify(saved)).not.toContain("e2e-canary-secret-value");

    // ── it reads back into the same typed form ───────────────────────────────────────────
    await page.goto(`/monitors/${monitorID}/edit`);
    await expect(page.getByTestId("canary-workflow")).toBeVisible();
    await expect(page.locator("textarea")).toHaveCount(0);
    await expect(page.getByTestId("canary-submit-url")).toHaveValue("https://files.example.invalid/files/upload");
    await expect(page.getByTestId("canary-completion-url")).toHaveValue(
      "https://files.example.invalid/tasks/{{ correlation_id }}",
    );
    await expect(page.getByTestId("canary-lifecycle-path")).toHaveValue("s3_path");
    // The binding halves are recombined from the document's marker and the flat key's name, which is
    // why the read view is complete without a JSON editor.
    await expect(page.getByLabel("binding name 0")).toHaveValue("upload");
    await expect(page.getByLabel("project secret 0")).toHaveValue(SECRET);
  });
});
