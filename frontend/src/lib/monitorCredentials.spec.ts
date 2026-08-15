import { describe, expect, it } from "vitest";

import { applyCredentialSelection, isDanglingSecretRef } from "./monitorCredentials";

describe("monitor credential wire shape", () => {
  it("emits a reference without retaining a plaintext value", () => {
    expect(
      applyCredentialSelection(
        { username: "cerbix", password: "stale-plaintext" },
        { mode: "ref", ref: "database-password" },
      ),
    ).toEqual({ username: "cerbix", password_ref: "database-password" });
  });

  it("emits a value without retaining a previous reference", () => {
    expect(
      applyCredentialSelection(
        { username: "cerbix", password_ref: "stale-reference" },
        { mode: "value", value: "new-plaintext" },
      ),
    ).toEqual({ username: "cerbix", password: "new-plaintext" });
  });

  it("omits both credential keys for an unchanged inline edit", () => {
    expect(
      applyCredentialSelection(
        { username: "cerbix", password_ref: "stale-reference" },
        { mode: "value", value: "" },
      ),
    ).toEqual({ username: "cerbix" });
  });
});

describe("isDanglingSecretRef", () => {
  const secrets = [{ name: "app-db" }, { name: "cache" }];

  it("flags a reference the project no longer has", () => {
    expect(isDanglingSecretRef("ref", "gone", secrets, true)).toBe(true);
  });

  it("stays quiet for a reference that resolves", () => {
    expect(isDanglingSecretRef("ref", "app-db", secrets, true)).toBe(false);
  });

  it("stays quiet until the list has actually loaded", () => {
    // Otherwise every reference looks dangling for the first frames, and a warning that
    // cries wolf during load is a warning operators learn to ignore.
    expect(isDanglingSecretRef("ref", "app-db", [], false)).toBe(false);
    expect(isDanglingSecretRef("ref", "gone", [], false)).toBe(false);
  });

  it("does not apply when the credential is an inline value", () => {
    expect(isDanglingSecretRef("value", "gone", secrets, true)).toBe(false);
  });

  it("does not flag an empty selection — that is 'nothing chosen', not 'missing'", () => {
    expect(isDanglingSecretRef("ref", "", secrets, true)).toBe(false);
  });
});
