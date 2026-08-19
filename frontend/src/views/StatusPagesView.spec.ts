import { flushPromises, mount } from "@vue/test-utils";
import { beforeEach, describe, expect, it, vi } from "vitest";

import StatusPagesView from "@/views/StatusPagesView.vue";

// FR-021 phase 4 (§15.0): the conversion gate in the UI. Three properties are load-bearing and
// none of them is visible from the store's tests:
//
//  1. the row shows the ACTIVE source and labels the dormant binding AS dormant — after a
//     conversion both are stored, and a UI that shows them side by side unlabelled makes the
//     operator read the wrong fact;
//  2. "Apply" is unavailable until a preview exists, because the two CAS tokens come from the
//     preview and a confirmation without them is an unpreviewed conversion;
//  3. a stale-page 409 DISCARDS the preview instead of leaving the same tokens on screen to be
//     retried into the identical failure.

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), PATCH: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));
vi.mock("@/components/AppShell.vue", () => ({
  default: { name: "AppShell", template: "<div><slot name='actions' /><slot /></div>" },
}));
vi.mock("@/stores/session", () => ({ useSession: () => ({ isOrgAdmin: () => true }) }));
vi.mock("@/stores/workspace", () => ({
  useWorkspace: () => ({
    init: () => Promise.resolve(),
    orgId: "o1",
    projectId: "p1",
    orgName: "Acme",
    projectName: "Payments",
  }),
}));

const RouterLink = { props: ["to"], template: "<a><slot /></a>" };

const page = { id: "sp1", org_id: "o1", slug: "acme-status", title: "Acme", visibility: "public" };
const serviceComponent = {
  id: "c1",
  status_page_id: "sp1",
  org_id: "o1",
  name: "Checkout",
  source: "service",
  source_project: "p1",
  service_id: "sv1",
  // The monitor binding it was converted AWAY from — dormant, and still stored.
  monitor_id: "mon1",
  revision: 4,
};

function mockLoads(component = serviceComponent) {
  apiMock.GET.mockImplementation((path: string) => {
    if (path.includes("status-pages") && path.includes("components")) return Promise.resolve({ data: [component] });
    if (path.includes("organizations")) return Promise.resolve({ data: [page] });
    if (path.includes("monitors")) return Promise.resolve({ data: [{ id: "mon1", name: "checkout-http" }] });
    // The REAL shape of this endpoint: ServiceSummary WRAPS the service with its rollup counts. The
    // fixture used to answer the flat shape, which is exactly why no test caught the blank dropdown —
    // the fake had the same wrong assumption as the code it was checking.
    if (path.includes("services")) {
      return Promise.resolve({
        data: [{
          service: { id: "sv1", slug: "checkout", name: "Checkout service" },
          revision: 1, context_members: [], sli_members: [], epoch_seq: 1, repairing_count: 0,
        }],
      });
    }
    if (path.includes("subscribers")) return Promise.resolve({ data: [] });
    return Promise.resolve({ data: [] });
  });
}

async function mountAndSelect() {
  const wrapper = mount(StatusPagesView, { global: { stubs: { RouterLink } } });
  await flushPromises();
  // Selecting the page loads its components.
  const pageButton = wrapper.findAll("button").find((b) => b.text().includes("Acme"));
  if (pageButton) {
    await pageButton.trigger("click");
    await flushPromises();
  }
  return wrapper;
}

beforeEach(() => {
  apiMock.GET.mockReset();
  apiMock.POST.mockReset();
  apiMock.PATCH.mockReset();
  apiMock.DELETE.mockReset();
});

describe("StatusPagesView conversion", () => {
  it("shows the active source and labels the dormant binding as kept for revert", async () => {
    mockLoads();
    const wrapper = await mountAndSelect();
    const row = wrapper.find('[data-testid="component-row"]');
    expect(row.exists()).toBe(true);
    expect(wrapper.find('[data-testid="component-source"]').text()).toBe("service");
    // The ACTIVE binding is the service, even though a monitor id is present.
    expect(wrapper.find('[data-testid="component-binding"]').text()).toContain("Checkout service");
    const dormant = wrapper.find('[data-testid="component-dormant"]');
    expect(dormant.exists()).toBe(true);
    expect(dormant.text()).toContain("kept for revert");
    expect(dormant.text()).toContain("checkout-http");
  });

  it("offers no way to confirm before a preview exists", async () => {
    mockLoads();
    const wrapper = await mountAndSelect();
    await wrapper.find('[data-testid="convert-component"]').trigger("click");
    await flushPromises();
    expect(wrapper.find('[data-testid="conversion-dialog"]').exists()).toBe(true);
    // No preview yet, so there is nothing to apply — the confirm control is not even rendered.
    expect(wrapper.find('[data-testid="conversion-confirm"]').exists()).toBe(false);
    expect(apiMock.POST).not.toHaveBeenCalled();
  });

  it("previews before and after, then confirms with the tokens the preview issued", async () => {
    mockLoads();
    apiMock.POST.mockImplementation((path: string, opts: { body: Record<string, unknown> }) => {
      if (path.endsWith("/conversion/preview")) {
        return Promise.resolve({
          data: {
            component: { status: "operational", source: "service" },
            proposed: { status: "no_data", source: "manual", reason: "no_manual_status" },
            summary: { summary: "operational", summary_state: "operational", unmeasured_count: 0 },
            proposed_summary: { summary: "no_data", summary_state: "no_data", unmeasured_count: 1 },
            revision: 4,
            page_generation: 9,
            no_op: false,
            reverts_to: "service",
            notes: ["After this change the component publishes \"no data\" until its first measurement."],
          },
        });
      }
      // The confirmation must carry EXACTLY the pair the preview returned.
      expect(opts.body.revision).toBe(4);
      expect(opts.body.page_generation).toBe(9);
      return Promise.resolve({ data: { ...serviceComponent, source: "manual", revision: 5 } });
    });

    const wrapper = await mountAndSelect();
    await wrapper.find('[data-testid="convert-component"]').trigger("click");
    await wrapper.find('[data-testid="conversion-source"]').setValue("manual");
    await wrapper.find('[data-testid="conversion-preview"]').trigger("click");
    await flushPromises();

    const result = wrapper.find('[data-testid="conversion-result"]');
    expect(result.exists()).toBe(true);
    // Both sides are shown, and the proposed side states the unknown as an unknown.
    expect(wrapper.find('[data-testid="proposed-status"]').text()).toBe("No data");
    expect(wrapper.find('[data-testid="summary-before"]').text()).toContain("All systems operational");
    expect(wrapper.find('[data-testid="summary-after"]').text()).toContain("No measurements available");
    expect(result.text()).toContain("no data");

    await wrapper.find('[data-testid="conversion-confirm"]').trigger("click");
    await flushPromises();
    // The dialog closes and the row now reports the new source.
    expect(wrapper.find('[data-testid="conversion-dialog"]').exists()).toBe(false);
    expect(wrapper.find('[data-testid="component-source"]').text()).toBe("manual");
  });

  it("discards a stale preview and says what to do, rather than keeping dead tokens", async () => {
    mockLoads();
    apiMock.POST.mockImplementation((path: string) => {
      if (path.endsWith("/conversion/preview")) {
        return Promise.resolve({
          data: {
            component: { status: "operational", source: "service" },
            proposed: { status: "operational", source: "monitor" },
            summary: { summary: "operational", summary_state: "operational", unmeasured_count: 0 },
            proposed_summary: { summary: "operational", summary_state: "operational", unmeasured_count: 0 },
            revision: 4,
            page_generation: 9,
            no_op: false,
            notes: [],
          },
        });
      }
      return Promise.resolve({ error: { error: "page_configuration_stale: page generation 11, expected 9" } });
    });

    const wrapper = await mountAndSelect();
    await wrapper.find('[data-testid="convert-component"]').trigger("click");
    await wrapper.find('[data-testid="conversion-source"]').setValue("monitor");
    await wrapper.find('[data-testid="conversion-preview"]').trigger("click");
    await flushPromises();
    await wrapper.find('[data-testid="conversion-confirm"]').trigger("click");
    await flushPromises();

    const err = wrapper.find('[data-testid="conversion-error"]');
    expect(err.exists()).toBe(true);
    expect(err.text()).toContain("Preview again");
    // The dead preview is gone, so the same tokens cannot be re-submitted.
    expect(wrapper.find('[data-testid="conversion-result"]').exists()).toBe(false);
    expect(wrapper.find('[data-testid="conversion-confirm"]').exists()).toBe(false);
  });

  // An operator's screenshot showed the SERVICE dropdown rendering a blank option. The label was the
  // visible half; the invisible half was worse — the option carried no value either, so a service
  // component could not be created at all. Both halves come from one cause: the list endpoint answers
  // ServiceSummary (the service wrapped with rollup counts) and the view read `sv.name`/`sv.id`
  // directly, behind an `as Service[]` cast that stopped the compiler from saying so.
  it("names each service in the source dropdown, and carries its id as the option value", async () => {
    mockLoads();
    const wrapper = await mountAndSelect();
    // The service picker only exists once the ADD form's source is Service — the same two clicks an
    // operator makes, and the state the screenshot was taken in.
    await wrapper.get('[data-testid="new-component-source"]').setValue("service");

    const picker = wrapper.get('[data-testid="new-component-service"]');
    const opts = picker.findAll("option").filter((o) => o.attributes("value") === "sv1");
    expect(opts, "no option carries the service id — the dropdown cannot bind a service").toHaveLength(1);
    expect(opts[0].text()).toBe("Checkout service");
  });

  it("does not offer no_data as a manual status an operator can state", async () => {
    mockLoads();
    const wrapper = await mountAndSelect();
    await wrapper.find('[data-testid="new-component-source"]').setValue("manual");
    await flushPromises();
    const values = wrapper.findAll("option").map((o) => o.attributes("value"));
    expect(values).not.toContain("no_data");
  });
});
