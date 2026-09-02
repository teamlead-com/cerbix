import { flushPromises, mount } from "@vue/test-utils";
import { describe, expect, it, vi } from "vitest";

import NewMonitorView from "@/views/NewMonitorView.vue";

// FR-028 stage 2 in the editor, against the real component rather than the helper library.
// The library's rules are unit-tested in `lib/scenarioBindings.spec.ts`; what these cases pin
// is that the FORM reaches them — the panel exists, the credential-bearing header stops being
// a free-text box, the D10 notice appears at the button, and the body that leaves the page
// carries the flat reference key and the placeholder and never a value.
//
// The three properties the owner approved the mock for are the three describes below.

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), PUT: vi.fn(), PATCH: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));
vi.mock("vue-router", () => ({
  useRoute: () => ({ query: {}, params: {} }),
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
  RouterLink: { props: ["to"], template: "<a><slot /></a>" },
}));
vi.mock("@/components/AppShell.vue", () => ({
  default: { name: "AppShell", template: "<div><slot name='actions' /><slot /></div>" },
}));
vi.mock("@/stores/session", () => ({
  useSession: () => ({ canProjectWrite: () => true, isOrgAdmin: () => true, isGlobalAdmin: false }),
}));
vi.mock("@/stores/workspace", () => ({
  useWorkspace: () => ({
    init: () => Promise.resolve(),
    orgId: "o1",
    projectId: "p1",
    orgName: "Acme",
    projectName: "API",
    projects: [{ id: "p1", name: "API", slug: "api" }],
  }),
}));
vi.mock("@/stores/branding", () => ({ useBranding: () => ({ load: () => Promise.resolve() }) }));

async function mountSyntheticForm() {
  for (const fn of Object.values(apiMock)) fn.mockReset();
  apiMock.GET.mockResolvedValue({ data: [{ id: "s1", name: "checkout-api-token" }] });
  apiMock.POST.mockResolvedValue({ data: { id: "m1" } });
  const w = mount(NewMonitorView, { global: { stubs: { RouterLink: { template: "<a><slot /></a>" } } } });
  await flushPromises();
  const card = w.findAll("button").find((b) => b.text().includes("Synthetic"));
  expect(card, "the form offers a Synthetic type card").toBeTruthy();
  await card!.trigger("click");
  await flushPromises();
  return w;
}

async function declareBinding(w: Awaited<ReturnType<typeof mountSyntheticForm>>, name = "login") {
  await w.find('[data-testid="new-binding"]').trigger("click");
  await w.find('[data-testid="new-binding-name"]').setValue(name);
  await w.find('[data-testid="new-binding-secret"]').setValue("checkout-api-token");
  await w.find('[data-testid="add-binding"]').trigger("click");
  await flushPromises();
}

describe("the binding → inventory mapping is on screen", () => {
  it("puts the panel on the synthetic form and nowhere else", async () => {
    const w = await mountSyntheticForm();
    expect(w.find('[data-testid="scenario-secrets"]').exists()).toBe(true);

    const http = w.findAll("button").find((b) => b.text().includes("HTTP"));
    await http!.trigger("click");
    await flushPromises();
    // The key is refused by the server on any other type (D6b), so the panel must not exist
    // where it could be filled in.
    expect(w.find('[data-testid="scenario-secrets"]').exists()).toBe(false);
  });

  it("never shows a binding name without the secret that fills it", async () => {
    const w = await mountSyntheticForm();
    await declareBinding(w);
    const row = w.find('[data-testid="scenario-binding"]');
    expect(row.exists()).toBe(true);
    expect(row.text()).toContain("login");
    expect((row.find('[data-testid="scenario-binding-secret"]').element as HTMLSelectElement).value)
      .toBe("checkout-api-token");
    // The flat key is shown, because it is what a bundle author and an API caller need.
    expect(row.text()).toContain("scenario_secret_login_ref");
    // Declared and never used is refused by the server; the row says so before a save.
    expect(row.text()).toContain("never used");
  });
});

describe("D7 arrives at the header field", () => {
  it("refuses a literal in a credential-bearing header, and offers the binding instead", async () => {
    const w = await mountSyntheticForm();
    // The default scaffold's second step carries `Authorization: Bearer {{token}}` — a literal
    // in a credential-bearing header, which is exactly what D7 refuses.
    expect(w.text()).toContain("must be exactly");

    await declareBinding(w);
    const picker = w.find('[data-testid="header-binding"]');
    expect(picker.exists(), "the value control becomes a binding picker").toBe(true);
    await picker.setValue("login");
    await flushPromises();
    expect(w.text()).not.toContain("must be exactly");
    expect(w.find('[data-testid="scenario-binding"]').text()).not.toContain("never used");
  });

  it("says what is NOT protected instead of implying a guarantee", async () => {
    const w = await mountSyntheticForm();
    const headerKeys = w.findAll('input[placeholder="Header"]');
    const headerValues = w.findAll('input[placeholder="value (supports {{var}})"]');
    expect(headerKeys.length, "the scaffold has a header row to type into").toBeGreaterThan(0);
    await headerKeys[0].setValue("x-tenant-secret");
    await headerValues[0].setValue("EXAMPLE-thirty-two-characters-long");
    await flushPromises();
    // A hint, not a refusal: cerbix cannot tell a credential from data here.
    expect(w.text()).toContain("nothing refuses it");
  });
});

describe("save before test, and the body that leaves the page", () => {
  it("blocks the test with the reason while a binding is declared", async () => {
    const w = await mountSyntheticForm();
    expect(w.find('[data-testid="test-blocked"]').exists(), "no bindings: the journey is testable").toBe(false);
    expect(w.find('[data-testid="test-connection"]').attributes("disabled")).toBeUndefined();

    await declareBinding(w);
    expect(w.find('[data-testid="test-blocked"]').text()).toContain("Save the monitor before testing it");
    expect(w.find('[data-testid="test-connection"]').attributes("disabled")).toBeDefined();
  });

  it("sends the flat reference key and a document carrying only the placeholder", async () => {
    const w = await mountSyntheticForm();
    await declareBinding(w);
    const picker = w.find('[data-testid="header-binding"]');
    await picker.setValue("login");
    await flushPromises();

    const name = w.findAll("input").find((i) => (i.element as HTMLInputElement).placeholder?.includes("checkout"))
      ?? w.findAll("input[type=text]")[0];
    await name.setValue("checkout journey");
    await w.find("form").trigger("submit");
    await flushPromises();

    expect(apiMock.POST).toHaveBeenCalled();
    const body = (apiMock.POST.mock.calls.at(-1)?.[1] as { body: Record<string, any> }).body;
    expect(body.config.scenario_secret_login_ref).toBe("checkout-api-token");
    expect(body.config.scenario).toContain("{{secret:login}}");
    // The document carries the placeholder and nothing that could be a credential: the literal
    // the scaffold started with is gone, and no key beside the reference holds a value. The
    // reference itself is a NAME, and a name is not a secret — that is the whole design.
    expect(body.config.scenario).not.toContain("Bearer");
    expect(Object.keys(body.config)).toEqual(["scenario", "scenario_secret_login_ref"]);
  });
});
