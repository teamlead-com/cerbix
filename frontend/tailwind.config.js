/** @type {import('tailwindcss').Config} */
export default {
  // Theme flips via CSS custom properties (prefers-color-scheme + [data-theme]);
  // this enables `dark:` utilities to also key off the data attribute.
  darkMode: ["selector", '[data-theme="dark"]'],
  content: ["./index.html", "./src/**/*.{js,ts,jsx,tsx,vue}"],
  theme: {
    extend: {
      colors: {
        // Semantic tokens — resolved from CSS variables in src/style.css so a
        // single set of Tailwind classes works in both themes.
        bg: "var(--bg)",
        surface: {
          DEFAULT: "var(--surface)",
          2: "var(--surface-2)",
        },
        inset: "var(--inset)",
        border: {
          DEFAULT: "var(--border)",
          strong: "var(--border-strong)",
        },
        ink: {
          DEFAULT: "var(--ink)",
          2: "var(--ink-2)",
          3: "var(--ink-3)",
        },
        accent: {
          DEFAULT: "var(--accent)",
          2: "var(--accent-2)",
          ink: "var(--accent-ink)",
          weak: "var(--accent-weak)",
        },
        up: { DEFAULT: "var(--up)", weak: "var(--up-weak)" },
        down: { DEFAULT: "var(--down)", weak: "var(--down-weak)" },
        degraded: { DEFAULT: "var(--degraded)", weak: "var(--degraded-weak)" },
        maint: { DEFAULT: "var(--maint)", weak: "var(--maint-weak)" },
        pending: { DEFAULT: "var(--pending)", weak: "var(--pending-weak)" },
      },
      fontFamily: {
        sans: [
          "-apple-system", "BlinkMacSystemFont", "Segoe UI", "Roboto",
          "Helvetica Neue", "Arial", "sans-serif",
        ],
        mono: [
          "ui-monospace", "SF Mono", "JetBrains Mono", "Menlo", "Consolas", "monospace",
        ],
      },
      borderRadius: {
        DEFAULT: "8px", // card/panel corners (matches `rounded-lg` on the status page)
        sm: "6px",
        xs: "4px",
      },
      boxShadow: {
        card: "var(--shadow)",
      },
    },
  },
  plugins: [],
};
