import { mount } from "@vue/test-utils";
import { describe, expect, it } from "vitest";

import OnboardingGuide from "@/components/OnboardingGuide.vue";
import { resolveOnboarding } from "@/lib/onboarding";

const RouterLink = { props: ["to"], template: '<a data-testid="router-link"><slot /></a>' };
const monitor = { id: "m1", name: "Checkout API", type: "http" as const, region: "core", created_at: "2026-09-19T09:00:00Z" };

function mountGuide(snapshot = resolveOnboarding({
  orgCount: 1,
  orgId: "o1",
  projectId: "p1",
  canCreateOrg: true,
  canCreateProject: true,
  canCreateMonitor: true,
  evidence: [{ monitor, heartbeat: null }],
  now: Date.parse("2026-09-19T09:00:10Z"),
})) {
  return mount(OnboardingGuide, {
    props: { snapshot, monitors: [monitor], orgName: "Acme", projectName: "Payments" },
    global: { stubs: { RouterLink } },
  });
}

describe("OnboardingGuide", () => {
  it("renders factual waiting copy without claiming health", () => {
    const wrapper = mountGuide();
    expect(wrapper.text()).toContain("Waiting for its first real result");
    expect(wrapper.text()).toContain("Creation alone does not prove");
    expect(wrapper.text()).toContain("not healthy yet");
  });

  it("preserves the server failure message for a DOWN first result", () => {
    const snapshot = resolveOnboarding({
      orgCount: 1,
      orgId: "o1",
      projectId: "p1",
      canCreateOrg: true,
      canCreateProject: true,
      canCreateMonitor: true,
      evidence: [{ monitor, heartbeat: { monitor_id: "m1", ts: "2026-09-19T09:01:00Z", up: false, code: 503, msg: "expected status 200..299, got 503" } }],
      journeyActive: true,
    });
    const wrapper = mountGuide(snapshot);
    expect(wrapper.text()).toContain("returned DOWN");
    expect(wrapper.text()).toContain("expected status 200..299, got 503");
  });

  it("uses the compact approved panel for an existing installation", () => {
    const snapshot = resolveOnboarding({
      orgCount: 1,
      orgId: "o1",
      projectId: "p1",
      canCreateOrg: true,
      canCreateProject: true,
      canCreateMonitor: true,
      evidence: [{ monitor, heartbeat: { monitor_id: "m1", ts: "2026-09-19T09:01:00Z", up: true } }],
    });
    const wrapper = mountGuide(snapshot);
    expect(wrapper.text()).toContain("setup already complete");
    expect(wrapper.text()).toContain("Collapse guide");
    expect(wrapper.text()).not.toContain("Skip for now");
  });

  it("marks the current step semantically and closes from Escape", async () => {
    const wrapper = mountGuide();
    expect(wrapper.get('[aria-current="step"]').text()).toContain("First result");
    expect(wrapper.findAll('[aria-live="polite"]')).toHaveLength(1);

    await wrapper.get('[data-testid="onboarding-guide"]').trigger("keydown", { key: "Escape" });
    expect(wrapper.emitted("close")).toHaveLength(1);
  });
});
