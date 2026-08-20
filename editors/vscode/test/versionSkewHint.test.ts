// Version-skew hint tests (memql#4000).
//
// THE ONE MINUTE THAT WOULD HAVE ENDED THE MOTIVATING INCIDENT.
//
// A extension built past v0.18.0 sent `AuthoringValidateBundleMsg.origin`. No
// release carried that field, so every installed cluster refused it -- and
// because the WebSocket bridge decoded with unknown fields ON, the refusal came
// back as an error out of `readLoop` that SEVERED THE SESSION. What the
// operator saw was `ERROR (validate): stream closed`. Nothing said the cluster
// was simply older than the extension.
//
// This is the sentence that says it. It is a HINT and must read as one: a
// closed socket is consistent with version skew, and equally with a laptop
// waking up, a pod restarting, or an ingress timing out. The extension cannot
// prove causation and must not claim it.

import test from "node:test";
import assert from "node:assert/strict";

import { isTransportClose, withVersionSkewHint } from "../src/version/skewHint.js";

const PLUGIN_TAG = "v0.18.0";

const hint = (over: Partial<Parameters<typeof withVersionSkewHint>[0]> = {}): string =>
  withVersionSkewHint({
    message: "stream closed",
    reason: "transport",
    recorded: "v0.17.0",
    extensionTag: PLUGIN_TAG,
    ...over,
  });

// --- When the hint fires ----------------------------------------------------

test("a transport close on a cluster behind the extension gets the hint", () => {
  const out = hint();
  assert.match(out, /^stream closed/, "the original failure stays first and intact");
  assert.match(out, /v0\.17\.0/, "the cluster's recorded version is named");
  assert.match(out, /v0\.18\.0/, "the extension's own pin is named");
  assert.ok(out.length > "stream closed".length, "a sentence was appended");
});

test("the hint names the upgrade action rather than leaving the operator to find it", () => {
  assert.match(hint(), /upgrad/i);
});

test("the hint is phrased as a possibility, not a diagnosis", () => {
  // The extension cannot prove causation from a closed socket. Wording that
  // asserted it would send an operator to upgrade a cluster over an unrelated
  // network blip -- and, worse, would be disbelieved the first time it was
  // wrong, which costs the hint its value in the case it IS right.
  const out = hint();
  assert.match(out, /\b(may|might|can|could|possible|would)\b/i);
});

// --- When it must NOT fire --------------------------------------------------

test("a cluster at the extension's own version gets no hint", () => {
  assert.equal(hint({ recorded: "v0.18.0" }), "stream closed");
});

test("a cluster AHEAD of the extension gets no hint", () => {
  // A developer running a locally built cluster. The skew exists but points
  // the other way, and telling them to upgrade would be wrong.
  assert.equal(hint({ recorded: "v0.19.0" }), "stream closed");
});

test("a cluster with no recorded version gets no hint", () => {
  // We cannot say it is behind, so we do not say anything. The recorded
  // version is exactly what memql#3990 and memql#3993 exist to supply.
  assert.equal(hint({ recorded: undefined }), "stream closed");
  assert.equal(hint({ recorded: "" }), "stream closed");
});

test("a cluster whose version is not comparable gets no hint", () => {
  // A branch name or the build stamp. "Behind" is unprovable, so claiming it
  // would be the same confident-and-wrong move the comparison module refuses.
  for (const recorded of ["main", "0.15.0-1737072000", "a256ab11", "v1"]) {
    assert.equal(hint({ recorded }), "stream closed", `${recorded} must not produce a hint`);
  }
});

test("a failure that is NOT a transport close gets no hint", () => {
  // `reason: "closed"` is the dispatcher being stopped or a request aborted --
  // that is US closing the socket, not the server refusing something. Hinting
  // at version skew when the extension itself hung up would be nonsense.
  assert.equal(hint({ reason: "closed" }), "stream closed");
  assert.equal(hint({ reason: undefined }), "stream closed");
});

test("an ordinary application error gets no hint even on an old cluster", () => {
  // A compile diagnostic, a permission denial, a bad argument. The socket is
  // fine, so a closed-session explanation does not apply.
  const out = withVersionSkewHint({
    message: "PERMISSION_DENIED: promote requires owner",
    recorded: "v0.17.0",
    extensionTag: PLUGIN_TAG,
  });
  assert.equal(out, "PERMISSION_DENIED: promote requires owner");
});

test("an empty message is returned untouched rather than becoming a bare hint", () => {
  assert.equal(hint({ message: "" }), "");
});

// --- isTransportClose -------------------------------------------------------

test("isTransportClose recognises the SDK's transport-reason error", () => {
  const err = Object.assign(new Error("stream closed"), { reason: "transport" });
  assert.equal(isTransportClose(err), true);
});

test("isTransportClose rejects a deliberate close and a plain error", () => {
  assert.equal(isTransportClose(Object.assign(new Error("aborted"), { reason: "closed" })), false);
  assert.equal(isTransportClose(new Error("stream closed")), false);
  assert.equal(isTransportClose("stream closed"), false);
  assert.equal(isTransportClose(undefined), false);
});

test("isTransportClose covers a send failure, which is the same evidence", () => {
  // `pendingError("send failed: ...", "transport")` is the write-side twin of
  // the read-side close: both mean the socket went away underneath a request.
  const err = Object.assign(new Error("send failed: not open"), { reason: "transport" });
  assert.equal(isTransportClose(err), true);
});

// --- Composition stays out of the panel -------------------------------------

test("the hint composes onto whatever message it is given", () => {
  // Kept in a testable module rather than at a render site, so the wording is
  // asserted once and every surface that reports a failure can reuse it.
  const out = withVersionSkewHint({
    message: "ERROR (validate): stream closed",
    reason: "transport",
    recorded: "v0.9.2",
    extensionTag: PLUGIN_TAG,
  });
  assert.match(out, /^ERROR \(validate\): stream closed/);
  assert.match(out, /v0\.9\.2/);
});

test("the extension tag defaults to the shipped pin when not supplied", () => {
  // The surfaces should not each have to remember to pass it.
  const out = withVersionSkewHint({
    message: "stream closed",
    reason: "transport",
    recorded: "v0.9.2",
  });
  assert.match(out, /v0\.9\.2/);
  assert.ok(out.length > "stream closed".length);
});
