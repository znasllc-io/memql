// Resolving the root a session's CAPABILITY SCRIPTS are read from
// (znasllc-io/memql#5056).
//
// The bug: "Update from origin and rebuild" fast-forwards the checkout, then
// builds it with the scripts frozen inside the extension at package time. The
// two cannot disagree out loud, so the first thing that noticed was a Dockerfile
// stage deleted in the commits the update had just pulled.
//
// These cases are the three worlds that matter -- a checkout that carries the
// capability contract (borrow it), one that does not (fall back, which is what
// happens today), and no checkout at all (an install, which has nothing to
// borrow from).
//
// Everything runs against temporary directories: the marker is a real stat.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";

import { resolveScriptRoot } from "../src/install/root.js";

async function tempDir(): Promise<string> {
  return fs.mkdtemp(path.join(os.tmpdir(), "memql-script-root-"));
}

/** A checkout carrying the capability-script contract. */
async function checkoutWithScripts(): Promise<string> {
  const dir = await tempDir();
  await fs.mkdir(path.join(dir, "scripts", "lib"), { recursive: true });
  await fs.writeFile(path.join(dir, "scripts", "lib", "capability.sh"), "#!/usr/bin/env bash\n");
  return dir;
}

test("a checkout carrying the contract is where scripts come from", async () => {
  // The fix: the tree being BUILT is the tree whose build recipe runs.
  const installRoot = await tempDir();
  const checkout = await checkoutWithScripts();

  assert.equal(resolveScriptRoot(installRoot, checkout), checkout);
});

test("a checkout without the contract falls back to the install root", async () => {
  // A checkout too old to answer the capability contract cannot run these
  // scripts at all, so it gets the answer it got before this function existed.
  const installRoot = await tempDir();
  const checkout = await tempDir();

  assert.equal(resolveScriptRoot(installRoot, checkout), installRoot);
});

test("no checkout at all is the install root", async () => {
  // An install runs before a checkout exists; there is nothing to borrow.
  const installRoot = await tempDir();

  assert.equal(resolveScriptRoot(installRoot), installRoot);
});

test("an empty checkout path is the install root", async () => {
  const installRoot = await tempDir();

  assert.equal(resolveScriptRoot(installRoot, ""), installRoot);
});

test("a DIRECTORY where capability.sh belongs is not the contract", async () => {
  // A half-written or differently-shaped tree is not a checkout to borrow from.
  const installRoot = await tempDir();
  const checkout = await tempDir();
  await fs.mkdir(path.join(checkout, "scripts", "lib", "capability.sh"), { recursive: true });

  assert.equal(resolveScriptRoot(installRoot, checkout), installRoot);
});

test("a checkout with scripts/lib but no capability.sh falls back", async () => {
  const installRoot = await tempDir();
  const checkout = await tempDir();
  await fs.mkdir(path.join(checkout, "scripts", "lib"), { recursive: true });

  assert.equal(resolveScriptRoot(installRoot, checkout), installRoot);
});
