// The from-source install lane, and being able to name it from a terminal.
//
// memql#5067. `install-main.json` -- the graph a developer's "install from
// main" actually runs -- was exercised by NO CI job. Every
// `install-cluster-e2e` leg installed with a tag:
//
//     TAG="e2e-${GITHUB_SHA::12}"
//     node editors/vscode/dist-install/cli.js install --tag="$TAG"
//
// `isMainBranchChoice("e2e-...")` is false, so `imagesFromSource` is false, so
// the RELEASE graph loads -- and that graph has no `buildImages` step at all.
// `buildImages` is the whole difference between the two lanes, and it has now
// shipped the same class of bug twice (memql#5056 fixed `rebuildFromCheckout`
// and missed it; memql#5064 fixed it). Both were found by an operator on a real
// machine, because no job runs the graph they live in. The green lanes were
// actively misleading: `round-trip` passes and reads like install coverage.
//
// WHY A FLAG RATHER THAN JUST PASSING `--tag=main`. The version and the lane
// are two questions, and a CI job needs opposite answers to them: the from-
// source GRAPH, over the REVISION UNDER TEST. `--tag=main` answers both at once
// and clones main's tip, so a lane built on it would green-light a tree that is
// not the pull request. `SessionOptions.imagesFromSource` was already a
// separate option for the repair path's sake (memql#4430); this only makes it
// nameable.

import assert from "node:assert/strict";
import { mkdtempSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { test } from "node:test";

import { parseCliArgs } from "../src/install/cli.js";
import { installGraphPath, loadGraphFile } from "../src/install/graph.js";
import { imagesFromSource, installPlan, type SessionOptions } from "../src/install/session.js";
import { DEFAULT_IMAGE_REGISTRY } from "../src/install/stackPin.js";
import { type Step } from "../src/install/graph.js";

const REPO_ROOT = path.resolve(__dirname, "..", "..", "..", "..");

function options(over: Partial<SessionOptions> = {}): SessionOptions {
  const dir = mkdtempSync(path.join(os.tmpdir(), "memql-from-source-"));
  return {
    root: REPO_ROOT,
    receiptFile: path.join(dir, "install-receipt.json"),
    skip: new Set<string>(),
    stepParams: {},
    ...over,
  };
}

const STEP = (id: string, script: string): Step => ({
  id,
  script,
  label: "Working",
  description: "",
  elevation: "none",
  retained: false,
  retainedReason: "",
  shared: false,
  sharedReason: "",
  readOnly: false,
  dependsOn: [],
  params: {},
  receipt: "",
  preExistingPath: "",
  containedBy: "",
  timeoutSeconds: 0,
  verify: { kind: "resultTrue", field: "result.ok" },
});

async function stepIds(fromSource: boolean): Promise<string[]> {
  const graph = await loadGraphFile(installGraphPath(REPO_ROOT, fromSource));
  return graph.steps.map((s) => s.id);
}

// ---------------------------------------------------------------------------
// the regression: what CI runs today cannot reach buildImages
// ---------------------------------------------------------------------------

test("the tag every e2e leg installs with selects the graph that has no build step", async () => {
  // Reproduces the lane as it stands. Not a hypothetical spelling: this is the
  // literal shape of `--tag="e2e-${GITHUB_SHA::12}"`.
  const opts = parseCliArgs(["install", "--tag=e2e-0123456789ab"]);
  assert.equal(imagesFromSource(opts), false);

  const release = await stepIds(false);
  assert.ok(
    !release.includes("buildImages"),
    "the release graph must have no buildImages step -- if it grows one, the premise of this\n" +
      "whole issue changes and the from-source leg needs re-arguing",
  );
});

// ---------------------------------------------------------------------------
// the flag
// ---------------------------------------------------------------------------

test("--from-source selects the graph buildImages lives in", async () => {
  const opts = parseCliArgs(["install", "--from-source"]);
  assert.equal(imagesFromSource(opts), true);

  const main = await stepIds(true);
  assert.ok(main.includes("buildImages"), `install-main.json lost its buildImages step: ${main.join(", ")}`);
});

test("--from-source composes with --commit, which is what makes a CI leg test the branch", async () => {
  // THE PAIR NO JOB COULD EXPRESS. `--tag=main` would have answered both
  // questions and cloned main's tip; these answer them separately, so the graph
  // under test is the from-source one and the tree under test is the pull
  // request's head.
  const opts = parseCliArgs(["install", "--commit=deadbeefcafe", "--from-source"]);
  assert.equal(opts.commit, "deadbeefcafe");
  assert.equal(opts.tag, undefined, "the lane must not have to name a version to name the lane");
  assert.equal(imagesFromSource(opts), true);

  const params = installPlan(opts)(STEP("stackCheckout", "install.cloneStack"));
  assert.equal(params.action, "run");
  if (params.action === "run") {
    assert.equal(params.params.commit, "deadbeefcafe", "the checkout must be the revision under test");
    assert.equal(params.params.branch, undefined, "and not a branch, which moves");
  }
});

test("the from-source lane still asks for no registry image", () => {
  // memql#4430's property, re-asserted through the NEW door. A from-source
  // cluster runs `memql-<node>:local`; handing it a GHCR pin is memql#4068 by a
  // route that only opens if this flag forgets what the version choice knew.
  const fromSource = installPlan(options({ imagesFromSource: true }))(STEP("clusterUp", "k3d.up"));
  assert.equal(fromSource.action, "run");
  if (fromSource.action === "run") {
    assert.equal(fromSource.params["image-tag"], undefined);
    assert.equal(fromSource.params["image-registry"], undefined);
  }

  // ...and the release lane is untouched by its existence.
  const released = installPlan(options({ tag: "v9.9.9" }))(STEP("clusterUp", "k3d.up"));
  assert.equal(released.action, "run");
  if (released.action === "run") {
    assert.equal(released.params["image-tag"], "9.9.9");
    assert.equal(released.params["image-registry"], DEFAULT_IMAGE_REGISTRY);
  }
});

// The build step has to be told WHICH checkout to build, and the CI lane
// reaches it by `--from-source` rather than by `--tag=main` -- so the assertion
// that already covers the wizard's path does not cover this one.
//
// It also builds ALL app nodes, and that is forced rather than chosen: session.ts
// records it as "a fresh install has no such thing as a node it can leave
// behind", because a partial build leaves the rest pulling an image nobody
// published.
test("the from-source lane tells the build which checkout to build", () => {
  const opts = parseCliArgs(["install", "--commit=deadbeefcafe", "--from-source"]);
  const decision = installPlan(opts)(STEP("buildImages", "k3d.dev"));
  assert.equal(decision.action, "run");
  if (decision.action === "run") {
    assert.ok(decision.params["repo-root"], "buildImages was not told which checkout to build");
    assert.equal(decision.params.node, undefined, "a fresh install must build every app node");
    // The lane is the GRAPH's to pin, not a caller's -- a plan that passed it
    // would make "build, but keep running released images" reachable.
    assert.equal(decision.params["image-source"], undefined);
  }
});

test("a repair is refused the flag, for the reason it is refused a version", () => {
  // The receipt records which lane the install ran on. A flag here could only
  // contradict that record or repeat it, and contradicting it would rebuild a
  // release install's images from its checkout and call the result a repair --
  // which is the lane crossing memql#4246 gives its own verb.
  assert.throws(() => parseCliArgs(["repair", "--from-source"]), /from-source/);
  assert.equal(parseCliArgs(["repair"]).imagesFromSource, undefined);
});

test("the flag is named in the unknown-flag message, so a typo is answerable", () => {
  try {
    parseCliArgs(["install", "--fromsource"]);
    assert.fail("--fromsource should be unknown");
  } catch (err) {
    assert.match((err as Error).message, /from-source/);
  }
});
