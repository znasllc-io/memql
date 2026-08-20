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

import { parseCliArgs, repairOptions, type CliOptions } from "../src/install/cli.js";
import { installPlan, type SessionOptions } from "../src/install/session.js";
import { REDACTED } from "../src/install/secrets.js";
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
  recordedImageTag,
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
// the repair VERB -- `cli.js repair`
// ---------------------------------------------------------------------------
//
// The tests above pin the RULE (`recordedCheckout`) and the PLAN
// (`installPlan`). What they cannot see is a caller that has both in hand and
// wires them together wrongly -- which is the whole surface of a repair, and
// which had exactly one implementation until the CLI grew a second: the wizard,
// behind a `vscode` import that the unit lane excludes by design.
//
// That is the same shape as memql#3560, where `tag` was simply absent from the
// wizard's object literal and every install from the "+" button died while the
// CLI's worked. So the CLI verb calls `recordedCheckout` rather than deriving
// the ref itself, and these tests assert the end-to-end flags, because a repair
// that quietly reinstalls this build's pin looks exactly like a repair that
// worked.

/**
 * A receipt as a completed install writes one, minus what a test overrides.
 *
 * `clusterParams` is UNDEFINED by default rather than `{}`, and the difference
 * is the whole of the memql#4068 fallback: a receipt with no `clusterUp` entry
 * at all is what an install that never got that far -- or one written before
 * the installer passed `--image-tag` -- actually looks like, and a repair of one
 * has nothing to replay and must fall back to deriving.
 */
function recordedInstall(
  checkout: Record<string, unknown>,
  checkoutParams: Record<string, string> = {},
  providerParams: Record<string, string> = {},
  clusterParams?: Record<string, string>,
): Receipt {
  const receipt = receiptWith(checkout, checkoutParams);
  if (clusterParams !== undefined) {
    receipt.entries.push({
      stepId: "clusterUp",
      script: "k3d.up",
      receipt: "stack",
      preExisting: false,
      params: { cluster: "memql", ...clusterParams },
      result: { cluster: "memql", clusterCreated: true, workloadsReady: true },
      changed: true,
      recordedAt: "2026-08-16T00:00:00.000Z",
    });
  }
  receipt.entries.push({
    stepId: "providerKey",
    script: "install.verifyProviderKey",
    receipt: "",
    preExisting: false,
    params: { "key-file": "/home/dev/.memql/key", provider: "openai", ...providerParams },
    result: { valid: true },
    changed: false,
    recordedAt: "2026-08-16T00:00:00.000Z",
  });
  receipt.entries.push({
    stepId: "seedBootstrap",
    script: "install.seedBootstrap",
    receipt: "",
    preExisting: false,
    params: {
      domain: "lab.example.com",
      "owner-email": "dev@example.com",
      "owner-first-name": "Dev",
      "owner-last-name": "Eloper",
    },
    result: { bootstrapComplete: true },
    changed: true,
    recordedAt: "2026-08-16T00:00:00.000Z",
  });
  return receipt;
}

/** `cli.js repair <argv>`, parsed. */
function repairArgs(argv: string[] = []): CliOptions {
  return parseCliArgs(["repair", "--receipt=/nonexistent/install-receipt.json", ...argv]);
}

test("repair is a verb, and it refuses to be handed a version", () => {
  assert.equal(parseCliArgs(["repair"]).command, "repair");
  // Upgrading is a different verb, picked by name. A `--tag` that appeared to
  // be accepted and was then silently overridden by the receipt would be worse
  // than the refusal: it reads as an upgrade that worked.
  assert.throws(() => parseCliArgs(["repair", "--tag=v9.9.9"]), /repair takes no --tag/);
  assert.throws(() => parseCliArgs(["repair", "--commit=" + "a".repeat(40)]), /repair takes no --tag/);
  // ...and install still takes both.
  assert.equal(parseCliArgs(["install", "--tag=v9.9.9"]).tag, "v9.9.9");
  assert.equal(parseCliArgs(["install", "--commit=" + "a".repeat(40)]).commit, "a".repeat(40));
});

test("a repair of a BRANCH install plans --commit, through the verb", () => {
  // THE FAILURE THE VERB EXISTS TO MAKE UNREACHABLE (memql#3605 by memql#3901's
  // route). If `repairOptions` read the tag instead of asking
  // `recordedCheckout`, a branch install's empty tag would fall through to
  // DEFAULT_STACK_TAG here and "repair" would mean "install the pinned
  // release" -- green, quiet, and an upgrade.
  const receipt = recordedInstall({ tag: "", ref: "main", refKind: "branch", commit: "d".repeat(40) }, { branch: "main" });
  const params = paramsFor(repairOptions(repairArgs(), receipt), "stackCheckout", "install.cloneStack");
  assert.equal(params.commit, "d".repeat(40));
  assert.equal(params.tag, undefined, "replaying the pin would be an upgrade wearing a repair's name");
  assert.equal(params.branch, undefined, "replaying --branch=main checks out wherever main is today");
});

test("a repair of a TAG install replays that tag, not this build's pin", () => {
  const receipt = recordedInstall({ tag: "v0.17.1", ref: "v0.17.1", refKind: "tag", commit: "e".repeat(40) }, { tag: "v0.17.1" });
  const params = paramsFor(repairOptions(repairArgs(), receipt), "stackCheckout", "install.cloneStack");
  assert.equal(params.tag, "v0.17.1");
  assert.notEqual(params.tag, DEFAULT_STACK_TAG, "a newer extension must not upgrade what it repairs");
  assert.equal(params.commit, undefined, "a release tag is immutable by convention; replaying it is enough");
});

// ---------------------------------------------------------------------------
// ...AND THE IMAGES IT RAN (memql#4068)
// ---------------------------------------------------------------------------
//
// The two tests above pin the CHECKOUT half of "a repair returns the cluster to
// the state its receipt describes". That half held; the images half did not,
// and the gap is exactly the shape of a branch install.
//
// `recordedCheckout()` answers a branch install with a COMMIT and an EMPTY tag
// -- deliberately, because there is no release, and answering "main" would send
// `imageTagFor` a branch name. The image tag was then derived a SECOND time from
// that same empty tag, which falls straight through to DEFAULT_STACK_TAG. So a
// repair run from a newer extension build replayed the recorded commit's manifests
// against a DIFFERENT release's engine images: an upgrade nobody asked for (the
// thing memql#3605 refuses) plus a manifest/image skew nobody chose.
//
// The fix is the same shape as memql#3901's: record the RESOLVED value and
// replay it, rather than deriving it again somewhere else. So these tests assert
// against the derivation itself -- `notEqual(planned, imageTagForVersion(...))`
// is the statement "nobody re-derived", and it is the assertion that fails if
// someone reintroduces the second derivation.

test("a repair of a BRANCH install replays the recorded IMAGE tag, not this build's pin", () => {
  // THE BUG. Recorded: a branch install of v0.17.1's images. This build pins
  // something newer, and the derivation has no tag to work from.
  const receipt = recordedInstall(
    { tag: "", ref: "main", refKind: "branch", commit: "d".repeat(40) },
    { branch: "main" },
    {},
    { "image-tag": "0.17.1" },
  );
  const opts = repairOptions(repairArgs(), receipt);
  const params = paramsFor(opts, "clusterUp", "k3d.up");

  assert.equal(
    params["image-tag"],
    "0.17.1",
    "a repair planned images the receipt does not describe -- the checkout was replayed and the images were not",
  );
  assert.notEqual(
    params["image-tag"],
    imageTagFor(DEFAULT_STACK_TAG),
    "replaying this build's pin is an upgrade wearing a repair's name",
  );
  // The load-bearing one: it must not merely HAPPEN to agree, it must not have
  // been derived at all. Re-deriving from a branch install's empty tag is the
  // defect, and this is the assertion that catches its return.
  assert.notEqual(
    params["image-tag"],
    imageTagForVersion(opts.tag ?? "", opts.latestRelease ?? ""),
    "the image tag was derived a second time rather than replayed from the receipt",
  );
});

test("a repair of a TAG install replays its recorded image tag too", () => {
  // THE CASE THAT ALREADY WORKED, pinned so a fix for branch installs cannot
  // regress it. A tag install's derivation and its recorded tag agree by
  // construction, which is precisely why nothing here noticed the branch case.
  const receipt = recordedInstall(
    { tag: "v0.17.1", ref: "v0.17.1", refKind: "tag", commit: "e".repeat(40) },
    { tag: "v0.17.1" },
    {},
    { "image-tag": "0.17.1" },
  );
  const params = paramsFor(repairOptions(repairArgs(), receipt), "clusterUp", "k3d.up");
  assert.equal(params["image-tag"], "0.17.1");
  assert.notEqual(
    params["image-tag"],
    imageTagFor(DEFAULT_STACK_TAG),
    "a newer extension build must not move the images of what it repairs",
  );
});

test("a receipt that never recorded an image tag falls back to deriving one", () => {
  // EVERY RECEIPT WRITTEN BEFORE `--image-tag` EXISTED, and every install that
  // failed before clusterUp. There is nothing to replay, so the derivation is
  // the only answer available -- and for a tag install it is the right one.
  const receipt = recordedInstall(
    { tag: "v0.17.1", ref: "v0.17.1", refKind: "tag", commit: "e".repeat(40) },
    { tag: "v0.17.1" },
  );
  const opts = repairOptions(repairArgs(), receipt);
  const params = paramsFor(opts, "clusterUp", "k3d.up");
  assert.equal(params["image-tag"], "0.17.1");
  assert.equal(
    params["image-tag"],
    imageTagForVersion(opts.tag ?? "", opts.latestRelease ?? ""),
    "with nothing recorded the plan must derive, not pass an empty --image-tag",
  );
});

test("the recorded image tag is read off clusterUp, and travels with the checkout", () => {
  // WHERE IT COMES FROM. `--image-tag` is a `clusterUp` param, not a
  // `stackCheckout` one, and it is read from the PARAMS rather than the result
  // -- k3d.up reports no image tag on its envelope, and the param is already
  // the resolved value because `installPlan` resolves before it builds the flag.
  const branch = recordedInstall(
    { tag: "", ref: "main", refKind: "branch", commit: "d".repeat(40) },
    { branch: "main" },
    {},
    { "image-tag": "0.17.1" },
  );
  assert.equal(recordedImageTag(branch), "0.17.1");

  // ...and it rides in the same struct as the ref, so a caller that replays the
  // checkout cannot replay only half of what the receipt describes.
  const replay = recordedCheckout(branch);
  assert.equal(replay.commit, "d".repeat(40));
  assert.equal(replay.tag, "");
  assert.equal(replay.imageTag, "0.17.1");

  // A tag install carries both halves too.
  const tagged = recordedCheckout(
    recordedInstall(
      { tag: "v0.17.1", refKind: "tag", commit: "e".repeat(40) },
      { tag: "v0.17.1" },
      {},
      { "image-tag": "0.17.1" },
    ),
  );
  assert.equal(tagged.tag, "v0.17.1");
  assert.equal(tagged.imageTag, "0.17.1");

  // Nothing recorded is "" rather than a guess, on both routes into it.
  assert.equal(recordedImageTag(recordedInstall({ refKind: "tag", commit: "f".repeat(40) })), "");
  assert.equal(recordedCheckout(recordedInstall({ tag: "v0.17.1" }, { tag: "v0.17.1" })).imageTag, "");
  assert.equal(recordedImageTag(null), "");
  assert.equal(recordedCheckout(null).imageTag, "");
});

test("a repair restores the answers the operator is never asked for again", () => {
  // seedBootstrap refuses an INCOMPLETE owner set, correctly: a partial seed
  // brings the cluster up green and leaves the operator at a login page for an
  // account that was never created (znasllc-io#3888). A repair collects none of
  // this, so it all has to come off the receipt.
  const opts = repairOptions(
    repairArgs(),
    recordedInstall({ tag: "v0.19.0", refKind: "tag", commit: "f".repeat(40) }, { tag: "v0.19.0" }),
  );
  assert.deepEqual(paramsFor(opts, "seedBootstrap", "install.seedBootstrap"), {
    domain: "lab.example.com",
    "owner-email": "dev@example.com",
    "owner-first-name": "Dev",
    "owner-last-name": "Eloper",
    "registration-mode": "invite_only",
    provider: "openai",
    "provider-key-file": "/home/dev/.memql/key",
  });
  // The VENDOR travels with the key path. Re-asserting the default over a
  // recorded OpenAI key verifies it against Anthropic and reports exit 3 --
  // REFUSED, "the key is bad" -- about a key that is fine (memql#3473).
  assert.deepEqual(paramsFor(opts, "providerKey", "install.verifyProviderKey"), {
    "key-file": "/home/dev/.memql/key",
    provider: "openai",
  });
  // And the domain reaches all four of its consumers, not just the two that
  // used to get it (memql#3593).
  // Anchored to the comma-joined list's separators: unanchored, `api.lab.example.com`
  // would also match `api.lab.example.com.evil.test` (CodeQL missing-regexp-anchor).
  assert.match(
    paramsFor(opts, "hostsBlock", "install.hostsEntries").hostnames!,
    /(^|,)api\.lab\.example\.com(,|$)/,
  );
  assert.equal(paramsFor(opts, "clusterUp", "k3d.up").domain, "lab.example.com");
});

test("a repair refuses rather than guesses, and every refusal names the remedy", () => {
  // Each of these would otherwise start a run that cannot finish, and fail deep
  // inside it with an exit code whose guidance says "a fault in MemQL rather
  // than in your machine" -- which would be a lie in all three cases.
  assert.throws(() => repairOptions(repairArgs(), null), /no receipt .*Install rather than repair/s);

  const noCheckout = recordedInstall({});
  assert.throws(() => repairOptions(repairArgs(), noCheckout), /records no checkout/);

  const pastedKey = recordedInstall(
    { tag: "v0.19.0", refKind: "tag", commit: "a".repeat(40) },
    { tag: "v0.19.0" },
    { "key-file": REDACTED },
  );
  assert.throws(() => repairOptions(repairArgs(), pastedKey), /no usable AI-provider key path/);

  // ...but a path the receipt never carried is not a value the receipt is
  // overriding, so the flag still supplies it.
  const supplied = repairOptions(repairArgs(["--provider-key-file=/tmp/key"]), pastedKey);
  assert.equal(paramsFor(supplied, "providerKey", "install.verifyProviderKey")["key-file"], "/tmp/key");
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
