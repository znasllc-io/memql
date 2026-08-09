// The TypeScript install-graph reader.
//
// This is a deliberate SECOND implementation of scripts/install/graph/graph.go,
// and the tests are what make that safe. Two readers of one document is a
// liability unless both are held to the same document: so the shipped
// install.json and uninstall.json are loaded HERE, by this reader, and every
// refusal the Go loader makes is asserted again below.
//
// The assertion that matters most: evaluateVerify reads the RESULT, never the
// exit code. A capability script that exits 0 having done nothing is the exact
// failure the verify vocabulary exists to catch, so "ok: true with a false
// result field" must evaluate FALSE, and scriptOk -- which really does read
// only the envelope -- must be unreachable on a mutating step.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import * as path from "node:path";

import {
  GraphError,
  evaluateVerify,
  graphDocumentPath,
  loadGraph,
  loadGraphFile,
  resolvePreExisting,
  stepById,
  stepPreExisting,
  topoOrder,
  verifyField,
  type Step,
} from "../src/install/graph.js";

const REPO_ROOT = path.resolve(__dirname, "..", "..", "..", "..");

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

/** A minimal valid install document with the given steps spliced in. */
function doc(kind: "install" | "uninstall", steps: unknown[]): string {
  return JSON.stringify({ name: kind, kind, description: "d", steps });
}

function readOnlyStep(over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: "a",
    script: "install.detect",
    description: "d",
    readOnly: true,
    elevation: "none",
    verify: { kind: "scriptOk" },
    ...over,
  };
}

function refuses(source: string, needle?: string): void {
  assert.throws(
    () => loadGraph(source, "test.json"),
    (err: unknown) => {
      if (!(err instanceof GraphError)) {
        throw new Error(`expected a GraphError, got ${String(err)}`);
      }
      if (needle) {
        assert.match(err.message, new RegExp(needle, "i"));
      }
      return true;
    },
  );
}

// -----------------------------------------------------------------------------
// the shipped documents
// -----------------------------------------------------------------------------

test("loads the shipped install graph", async () => {
  const g = await loadGraphFile(graphDocumentPath("install", REPO_ROOT));
  assert.equal(g.kind, "install");
  assert.ok(g.steps.length >= 12);
  assert.ok(stepById(g, "detect"));
  assert.ok(stepById(g, "seedBootstrap"));
  assert.equal(stepById(g, "seedBootstrap")?.containedBy, "clusterUp");
});

test("loads the shipped uninstall graph", async () => {
  const g = await loadGraphFile(graphDocumentPath("uninstall", REPO_ROOT));
  assert.equal(g.kind, "uninstall");
  for (const s of g.steps) {
    assert.equal(s.script, "install.removeArtifact");
    assert.ok(s.reverses, `${s.id} reverses nothing`);
  }
});

test("every uninstall step reverses a real install step", async () => {
  const install = await loadGraphFile(graphDocumentPath("install", REPO_ROOT));
  const uninstall = await loadGraphFile(graphDocumentPath("uninstall", REPO_ROOT));
  for (const s of uninstall.steps) {
    assert.ok(stepById(install, s.reverses!), `${s.id} reverses unknown install step ${s.reverses}`);
  }
});

test("waves are a real topological decomposition of the shipped graph", async () => {
  const g = await loadGraphFile(graphDocumentPath("install", REPO_ROOT));
  const waves = topoOrder(g);
  const seen = new Set<string>();
  for (const wave of waves) {
    // Deterministic ordering inside a wave, as in the Go implementation.
    assert.deepEqual([...wave].sort(), wave);
    for (const id of wave) {
      for (const dep of stepById(g, id)?.dependsOn ?? []) {
        assert.ok(seen.has(dep), `${id} runs in the same wave as (or before) its dependency ${dep}`);
      }
    }
    for (const id of wave) seen.add(id);
  }
  assert.equal(seen.size, g.steps.length);
  assert.deepEqual(waves[0], ["detect"]);
});

// -----------------------------------------------------------------------------
// refusals -- the same ones graph.go makes
// -----------------------------------------------------------------------------

test("refuses an unknown field rather than ignoring it", () => {
  refuses(doc("install", [readOnlyStep({ retries: 3 })]), "unknown field");
  refuses(
    JSON.stringify({ name: "x", kind: "install", description: "d", steps: [readOnlyStep()], parallel: true }),
    "unknown field",
  );
});

test("refuses a step with no verify", () => {
  const s = readOnlyStep();
  delete s.verify;
  refuses(doc("install", [s]), "verify");
});

test("refuses scriptOk on a step that changed the machine", () => {
  refuses(
    doc("install", [readOnlyStep({ readOnly: false, verify: { kind: "scriptOk" } })]),
    "rubber stamp|readOnly",
  );
});

test("refuses a verify field that is not spelled result.<name>", () => {
  refuses(doc("install", [readOnlyStep({ verify: { kind: "resultTrue", field: "ok" } })]), "result\\.");
  refuses(doc("install", [readOnlyStep({ verify: { kind: "resultTrue", field: "changed" } })]), "result\\.");
});

test("refuses resultEquals with no value, and a value on the kinds that take none", () => {
  refuses(doc("install", [readOnlyStep({ verify: { kind: "resultEquals", field: "result.k" } })]), "value");
  refuses(
    doc("install", [readOnlyStep({ verify: { kind: "resultTrue", field: "result.k", value: "x" } })]),
    "takes no value",
  );
});

test("refuses an unknown verify kind", () => {
  refuses(doc("install", [readOnlyStep({ verify: { kind: "exitZero" } })]), "unknown verify kind");
});

test("refuses an undeclared or unknown elevation", () => {
  const s = readOnlyStep();
  delete s.elevation;
  refuses(doc("install", [s]), "elevation");
  refuses(doc("install", [readOnlyStep({ elevation: "root" })]), "elevation");
});

test("refuses a dangling, self, or duplicated dependency", () => {
  refuses(doc("install", [readOnlyStep({ dependsOn: ["nope"] })]), "not a step");
  refuses(doc("install", [readOnlyStep({ dependsOn: ["a"] })]), "itself");
  refuses(doc("install", [readOnlyStep(), readOnlyStep()]), "duplicate step id");
});

test("refuses a dependency cycle", () => {
  refuses(
    doc("install", [
      readOnlyStep({ id: "a", dependsOn: ["b"] }),
      readOnlyStep({ id: "b", dependsOn: ["a"] }),
    ]),
    "cycle",
  );
});

test("refuses a receipt with no pre-existence signal, and the reverse", () => {
  refuses(
    doc("install", [
      readOnlyStep({ readOnly: false, receipt: "binary", verify: { kind: "resultTrue", field: "result.installed" } }),
    ]),
    "preExistingPath",
  );
  refuses(doc("install", [readOnlyStep({ preExistingPath: "result.preExisting" })]), "no receipt");
});

test("refuses a malformed preExistingPath", () => {
  refuses(
    doc("install", [
      readOnlyStep({
        readOnly: false,
        receipt: "binary",
        preExistingPath: "preExisting",
        verify: { kind: "resultTrue", field: "result.installed" },
      }),
    ]),
    "preExistingPath",
  );
});

test("refuses receipt on an uninstall step and reverses on an install step", () => {
  refuses(doc("install", [readOnlyStep({ reverses: "x" })]), "reverses");
  refuses(
    doc("uninstall", [
      readOnlyStep({ readOnly: false, receipt: "binary", preExistingPath: "none", verify: { kind: "resultTrue", field: "result.removed" } }),
    ]),
    "receipt",
  );
});

test("refuses both receipt and containedBy on one step", () => {
  refuses(
    doc("install", [
      readOnlyStep({ id: "a", readOnly: false, verify: { kind: "resultTrue", field: "result.x" } }),
      readOnlyStep({
        id: "b",
        readOnly: false,
        receipt: "binary",
        preExistingPath: "none",
        containedBy: "a",
        verify: { kind: "resultTrue", field: "result.x" },
      }),
    ]),
    "both receipt and containedBy",
  );
});

test("refuses containedBy naming a step that is not in the graph", () => {
  refuses(
    doc("install", [
      readOnlyStep({ readOnly: false, containedBy: "ghost", verify: { kind: "resultTrue", field: "result.x" } }),
    ]),
    "not a step",
  );
});

test("refuses an empty graph and an unknown kind", () => {
  refuses(JSON.stringify({ name: "x", kind: "install", description: "d", steps: [] }), "no steps");
  refuses(JSON.stringify({ name: "x", kind: "repair", description: "d", steps: [readOnlyStep()] }), "kind");
  refuses("{ nope", "");
});

// -----------------------------------------------------------------------------
// evaluateVerify -- reads the RESULT, never the exit code
// -----------------------------------------------------------------------------

test("evaluateVerify reads the result, not the exit code", () => {
  // The whole point. The script exited 0 and said so; it did not do the thing.
  const envelope = { ok: true, capability: "install.binary", changed: false, result: { installed: false }, error: null };
  assert.equal(evaluateVerify({ kind: "resultTrue", field: "result.installed" }, envelope), false);
  assert.equal(
    evaluateVerify({ kind: "resultTrue", field: "result.installed" }, { ...envelope, result: { installed: true } }),
    true,
  );
});

test("evaluateVerify is false whenever the envelope is not ok", () => {
  const envelope = { ok: false, result: { installed: true } };
  assert.equal(evaluateVerify({ kind: "resultTrue", field: "result.installed" }, envelope), false);
  assert.equal(evaluateVerify({ kind: "scriptOk" }, envelope), false);
});

test("evaluateVerify -- scriptOk reads only the envelope", () => {
  assert.equal(evaluateVerify({ kind: "scriptOk" }, { ok: true, result: {} }), true);
});

test("evaluateVerify -- a missing field is never a pass", () => {
  const envelope = { ok: true, result: {} };
  assert.equal(evaluateVerify({ kind: "resultTrue", field: "result.x" }, envelope), false);
  assert.equal(evaluateVerify({ kind: "resultFalse", field: "result.x" }, envelope), false);
  assert.equal(evaluateVerify({ kind: "resultNonEmpty", field: "result.x" }, envelope), false);
  assert.equal(evaluateVerify({ kind: "resultEquals", field: "result.x", value: "" }, envelope), false);
});

test("evaluateVerify -- shell booleans read as booleans", () => {
  assert.equal(evaluateVerify({ kind: "resultTrue", field: "result.x" }, { ok: true, result: { x: "true" } }), true);
  assert.equal(evaluateVerify({ kind: "resultFalse", field: "result.x" }, { ok: true, result: { x: "false" } }), true);
  assert.equal(evaluateVerify({ kind: "resultFalse", field: "result.x" }, { ok: true, result: { x: true } }), false);
});

test("evaluateVerify -- nonEmpty and equals", () => {
  const ok = (result: Record<string, unknown>) => ({ ok: true, result });
  assert.equal(evaluateVerify({ kind: "resultNonEmpty", field: "result.x" }, ok({ x: "v" })), true);
  assert.equal(evaluateVerify({ kind: "resultNonEmpty", field: "result.x" }, ok({ x: "  " })), false);
  assert.equal(evaluateVerify({ kind: "resultNonEmpty", field: "result.x" }, ok({ x: [] })), false);
  assert.equal(evaluateVerify({ kind: "resultNonEmpty", field: "result.x" }, ok({ x: [1] })), true);
  assert.equal(evaluateVerify({ kind: "resultEquals", field: "result.x", value: "stack" }, ok({ x: "stack" })), true);
  assert.equal(evaluateVerify({ kind: "resultEquals", field: "result.x", value: "stack" }, ok({ x: "binary" })), false);
  assert.equal(evaluateVerify({ kind: "resultEquals", field: "result.x", value: "3" }, ok({ x: 3 })), true);
});

// -----------------------------------------------------------------------------
// pre-existence
// -----------------------------------------------------------------------------

test("stepPreExisting decodes the grammar", () => {
  const s = (p: string): Step =>
    ({ id: "a", script: "s", description: "d", elevation: "none", verify: { kind: "scriptOk" }, preExistingPath: p }) as Step;
  assert.deepEqual(stepPreExisting(s("result.preExisting")), { field: "preExisting", negate: false });
  assert.deepEqual(stepPreExisting(s("!result.cloned")), { field: "cloned", negate: true });
  assert.equal(stepPreExisting(s("none")), null);
  assert.equal(stepPreExisting({ id: "a" } as Step), null);
});

test("resolvePreExisting -- the negated form is what protects an existing checkout", () => {
  const step = { preExistingPath: "!result.cloned" } as Step;
  // cloned:false means we found a checkout already there -- it pre-existed.
  assert.equal(resolvePreExisting(step, { ok: true, result: { cloned: false } }), true);
  assert.equal(resolvePreExisting(step, { ok: true, result: { cloned: true } }), false);
  // "none" and an absent declaration both mean "we made it".
  assert.equal(resolvePreExisting({ preExistingPath: "none" } as Step, { ok: true, result: {} }), false);
  assert.equal(resolvePreExisting({} as Step, { ok: true, result: {} }), false);
});

test("resolvePreExisting is fail-safe when the field is missing", () => {
  // A receipt that cannot tell must not claim we created it: an absent signal
  // reads as "it was already here", which uninstall refuses to remove.
  assert.equal(resolvePreExisting({ preExistingPath: "result.preExisting" } as Step, { ok: true, result: {} }), true);
});

test("verifyField strips the result. prefix", () => {
  assert.equal(verifyField({ verify: { kind: "resultTrue", field: "result.installed" } } as Step), "installed");
  assert.equal(verifyField({ verify: { kind: "scriptOk" } } as Step), "");
});

// -----------------------------------------------------------------------------
// the substrate is runnable outside VS Code
// -----------------------------------------------------------------------------

test("src/install imports nothing from vscode", async () => {
  const dir = path.join(REPO_ROOT, "editors", "vscode", "src", "install");
  const files = (await fs.readdir(dir)).filter((f) => f.endsWith(".ts"));
  assert.ok(files.length > 0, "no sources found in src/install");
  for (const f of files) {
    const src = await fs.readFile(path.join(dir, f), "utf8");
    assert.doesNotMatch(
      src,
      /from\s+["']vscode["']|require\(\s*["']vscode["']\s*\)/,
      `${f} imports vscode -- the install substrate has to run as plain node from cli.ts, ` +
        `where that module does not exist`,
    );
  }
});
