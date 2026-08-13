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
  deleteReceipt,
  emptyReceipt,
  entryFor,
  parseReceipt,
  readReceipt,
  removalParams,
  serializeReceipt,
  type Receipt,
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

test("appendReceiptEntry replaces an entry for a step that ran twice, but preExisting RATCHETS", async () => {
  // THIS ASSERTION USED TO READ `true`, and that was the bug (memql#3605).
  //
  // `preExisting` answers "was this artifact on the machine before memQL ever
  // ran" -- a fact about the PAST, which a later run cannot change. Newest-wins
  // let it flip anyway: a repair re-runs `clusterUp` against a cluster that now
  // exists, reports `clusterCreated: false`, and the derivation reads that as
  // "it was already here".
  //
  // The damage lands at uninstall, which cannot afford to be wrong:
  // `refuse_if_pre_existing` is an unconditional exit 3, so after any repair the
  // uninstall REFUSED to delete a cluster memQL had created -- leaving the
  // operator to run `k3d cluster delete` by hand, the very manual step the
  // installer exists to remove.
  //
  // Everything else about the entry is still newest-wins: where the artifact
  // landed is a present-tense fact and the latest run knows it best.
  const dir = await tempDir();
  const file = path.join(dir, "r.json");
  await appendReceiptEntry(file, "install", entry({ stepId: "toolK3d", preExisting: false }));
  await appendReceiptEntry(file, "install", entry({ stepId: "toolK3d", preExisting: true }));
  const r = await readReceipt(file);
  assert.equal(r?.entries.length, 1);
  assert.equal(
    entryFor(r!, "toolK3d")?.preExisting,
    false,
    "a later run must not be able to claim memQL found an artifact it created",
  );
});

test("preExisting stays true when it was true from the start", async () => {
  // The ratchet only runs one way. An artifact genuinely already on the machine
  // stays protected no matter how many times the step re-runs.
  const dir = await tempDir();
  const file = path.join(dir, "r.json");
  await appendReceiptEntry(file, "install", entry({ stepId: "toolMkcert", preExisting: true }));
  await appendReceiptEntry(file, "install", entry({ stepId: "toolMkcert", preExisting: true }));
  const r = await readReceipt(file);
  assert.equal(entryFor(r!, "toolMkcert")?.preExisting, true);
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

test("removalParams has nothing to remove when no run recorded a target", () => {
  // THIS TEST USED TO ASSERT THE OPPOSITE, and its reason was "better a missing
  // --path (the script exits 2 on a required param) than a guessed one". The
  // first half is right; the conclusion is not the only alternative to guessing
  // (memql#3564). A step that never recorded WHERE it wrote never wrote
  // anywhere -- and the operator who hit this got their whole uninstall stopped,
  // reported as "a fault in memQL", over a hosts block that does not exist.
  //
  // Nothing is guessed here either: null means "nothing to remove", the step
  // skips as satisfied, and the removals waiting on it still run.
  assert.equal(removalParams(entry({ receipt: "binary", result: {} })), null);
});

test("removalParams on an entry with no receipt has nothing to remove", () => {
  assert.equal(removalParams(entry({ receipt: "" })), null);
});

// -----------------------------------------------------------------------------
// A receipt is not a place for a secret (memql#3545)
// -----------------------------------------------------------------------------

test("a param that looks like a provider key is redacted before it is written", async () => {
  // DEFENCE IN DEPTH, and the depth is the point: the field validation
  // (addClusterCollect.test.ts) is what stops a key being typed here in the
  // first place, and it runs in ONE screen. This runs on the write itself, so
  // it also covers the CLI, a future surface, and a graph that pins a param
  // nobody re-read.
  //
  // The receipt is long-lived, mode 0600, and read back by repair and uninstall
  // for the life of the install. A secret that reaches it does not expire and
  // nothing ever rewrites it -- so the value is dropped at the boundary rather
  // than filtered on the way out.
  const dir = await tempDir();
  const file = path.join(dir, "install-receipt.json");

  await appendReceiptEntry(file, "install", {
    stepId: "providerKey",
    script: "install.verifyProviderKey",
    receipt: "",
    preExisting: false,
    params: {
      "key-file": "sk-ant-api03-EXAMPLE-not-a-real-key-aaaaaaaaaaaaaaaaaaaaaa",
      provider: "anthropic",
    },
    result: {},
    changed: false,
    recordedAt: "2026-08-11T00:00:00.000Z",
  });

  const raw = await fs.readFile(file, "utf8");
  assert.doesNotMatch(raw, /sk-ant/, "no part of a provider key may reach the receipt file");

  const receipt = await readReceipt(file);
  const entry = entryFor(receipt!, "providerKey")!;
  assert.notEqual(entry.params["key-file"], undefined, "the key is redacted, not dropped silently");
  assert.doesNotMatch(entry.params["key-file"]!, /sk-ant/);
  assert.equal(entry.params["provider"], "anthropic", "ordinary params are untouched");
});

test("an ordinary path param survives the write unchanged", async () => {
  const dir = await tempDir();
  const file = path.join(dir, "install-receipt.json");
  await appendReceiptEntry(file, "install", {
    stepId: "providerKey",
    script: "install.verifyProviderKey",
    receipt: "",
    preExisting: false,
    params: { "key-file": "/home/someone/.memql/key", provider: "anthropic" },
    result: {},
    changed: false,
    recordedAt: "2026-08-11T00:00:00.000Z",
  });
  const receipt = await readReceipt(file);
  assert.equal(entryFor(receipt!, "providerKey")!.params["key-file"], "/home/someone/.memql/key");
});

// -----------------------------------------------------------------------------
// The record of an install must be removable (memql#3544)
// -----------------------------------------------------------------------------

test("deleteReceipt removes the file, and is silent when there is none", async () => {
  // NOTHING DELETED THE RECEIPT. Not the uninstall that removed every artifact
  // it describes, not anything else -- so a machine that had been fully
  // uninstalled still carried the record of an install, presence still answered
  // "installed", and the wizard went on offering Repair and Uninstall for a
  // cluster that was gone. Permanently, and with no control anywhere that could
  // clear it.
  const dir = await tempDir();
  const file = path.join(dir, "install-receipt.json");
  await appendReceiptEntry(file, "install", {
    stepId: "clusterUp",
    script: "k3d.up",
    receipt: "stack",
    preExisting: false,
    params: {},
    result: {},
    changed: true,
    recordedAt: "2026-08-11T00:00:00.000Z",
  });

  await deleteReceipt(file);
  assert.equal(await readReceipt(file), null);

  // Idempotent: an uninstall re-run after a successful one must not fail on a
  // file its predecessor already removed.
  await deleteReceipt(file);
});

// -----------------------------------------------------------------------------
// a retried step, and a step that never wrote anything (memql#3564)
// -----------------------------------------------------------------------------

// The receipt is APPEND-ONLY and steps get retried, so a step usually has more
// than one entry. `entries.find(...)` returned the FIRST, which on a retried
// step is the OLDEST, which is the failure -- so an uninstall read the attempt
// that left nothing behind and never saw the one that did.
test("a retried step resolves to what the successful run recorded", () => {
  const receipt: Receipt = {
    ...emptyReceipt("install"),
    entries: [
      // The failed attempt: no --tag, so clone-stack.sh exited 2 before cloning.
      entry({
        stepId: "stackCheckout",
        script: "install.cloneStack",
        receipt: "checkout",
        params: { dest: "/home/dev/.memql/src" },
        result: {},
        changed: false,
      }),
      // The run that worked.
      entry({
        stepId: "stackCheckout",
        script: "install.cloneStack",
        receipt: "checkout",
        params: { tag: "v0.16.0", dest: "/home/dev/.memql/src" },
        result: { dest: "/home/dev/.memql/src", commit: "abc123", cloned: true },
        changed: true,
      }),
    ],
  };

  const resolved = entryFor(receipt, "stackCheckout")!;
  assert.equal(resolved.result.commit, "abc123", "the successful run's result is invisible");
  assert.deepEqual(removalParams(resolved), {
    kind: "checkout",
    path: "/home/dev/.memql/src",
    "pre-existing": "false",
  });
});

// The two questions an uninstall asks have opposite answers in time. "Is it
// ours to remove" can only be answered by the FIRST run: if run 1 created the
// CA and run 2 then found it there, reading the newest would conclude the
// operator already had it and refuse to remove something memQL created.
test("pre-existence is the FIRST run's answer, not the latest", () => {
  const receipt: Receipt = {
    ...emptyReceipt("install"),
    entries: [
      entry({ stepId: "localCA", receipt: "mkcertCA", preExisting: false, result: { caroot: "/ca" } }),
      entry({ stepId: "localCA", receipt: "mkcertCA", preExisting: true, result: { caroot: "/ca" } }),
    ],
  };

  const resolved = entryFor(receipt, "localCA")!;
  assert.equal(
    resolved.preExisting,
    false,
    "memQL created this CA on the first run; a later run finding it there does not make it the operator's",
  );
});

// The failure the operator actually hit. hostsBlock died on a read-only
// /etc/hosts and recorded `{remedy}` -- no hostsFile, because nothing was
// written. The uninstall then passed no --path, remove-artifact.sh exited 2,
// and the wizard reported "a fault in memQL" about a block that does not exist.
test("a step that never recorded where it wrote has nothing to remove", () => {
  const hostsBlock = entry({
    stepId: "hostsBlock",
    script: "install.hostsEntries",
    receipt: "hostsEntries",
    params: { action: "add", confirm: "add-memql-hosts" },
    result: { remedy: "sudo .../hosts-entries.sh --action=add" },
    changed: false,
  });

  assert.equal(
    removalParams(hostsBlock),
    null,
    "a hosts block that was never written is not there to remove -- and exiting 2 " +
      "over it stops the whole uninstall",
  );
});

// The other half: when the result is empty but the installer's own invocation
// named the target, that IS the location. Not a guess -- the exact value the
// run used, so if anything landed it landed there.
test("a target the run was invoked with is used when the result carries none", () => {
  const checkout = entry({
    stepId: "stackCheckout",
    script: "install.cloneStack",
    receipt: "checkout",
    params: { tag: "v0.16.0", dest: "/opt/memql/src" },
    result: {},
  });

  assert.deepEqual(removalParams(checkout), {
    kind: "checkout",
    path: "/opt/memql/src",
    "pre-existing": "false",
  });
});

// install.binary takes --dest=<directory> and reports result.path as the FILE
// inside it. Falling back to the param would hand remove-artifact.sh a
// directory where it expects a file.
test("a binary never falls back to its --dest directory", () => {
  const binary = entry({
    stepId: "toolK3d",
    receipt: "binary",
    params: { tool: "k3d", dest: "/home/dev/.memql/bin" },
    result: {},
  });

  assert.equal(
    removalParams(binary),
    null,
    "the param is the directory the binary goes IN; removing that is not removing the binary",
  );
});
