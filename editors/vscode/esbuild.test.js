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
//
// `vscode` is ALIASED to test/support/vscodeStub.ts rather than left external
// (memql#3387). The module exists only inside a running editor, so an external
// `require("vscode")` from a bundle is not resolvable in plain Node -- which is
// exactly why every module carrying logic in this package is free of the import
// (enforced by cmd/memql-lsp/vscodeimportrule_test.go). The one adapter that
// has to be driven here anyway is src/extension.ts: activate()'s entire content
// is the ORDER two independent surfaces come up in, and an ordering defect is
// what a unit test catches and the host smoke lane does not. The alias supplies
// the stub in its place; the stub's header covers what it does and does not
// model. `vscode-languageclient/node` is aliased for the same reason one step
// removed -- it subclasses editor-supplied classes at module load, so it cannot
// be required against a fake `vscode` at all. Every other test file is
// untouched: none of them import either module.
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
    alias: {
      vscode: path.join(__dirname, "test", "support", "vscodeStub.ts"),
      "vscode-languageclient/node": path.join(
        __dirname,
        "test",
        "support",
        "languageClientStub.ts"
      ),
    },
    sourcemap: true,
    logLevel: "info",
  })
  .catch((err) => {
    console.error(err);
    process.exit(1);
  });
