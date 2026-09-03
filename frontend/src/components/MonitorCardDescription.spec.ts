import { mount } from "@vue/test-utils";
import { describe, expect, it } from "vitest";

import MonitorCard from "@/components/MonitorCard.vue";

// FR-030 on the dashboard panel, and the GEOMETRY the reviewer's P1 [122] was right to challenge.
// What is measured here is the structure that decides the card's height: how many flex children the
// card has, and whether the description can ever become a second line.

const monitor = (over: Record<string, unknown> = {}) =>
  ({
    id: "m1",
    project_id: "p1",
    name: "payments-callback",
    slug: "payments-callback",
    type: "http",
    status: "up",
    description: "",
    ...over,
  }) as any;

const stubs = { RouterLink: { props: ["to"], template: "<a><slot /></a>" }, UptimeBar: true, Sparkline: true, StatusPill: true };
const card = (props: Record<string, unknown>) => mount(MonitorCard, { props: props as any, global: { stubs } });
// The card is `flex flex-col`, so its height is the sum of its DIRECT children plus the gaps.
const flexChildren = (w: ReturnType<typeof card>) => w.element.children.length;

describe("monitor card description", () => {
  it("renders one line, never two, and carries the full text as its tooltip", () => {
    const long = "Confirms the payment provider can reach our callback URL; a DOWN here means paid orders stay pending.";
    const w = card({ monitor: monitor({ description: long }) });
    const el = w.find('[data-testid="monitor-description"]');
    expect(el.exists()).toBe(true);
    expect(el.text()).toBe(long);
    expect(el.attributes("title"), "the whole sentence is reachable without leaving the dashboard").toBe(long);
    // `truncate` is what keeps a long description from wrapping into a second line. Without it the
    // card's height would depend on the LENGTH of the text, not merely on its presence.
    expect(el.classes()).toContain("truncate");
    expect(w.findAll('[data-testid="monitor-description"]').length).toBe(1);
  });

  it("adds no element at all when there is no description", () => {
    const w = card({ monitor: monitor() });
    expect(w.find('[data-testid="monitor-description"]').exists()).toBe(false);
  });

  // The honest statement of the height contract, asserted rather than promised in prose: a described
  // card has exactly ONE more flex child than an undescribed one, so it is one line taller — and the
  // card's height was ALREADY content-dependent before this field existed, which is the fact the
  // spec's "the panel keeps its height" sentence ignored.
  it("is one flex child taller, and the card's height was already content-dependent", () => {
    const bare = card({ monitor: monitor() });
    const described = card({ monitor: monitor({ description: "What it is for." }) });
    expect(described.element.children.length).toBe(flexChildren(bare) + 1);

    // Pre-existing variation, same component, no description involved: a push monitor has no latency
    // column and no error-budget meter, so it is already shorter than an http monitor beside it.
    const push = card({ monitor: monitor({ type: "push" }), budgetLeft: 40 });
    const http = card({ monitor: monitor({ type: "http" }), budgetLeft: 40 });
    expect(flexChildren(http)).toBeGreaterThan(flexChildren(push));
  });
});
