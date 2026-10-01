import { createPinia, setActivePinia } from "pinia";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { useBranding } from "@/stores/branding";

const apiMock = vi.hoisted(() => ({ GET: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));

const BRAND_PROPERTIES = [
  "--accent",
  "--accent-2",
  "--accent-weak",
  "--accent-ink",
] as const;

function seedBrandProperties() {
  const style = document.documentElement.style;
  style.setProperty("--accent", "#111111");
  style.setProperty("--accent-2", "#222222");
  style.setProperty("--accent-weak", "rgba(17, 17, 17, 0.14)");
  style.setProperty("--accent-ink", "#ffffff");
  style.setProperty("--up", "#12A05C");
}

function loadBranding(accentColor: string) {
  apiMock.GET.mockResolvedValue({
    data: {
      product_name: "cerbix",
      accent_color: accentColor,
      logo_url: "",
      footer_text: "",
      support_url: "",
      announcement: { enabled: false, text: "", level: "info" },
    },
  });
  return useBranding().load();
}

describe("branding accent CSS ownership", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    for (const property of BRAND_PROPERTIES) {
      document.documentElement.style.removeProperty(property);
    }
    document.documentElement.style.removeProperty("--up");
    apiMock.GET.mockReset();
  });

  it("sets all four brand properties and preserves operational properties for a valid accent", async () => {
    seedBrandProperties();

    await loadBranding("#F4E36A");

    const style = document.documentElement.style;
    expect(style.getPropertyValue("--accent")).toBe("#F4E36A");
    expect(style.getPropertyValue("--accent-2")).toBe("#F4E36A");
    expect(style.getPropertyValue("--accent-weak")).toBe(
      "rgba(244, 227, 106, 0.14)",
    );
    expect(style.getPropertyValue("--accent-ink")).toBe("#0b0b0f");
    expect(style.getPropertyValue("--up")).toBe("#12A05C");
  });

  it("clears all four brand properties when the loaded accent is empty", async () => {
    seedBrandProperties();

    await loadBranding("");

    const style = document.documentElement.style;
    for (const property of BRAND_PROPERTIES) {
      expect(style.getPropertyValue(property), property).toBe("");
    }
    expect(style.getPropertyValue("--up")).toBe("#12A05C");
  });

  it("uses the same clear path for an invalid six-digit accent value", async () => {
    seedBrandProperties();

    await loadBranding("#F4E36");

    const style = document.documentElement.style;
    for (const property of BRAND_PROPERTIES) {
      expect(style.getPropertyValue(property), property).toBe("");
    }
    expect(style.getPropertyValue("--up")).toBe("#12A05C");
  });
});
