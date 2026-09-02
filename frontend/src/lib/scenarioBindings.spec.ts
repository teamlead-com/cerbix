import { describe, expect, it } from "vitest";

import {
  applyBindings,
  bindingFromRefKey,
  bindingRefKey,
  bindingsFromConfig,
  firstBindingIssue,
  isSecretCapableHeader,
  malformedRefKeys,
  testBeforeSaveBlockedReason,
  validateScenarioBindings,
  type BindingStep,
} from "./scenarioBindings";

const step = (over: Partial<BindingStep> = {}): BindingStep => ({ url: "https://api.internal/x", headers: [], body: "", ...over });

describe("the binding's wire shape", () => {
  it("round-trips a reference key", () => {
    expect(bindingRefKey("login")).toBe("scenario_secret_login_ref");
    expect(bindingFromRefKey("scenario_secret_login_ref")).toBe("login");
  });

  // The server refuses a key that looks like a reference and does not parse, because a typo
  // silently ignored is a binding the operator believes they declared.
  it("refuses a key that does not parse, and does not silently drop it", () => {
    expect(bindingFromRefKey("scenario_secret_Login_ref")).toBeNull();
    expect(malformedRefKeys({ scenario_secret_Login_ref: "x", scenario_secret_ok_ref: "y" })).toEqual([
      "scenario_secret_Login_ref",
    ]);
  });

  it("reads bindings out of a stored config, sorted", () => {
    expect(
      bindingsFromConfig({
        scenario: "{}",
        scenario_secret_upload_ref: "files-key",
        scenario_secret_login_ref: "api-token",
      }),
    ).toEqual([
      { name: "login", secret: "api-token" },
      { name: "upload", secret: "files-key" },
    ]);
  });

  // Removing a binding in the editor must actually remove it: the store clears the matching
  // monitor_secret_refs row on the same write, and a key left behind would keep the secret
  // undeletable for a monitor that no longer uses it.
  it("drops every stale reference key before writing the current set", () => {
    expect(
      applyBindings(
        { scenario: "{}", scenario_secret_gone_ref: "old-secret" },
        [{ name: "login", secret: "api-token" }],
      ),
    ).toEqual({ scenario: "{}", scenario_secret_login_ref: "api-token" });
  });

  it("keeps the credential-bearing header set the server has", () => {
    expect(isSecretCapableHeader("Authorization")).toBe(true);
    expect(isSecretCapableHeader("  X-API-Key ")).toBe(true);
    expect(isSecretCapableHeader("x-tenant-secret")).toBe(false);
  });
});

describe("the rules, at the field", () => {
  const secrets = ["api-token", "files-key"];

  it("accepts a header that is exactly the placeholder and a body that uses it too", () => {
    const issues = validateScenarioBindings(
      [
        step({ headers: [{ k: "authorization", v: "{{secret:login}}" }] }),
        step({ body: '{"t":"{{secret:login}}"}' }),
      ],
      [{ name: "login", secret: "api-token" }],
      secrets,
      true,
    );
    expect(firstBindingIssue(issues)).toBe("");
  });

  it("refuses a literal in a credential-bearing header, at that header", () => {
    const issues = validateScenarioBindings(
      [step({ headers: [{ k: "Authorization", v: "Bearer abc.def" }] })],
      [],
      secrets,
      true,
    );
    expect(issues.headerErrors["0:0"]).toContain("must be exactly {{secret:<binding>}}");
    // And it never echoes what was typed.
    expect(JSON.stringify(issues)).not.toContain("abc.def");
  });

  it("refuses a placeholder in a URL, at that URL", () => {
    const issues = validateScenarioBindings(
      [step({ url: "https://api.internal/act?token={{secret:login}}", headers: [] })],
      [{ name: "login", secret: "api-token" }],
      secrets,
      true,
    );
    expect(issues.urlErrors[0]).toContain("must not reference a secret in its URL");
  });

  it("refuses the same header twice, case-insensitively", () => {
    const issues = validateScenarioBindings(
      [step({ headers: [{ k: "Authorization", v: "{{secret:login}}" }, { k: "authorization", v: "{{secret:login}}" }] })],
      [{ name: "login", secret: "api-token" }],
      secrets,
      true,
    );
    expect(issues.headerErrors["0:1"]).toContain("twice");
  });

  it("refuses a declared binding nobody uses", () => {
    const issues = validateScenarioBindings([step()], [{ name: "login", secret: "api-token" }], secrets, true);
    expect(issues.bindingErrors.login).toContain("declared and never used");
  });

  it("refuses a placeholder no binding declares", () => {
    const issues = validateScenarioBindings(
      [step({ headers: [{ k: "authorization", v: "{{secret:ghost}}" }] })],
      [],
      secrets,
      true,
    );
    expect(issues.headerErrors["0:0"]).toContain('binding "ghost"');
  });

  it("names a binding whose secret is gone, and stays quiet until the inventory has loaded", () => {
    const args = [
      [step({ headers: [{ k: "authorization", v: "{{secret:login}}" }] })],
      [{ name: "login", secret: "deleted-secret" }],
      secrets,
    ] as const;
    expect(validateScenarioBindings(...args, true).bindingErrors.login).toContain("no longer exists");
    expect(validateScenarioBindings(...args, false).bindingErrors.login).toBeUndefined();
  });

  // The residual is a HINT and never a refusal: a rule that guesses at values would refuse
  // legitimate data and still miss the next credential (D7).
  it("hints at a pasted-looking value in an ordinary header without refusing it", () => {
    const issues = validateScenarioBindings(
      [step({ headers: [{ k: "x-tenant-secret", v: "EXAMPLE-thirty-two-characters-long" }] })],
      [],
      secrets,
      true,
    );
    expect(issues.residualHints["0:0"]).toContain("nothing refuses it");
    expect(issues.headerErrors["0:0"]).toBeUndefined();
    expect(firstBindingIssue(issues)).toBe("");
  });

  it("does not hint at an interpolated variable or a short value", () => {
    const issues = validateScenarioBindings(
      [step({ headers: [{ k: "x-run", v: "{{token}}-and-more-than-twenty-chars" }, { k: "x-mode", v: "fast" }] })],
      [],
      secrets,
      true,
    );
    expect(issues.residualHints).toEqual({});
  });
});

describe("save before test", () => {
  it("blocks with the reason when a binding is declared", () => {
    expect(testBeforeSaveBlockedReason([{ name: "login", secret: "api-token" }])).toContain(
      "Save the monitor before testing it",
    );
    expect(testBeforeSaveBlockedReason([{ name: "login", secret: "a" }, { name: "upload", secret: "b" }])).toContain(
      "bindings login, upload are resolved",
    );
  });

  it("allows a credential-free scenario to be tested", () => {
    expect(testBeforeSaveBlockedReason([])).toBe("");
  });
});
