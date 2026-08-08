// Session-define lifetime.
//
// This is the invisible failure the whole registry exists for: a
// session-defined construct dies with the stream, and its absence is silent.
// After a reconnect the next ExecuteQueryMsg naming that construct resolves
// against the DEPLOYED definition and returns a perfectly good result --
// computed from code the developer is not looking at. Nothing errors, nothing
// looks wrong, the answer just is not theirs.
//
// So the decision to inject cannot be "have the sources changed". That is
// exactly the test that fails here, because after a reconnect the sources are
// IDENTICAL and the registration is gone.

import test from "node:test";
import assert from "node:assert/strict";

import { SessionRegistry } from "../src/run/session.js";

test("needsInjection -- true with no record at all", () => {
  const s = new SessionRegistry();
  assert.equal(s.needsInjection("local", "query q { }"), true);
});

test("needsInjection -- false immediately after recording the same sources", () => {
  const s = new SessionRegistry();
  s.recordInjection("local", "query q { }");
  assert.equal(s.needsInjection("local", "query q { }"), false);
});

test("needsInjection -- true when the buffer changed", () => {
  // This is what makes an unsaved edit take effect on the very next run,
  // with no save and no redeploy.
  const s = new SessionRegistry();
  s.recordInjection("local", "query q { }");
  assert.equal(s.needsInjection("local", "query q { filter true }"), true);
});

test("needsInjection -- TRUE AFTER A RECONNECT even though the sources are identical", () => {
  // The load-bearing case. Sources equality alone says "already injected";
  // the epoch says the stream that held it is gone.
  const s = new SessionRegistry();
  s.recordInjection("local", "query q { }");
  assert.equal(s.needsInjection("local", "query q { }"), false);

  s.noteStreamReset();

  assert.equal(
    s.needsInjection("local", "query q { }"),
    true,
    "an unchanged buffer must still be re-injected after the stream that held it ended",
  );
});

test("needsInjection -- is per cluster", () => {
  // Each cluster has its own stream and its own authored registry; injecting
  // into one says nothing about the other.
  const s = new SessionRegistry();
  s.recordInjection("local", "query q { }");
  assert.equal(s.needsInjection("staging", "query q { }"), true);
});

test("recordInjection after a reset re-arms the skip", () => {
  const s = new SessionRegistry();
  s.recordInjection("local", "query q { }");
  s.noteStreamReset();
  s.recordInjection("local", "query q { }");
  assert.equal(s.needsInjection("local", "query q { }"), false);
});

test("forget -- a failed define must not leave a record claiming the construct is live", () => {
  // The other direction of the same silent failure: a stale record makes the
  // next run skip the inject and invoke against a registry that no longer
  // holds what the record claims.
  const s = new SessionRegistry();
  s.recordInjection("local", "query q { }");
  s.forget("local");
  assert.equal(s.needsInjection("local", "query q { }"), true);
});

test("lastInjected -- reports the live bundle, and nothing after a reconnect", () => {
  const s = new SessionRegistry();
  s.recordInjection("local", "query q { }");
  assert.equal(s.lastInjected("local"), "query q { }");
  s.noteStreamReset();
  // Not "the sources we last sent" -- "what is believed live right now". A
  // caller asking this after a reconnect must be told nothing is.
  assert.equal(s.lastInjected("local"), undefined);
});

test("streamEpoch -- advances once per reset", () => {
  const s = new SessionRegistry();
  const start = s.streamEpoch;
  s.noteStreamReset();
  s.noteStreamReset();
  assert.equal(s.streamEpoch, start + 2);
});
