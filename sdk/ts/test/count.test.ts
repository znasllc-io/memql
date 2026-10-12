// A `count` query's answer (memql#6033): how many rows matched, and whether
// that is the whole number or the engine's window.
//
// The engine reads a count up to its window (5,000 by default, and
// configurable), and one row past it: a set larger than the window answers the
// window with `atLeast` true. A client cannot guess the window, so it reads
// the signal rather than comparing the figure with a number it assumes.

import test from "node:test";
import assert from "node:assert/strict";

import { Result } from "../src/client/types.js";

const answer = (data: unknown[]) => new Result({ data } as never);

test("a count within the window is exact", () => {
  assert.deepEqual(answer([{ count: 1204, atLeast: false }]).count(), { count: 1204, atLeast: false });
  assert.deepEqual(answer([{ count: 0, atLeast: false }]).count(), { count: 0, atLeast: false });
});

test("a count that reached the window says it is at least that many", () => {
  assert.deepEqual(answer([{ count: 5000, atLeast: true }]).count(), { count: 5000, atLeast: true });
});

test("only the engine's own true makes a count a floor", () => {
  // Whatever else arrives there -- nothing, a string, a number -- the figure
  // is the one the engine counted, not one it stopped at.
  assert.deepEqual(answer([{ count: 12 }]).count(), { count: 12, atLeast: false });
  assert.deepEqual(answer([{ count: 12, atLeast: "true" }]).count(), { count: 12, atLeast: false });
});

test("a count carried as a string is read as the number it names", () => {
  // The wire carries large integers as strings in places; a count is one.
  assert.deepEqual(answer([{ count: "5000", atLeast: true }]).count(), { count: 5000, atLeast: true });
});

test("a result that is not a count has no count", () => {
  assert.equal(answer([]).count(), null);
  assert.equal(answer([{ id: "a", name: "row" }]).count(), null);
  assert.equal(answer([{ count: "" }]).count(), null);
  assert.equal(answer([{ count: "many" }]).count(), null);
  assert.equal(new Result(null).count(), null);
});
