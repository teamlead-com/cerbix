import { mount } from "@vue/test-utils";
import { describe, expect, it } from "vitest";

import BrandGlyph from "@/components/BrandGlyph.vue";

describe("BrandGlyph", () => {
  it("renders the canonical sealed-C geometry and stroke contract", () => {
    const wrapper = mount(BrandGlyph, { props: { size: 24 } });
    const svg = wrapper.get('[data-brand-glyph="sealed-c"]');

    expect(svg.attributes("viewBox")).toBe("0 0 32 32");
    expect(svg.attributes("aria-hidden")).toBe("true");
    expect(svg.attributes("stroke")).toBe("currentColor");
    expect(svg.attributes("stroke-width")).toBe("2.5");
    expect(svg.attributes("stroke-linecap")).toBe("square");
    expect(svg.attributes("stroke-linejoin")).toBe("round");
    expect(svg.findAll("path").map((path) => path.attributes("d"))).toEqual([
      "M21.4 9.5a8.5 8.5 0 1 0 .1 12.9",
      "M19.9 16h4.9",
    ]);
    expect(svg.attributes("width")).toBe("24");
    expect(svg.attributes("height")).toBe("24");
  });

  it("defaults to a 16px square when no size is supplied", () => {
    const wrapper = mount(BrandGlyph);
    const svg = wrapper.get('[data-brand-glyph="sealed-c"]');

    expect(svg.attributes("width")).toBe("16");
    expect(svg.attributes("height")).toBe("16");
  });
});
