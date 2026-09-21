import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import path from "node:path";
import { readFileSync } from "node:fs";

// The browser QA harness (clients/os/DESIGN.md: "the acceptance for any surface
// change under these rules is rendered screenshots, both modes, empty and
// populated -- not the diff").
//
// TWO THINGS THIS GETS WRONG BY DEFAULT, both silent, both fixed here:
//
//  1. THE SWAP PLUGIN NEEDS `enforce: "pre"`. Vite's own resolver is a `pre`
//     plugin, so a plain `resolveId` hook is asked AFTER the id is resolved and
//     never sees it. The symptom is indistinguishable from having no plugin at
//     all: the app mounts, the stylesheet is there, and every live list says
//     "Not connected to the cluster".
//
//  2. THE SWAP IS DECIDED ON THE RESOLVED PATH, not the specifier.
//     `src/live/connection` is imported as `"../../../live/connection"` from an
//     app tree and as `"./connection"` from inside `src/live`, so any
//     `endsWith` match catches one and misses the other -- and missing one
//     means the app holds a SECOND, empty instance of the seam.
//
// Its own cacheDir, because sharing `node_modules/.vite` with an ordinary dev
// server answers `504 Outdated Optimize Dep` for react, which renders as a
// blank white page with nothing on `window.onerror`.

const root = path.resolve(__dirname, "..");
const repo = path.resolve(root, "..", "..");
const realConnection = path.join(root, "src/live/connection.tsx");
const seededAccess = path.join(root, "test/seededAccess.ts");
const seedsFile = path.join(repo, "dsl/rbac/seeds.memql");
const shim = path.join(__dirname, "connectionShim.tsx");

export default defineConfig({
  root: __dirname,
  cacheDir: path.join(__dirname, ".vite"),
  plugins: [
    react(),
    {
      // `test/seededAccess.ts` reads dsl/rbac/seeds.memql off disk with
      // node:fs, at MODULE LOAD, so importing it in a browser fails the whole
      // module graph -- and silently: the page stays blank with nothing on
      // window.onerror, because the failure is an unresolved import rather
      // than a thrown error.
      //
      // THE SEEDS ARE INLINED RATHER THAN THE MODULE REPLACED. A browser copy
      // of "which role holds what" would be a second copy of the policy in a
      // second language, which is the exact defect the registry's switch to
      // `requires:` removed. So the module keeps its own parse and its own
      // regex, and only the SOURCE of the text changes -- read here, on the
      // Node side, from the one file the Go gates read too.
      name: "qa-seeded-access-inline",
      enforce: "pre",
      async resolveId(id, importer, options) {
        const resolved = await this.resolve(id, importer, { ...options, skipSelf: true });
        if (resolved && path.resolve(resolved.id.split("?")[0]) === seededAccess) {
          return seededAccess + "?qa-inline";
        }
        return null;
      },
      load(id) {
        if (!id.startsWith(seededAccess) || !id.includes("qa-inline")) return null;
        const src = readFileSync(seededAccess, "utf8");
        const seeds = readFileSync(seedsFile, "utf8");
        return src
          .replace(/^import \{ readFileSync \}.*$/m, "")
          .replace(/^import \{ dirname, join \}.*$/m, "")
          .replace(/^import \{ fileURLToPath \}.*$/m, "")
          .replace(/^const here = .*$/m, "")
          .replace(/^const SEEDS_PATH = .*$/m, "")
          .replace("readFileSync(SEEDS_PATH, \"utf8\")", JSON.stringify(seeds));
      },
    },
    {
      name: "qa-connection-swap",
      enforce: "pre",
      async resolveId(id, importer, options) {
        // `vitest` is not installed as a runtime dependency; the fixture
        // connection imports `vi` from it, so the shim answers for it.
        if (id === "vitest") return path.join(__dirname, "vitestShim.ts");
        // The shim's OWN import of the real module must not be swapped for
        // itself, or its re-exports resolve to nothing.
        if (importer === shim) return null;
        const resolved = await this.resolve(id, importer, { ...options, skipSelf: true });
        if (resolved && path.resolve(resolved.id.split("?")[0]) === realConnection) return shim;
        return null;
      },
    },
  ],
  server: { fs: { allow: [path.resolve(root, "..", "..")] } },
});
