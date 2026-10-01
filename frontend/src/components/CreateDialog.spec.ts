import { flushPromises, mount } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { beforeEach, describe, expect, it, vi } from "vitest";

import CreateDialog from "@/components/CreateDialog.vue";
import { useUi } from "@/stores/ui";
import { useWorkspace } from "@/stores/workspace";

const routerPush = vi.hoisted(() => vi.fn());
vi.mock("vue-router", () => ({ useRouter: () => ({ push: routerPush }) }));

const mounted: Array<{ unmount: () => void }> = [];

function setup() {
  const pinia = createPinia();
  setActivePinia(pinia);
  const ui = useUi();
  const ws = useWorkspace();
  ws.$patch({ orgs: [{ id: "org-1", name: "Acme" }], orgId: "org-1" });
  ui.openCreate("project");

  const opener = document.createElement("button");
  opener.type = "button";
  opener.textContent = "New project";
  document.body.append(opener);
  opener.focus();

  const wrapper = mount(CreateDialog, { attachTo: document.body, global: { plugins: [pinia] } });
  mounted.push(wrapper);
  return { opener, pinia, ui, ws, wrapper };
}

beforeEach(() => {
  mounted.splice(0).forEach((wrapper) => wrapper.unmount());
  document.body.innerHTML = "";
  routerPush.mockReset();
});

describe("CreateDialog", () => {
  it("provides a named project dialog and focuses Name first", async () => {
    const { wrapper } = setup();
    await flushPromises();

    const dialog = wrapper.get('[role="dialog"]');
    expect(dialog.attributes("aria-modal")).toBe("true");
    expect(dialog.attributes("aria-labelledby")).toBe("create-dialog-title");
    expect(dialog.attributes("aria-describedby")).toBe("create-dialog-description");
    expect(wrapper.get("#create-dialog-title").text()).toBe("New project");
    expect(wrapper.get("#create-dialog-description").text()).toContain("A team or app inside Acme");
    expect(document.activeElement).toBe(wrapper.get('input[placeholder="API"]').element);
  });

  it("closes on Escape and outside activation, restoring opener focus", async () => {
    const first = setup();
    await flushPromises();
    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    await flushPromises();
    expect(first.ui.createKind).toBe("");
    expect(document.activeElement).toBe(first.opener);

    const second = setup();
    await flushPromises();
    document.body.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    await flushPromises();
    expect(second.ui.createKind).toBe("");
    expect(document.activeElement).toBe(second.opener);
  });

  it("keeps visible focus indicators on editable controls", async () => {
    const { wrapper } = setup();
    await flushPromises();

    for (const control of wrapper.findAll("input, button")) {
      expect(control.classes().join(" ")).toContain("focus-visible");
    }
  });
});
