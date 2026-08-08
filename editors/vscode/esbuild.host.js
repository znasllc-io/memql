#!/usr/bin/env node
// Bundles the Extension Development Host smoke lane, mirroring how
// esbuild.test.js bundles the unit tests and esbuild.js bundles the extension.
//
// The reason is the same one those two files give: @znasllc-io/memql-sdk-core
// is pure ESM and the extension host loads modules through CommonJS `require`,
// so a raw tsc emit of these files would throw ERR_REQUIRE_ESM the moment a
// case imported anything reaching the SDK -- and every panel case does, via
// src/webview/*. Bundling inlines the SDK into CJS, which also means the smoke
// lane exercises the SAME module boundary the packaged extension does.
//
// `vscode` stays external: it is supplied by the host at require time and has
// no on-disk package to inline.
//
// Only test-host/index.ts is an entry point. test-host/runner.js runs OUTSIDE
// the host in plain Node and is deliberately not bundled -- see its header.
const esbuild = require("esbuild");
const fs = require("fs");

// Wipe dist-host/ first, for the reason esbuild.js wipes out/: a stale bundle
// from an older checkout is still perfectly requireable, so it would run
// silently instead of failing.
fs.rmSync("dist-host", { recursive: true, force: true });

esbuild
  .build({
    entryPoints: ["test-host/index.ts"],
    bundle: true,
    outdir: "dist-host",
    outbase: ".",
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
