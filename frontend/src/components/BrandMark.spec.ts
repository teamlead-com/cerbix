import { mount } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { describe, expect, it } from "vitest";

import BrandMark from "@/components/BrandMark.vue";
import { useBranding } from "@/stores/branding";

function piniaWithBranding(patch: { logoUrl: string }) {
  const pinia = createPinia();
  setActivePinia(pinia);
  useBranding().$patch(patch);
  return pinia;
}

describe("BrandMark", () => {
  it("prefers a configured custom image over Sealed C", () => {
    const wrapper = mount(BrandMark, {
      global: { plugins: [piniaWithBranding({ logoUrl: "/custom.svg" })] },
    });

    expect(wrapper.get('[data-testid="brand-mark"] img').attributes("src")).toBe(
      "/custom.svg",
    );
    expect(wrapper.find('[data-brand-glyph="sealed-c"]').exists()).toBe(false);
  });

  it("renders exactly the shared fallback glyph", () => {
    const wrapper = mount(BrandMark, {
      global: { plugins: [piniaWithBranding({ logoUrl: "" })] },
    });

    expect(
      wrapper.get('[data-testid="brand-mark"] [data-brand-glyph="sealed-c"]'),
    ).toBeTruthy();
  });
});
