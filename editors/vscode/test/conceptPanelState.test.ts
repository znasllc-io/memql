// ConceptPanelState tests.
//
// ConceptPanelState carries the Concept panel's staleness guards (a page
// response arriving after Reload / a cluster switch, and a row-detail
// response for a since-superseded selection) plus loadPage's concurrency
// guard (two "Load more" clicks before the first response lands must not
// both append the same page). It is built free of `vscode` imports
// specifically so it can be driven here with fake fetch functions instead
// of a real SDK QueryClient / gRPC round-trip. See conceptsCache.test.ts
// for the sibling guard on the Concepts tree.

import test from "node:test";
import assert from "node:assert/strict";

import { ConceptPanelState } from "../src/webview/conceptPanelState.js";

function deferred<T>(): {
  promise: Promise<T>;
  resolve: (v: T) => void;
  reject: (e: unknown) => void;
} {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

test("loadPage appends rows and advances the cursor", async () => {
  const state = new ConceptPanelState<{ id: string }>();

  const changed = await state.loadPage(() =>
    Promise.resolve({ rows: [{ id: "a" }, { id: "b" }], nextCursor: "cursor-1" }),
  );

  assert.equal(changed, true);
  assert.deepEqual(state.nodes, [{ id: "a" }, { id: "b" }]);
  assert.equal(state.nextCursor, "cursor-1");
  assert.equal(state.error, "");
});

test("a fresh reset() wins over a late-resolving stale loadPage()", async () => {
  const state = new ConceptPanelState<{ id: string }>();

  const stale = deferred<{ rows: { id: string }[]; nextCursor: string }>();
  const staleLoad = state.loadPage(() => stale.promise);

  // Simulate Reload (or a cluster switch) arriving while `stale` is still in
  // flight: reset() bumps the generation before the fresh load starts.
  state.reset();

  const fresh = deferred<{ rows: { id: string }[]; nextCursor: string }>();
  const freshLoad = state.loadPage(() => fresh.promise);
  fresh.resolve({ rows: [{ id: "fresh" }], nextCursor: "cursor-fresh" });
  assert.equal(await freshLoad, true);
  assert.deepEqual(state.nodes, [{ id: "fresh" }]);

  // Now let the stale request resolve AFTER the fresh one already won.
  const staleChanged = await (async () => {
    stale.resolve({ rows: [{ id: "stale" }], nextCursor: "cursor-stale" });
    return staleLoad;
  })();

  assert.equal(staleChanged, false, "a discarded settle must report no change");
  assert.deepEqual(
    state.nodes,
    [{ id: "fresh" }],
    "the stale settle must not have appended its rows onto the fresh list",
  );
  assert.equal(state.nextCursor, "cursor-fresh", "the stale settle must not have overwritten the cursor");
});

test("a late-resolving stale loadPage() rejection does not plant an error over a fresh success", async () => {
  const state = new ConceptPanelState<{ id: string }>();

  const stale = deferred<{ rows: { id: string }[]; nextCursor: string }>();
  const staleLoad = state.loadPage(() => stale.promise);

  state.reset();

  const fresh = deferred<{ rows: { id: string }[]; nextCursor: string }>();
  const freshLoad = state.loadPage(() => fresh.promise);
  fresh.resolve({ rows: [{ id: "fresh" }], nextCursor: "" });
  await freshLoad;
  assert.equal(state.error, "");

  stale.reject(new Error("stale boom"));
  assert.equal(await staleLoad, false);
  assert.equal(state.error, "", "the stale rejection must not turn the fresh state into an error");
  assert.deepEqual(state.nodes, [{ id: "fresh" }]);
});

test("a non-stale loadPage() rejection sets the error message", async () => {
  const state = new ConceptPanelState<{ id: string }>();

  const changed = await state.loadPage(() => Promise.reject(new Error("boom")));

  assert.equal(changed, true);
  assert.equal(state.error, "boom");
  assert.deepEqual(state.nodes, []);
});

test("a second loadPage() call while one is already in flight is dropped, not double-fetched", async () => {
  const state = new ConceptPanelState<{ id: string }>();

  let fetchCalls = 0;
  const pending = deferred<{ rows: { id: string }[]; nextCursor: string }>();
  const first = state.loadPage(() => {
    fetchCalls++;
    return pending.promise;
  });

  // Simulate a second "Load more" click landing before the first response
  // returns -- this must not start a second fetch against the same page.
  const second = state.loadPage(() => {
    fetchCalls++;
    return Promise.resolve({ rows: [{ id: "must-not-be-called" }], nextCursor: "" });
  });
  assert.equal(await second, false, "the concurrent call must report no change");
  assert.equal(fetchCalls, 1, "the second call's fetch must never run");

  pending.resolve({ rows: [{ id: "a" }, { id: "b" }], nextCursor: "cursor-1" });
  assert.equal(await first, true);
  assert.deepEqual(
    state.nodes,
    [{ id: "a" }, { id: "b" }],
    "the page must be appended exactly once, not twice",
  );
});

test("loadPage() is not blocked by a still-in-flight call from a since-reset() generation", async () => {
  const state = new ConceptPanelState<{ id: string }>();

  const stale = deferred<{ rows: { id: string }[]; nextCursor: string }>();
  const staleLoad = state.loadPage(() => stale.promise);

  // reset() (Reload / a cluster switch) fires while `stale` is still in
  // flight. A fresh loadPage() issued right after must be allowed to start
  // immediately -- it must not be treated as "a load already in flight"
  // just because the OLD generation's fetch hasn't settled yet.
  state.reset();

  let freshFetchCalls = 0;
  const changed = await state.loadPage(() => {
    freshFetchCalls++;
    return Promise.resolve({ rows: [{ id: "fresh" }], nextCursor: "" });
  });
  assert.equal(changed, true);
  assert.equal(freshFetchCalls, 1);
  assert.deepEqual(state.nodes, [{ id: "fresh" }]);

  // The stale fetch finally settles; it must still be discarded (staleness
  // guard), and must not disturb the fresh generation's in-flight marker.
  stale.resolve({ rows: [{ id: "stale" }], nextCursor: "" });
  assert.equal(await staleLoad, false);
  assert.deepEqual(state.nodes, [{ id: "fresh" }]);
});

test("selectRow: beginSelection sets the selection synchronously", () => {
  const state = new ConceptPanelState<{ id: string }>();
  state.beginSelection("row-1");
  assert.equal(state.selectedRowId, "row-1");
});

test("selecting row B discards row A's late-resolving detail fetch", async () => {
  const state = new ConceptPanelState<{ id: string }>();

  const forA = deferred<{ id: string } | null>();
  const tokenA = state.beginSelection("row-a");
  const resolveA = state.resolveSelection(tokenA, () => forA.promise);

  // The user clicks row B before A's detail arrives.
  const tokenB = state.beginSelection("row-b");
  assert.equal(state.selectedRowId, "row-b");

  const forB = deferred<{ id: string } | null>();
  const resolveB = state.resolveSelection(tokenB, () => forB.promise);
  forB.resolve({ id: "detail-b" });
  assert.equal(await resolveB, true);
  assert.deepEqual(state.detail, { id: "detail-b" });

  // A's fetch finally resolves -- it must not paint over B's detail.
  forA.resolve({ id: "detail-a" });
  const changedA = await resolveA;
  assert.equal(changedA, false, "a superseded selection's settle must report no change");
  assert.deepEqual(
    state.detail,
    { id: "detail-b" },
    "row A's late detail must not overwrite row B's",
  );
  assert.equal(state.selectedRowId, "row-b");
});

test("a superseded selection's rejection does not plant an error over the newer selection", async () => {
  const state = new ConceptPanelState<{ id: string }>();

  const forA = deferred<{ id: string } | null>();
  const tokenA = state.beginSelection("row-a");
  const resolveA = state.resolveSelection(tokenA, () => forA.promise);

  const tokenB = state.beginSelection("row-b");
  const resolveB = state.resolveSelection(tokenB, () => Promise.resolve({ id: "detail-b" }));
  await resolveB;

  forA.reject(new Error("row-a fetch failed"));
  assert.equal(await resolveA, false);
  assert.equal(state.error, "", "row A's stale rejection must not surface as the panel's error");
  assert.deepEqual(state.detail, { id: "detail-b" });
});

test("reset() discards an in-flight selection the same way it discards an in-flight page load", async () => {
  const state = new ConceptPanelState<{ id: string }>();

  const pending = deferred<{ id: string } | null>();
  const token = state.beginSelection("row-1");
  const resolve = state.resolveSelection(token, () => pending.promise);

  state.reset();
  pending.resolve({ id: "detail-1" });

  assert.equal(await resolve, false);
  assert.equal(state.detail, null, "reset() must leave detail cleared, not painted by the stale settle");
  assert.equal(state.selectedRowId, undefined);
});

test("setConnectionError records the message without touching rows or selection", () => {
  const state = new ConceptPanelState<{ id: string }>();
  state.beginSelection("row-1");

  state.setConnectionError("Not connected.");

  assert.equal(state.error, "Not connected.");
  assert.equal(state.selectedRowId, "row-1");
  assert.deepEqual(state.nodes, []);
});

test("reset() clears rows, cursor, selection, detail, and error", async () => {
  const state = new ConceptPanelState<{ id: string }>();

  await state.loadPage(() => Promise.resolve({ rows: [{ id: "a" }], nextCursor: "cursor-1" }));
  const token = state.beginSelection("row-1");
  await state.resolveSelection(token, () => Promise.resolve({ id: "detail-1" }));

  assert.deepEqual(state.nodes, [{ id: "a" }]);
  assert.equal(state.nextCursor, "cursor-1");
  assert.deepEqual(state.detail, { id: "detail-1" });

  state.reset();

  assert.deepEqual(state.nodes, []);
  assert.equal(state.nextCursor, "");
  assert.equal(state.selectedRowId, undefined);
  assert.equal(state.detail, null);
  assert.equal(state.error, "");
});
