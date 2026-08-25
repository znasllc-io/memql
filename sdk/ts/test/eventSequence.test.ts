// The live-data continuity fields on a delivered event (memql#4536).
//
// seq numbers every notification on a stream; gapBefore says deliveries were
// dropped between the previous one and this one. Both are DECODE-only here --
// LiveCollection is what acts on them -- so what these tests pin is that the
// wire's several spellings of "no sequence" all arrive as the same value.

import test from "node:test";
import assert from "node:assert/strict";

import { eventFromWire } from "../src/client/types.js";

test("seq decodes protojson's uint64 string", () => {
  const ev = eventFromWire({ subscriptionId: "s1", seq: "42" });
  assert.equal(ev.seq, 42);
  assert.equal(ev.gapBefore, false);
});

test("seq decodes a plain number from a bridge that emits one", () => {
  assert.equal(eventFromWire({ subscriptionId: "s1", seq: 7 }).seq, 7);
});

test("every unusable seq spelling reads as 0, the same as an older server", () => {
  // A server predating the field omits it; the current bridge marshals with
  // EmitUnpopulated and sends "0"; a malformed value is neither. All three
  // must land on one case, or a consumer grows three.
  for (const raw of [undefined, "0", "", "not-a-number", -1, Number.NaN]) {
    const ev = eventFromWire({ subscriptionId: "s1", ...(raw === undefined ? {} : { seq: raw }) });
    assert.equal(ev.seq, 0, `seq ${String(raw)} must decode as unnumbered`);
  }
});

test("gapBefore is a real boolean, absent or not", () => {
  // protojson omits a false bool, so the field is missing on every ordinary
  // event -- a consumer reading it raw would be reading undefined.
  assert.equal(eventFromWire({ subscriptionId: "s1" }).gapBefore, false);
  assert.equal(eventFromWire({ subscriptionId: "s1", gapBefore: true }).gapBefore, true);
});

test("the continuity fields do not disturb the payload-omitted contract", () => {
  const ev = eventFromWire({
    subscriptionId: "s1",
    kind: "EVENT_KIND_NODE_UPDATED",
    payloadOmitted: true,
    seq: "3",
    gapBefore: true,
  });
  assert.equal(ev.payloadOmitted, true);
  assert.equal(ev.kind, "NODE_UPDATED");
  assert.equal(ev.seq, 3);
  assert.equal(ev.gapBefore, true);
});
