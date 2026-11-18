import { type VueWrapper, flushPromises, mount } from "@vue/test-utils";
import { beforeEach, describe, expect, it, vi } from "vitest";

import TelephonyPanel from "@/components/settings/TelephonyPanel.vue";

// FR-041a §4.1, mock §1 (1a–1i, 1c′, 1c″): Settings → Administration → Telephony. The assertions
// are on what the admin sees and on the BODY that leaves the page: a secret nobody retyped is
// absent from it, both provider blocks always travel, and a confirmed save is the refused body plus
// `confirm_unroutable: true`. The server's sentences are shown as they come (rulings 8 and 9).

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), PUT: vi.fn(), PATCH: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));

const sessionState = vi.hoisted(() => ({
  value: null as null | Record<string, unknown>,
}));
vi.mock("@/stores/session", async () => {
  const { reactive } = await vi.importActual<typeof import("vue")>("vue");
  return {
    useSession: () => {
      if (!sessionState.value) {
        sessionState.value = reactive({
          isGlobalAdmin: true,
          user: { id: "u1", email: "admin@cerbix.local" },
          phone: { number_masked: "+90•••••6869", verified: true, language: null },
          telephonyEnabled: true,
          fetchMe: vi.fn(async () => {}),
        });
      }
      return sessionState.value;
    },
  };
});

type View = Record<string, unknown>;
type Body = Record<string, any>; // eslint-disable-line @typescript-eslint/no-explicit-any

function twilioView(over: View = {}): View {
  return {
    configured: true,
    encryption_key_configured: true,
    enabled: true,
    provider: "twilio",
    twilio: { account_sid: "AC7f3e0c1b9a4d6e2f8a0b3c5d7e9f1a2b", auth_token_set: true, auth_token_unreadable: false, from: "+15550100" },
    generic_http: { method: "POST", url: "", headers_set: false, headers_unreadable: false, content_type: "application/json", body_template: "", from: "", response_code_field: "" },
    public_base_url: "",
    voice_language: "en-US",
    sms_encoding: "transliterate",
    sms_max_segments: 2,
    storm: { per_incident_per_hour: 5, per_number_per_hour: 20 },
    ...over,
  };
}

function gatewayView(over: View = {}): View {
  return twilioView({
    provider: "generic_http",
    generic_http: {
      method: "POST",
      url: "https://sms-gw.example.net/v2/messages",
      headers_set: true,
      headers_unreadable: false,
      content_type: "application/json",
      body_template: '{"to": "{{to}}", "from": "{{from}}", "message": "{{text}}"}',
      from: "CERBIX",
      response_code_field: "error_code",
    },
    ...over,
  });
}

let telephonyView: View;
let mailURL: string;

function ok<T>(data: T) {
  return { data, error: undefined, response: { status: 200, ok: true } };
}
function fail(status: number, error: unknown) {
  return { data: undefined, error, response: { status, ok: false } };
}

beforeEach(() => {
  for (const f of Object.values(apiMock)) f.mockReset();
  sessionState.value = null;
  telephonyView = twilioView();
  mailURL = "https://cerbix.example.com";
  apiMock.GET.mockImplementation(async (path: string) => {
    if (path === "/api/v1/settings/telephony") return ok(telephonyView);
    if (path === "/api/v1/settings/mail") return ok({ public_base_url: mailURL });
    return ok({});
  });
});

function session() {
  return sessionState.value as { phone: unknown; fetchMe: ReturnType<typeof vi.fn> };
}

async function mountPanel(): Promise<VueWrapper> {
  const w = mount(TelephonyPanel);
  await flushPromises();
  // the mock session is created by the panel's first useSession()
  return w;
}

const norm = (s: string) => s.replace(/\s+/g, " ").trim();
const tid = (w: VueWrapper, id: string) => w.get(`[data-testid="${id}"]`);
const has = (w: VueWrapper, id: string) => w.find(`[data-testid="${id}"]`).exists();
const text = (w: VueWrapper, id: string) => norm(tid(w, id).text());

function putBodies(): Body[] {
  return apiMock.PUT.mock.calls.map((c) => (c[1] as { body: Body }).body);
}
async function save(w: VueWrapper) {
  await tid(w, "telephony-save").trigger("click");
  await flushPromises();
}

describe("TelephonyPanel · 1a not configured", () => {
  it("opens on Twilio with the §4.1 defaults and the 'Off · not configured' pill", async () => {
    telephonyView = {
      configured: false,
      encryption_key_configured: true,
      enabled: false,
      provider: "",
      twilio: { account_sid: "", auth_token_set: false, from: "" },
      generic_http: { method: "", url: "", headers_set: false, content_type: "", body_template: "", from: "", response_code_field: "" },
    };
    const w = await mountPanel();
    expect(text(w, "telephony-pill")).toBe("Off · not configured");
    expect((tid(w, "telephony-provider-twilio").element as HTMLInputElement).checked).toBe(true);
    expect((tid(w, "telephony-provider-generic").element as HTMLInputElement).checked).toBe(false);
    expect((tid(w, "telephony-enabled").element as HTMLInputElement).checked).toBe(false);
    expect((tid(w, "telephony-voice-language").element as HTMLInputElement).value).toBe("en-US");
    expect((tid(w, "telephony-encoding-transliterate").element as HTMLInputElement).checked).toBe(true);
    expect((tid(w, "telephony-max-segments").element as HTMLSelectElement).value).toBe("2");
    expect((tid(w, "telephony-storm-incident").element as HTMLInputElement).value).toBe("5");
    expect((tid(w, "telephony-storm-number").element as HTMLInputElement).value).toBe("20");
    expect(text(w, "telephony-save-note")).toBe("Nothing is stored until you save.");
    expect((tid(w, "twilio-auth-token").attributes("placeholder"))).toBe("auth token");
    expect(has(w, "twilio-auth-token-stored")).toBe(false);
  });

  it("pills read 'On · Twilio', 'On · Generic HTTP' and 'Off'", async () => {
    let w = await mountPanel();
    expect(text(w, "telephony-pill")).toBe("On · Twilio");
    telephonyView = gatewayView();
    w = await mountPanel();
    expect(text(w, "telephony-pill")).toBe("On · Generic HTTP");
    telephonyView = twilioView({ enabled: false });
    w = await mountPanel();
    expect(text(w, "telephony-pill")).toBe("Off");
  });
});

describe("TelephonyPanel · 1b secrets and the whole-group body", () => {
  it("marks a stored token, never echoes it, and keeps it by leaving the key out of the body", async () => {
    apiMock.PUT.mockResolvedValue(ok(telephonyView));
    const w = await mountPanel();
    expect(text(w, "twilio-auth-token-stored")).toBe("✓ stored");
    const token = tid(w, "twilio-auth-token");
    expect(token.attributes("placeholder")).toBe("•••••••• (unchanged)");
    expect((token.element as HTMLInputElement).value).toBe("");
    await save(w);
    const [body] = putBodies();
    expect(body.twilio).toEqual({ account_sid: "AC7f3e0c1b9a4d6e2f8a0b3c5d7e9f1a2b", from: "+15550100" });
    expect("auth_token" in body.twilio).toBe(false);
    // both provider blocks travel, so switching provider never wipes the other's stored fields
    expect(body.generic_http).toEqual({
      method: "POST",
      url: "",
      content_type: "application/json",
      body_template: "",
      from: "",
      response_code_field: "",
    });
    expect(body).toMatchObject({
      enabled: true,
      provider: "twilio",
      public_base_url: "",
      voice_language: "en-US",
      sms_encoding: "transliterate",
      sms_max_segments: 2,
      storm: { per_incident_per_hour: 5, per_number_per_hour: 20 },
    });
    expect("confirm_unroutable" in body).toBe(false);
  });

  it("sends a typed token, then clears the field after the save", async () => {
    apiMock.PUT.mockResolvedValue(ok(telephonyView));
    const w = await mountPanel();
    await tid(w, "twilio-auth-token").setValue("tok-123");
    await save(w);
    expect(putBodies()[0].twilio.auth_token).toBe("tok-123");
    expect((tid(w, "twilio-auth-token").element as HTMLInputElement).value).toBe("");
  });

  it("names a Messaging Service From as SMS only", async () => {
    const w = await mountPanel();
    expect(has(w, "telephony-messaging-note")).toBe(false);
    await tid(w, "twilio-from").setValue("MG0123456789abcdef0123456789abcdef");
    expect(text(w, "telephony-messaging-note")).toContain("SMS only.");
  });
});

describe("TelephonyPanel · unreadable secrets", () => {
  it("a token the keys cannot open is not shown as stored and healthy", async () => {
    telephonyView = twilioView({ twilio: { account_sid: "AC7f3e0c1b9a4d6e2f8a0b3c5d7e9f1a2b", auth_token_set: true, auth_token_unreadable: true, from: "+15550100" } });
    const w = await mountPanel();
    const mark = tid(w, "twilio-auth-token-stored");
    expect(norm(mark.text())).toBe("stored · cannot be read");
    expect(mark.text()).not.toContain("✓");
    expect(mark.classes()).toContain("text-down");
    expect(mark.classes()).not.toContain("text-up");
    expect(tid(w, "twilio-auth-token").attributes("placeholder")).toBe("stored, but unreadable — type it again");
  });

  it("headers the keys cannot open read the same", async () => {
    telephonyView = gatewayView({ generic_http: { ...(gatewayView().generic_http as View), headers_unreadable: true } });
    const w = await mountPanel();
    expect(text(w, "generic-headers-stored")).toBe("stored · cannot be read");
    expect(tid(w, "generic-headers-stored").classes()).toContain("text-down");
    expect(tid(w, "generic-headers").attributes("placeholder")).toBe("stored, but unreadable — type it again");
  });
});

describe("TelephonyPanel · the gateway block while Twilio is selected", () => {
  beforeEach(() => {
    telephonyView = gatewayView({ enabled: false });
    apiMock.PUT.mockResolvedValue(ok(telephonyView));
  });

  it("a header line that does not parse, left in the hidden block, does not stop Save", async () => {
    const w = await mountPanel();
    await tid(w, "generic-headers").setValue("not a header");
    await tid(w, "telephony-provider-twilio").setValue(true);
    await save(w);
    expect(apiMock.PUT).toHaveBeenCalledTimes(1);
    const g = putBodies()[0].generic_http;
    expect("headers" in g).toBe(false);
    expect("headers_clear" in g).toBe(false);
  });

  it("a removal marked in the hidden block is not sent", async () => {
    const w = await mountPanel();
    await tid(w, "generic-headers-remove").trigger("click");
    await tid(w, "telephony-provider-twilio").setValue(true);
    await save(w);
    const g = putBodies()[0].generic_http;
    expect("headers_clear" in g).toBe(false);
    expect("headers" in g).toBe(false);
  });

  it("typed headers in the hidden block are not sent", async () => {
    const w = await mountPanel();
    await tid(w, "generic-headers").setValue("X-Api-Key: abc");
    await tid(w, "telephony-provider-twilio").setValue(true);
    await save(w);
    expect("headers" in putBodies()[0].generic_http).toBe(false);
  });
});

describe("TelephonyPanel · 1c generic HTTP headers", () => {
  beforeEach(() => {
    telephonyView = gatewayView();
    apiMock.PUT.mockResolvedValue(ok(telephonyView));
  });

  it("shows the gateway's SMS-only line and the stored headers' mark", async () => {
    const w = await mountPanel();
    expect(text(w, "telephony-gateway-note")).toContain("SMS only.");
    expect(text(w, "generic-headers-stored")).toBe("✓ stored");
    expect(tid(w, "generic-headers").attributes("placeholder")).toBe("•••••••• (unchanged)");
  });

  it("keeps the stored headers when nothing is typed", async () => {
    const w = await mountPanel();
    await save(w);
    const g = putBodies()[0].generic_http;
    expect("headers" in g).toBe(false);
    expect("headers_clear" in g).toBe(false);
    expect(g).toMatchObject({ method: "POST", url: "https://sms-gw.example.net/v2/messages", from: "CERBIX", response_code_field: "error_code" });
  });

  it("replaces them with the typed map", async () => {
    const w = await mountPanel();
    await tid(w, "generic-headers").setValue("X-Api-Key: abc\nAuthorization: Bearer 9f2c");
    await save(w);
    expect(putBodies()[0].generic_http.headers).toEqual({ "X-Api-Key": "abc", Authorization: "Bearer 9f2c" });
  });

  it("refuses a line that is not 'Name: value' before sending", async () => {
    const w = await mountPanel();
    await tid(w, "generic-headers").setValue("X-Api-Key: abc\nnot a header");
    await save(w);
    expect(apiMock.PUT).not.toHaveBeenCalled();
    expect(text(w, "generic-headers-error")).toContain("Line 2");
  });

  it("1c′: Remove stored headers locks the field and sends headers_clear alone; Undo restores", async () => {
    const w = await mountPanel();
    await tid(w, "generic-headers-remove").trigger("click");
    expect(tid(w, "generic-headers").attributes("disabled")).toBeDefined();
    expect(text(w, "generic-headers-removal")).toContain("Removed when you save. The gateway will be called with no extra headers.");
    await tid(w, "generic-headers-undo").trigger("click");
    expect(tid(w, "generic-headers").attributes("disabled")).toBeUndefined();
    expect(has(w, "generic-headers-removal")).toBe(false);
    await save(w);
    expect("headers_clear" in putBodies()[0].generic_http).toBe(false);

    await tid(w, "generic-headers-remove").trigger("click");
    await save(w);
    const g = putBodies()[1].generic_http;
    expect(g.headers_clear).toBe(true);
    expect("headers" in g).toBe(false);
  });

  it("offers no removal when no headers are stored", async () => {
    telephonyView = gatewayView({ generic_http: { ...(gatewayView().generic_http as View), headers_set: false } });
    const w = await mountPanel();
    expect(has(w, "generic-headers-remove")).toBe(false);
    expect(has(w, "generic-headers-stored")).toBe(false);
  });
});

describe("TelephonyPanel · 1d / 1c″ refusals", () => {
  beforeEach(() => {
    telephonyView = gatewayView();
  });

  it("1d: template_invalid shows the server's sentence under the field and the note under Save", async () => {
    const sentence = "generic_http.body_template: {{to}} is outside a JSON string";
    apiMock.PUT.mockResolvedValue(fail(400, { error: sentence, reason: "template_invalid" }));
    const w = await mountPanel();
    await save(w);
    expect(text(w, "generic-template-error")).toBe(sentence);
    expect(text(w, "telephony-save-note")).toBe("Not saved: the body template has a placeholder outside a value.");
    expect(has(w, "telephony-saved")).toBe(false);
  });

  it("1c″: headers_conflict shows its sentence under Save", async () => {
    apiMock.PUT.mockResolvedValue(fail(400, { error: "generic_http.headers_clear cannot be sent together with new headers", reason: "headers_conflict" }));
    const w = await mountPanel();
    await save(w);
    expect(text(w, "telephony-save-note")).toBe(
      "Not saved: remove the stored headers or type new ones, not both. To replace them, type the new headers and leave “Remove stored headers” off.",
    );
  });

  it("any other refusal shows the server's sentence as it is", async () => {
    apiMock.PUT.mockResolvedValue(fail(400, { error: "twilio.from must be an E.164 number or a Messaging Service SID" }));
    const w = await mountPanel();
    await save(w);
    expect(text(w, "telephony-save-note")).toBe("twilio.from must be an E.164 number or a Messaging Service SID");
  });
});

describe("TelephonyPanel · 1e no encryption key", () => {
  it("says so on load and disables every field and Save", async () => {
    telephonyView = twilioView({ configured: false, enabled: false, encryption_key_configured: false });
    const w = await mountPanel();
    expect(text(w, "telephony-key-missing")).toContain("Phone alerts need an encryption key, and this server has none.");
    expect(tid(w, "telephony-save").attributes("disabled")).toBeDefined();
    // every field sits in one disabled <fieldset>
    for (const id of ["telephony-enabled", "telephony-provider-generic", "twilio-account-sid", "twilio-auth-token", "telephony-storm-number"]) {
      expect(tid(w, id).element.matches(":disabled"), id).toBe(true);
    }
    expect(text(w, "telephony-save-note")).toBe("Needs the encryption key.");
    // the guard inside save() holds even when the button's disabled attribute is gone
    tid(w, "telephony-save").element.removeAttribute("disabled");
    await tid(w, "telephony-save").trigger("click");
    await flushPromises();
    expect(apiMock.PUT).not.toHaveBeenCalled();
  });

  it("a 409 encryption_key_missing at save reads the same sentence under Save", async () => {
    apiMock.PUT.mockResolvedValue(fail(409, { error: "telephony settings need security.encryption_key to be configured", reason: "encryption_key_missing" }));
    const w = await mountPanel();
    await save(w);
    expect(text(w, "telephony-save-note")).toContain("Phone alerts need an encryption key, and this server has none.");
  });
});

const threeServices = [
  { project: "prod", service: "Checkout", slug: "checkout" },
  { project: "prod", service: "Payments", slug: "payments" },
  { project: "staging", service: "Search", slug: "search" },
];
function unroutable(services: object[], total: number) {
  return fail(409, {
    error: `this change leaves ${total} paging service(s) without a route; resend with confirm_unroutable to save it`,
    reason: "services_without_route",
    services_without_route: services,
    services_without_route_total: total,
  });
}

describe("TelephonyPanel · 1f switching phone alerts off", () => {
  it("shows the services grouped by project, and 'Turn off anyway' resends the same body confirmed", async () => {
    apiMock.PUT.mockResolvedValueOnce(unroutable(threeServices, 3)).mockResolvedValueOnce(ok(twilioView({ enabled: false })));
    const w = await mountPanel();
    await tid(w, "telephony-enabled").setValue(false);
    await save(w);
    const warn = text(w, "telephony-unroutable");
    expect(warn).toContain("Not saved yet — 3 services would be left with no route.");
    expect(warn).toContain("Phone channels stay, but they would not be called or texted and would stop counting as routes.");
    const groups = w.findAll('[data-testid="telephony-unroutable-group"]');
    expect(groups).toHaveLength(2);
    expect(norm(groups[0].text())).toContain("checkout");
    expect(norm(groups[0].text())).toContain("payments");
    expect(norm(groups[0].text())).toContain("project prod");
    expect(norm(groups[1].text())).toContain("search");
    expect(norm(groups[1].text())).toContain("project staging");
    expect(has(w, "telephony-saved")).toBe(false);
    expect(text(w, "telephony-unroutable-confirm")).toBe("Turn off anyway");
    expect(text(w, "telephony-unroutable-keep")).toBe("Keep phone alerts on");

    await tid(w, "telephony-unroutable-confirm").trigger("click");
    await flushPromises();
    const [first, second] = putBodies();
    expect(first.enabled).toBe(false);
    expect(second).toEqual({ ...first, confirm_unroutable: true });
    expect(has(w, "telephony-unroutable")).toBe(false);
    expect(text(w, "telephony-saved")).toBe("✓ Saved");
    expect(text(w, "telephony-pill")).toBe("Off");
  });

  it("'Keep phone alerts on' ticks Enable again and sends nothing", async () => {
    apiMock.PUT.mockResolvedValueOnce(unroutable(threeServices, 3));
    const w = await mountPanel();
    await tid(w, "telephony-enabled").setValue(false);
    await save(w);
    await tid(w, "telephony-unroutable-keep").trigger("click");
    await flushPromises();
    expect((tid(w, "telephony-enabled").element as HTMLInputElement).checked).toBe(true);
    expect(apiMock.PUT).toHaveBeenCalledTimes(1);
    expect(has(w, "telephony-unroutable")).toBe(false);
  });

  it("more than 50: '… and 23 more.'", async () => {
    const fifty = Array.from({ length: 50 }, (_, i) => ({ project: "prod", service: `S${i}`, slug: `s-${String(i).padStart(2, "0")}` }));
    apiMock.PUT.mockResolvedValueOnce(unroutable(fifty, 73));
    const w = await mountPanel();
    await tid(w, "telephony-enabled").setValue(false);
    await save(w);
    expect(text(w, "telephony-unroutable")).toContain("Not saved yet — 73 services would be left with no route.");
    expect(text(w, "telephony-unroutable-more")).toBe("… and 23 more.");
  });

  it("nobody loses a route: no 409, saved at once", async () => {
    apiMock.PUT.mockResolvedValueOnce(ok(twilioView({ enabled: false })));
    const w = await mountPanel();
    await tid(w, "telephony-enabled").setValue(false);
    await save(w);
    expect(apiMock.PUT).toHaveBeenCalledTimes(1);
    expect(text(w, "telephony-saved")).toBe("✓ Saved");
  });

  it("a change of the form withdraws a warning the server gave for another body", async () => {
    apiMock.PUT.mockResolvedValueOnce(unroutable(threeServices, 3));
    const w = await mountPanel();
    await tid(w, "telephony-enabled").setValue(false);
    await save(w);
    await tid(w, "telephony-storm-number").setValue("30");
    expect(has(w, "telephony-unroutable")).toBe(false);
  });
});

describe("TelephonyPanel · 1g switching to the generic HTTP gateway", () => {
  it("names the gateway's missing calls; 'Keep Twilio' resets the provider and sends nothing", async () => {
    apiMock.PUT.mockResolvedValueOnce(unroutable([{ project: "prod", service: "Payments", slug: "payments" }], 1));
    const w = await mountPanel();
    await tid(w, "telephony-provider-generic").setValue(true);
    await tid(w, "generic-url").setValue("https://sms-gw.example.net/v2/messages");
    await tid(w, "generic-body-template").setValue('{"to": "{{to}}", "message": "{{text}}"}');
    await save(w);
    const warn = text(w, "telephony-unroutable");
    expect(warn).toContain("Not saved yet — 1 service would be left with no route.");
    expect(warn).toContain("The generic HTTP gateway cannot place calls");
    expect(warn).toContain("SMS channels are not affected.");
    expect(text(w, "telephony-unroutable-confirm")).toBe("Switch anyway");
    expect(text(w, "telephony-unroutable-keep")).toBe("Keep Twilio");
    await tid(w, "telephony-unroutable-keep").trigger("click");
    await flushPromises();
    expect((tid(w, "telephony-provider-twilio").element as HTMLInputElement).checked).toBe(true);
    expect(apiMock.PUT).toHaveBeenCalledTimes(1);
  });

  it("'Switch anyway' resends confirmed", async () => {
    apiMock.PUT.mockResolvedValueOnce(unroutable([{ project: "prod", service: "Payments", slug: "payments" }], 1)).mockResolvedValueOnce(ok(gatewayView()));
    const w = await mountPanel();
    await tid(w, "telephony-provider-generic").setValue(true);
    await save(w);
    await tid(w, "telephony-unroutable-confirm").trigger("click");
    await flushPromises();
    const [first, second] = putBodies();
    expect(first.provider).toBe("generic_http");
    expect(second).toEqual({ ...first, confirm_unroutable: true });
    expect(text(w, "telephony-saved")).toBe("✓ Saved");
  });

  it("enabled → off dominates when both changed", async () => {
    apiMock.PUT.mockResolvedValueOnce(unroutable(threeServices, 3));
    const w = await mountPanel();
    await tid(w, "telephony-provider-generic").setValue(true);
    await tid(w, "telephony-enabled").setValue(false);
    await save(w);
    expect(text(w, "telephony-unroutable-confirm")).toBe("Turn off anyway");
  });
});

describe("TelephonyPanel · a Messaging Service From and a 409 of unknown cause", () => {
  const mg = "MG0123456789abcdef0123456789abcdef";

  it("a 409 for an MG From names the Messaging Service; 'Keep the From number' restores the saved From", async () => {
    apiMock.PUT.mockResolvedValueOnce(unroutable([{ project: "prod", service: "Payments", slug: "payments" }], 1));
    const w = await mountPanel();
    await tid(w, "twilio-from").setValue(mg);
    await save(w);
    expect(putBodies()[0].twilio.from).toBe(mg);
    const warn = text(w, "telephony-unroutable");
    expect(warn).toContain("Not saved yet — 1 service would be left with no route.");
    expect(warn).toContain("A Messaging Service cannot place calls");
    expect(text(w, "telephony-unroutable-confirm")).toBe("Switch anyway");
    expect(text(w, "telephony-unroutable-keep")).toBe("Keep the From number");
    await tid(w, "telephony-unroutable-keep").trigger("click");
    await flushPromises();
    expect((tid(w, "twilio-from").element as HTMLInputElement).value).toBe("+15550100");
    expect(apiMock.PUT).toHaveBeenCalledTimes(1);
    expect(has(w, "telephony-unroutable")).toBe(false);
  });

  it("a 409 the form cannot attribute shows the server's sentence; Save anyway confirms, Cancel sends nothing", async () => {
    const sentence = "this change leaves 2 paging service(s) without a route; resend with confirm_unroutable to save it";
    apiMock.PUT.mockResolvedValueOnce(unroutable(threeServices.slice(0, 2), 2)).mockResolvedValueOnce(unroutable(threeServices.slice(0, 2), 2));
    const w = await mountPanel();
    await tid(w, "telephony-storm-number").setValue("30");
    await save(w);
    const warn = text(w, "telephony-unroutable");
    expect(warn.startsWith(sentence)).toBe(true);
    expect(warn).not.toContain("Not saved yet");
    expect(w.findAll('[data-testid="telephony-unroutable-group"]')).toHaveLength(1);
    expect(text(w, "telephony-unroutable-confirm")).toBe("Save anyway");
    expect(text(w, "telephony-unroutable-keep")).toBe("Cancel");
    await tid(w, "telephony-unroutable-keep").trigger("click");
    await flushPromises();
    expect(has(w, "telephony-unroutable")).toBe(false);
    expect(apiMock.PUT).toHaveBeenCalledTimes(1);
    expect((tid(w, "telephony-storm-number").element as HTMLInputElement).value).toBe("30");

    apiMock.PUT.mockReset();
    apiMock.PUT.mockResolvedValueOnce(unroutable(threeServices.slice(0, 2), 2)).mockResolvedValueOnce(ok(twilioView({ storm: { per_incident_per_hour: 5, per_number_per_hour: 30 } })));
    await save(w);
    await tid(w, "telephony-unroutable-confirm").trigger("click");
    await flushPromises();
    const [first, second] = putBodies();
    expect(second).toEqual({ ...first, confirm_unroutable: true });
    expect(text(w, "telephony-saved")).toBe("✓ Saved");
  });
});

describe("TelephonyPanel · while a save is in flight", () => {
  it("locks the form, and an edit that slips in anyway withdraws the warning instead of being confirmed", async () => {
    let resolve!: (v: unknown) => void;
    apiMock.PUT.mockReturnValueOnce(new Promise((r) => (resolve = r)));
    const w = await mountPanel();
    await tid(w, "telephony-enabled").setValue(false);
    await tid(w, "telephony-save").trigger("click");
    await flushPromises();
    for (const id of ["telephony-enabled", "twilio-from", "telephony-storm-number"]) {
      expect(tid(w, id).element.matches(":disabled"), id).toBe(true);
    }
    // an edit the lock did not stop (dispatched straight at the element)
    const storm = tid(w, "telephony-storm-number").element as HTMLInputElement;
    storm.value = "31";
    storm.dispatchEvent(new Event("input"));
    resolve(unroutable(threeServices, 3));
    await flushPromises();
    expect(has(w, "telephony-unroutable")).toBe(false);
    expect(storm.value).toBe("31");
    expect(apiMock.PUT).toHaveBeenCalledTimes(1);
  });

  it("without an edit meanwhile, the warning is shown", async () => {
    let resolve!: (v: unknown) => void;
    apiMock.PUT.mockReturnValueOnce(new Promise((r) => (resolve = r)));
    const w = await mountPanel();
    await tid(w, "telephony-enabled").setValue(false);
    await tid(w, "telephony-save").trigger("click");
    resolve(unroutable(threeServices, 3));
    await flushPromises();
    expect(has(w, "telephony-unroutable")).toBe(true);
    expect(tid(w, "telephony-enabled").element.matches(":disabled")).toBe(false);
  });
});

describe("TelephonyPanel · 1h no public URL anywhere", () => {
  it("warns iff this field and Email's public URL are both empty; Save stays enabled", async () => {
    mailURL = "";
    const w = await mountPanel();
    expect(text(w, "telephony-no-link-warning")).toContain("Messages will carry no link.");
    expect(tid(w, "telephony-save").attributes("disabled")).toBeUndefined();
    await tid(w, "telephony-public-base-url").setValue("https://alerts.example.com");
    expect(has(w, "telephony-no-link-warning")).toBe(false);
  });

  it("names the mail URL in the hint when it is set", async () => {
    const w = await mountPanel();
    expect(has(w, "telephony-no-link-warning")).toBe(false);
    expect(text(w, "telephony-public-url-hint")).toContain("Empty: links use https://cerbix.example.com from Email settings.");
  });
});

describe("TelephonyPanel · 1i Send test SMS", () => {
  it("no verified number: disabled, with the way to Account → Phone", async () => {
    const w0 = mount(TelephonyPanel);
    session().phone = null;
    await flushPromises();
    const btn = tid(w0, "telephony-test-send");
    expect(norm(btn.text())).toBe("Send test SMS");
    expect(btn.attributes("disabled")).toBeDefined();
    expect(text(w0, "telephony-test-note")).toBe("You have no verified number. Add one under Account → Phone.");
    expect(has(w0, "telephony-test-phone-link")).toBe(true);
  });

  it("an unverified number reads the same", async () => {
    const w0 = mount(TelephonyPanel);
    session().phone = { number_masked: "+90•••••6869", verified: false, language: null };
    await flushPromises();
    expect(text(w0, "telephony-test-note")).toBe("You have no verified number. Add one under Account → Phone.");
  });

  it("ready: names the masked number", async () => {
    const w = await mountPanel();
    expect(text(w, "telephony-test-send")).toBe("Send test SMS to +90•••••6869");
    expect(tid(w, "telephony-test-send").attributes("disabled")).toBeUndefined();
    expect(text(w, "telephony-test-note")).toBe("Sends a fixed text to your own verified number, with the saved settings.");
  });

  it("unsaved changes: disabled until saved", async () => {
    const w = await mountPanel();
    await tid(w, "twilio-from").setValue("+15550111");
    expect(tid(w, "telephony-test-send").attributes("disabled")).toBeDefined();
    expect(text(w, "telephony-test-note")).toBe("Save first — the test uses the saved settings.");
  });

  it("phone alerts off: disabled", async () => {
    telephonyView = twilioView({ enabled: false });
    const w = await mountPanel();
    expect(tid(w, "telephony-test-send").attributes("disabled")).toBeDefined();
    expect(text(w, "telephony-test-note")).toBe("Turn phone alerts on and save to send a test.");
  });

  it("sending: 'Sending…' and disabled", async () => {
    let resolve!: (v: unknown) => void;
    apiMock.POST.mockReturnValue(new Promise((r) => (resolve = r)));
    const w = await mountPanel();
    await tid(w, "telephony-test-send").trigger("click");
    expect(text(w, "telephony-test-send")).toBe("Sending…");
    expect(tid(w, "telephony-test-send").attributes("disabled")).toBeDefined();
    resolve(ok({ outcome: "sent", http_status: 201, provider_code: "" }));
    await flushPromises();
    expect(apiMock.POST.mock.calls[0][0]).toBe("/api/v1/settings/telephony/test-sms");
  });

  const rows: [string, View, Record<string, unknown>, string][] = [
    ["accepted", twilioView(), { outcome: "sent", http_status: 201, provider_code: "" }, "✓ Accepted by the provider · HTTP 201. Check your phone."],
    ["rejected with a code", twilioView(), { outcome: "rejected", http_status: 400, provider_code: "21211" }, "Rejected by the provider · HTTP 400 · code 21211. Check the From number and the account."],
    [
      "rejected by a gateway without an error code field",
      gatewayView({ generic_http: { ...(gatewayView().generic_http as View), response_code_field: "" } }),
      { outcome: "rejected", http_status: 400, provider_code: "" },
      "Rejected by the gateway · HTTP 400. Set “Error code field” to see the gateway's own code.",
    ],
    ["a redirect", gatewayView(), { outcome: "rejected", http_status: 302, provider_code: "" }, "Rejected · HTTP 302. A redirect is not followed; put the final URL in the settings."],
    [
      "unreachable with its transport class",
      twilioView(),
      { outcome: "unreachable", http_status: 0, provider_code: "", transport_class: "DNS lookup failed" },
      "No answer from the provider (DNS lookup failed). The message may or may not have been sent.",
    ],
    ["unreachable without a class", twilioView(), { outcome: "unreachable", http_status: 0, provider_code: "" }, "No answer from the provider. The message may or may not have been sent."],
  ];
  for (const [name, v, answer, line] of rows) {
    it(`answer: ${name}`, async () => {
      telephonyView = v;
      apiMock.POST.mockResolvedValue(ok(answer));
      const w = await mountPanel();
      await tid(w, "telephony-test-send").trigger("click");
      await flushPromises();
      const got = text(w, "telephony-test-result");
      expect(got).toBe(line);
      expect(got).not.toContain("5 s");
      expect(got).not.toContain("within");
      expect(text(w, "telephony-test-send")).toBe("Send test SMS to +90•••••6869");
    });
  }

  it("a 409 telephony_disabled reads the 'off' pre-state and re-reads /me", async () => {
    apiMock.POST.mockResolvedValue(fail(409, { error: "phone alerts are turned off on this instance", reason: "telephony_disabled" }));
    const w = await mountPanel();
    await tid(w, "telephony-test-send").trigger("click");
    await flushPromises();
    expect(text(w, "telephony-test-note")).toBe("Turn phone alerts on and save to send a test.");
    expect(session().fetchMe).toHaveBeenCalled();
  });

  it("a 409 no_verified_number reads the 'no number' pre-state", async () => {
    apiMock.POST.mockResolvedValue(fail(409, { error: "verify your own phone number in your profile first; the test message goes only there", reason: "no_verified_number" }));
    const w = await mountPanel();
    await tid(w, "telephony-test-send").trigger("click");
    await flushPromises();
    expect(text(w, "telephony-test-note")).toBe("You have no verified number. Add one under Account → Phone.");
    expect(session().fetchMe).toHaveBeenCalled();
  });

  it("a 503 shows the server's sentence under the action", async () => {
    apiMock.POST.mockResolvedValue(fail(503, { error: "the stored telephony credential cannot be decrypted with the configured keys" }));
    const w = await mountPanel();
    await tid(w, "telephony-test-send").trigger("click");
    await flushPromises();
    expect(text(w, "telephony-test-error")).toBe("the stored telephony credential cannot be decrypted with the configured keys");
  });
});

describe("TelephonyPanel · after a save", () => {
  it("re-reads /me, shows '✓ Saved' and takes the server's view", async () => {
    apiMock.PUT.mockResolvedValue(ok(twilioView({ twilio: { account_sid: "AC7f3e0c1b9a4d6e2f8a0b3c5d7e9f1a2b", auth_token_set: true, from: "+15550199" } })));
    const w = await mountPanel();
    await tid(w, "twilio-from").setValue("+15550199");
    await save(w);
    expect(session().fetchMe).toHaveBeenCalledTimes(1);
    expect(text(w, "telephony-saved")).toBe("✓ Saved");
    // the form equals the saved view again, so the test is offered
    expect(tid(w, "telephony-test-send").attributes("disabled")).toBeUndefined();
  });

  it("a refused save does not re-read /me", async () => {
    apiMock.PUT.mockResolvedValue(fail(400, { error: "nope" }));
    const w = await mountPanel();
    await save(w);
    expect(session().fetchMe).not.toHaveBeenCalled();
  });
});
