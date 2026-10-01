<script lang="ts">
type OverlayController = {
  keydown: (event: KeyboardEvent) => void;
  focusin: (event: FocusEvent) => void;
  click: (event: MouseEvent) => void;
  getSurface: () => HTMLElement | null;
};

const openOverlays: OverlayController[] = [];
</script>

<script setup lang="ts">
import { nextTick, onBeforeUnmount, ref, watch, type Ref } from "vue";

type InitialFocusTarget = string | HTMLElement | Ref<HTMLElement | null> | null;

const focusableSelector = [
  "button",
  'input:not([type="hidden"])',
  "select",
  "textarea",
  "a[href]",
  '[contenteditable="true"]',
  "[tabindex]",
].join(",");

const props = withDefaults(
  defineProps<{
    open: boolean;
    id?: string;
    role?: "menu" | "dialog";
    modal?: boolean;
    labelledby?: string;
    describedby?: string;
    closeOnOutside?: boolean;
    initialFocus?: InitialFocusTarget;
  }>(),
  {
    role: "dialog",
    modal: true,
    closeOnOutside: false,
    initialFocus: null,
  },
);

const emit = defineEmits<{
  close: [];
}>();

const surface = ref<HTMLElement | null>(null);
let opener: HTMLElement | null = null;
let closeRequested = false;

const controller: OverlayController = {
  keydown: onDocumentKeydown,
  focusin: onDocumentFocusin,
  click: onDocumentClick,
  getSurface: () => surface.value,
};

function isTopmost() {
  return openOverlays[openOverlays.length - 1] === controller;
}

function containsTopmostOverlay() {
  if (!props.open || !props.modal || !surface.value) return false;
  if (isTopmost()) return true;
  const topmost = openOverlays[openOverlays.length - 1];
  const topmostSurface = topmost?.getSurface();
  return !!topmostSurface && surface.value.contains(topmostSurface);
}

function addListeners() {
  if (openOverlays.includes(controller)) return;
  openOverlays.push(controller);
  document.addEventListener("keydown", controller.keydown, true);
  document.addEventListener("focusin", controller.focusin, true);
  document.addEventListener("click", controller.click, true);
}

function removeListeners() {
  const index = openOverlays.indexOf(controller);
  if (index !== -1) openOverlays.splice(index, 1);
  document.removeEventListener("keydown", controller.keydown, true);
  document.removeEventListener("focusin", controller.focusin, true);
  document.removeEventListener("click", controller.click, true);
}

function requestClose() {
  if (closeRequested) return;
  closeRequested = true;
  emit("close");
}

function isHidden(element: HTMLElement) {
  let current: HTMLElement | null = element;
  while (current) {
    if (
      current.hidden ||
      current.hasAttribute("inert") ||
      current.getAttribute("aria-hidden") === "true"
    ) {
      return true;
    }
    const style = window.getComputedStyle(current);
    if (style.display === "none" || style.visibility === "hidden" || style.visibility === "collapse") {
      return true;
    }
    if (current === surface.value) break;
    current = current.parentElement;
  }
  return false;
}

function isKeyboardFocusable(element: HTMLElement) {
  if (element.matches(":disabled") || element.closest("fieldset[disabled]")) return false;
  if (isHidden(element)) return false;
  const tabindex = element.getAttribute("tabindex");
  if (tabindex !== null && Number.parseInt(tabindex, 10) < 0) return false;
  return element.tabIndex >= 0;
}

function focusableElements() {
  return Array.from(surface.value?.querySelectorAll<HTMLElement>(focusableSelector) ?? []).filter(
    isKeyboardFocusable,
  );
}

function resolveInitialFocus() {
  const target = props.initialFocus;
  if (!target) return null;

  let element: HTMLElement | null = null;
  if (typeof target === "string") {
    try {
      element = surface.value?.querySelector<HTMLElement>(target) ?? null;
    } catch {
      element = null;
    }
  } else if (target instanceof HTMLElement) {
    element = target;
  } else {
    element = target.value;
  }

  return element && surface.value?.contains(element) ? element : null;
}

async function focusInitialTarget() {
  await nextTick();
  if (!props.open || !surface.value) return;
  const target = resolveInitialFocus() ?? focusableElements()[0] ?? surface.value;
  target.focus();
}

function focusForTab(event: KeyboardEvent) {
  if (!props.modal || event.key !== "Tab" || !containsTopmostOverlay()) return;

  const focusables = focusableElements();
  if (!focusables.length) {
    event.preventDefault();
    surface.value?.focus();
    return;
  }

  const active = document.activeElement;
  const first = focusables[0];
  const last = focusables[focusables.length - 1];
  if (event.shiftKey && (active === first || !surface.value?.contains(active))) {
    event.preventDefault();
    last.focus();
  } else if (!event.shiftKey && (active === last || !surface.value?.contains(active))) {
    event.preventDefault();
    first.focus();
  }
}

function onDocumentKeydown(event: KeyboardEvent) {
  if (!props.open) return;
  if (event.key === "Escape") {
    if (!isTopmost()) return;
    event.preventDefault();
    event.stopImmediatePropagation();
    requestClose();
    return;
  }
  focusForTab(event);
}

function onDocumentFocusin(event: FocusEvent) {
  if (!containsTopmostOverlay() || !surface.value) return;
  const target = event.target;
  if (target instanceof Node && surface.value.contains(target)) return;
  const focusables = focusableElements();
  (focusables[0] ?? surface.value).focus();
}

function onDocumentClick(event: MouseEvent) {
  if (!props.open || !isTopmost() || !surface.value) return;
  const target = event.target;
  if (target instanceof Node && surface.value.contains(target)) return;
  if (!props.closeOnOutside && !props.modal) return;

  event.preventDefault();
  event.stopImmediatePropagation();
  if (props.closeOnOutside) requestClose();
}

function captureOpener() {
  const active = document.activeElement;
  opener = active instanceof HTMLElement && active !== document.body ? active : null;
}

function restoreOpenerFocus() {
  if (opener?.isConnected) opener.focus();
  opener = null;
}

watch(
  () => props.open,
  (open) => {
    if (open) {
      closeRequested = false;
      captureOpener();
      addListeners();
      void focusInitialTarget();
    } else {
      removeListeners();
      closeRequested = false;
      restoreOpenerFocus();
    }
  },
  { immediate: true },
);

onBeforeUnmount(() => {
  removeListeners();
  if (props.open) restoreOpenerFocus();
});
</script>

<template>
  <div
    v-if="props.open"
    data-overlay-backdrop
    class="motion-safe:transition-opacity motion-reduce:transition-none"
  >
    <div
      ref="surface"
      :id="props.id || undefined"
      class="motion-safe:transition-transform motion-reduce:transition-none"
      :role="props.role"
      :aria-modal="props.modal ? 'true' : undefined"
      :aria-labelledby="props.labelledby || undefined"
      :aria-describedby="props.describedby || undefined"
      tabindex="-1"
    >
      <slot />
    </div>
  </div>
</template>
