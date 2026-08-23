/// <reference types="vitest/config" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// The portal is served from its OWN ORIGIN ROOT (memql#3711): it is site #1,
// resolved by component/edge from its own hostname (portal.<domain>) and
// served the same way as any other hosted site, not mounted at a `/portal/`
// sub-path of the bff's HTTP server the way component/portal (retired) used
// to. `base` has to match the root, or every hashed asset URL the bundle
// emits ("/assets/index-abc.js") resolves against the wrong path and 404s.
//
// It is also what makes the SPA fallback coherent: the edge returns
// index.html for any path it cannot resolve to a file (handler.go), and
// index.html's own script tag then points back at the root.
//
// import.meta.env.BASE_URL derives from this everywhere it is read
// (App.tsx's router basename, cluster/endpoint.ts's bridge + OAuth-callback
// paths, cluster/config.ts's runtime-config path) -- ONE choke point, so
// this is the only line that needed to change for the origin move.
const BASE = "/";

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
    // `npm run dev` talks to a cluster over the same relative `/_memql/ws`
    // path the built bundle uses (memql#4157). The rewrite matches the
    // edge serveAPI hop (`/_memql/*` -> bff `/memql/*`). MEMQL_PORTAL_DEV_TARGET
    // points at whatever front door the developer is running against.
    proxy: {
      "/_memql/ws": {
        target: process.env.MEMQL_PORTAL_DEV_TARGET ?? "http://localhost:8085",
        ws: true,
        changeOrigin: true,
        rewrite: (path) => path.replace(/^\/_memql/, "/memql"),
      },
      "/memql/ws": {
        target: process.env.MEMQL_PORTAL_DEV_TARGET ?? "http://localhost:8085",
        ws: true,
        changeOrigin: true,
      },
      // The Library's two byte-bearing routes (memql#4343): the one thing the
      // portal does over plain HTTP rather than on the stream. They live at
      // the bff's OWN root rather than under /memql, so this rewrite STRIPS
      // the marker instead of swapping it -- exactly what
      // component/edge/proxy.go's upstreamPath does for the same prefix, and
      // the reason it is a second entry rather than a widened first one.
      "/_memql/artifacts": {
        target: process.env.MEMQL_PORTAL_DEV_TARGET ?? "http://localhost:8085",
        changeOrigin: true,
        rewrite: (path) => path.replace(/^\/_memql/, ""),
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
