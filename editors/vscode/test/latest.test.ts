// Latest tests.
//
// Latest is the single supersession guard the whole extension now shares
// (connection/manager.ts, state/conceptsCache.ts, state/conceptPanelState.ts).
// Its callers' own tests exercise it through their real state machines; these
// pin the primitive itself, so a change to the guard fails HERE with a
// one-line diagnosis rather than as four confusing failures three modules
// away.

import test from "node:test";
import assert from "node:assert/strict";

import { Latest } from "../src/async/latest.js";

test("a token taken from current() is current until something supersedes it", () => {
  const latest = new Latest();

  const token = latest.current;

  assert.equal(latest.isCurrent(token), true);
});

test("current() does not supersede anything -- two captures in the same generation agree", () => {
  const latest = new Latest();

  const first = latest.current;
  const second = latest.current;

  assert.equal(first, second, "current() must be a read, not a bump");
  assert.equal(latest.isCurrent(first), true);
  assert.equal(latest.isCurrent(second), true);
});

test("begin() supersedes an outstanding token", () => {
  const latest = new Latest();

  const stale = latest.begin();
  const fresh = latest.begin();

  assert.equal(latest.isCurrent(stale), false, "the older token must be superseded");
  assert.equal(latest.isCurrent(fresh), true);
});

test("begin() supersedes a token captured with current()", () => {
  const latest = new Latest();

  const captured = latest.current;
  latest.begin();

  assert.equal(latest.isCurrent(captured), false);
});

test("invalidate() supersedes without minting a token", () => {
  const latest = new Latest();

  const token = latest.current;
  latest.invalidate();

  assert.equal(latest.isCurrent(token), false);
});

test("invalidate() supersedes a token that begin() minted", () => {
  const latest = new Latest();

  const token = latest.begin();
  latest.invalidate();

  assert.equal(latest.isCurrent(token), false);
});

test("only the newest token is current, however many were minted", () => {
  const latest = new Latest();

  const tokens = [latest.begin(), latest.begin(), latest.begin()];

  assert.deepEqual(
    tokens.map((t) => latest.isCurrent(t)),
    [false, false, true],
    "every superseded token must stay superseded, not just the immediately previous one",
  );
});

test("a superseded token never becomes current again", () => {
  const latest = new Latest();

  const stale = latest.begin();
  latest.invalidate();
  latest.invalidate();
  latest.begin();

  assert.equal(
    latest.isCurrent(stale),
    false,
    "the counter is monotonic -- a reused or wrapped token would make a stale result test as current, " +
      "which is the exact failure this guard exists to prevent",
  );
});

test("current() after begin() names the token begin() returned", () => {
  const latest = new Latest();

  const token = latest.begin();

  assert.equal(latest.current, token);
});

test("current() after invalidate() is a token nothing outstanding holds", () => {
  const latest = new Latest();

  const before = latest.current;
  latest.invalidate();
  const after = latest.current;

  assert.notEqual(before, after);
  assert.equal(latest.isCurrent(after), true);
  assert.equal(latest.isCurrent(before), false);
});

test("two Latest instances are independent", () => {
  const list = new Latest<"list">();
  const selection = new Latest<"selection">();

  const listToken = list.current;
  selection.begin();
  selection.begin();

  assert.equal(
    list.isCurrent(listToken),
    true,
    "one guard's supersessions must not leak into another's -- ConceptPanelState relies on " +
      "selecting a row NOT discarding an in-flight page load",
  );
});

// The staleness pattern the four hand-rolled counters implemented, driven end
// to end against the shared guard: capture before the await, compare after,
// discard on mismatch.
test("the discard-on-mismatch pattern: a stale settle loses to a fresher one", async () => {
  const latest = new Latest();
  let written = "";

  const run = async (label: string, settle: Promise<string>): Promise<boolean> => {
    const token = latest.current;
    const value = await settle;
    if (!latest.isCurrent(token)) return false;
    written = `${label}:${value}`;
    return true;
  };

  let releaseStale!: (v: string) => void;
  const stalePromise = new Promise<string>((resolve) => {
    releaseStale = resolve;
  });
  const staleRun = run("stale", stalePromise);

  // The invalidating event lands while `stale` is still in flight.
  latest.invalidate();
  assert.equal(await run("fresh", Promise.resolve("b")), true);
  assert.equal(written, "fresh:b");

  releaseStale("a");
  assert.equal(await staleRun, false, "the superseded run must report that it wrote nothing");
  assert.equal(written, "fresh:b", "the superseded run must not have written over the fresh result");
});
