// How a version is put into words (memql#3995).
//
// SIX RENDER STATES, and the reason there are six rather than three is that
// "we do not know" splits three ways and each asks the operator for something
// different:
//
//   unknown        nothing has reported a version for this cluster at all.
//   unfetched      we know the cluster's version but not the newest release --
//                  offline, or the listing failed. We cannot say "up to date".
//   notComparable  the cluster reported something that is not a release (a
//                  branch, a sha, the build stamp). Equally unsayable.
//
// Collapsing any of them into "current" is the failure this epic exists to
// prevent, so a test walks every state and asserts none of them claims it.

import test from "node:test";
import assert from "node:assert/strict";

import { describeVersion } from "../src/version/describe.js";
import type { ReleaseListing } from "../src/version/releaseCache.js";

const listing = (tags: string[], error?: string): ReleaseListing => ({
  tags,
  error,
  fetchedAt: 1000,
});

const KNOWN = listing(["v0.18.0", "v0.17.1", "v0.17.0"]);

// --- Behind: the state the whole epic is for --------------------------------

test("a cluster behind the newest release says so, and names the release", () => {
  const d = describeVersion({ recorded: "v0.17.0", listing: KNOWN });
  assert.equal(d.state, "behind");
  assert.equal(d.upgradeAvailable, true);
  assert.equal(d.latest, "v0.18.0");
  assert.match(d.short, /v0\.17\.0/);
  assert.match(d.short, /v0\.18\.0/);
  assert.match(d.sentence, /v0\.17\.0/);
  assert.match(d.sentence, /v0\.18\.0/);
});

test("the short form stays short enough for a tree row", () => {
  // It is rendered as dimmed text after the cluster's name, beside an endpoint.
  const d = describeVersion({ recorded: "v0.17.0", listing: KNOWN });
  assert.ok(d.short.length <= 40, `too long for a row description: ${d.short}`);
});

// --- Current: says nothing extra --------------------------------------------

test("a current cluster adds no availability clause", () => {
  const d = describeVersion({ recorded: "v0.18.0", listing: KNOWN });
  assert.equal(d.state, "current");
  assert.equal(d.upgradeAvailable, false);
  assert.equal(d.short, "v0.18.0", "nothing is appended when there is nothing to say");
});

// --- Ahead: ordinary, and not an error --------------------------------------

test("a cluster ahead of the newest release is reported without alarm", () => {
  // A developer running a locally built cluster. Offering them an "upgrade"
  // to an older release would be wrong.
  const d = describeVersion({ recorded: "v0.19.0", listing: KNOWN });
  assert.equal(d.state, "ahead");
  assert.equal(d.upgradeAvailable, false);
  assert.equal(d.short, "v0.19.0");
  assert.doesNotMatch(d.sentence, /available/i);
});

// --- Unknown: nothing has reported a version --------------------------------

test("a cluster with no recorded version reads as unknown", () => {
  const d = describeVersion({ recorded: undefined, listing: KNOWN });
  assert.equal(d.state, "unknown");
  assert.equal(d.upgradeAvailable, false);
  assert.equal(d.short, "", "an unknown version adds nothing to a tree row");
  assert.match(d.sentence, /unknown/i, "but the page says the word");
});

test("an empty or blank recorded version is unknown, not an empty release", () => {
  assert.equal(describeVersion({ recorded: "", listing: KNOWN }).state, "unknown");
  assert.equal(describeVersion({ recorded: "   ", listing: KNOWN }).state, "unknown");
});

// --- Unfetched: we know the cluster but not the world -----------------------

test("a known cluster with no release listing is unfetched, not current", () => {
  // Offline. We know what the cluster runs and nothing about what exists, so
  // "up to date" is exactly the claim we cannot make.
  const d = describeVersion({ recorded: "v0.17.0", listing: undefined });
  assert.equal(d.state, "unfetched");
  assert.equal(d.upgradeAvailable, false);
  assert.equal(d.short, "v0.17.0", "the version we DO know still shows");
});

test("an unfetched state names the fetch failure so the tooltip can explain itself", () => {
  const d = describeVersion({
    recorded: "v0.17.0",
    listing: listing([], "git ls-remote failed: network is unreachable"),
  });
  assert.equal(d.state, "unfetched");
  assert.match(d.sentence, /network is unreachable/);
});

test("a STALE listing still compares, because a known-newer release must not vanish", () => {
  // Serve-stale-on-failure exists precisely so going offline does not turn
  // "a newer release exists" into silence. The error is reported alongside.
  const d = describeVersion({
    recorded: "v0.17.0",
    listing: listing(["v0.18.0"], "network is unreachable"),
  });
  assert.equal(d.state, "behind");
  assert.equal(d.upgradeAvailable, true);
  assert.match(d.sentence, /network is unreachable/, "the staleness is still disclosed");
});

// --- notComparable: the cluster said something that is not a release --------

test("a branch name is reported verbatim and compared to nothing", () => {
  const d = describeVersion({ recorded: "main", listing: KNOWN });
  assert.equal(d.state, "notComparable");
  assert.equal(d.upgradeAvailable, false);
  assert.equal(d.short, "main", "what the cluster said is shown, not hidden");
  assert.match(d.sentence, /main/);
});

test("the build stamp is notComparable, and the sentence says why that is not up to date", () => {
  const d = describeVersion({ recorded: "0.15.0-1737072000", listing: KNOWN });
  assert.equal(d.state, "notComparable");
  assert.match(d.sentence, /0\.15\.0-1737072000/);
});

test("an uncut build's revision reaches the row, which is why it is in the value", () => {
  // THE SURFACE memql#4575 IS ABOUT. `dev` alone told an operator nothing: a
  // cluster rebuilt an hour ago and one installed last week rendered
  // identically. The revision rides the recorded value precisely so that this
  // function -- which shows `recorded` verbatim in this state -- carries it to
  // every row and page without any of them learning a second field.
  const d = describeVersion({ recorded: "dev+a1b2c3d4e5f6", listing: KNOWN });
  assert.equal(d.state, "notComparable");
  assert.equal(d.upgradeAvailable, false);
  assert.equal(d.short, "dev+a1b2c3d4e5f6");
  assert.match(d.sentence, /dev\+a1b2c3d4e5f6/);
});

// --- The rule that outranks every other assertion here ----------------------

test("NO state other than current ever reads as up to date", () => {
  // The failure direction that reproduces the motivating incident. Every way
  // of not knowing must be distinguishable from knowing the answer is yes.
  const cases = [
    { recorded: undefined, listing: KNOWN },
    { recorded: "", listing: KNOWN },
    { recorded: "v0.17.0", listing: undefined },
    { recorded: "v0.17.0", listing: listing([], "offline") },
    { recorded: "main", listing: KNOWN },
    { recorded: "0.15.0-1737072000", listing: KNOWN },
    { recorded: "a256ab11", listing: KNOWN },
    { recorded: "dev", listing: KNOWN },
    { recorded: "dev+a1b2c3d4e5f6", listing: KNOWN },
    { recorded: "dev+a1b2c3d4e5f6-dirty", listing: KNOWN },
    { recorded: "v1", listing: KNOWN },
  ];
  for (const input of cases) {
    const d = describeVersion(input);
    assert.notEqual(d.state, "current", `${JSON.stringify(input)} must not read as current`);
    assert.doesNotMatch(
      d.sentence,
      /\bup to date\b|\bnewest release\b/i,
      `${JSON.stringify(input)} must not claim to be up to date`,
    );
  }
});

test("upgradeAvailable is true ONLY when behind", () => {
  // It gates a button that moves a cluster. Every other state must leave it
  // off rather than offering an action nothing supports.
  assert.equal(describeVersion({ recorded: "v0.17.0", listing: KNOWN }).upgradeAvailable, true);
  for (const recorded of [undefined, "", "v0.18.0", "v0.19.0", "main", "0.15.0-1737072000"]) {
    assert.equal(
      describeVersion({ recorded, listing: KNOWN }).upgradeAvailable,
      false,
      `${String(recorded)} must not offer an upgrade`,
    );
  }
});

test("every state produces a non-empty sentence", () => {
  // The connection page renders one unconditionally, so an empty string would
  // be a blank fact rather than an explanation.
  for (const recorded of [undefined, "v0.17.0", "v0.18.0", "v0.19.0", "main"]) {
    for (const l of [KNOWN, undefined, listing([], "offline")]) {
      assert.match(describeVersion({ recorded, listing: l }).sentence, /\S/);
    }
  }
});
