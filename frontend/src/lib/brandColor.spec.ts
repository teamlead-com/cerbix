import { describe, expect, it } from "vitest";

import { accentInkFor } from "@/lib/brandColor";

type Ink = "#ffffff" | "#0b0b0f";

function channel(value: number): number {
  const normalized = value / 255;
  return normalized <= 0.04045
    ? normalized / 12.92
    : Math.pow((normalized + 0.055) / 1.055, 2.4);
}

function oracleLuminance(hex: string): number {
  const normalized = hex.trim();
  const rgb = [0, 2, 4].map((offset) => Number.parseInt(normalized.slice(1 + offset, 3 + offset), 16));
  return 0.2126 * channel(rgb[0]) + 0.7152 * channel(rgb[1]) + 0.0722 * channel(rgb[2]);
}

function contrastRatio(first: number, second: number): number {
  return (Math.max(first, second) + 0.05) / (Math.min(first, second) + 0.05);
}

function expectedInk(hex: string): Ink {
  const accent = oracleLuminance(hex);
  const lightContrast = contrastRatio(accent, oracleLuminance("#ffffff"));
  const darkContrast = contrastRatio(accent, oracleLuminance("#0b0b0f"));
  return lightContrast >= darkContrast ? "#ffffff" : "#0b0b0f";
}

describe("accentInkFor", () => {
  it.each([
    ["#000000", "#ffffff"],
    ["#17324D", "#ffffff"],
    ["#5854F2", "#ffffff"],
    ["#FFFFFF", "#0b0b0f"],
    ["#F4E36A", "#0b0b0f"],
  ] as const)("chooses the higher-contrast ink for %s", (accent, expected) => {
    expect(accentInkFor(accent)).toBe(expected);
    expect(accentInkFor(accent)).toBe(expectedInk(accent));
  });

  it("accepts mixed-case hex with surrounding whitespace", () => {
    const accent = "  #f4E36a  ";

    expect(accentInkFor(accent)).toBe(expectedInk(accent));
    expect(accentInkFor(accent)).toBe("#0b0b0f");
  });

  it("uses the independently computed winner at a non-equal contrast boundary", () => {
    const accent = "#777777";
    const accentLuminance = oracleLuminance(accent);
    const lightContrast = contrastRatio(accentLuminance, oracleLuminance("#ffffff"));
    const darkContrast = contrastRatio(accentLuminance, oracleLuminance("#0b0b0f"));

    expect(lightContrast).not.toBe(darkContrast);
    expect(accentInkFor(accent)).toBe(expectedInk(accent));
  });
});
