// ConceptsCache tests.
//
// ConceptsCache carries the Concepts tree's only concurrency logic: a
// generation counter that stops a slow, superseded listConcepts() call from
// clobbering a fresher load's result -- the same shape of race
// ConnectionManager guards against for connect() (see
// connection/manager.ts, test/manager.test.ts). It is built free of
// `vscode` imports specifically so it can be driven here with a fake fetch
// function instead of a real SDK QueryClient / gRPC round-trip.

import test from "node:test";
import assert from "node:assert/strict";

import { ConceptsCache } from "../src/state/conceptsCache.js";

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

test("a fresh load wins over a late-resolving stale load", async () => {
  const cache = new ConceptsCache<string>();

  const stale = deferred<string[]>();
  const loadStale = cache.load(() => stale.promise);

  // Simulate a cluster switch arriving while `stale` is still in flight:
  // invalidate() bumps the generation before the fresh load starts.
  cache.invalidate();

  const fresh = deferred<string[]>();
  const loadFresh = cache.load(() => fresh.promise);
  fresh.resolve(["fresh-concept"]);
  assert.deepEqual(await loadFresh, ["fresh-concept"]);
  assert.equal(cache.cachedError, undefined);

  // Now let the stale request resolve AFTER the fresh one already won.
  stale.resolve(["stale-concept"]);
  assert.deepEqual(
    await loadStale,
    ["stale-concept"],
    "the stale caller's own promise still resolves to its own data",
  );

  // But the cache itself must still hold the fresh result: a further load()
  // must return it without re-invoking fetch.
  let refetched = false;
  const result = await cache.load(() => {
    refetched = true;
    return Promise.resolve(["should not be called"]);
  });
  assert.equal(refetched, false, "a populated cache must not re-fetch");
  assert.deepEqual(result, ["fresh-concept"], "stale settle must not have overwritten the cache");
});

test("a late-resolving stale rejection does not plant an error over a fresh success", async () => {
  const cache = new ConceptsCache<string>();

  const stale = deferred<string[]>();
  const loadStale = cache.load(() => stale.promise);

  cache.invalidate();

  const fresh = deferred<string[]>();
  const loadFresh = cache.load(() => fresh.promise);
  fresh.resolve(["fresh-concept"]);
  await loadFresh;
  assert.equal(cache.cachedError, undefined);

  stale.reject(new Error("stale boom"));
  assert.deepEqual(
    await loadStale,
    [],
    "a discarded rejection still resolves its own caller to an empty list rather than throwing",
  );

  assert.equal(
    cache.cachedError,
    undefined,
    "the stale rejection must not turn the fresh cache into a sticky error",
  );
  const result = await cache.load(() => Promise.reject(new Error("must not be called")));
  assert.deepEqual(result, ["fresh-concept"], "the cache must still serve the fresh result");
});

test("a non-stale rejection caches the error message and returns an empty list", async () => {
  const cache = new ConceptsCache<string>();

  const result = await cache.load(() => Promise.reject(new Error("boom")));

  assert.deepEqual(result, []);
  assert.equal(cache.cachedError, "boom");
});

test("a non-Error rejection is stringified into the cached error message", async () => {
  const cache = new ConceptsCache<string>();

  await cache.load(() => Promise.reject("plain string failure"));

  assert.equal(cache.cachedError, "plain string failure");
});

test("invalidate() clears both the cache and the error so the next load re-fetches", async () => {
  const cache = new ConceptsCache<string>();

  await cache.load(() => Promise.reject(new Error("boom")));
  assert.equal(cache.cachedError, "boom");

  cache.invalidate();
  assert.equal(cache.cachedError, undefined);

  let calls = 0;
  const result = await cache.load(() => {
    calls++;
    return Promise.resolve(["ok"]);
  });
  assert.equal(calls, 1);
  assert.deepEqual(result, ["ok"]);
  assert.equal(cache.cachedError, undefined);
});

test("a populated cache short-circuits fetch on subsequent load() calls", async () => {
  const cache = new ConceptsCache<string>();

  let calls = 0;
  const fetch = () => {
    calls++;
    return Promise.resolve(["a", "b"]);
  };

  await cache.load(fetch);
  await cache.load(fetch);
  await cache.load(fetch);

  assert.equal(calls, 1, "fetch must run exactly once while the cache holds a value");
});
