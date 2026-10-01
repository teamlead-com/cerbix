function channel(value: number): number {
  const normalized = value / 255;
  return normalized <= 0.04045
    ? normalized / 12.92
    : Math.pow((normalized + 0.055) / 1.055, 2.4);
}

function luminance(hex: string): number {
  const normalized = hex.trim();
  const rgb = [0, 2, 4].map((offset) => Number.parseInt(normalized.slice(1 + offset, 3 + offset), 16));
  return 0.2126 * channel(rgb[0]) + 0.7152 * channel(rgb[1]) + 0.0722 * channel(rgb[2]);
}

function contrastRatio(first: number, second: number): number {
  return (Math.max(first, second) + 0.05) / (Math.min(first, second) + 0.05);
}

export function accentInkFor(hex: string): "#ffffff" | "#0b0b0f" {
  const accent = luminance(hex);
  const lightContrast = contrastRatio(accent, 1);
  const darkContrast = contrastRatio(accent, luminance("#0b0b0f"));
  return lightContrast >= darkContrast ? "#ffffff" : "#0b0b0f";
}
