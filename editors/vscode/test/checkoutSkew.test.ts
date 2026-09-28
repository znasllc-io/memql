// The extension's own commit against the checkout it drives (memql#5076).
//
// THE DEFECT, TWICE. memql#5056 and memql#5064 were both a divergence between
// the extension and the checkout it builds from, and both presented as
// something else. The observed shape:
//
//     buildinfo.commit=07f9747c1   <- the checkout had the fix
//     staged scripts               <- the extension did not
//
// Neither side could tell the operator, so it read as the product being
// broken. Both investigations took hours.
//
// WHAT THESE TESTS PIN is that the extension can now SAY it, and -- the harder
// half -- that it says it in the right register. A checkout weeks ahead of the
// extension is the NORMAL state of a from-source install, so a warning on the
// ordinary case would be learned as noise and the one that matters would go
// unread. So: a quiet fact where a person browses, an attention line where
// they are about to act, and nothing at all where there is no checkout.

import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { test } from "node:test";

import { readBuildStamp } from "../src/version/buildStamp.js";
import { checkoutSkew, checkoutSkewFactValue, shortCommit } from "../src/version/checkoutSkew.js";
import { rebuildCheck, type RebuildPreflightInputs } from "../src/state/rebuildPreflight.js";
import { localOverviewBar } from "../src/deploy/instanceActions.js";
import { localOverviewScreen } from "../src/webview/deploymentScreens.js";
import type { Instance } from "../src/state/deployments.js";
import { STAGED_ROOT_DIR } from "../src/install/root.js";

const EXT = "07f9747c1aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
const CHECKOUT = "68000158e0000000000000000000000000000000";

// ---------------------------------------------------------------------------
// the comparison
// ---------------------------------------------------------------------------

test("two different commits are reported as diverged, naming both", () => {
  const skew = checkoutSkew({ extensionCommit: EXT, checkoutCommit: CHECKOUT });
  assert.equal(skew.state, "diverged");
  assert.match(skew.sentence, new RegExp(shortCommit(EXT)));
  assert.match(skew.sentence, new RegExp(shortCommit(CHECKOUT)));
  // The sentence that would have ended both investigations: WHICH half comes
  // from where.
  assert.match(skew.sentence, /runs the checkout's scripts, while its steps and this page come from this extension/);
});

test("the same commit is reported as the same, and claims no direction either way", () => {
  const skew = checkoutSkew({ extensionCommit: EXT, checkoutCommit: EXT });
  assert.equal(skew.state, "same");
  assert.doesNotMatch(skew.sentence, /ahead|behind/i);
});

// A DIRTY BUILD IS NOT ITS COMMIT. This is the case a developer is most likely
// to be debugging, and "the same commit as the checkout" would be false in it.
test("a build from uncommitted edits is diverged even when the commits match", () => {
  const skew = checkoutSkew({ extensionCommit: EXT, extensionDirty: true, checkoutCommit: EXT });
  assert.equal(skew.state, "diverged");
  assert.match(skew.sentence, /uncommitted edits/);
});

test("an unstamped extension says it cannot be compared, rather than claiming a match", () => {
  const skew = checkoutSkew({ extensionCommit: undefined, checkoutCommit: CHECKOUT });
  assert.equal(skew.state, "unknown");
  assert.match(skew.sentence, /run from source rather than packaged/);
});

// noCheckout is SEPARATE from unknown because they send a reader to different
// places, and because collapsing them would put "cannot tell" on every remote
// instance -- none of which has a checkout by construction.
test("no checkout is its own answer, and contributes no fact row", () => {
  const skew = checkoutSkew({ extensionCommit: EXT, checkoutCommit: "" });
  assert.equal(skew.state, "noCheckout");
  assert.equal(checkoutSkewFactValue({ extensionCommit: EXT, checkoutCommit: "" }), "");
});

// ---------------------------------------------------------------------------
// the stamp
// ---------------------------------------------------------------------------

function stagedExtension(contents?: string): string {
  const dir = mkdtempSync(path.join(os.tmpdir(), "memql-stamp-"));
  mkdirSync(path.join(dir, STAGED_ROOT_DIR), { recursive: true });
  if (contents !== undefined) {
    writeFileSync(path.join(dir, STAGED_ROOT_DIR, "buildinfo.json"), contents);
  }
  return dir;
}

test("a packaged extension reads its own commit back", () => {
  const dir = stagedExtension(
    JSON.stringify({ commit: EXT, dirty: false, version: "0.3.1", builtAt: "2026-09-06T00:00:00Z" }),
  );
  const stamp = readBuildStamp(dir)!;
  assert.equal(stamp.commit, EXT);
  assert.equal(stamp.dirty, false);
  assert.equal(stamp.version, "0.3.1");
});

test("an unstamped, corrupt, empty-commit or path-less read all answer undefined", () => {
  assert.equal(readBuildStamp(stagedExtension()), undefined, "no file");
  assert.equal(readBuildStamp(stagedExtension("{not json")), undefined, "corrupt");
  assert.equal(readBuildStamp(stagedExtension(JSON.stringify({ commit: "  " }))), undefined, "blank commit");
  // A BLANK PATH MUST NOT THROW. This runs during activation, and
  // `path.join(undefined, ...)` raises ERR_INVALID_ARG_TYPE -- which would take
  // the whole activation down over a diagnostic field.
  assert.equal(readBuildStamp(undefined), undefined, "no path");
  assert.equal(readBuildStamp(""), undefined, "empty path");
});

// ---------------------------------------------------------------------------
// where it is said: the cluster page
// ---------------------------------------------------------------------------

function overview(instance: Partial<Instance>, detailsOpen = true): string {
  const full = {
    name: "local",
    kind: "local",
    presence: "installed-healthy",
    connected: true,
    registered: true,
    ...instance,
  } as Instance;
  const parts = localOverviewScreen({
    instance: full,
    bar: localOverviewBar({ instance: full, connection: "connected", upgrade: { kind: "none", reason: "" } }),
    runs: [],
    nowMs: 0,
    upgrade: { kind: "none", reason: "not what this file is about" },
    releases: undefined,
    detailsOpen,
    home: "/home/me",
  });
  return parts.body;
}

/** The page above its Details disclosure: the facts a person reads first. */
function primary(html: string): string {
  const at = html.indexOf("mq-disclosure");
  return at < 0 ? html : html.slice(0, at);
}

// A FACT, NOT AN ALERT, and PROMINENT only when it is news. The primary tier
// carries it when the two have diverged, because that is the thing that
// explains a whole class of "the product is broken" reports.
test("a diverged extension is stated on the page, not buried in Details", () => {
  const html = overview({ checkout: "/src/memql", checkoutCommit: CHECKOUT, extensionCommit: EXT });
  assert.match(primary(html), /From a different commit than your checkout/);
  // The two commits themselves are for Details.
  assert.match(html, new RegExp(shortCommit(EXT)));
});

// ...and QUIET when they agree. Two matching commits are a stamp, kept under
// Details for a support case.
test("an agreeing extension is a Details stamp, not a headline", () => {
  const html = overview({ checkout: "/src/memql", checkoutCommit: EXT, extensionCommit: EXT });
  assert.doesNotMatch(primary(html), /Extension/, "an agreement was promoted to the headline");
  assert.match(html, /Same commit as your checkout/, "the agreement is not recorded anywhere");
});

// A remote instance has no checkout, and a row saying so on every one of them
// is the noise that makes the row that matters unreadable.
test("an instance with no checkout says nothing about a build it cannot compare", () => {
  const html = overview({ extensionCommit: EXT });
  assert.doesNotMatch(html, new RegExp(shortCommit(EXT)));
});

// ---------------------------------------------------------------------------
// where it matters: before a build from the checkout
// ---------------------------------------------------------------------------

const preflightBase: RebuildPreflightInputs = {
  dockerReachable: true,
  checkoutDir: "/src/memql",
  checkoutIsMemql: true,
  state: { ref: { kind: "branch", name: "main" }, commit: CHECKOUT, dirtyCount: 0, deployDirty: false },
  imageSource: "checkout",
  releasedTag: "v0.17.0",
};

const SKEW_LINE = "This extension is from a different commit than your checkout.";

test("the check raises the skew where the operator is about to act on it", () => {
  const notices = rebuildCheck({ ...preflightBase, extensionCommit: EXT }).notices;
  assert.ok(notices.some((n) => n.line === SKEW_LINE));
});

test("...and does not raise it when there is none", () => {
  assert.ok(!rebuildCheck({ ...preflightBase, extensionCommit: CHECKOUT }).notices.some((n) => n.line === SKEW_LINE));
});

// AN UNSTAMPED EXTENSION IS NOT RAISED. It is what every developer running the
// extension from source sees, on every rebuild -- a notice that is always there
// is the noise that hides the one that matters, and there is nothing to do
// about it. The comparison is still in Details for a support case.
test("an extension that cannot say what it is built from is not raised", () => {
  assert.ok(!rebuildCheck(preflightBase).notices.some((n) => n.line === SKEW_LINE));
});

// THE COMMIT THE BUILD WILL ACTUALLY USE. The check compares against git's
// CURRENT HEAD in the checkout, not the commit the receipt recorded at install
// time -- which is exactly as stale as the last install.
test("the check compares against the checkout's live HEAD, not the receipt", () => {
  const moved = "abcdef1234567890abcdef1234567890abcdef12";
  const agreeing = rebuildCheck({ ...preflightBase, state: { ...preflightBase.state!, commit: moved }, extensionCommit: moved });
  assert.ok(!agreeing.notices.some((n) => n.line === SKEW_LINE), "compared against the receipt instead of HEAD");
  const facts = rebuildCheck({ ...preflightBase, state: { ...preflightBase.state!, commit: moved }, extensionCommit: EXT }).facts;
  assert.match(facts.find((f) => f.label === "Commit")?.value ?? "", new RegExp(shortCommit(moved)));
});
