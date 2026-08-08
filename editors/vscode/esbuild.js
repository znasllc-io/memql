#!/usr/bin/env node
// Bundles the extension's entry point for packaging.
//
// @znasllc-io/memql-sdk-core and @znasllc-io/memql-view-kit are `file:`
// workspace dependencies that land in node_modules as symlinks; `vsce
// package` has no bundler of its own, so it either fails to follow the
// symlink (ERR_MODULE_NOT_FOUND at first connect) or absorbs the linked
// packages' src/, dist-test/, and node_modules/ straight into the VSIX --
// and either way it "succeeds", so CI would not catch it. Bundling here
// inlines every dependency except `vscode` (supplied by the extension host)
// into one self-contained out/extension.js; .vscodeignore excludes
// node_modules from the VSIX entirely since nothing in it is needed anymore.
//
// This also resolves the ESM/CommonJS boundary: @znasllc-io/memql-sdk-core is
// pure ESM and the VS Code extension host loads `main` via CommonJS
// `require` (that host contract is unchanged -- see tsconfig.json), so a
// plain static import of it only works because esbuild converts the whole
// bundle to CJS, inlining the ESM source rather than leaving a `require()`
// pointed at it.
const esbuild = require("esbuild");
const fs = require("fs");

// Wipe out/ first: tsc used to emit one .js per source file here (before
// tsconfig.json switched to noEmit + this bundler), and a stale per-file tree
// left over from an old checkout/branch would otherwise sit alongside the
// bundle forever -- harmless to `require`, but it ships in the VSIX and is
// exactly the kind of stray content finding 4 asked to prune.
fs.rmSync("out", { recursive: true, force: true });

esbuild
  .build({
    entryPoints: ["src/extension.ts"],
    bundle: true,
    outfile: "out/extension.js",
    platform: "node",
    format: "cjs",
    target: "node20",
    external: ["vscode"],
    sourcemap: true,
    logLevel: "info",
  })
  .catch((err) => {
    console.error(err);
    process.exit(1);
  });
