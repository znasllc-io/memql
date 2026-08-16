// Availability on the Deployments surfaces (memql#3996).
//
// The Deployments tree and the instance page are where an operator looks at a
// cluster they are about to move, so this is where "a newer release exists"
// has to be visible. Both read the SAME describeVersion decision the Clusters
// surfaces read (memql#3995) -- two surfaces deciding these states for
// themselves is how one of them ends up rendering an unfetched listing as
// current, which is the failure the epic exists to prevent.
//
// ONE PLACE THESE ROWS DELIBERATELY DIFFER FROM THE CLUSTERS TREE, and it is
// the first thing to check when this file is next edited: describeVersion
// returns an EMPTY short for an unrecorded version, because a Clusters row that
// appended "unknown" to every cluster on a fresh install would be noise. These
// rows already print a version for every instance, so falling silent for one of
// them would read as a fact about that instance. `displayVersion`'s word
// "unknown" stays.

import test from "node:test";
import assert from "node:assert/strict";

import { instanceRowStatus } from "../src/state/deploymentsCatalog.js";
import type { Instance } from "../src/state/deployments.js";
import type { ReleaseListing } from "../src/version/releaseCache.js";
import {
  renderChooseTag,
  renderInstanceOverview,
  renderRemoteInstance,
} from "../src/webview/deploymentScreens.js";

const listing = (tags: string[], error?: string): ReleaseListing => ({
  tags,
  error,
  fetchedAt: 1000,
});

const KNOWN = listing(["v0.19.0", "v0.18.0", "v0.17.1"]);

const healthy = (version?: string): Instance => ({
  name: "local",
  kind: "local",
  presence: "installed-healthy",
  connected: false,
  ...(version === undefined ? {} : { version }),
});

// --- the tree row -----------------------------------------------------------

test("a behind instance row carries the availability clause", () => {
  const status = instanceRowStatus(healthy("v0.18.0"), KNOWN);
  assert.equal(status.description, "healthy - v0.18.0 - v0.19.0 available");
  assert.match(status.tooltip, /v0\.19\.0 has been released/);
  // The presence verdict stays FIRST: it is what the operator opened the row
  // for, and a tooltip that led with the version would bury it.
  assert.ok(status.tooltip.startsWith("local is healthy."));
});

test("a current instance row adds nothing", () => {
  const status = instanceRowStatus(healthy("v0.19.0"), KNOWN);
  assert.equal(status.description, "healthy - v0.19.0");
  assert.doesNotMatch(status.description, /available/);
});

test("an unfetched listing shows the version and names the failure", () => {
  const status = instanceRowStatus(healthy("v0.18.0"), listing([], "git ls-remote: network unreachable"));
  // The recorded version, and NOT a claim about whether it is current.
  assert.equal(status.description, "healthy - v0.18.0");
  assert.doesNotMatch(status.description, /available/);
  assert.doesNotMatch(status.tooltip, /newest release/);
  assert.match(status.tooltip, /network unreachable/);
});

test("no listing at all is unfetched, not current", () => {
  const status = instanceRowStatus(healthy("v0.18.0"), undefined);
  assert.equal(status.description, "healthy - v0.18.0");
  assert.match(status.tooltip, /not been fetched/);
  assert.doesNotMatch(status.tooltip, /newest release/);
});

test("an unrecorded version keeps the word unknown", () => {
  // The one place these rows diverge from the Clusters tree, on purpose. See
  // the header: a blank here reads as "this instance has no version".
  const status = instanceRowStatus(healthy(), KNOWN);
  assert.equal(status.description, "healthy - unknown");
});

test("an unreachable instance keeps its version and gains the clause", () => {
  const status = instanceRowStatus(
    { ...healthy("v0.18.0"), presence: "installed-unreachable" },
    KNOWN,
  );
  assert.equal(status.icon, "unreachable");
  assert.equal(status.description, "not answering - v0.18.0 - v0.19.0 available");
  assert.ok(status.tooltip.startsWith("A local cluster is installed but is not answering."));
});

test("an absent instance says nothing about versions", () => {
  // There is no version to compare and no cluster to move; a row that offered
  // an upgrade here would be offering to upgrade nothing.
  const status = instanceRowStatus({ ...healthy(), presence: "absent" }, KNOWN);
  assert.equal(status.description, "not installed");
  assert.doesNotMatch(status.tooltip, /available/);
});

test("a cluster ahead of every release is never offered an upgrade", () => {
  // A locally built cluster. Offering it a "move" to an older release is the
  // one thing the availability clause must not read as.
  const status = instanceRowStatus(healthy("v0.20.0"), KNOWN);
  assert.equal(status.description, "healthy - v0.20.0");
  assert.doesNotMatch(status.description, /available/);
});

test("a build stamp is shown and not compared", () => {
  // What every engine shipped before memql#3998 reports. Showing it beats
  // hiding it -- it is what the cluster actually said -- but it cannot be
  // placed on the release line, and must not be placed anyway.
  const status = instanceRowStatus(healthy("0.15.0-1737072000"), KNOWN);
  assert.equal(status.description, "healthy - 0.15.0-1737072000");
  assert.doesNotMatch(status.description, /available/);
  assert.match(status.tooltip, /does not name a release/);
});

// --- the instance page ------------------------------------------------------

const OVERVIEW = {
  runs: [],
  actions: [],
  nowMs: 0,
  error: "",
} as const;

test("the local instance page carries a latest fact beside version", () => {
  const html = renderInstanceOverview({
    ...OVERVIEW,
    instance: healthy("v0.18.0"),
    releases: KNOWN,
  });
  assert.match(html, /fact-key">version<\/span><span class="fact-value">v0\.18\.0/);
  assert.match(html, /fact-key">latest<\/span><span class="fact-value">v0\.19\.0/);
});

test("the latest fact is present even when nothing was fetched", () => {
  // UNCONDITIONAL, and this is the assertion that keeps it that way. The
  // operator opened this page to ask what this cluster is; a fact that vanished
  // when the answer was "we do not know" would read as "there is nothing
  // newer", which is exactly the reading this epic exists to prevent.
  const html = renderInstanceOverview({
    ...OVERVIEW,
    instance: healthy("v0.18.0"),
    releases: listing([], "git ls-remote: network unreachable"),
  });
  assert.match(html, /fact-key">latest<\/span><span class="fact-value">not fetched/);
  // And the reason reaches the page, because the lede IS the row tooltip.
  assert.match(html, /network unreachable/);
});

test("the remote instance page carries the same latest fact", () => {
  const html = renderRemoteInstance({
    instance: { name: "staging", kind: "remote", presence: "installed-healthy", connected: true },
    runs: [],
    pipeline: { kind: "present", title: "Deploy", detail: "", actions: [] },
    nowMs: 0,
    outcome: "",
    error: "",
    releases: KNOWN,
  });
  assert.match(html, /fact-key">latest<\/span><span class="fact-value">v0\.19\.0/);
});

// --- the tag picker ---------------------------------------------------------

const CHOOSE = {
  instance: healthy("v0.18.0"),
  target: "",
  tagError: "",
  plan: [],
  summary: "",
  sameVersion: false,
} as const;

test("the picker marks the newest option without selecting it", () => {
  const html = renderChooseTag({
    ...CHOOSE,
    listing: { tags: ["v0.19.0", "v0.18.0", "v0.17.1"], error: "" },
  });

  assert.match(html, /<option value="v0\.19\.0">v0\.19\.0 \(newest\)<\/option>/);
  // THE EMPTY FIRST OPTION IS THE SELECTED ONE. `renderChooseTag`'s comment
  // calls it deliberate and this is what holds it: a version the page picked
  // silently is not a version the operator can be held to, and marking which
  // one is newest is help, while selecting it is a decision.
  //
  // Asserted by POSITION rather than by presence: `selected` appearing
  // somewhere in the document is not the claim -- the claim is that it is on
  // the empty option and that the empty option comes first.
  const empty = html.indexOf('<option value="" selected></option>');
  const newestOption = html.indexOf('<option value="v0.19.0"');
  assert.notEqual(empty, -1, "the empty option is not selected");
  assert.notEqual(newestOption, -1, "the newest release is not offered");
  assert.ok(empty < newestOption, "the empty option must come first");
  assert.equal(html.match(/<option [^>]*selected/g)?.length, 1, "exactly one option is selected");
});

test("the newest marker follows the list, not a guess", () => {
  // The list arrives newest-first from install/tags.ts, so the mark is the
  // first entry. A picker offering only older tags marks the newest of THOSE
  // rather than nothing, because the operator is choosing among these.
  const html = renderChooseTag({
    ...CHOOSE,
    listing: { tags: ["v0.17.1", "v0.17.0"], error: "" },
  });
  assert.match(html, /<option value="v0\.17\.1">v0\.17\.1 \(newest\)<\/option>/);
  assert.doesNotMatch(html, /v0\.17\.0 \(newest\)/);
});

test("an operator's own choice is what gets selected", () => {
  const html = renderChooseTag({
    ...CHOOSE,
    target: "v0.18.0",
    listing: { tags: ["v0.19.0", "v0.18.0"], error: "" },
  });
  assert.match(html, /<option value="v0\.18\.0" selected>v0\.18\.0<\/option>/);
  assert.doesNotMatch(html, /<option value="" selected>/);
});
