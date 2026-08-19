import { mount } from "@vue/test-utils";
import { describe, expect, it } from "vitest";

import IncidentSubjectChip from "@/components/IncidentSubjectChip.vue";

// FR-022, mock panels 1–2. The chip is the anchor's DISCRIMINATOR made visible, and the case that
// matters most is the one that renders nothing: a manual project-level incident has no subject, and
// a chip that appeared anyway — labelled "monitor" from an empty id, say — would be a claim about
// the incident that the schema explicitly allows to be false.

const RouterLink = { props: ["to"], template: '<a :data-to="to.params.id"><slot /></a>' };
const mountChip = (props: Record<string, unknown>) =>
  mount(IncidentSubjectChip, { props, global: { stubs: { RouterLink } } });

describe("IncidentSubjectChip", () => {
  it("names the SERVICE it is an incident of, and links to it", () => {
    const w = mountChip({ serviceId: "svc1", serviceSlug: "checkout" });
    const chip = w.get('[data-testid="incident-subject"]');
    expect(chip.text()).toContain("service");
    expect(chip.text()).toContain("checkout");
    expect(chip.attributes("data-to")).toBe("svc1");
  });

  it("names the MONITOR for a monitor incident — the same grammar, not a second one", () => {
    const w = mountChip({ monitorId: "mon1", monitorName: "checkout-http" });
    const chip = w.get('[data-testid="incident-subject"]');
    expect(chip.text()).toContain("monitor");
    expect(chip.text()).toContain("checkout-http");
    expect(chip.attributes("data-to")).toBe("mon1");
  });

  it("renders NOTHING for a project-level incident, which is what makes it a discriminator", () => {
    const w = mountChip({});
    expect(w.find('[data-testid="incident-subject"]').exists()).toBe(false);
    expect(w.text()).toBe("");
  });

  it("still states the KIND when the name has not resolved — it never waits on a second request", () => {
    const w = mountChip({ serviceId: "svc1" });
    const chip = w.get('[data-testid="incident-subject"]');
    expect(chip.text()).toContain("service");
    // No stray separator, and no raw UUID pretending to be a name.
    expect(chip.text()).not.toContain("·");
    expect(chip.text()).not.toContain("svc1");
  });
});
