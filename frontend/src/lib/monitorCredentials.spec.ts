import { describe, expect, it } from "vitest";

import { applyCredentialSelection } from "./monitorCredentials";

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
