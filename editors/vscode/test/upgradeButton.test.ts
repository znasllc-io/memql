// The one button that moves a cluster to the newest release (memql#3997).
//
// Three things are being held here and they fail differently:
//
//   WHEN IT IS OFFERED. Only in the `behind` state. Every other state has a
//   specific reason not to move a cluster, and `ahead` has the worst one -- a
//   locally built cluster offered a "move" to an OLDER release.
//
//   WHAT IT REFUSES. A barrier turns the confirmation into a refusal and the
//   move does not run. Not a warning: the crossing can leave a cluster with an
//   empty graph and no error anywhere, and a warning is a thing operators click
//   past.
//
//   WHAT IT SAYS. The confirmation names the instance, `current -> target`, and
//   WHICH MACHINERY RUNS. The third is the one that gets dropped and the one
//   that decides whether an operator lets a fifteen-step run proceed.

import test from "node:test";
import assert from "node:assert/strict";

import { roleVisibility } from "../src/deploy/actions.js";
import { moveFlowFor } from "../src/deploy/instanceActions.js";
import { upgradeVerdict } from "../src/deploy/upgrade.js";
import type { Instance } from "../src/state/deployments.js";
import { describeVersion } from "../src/version/describe.js";
import type { ReleaseListing } from "../src/version/releaseCache.js";
import { renderInstanceOverview, renderRemoteInstance } from "../src/webview/deploymentScreens.js";

const listing = (tags: string[], error?: string): ReleaseListing => ({
  tags,
  error,
  fetchedAt: 1000,
});

// v0.19.0 is newest; the v0.18.0 barrier sits between it and anything at or
// before v0.18.0.
const NEWEST = listing(["v0.19.0", "v0.18.0", "v0.17.1", "v0.17.0"]);
// A listing whose newest is BEFORE the barrier, so a move inside it is a retag.
const PRE_BARRIER = listing(["v0.18.0", "v0.17.1", "v0.17.0"]);

const local = (version?: string): Instance => ({
  name: "local",
  kind: "local",
  presence: "installed-healthy",
  connected: false,
  ...(version === undefined ? {} : { version }),
});

const remote = (version?: string): Instance => ({
  name: "staging",
  kind: "remote",
  presence: "installed-healthy",
  connected: true,
  ...(version === undefined ? {} : { version }),
});

const verdictFor = (instance: Instance, releases: ReleaseListing | undefined, role?: string) =>
  upgradeVerdict({
    instance,
    version: describeVersion({ recorded: instance.version, listing: releases }),
    ...(role === undefined ? {} : { visibility: roleVisibility(role as never) }),
  });

// --- which machinery ---------------------------------------------------------

test("the flow comes from the same mapping Create deployment uses", () => {
  // A second copy of this mapping is how an upgrade would one day route a local
  // instance into the FULL install graph -- the one mistake InstanceActionFlow
  // names outright.
  assert.equal(moveFlowFor(local()), "upgradeToTag");
  assert.equal(moveFlowFor(remote()), "deployControl");
});

// --- when it is offered ------------------------------------------------------

test("a cluster behind the newest release is offered the move", () => {
  const verdict = verdictFor(local("v0.17.1"), PRE_BARRIER);
  assert.equal(verdict.kind, "offer");
  if (verdict.kind !== "offer") return;
  assert.equal(verdict.label, "Upgrade to v0.18.0");
  assert.equal(verdict.target.from, "v0.17.1");
  assert.equal(verdict.target.to, "v0.18.0");
  assert.equal(verdict.target.flow, "upgradeToTag");
  // THE PHRASE IS THE TARGET, never the word "yes": re-typing the version
  // forces the operator to look at what they are moving to.
  assert.equal(verdict.phrase, "v0.18.0");
});

test("the confirmation names the instance, the move, and the machinery", () => {
  const verdict = verdictFor(local("v0.17.1"), PRE_BARRIER);
  if (verdict.kind !== "offer") return assert.fail("expected an offer");
  assert.match(verdict.confirmation, /Move local from v0\.17\.1 to v0\.18\.0\./);
  // The third part, which is the one that gets dropped: a local move re-runs
  // fifteen install steps and someone watching that without being told reads it
  // as a reinstall of their machine.
  assert.match(verdict.confirmation, /re-runs the install graph/);
  assert.match(verdict.confirmation, /verifies first and\s+skips|verifies first and skips/);
});

test("a move over a checkout-mode cluster says it returns to released images", () => {
  // The lane crossing, stated in the confirmation the operator reads (memql#4246).
  // Without it the only notice is the Deployments row afterwards, which stops
  // saying `checkout <commit>` and starts saying a version -- a developer whose
  // edits quietly stopped running has no reason to look there.
  const verdict = verdictFor(
    { ...local("v0.17.1"), imageSource: "checkout" },
    PRE_BARRIER,
  );
  if (verdict.kind !== "offer") return assert.fail("expected an offer");
  assert.match(verdict.confirmation, /returns local to released images/);
  assert.match(verdict.confirmation, /runs a checkout build today/);

  // Said only when it is true: a released-lane cluster crosses nothing.
  const plain = verdictFor(local("v0.17.1"), PRE_BARRIER);
  if (plain.kind !== "offer") return assert.fail("expected an offer");
  assert.doesNotMatch(plain.confirmation, /returns local to released images/);
});

test("the remote confirmation names the OTHER machinery", () => {
  const verdict = verdictFor(remote("v0.17.1"), PRE_BARRIER);
  if (verdict.kind !== "offer") return assert.fail("expected an offer");
  assert.match(verdict.confirmation, /Move staging from v0\.17\.1 to v0\.18\.0\./);
  assert.match(verdict.confirmation, /cuts a deployment record/);
  // The engine is the authority and the confirmation says so, rather than
  // implying this editor decided the operator may.
  assert.match(verdict.confirmation, /cluster decides whether you may/);
});

test("every state but behind is offered nothing", () => {
  // Each of these has a specific reason, and none of them is "we are not sure
  // so let us try".
  const cases: [string, Instance, ReleaseListing | undefined][] = [
    ["current", local("v0.19.0"), NEWEST],
    ["ahead (a locally built cluster)", local("v0.20.0"), NEWEST],
    ["unknown", local(), NEWEST],
    ["notComparable (the build stamp)", local("0.15.0-1737072000"), NEWEST],
    ["unfetched", local("v0.17.1"), undefined],
    ["unfetched, listing failed", local("v0.17.1"), listing([], "network unreachable")],
  ];
  for (const [name, instance, releases] of cases) {
    assert.equal(verdictFor(instance, releases).kind, "none", `${name} offered a move`);
  }
});

test("an absent instance is offered nothing", () => {
  const verdict = verdictFor(
    { ...local("v0.17.1"), presence: "absent" },
    PRE_BARRIER,
  );
  assert.equal(verdict.kind, "none");
  if (verdict.kind !== "none") return;
  assert.match(verdict.reason, /nothing is installed/);
});

test("an unreachable cluster is still offered the move", () => {
  // Consistent with the action table, which offers "Create deployment" for
  // installed-unreachable. Inventing a new restriction here would mean the
  // upgrade and the move-to-any-tag disagreed about the same cluster.
  const verdict = verdictFor(
    { ...local("v0.17.1"), presence: "installed-unreachable" },
    PRE_BARRIER,
  );
  assert.equal(verdict.kind, "offer");
});

// --- the role gate, which is a courtesy -------------------------------------

test("a role that cannot cut and deploy is not shown the remote button", () => {
  assert.equal(verdictFor(remote("v0.17.1"), PRE_BARRIER, "reader").kind, "none");
});

test("a developer IS shown it, because the engine allows a developer", () => {
  // memql#3997's text says "owner-gated". Taken literally that is a UI gate
  // STRICTER than the gate it mirrors -- DEPLOY_ACTIONS records cutVersion and
  // deploy as `developer` -- and epic memql#3989's "What must not regress" ends
  // with "The engine stays the authority on every gate".
  //
  // This test is the guard, and the failure it guards has a bug number: before
  // memql#3331 satisfiesTier approximated the developer tier as "admin or
  // above" and, in that comment's own words, "hid cut/deploy from a developer
  // who was entitled to them" (actions.ts:136-140). An owner-hardcoded button
  // would hide the same two calls from the same role again.
  assert.equal(verdictFor(remote("v0.17.1"), PRE_BARRIER, "developer").kind, "offer");
  assert.equal(verdictFor(remote("v0.17.1"), PRE_BARRIER, "owner").kind, "offer");
});

test("an unread role offers the button", () => {
  // A caller whose role could not be read may well be entitled, and the engine
  // refuses anything they are not.
  assert.equal(verdictFor(remote("v0.17.1"), PRE_BARRIER).kind, "offer");
});

test("the role gate does not apply to a local cluster", () => {
  // Nothing here asks a cluster for permission to change the machine it is
  // running on.
  assert.equal(verdictFor(local("v0.17.1"), PRE_BARRIER, "reader").kind, "offer");
});

// --- the refusal -------------------------------------------------------------

test("a move across a barrier is refused, with the runbook", () => {
  const verdict = verdictFor(local("v0.18.0"), NEWEST);
  assert.equal(verdict.kind, "refused");
  if (verdict.kind !== "refused") return;
  assert.equal(verdict.barriers.length, 1);
  assert.equal(verdict.docHref, "docs/public/operate/upgrade-barriers.md");
  assert.match(verdict.message, /is not a retag, so it was not run/);
  // The barrier's own summary, quoted rather than paraphrased: it is the
  // sentence a reviewer approved when the barrier was added.
  assert.match(verdict.message, /CloudNativePG/);
  // A refusal that says only "no" is a dead end.
  assert.match(verdict.message, /docs\/public\/operate\/upgrade-barriers\.md/);
});

test("a move that stops at the barrier is not refused", () => {
  // v0.17.1 -> v0.18.0 arrives AT the barrier's afterVersion, which is an
  // ordinary retag. Getting this off by one turns every v0.17.x upgrade into a
  // refusal.
  assert.equal(verdictFor(local("v0.17.1"), PRE_BARRIER).kind, "offer");
});

test("the remote path is refused by the same barrier", () => {
  // The barrier is about what the release changed, not about which machinery
  // moves the cluster.
  assert.equal(verdictFor(remote("v0.18.0"), NEWEST, "owner").kind, "refused");
});

// --- the button --------------------------------------------------------------

const OVERVIEW = { runs: [], actions: [], nowMs: 0, error: "" } as const;

test("the page draws the button when the move is offered", () => {
  const instance = local("v0.17.1");
  const html = renderInstanceOverview({
    ...OVERVIEW,
    instance,
    releases: PRE_BARRIER,
    upgrade: verdictFor(instance, PRE_BARRIER),
  });
  assert.match(html, /data-act="upgrade"/);
  assert.match(html, /Upgrade to v0\.18\.0/);
});

test("the page draws the button for a REFUSED move too", () => {
  // Hiding it would leave an operator looking at a row that says
  // `v0.19.0 available` beside a page that offers nothing, with no way to find
  // out why. Pressing it produces the refusal and the runbook.
  const instance = local("v0.18.0");
  const html = renderInstanceOverview({
    ...OVERVIEW,
    instance,
    releases: NEWEST,
    upgrade: verdictFor(instance, NEWEST),
  });
  assert.match(html, /data-act="upgrade"/);
  assert.match(html, /not a retag/);
});

test("the page draws no button when there is nothing to offer", () => {
  const instance = local("v0.19.0");
  const html = renderInstanceOverview({
    ...OVERVIEW,
    instance,
    releases: NEWEST,
    upgrade: verdictFor(instance, NEWEST),
  });
  assert.doesNotMatch(html, /data-act="upgrade"/);
});

test("the remote page draws it on the same terms", () => {
  const instance = remote("v0.17.1");
  const html = renderRemoteInstance({
    instance,
    runs: [],
    pipeline: { kind: "present", title: "Deploy", detail: "", actions: [] },
    nowMs: 0,
    outcome: "",
    error: "",
    releases: PRE_BARRIER,
    upgrade: verdictFor(instance, PRE_BARRIER, "owner"),
  });
  assert.match(html, /data-act="upgrade"/);
  assert.match(html, /Upgrade to v0\.18\.0/);
});

test("everything the button draws is escaped", () => {
  const instance: Instance = {
    name: "<img src=x>",
    kind: "local",
    presence: "installed-healthy",
    connected: false,
    version: "v0.17.1",
  };
  const html = renderInstanceOverview({
    ...OVERVIEW,
    instance,
    releases: PRE_BARRIER,
    upgrade: verdictFor(instance, PRE_BARRIER),
  });
  assert.doesNotMatch(html, /<img src=x>/);
  assert.match(html, /&lt;img src=x&gt;/);
});
