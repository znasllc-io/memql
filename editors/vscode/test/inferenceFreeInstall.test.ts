// THE D8 ASSERTION (epic memql#4676, task memql#4686).
//
// Install, uninstall, repair and update complete with NO MODEL AND NO KEY
// anywhere, make no inference call, and gate on none.
//
// ===========================================================================
// AN ASSERTION THAT A CALL WAS NOT MADE MUST BE ABLE TO DETECT ONE
// ===========================================================================
// "We did not observe an inference call" is not evidence; a test that observes
// nothing passes identically against code that makes no calls and against code
// whose calls it cannot see. So the seams are stubbed to FAIL LOUDLY: every
// outbound network primitive the extension could reach an LLM through is
// replaced with a function that throws, and the throw is what the test is
// listening for. A flow that touched one takes the test down with a message
// naming the seam.
//
// The negative control is checked in the same file: `tripwires reachable`
// proves the tripwires fire when something DOES call them. Without it, a
// refactor that renamed a seam would leave every case here green while
// guarding nothing.
//
// ===========================================================================
// WHAT COUNTS AS "THE FLOWS"
// ===========================================================================
// The shipped step graphs -- scripts/install/graph/install.json and
// uninstall.json -- are the single description of what an install and an
// uninstall do. Repair and update are the same graph documents driven with
// different retained/pre-existing state, which is why they are covered here by
// reading the same documents rather than by a second fixture that could
// describe a flow nobody runs.

import { strict as assert } from "node:assert";
import test from "node:test";
import * as path from "node:path";

import { graphDocumentPath, loadGraphFile, topoOrder } from "../src/install/graph";
import { CAPABILITY_SCRIPTS } from "../src/install/runner";
import { offerFromProbe, probeForLocalModels } from "../src/install/ollama";

const REPO_ROOT = path.resolve(__dirname, "..", "..", "..", "..");

// The seams an inference call would have to leave through. Every one is
// replaced with a thrower for the duration of a case.
interface Tripwire {
  name: string;
  install: () => void;
  restore: () => void;
}

function tripwires(): Tripwire[] {
  const wires: Tripwire[] = [];

  const realFetch = globalThis.fetch;
  wires.push({
    name: "fetch",
    install: () => {
      (globalThis as { fetch: unknown }).fetch = (input: unknown) => {
        throw new Error(`D8 VIOLATION: an install flow called fetch(${String(input)})`);
      };
    },
    restore: () => {
      (globalThis as { fetch: unknown }).fetch = realFetch;
    },
  });

  for (const mod of ["node:https", "node:http"]) {
    // eslint-disable-next-line @typescript-eslint/no-var-requires
    const lib = require(mod) as { request: unknown; get: unknown };
    const realRequest = lib.request;
    const realGet = lib.get;
    wires.push({
      name: mod,
      install: () => {
        lib.request = () => {
          throw new Error(`D8 VIOLATION: an install flow opened a ${mod} request`);
        };
        lib.get = () => {
          throw new Error(`D8 VIOLATION: an install flow opened a ${mod} GET`);
        };
      },
      restore: () => {
        lib.request = realRequest;
        lib.get = realGet;
      },
    });
  }
  return wires;
}

async function withTripwires<T>(fn: () => Promise<T> | T): Promise<T> {
  const wires = tripwires();
  for (const w of wires) w.install();
  try {
    return await fn();
  } finally {
    for (const w of wires) w.restore();
  }
}

// THE NEGATIVE CONTROL. Without this, a renamed seam leaves every case below
// green while guarding nothing.
test("the tripwires actually fire", async () => {
  let fired = "";
  await withTripwires(async () => {
    try {
      await (globalThis.fetch as (u: string) => Promise<unknown>)("https://api.anthropic.com/v1/messages");
    } catch (err) {
      fired = err instanceof Error ? err.message : String(err);
    }
  });
  assert.match(fired, /D8 VIOLATION/, "the fetch tripwire did not fire, so every case in this file guards nothing");
});

test("no environment variable in the install flows names a model or a key", async () => {
  // A flow that GATED on inference would have to read a credential from
  // somewhere. The graph documents name every env the steps use.
  for (const kind of ["install", "uninstall"] as const) {
    const graph = await loadGraphFile(graphDocumentPath(kind, REPO_ROOT));
    const text = JSON.stringify(graph);
    for (const forbidden of [
      "ANTHROPIC_API_KEY",
      "OPENAI_API_KEY",
      "MEMQL_AI_ANTHROPIC_API_KEY",
      "MEMQL_AI_OPENAI_API_KEY",
    ]) {
      assert.equal(
        text.includes(forbidden),
        false,
        `the ${kind} graph names ${forbidden}. Install, uninstall, repair and update must complete ` +
          `with no key anywhere (design D8): a step that reads one has made inference a ` +
          `prerequisite of installing the product.`,
      );
    }
  }
});

test("no step of install or uninstall runs a capability that needs a model", async () => {
  // The capability allowlist is the complete set of things a step can run.
  // None of them is an inference call, and a step naming one that is not in
  // the allowlist cannot run at all.
  for (const kind of ["install", "uninstall"] as const) {
    const graph = await loadGraphFile(graphDocumentPath(kind, REPO_ROOT));
    for (const step of graph.steps) {
      assert.ok(
        CAPABILITY_SCRIPTS[step.script],
        `${kind} step ${step.id} runs ${step.script}, which is not an allowlisted capability`,
      );
      assert.equal(
        step.script.startsWith("ai.") || step.script.includes("infer") || step.script.includes("model"),
        false,
        `${kind} step ${step.id} runs ${step.script}, which reads as an inference call`,
      );
    }
  }
});

test("loading and ordering every flow makes no outbound call", async () => {
  // The graph load + topological order is everything the extension does
  // BEFORE it starts running steps -- the part that decides what the flow
  // will be. If inference had crept into that decision, it would fire here.
  await withTripwires(async () => {
    for (const kind of ["install", "uninstall"] as const) {
      const graph = await loadGraphFile(graphDocumentPath(kind, REPO_ROOT));
      assert.ok(graph.steps.length > 0, `${kind} graph has no steps`);
      assert.ok(topoOrder(graph).length > 0, `${kind} graph produced no waves`);
    }
  });
});

test("the local-model probe never throws, whatever it finds", async () => {
  // A probe that could abort a flow would make an inference-free machine
  // unable to install MemQL -- the exact property D8 exists to guarantee.
  const answers = [
    // No runtime.
    { exitCode: 0, envelope: { ok: true, capability: "install.detectOllama", changed: false, result: { found: false, models: [] }, error: null }, stdout: "", stderr: "", parseError: undefined },
    // The probe itself failed.
    { exitCode: 4, envelope: { ok: false, capability: "install.detectOllama", changed: false, result: {}, error: { code: 4, message: "curl is required" } }, stdout: "", stderr: "", parseError: undefined },
    // No envelope at all.
    { exitCode: 127, envelope: null, stdout: "", stderr: "no such file", parseError: "no envelope" },
  ];
  for (const answer of answers) {
    const probe = await probeForLocalModels(
      { root: REPO_ROOT },
      { run: async () => answer as never },
    );
    assert.equal(probe.found, false);
  }

  // Even a runner that THROWS comes back as a value.
  const thrown = await probeForLocalModels(
    { root: REPO_ROOT },
    {
      run: async () => {
        throw new Error("the runner exploded");
      },
    },
  );
  assert.equal(thrown.found, false);
  assert.match(thrown.error, /exploded/);
});

test("a machine with no runtime is offered nothing at all", () => {
  // An install is not the moment to sell somebody a capability they did not
  // ask for, and a dialog that appears only on the machines that CANNOT use
  // the feature is the worst possible place to put one.
  assert.equal(offerFromProbe({ found: false, endpoint: "", runtime: "", models: [], error: "" }).show, false);
  // A probe that could not tell offers nothing either: we say nothing rather
  // than guessing in either direction.
  assert.equal(
    offerFromProbe({ found: true, endpoint: "x", runtime: "ollama", models: ["a"], error: "boom" }).show,
    false,
  );
});

test("a machine WITH models gets a real offer -- the reachable positive", () => {
  // Without this, the two cases above would pass identically against a
  // function that never offers anything.
  const offer = offerFromProbe({
    found: true,
    endpoint: "http://127.0.0.1:11434",
    runtime: "ollama",
    models: ["llama3.1:8b", "nomic-embed-text"],
    error: "",
  });
  assert.equal(offer.show, true);
  assert.match(offer.detail, /llama3\.1:8b/);
  assert.match(offer.detail, /nomic-embed-text/);
  assert.ok(offer.accept.length > 0);
});

test("a runtime with no models is told what to install, not that it failed", () => {
  const offer = offerFromProbe({
    found: true,
    endpoint: "http://127.0.0.1:11434",
    runtime: "ollama",
    models: [],
    error: "",
  });
  assert.equal(offer.show, true);
  assert.match(offer.detail, /7-8B instruct/);
});

test("the probe's capability id resolves the same script the installer runs", () => {
  // Two entry points into ONE behaviour, rather than two behaviours that
  // agree today.
  assert.equal(CAPABILITY_SCRIPTS["install.detectOllama"], "scripts/install/detect-ollama.sh");
});
