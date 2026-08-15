import { describe, expect, it } from "vitest";

import { describeSecretError, monitorCount } from "./secretErrors";

describe("describeSecretError", () => {
  it("explains the quota rather than echoing its code", () => {
    // The gap this file exists to close: the server returns secret_quota and the panel
    // used to render exactly that string.
    const msg = describeSecretError("add", { error: "secret_quota" }, "app-db");
    expect(msg).not.toBe("secret_quota");
    expect(msg).toContain("Delete a secret");
  });

  it("explains a secret that vanished under the operator", () => {
    const msg = describeSecretError("update", { error: "not found" }, "app-db");
    expect(msg).not.toBe("not found");
    expect(msg).toContain("app-db");
  });

  it("names the consumers on the in-use guards, singular and plural", () => {
    expect(describeSecretError("delete", { error: "secret_in_use", count: 1 }, "app-db")).toContain("1 monitor ");
    expect(describeSecretError("delete", { error: "secret_in_use", count: 3 }, "app-db")).toContain("3 monitors");
    expect(describeSecretError("update", { error: "secret_renamed_in_use", count: 2 }, "app-db")).toContain("file source");
  });

  it("uses the NEW name when a rename collides", () => {
    expect(describeSecretError("update", { error: "secret_exists" }, "old", "taken")).toContain('"taken"');
  });

  it("falls back per action when the code is unknown or absent", () => {
    expect(describeSecretError("add", undefined, "app-db")).toBe("Could not add the secret.");
    expect(describeSecretError("delete", { error: "" }, "app-db")).toBe("Could not delete the secret.");
    // An unrecognised code is still shown: hiding it would leave nothing to report.
    expect(describeSecretError("add", { error: "teapot" }, "app-db")).toBe("teapot");
  });
});

describe("monitorCount", () => {
  it("is plural-aware", () => {
    expect(monitorCount(0)).toBe("0 monitors");
    expect(monitorCount(1)).toBe("1 monitor");
    expect(monitorCount(2)).toBe("2 monitors");
  });
});
