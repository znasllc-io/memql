// The display half of install/secrets.ts (memql#4194).
//
// The file gates (redactSecrets / withholdResult) are covered by
// receiptSecrets.test.ts and runLogSecrets.test.ts. These cases cover the
// third family: text on its way to a HUMAN surface -- panels, tooltips and the
// output channels -- where the home directory and any echoed credential must
// not survive.

import test from "node:test";
import assert from "node:assert/strict";

import { maskHomePath, redactForDisplay, SCRUBBED } from "../src/install/secrets.js";

test("maskHomePath folds every occurrence of the home directory to ~", () => {
  const text = "wrote /home/op/.memql/clusters.yaml and read /home/op/.memql/install-receipt.json";
  assert.equal(
    maskHomePath(text, "/home/op"),
    "wrote ~/.memql/clusters.yaml and read ~/.memql/install-receipt.json",
  );
});

test("maskHomePath tolerates a trailing slash on the home it was given", () => {
  assert.equal(maskHomePath("/home/op/.memql", "/home/op/"), "~/.memql");
});

test("maskHomePath refuses to corrupt paths on a degenerate home", () => {
  const text = "/etc/hosts and /home/op/file";
  assert.equal(maskHomePath(text, "/"), text);
  assert.equal(maskHomePath(text, ""), text);
});

test("redactForDisplay scrubs provider keys out of echoed stderr", () => {
  const out = redactForDisplay("using key sk-ant-abcdef1234567890 for provider", "/home/op");
  assert.doesNotMatch(out, /sk-ant/);
  // Verbatim containment, not a regex built by escaping the marker by hand
  // (CodeQL js/incomplete-sanitization): the claim is exactly "the marker
  // appears", and includes() states it without a sanitizer to get wrong.
  assert.ok(out.includes(SCRUBBED), "the scrub marker must replace the key");
});

test("redactForDisplay scrubs every mql_ credential family", () => {
  for (const token of [
    "mql_pat_abcdefghijklmnop",
    "mql_wkr_abcdefghijklmnop",
    "mql_rec_abcdefghijklmnop",
    "mql_enr_abcdefghijklmnop",
  ]) {
    const out = redactForDisplay(`step echoed ${token} to stderr`, "/home/op");
    assert.doesNotMatch(out, /mql_[a-z]{3}_[A-Za-z0-9]/, token);
  }
});

test("redactForDisplay leaves ordinary operational text alone", () => {
  const text = 'kubectl get pods -n memql returned "ImagePullBackOff" for bff-0';
  assert.equal(redactForDisplay(text, "/home/op"), text);
});
