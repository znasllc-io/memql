// Installing from `main`, and keeping "what is on this machine" answerable.
//
// znasllc-io/memql#3901, split out of memql#3882. The wizard can pick any
// release TAG; it could not pick `main`, which is what an operator wants when
// the fix they are waiting on has landed and no tag carries it yet -- the exact
// situation that produced #3882 ("the install faithfully deployed v0.17.1, and
// all three fixes lived on main").
//
// clone-stack.sh refused a branch, deliberately, and the refusal protected a
// real property: a branch MOVES, so two installs of "the same version" a week
// apart are not the same install and nobody can say afterwards what a machine
// is running. The property is not "never check out a branch" -- it is "the
// answer must survive". Resolve the branch to a COMMIT, record the commit, and
// it does.
//
// Two things this file pins that no other test can:
//
//   1. A `main` install never asks for a `main` IMAGE. build-engine-images.yml
//      publishes memql-<node>:<version> on a release dispatch only; there is no
//      main tag in GHCR, so `imageTagFor("main")` would be an ImagePullBackOff
//      on every pod. This is the failure the feature invites and nothing else
//      would catch.
//   2. A repair of a branch install replays the COMMIT, not the branch.
//      Replaying `--branch=main` would check out wherever main is today -- so
//      "repair" would silently mean "upgrade", which is memql#3605's failure
//      arriving by the one route that reopens it.

import assert from "node:assert/strict";
import { mkdtempSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { test } from "node:test";

import { installPlan, type SessionOptions } from "../src/install/session.js";
import {
  DEFAULT_STACK_TAG,
  MAIN_BRANCH_CHOICE,
  imageTagFor,
  imageTagForVersion,
  isMainBranchChoice,
} from "../src/install/stackPin.js";
import {
  emptyReceipt,
  recordedCheckout,
  recordedStackCommit,
  recordedStackRefKind,
  recordedStackTag,
  type Receipt,
  type ReceiptEntry,
} from "../src/install/receipt.js";
import { versionChoiceList } from "../src/webview/installScreens.js";
import { isReleaseTag } from "../src/install/tags.js";
import type { Step } from "../src/install/graph.js";

function options(over: Partial<SessionOptions> = {}): SessionOptions {
  const dir = mkdtempSync(path.join(os.tmpdir(), "memql-main-branch-"));
  return {
    root: "/nonexistent",
    receiptFile: path.join(dir, "install-receipt.json"),
    skip: new Set<string>(),
    provider: "anthropic",
    stepParams: {},
    ...over,
  };
}

const STEP = (id: string, script: string): Step => ({
  id,
  script,
  description: "",
  elevation: "none",
  retained: false,
  retainedReason: "",
  shared: false,
  sharedReason: "",
  verify: { kind: "scriptOk" },
});

function paramsFor(opts: SessionOptions, id: string, script: string): Record<string, string> {
  const decision = installPlan(opts)(STEP(id, script));
  assert.equal(decision.action, "run", `${id} was not planned as a run`);
  return decision.action === "run" ? decision.params : {};
}

// ---------------------------------------------------------------------------
// which ref the checkout is asked for
// ---------------------------------------------------------------------------

test("a release tag still goes out as --tag, unchanged", () => {
  const params = paramsFor(options({ tag: "v9.9.9" }), "stackCheckout", "install.cloneStack");
  assert.equal(params.tag, "v9.9.9");
  assert.equal(params.branch, undefined);
  assert.equal(params.commit, undefined);
});

test("main goes out as --branch, never as --tag", () => {
  // clone-stack.sh refuses a BARE branch name in --tag and always will. The
  // whole point of the labelled flag is that asking for a branch is deliberate.
  const params = paramsFor(
    options({ tag: MAIN_BRANCH_CHOICE }),
    "stackCheckout",
    "install.cloneStack",
  );
  assert.equal(params.branch, MAIN_BRANCH_CHOICE);
  assert.equal(params.tag, undefined, "sending main as --tag is exit 2 by design");
});

test("a recorded commit outranks whatever version the session carries", () => {
  // The repair case. A repair runs inside a session that may still be holding
  // the version field's value; the receipt has to win, or the repair upgrades.
  const params = paramsFor(
    options({ tag: "v9.9.9", commit: "a".repeat(40) }),
    "stackCheckout",
    "install.cloneStack",
  );
  assert.equal(params.commit, "a".repeat(40));
  assert.equal(params.tag, undefined);
  assert.equal(params.branch, undefined);
});

// ---------------------------------------------------------------------------
// the images a main install runs
// ---------------------------------------------------------------------------

test("a main install never asks for a main image tag", () => {
  // THE FAILURE THIS FEATURE INVITES. There is no memql-<node>:main in GHCR --
  // build-engine-images.yml publishes on an explicit release dispatch only --
  // so passing the version straight through is ImagePullBackOff on every pod.
  const params = paramsFor(
    options({ tag: MAIN_BRANCH_CHOICE, latestRelease: "v0.19.0" }),
    "clusterUp",
    "k3d.up",
  );
  assert.equal(params["image-tag"], "0.19.0");
  assert.notEqual(params["image-tag"], "main");
});

test("a main install with no listing falls back to the pin, which is a real release", () => {
  const params = paramsFor(options({ tag: MAIN_BRANCH_CHOICE }), "clusterUp", "k3d.up");
  assert.equal(params["image-tag"], imageTagFor(DEFAULT_STACK_TAG));
  assert.ok(isReleaseTag(DEFAULT_STACK_TAG), "the fallback must be a published release");
});

test("a tag install ignores latestRelease entirely", () => {
  const params = paramsFor(
    options({ tag: "v9.9.9", latestRelease: "v0.19.0" }),
    "clusterUp",
    "k3d.up",
  );
  assert.equal(params["image-tag"], "9.9.9");
});

test("imageTagForVersion never returns a branch name", () => {
  assert.equal(imageTagForVersion("main", "v0.19.0"), "0.19.0");
  assert.equal(imageTagForVersion("v1.2.3", "v0.19.0"), "1.2.3");
  assert.equal(imageTagForVersion("", "v0.19.0"), imageTagFor(DEFAULT_STACK_TAG));
});

test("main is not mistaken for a release tag anywhere", () => {
  assert.equal(isReleaseTag(MAIN_BRANCH_CHOICE), false);
  assert.equal(isMainBranchChoice(MAIN_BRANCH_CHOICE), true);
  assert.equal(isMainBranchChoice("v0.18.0"), false);
  assert.equal(isMainBranchChoice("  main  "), true, "the picker's value may arrive padded");
});

// ---------------------------------------------------------------------------
// what the receipt records, and what a repair replays
// ---------------------------------------------------------------------------

function receiptWith(result: Record<string, unknown>, params: Record<string, string> = {}): Receipt {
  const entry: ReceiptEntry = {
    stepId: "stackCheckout",
    script: "install.cloneStack",
    receipt: "checkout",
    preExisting: false,
    params,
    result,
    changed: true,
    recordedAt: "2026-08-16T00:00:00.000Z",
  };
  const receipt = emptyReceipt("install");
  receipt.entries.push(entry);
  return receipt;
}

test("a tag install answers with a tag, and a repair replays it", () => {
  const receipt = receiptWith(
    { tag: "v0.18.0", ref: "v0.18.0", refKind: "tag", commit: "b".repeat(40) },
    { tag: "v0.18.0" },
  );
  assert.equal(recordedStackTag(receipt), "v0.18.0");
  assert.equal(recordedStackRefKind(receipt), "tag");

  const replay = recordedCheckout(receipt);
  assert.equal(replay.tag, "v0.18.0");
  assert.equal(replay.commit, "", "replaying an immutable tag is the unchanged behaviour");
  assert.equal(replay.label, "v0.18.0");
});

test("a branch install answers with a COMMIT, never with the branch", () => {
  // "Which version is this cluster?" must answer a commit for a branch install
  // and a tag for a tag install -- never a bare ref.
  const receipt = receiptWith(
    { tag: "", ref: "main", refKind: "branch", commit: "c".repeat(40) },
    { branch: "main" },
  );
  assert.equal(recordedStackCommit(receipt), "c".repeat(40));
  assert.equal(
    recordedStackTag(receipt),
    "",
    "a branch install has no release; reporting one would send imageTagFor a branch name",
  );

  const replay = recordedCheckout(receipt);
  assert.equal(replay.commit, "c".repeat(40));
  assert.equal(replay.tag, "", "replaying --branch=main would be an upgrade, not a repair");
  assert.equal(replay.label, "ccccccc");
});

test("a repair of a branch install plans --commit end to end", () => {
  // The whole chain, because each half looked right on its own the last time
  // this class of bug shipped (memql#3605).
  const receipt = receiptWith({ refKind: "branch", ref: "main", commit: "d".repeat(40) });
  const replay = recordedCheckout(receipt);
  const params = paramsFor(
    options({ tag: replay.tag || "v9.9.9", commit: replay.commit }),
    "stackCheckout",
    "install.cloneStack",
  );
  assert.equal(params.commit, "d".repeat(40));
  assert.equal(params.branch, undefined, "a repair must not re-resolve the branch");
});

test("a receipt written before refKind existed is treated as a tag install", () => {
  // Correct by construction: nothing but a tag install was possible then.
  const receipt = receiptWith({ tag: "v0.17.1", commit: "e".repeat(40) }, { tag: "v0.17.1" });
  assert.equal(recordedStackRefKind(receipt), "");
  const replay = recordedCheckout(receipt);
  assert.equal(replay.tag, "v0.17.1");
  assert.equal(replay.commit, "");
});

test("no receipt answers nothing rather than guessing", () => {
  const replay = recordedCheckout(null);
  assert.equal(replay.tag, "");
  assert.equal(replay.commit, "");
  assert.equal(replay.label, "");
});

// ---------------------------------------------------------------------------
// how the picker presents it
// ---------------------------------------------------------------------------

test("main is offered last, apart from the release tags, and labelled", () => {
  const list = versionChoiceList(["v0.19.0", "v0.18.0"], "v0.18.0", "v0.19.0");
  const values = list.map((c) => c.value);
  assert.deepEqual(values, ["v0.18.0", "v0.19.0", "main"]);

  const main = list[list.length - 1]!;
  assert.equal(main.value, "main");
  assert.notEqual(main.label, "main", "a bare 'main' reads as just another tag in a list of tags");
  assert.match(main.label, /development/);
  // The label must name the images, because that skew is the one thing an
  // operator cannot discover from anywhere else.
  assert.match(main.label, /v0\.19\.0/);
});

test("the selected version stays first, and main is not duplicated", () => {
  const list = versionChoiceList(["v0.19.0", "main", "v0.18.0"], "v0.19.0", "v0.19.0");
  assert.deepEqual(list.map((c) => c.value), ["v0.19.0", "v0.18.0", "main"]);
  assert.equal(list.filter((c) => c.value === "main").length, 1);
});

test("choosing main does not pin it to the top as a selected release", () => {
  const list = versionChoiceList(["v0.19.0", "v0.18.0"], "main", "v0.19.0");
  assert.deepEqual(list.map((c) => c.value), ["v0.19.0", "v0.18.0", "main"]);
});
