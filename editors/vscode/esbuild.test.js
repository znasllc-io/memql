#!/usr/bin/env node
// Bundles each test/*.test.ts entry for `node --test`, mirroring how
// esbuild.js bundles the packaged extension.
//
// @znasllc-io/memql-sdk-core is pure ESM; tsc's raw per-file emit (module
// "ESNext"/moduleResolution "bundler", see tsconfig.json) is a type-check
// pass only and cannot produce runnable output on its own -- plain
// `node --test` on that output would hit a real `require()` of an ESM-only
// package and throw ERR_REQUIRE_ESM. Bundling the test files the same way
// the extension itself is bundled inlines the SDK into CommonJS, so the
// tests exercise the same module boundary the packaged extension does.
const esbuild = require("esbuild");
const fs = require("fs");
const path = require("path");

const testDir = path.join(__dirname, "test");
const entryPoints = fs
  .readdirSync(testDir)
  .filter((f) => f.endsWith(".test.ts"))
  .map((f) => path.join("test", f));

esbuild
  .build({
    entryPoints,
    bundle: true,
    outdir: "dist-test",
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
