// The install receipt.
//
// The receipt is the only thing that makes an uninstall honest: it is where
// "we put a k3d binary at /home/x/.memql/bin/k3d, and it was NOT already
// there" is written down. Two properties are load-bearing and both are
// asserted here.
//
//   1. It is written PER STEP, through an atomic rename. A run killed between
//      two steps must leave a receipt that describes everything done so far --
//      not a truncated file, and never a file describing more than happened.
//   2. It carries the pre-existence verdict FAITHFULLY. remove-artifact.sh
//      refuses unconditionally on --pre-existing=true; the receipt is what
//      feeds that flag, so a receipt that rounds "already here" down to false
//      is how an uninstall deletes the developer's own k3d.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";

import {
  RECEIPT_VERSION,
  appendReceiptEntry,
  defaultReceiptPath,
  emptyReceipt,
  entryFor,
  parseReceipt,
  readReceipt,
  removalParams,
  serializeReceipt,
  type ReceiptEntry,
} from "../src/install/receipt.js";

async function tempDir(): Promise<string> {
  return fs.mkdtemp(path.join(os.tmpdir(), "memql-receipt-"));
}

function entry(over: Partial<ReceiptEntry> = {}): ReceiptEntry {
  return {
    stepId: "toolK3d",
    script: "install.binary",
    receipt: "binary",
    preExisting: false,
    params: { tool: "k3d" },
    result: { tool: "k3d", path: "/home/dev/.memql/bin/k3d", installed: true },
    changed: true,
    recordedAt: "2026-08-08T00:00:00.000Z",
    ...over,
  };
}

// -----------------------------------------------------------------------------
// shape
// -----------------------------------------------------------------------------

test("an empty receipt round-trips", () => {
  const r = emptyReceipt("install");
  const parsed = parseReceipt(serializeReceipt(r));
  assert.ok(parsed.ok);
  assert.equal(parsed.receipt.version, RECEIPT_VERSION);
  assert.equal(parsed.receipt.graph, "install");
  assert.deepEqual(parsed.receipt.entries, []);
});

test("parseReceipt refuses garbage rather than reporting an empty install", () => {
  // An unreadable receipt reported as "nothing was installed" is an uninstall
  // that silently leaves a cluster, a CA and three binaries behind.
  assert.equal(parseReceipt("{ nope").ok, false);
  assert.equal(parseReceipt("[]").ok, false);
  assert.equal(parseReceipt(JSON.stringify({ version: 1, graph: "install" })).ok, false);
  assert.equal(parseReceipt(JSON.stringify({ version: 99, graph: "install", entries: [] })).ok, false);
});

test("parseReceipt drops an entry it cannot read, keeping the rest", () => {
  const good = entry();
  const raw = JSON.stringify({
    version: RECEIPT_VERSION,
    graph: "install",
    startedAt: "t",
    updatedAt: "t",
    entries: [good, { stepId: 7 }, { script: "x" }],
  });
  const parsed = parseReceipt(raw);
  assert.ok(parsed.ok);
  assert.equal(parsed.receipt.entries.length, 1);
  assert.equal(parsed.dropped, 2);
});

test("readReceipt returns null for a receipt that was never written", async () => {
  const dir = await tempDir();
  assert.equal(await readReceipt(path.join(dir, "nope.json")), null);
});

test("defaultReceiptPath lives under the installer's own directory", () => {
  assert.equal(defaultReceiptPath("/home/dev"), path.join("/home/dev", ".memql", "install-receipt.json"));
});

// -----------------------------------------------------------------------------
// per-step, atomic
// -----------------------------------------------------------------------------

test("appendReceiptEntry writes each step and leaves the file readable after every one", async () => {
  const dir = await tempDir();
  const file = path.join(dir, "nested", "install-receipt.json");

  await appendReceiptEntry(file, "install", entry({ stepId: "toolK3d" }));
  let r = await readReceipt(file);
  assert.equal(r?.entries.length, 1);

  await appendReceiptEntry(file, "install", entry({ stepId: "toolKubectl" }));
  r = await readReceipt(file);
  assert.equal(r?.entries.length, 2);
  assert.deepEqual(r?.entries.map((e: { stepId: string }) => e.stepId), ["toolK3d", "toolKubectl"]);

  // Atomic rename: no scratch file survives the write.
  const left = await fs.readdir(path.dirname(file));
  assert.deepEqual(left, ["install-receipt.json"]);
});

test("appendReceiptEntry replaces an entry for a step that ran twice", async () => {
  const dir = await tempDir();
  const file = path.join(dir, "r.json");
  await appendReceiptEntry(file, "install", entry({ stepId: "toolK3d", preExisting: false }));
  await appendReceiptEntry(file, "install", entry({ stepId: "toolK3d", preExisting: true }));
  const r = await readReceipt(file);
  assert.equal(r?.entries.length, 1);
  assert.equal(entryFor(r!, "toolK3d")?.preExisting, true);
});

test("concurrent appends -- a wave's steps all land", async () => {
  // Steps in a wave finish concurrently, so the receipt writer serialises
  // them; losing one is losing the artifact it describes.
  const dir = await tempDir();
  const file = path.join(dir, "r.json");
  await Promise.all(
    ["toolK3d", "toolKubectl", "toolMkcert"].map((id) => appendReceiptEntry(file, "install", entry({ stepId: id }))),
  );
  const r = await readReceipt(file);
  assert.equal(r?.entries.length, 3);
  assert.deepEqual(new Set(r?.entries.map((e: { stepId: string }) => e.stepId)), new Set(["toolK3d", "toolKubectl", "toolMkcert"]));
});

test("appendReceiptEntry refuses to write over a receipt it cannot parse", async () => {
  const dir = await tempDir();
  const file = path.join(dir, "r.json");
  await fs.writeFile(file, "{ half-written", "utf8");
  await assert.rejects(() => appendReceiptEntry(file, "install", entry()));
  assert.equal(await fs.readFile(file, "utf8"), "{ half-written");
});

// -----------------------------------------------------------------------------
// what uninstall reads back
// -----------------------------------------------------------------------------

test("removalParams -- each artifact kind names its own target", () => {
  assert.deepEqual(removalParams(entry({ receipt: "binary", result: { path: "/b/k3d" } })), {
    kind: "binary",
    path: "/b/k3d",
    "pre-existing": "false",
  });
  assert.deepEqual(removalParams(entry({ receipt: "checkout", result: { dest: "/home/dev/.memql/src" } })), {
    kind: "checkout",
    path: "/home/dev/.memql/src",
    "pre-existing": "false",
  });
  assert.deepEqual(removalParams(entry({ receipt: "hostsEntries", result: { hostsFile: "/etc/hosts" } })), {
    kind: "hostsEntries",
    path: "/etc/hosts",
    "pre-existing": "false",
  });
  assert.deepEqual(removalParams(entry({ receipt: "mkcertCA", result: { caroot: "/home/dev/.local/share/mkcert" } })), {
    kind: "mkcertCA",
    caroot: "/home/dev/.local/share/mkcert",
    "pre-existing": "false",
  });
  assert.deepEqual(removalParams(entry({ receipt: "stack", result: { cluster: "memql" } })), {
    kind: "stack",
    cluster: "memql",
    "pre-existing": "false",
  });
});

test("removalParams carries the pre-existence verdict faithfully", () => {
  // This flag is the last wall between an uninstall and a developer's own
  // k3d cluster. remove-artifact.sh refuses on true; the receipt must say so.
  const params = removalParams(entry({ receipt: "stack", preExisting: true, result: { cluster: "memql" } }));
  assert.equal(params?.["pre-existing"], "true");
});

test("removalParams omits a target it has no recorded value for", () => {
  // Better a missing --path (the script exits 2 on a required param) than a
  // guessed one.
  const params = removalParams(entry({ receipt: "binary", result: {} }));
  assert.deepEqual(params, { kind: "binary", "pre-existing": "false" });
});

test("removalParams on an entry with no receipt has nothing to remove", () => {
  assert.equal(removalParams(entry({ receipt: "" })), null);
});
