import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

import { accentInkFor } from "@/lib/brandColor";

const here = dirname(fileURLToPath(import.meta.url));
const styleSource = readFileSync(resolve(here, "../style.css"), "utf8");

type RGB = { r: number; g: number; b: number; a: number };
type ThemeTokens = Record<string, string>;
type ThemeName = "light" | "system-dark" | "dark";

const requiredTokens = [
  "--ink",
  "--ink-2",
  "--ink-3",
  "--bg",
  "--surface",
  "--surface-2",
  "--inset",
  "--accent",
  "--accent-2",
  "--accent-ink",
  "--up",
  "--down",
  "--degraded",
  "--maint",
  "--pending",
] as const;

const statusNames = ["up", "down", "degraded", "maint", "pending"] as const;
const expectedStatusColors: Record<ThemeName, Record<(typeof statusNames)[number], string>> = {
  light: {
    up: "#12a05c",
    down: "#e0393f",
    degraded: "#b97800",
    maint: "#3a7de5",
    pending: "#8a8a9d",
  },
  "system-dark": {
    up: "#35c67f",
    down: "#ff5f64",
    degraded: "#e0a53a",
    maint: "#5c9bff",
    pending: "#6c6c7e",
  },
  dark: {
    up: "#35c67f",
    down: "#ff5f64",
    degraded: "#e0a53a",
    maint: "#5c9bff",
    pending: "#6c6c7e",
  },
};

function declarations(block: string): ThemeTokens {
  return Object.fromEntries(
    [...block.matchAll(/(--[\w-]+)\s*:\s*([^;]+);/g)].map(([, name, value]) => [
      name,
      value.trim(),
    ]),
  );
}

function themeTokens(source: string): Record<ThemeName, ThemeTokens> {
  const light = source.match(/:root\s*\{([\s\S]*?)\n\}/)?.[1];
  const systemDark = source.match(/:root:not\(\[data-theme="light"\]\)\s*\{([\s\S]*?)\n\}/)?.[1];
  const dark = source.match(/:root\[data-theme="dark"\]\s*\{([\s\S]*?)\n\}/)?.[1];
  if (!light || !systemDark || !dark) throw new Error("Could not find all theme token blocks");
  return {
    light: declarations(light),
    "system-dark": declarations(systemDark),
    dark: declarations(dark),
  };
}

function parseColor(value: string): RGB {
  const hex = value.match(/^#([\da-f]{6})$/i);
  if (hex) {
    return {
      r: parseInt(hex[1].slice(0, 2), 16) / 255,
      g: parseInt(hex[1].slice(2, 4), 16) / 255,
      b: parseInt(hex[1].slice(4, 6), 16) / 255,
      a: 1,
    };
  }

  const rgba = value.match(/^rgba\(\s*([\d.]+)\s*,\s*([\d.]+)\s*,\s*([\d.]+)\s*,\s*([\d.]+)\s*\)$/i);
  if (rgba) {
    return {
      r: Number(rgba[1]) / 255,
      g: Number(rgba[2]) / 255,
      b: Number(rgba[3]) / 255,
      a: Number(rgba[4]),
    };
  }

  throw new Error(`Unsupported color token: ${value}`);
}

function composite(foreground: RGB, background: RGB): RGB {
  return {
    r: foreground.r * foreground.a + background.r * (1 - foreground.a),
    g: foreground.g * foreground.a + background.g * (1 - foreground.a),
    b: foreground.b * foreground.a + background.b * (1 - foreground.a),
    a: 1,
  };
}

function relativeLuminance(color: RGB): number {
  const linear = (channel: number) =>
    channel <= 0.04045
      ? channel / 12.92
      : ((channel + 0.055) / 1.055) ** 2.4;
  return 0.2126 * linear(color.r) + 0.7152 * linear(color.g) + 0.0722 * linear(color.b);
}

function contrastRatio(first: RGB, second: RGB): number {
  const a = relativeLuminance(first);
  const b = relativeLuminance(second);
  return (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
}

function colorContrast(first: string, second: string): number {
  return contrastRatio(parseColor(first), parseColor(second));
}

function tagContaining(source: string, text: string): string {
  const tags = tagsContaining(source, text);
  if (tags.length !== 1) {
    throw new Error(`Expected one containing tag for ${text}, found ${tags.length}`);
  }
  return tags[0];
}

function tagsContaining(source: string, text: string): string[] {
  const tags: string[] = [];
  let from = 0;
  while (from < source.length) {
    const index = source.indexOf(text, from);
    if (index < 0) break;
    const start = source.lastIndexOf("<", index);
    const end = source.indexOf(">", index);
    if (start < 0 || end < 0) throw new Error(`Could not find containing tag for ${text}`);
    tags.push(source.slice(start, end + 1));
    from = index + text.length;
  }
  return tags;
}

function sourceFile(name: string): string {
  return readFileSync(resolve(here, "../components", name), "utf8");
}

function viewFile(name: string): string {
  return readFileSync(resolve(here, "../views", name), "utf8");
}

const themes = themeTokens(styleSource);

describe("semantic token contrast matrix", () => {
  it("parses every required token from both default theme declarations", () => {
    for (const theme of Object.values(themes)) {
      for (const token of requiredTokens) {
        expect(theme[token], `${token} is missing`).toBeTruthy();
      }
    }
  });

  it("keeps primary and secondary meaningful text at the small-text target", () => {
    for (const theme of Object.values(themes)) {
      for (const foreground of ["--ink", "--ink-2"]) {
        for (const background of ["--bg", "--surface", "--surface-2"]) {
          expect(
            colorContrast(theme[foreground], theme[background]),
            `${foreground} on ${background}`,
          ).toBeGreaterThanOrEqual(4.5);
        }
      }
    }
  });

  it("keeps tertiary metadata at the lower metadata target without making it a primary role", () => {
    for (const theme of Object.values(themes)) {
      for (const background of ["--bg", "--surface", "--surface-2"]) {
        expect(
          colorContrast(theme["--ink-3"], theme[background]),
          `--ink-3 on ${background}`,
        ).toBeGreaterThanOrEqual(3);
      }
    }
  });

  it("keeps default action foreground and background pairs readable", () => {
    for (const theme of Object.values(themes)) {
      for (const background of ["--accent", "--accent-2"]) {
        expect(
          colorContrast(theme["--accent-ink"], theme[background]),
          `--accent-ink on ${background}`,
        ).toBeGreaterThanOrEqual(4.5);
      }
    }
  });

  it("keeps the light secondary iris distinct from primary iris while preserving action contrast", () => {
    const light = themes.light;
    expect(light["--accent-2"]).not.toBe(light["--accent"]);
    expect(colorContrast(light["--accent-ink"], light["--accent-2"])).toBeGreaterThanOrEqual(4.5);
  });

  it("uses the helper-selected ink for every reviewed custom accent example", () => {
    const examples = [
      ["#000000", "#ffffff"],
      ["#17324D", "#ffffff"],
      ["#5854F2", "#ffffff"],
      ["#FFFFFF", "#0b0b0f"],
      ["#F4E36A", "#0b0b0f"],
    ] as const;

    for (const [accent, expectedInk] of examples) {
      expect(accentInkFor(accent), `ink for ${accent}`).toBe(expectedInk);
      expect(colorContrast(expectedInk, accent), `contrast for ${accent}`).toBeGreaterThanOrEqual(4.5);
    }
  });

  it("keeps each status foreground in its own hue and readable on its composited weak surface", () => {
    for (const themeName of ["light", "system-dark", "dark"] as const) {
      const theme = themes[themeName];
      const surface = parseColor(theme["--surface"]);
      for (const status of statusNames) {
        const foreground = theme[`--${status}`];
        const weak = composite(parseColor(theme[`--${status}-weak`]), surface);
        expect(foreground, `--${status} foreground in ${themeName}`).toBe(expectedStatusColors[themeName][status]);
        expect(
          contrastRatio(parseColor(foreground), weak),
          `--${status} on --${status}-weak over --surface`,
        ).toBeGreaterThanOrEqual(3);
      }
    }
  });

  it("keeps the focus indicator at 3:1 against every default surrounding surface", () => {
    for (const theme of Object.values(themes)) {
      for (const background of ["--bg", "--surface", "--surface-2", "--inset"]) {
        expect(
          colorContrast(theme["--focus"], theme[background]),
          `--focus on ${background}`,
        ).toBeGreaterThanOrEqual(3);
      }
    }
  });
});

describe("semantic text roles in operating surfaces", () => {
  it("uses a reviewed role for breadcrumb context and account role labels", () => {
    const source = sourceFile("AppShell.vue");
    const breadcrumb = tagContaining(source, '<nav aria-label="Breadcrumb"');
    const adminLabel = tagContaining(source, ">Global admin</div>");

    expect(breadcrumb).toContain("text-ink-2");
    expect(breadcrumb).not.toContain("text-ink-3");
    expect(adminLabel).toContain("text-ink-2");
    expect(adminLabel).not.toContain("text-ink-3");
  });

  it("uses a reviewed role for every workspace scope, transition, and empty state", () => {
    const source = sourceFile("WorkspaceSwitcher.vue");
    const loadingNodes = tagsContaining(source, "Loading projects…");
    expect(loadingNodes).toHaveLength(2);
    for (const tag of loadingNodes) {
      expect(tag).toContain("text-ink-2");
      expect(tag).not.toContain("text-ink-3");
    }
    for (const text of [
      '{{ ws.projectName || "organization" }}',
      ">Organization</div>",
      ">Project</div>",
      "No projects in this organization.",
    ]) {
      const tag = tagContaining(source, text);
      expect(tag, text).toContain("text-ink-2");
      expect(tag, text).not.toContain("text-ink-3");
    }
  });

  it("uses a reviewed role for dialog explanation and field labels", () => {
    const source = sourceFile("CreateDialog.vue");
    for (const text of [
      "id=\"create-dialog-description\"",
      ">Organization</span>",
      ">Name</span>",
      "Slug <span",
      "— used in URLs, lowercase",
    ]) {
      const tag = tagContaining(source, text);
      expect(tag, text).toContain("text-ink-2");
      expect(tag, text).not.toContain("text-ink-3");
    }
  });

  it("uses a reviewed role for search loading, no-match, and error states", () => {
    const source = sourceFile("SearchBox.vue");
    for (const text of [
      ">Searching…</p>",
      ">{{ error }}</p>",
      ">No matches for “{{ q.trim() }}”.</p>",
    ]) {
      const tag = tagContaining(source, text);
      expect(tag, text).toContain("text-ink-2");
      expect(tag, text).not.toContain("text-ink-3");
    }
  });

  it("uses a reviewed role for dashboard scope, explanatory, loading, and no-data labels", () => {
    const source = viewFile("DashboardView.vue");
    for (const text of [
      '<p class="mt-[3px] text-[13px] text-ink-2">',
      ">Loading availability…</p>",
      ">{{ empty }}</p>",
      "Ask a global admin to create an organization.",
      "Ask an org admin to create a project in {{ ws.orgName }}.",
      "Ask an editor or admin to create the first monitor.",
      ">Project availability · 90 days</span",
      'class="mt-[10px] flex gap-4 text-[12px] text-ink-2"',
    ]) {
      const tag = tagContaining(source, text);
      expect(tag, text).toContain("text-ink-2");
      expect(tag, text).not.toContain("text-ink-3");
    }
  });

  it("uses a reviewed role for KPI labels and explanatory subtext", () => {
    const source = sourceFile("Kpi.vue");
    const label = tagContaining(source, "{{ label }}");
    const subtext = tagContaining(source, 'class="flex items-center gap-[5px] text-xs');
    expect(label).toContain("text-ink-2");
    expect(label).not.toContain("text-ink-3");
    expect(subtext).toContain("text-ink-2");
    expect(subtext).not.toContain("text-ink-3");
    expect(source).toMatch(/dir === "neg"\s*\?\s*"text-down"\s*:\s*"text-ink-2"/);
  });
});

describe("focus and reduced-motion CSS contract", () => {
  it("cannot let outline-none remove the global focus-visible indicator", () => {
    expect(styleSource).toMatch(
      /:focus-visible\s*\{[^}]*outline:\s*2px solid var\(--focus\)\s*!important;/s,
    );
  });

  it("stops decorative transitions and animations under reduced motion", () => {
    expect(styleSource).toMatch(/@media\s*\(prefers-reduced-motion:\s*reduce\)/);
    expect(styleSource).toMatch(/\.transition[\s\S]*\.animate-pulse[\s\S]*animation:\s*none\s*!important/s);
  });
});
