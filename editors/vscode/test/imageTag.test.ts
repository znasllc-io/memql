// The image tag an install asks the registry for, per release.
//
// A RELEASE'S IMAGES CARRY THE TAG ITS PUBLICATION WROTE, and that changed once.
// Through v0.31.2 the node images were pushed under the bare version
// (`memql-bff:0.31.2`). From v0.31.3 the native publisher's registry targets
// name the git tag itself (`memql-bff:v0.31.3`), and no bare tag exists for
// those releases at all (`ghcr.io/znasllc-io/memql-*` checked 2026-11-04). An
// install that asked for `0.31.12` was an ImagePullBackOff on every pod.
//
// What would make these fail: `imageTagFor` stripping the `v` from every
// release again (the v0.31.3 rows), or keeping it on every release (the rows
// below it, whose images exist only bare).

import test from "node:test";
import assert from "node:assert/strict";

import {
  DEFAULT_STACK_TAG,
  imageTagFor,
  imageTagForVersion,
} from "../src/install/stackPin.js";

test("a release published before v0.31.3 is asked for by its bare version", () => {
  assert.equal(imageTagFor("v0.17.1"), "0.17.1");
  assert.equal(imageTagFor("v0.30.0"), "0.30.0");
  assert.equal(imageTagFor("v0.31.2"), "0.31.2");
});

test("a release published from v0.31.3 on is asked for by its git tag", () => {
  assert.equal(imageTagFor("v0.31.3"), "v0.31.3");
  assert.equal(imageTagFor("v0.31.12"), "v0.31.12");
  assert.equal(imageTagFor("v1.0.0"), "v1.0.0");
  // Spelled bare, it is still that release, and its images still carry the `v`.
  assert.equal(imageTagFor("0.31.12"), "v0.31.12");
});

test("what is not a release passes through unchanged but for a leading v", () => {
  // No ordering to decide by, so the old conversion stands: nothing here may
  // invent a `v` for a value no release was published under.
  assert.equal(imageTagFor("v0.31"), "0.31");
  assert.equal(imageTagFor("latest"), "latest");
});

test("the offline fallback names images its release published", () => {
  // The pin is what a `--commit` install and the upgrade leg's first install
  // pull. Whatever release it names, the tag derived from it must be the form
  // that release was published under, or every pod of those installs fails to
  // pull.
  assert.equal(imageTagForVersion(""), imageTagFor(DEFAULT_STACK_TAG));
  assert.equal(imageTagForVersion(""), "v0.31.12");
});
