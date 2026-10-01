// The machine's one run slot, shared by the two pages that change the local
// cluster.
//
// The Deployment page's runs (update, change version, rebuild, pull and
// rebuild) are LocalRuns the slot owns. The Add Cluster page's (install,
// repair, uninstall) live in that page and HOLD the slot while they go. Two at
// once is two answers to what the machine is -- a rebuild under an uninstall
// rebuilds images into a cluster being deleted -- so whichever page asks
// second is refused, in one sentence, with Show.
//
// The pages' own halves of this are in test/deploymentPanel.test.ts and
// test/addClusterPanel.test.ts; these cases hold the slot's rules.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";

import { LocalRuns, SLOT_SHOW, slotRefusal, type LocalRunDeps } from "../src/deploy/localRun.js";
import { loadGraph, type Graph } from "../src/install/graph.js";
import type { RunScript, ScriptOutcome } from "../src/install/runner.js";

const REPO_ROOT = path.resolve(__dirname, "..", "..", "..", "..");

const ONE_STEP: Graph = loadGraph(
  JSON.stringify({
    name: "test-update",
    kind: "install",
    steps: [
      {
        id: "fetch",
        label: "Downloading MemQL",
        description: "fetch",
        script: "install.binary",
        elevation: "none",
        retained: false,
        retainedReason: "",
        shared: false,
        sharedReason: "",
        receipt: "fetch",
        preExistingPath: "none",
        verify: { kind: "resultTrue", field: "result.installed" },
      },
    ],
  }),
  "test-fixture",
);

const OK: ScriptOutcome = {
  argv: [],
  exitCode: 0,
  signal: null,
  stdout: "",
  stderr: "",
  envelope: { ok: true, capability: "t", changed: true, result: { installed: true }, error: null },
};

/** A runner whose steps answer only when the case says so; the Docker gate answers at once. */
function heldRunner(): { run: RunScript; release: () => void } {
  const pending: ((outcome: ScriptOutcome) => void)[] = [];
  let released = false;
  const run: RunScript = async ({ capability }) => {
    if (capability === "install.dockerAccess" || released) return OK;
    return new Promise<ScriptOutcome>((resolve) => pending.push(resolve));
  };
  return {
    run,
    release: () => {
      released = true;
      for (const finish of pending.splice(0)) finish(OK);
    },
  };
}

function deps(run: RunScript, reveal?: () => void): LocalRunDeps {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "memql-slot-"));
  return {
    installRoot: REPO_ROOT,
    receiptFile: path.join(dir, "install-receipt.json"),
    runsDir: path.join(dir, "runs"),
    runScript: run,
    graphs: { update: ONE_STEP },
    ...(reveal !== undefined ? { reveal } : {}),
  };
}

test("an empty slot is not busy, and either page may take it", () => {
  const runs = new LocalRuns();
  assert.equal(runs.busy(), undefined);
  const release = runs.hold({ busy: "installing", reveal: () => undefined });
  assert.ok(release !== undefined, "the Add Cluster page could not take an empty slot");
  release();
  assert.equal(runs.busy(), undefined, "a released slot is free again");
});

test("a run the Add Cluster page holds refuses a Deployment page run", () => {
  const runs = new LocalRuns();
  let revealed = 0;
  const release = runs.hold({ busy: "uninstalling", reveal: () => (revealed += 1) });
  assert.ok(release !== undefined);

  const runner = heldRunner();
  assert.equal(runs.start({ kind: "update", instance: "local", from: "v0.23.5", to: "v0.24.0" }, deps(runner.run)), undefined);
  assert.equal(runs.current, undefined, "a refused run leaves no run behind");
  assert.equal(runs.inFlight, false, "the slot's own flag is about its own runs");

  const busy = runs.busy();
  assert.ok(busy !== undefined);
  assert.equal(slotRefusal(busy), "MemQL: The local cluster is busy uninstalling.");
  busy.reveal();
  assert.equal(revealed, 1, "Show goes to the page running it");

  // A second hold is refused too: one run at a time, whichever page.
  assert.equal(runs.hold({ busy: "installing", reveal: () => undefined }), undefined);
  release!();
  assert.ok(runs.start({ kind: "update", instance: "local", from: "v0.23.5", to: "v0.24.0" }, deps(runner.run)) !== undefined);
});

test("a Deployment page run refuses the Add Cluster page, and Show reaches its page", async () => {
  const runs = new LocalRuns();
  const runner = heldRunner();
  let revealed = 0;
  const run = runs.start(
    { kind: "update", instance: "local", label: "memql.localhost", from: "v0.23.5", to: "v0.24.0" },
    deps(runner.run, () => (revealed += 1)),
  );
  assert.ok(run !== undefined);
  assert.equal(runs.hold({ busy: "installing", reveal: () => undefined }), undefined, "an install started under an update");

  const busy = runs.busy();
  assert.ok(busy !== undefined);
  assert.equal(slotRefusal(busy), "MemQL: The local cluster is busy updating.");
  busy.reveal();
  assert.equal(revealed, 1, "Show brings the Deployment page forward on the run");

  runner.release();
  await run!.settled();
  assert.equal(runs.busy(), undefined, "a settled run frees the slot");
  assert.ok(runs.hold({ busy: "installing", reveal: () => undefined }) !== undefined);
});

test("the refusal is one sentence, and its button says what it does", () => {
  const sentence = slotRefusal({ busy: "rebuilding", reveal: () => undefined });
  assert.equal(sentence.split(". ").length, 1);
  assert.equal(SLOT_SHOW, "Show");
});
