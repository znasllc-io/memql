// Resolving the root a packaged extension runs an install from.
//
// The cases are the two worlds the extension actually lives in -- a .vsix, whose
// only copy of scripts/ is the staged tree beside out/, and the Extension
// Development Host, which runs out of editors/vscode/ in a checkout where the
// real scripts/ is two levels up. The third case is the one that produced
// memql#3487: neither, which must still hand back a path so the caller fails on
// a NAMED missing graph document rather than here.
//
// Everything runs against temporary directories: the marker is a real stat, so a
// test that faked the filesystem would be testing its own fake.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";

import { resolveInstallRoot, STAGED_ROOT_DIR } from "../src/install/root.js";

async function tempDir(): Promise<string> {
  return fs.mkdtemp(path.join(os.tmpdir(), "memql-install-root-"));
}

/** Builds the shape package.sh produces: <ext>/staged/scripts/install/graph. */
async function stage(extensionPath: string): Promise<string> {
  const staged = path.join(extensionPath, STAGED_ROOT_DIR);
  await fs.mkdir(path.join(staged, "scripts", "install", "graph"), { recursive: true });
  await fs.writeFile(path.join(staged, "scripts", "install", "graph", "install.json"), "{}");
  return staged;
}

test("a staged tree is the root", async () => {
  const extensionPath = await tempDir();
  const staged = await stage(extensionPath);

  assert.equal(resolveInstallRoot(extensionPath), staged);
});

test("a staged tree wins over an explicit checkout root", async () => {
  // A packaged extension may sit anywhere; only its OWN staged copy is known to
  // be the tree it was built against.
  const extensionPath = await tempDir();
  const staged = await stage(extensionPath);
  const checkout = await tempDir();

  assert.equal(resolveInstallRoot(extensionPath, checkout), staged);
});

test("no staged tree falls back to the repository root two levels up", async () => {
  // The Extension Development Host shape: extensionPath is <repo>/editors/vscode
  // and the real scripts/ is at <repo>.
  const repo = await tempDir();
  const extensionPath = path.join(repo, "editors", "vscode");
  await fs.mkdir(extensionPath, { recursive: true });
  await fs.mkdir(path.join(repo, "scripts", "install", "graph"), { recursive: true });

  assert.equal(resolveInstallRoot(extensionPath), repo);
});

test("no staged tree honours an explicit checkout root", async () => {
  const extensionPath = await tempDir();
  const checkout = await tempDir();

  assert.equal(resolveInstallRoot(extensionPath, checkout), checkout);
});

test("a staged directory without the graph marker is not a staged tree", async () => {
  // A half-written stage is not a stage. The marker is the graph directory
  // precisely because an install cannot begin without a graph document, so its
  // absence is unambiguous.
  const repo = await tempDir();
  const extensionPath = path.join(repo, "editors", "vscode");
  await fs.mkdir(path.join(extensionPath, STAGED_ROOT_DIR, "scripts", "lib"), {
    recursive: true,
  });

  assert.equal(resolveInstallRoot(extensionPath), repo);
});

test("a file where the graph directory belongs is not a staged tree", async () => {
  const repo = await tempDir();
  const extensionPath = path.join(repo, "editors", "vscode");
  await fs.mkdir(path.join(extensionPath, STAGED_ROOT_DIR, "scripts", "install"), {
    recursive: true,
  });
  await fs.writeFile(
    path.join(extensionPath, STAGED_ROOT_DIR, "scripts", "install", "graph"),
    "not a directory"
  );

  assert.equal(resolveInstallRoot(extensionPath), repo);
});

test("neither present still yields a path, so the caller fails on the graph document", async () => {
  const repo = await tempDir();
  const extensionPath = path.join(repo, "editors", "vscode");
  await fs.mkdir(extensionPath, { recursive: true });

  assert.equal(resolveInstallRoot(extensionPath), repo);
});
