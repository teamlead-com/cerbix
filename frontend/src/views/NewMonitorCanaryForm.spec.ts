import { flushPromises, mount } from "@vue/test-utils";
import { describe, expect, it, vi } from "vitest";

import NewMonitorView from "@/views/NewMonitorView.vue";

// FR-029 phase F against the REAL component, not the helper library. The library's rules are unit
// tested in `lib/canaryWorkflow.spec.ts`; what these cases pin is that the FORM reaches them — the
// typed sections exist, a credential-bearing header stops being a free-text box, the refusals appear
// at their own fields, and the body that leaves the page is the document plus the flat ref keys with
// no project-secret name inside the document.
//
// And the contract the mock was approved for: there is no JSON editor anywhere.

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

async function mountCanaryForm() {
  for (const fn of Object.values(apiMock)) fn.mockReset();
  apiMock.GET.mockResolvedValue({ data: [{ id: "s1", name: "upload-token" }] });
  apiMock.POST.mockResolvedValue({ data: { id: "m1" } });
  const w = mount(NewMonitorView, { global: { stubs: { RouterLink: { template: "<a><slot /></a>" } } } });
  await flushPromises();
  const card = w.findAll("button").find((b) => b.text().includes("Async canary"));
  expect(card, "the form offers an Async canary type card").toBeTruthy();
  await card!.trigger("click");
  await flushPromises();
  return w;
}

type Wrapper = Awaited<ReturnType<typeof mountCanaryForm>>;

async function set(w: Wrapper, testid: string, value: string) {
  await w.find(`[data-testid="${testid}"]`).setValue(value);
  await flushPromises();
}

/** Fill the form to a state the library considers valid, through the DOM only. */
async function fillValid(w: Wrapper) {
  await w.find('input[placeholder="payments-callback"]').setValue("e2e-canary");
  // Several workflow bounds are expressed against the MONITOR's timeout, and a canary's interval may
  // not be shorter than it — so a form that never sets these cannot be valid, whatever else it says.
  await set(w, "monitor-timeout", "300");
  await set(w, "monitor-interval", "300");
  await set(w, "canary-submit-url", "https://files.example.com/files/upload");
  await set(w, "canary-submit-timeout", "30");
  await set(w, "canary-accepted-status", "202");
  await w.find('[data-testid="canary-add-body-field"]').trigger("click");
  await flushPromises();
  await w.find('[aria-label="field key 0"]').setValue("tenant");
  await w.find('[aria-label="field value 0"]').setValue("canary");
  await set(w, "canary-correlate-path", "task_id");
  await set(w, "canary-completion-url", "https://files.example.com/tasks/{{ correlation_id }}");
  await set(w, "canary-completion-timeout", "240");
  await set(w, "canary-poll-interval", "5");
  await set(w, "canary-poll-attempts", "48");
  await set(w, "canary-max-latency", "240");
  await set(w, "canary-required-fields", "s3_path, byte_size");
  await set(w, "canary-lifecycle-path", "s3_path");
  await set(w, "canary-cleanup-prefix", "canary/");
  await flushPromises();
}

describe("the typed form exists and nothing else does", () => {
  it("puts the workflow sections on the canary form and nowhere else", async () => {
    const w = await mountCanaryForm();
    expect(w.find('[data-testid="canary-workflow"]').exists()).toBe(true);

    const http = w.findAll("button").find((b) => b.text().includes("HTTP"));
    await http!.trigger("click");
    await flushPromises();
    // A `canary_secret_*` key is refused by the server on any other type, so the section must not
    // exist where it could be filled in.
    expect(w.find('[data-testid="canary-workflow"]').exists()).toBe(false);
  });

  it("has NO JSON editor anywhere — the contract the mock was approved for", async () => {
    const w = await mountCanaryForm();
    // Asserted as a MECHANISM, not as a word search: an earlier version of this case grepped the
    // rendered HTML for /workflow.*json/ and failed on the innocent label "Required JSON fields",
    // which is a test about spelling rather than about the contract.
    expect(w.findAll("textarea").length).toBe(0);
    // No control anywhere holds the document. If a JSON editor existed, some input's value would be
    // the document — and the document always begins with its kind.
    for (const el of [...w.findAll("input"), ...w.findAll("select")]) {
      expect(String((el.element as HTMLInputElement).value)).not.toContain('"kind"');
      expect(String((el.element as HTMLInputElement).value)).not.toContain("async_transaction_v1");
    }
    // And the positive half of "typed form only": every stage is a section of typed fields.
    const text = w.find('[data-testid="canary-workflow"]').text();
    for (const stage of ["Secrets", "Submit", "Correlate", "Completion", "Result", "Cleanup"]) {
      expect(text).toContain(stage);
    }
  });

  it("refuses an interval shorter than the timeout, at the field that causes it", async () => {
    const w = await mountCanaryForm();
    await set(w, "monitor-timeout", "300");
    await set(w, "monitor-interval", "60");
    const msg = w.find('[data-testid="canary-refusal-cadence"]');
    expect(msg.exists()).toBe(true);
    expect(msg.text()).toMatch(/may not overlap the next/);
  });

  it("hides the target field, because a canary has none", async () => {
    const w = await mountCanaryForm();
    // The same gap that left a synthetic monitor uncreatable until iter-0167: asking for a value the
    // type does not have, and then refusing to submit without it.
    expect(w.find('[data-testid="canary-workflow"]').exists()).toBe(true);
    const labels = w.findAll("span").map((s) => s.text());
    expect(labels).not.toContain("URL");
  });
});

describe("a credential-bearing header offers a binding and nothing else (D7)", () => {
  it("swaps the value box for a binding picker from the first keystroke of the name", async () => {
    const w = await mountCanaryForm();
    await w.find('[data-testid="canary-add-submit-header"]').trigger("click");
    await flushPromises();

    // An ordinary header keeps its free-text value.
    await w.find('[aria-label="submit header name 0"]').setValue("x-tenant");
    await flushPromises();
    expect(w.find('[aria-label="submit header value 0"]').exists()).toBe(true);
    expect(w.find('[aria-label="submit header binding 0"]').exists()).toBe(false);

    // A credential-bearing one does not: the control itself teaches the rule, before a token is
    // pasted rather than after a failed save.
    await w.find('[aria-label="submit header name 0"]').setValue("authorization");
    await flushPromises();
    expect(w.find('[data-testid="canary-credential-header"]').exists()).toBe(true);
    expect(w.find('[aria-label="submit header binding 0"]').exists()).toBe(true);
    expect(w.find('[aria-label="submit header value 0"]').exists()).toBe(false);
  });

  it("states the residual instead of implying a guarantee it cannot give", async () => {
    const w = await mountCanaryForm();
    expect(w.find('[data-testid="canary-workflow"]').text()).toMatch(/not detectable/i);
  });
});

describe("refusals appear at their own field", () => {
  it("refuses the correlation placeholder in the submit URL and says why", async () => {
    const w = await mountCanaryForm();
    await set(w, "canary-submit-url", "https://files.example.com/files/{{ correlation_id }}/upload");
    const msg = w.find('[data-testid="canary-refusal-submitURL"]');
    expect(msg.exists()).toBe(true);
    expect(msg.text()).toMatch(/only legal in the completion URL/);
  });

  it("refuses plaintext HTTP at the URL field", async () => {
    const w = await mountCanaryForm();
    await set(w, "canary-submit-url", "http://files.example.com/files/upload");
    expect(w.find('[data-testid="canary-refusal-submitURL"]').text()).toMatch(/HTTPS only/);
  });

  it("refuses a poll budget that cannot fit its window, at the attempts field", async () => {
    const w = await mountCanaryForm();
    await fillValid(w);
    await set(w, "canary-poll-interval", "10");
    await set(w, "canary-poll-attempts", "60");
    expect(w.find('[data-testid="canary-refusal-pollMaxAttempts"]').text()).toMatch(
      /fit inside the completion timeout/,
    );
  });

  it("demands the acknowledgement for cleanup: none (D10)", async () => {
    const w = await mountCanaryForm();
    await fillValid(w);
    await w.find('[data-testid="canary-cleanup-kind"]').setValue("none");
    await flushPromises();
    const msg = w.find('[data-testid="canary-refusal-cleanupAcknowledged"]');
    expect(msg.exists()).toBe(true);
    expect(msg.text()).toMatch(/must be acknowledged/);
    await w.find('[data-testid="canary-cleanup-ack"]').setValue(true);
    await flushPromises();
    expect(w.find('[data-testid="canary-refusal-cleanupAcknowledged"]').exists()).toBe(false);
  });
});

describe("what leaves the page", () => {
  it("sends the document and the flat ref keys, with no secret name inside the document", async () => {
    const w = await mountCanaryForm();
    await fillValid(w);
    // Declare a binding and use it, so the wire body carries both halves.
    await w.find('[data-testid="canary-add-binding"]').trigger("click");
    await flushPromises();
    await w.find('[aria-label="binding name 0"]').setValue("upload");
    await w.find('[aria-label="project secret 0"]').setValue("upload-token");
    await w.find('[data-testid="canary-add-submit-header"]').trigger("click");
    await flushPromises();
    await w.find('[aria-label="submit header name 0"]').setValue("authorization");
    await flushPromises();
    await w.find('[aria-label="submit header binding 0"]').setValue("upload");
    await flushPromises();

    const submit = w.findAll("button").find((b) => /create monitor/i.test(b.text()));
    expect(submit, "the form offers Create").toBeTruthy();
    expect(submit!.attributes("disabled"), "a valid canary must be submittable").toBeUndefined();
    // The page submits through the FORM, not through a click handler on the button — the same way
    // `NewMonitorScenarioBinding.spec.ts` drives it, and jsdom does not turn a click on a submit
    // button into a form submission by itself.
    await w.find("form").trigger("submit");
    await flushPromises();

    expect(apiMock.POST).toHaveBeenCalled();
    const body = apiMock.POST.mock.calls.at(-1)![1].body as any;
    expect(body.type).toBe("async_canary");
    expect(body.config.canary_secret_upload_ref).toBe("upload-token");
    const doc = body.config.workflow as string;
    expect(doc).toBeTruthy();
    // D3f: the document keeps the marker and never the project-secret name.
    expect(doc).not.toContain("upload-token");
    expect(doc).toContain('"secret_ref":"upload"');
    expect(JSON.parse(doc).kind).toBe("async_transaction_v1");
    // No target is sent for a type that has none.
    expect(body.target ?? "").toBe("");
  });

  it("keeps Create disabled while a refusal stands", async () => {
    const w = await mountCanaryForm();
    await fillValid(w);
    await set(w, "canary-submit-url", "http://files.example.com/files/upload"); // HTTPS only
    const submit = w.findAll("button").find((b) => /create monitor/i.test(b.text()));
    expect(submit!.attributes("disabled")).toBeDefined();
  });
});
