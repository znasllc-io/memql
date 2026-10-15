// versionOrder (memql#6033): which of two version times is later.
//
// A row's `createdAt` is the time of its version, and the engine writes it as
// RFC 3339 with the trailing zeros of the fraction trimmed (Go's RFC3339Nano).
// A string comparison therefore gets it wrong exactly where it matters --
// `.3251Z` is the EARLIER instant and the larger string beside `.325101Z` --
// and so would any client that kept the newer of two copies of a row by
// comparing them as text. This is the one comparison, exported so a client does
// not write a second.

import test from "node:test";
import assert from "node:assert/strict";

import { versionOrder } from "../src/client/index.js";

const at = (fraction: string, whole = "2026-10-12T09:00:00") => `${whole}${fraction}`;

test("a version time is ordered by the instant it names, not by its spelling", () => {
  // The pair a string comparison gets backwards.
  assert.ok("a" < "b");
  assert.ok(at(".3251Z") > at(".325101Z"), "as text, the earlier instant is the larger string");
  assert.ok((versionOrder(at(".3251Z"), at(".325101Z")) as number) < 0);
  assert.ok((versionOrder(at(".325101Z"), at(".3251Z")) as number) > 0);
});

test("it keeps every digit the wire carries, to the nanosecond", () => {
  assert.ok((versionOrder(at(".123456788Z"), at(".123456789Z")) as number) < 0);
  assert.ok((versionOrder(at(".123456789Z"), at(".123456788Z")) as number) > 0);
  // A microsecond is not rounded away either: this is the precision the column keeps.
  assert.ok((versionOrder(at(".000001Z"), at("Z")) as number) > 0);
});

test("whole seconds weigh more than any fraction", () => {
  assert.ok((versionOrder(at(".999999999Z"), at(".000000001Z", "2026-10-12T09:00:01")) as number) < 0);
});

test("the same instant is the same order, however it is spelled", () => {
  assert.equal(versionOrder(at(".5Z"), at(".500000Z")), 0);
  assert.equal(versionOrder(at("Z"), at(".000000000Z")), 0);
  // A zone offset is folded into the instant.
  assert.equal(versionOrder("2026-10-12T09:00:00Z", "2026-10-12T10:00:00+01:00"), 0);
  assert.ok((versionOrder("2026-10-12T09:00:00Z", "2026-10-12T09:30:00+01:00") as number) > 0);
});

test("it claims no order where either side is not a time to order by", () => {
  const time = at(".3251Z");
  for (const notATime of [undefined, null, 42, true, {}, [], "", "yesterday", "2026-13-45T99:00:00Z"]) {
    assert.equal(versionOrder(notATime, time), null, `${JSON.stringify(notATime)} on the left`);
    assert.equal(versionOrder(time, notATime), null, `${JSON.stringify(notATime)} on the right`);
  }
  assert.equal(versionOrder(undefined, undefined), null);
});
