import { defineStore } from "pinia";
import { api } from "@/api/client";
import type { components } from "@/api/schema";

type PublicBranding = components["schemas"]["PublicBranding"];
type Announcement = components["schemas"]["Announcement"];

function hexToRgba(hex: string, alpha: number): string {
  const m = /^#([0-9a-f]{6})$/i.exec(hex.trim());
  if (!m) return "";
  const n = parseInt(m[1], 16);
  return `rgba(${(n >> 16) & 255}, ${(n >> 8) & 255}, ${n & 255}, ${alpha})`;
}

// applyAccent overrides the accent CSS custom properties at runtime.
function applyAccent(color: string) {
  const root = document.documentElement.style;
  if (!/^#[0-9a-f]{6}$/i.test(color)) {
    root.removeProperty("--accent");
    root.removeProperty("--accent-2");
    root.removeProperty("--accent-weak");
    return;
  }
  root.setProperty("--accent", color);
  root.setProperty("--accent-2", color);
  const weak = hexToRgba(color, 0.14);
  if (weak) root.setProperty("--accent-weak", weak);
}

export const useBranding = defineStore("branding", {
  state: () => ({
    productName: "cerbix",
    accentColor: "",
    footerText: "",
    supportUrl: "",
    announcement: { enabled: false, text: "", level: "info" } as Announcement,
    loaded: false,
  }),
  actions: {
    async load() {
      try {
        const res = await api.GET("/api/v1/public/branding");
        const b = (res.data ?? {}) as PublicBranding;
        this.productName = b.product_name?.trim() || "cerbix";
        this.accentColor = b.accent_color || "";
        this.footerText = b.footer_text || "";
        this.supportUrl = b.support_url || "";
        this.announcement = b.announcement ?? { enabled: false, text: "", level: "info" };
        if (this.accentColor) applyAccent(this.accentColor);
        document.title = this.productName;
      } catch {
        /* branding is best-effort; defaults stand */
      } finally {
        this.loaded = true;
      }
    },
  },
});
