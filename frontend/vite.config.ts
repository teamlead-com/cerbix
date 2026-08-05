import { fileURLToPath, URL } from "node:url";

import vue from "@vitejs/plugin-vue";
import AutoImport from "unplugin-auto-import/vite";
import Components from "unplugin-vue-components/vite";
import svgLoader from "vite-svg-loader";
import { defineConfig, loadEnv } from "vite";

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd());
  const backend = env.VITE_BACKEND_URL || "http://localhost:8080";

  return {
    plugins: [
      vue(),
      svgLoader({ svgo: false }),
      AutoImport({
        imports: ["vue", "vue-router", "pinia", "@vueuse/core"],
        vueTemplate: true,
        dts: "auto-imports.d.ts",
      }),
      Components({
        dirs: ["./src/components"],
        dts: "components.d.ts",
      }),
    ],
    resolve: {
      alias: {
        "@": fileURLToPath(new URL("src", import.meta.url)),
      },
    },
    server: {
      port: Number(env.VITE_PORT) || 5173,
      host: true,
      // Proxy API + auth to the cerbix backend during local dev.
      proxy: {
        "/api": { target: backend, changeOrigin: true },
        "/auth": { target: backend, changeOrigin: true },
      },
    },
    build: {
      outDir: "dist",
      emptyOutDir: true,
    },
  };
});
