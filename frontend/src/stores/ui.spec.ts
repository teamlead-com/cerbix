import { createPinia, setActivePinia } from "pinia";
import { beforeEach, describe, expect, it } from "vitest";

import { useUi } from "@/stores/ui";

// FR-041a mock 5a-2: `/i/:ref` sets a one-line notice when it switched workspace; the incident page
// reads it ONCE, so a later visit to the same incident does not repeat it.
describe("ui.incidentNotice", () => {
  beforeEach(() => setActivePinia(createPinia()));

  it("is read once", () => {
    const ui = useUi();
    expect(ui.takeIncidentNotice()).toBe("");
    ui.setIncidentNotice("Switched to Acme / prod to open this incident.");
    expect(ui.incidentNotice).toBe("Switched to Acme / prod to open this incident.");
    expect(ui.takeIncidentNotice()).toBe("Switched to Acme / prod to open this incident.");
    expect(ui.takeIncidentNotice()).toBe("");
    expect(ui.incidentNotice).toBe("");
  });
});
