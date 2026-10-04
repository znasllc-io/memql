// "Update from origin and rebuild" (memql#4578), end of the pure half.
//
// The run itself is scripts/install/update-stack.sh and is tested there against
// real repositories, because fast-forward-ness and conflict detection are git's
// semantics and a stub would only assert our own assumptions back at us. What
// is tested HERE is everything the editor decides before and after that run:
// which state it read, what it told the operator it was about to do, which two
// steps it planned with which flags, and what it said when it landed.
//
// The seam is `readUpdateState`'s injected runner, so the git invocations are
// asserted as the sequence of arguments they are, without a repository.

import test from "node:test";
import assert from "node:assert/strict";

import { readUpdateState, type UpdateState } from "../src/install/updateState.js";
import { updateCheck, type UpdatePreflightInputs } from "../src/state/updatePreflight.js";
import { updateRebuildPlan, type SessionOptions } from "../src/install/session.js";
import type { Step } from "../src/install/graph.js";
import type { Instance } from "../src/state/deployments.js";
import { updatedMessage } from "../src/state/imageLane.js";
import { pullRebuildScreen } from "../src/webview/deploymentScreens.js";

// -----------------------------------------------------------------------------
// reading the state
// -----------------------------------------------------------------------------

/** A git stand-in: exact argv join -> stdout, and anything unlisted rejects. */
function gitFake(answers: Record<string, string>): {
  run: (args: string[], timeoutMs?: number) => Promise<string>;
  asked: string[];
} {
  const asked: string[] = [];
  return {
    asked,
    run: async (args) => {
      const key = args.join(" ");
      asked.push(key);
      if (key in answers) return answers[key];
      throw new Error(`git ${key} exited 1`);
    },
  };
}

const BASE = {
  "rev-parse HEAD": "aaaa111\n",
  "status --porcelain": "",
  "symbolic-ref --short -q HEAD": "main\n",
  "rev-parse --absolute-git-dir": "/src/.git\n",
  "rev-parse --is-shallow-repository": "false\n",
};

test("a checkout at the tip reports both counts as zero, without touching the object store", async () => {
  const g = gitFake({
    ...BASE,
    "ls-remote --heads origin refs/heads/main": "aaaa111\trefs/heads/main\n",
  });
  const state = await readUpdateState("/src", {}, g.run, () => false);
  assert.equal(state?.branch, "main");
  assert.equal(state?.detached, false);
  assert.equal(state?.ahead, 0);
  assert.equal(state?.behind, 0);
  // Equal heads need no `cat-file` and no `rev-list`: the answer is already
  // known, and asking anyway is two spawns per checklist paint.
  assert.equal(
    g.asked.some((a) => a.startsWith("cat-file")),
    false,
  );
});

test("the counts are EXACT when the remote's commit is already local", async () => {
  const g = gitFake({
    ...BASE,
    "ls-remote --heads origin refs/heads/main": "bbbb222\trefs/heads/main\n",
    "cat-file -e bbbb222^{commit}": "",
    "rev-list --count bbbb222..HEAD": "0\n",
    "rev-list --count HEAD..bbbb222": "7\n",
  });
  const state = await readUpdateState("/src", {}, g.run, () => false);
  assert.equal(state?.ahead, 0);
  assert.equal(state?.behind, 7);
  assert.equal(state?.remoteHead, "bbbb222");
});

// THE ANSWER THAT MUST STAY ABSENT. Computing these against the stale
// remote-tracking ref would give a specific, confident number that was true at
// the last fetch -- the exact shape of wrong answer this area avoids.
test("the counts are ABSENT, not zero, when the remote's commit is not local yet", async () => {
  const g = gitFake({
    ...BASE,
    "ls-remote --heads origin refs/heads/main": "cccc333\trefs/heads/main\n",
    // cat-file is not answered, so it rejects: the commit is not here.
  });
  const state = await readUpdateState("/src", {}, g.run, () => false);
  assert.equal(state?.remoteHead, "cccc333");
  assert.equal(state?.ahead, undefined);
  assert.equal(state?.behind, undefined);
  assert.equal(state?.remoteError, "");
});

test("an unreachable remote is a fact on the state, not a thrown error", async () => {
  const g = gitFake(BASE); // ls-remote is unanswered, so it rejects
  const state = await readUpdateState("/src", {}, g.run, () => false);
  assert.notEqual(state, undefined);
  assert.notEqual(state?.remoteError, "");
  assert.equal(state?.ahead, undefined);
});

test("a detached checkout takes the branch the install recorded, and says it is detached", async () => {
  const g = gitFake({
    ...BASE,
    "symbolic-ref --short -q HEAD": "", // detached
    "ls-remote --heads origin refs/heads/main": "aaaa111\trefs/heads/main\n",
  });
  const state = await readUpdateState("/src", { fallbackBranch: "main" }, g.run, () => false);
  assert.equal(state?.detached, true);
  assert.equal(state?.branch, "main");
});

test("a detached checkout with nothing recorded has no branch, and asks the remote nothing", async () => {
  const g = gitFake({ ...BASE, "symbolic-ref --short -q HEAD": "" });
  const state = await readUpdateState("/src", {}, g.run, () => false);
  assert.equal(state?.branch, "");
  assert.equal(
    g.asked.some((a) => a.startsWith("ls-remote")),
    false,
  );
});

test("an unfinished operation is read off the git directory, as the script reads it", async () => {
  const g = gitFake({ ...BASE, "ls-remote --heads origin refs/heads/main": "aaaa111\trefs/heads/main\n" });
  const merging = await readUpdateState("/src", {}, g.run, (p) => p === "/src/.git/MERGE_HEAD");
  assert.equal(merging?.inProgress, "a merge");
  // A rebase leaves only a directory, which is why the check is a filesystem
  // one for all four rather than `rev-parse` for the three that are refs.
  const rebasing = await readUpdateState("/src", {}, g.run, (p) => p === "/src/.git/rebase-apply");
  assert.equal(rebasing?.inProgress, "a rebase");
});

test("a directory git cannot read at all is undefined, not a half-built state", async () => {
  const g = gitFake({});
  assert.equal(await readUpdateState("/src", {}, g.run, () => false), undefined);
});

// -----------------------------------------------------------------------------
// the checklist
// -----------------------------------------------------------------------------

function state(over: Partial<UpdateState> = {}): UpdateState {
  return {
    head: "aaaa111",
    branch: "main",
    detached: false,
    dirtyCount: 0,
    inProgress: "",
    shallow: false,
    remote: "origin",
    remoteHead: "bbbb222",
    remoteError: "",
    ahead: 0,
    behind: 3,
    ...over,
  };
}

function inputs(over: Partial<UpdatePreflightInputs> = {}): UpdatePreflightInputs {
  return {
    dockerReachable: true,
    checkoutDir: "/home/me/.memql/src",
    checkoutIsMemql: true,
    imageSource: "checkout",
    releasedTag: "v0.19.1",
    update: state(),
    ...over,
  };
}

function fact(check: ReturnType<typeof updateCheck>, label: string): string {
  return check.facts.find((f) => f.label === label)?.value ?? "";
}

function lines(check: ReturnType<typeof updateCheck>): string[] {
  return check.notices.map((n) => n.line);
}

test("the facts say what will be pulled: the branch, how far, and what is yours", () => {
  const check = updateCheck(inputs({ update: state({ dirtyCount: 2, ahead: 1 }) }));
  assert.equal(fact(check, "Branch"), "main from origin");
  assert.equal(fact(check, "To pull"), "3 new commits");
  assert.equal(fact(check, "Your commits"), "1 not on origin");
  assert.equal(fact(check, "Uncommitted"), "2 files");
  // A clean checkout with nothing of its own says neither.
  const clean = updateCheck(inputs());
  assert.equal(fact(clean, "Uncommitted"), "");
  assert.equal(fact(clean, "Your commits"), "");
});

test("an unknown distance is said to be unknown rather than rendered as up to date", () => {
  const check = updateCheck(inputs({ update: state({ ahead: undefined, behind: undefined }) }));
  assert.equal(fact(check, "To pull"), "New commits available");
  assert.equal(fact(updateCheck(inputs({ update: state({ behind: 0 }) })), "To pull"), "Up to date");
});

test("an unreachable remote is a notice, and the run still gets to try", () => {
  const check = updateCheck(inputs({ update: state({ remoteError: "could not resolve host", ahead: undefined, behind: undefined }) }));
  assert.ok(lines(check).includes("Couldn't reach origin."));
  assert.equal(fact(check, "To pull"), "");
  // Not blocking: the run reaches the network properly and reports what it finds.
  assert.equal(check.blocked, false);
});

test("a shallow checkout is warned about once, because the deepening is the slow part", () => {
  assert.ok(lines(updateCheck(inputs({ update: state({ shallow: true }) }))).some((l) => /full history/.test(l)));
  assert.ok(!lines(updateCheck(inputs())).some((l) => /history/.test(l)));
});

test("a merge in progress or no branch blocks the pull, and says why in one line", () => {
  const merging = updateCheck(inputs({ update: state({ inProgress: "a merge" }) }));
  assert.equal(merging.blocked, true);
  assert.ok(lines(merging).includes("A merge is in progress in this folder."));
  const detached = updateCheck(inputs({ update: state({ branch: "" }) }));
  assert.equal(detached.blocked, true);
  assert.ok(lines(detached).includes("This folder isn't on a branch."));
});

test("the rebuild half's notices ride along -- one account of one run", () => {
  const check = updateCheck(inputs({ dockerReachable: false }));
  assert.ok(lines(check).includes("Docker isn't running."));
  assert.equal(check.blocked, true);
});

// TWO BLOCKING CASES, AND ONLY TWO. Both are refusals the script makes before
// it fetches; everything else is a maybe only the run can settle, and a Start
// disabled on a maybe withholds a button that would very often have worked.
test("only an unfinished operation and a missing branch block Start", () => {
  const blocked = (update: UpdateState | undefined): boolean => updateCheck(inputs({ update })).blocked;
  assert.equal(blocked(state()), false);
  assert.equal(blocked(state({ dirtyCount: 9 })), false);
  assert.equal(blocked(state({ ahead: 4 })), false);
  assert.equal(blocked(state({ inProgress: "a merge" })), true);
  assert.equal(blocked(state({ branch: "" })), true);
  // Nothing read means nothing to block on -- the checklist says so in a line.
  assert.equal(blocked(undefined), false);
});

test("a checkout git could not read says so instead of describing an update", () => {
  const check = updateCheck(inputs({ update: undefined }));
  assert.ok(lines(check).includes("Couldn't read the folder's git status."));
  assert.equal(fact(check, "Branch"), "");
});

// -----------------------------------------------------------------------------
// the plan
// -----------------------------------------------------------------------------

function options(over: Partial<SessionOptions> = {}): SessionOptions {
  return {
    root: "/repo",
    receiptFile: "/tmp/receipt.json",
    skip: new Set<string>(),
    stepParams: {},
    ...over,
  };
}

function step(id: string, script: string): Step {
  return {
    id,
    script,
    label: "Working",
    description: "",
    elevation: "none",
    retained: false,
    retainedReason: "",
    shared: false,
    sharedReason: "",
    verify: { kind: "scriptOk" },
  };
}

test("the update plan names the checkout, the branch and the strategy", () => {
  const plan = updateRebuildPlan(
    options({ stackDir: "/home/me/.memql/src", branch: "main", strategy: "merge" }),
  );
  assert.deepEqual(plan(step("updateCheckout", "install.updateStack")), {
    action: "run",
    params: { dest: "/home/me/.memql/src", branch: "main", strategy: "merge" },
  });
});

// EMPTY IS ABSENT, not "". `install.updateStack` defaults the remote to origin
// and takes the branch the checkout is on; `--branch=` is a branch name of ""
// and exits 2, which is a refusal about a flag the operator never typed.
test("the update plan omits what it does not know rather than passing blanks", () => {
  const plan = updateRebuildPlan(options({ stackDir: "/src" }));
  assert.deepEqual(plan(step("updateCheckout", "install.updateStack")), {
    action: "run",
    params: { dest: "/src" },
  });
});

test("the plan's second step is the rebuild's, and carries no update flags", () => {
  const plan = updateRebuildPlan(
    options({ stackDir: "/src", appName: "memql-local", nodes: " bff , agent ", branch: "main" }),
  );
  assert.deepEqual(plan(step("rebuildFromCheckout", "k3d.dev")), {
    action: "run",
    // `--image-source=checkout` is absent HERE and pinned by the graph, which is
    // what keeps "update, but keep running released images" unreachable.
    params: { "repo-root": "/src", "app-name": "memql-local", node: "bff,agent" },
  });
});

// The same discipline `rebuildPlan` has: a caller who hands this the install
// document gets nothing out of it, rather than an `install.cloneStack`
// invocation carrying a `--strategy`.
test("the update plan skips any step that is not one of its two", () => {
  const plan = updateRebuildPlan(options({ stackDir: "/src" }));
  const planned = plan(step("stackCheckout", "install.cloneStack"));
  assert.equal(planned.action, "skip");
  const skipped = updateRebuildPlan(options({ stackDir: "/src", skip: new Set(["updateCheckout"]) }));
  assert.equal(skipped(step("updateCheckout", "install.updateStack")).action, "skip");
});

// -----------------------------------------------------------------------------
// what it says afterwards
// -----------------------------------------------------------------------------

test("the finished message is read off both envelopes, never off what was asked for", () => {
  const rebuild = { nodes: "bff agent", commit: "abc1234def567", dirtyCount: 2 };
  assert.equal(
    updatedMessage("local", { outcome: "fastForward", behind: 12 }, rebuild),
    "Pulled 12 new commits. local now runs your build (abc1234, 2 uncommitted files).",
  );
  assert.match(updatedMessage("local", { outcome: "upToDate", behind: 0 }, rebuild), /^Already up to date\./);
  assert.match(updatedMessage("local", { outcome: "merged" }, rebuild), /^Merged the latest with your commits\./);
  // No node list and no double hyphen: a full rebuild names nine services
  // nobody asked about.
  assert.doesNotMatch(updatedMessage("local", { outcome: "upToDate" }, rebuild), /bff|--/);
});

test("an outcome the envelope did not carry is left unnamed rather than guessed", () => {
  const said = updatedMessage("local", {}, { nodes: "bff" });
  assert.match(said, /^Pulled the latest\./);
  assert.doesNotMatch(said, /up to date/i);
  assert.match(updatedMessage("local", undefined, undefined), /^Pulled the latest\. local now runs your build\.$/);
});

// -----------------------------------------------------------------------------
// the screen
// -----------------------------------------------------------------------------

const LOCAL: Instance = { name: "local", kind: "local", presence: "installed-healthy", connected: true, checkout: "/home/me/src", checkoutBranch: "main" };

function screen(over: Partial<Parameters<typeof pullRebuildScreen>[0]> = {}): string {
  const parts = pullRebuildScreen({
    instance: LOCAL,
    check: updateCheck(inputs({ checkoutDir: "/home/me/src" })),
    nodes: "",
    home: "/home/me",
    merge: false,
    offerMerge: true,
    ...over,
  });
  // The kit escapes an apostrophe; the assertions read the words.
  return (parts.head + parts.body + parts.actions).replace(/&#39;/g, "'");
}

test("merging your own commits is a switch, off by default -- the answer that changes less", () => {
  const off = screen();
  assert.match(off, /role="switch"[^>]*data-field="merge"/);
  assert.doesNotMatch(off, /role="switch"[^>]* checked/);
  assert.match(screen({ merge: true }), /role="switch" id="dp-merge" checked/);
  // Offered only when the checkout may have commits of its own.
  assert.doesNotMatch(screen({ offerMerge: false }), /data-field="merge"/);
});

test("the pull is offered when the check is clean, and absent -- not disabled -- when it is blocked or still checking", () => {
  assert.match(screen(), /data-act="beginPullRebuild"/);
  const blocked = screen({ check: updateCheck(inputs({ update: state({ inProgress: "a merge" }) })) });
  assert.doesNotMatch(blocked, /data-act="beginPullRebuild"/);
  assert.match(blocked, /Can't pull yet/);
  const checking = screen({ check: undefined });
  assert.doesNotMatch(checking, /data-act="beginPullRebuild"/);
  assert.match(checking, /mq-skeleton/);
  assert.doesNotMatch(checking, /Checking what an update would do/);
  assert.doesNotMatch(`${blocked}${checking}`, /disabled/);
});

test("the screen names the folder under ~ and never the word origin in its title", () => {
  const html = screen();
  assert.match(html, /~\/src/);
  assert.match(html, />Pull and rebuild</);
  assert.doesNotMatch(html, /Update from origin/);
});
