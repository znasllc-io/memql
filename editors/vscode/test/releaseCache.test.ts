// Release-listing cache tests (memql#3992).
//
// Three properties, each defending a different failure:
//
//   SINGLE-FLIGHT   -- a tree with N rows must not become N `git ls-remote`
//                      subprocesses. Every row asks; exactly one process runs.
//   STALE-ON-FAILURE-- going offline must not make a known-newer release
//                      vanish. The last good answer survives a failed refresh,
//                      because forgetting it would silently turn "a newer
//                      release exists" into "up to date".
//   NO FETCH ON REST-- constructing the cache, and peeking at it, do no network
//                      work. Activation must stay offline; the first fetch is
//                      triggered by a surface actually being looked at.

import test from "node:test";
import assert from "node:assert/strict";

import { createReleaseCache, latestRelease } from "../src/version/releaseCache.js";
import type { TagListing } from "../src/install/tags.js";

// A fetch stub that counts calls and answers from a script.
function stubFetch(answers: TagListing[]): { fetch: () => Promise<TagListing>; calls: () => number } {
  let n = 0;
  return {
    fetch: async () => {
      const answer = answers[Math.min(n, answers.length - 1)] as TagListing;
      n++;
      return answer;
    },
    calls: () => n,
  };
}

const OK: TagListing = { tags: ["v0.18.0", "v0.17.1", "v0.17.0"], error: "" };
const NEWER: TagListing = { tags: ["v0.19.0", "v0.18.0"], error: "" };
const OFFLINE: TagListing = { tags: [], error: "git ls-remote failed: network is unreachable" };

// A controllable clock, so the TTL is tested by advancing time rather than by
// sleeping.
function clock(start = 1_000): { now: () => number; advance: (ms: number) => void } {
  let t = start;
  return { now: () => t, advance: (ms) => { t += ms; } };
}

// --- Nothing happens until somebody asks ------------------------------------

test("constructing the cache issues no fetch", () => {
  // Activation must do no network work. A cache that fetched on construction
  // would spawn a subprocess every time the extension loads.
  const f = stubFetch([OK]);
  createReleaseCache({ fetch: f.fetch, now: clock().now });
  assert.equal(f.calls(), 0);
});

test("peek returns undefined before anything has been fetched", () => {
  const f = stubFetch([OK]);
  const cache = createReleaseCache({ fetch: f.fetch, now: clock().now });
  assert.equal(cache.peek(), undefined);
  assert.equal(f.calls(), 0);
});

test("peek never triggers a fetch, even after one has happened", async () => {
  // peek is what synchronous render paths call. If it fetched, every tree
  // repaint would be a subprocess.
  const f = stubFetch([OK]);
  const cache = createReleaseCache({ fetch: f.fetch, now: clock().now });
  await cache.get();
  assert.equal(f.calls(), 1);
  cache.peek();
  cache.peek();
  assert.equal(f.calls(), 1);
});

// --- TTL --------------------------------------------------------------------

test("get fetches once and returns the listing", async () => {
  const c = clock();
  const f = stubFetch([OK]);
  const cache = createReleaseCache({ fetch: f.fetch, now: c.now, ttlMs: 1000 });
  const listing = await cache.get();
  assert.deepEqual(listing.tags, ["v0.18.0", "v0.17.1", "v0.17.0"]);
  assert.equal(listing.error, undefined);
  assert.equal(listing.fetchedAt, 1000);
  assert.equal(f.calls(), 1);
});

test("a second get inside the TTL does not re-fetch", async () => {
  const c = clock();
  const f = stubFetch([OK, NEWER]);
  const cache = createReleaseCache({ fetch: f.fetch, now: c.now, ttlMs: 1000 });
  await cache.get();
  c.advance(999);
  const second = await cache.get();
  assert.equal(f.calls(), 1);
  assert.deepEqual(second.tags, OK.tags);
});

test("a get after the TTL expires re-fetches", async () => {
  const c = clock();
  const f = stubFetch([OK, NEWER]);
  const cache = createReleaseCache({ fetch: f.fetch, now: c.now, ttlMs: 1000 });
  await cache.get();
  c.advance(1001);
  const second = await cache.get();
  assert.equal(f.calls(), 2);
  assert.deepEqual(second.tags, NEWER.tags);
});

test("refresh bypasses the TTL", async () => {
  const c = clock();
  const f = stubFetch([OK, NEWER]);
  const cache = createReleaseCache({ fetch: f.fetch, now: c.now, ttlMs: 1_000_000 });
  await cache.get();
  const refreshed = await cache.refresh();
  assert.equal(f.calls(), 2, "refresh must not be answered from the cache");
  assert.deepEqual(refreshed.tags, NEWER.tags);
});

// --- Single-flight ----------------------------------------------------------

test("concurrent gets collapse into ONE fetch", async () => {
  // The motivating case: a Clusters tree with N rows, each asking whether a
  // newer release exists, must not spawn N git subprocesses.
  let release!: (l: TagListing) => void;
  let calls = 0;
  const cache = createReleaseCache({
    fetch: () => {
      calls++;
      return new Promise<TagListing>((resolve) => {
        release = resolve;
      });
    },
    now: clock().now,
  });

  const inFlight = [cache.get(), cache.get(), cache.get(), cache.get(), cache.get()];
  assert.equal(calls, 1, "five callers, one subprocess");
  release(OK);
  const results = await Promise.all(inFlight);
  assert.equal(calls, 1);
  for (const r of results) assert.deepEqual(r.tags, OK.tags);
});

test("a refresh during an in-flight fetch joins it rather than starting a second", async () => {
  let release!: (l: TagListing) => void;
  let calls = 0;
  const cache = createReleaseCache({
    fetch: () => {
      calls++;
      return new Promise<TagListing>((resolve) => {
        release = resolve;
      });
    },
    now: clock().now,
  });

  const first = cache.get();
  const second = cache.refresh();
  assert.equal(calls, 1);
  release(OK);
  assert.deepEqual((await first).tags, OK.tags);
  assert.deepEqual((await second).tags, OK.tags);
  assert.equal(calls, 1);
});

test("a fetch that has settled does not keep answering later callers from its promise", async () => {
  // The in-flight promise must be CLEARED on settle, or the TTL could never
  // expire and refresh would be permanently inert.
  const c = clock();
  const f = stubFetch([OK, NEWER]);
  const cache = createReleaseCache({ fetch: f.fetch, now: c.now, ttlMs: 10 });
  await cache.get();
  c.advance(11);
  await cache.get();
  assert.equal(f.calls(), 2);
});

// --- Serve stale on failure -------------------------------------------------

test("a failed refresh keeps the last good tags", async () => {
  // Going offline must not make a known-newer release vanish. Dropping the
  // tags here would silently turn "a newer release exists" into "up to date",
  // which is the failure direction this epic exists to prevent.
  const c = clock();
  const f = stubFetch([OK, OFFLINE]);
  const cache = createReleaseCache({ fetch: f.fetch, now: c.now, ttlMs: 10 });
  await cache.get();
  c.advance(11);
  const stale = await cache.get();
  assert.deepEqual(stale.tags, OK.tags, "the last good answer must survive");
  assert.equal(stale.error, OFFLINE.error, "and the failure must be reportable");
});

test("fetchedAt describes the TAGS, so a failed refresh does not advance it", async () => {
  // The instance page renders "as of <fetchedAt>" beside a stale listing. If a
  // failed attempt advanced it, the page would claim freshness it does not have.
  const c = clock(5000);
  const f = stubFetch([OK, OFFLINE]);
  const cache = createReleaseCache({ fetch: f.fetch, now: c.now, ttlMs: 10 });
  await cache.get();
  c.advance(11);
  const stale = await cache.get();
  assert.equal(stale.fetchedAt, 5000);
});

test("a failure with no prior success reports empty tags and the reason", async () => {
  const f = stubFetch([OFFLINE]);
  const cache = createReleaseCache({ fetch: f.fetch, now: clock().now });
  const listing = await cache.get();
  assert.deepEqual(listing.tags, []);
  assert.equal(listing.error, OFFLINE.error);
});

test("a later success clears the recorded error", async () => {
  const c = clock();
  const f = stubFetch([OFFLINE, OK]);
  const cache = createReleaseCache({ fetch: f.fetch, now: c.now, ttlMs: 10 });
  await cache.get();
  c.advance(11);
  const recovered = await cache.get();
  assert.equal(recovered.error, undefined);
  assert.deepEqual(recovered.tags, OK.tags);
});

test("a failed attempt still resets the TTL clock, so an offline editor does not hammer", async () => {
  const c = clock();
  const f = stubFetch([OFFLINE]);
  const cache = createReleaseCache({ fetch: f.fetch, now: c.now, ttlMs: 1000 });
  await cache.get();
  c.advance(500);
  await cache.get();
  assert.equal(f.calls(), 1, "a retry inside the TTL would spawn a subprocess per repaint");
});

test("a listing with no releases is a failure for caching purposes, not a good empty answer", async () => {
  // listReleaseTags reports "the repository published no release tags" as an
  // error string, and it is indistinguishable here from a network failure. Both
  // must leave any previously known tags in place.
  const c = clock();
  const noTags: TagListing = { tags: [], error: "The repository published no release tags." };
  const f = stubFetch([OK, noTags]);
  const cache = createReleaseCache({ fetch: f.fetch, now: c.now, ttlMs: 10 });
  await cache.get();
  c.advance(11);
  const after = await cache.get();
  assert.deepEqual(after.tags, OK.tags);
  assert.equal(after.error, noTags.error);
});

// --- latestRelease ----------------------------------------------------------

test("latestRelease is the first tag, because the listing is newest-first", () => {
  assert.equal(latestRelease({ tags: ["v0.18.0", "v0.17.0"], fetchedAt: 1 }), "v0.18.0");
});

test("latestRelease is undefined when nothing is known", () => {
  // Both are ordinary states -- never fetched, or fetched and empty -- and the
  // surfaces render them as "unknown" rather than as an error.
  assert.equal(latestRelease(undefined), undefined);
  assert.equal(latestRelease({ tags: [], fetchedAt: 0 }), undefined);
});

test("latestRelease still answers from a stale listing that carries an error", () => {
  // The whole point of serving stale: offline, the newest KNOWN release is
  // still the honest answer to "is there something newer".
  assert.equal(
    latestRelease({ tags: ["v0.18.0"], error: "network is unreachable", fetchedAt: 1 }),
    "v0.18.0",
  );
});
