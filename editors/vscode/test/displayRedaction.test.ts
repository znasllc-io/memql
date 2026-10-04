// The display half of install/secrets.ts (memql#4194).
//
// The file gates (redactSecrets / withholdResult) are covered by
// receiptSecrets.test.ts and runLogSecrets.test.ts. These cases cover the
// third family: text on its way to a HUMAN surface -- panels, tooltips and the
// output channels -- where the home directory and any echoed credential must
// not survive. That the install page's log actually calls it is asserted in
// addClusterPanel.test.ts.

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

// ---------------------------------------------------------------------------
// session credentials (memql#4625)
// ---------------------------------------------------------------------------
//
// DEFENCE IN DEPTH, NOT A RESPONSE TO A LEAK. The #4625 audit could find no
// path that puts a session token into an error message or an output channel,
// so the protection today is "nothing puts one there" rather than "anything
// that does gets stripped". The first is a property of every current call
// site and dies the moment somebody adds one; the second is a property of
// this function. These pin the second.

// BUILT, NOT PASTED. A JWT literal in a source file is a secret-scanner
// finding wherever that file travels, even when -- as here -- it is three
// base64url blobs of `{"alg":"none"}` with no key behind them. Assembling it
// keeps the test exercising a whole JWT while leaving no token-shaped literal
// in the tree for a scanner to trip over.
function fakeJwt(): string {
  const seg = (o: unknown): string =>
    Buffer.from(JSON.stringify(o)).toString("base64url");
  return [seg({ alg: "none", typ: "JWT" }), seg({ sub: "v1:identity:user:ada" }), seg("sig")].join(
    ".",
  );
}

test("redactForDisplay scrubs a JWT", () => {
  const jwt = fakeJwt();
  assert.ok(jwt.startsWith("eyJ"), "the fixture is not JWT-shaped, so this asserts nothing");
  const out = redactForDisplay(`connect failed with token ${jwt}`, "/home/ada");
  assert.ok(!out.includes(jwt), `the JWT survived: ${out}`);
  assert.ok(!out.includes("eyJ"), `a JWT fragment survived: ${out}`);
  assert.ok(out.includes(SCRUBBED));
});

// A truncated token is still a credential's prefix, and a log that keeps half
// of one has kept the half an attacker starts from.
test("redactForDisplay scrubs a truncated JWT", () => {
  const half = fakeJwt().slice(0, 24);
  const out = redactForDisplay(`Authorization header was ${half}`, "/home/ada");
  assert.ok(!out.includes(half), out);
});

// The scheme survives so the line still reads as an auth header. A redacted
// log is only worth reading if it still says what it was.
test("redactForDisplay scrubs a bearer credential and keeps the scheme", () => {
  const out = redactForDisplay("sent Authorization: Bearer rt_abcdefghijklmnop", "/home/ada");
  assert.ok(!out.includes("rt_abcdefghijklmnop"), `the bearer survived: ${out}`);
  assert.ok(out.includes("Bearer"), `the scheme was eaten, so the line no longer says what it was: ${out}`);
  assert.ok(out.includes(SCRUBBED));
});

// The rules must stay anchored on structure a normal word cannot have. A
// redactor that eats the paths and ids an operator is reading the text FOR is
// the failure `looksLikeProviderKey` documents.
test("redactForDisplay leaves ordinary output alone", () => {
  const ordinary =
    "applied deploy/k8s/overlays/local; node bff-0 ready; concept v1:identity:user; bearing left";
  assert.equal(redactForDisplay(ordinary, "/home/ada"), ordinary);
});
