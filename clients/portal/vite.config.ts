/// <reference types="vitest/config" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// The portal is served from a SUB-PATH, not the origin root: component/portal
// mounts it at `/portal/` on the bff's HTTP server, which already owns `/`,
// `/memql/ws`, `/healthz`, `/spaces/...` and friends. `base` has to match, or
// every hashed asset URL the bundle emits ("/assets/index-abc.js") resolves
// against the origin root and 404s.
//
// It is also what makes the SPA fallback coherent: the Go handler returns
// index.html for unknown paths BENEATH /portal/, and index.html's own script
// tag then points back inside that prefix.
const BASE = "/portal/";

export default defineConfig({
  base: BASE,
  plugins: [react(), tailwindcss()],
  build: {
    // Emitted into clients/portal/dist and copied into the image at
    // /app/portal (see the Dockerfile's portal stage). Never committed --
    // .gitignore carries the entry.
    outDir: "dist",
    emptyOutDir: true,
    // Hashed filenames under assets/ are what earns the immutable
    // Cache-Control header the Go handler sets on that prefix. Vite's default
    // already does this; stated explicitly because the header contract
    // depends on it.
    assetsDir: "assets",
    sourcemap: true,
  },
  server: {
    // `npm run dev` talks to a cluster over the same relative `/memql/ws`
    // path the built bundle uses, so the dev server proxies it rather than
    // the app carrying a dev-only absolute endpoint. MEMQL_PORTAL_DEV_TARGET
    // points at whatever front door the developer is running against.
    proxy: {
      "/memql/ws": {
        target: process.env.MEMQL_PORTAL_DEV_TARGET ?? "http://localhost:8085",
        ws: true,
        changeOrigin: true,
      },
    },
  },
  test: {
    environment: "jsdom",
    globals: true,
    include: ["test/**/*.test.ts", "test/**/*.test.tsx"],
    setupFiles: ["./test/setup.ts"],
  },
});
