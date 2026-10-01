import { h, nextTick } from "vue";
import { mount } from "@vue/test-utils";
import { afterEach, describe, expect, it, vi } from "vitest";

import OverlaySurface from "@/components/OverlaySurface.vue";

const mounted: Array<{ unmount: () => void }> = [];

function addOpener() {
  const opener = document.createElement("button");
  opener.type = "button";
  opener.textContent = "Open overlay";
  document.body.append(opener);
  opener.focus();
  return opener;
}

function mountSurface(props: Record<string, unknown> = {}, content?: () => unknown) {
  const wrapper = mount(OverlaySurface, {
    attachTo: document.body,
    props: {
      open: true,
      role: "dialog",
      modal: true,
      labelledby: "overlay-title",
      describedby: "overlay-description",
      ...props,
    },
    slots: {
      default: content ?? (() => [
        h("h2", { id: "overlay-title" }, "Overlay title"),
        h("p", { id: "overlay-description" }, "Overlay description"),
        h("button", { type: "button" }, "First action"),
        h("button", { type: "button" }, "Last action"),
      ]),
    },
  });
  mounted.push(wrapper);
  return wrapper;
}

afterEach(() => {
  mounted.splice(0).forEach((wrapper) => wrapper.unmount());
  document.body.innerHTML = "";
  vi.restoreAllMocks();
});

describe("OverlaySurface", () => {
  it("exposes dialog ARIA relationships and focuses the first meaningful control", async () => {
    addOpener();
    const wrapper = mountSurface();
    await nextTick();

    const dialog = wrapper.get('[role="dialog"]');
    expect(dialog.attributes("aria-modal")).toBe("true");
    expect(dialog.attributes("aria-labelledby")).toBe("overlay-title");
    expect(dialog.attributes("aria-describedby")).toBe("overlay-description");
    expect(document.activeElement).toBe(wrapper.findAll("button")[0].element);
  });

  it("closes once on Escape and restores focus when the parent closes it", async () => {
    const opener = addOpener();
    const wrapper = mountSurface();
    await nextTick();

    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    expect(wrapper.emitted("close")).toHaveLength(1);

    await wrapper.setProps({ open: false });
    expect(document.activeElement).toBe(opener);

    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    expect(wrapper.emitted("close")).toHaveLength(1);
  });

  it("wraps Tab within modal focusables in both directions", async () => {
    addOpener();
    const wrapper = mountSurface();
    await nextTick();
    const buttons = wrapper.findAll("button");
    const first = buttons[0].element;
    const last = buttons[1].element;

    last.focus();
    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Tab", bubbles: true }));
    expect(document.activeElement).toBe(first);

    first.focus();
    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Tab", shiftKey: true, bubbles: true }));
    expect(document.activeElement).toBe(last);
  });

  it("does not trap or redirect focus for non-modal menus", async () => {
    addOpener();
    const background = document.createElement("button");
    background.type = "button";
    background.textContent = "Background action";
    document.body.append(background);

    const wrapper = mountSurface({ role: "menu", modal: false });
    await nextTick();
    const menuButton = wrapper.findAll("button")[1].element;
    menuButton.focus();

    const tabEvent = new KeyboardEvent("keydown", { key: "Tab", bubbles: true, cancelable: true });
    menuButton.dispatchEvent(tabEvent);
    background.focus();

    expect(tabEvent.defaultPrevented).toBe(false);
    expect(document.activeElement).toBe(background);
    expect(wrapper.get('[role="menu"]').attributes("aria-modal")).toBeUndefined();
  });

  it("closes on outside activation only when configured", async () => {
    addOpener();
    const wrapper = mountSurface({ closeOnOutside: false });
    await nextTick();

    document.body.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    expect(wrapper.emitted("close")).toBeUndefined();

    await wrapper.setProps({ closeOnOutside: true });
    document.body.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    expect(wrapper.emitted("close")).toHaveLength(1);
  });

  it("keeps pointer focus inside a modal and blocks outside activation", async () => {
    addOpener();
    const background = document.createElement("button");
    const activated = vi.fn();
    background.type = "button";
    background.textContent = "Background action";
    background.addEventListener("click", activated);
    background.addEventListener("mousedown", () => background.focus());
    document.body.append(background);

    const wrapper = mountSurface({ closeOnOutside: false });
    await nextTick();
    const first = wrapper.findAll("button")[0].element;

    background.dispatchEvent(new MouseEvent("mousedown", { bubbles: true, cancelable: true }));
    background.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true }));

    expect(document.activeElement).toBe(first);
    expect(activated).not.toHaveBeenCalled();
  });

  it("returns programmatic focus attempts to the modal surface", async () => {
    addOpener();
    const background = document.createElement("button");
    background.type = "button";
    background.textContent = "Background action";
    document.body.append(background);

    const wrapper = mountSurface({ closeOnOutside: false });
    await nextTick();
    const first = wrapper.findAll("button")[0].element;

    background.focus();

    expect(document.activeElement).toBe(first);
  });

  it("ignores hidden, disabled, aria-hidden, and negative-tabindex controls", async () => {
    const wrapper = mountSurface({}, () => h("div", [
      h("button", { id: "display-none", style: "display: none" }, "Display none"),
      h("button", { id: "hidden", hidden: true }, "Hidden"),
      h("button", { id: "negative", tabindex: "-2" }, "Negative tabindex"),
      h("div", { "aria-hidden": "true" }, [h("button", { id: "aria-hidden" }, "Aria hidden")]),
      h("fieldset", { disabled: true }, [h("button", { id: "fieldset-disabled" }, "Fieldset disabled")]),
      h("button", { id: "first-visible" }, "First visible"),
      h("button", { id: "last-visible" }, "Last visible"),
    ]));
    await nextTick();

    const first = wrapper.get("#first-visible").element;
    const last = wrapper.get("#last-visible").element;
    expect(document.activeElement).toBe(first);

    last.focus();
    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Tab", bubbles: true }));
    expect(document.activeElement).toBe(first);
  });

  it("closes only the topmost overlay on Escape", async () => {
    addOpener();
    const first = mountSurface();
    await nextTick();
    const second = mountSurface();
    await nextTick();

    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    expect(first.emitted("close")).toBeUndefined();
    expect(second.emitted("close")).toHaveLength(1);

    await second.setProps({ open: false });
    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    expect(first.emitted("close")).toHaveLength(1);
  });

  it("keeps a modal drawer containing focus when a nested non-modal workspace menu is topmost", async () => {
    addOpener();
    const drawer = mountSurface({}, () => [
      h("button", { id: "drawer-action", type: "button" }, "Drawer action"),
      h(
        OverlaySurface,
        {
          open: true,
          id: "workspace-switcher-menu",
          role: "menu",
          modal: false,
          closeOnOutside: true,
          labelledby: "workspace-switcher-trigger",
        },
        {
          default: () => h("button", { id: "workspace-project", type: "button" }, "Project"),
        },
      ),
    ]);
    await nextTick();
    await nextTick();

    const outside = document.createElement("button");
    outside.type = "button";
    outside.textContent = "Outside";
    document.body.append(outside);
    outside.focus();

    expect(drawer.get('[role="dialog"]').element.contains(document.activeElement)).toBe(true);

    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    const overlays = drawer.findAllComponents(OverlaySurface);
    expect(overlays[0].emitted("close")).toHaveLength(1);
    expect(drawer.emitted("close")).toBeUndefined();
  });

  it("registers and removes all capture listeners on close and unmount", async () => {
    addOpener();
    const addSpy = vi.spyOn(document, "addEventListener");
    const removeSpy = vi.spyOn(document, "removeEventListener");
    const wrapper = mountSurface();
    await nextTick();

    expect(addSpy).toHaveBeenCalledWith("keydown", expect.any(Function), true);
    expect(addSpy).toHaveBeenCalledWith("focusin", expect.any(Function), true);
    expect(addSpy).toHaveBeenCalledWith("click", expect.any(Function), true);

    await wrapper.setProps({ open: false });
    expect(removeSpy).toHaveBeenCalledWith("keydown", expect.any(Function), true);
    expect(removeSpy).toHaveBeenCalledWith("focusin", expect.any(Function), true);
    expect(removeSpy).toHaveBeenCalledWith("click", expect.any(Function), true);

    removeSpy.mockClear();
    const second = mountSurface();
    await nextTick();
    second.unmount();
    expect(removeSpy).toHaveBeenCalledWith("keydown", expect.any(Function), true);
    expect(removeSpy).toHaveBeenCalledWith("focusin", expect.any(Function), true);
    expect(removeSpy).toHaveBeenCalledWith("click", expect.any(Function), true);
  });

  it("restores opener focus when unmounted while open", async () => {
    const opener = addOpener();
    const wrapper = mountSurface();
    await nextTick();

    wrapper.unmount();

    expect(document.activeElement).toBe(opener);
  });

  it("still renders and responds to close while reduced motion is preferred", async () => {
    Object.defineProperty(window, "matchMedia", {
      configurable: true,
      value: vi.fn(() => ({ matches: true, addEventListener: vi.fn(), removeEventListener: vi.fn() })),
    });
    addOpener();
    const wrapper = mountSurface();
    await nextTick();

    expect(wrapper.get('[role="dialog"]').exists()).toBe(true);
    expect(wrapper.get('[role="dialog"]').classes()).toContain("motion-reduce:transition-none");

    await wrapper.setProps({ open: false });
    expect(wrapper.find('[role="dialog"]').exists()).toBe(false);
  });
});
