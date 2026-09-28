// What a rebuild from the checkout will build, what needs attention first, and
// the screen that says so (memql#4246).
//
// THE SHAPE CHANGED. The old "Before it runs" list was seven rows, most of them
// "OK" facts that asked nothing of anybody. The check is now FACTS (the folder,
// the commit) and NOTICES only for what is not fine, and a notice that makes the
// rebuild pointless is BLOCKING: the screen then offers no Rebuild at all --
// absent, never disabled -- but the fix instead.
//
// The notice that carries the epic is still the lane crossing: a rebuild moves
// a cluster off released images, and nothing else on the machine says so.

import test from "node:test";
import assert from "node:assert/strict";

import { checkoutCommitFact, rebuildCheck, type RebuildPreflightInputs } from "../src/state/rebuildPreflight.js";
import { rebuildScreen } from "../src/webview/deploymentScreens.js";
import type { Instance } from "../src/state/deployments.js";

const base: RebuildPreflightInputs = {
  dockerReachable: true,
  checkoutDir: "/home/me/.memql/src",
  checkoutIsMemql: true,
  state: {
    commit: "abc1234def",
    ref: { kind: "tag" as const, name: "v0.17.0" },
    dirtyCount: 4,
    deployDirty: false,
  },
  imageSource: "released",
  releasedTag: "v0.17.0",
};

const LOCAL: Instance = { name: "local", kind: "local", presence: "installed-healthy", connected: true, checkout: "/home/me/.memql/src" };

test("the facts say what will be built: the folder, and the commit in it", () => {
  assert.deepEqual(rebuildCheck(base).facts, [
    { label: "Source", value: "/home/me/.memql/src", mono: true },
    { label: "Commit", value: "v0.17.0 @ abc1234 · 4 uncommitted", mono: true },
  ]);
});

test("the commit fact names the ref, the short commit and the count, and a clean tree says no count", () => {
  assert.equal(checkoutCommitFact({ ...base.state!, ref: { kind: "branch", name: "main" }, dirtyCount: 0 }), "main @ abc1234");
  assert.equal(checkoutCommitFact({ ...base.state!, ref: { kind: "detached", name: "" } }), "abc1234 · 4 uncommitted");
});

test("a clean, reachable first rebuild has one notice: the cluster switches to your own build", () => {
  const check = rebuildCheck(base);
  assert.equal(check.blocked, false);
  assert.deepEqual(check.notices.map((n) => n.line), ["The cluster switches to your own build."]);
  assert.equal(check.notices[0]!.tone, "info");
});

test("staying on your own build says nothing about the lane", () => {
  assert.deepEqual(rebuildCheck({ ...base, imageSource: "checkout" }).notices, []);
});

test("Docker not running blocks the rebuild, and offers Check again", () => {
  const check = rebuildCheck({ ...base, dockerReachable: false });
  assert.equal(check.blocked, true);
  const docker = check.notices.find((n) => n.line === "Docker isn't running.")!;
  assert.equal(docker.tone, "error");
  assert.equal(docker.fix, "checkAgain");
});

test("a folder that is not a MemQL checkout blocks it, and offers Repair", () => {
  const check = rebuildCheck({ ...base, checkoutIsMemql: false });
  assert.equal(check.blocked, true);
  assert.equal(check.notices.find((n) => n.fix === "repair")?.line, "This folder isn't a MemQL checkout.");
});

test("edits under deploy/ are said, and do not block", () => {
  const check = rebuildCheck({ ...base, state: { ...base.state!, deployDirty: true } });
  assert.equal(check.blocked, false);
  assert.ok(check.notices.some((n) => n.line === "Changes under deploy/ aren't applied. Only code is rebuilt."));
});

test("git that could not read the folder says so rather than reporting a clean tree", () => {
  const check = rebuildCheck({ ...base, state: undefined });
  assert.ok(check.notices.some((n) => n.line === "Couldn't read the folder's git status."));
  assert.equal(check.facts.some((f) => f.label === "Commit"), false, "no commit invented");
});

test("an extension from a different commit than the checkout is said here, where the checkout's scripts run", () => {
  const skewed = rebuildCheck({ ...base, extensionCommit: "fff0000" });
  assert.ok(skewed.notices.some((n) => n.line === "This extension is from a different commit than your checkout."));
  assert.equal(rebuildCheck({ ...base, extensionCommit: "abc1234def" }).notices.some((n) => /different commit/.test(n.line)), false);
});

// -----------------------------------------------------------------------------
// the screen
// -----------------------------------------------------------------------------

function screen(check: ReturnType<typeof rebuildCheck> | undefined, nodes = ""): string {
  const parts = rebuildScreen({ instance: LOCAL, check, nodes, home: "/home/me" });
  // The kit escapes an apostrophe; the assertions read the words.
  return (parts.head + parts.body + parts.actions).replace(/&#39;/g, "'");
}

test("while the checks run, the facts are the shape of facts and the bar says Checking", () => {
  const html = screen(undefined);
  assert.match(html, /mq-skeleton/);
  assert.match(html, /Checking/);
  assert.doesNotMatch(html, /data-act="beginRebuild"/);
});

test("ready, the screen names the folder as ~/..., asks one thing, and offers Rebuild", () => {
  const html = screen(rebuildCheck(base), "bff");
  assert.match(html, /~\/\.memql\/src/);
  assert.doesNotMatch(html, /\/home\/me\//, "the home directory reached the page");
  assert.match(html, /data-field="nodes"/);
  assert.match(html, /data-act="beginRebuild"[^>]*>Rebuild</);
});

test("blocked, there is no Rebuild -- only the fix, and a way back", () => {
  const html = screen(rebuildCheck({ ...base, dockerReachable: false }));
  assert.doesNotMatch(html, /data-act="beginRebuild"/);
  assert.match(html, /data-act="checkAgain"/);
  assert.match(html, /Can't rebuild yet/);
  assert.doesNotMatch(html, /disabled/);
});
