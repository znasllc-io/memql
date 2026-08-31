/// <reference types="vitest/config" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

import pkg from "./package.json" with { type: "json" };

const BASE = "/";

// The OS bundle's own build identifier, compiled in as `__OS_BUILD__`
// (memql#4744). Diagnostics needs to name the CLIENT that is running, and
// nothing in the bundle knew its own identity: `package.json` is private and
// never imported, `runtime-config.json` carries no version, and the only
// build identity reaching the browser was the SERVER's, off the ServerHello.
//
// It is deliberately NOT a git sha. The release path builds this bundle
// inside the Docker stage that does `COPY clients/os ./clients/os` -- no
// `.git` is present there, so a sha derived at build time would resolve to
// nothing in exactly the build whose identity matters most, and would do it
// silently. The package version is present in both paths, and
// MEMQL_OS_BUILD lets the release build stamp something sharper without the
// bundle having to guess.
//
// Constant per build in every path, so the bundle stays reproducible: no
// timestamp, no host, no working-directory state.
const OS_BUILD = process.env.MEMQL_OS_BUILD?.trim() || pkg.version;

export default defineConfig({
  base: BASE,
  plugins: [react()],
  define: {
    __OS_BUILD__: JSON.stringify(OS_BUILD),
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    assetsDir: "assets",
    sourcemap: true,
  },
  test: {
    environment: "jsdom",
    globals: true,
    include: ["test/**/*.test.ts", "test/**/*.test.tsx"],
    setupFiles: ["./test/setup.ts"],
  },
});
