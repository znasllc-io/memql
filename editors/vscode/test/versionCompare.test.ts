// Version comparison tests (memql#3991).
//
// One property matters more than every other assertion in this file: a value
// this module cannot parse must report `notComparable`, and `notComparable`
// must NEVER be rendered as "up to date". That is the failure direction which
// reproduces the motivating incident -- a extension talking to a cluster older
// than itself, with nothing anywhere saying so.
//
// The comparison lives here rather than in install/tags.ts because that
// module's `parseSemver` deliberately refuses pre-releases and requires the `v`
// prefix: it feeds a PICKER, and a tag it offered that `stackCheckout` could
// not find is worse than a tag it withheld. This module answers a different
// question -- "how does what a cluster reports relate to what exists?" -- about
// values nobody chose from a list. Widening the picker's parser to serve it
// would put release candidates in front of an operator installing a cluster.

import test from "node:test";
import assert from "node:assert/strict";

import {
  compareVersions,
  isReleaseShaped,
  parseRelease,
  type VersionRelation,
} from "../src/version/compare.js";
import { isReleaseTag } from "../src/install/tags.js";

// --- Ordinary release ordering ---------------------------------------------

test("an older recorded version is behind the latest release", () => {
  assert.equal(compareVersions("v0.17.0", "v0.18.0"), "behind");
});

test("the same release is current", () => {
  assert.equal(compareVersions("v0.18.0", "v0.18.0"), "current");
});

test("a newer recorded version is ahead of the latest release", () => {
  // A developer running a locally built cluster. Not an error, and must not be
  // reported as behind.
  assert.equal(compareVersions("v0.19.0", "v0.18.0"), "ahead");
});

test("components compare NUMERICALLY, not lexicographically", () => {
  // `v0.9.2` sorts after `v0.17.0` as a string. A lexicographic comparison
  // would call a nine-month-old cluster up to date.
  assert.equal(compareVersions("v0.9.2", "v0.17.0"), "behind");
  assert.equal(compareVersions("v0.17.0", "v0.9.2"), "ahead");
});

test("minor and patch are each compared in their own right", () => {
  assert.equal(compareVersions("v1.2.3", "v1.3.0"), "behind");
  assert.equal(compareVersions("v1.2.3", "v1.2.4"), "behind");
  assert.equal(compareVersions("v2.0.0", "v1.99.99"), "ahead");
});

// --- The `v` prefix is tolerated on EITHER side -----------------------------
//
// `v0.18.0` and `0.18.0` are the same release. The two sides of this comparison
// come from different places -- one from a release-tag listing, the other from
// whatever a cluster or an install receipt reported -- and they do not agree on
// the prefix. `stackPin.imageTagFor` exists for exactly this skew.

test("a bare recorded version matches a v-prefixed release", () => {
  assert.equal(compareVersions("0.18.0", "v0.18.0"), "current");
});

test("a v-prefixed recorded version matches a bare release", () => {
  assert.equal(compareVersions("v0.18.0", "0.18.0"), "current");
});

test("both sides bare still compare", () => {
  assert.equal(compareVersions("0.17.0", "0.18.0"), "behind");
});

test("surrounding whitespace does not defeat the comparison", () => {
  // The recorded value passes through YAML and several capability envelopes.
  assert.equal(compareVersions("  v0.18.0 ", "v0.18.0"), "current");
});

// --- Pre-releases order BEFORE their release --------------------------------

test("a release candidate is behind its own release", () => {
  assert.equal(compareVersions("v1.0.0-rc.1", "v1.0.0"), "behind");
});

test("a release is ahead of its own release candidate", () => {
  assert.equal(compareVersions("v1.0.0", "v1.0.0-rc.1"), "ahead");
});

test("two pre-releases of the same version order against each other", () => {
  assert.equal(compareVersions("v1.0.0-alpha", "v1.0.0-beta"), "behind");
  assert.equal(compareVersions("v1.0.0-rc.1", "v1.0.0-rc.2"), "behind");
  assert.equal(compareVersions("v1.0.0-rc.2", "v1.0.0-rc.1"), "ahead");
});

test("a numeric pre-release identifier compares numerically", () => {
  // `rc.10` is after `rc.9`, which string comparison gets backwards.
  assert.equal(compareVersions("v1.0.0-rc.9", "v1.0.0-rc.10"), "behind");
});

test("a longer pre-release is ahead when every shared identifier is equal", () => {
  assert.equal(compareVersions("v1.0.0-rc", "v1.0.0-rc.1"), "behind");
});

test("a numeric identifier is behind an alphanumeric one at the same position", () => {
  // Both operands carry a non-digit somewhere in their identifiers, so both
  // survive the build-stamp refusal below and the SemVer rule is what decides.
  // `v1.0.0-1` on its own would NOT reach this comparison -- an all-numeric
  // pre-release is a build stamp here, deliberately, and that rule outranks
  // this one.
  assert.equal(compareVersions("v1.0.0-1.x", "v1.0.0-alpha.x"), "behind");
  assert.equal(compareVersions("v1.0.0-alpha.x", "v1.0.0-1.x"), "ahead");
});

test("a pre-release of a HIGHER version still beats a lower release", () => {
  // The pre-release rule is about the same version triple only. v2.0.0-rc.1 is
  // a newer thing than v1.9.9, and reporting it as behind would be wrong.
  assert.equal(compareVersions("v2.0.0-rc.1", "v1.9.9"), "ahead");
});

// --- notComparable, and the direction it must never fail in -----------------
//
// Each of these is a value a cluster genuinely reports today. What they share
// is that this module cannot place them on the release line at all.

const NOT_COMPARABLE: ReadonlyArray<readonly [string, string]> = [
  ["main", "a branch name"],
  ["feat/cluster-upgrade", "a branch name with a slash"],
  ["HEAD", "a symbolic ref"],
  ["a256ab11", "a short commit sha"],
  ["a256ab11c4f0e9d3b7a1428f6e5d0c9b3a7f2e18", "a full commit sha"],
  ["0.15.0-1737072000", "the Dockerfile's build stamp"],
  ["v0.15.0-1737072000", "the build stamp with a v prefix"],
  ["v1", "the literal ServerHello.version"],
  ["0.18", "a two-component version"],
  ["v0.18.0.1", "a four-component version"],
  ["latest", "a floating tag"],
  ["", "an empty string"],
  ["   ", "whitespace only"],
];

for (const [value, description] of NOT_COMPARABLE) {
  test(`notComparable: ${description} (${JSON.stringify(value)})`, () => {
    assert.equal(compareVersions(value, "v0.18.0"), "notComparable");
    assert.equal(compareVersions("v0.18.0", value), "notComparable");
  });
}

test("an unparseable value NEVER reports as current, in either position", () => {
  // The single most important assertion in this file. "current" is what a
  // surface renders as "up to date", and saying that about a cluster whose
  // version we could not read is precisely the incident this epic exists to
  // prevent -- a extension newer than its cluster, with nothing saying so.
  for (const [value] of NOT_COMPARABLE) {
    const forward: VersionRelation = compareVersions(value, "v0.18.0");
    const backward: VersionRelation = compareVersions("v0.18.0", value);
    assert.notEqual(forward, "current", `${JSON.stringify(value)} must not read as up to date`);
    assert.notEqual(backward, "current", `${JSON.stringify(value)} must not read as up to date`);
  }
});

test("the build stamp is notComparable rather than a pre-release of 0.15.0", () => {
  // `0.15.0-1737072000` is a valid semver pre-release by the letter of the
  // spec, and treating it as one would be actively harmful: EVERY currently
  // shipped engine reports this shape, so a cluster genuinely running v0.18.0
  // would be reported as "0.15.0-something, behind v0.18.0" -- a confident
  // statement about a release it is not running. We cannot tell, so we say so.
  assert.equal(compareVersions("0.15.0-1737072000", "v0.15.0"), "notComparable");
  assert.equal(isReleaseShaped("0.15.0-1737072000"), false);
});

test("an all-numeric pre-release is a build stamp, not a release candidate", () => {
  assert.equal(isReleaseShaped("1.0.0-1"), false);
  assert.equal(isReleaseShaped("1.0.0-20260816"), false);
  assert.equal(isReleaseShaped("1.0.0-1.2"), false);
  // One non-digit anywhere in the identifiers makes it a release we cut.
  assert.equal(isReleaseShaped("1.0.0-rc1"), true);
  assert.equal(isReleaseShaped("1.0.0-1.rc"), true);
});

test("undefined on either side is notComparable", () => {
  // The ordinary case: a cluster with no recorded version, or a release listing
  // that has not been fetched yet. Both are expected, neither is an error.
  assert.equal(compareVersions(undefined, "v0.18.0"), "notComparable");
  assert.equal(compareVersions("v0.18.0", undefined), "notComparable");
  assert.equal(compareVersions(undefined, undefined), "notComparable");
});

// --- parseRelease / isReleaseShaped -----------------------------------------

test("parseRelease returns the components of a release", () => {
  assert.deepEqual(parseRelease("v0.18.0"), {
    major: 0,
    minor: 18,
    patch: 0,
    prerelease: [],
  });
});

test("parseRelease returns the pre-release identifiers", () => {
  assert.deepEqual(parseRelease("v1.2.3-rc.1"), {
    major: 1,
    minor: 2,
    patch: 3,
    prerelease: ["rc", "1"],
  });
});

test("parseRelease returns null for anything not release-shaped", () => {
  assert.equal(parseRelease("main"), null);
  assert.equal(parseRelease("0.15.0-1737072000"), null);
  assert.equal(parseRelease(undefined), null);
});

test("build metadata is ignored for precedence, as semver specifies", () => {
  // It is defined to carry no ordering. Refusing it outright would report a
  // value that IS unambiguously release 1.0.0 as unknown.
  assert.equal(isReleaseShaped("v1.0.0+abc123"), true);
  assert.equal(compareVersions("v1.0.0+abc123", "v1.0.0"), "current");
  assert.equal(compareVersions("v1.0.0+abc123", "v1.0.0+def456"), "current");
});

test("isReleaseShaped is the record-quality test the learners use", () => {
  // memql#3993 refuses to let a non-release-shaped value overwrite a
  // release-shaped one. This predicate is that rule.
  assert.equal(isReleaseShaped("v0.18.0"), true);
  assert.equal(isReleaseShaped("0.18.0"), true);
  assert.equal(isReleaseShaped("v1.0.0-rc.1"), true);
  assert.equal(isReleaseShaped("main"), false);
  assert.equal(isReleaseShaped(""), false);
  assert.equal(isReleaseShaped(undefined), false);
});

test("a leading zero in a component is refused rather than silently coerced", () => {
  // `v01.2.3` is not a tag this repository cuts, and accepting it would make
  // two spellings of the same release compare as equal by accident.
  assert.equal(isReleaseShaped("v01.2.3"), false);
  assert.equal(isReleaseShaped("v1.02.3"), false);
});

// --- The picker's parser stays narrow ---------------------------------------

test("install/tags.ts still refuses everything this module accepts beyond a plain release", () => {
  // The guard for "must not widen parseSemver". That module feeds the install
  // picker, where offering a release candidate would invite an operator to move
  // a cluster to something no overlay was reviewed against -- and offering a
  // bare `0.18.0` would produce a checkout that fails to find the ref.
  assert.equal(isReleaseTag("v0.18.0"), true);
  assert.equal(isReleaseTag("v1.0.0-rc.1"), false, "the picker must not offer a pre-release");
  assert.equal(isReleaseTag("0.18.0"), false, "the picker requires the v prefix");
  // Meanwhile this module accepts all three, because it is answering a
  // different question about values nobody chose from a list.
  assert.equal(isReleaseShaped("v0.18.0"), true);
  assert.equal(isReleaseShaped("v1.0.0-rc.1"), true);
  assert.equal(isReleaseShaped("0.18.0"), true);
});
