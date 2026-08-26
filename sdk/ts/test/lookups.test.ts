// Relationship lookups: batching, coalescing, and what a cell shows when a
// row cannot be resolved (epic memql#4661, task memql#4671).
//
// The two properties worth pinning are the ones that make batching worth
// doing at all, and each fails in a way a page still LOOKS fine under:
//
//   * one read per (concept, id set). Resolve per cell and a table of a
//     hundred rows issues a hundred reads -- the page renders correctly and
//     the cluster carries a hundred times the load;
//
//   * coalescing. Several cells and several columns ask for the same ids in
//     the same tick; without the in-flight map, a render stampedes its own
//     cluster on mount for answers already on their way.

import test from "node:test";
import assert from "node:assert/strict";

import { LookupCache, isRefBinding, parseRefBinding } from "../src/client/lookups.js";
import { Result } from "../src/client/types.js";
import type { QueryClient } from "../src/client/query.js";

interface Call {
  name: string;
  call: string;
}

// fakeQuery records what was asked and answers from a fixed row set. Resolve
// is deferred through a queue so the coalescing test can hold a read open --
// concurrency is the thing under test, so the test has to be able to create
// some.
function fakeQuery(rows: Record<string, Record<string, unknown>>) {
  const calls: Call[] = [];
  let release: (() => void) | undefined;
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });
  let gated = false;

  const query = {
    executeNamed: async (name: string, call: string) => {
      calls.push({ name, call });
      if (gated) await gate;
      const wanted = [...call.matchAll(/"([^"]+)"/g)].map((m) => m[1]!);
      const found = wanted.filter((id) => rows[id] !== undefined).map((id) => rows[id]!);
      return new Result({ bundle: { nodes: found as never }, meta: { cursor: "" } });
    },
  } as unknown as QueryClient;

  return {
    query,
    calls,
    hold: () => {
      gated = true;
    },
    release: () => {
      gated = false;
      release?.();
    },
  };
}

const AGENTS = {
  "agent:a1": { id: "agent:a1", concept: "v1:agents:agent", payload: { name: "Planner" } },
  "agent:a2": { id: "agent:a2", concept: "v1:agents:agent", payload: { name: "Scribe" } },
};

test("parseRefBinding reads a relationship path and refuses everything else", () => {
  assert.deepEqual(parseRefBinding("ref:ownerAgent.name"), { as: "ownerAgent", field: "name" });
  assert.equal(isRefBinding("ref:ownerAgent.name"), true);

  // A plain field name is not a lookup, even one that starts with the prefix
  // and has no dot: a binding that named a relationship and no field is not a
  // PARTIAL lookup, it is a field name.
  assert.equal(parseRefBinding("ownerAgentId"), undefined);
  assert.equal(parseRefBinding("ref:ownerAgent"), undefined);
  assert.equal(parseRefBinding("ref:.name"), undefined);
  assert.equal(parseRefBinding("ref:ownerAgent."), undefined);
  assert.equal(isRefBinding("ownerAgentId"), false);
});

test("one read per (concept, id set), not one per cell", () => {
  const { query, calls } = fakeQuery(AGENTS);
  const cache = new LookupCache();

  // Twenty cells' worth of pointers, three distinct.
  const ids = Array.from({ length: 20 }, (_, i) => `agent:a${(i % 3) + 1}`);
  return cache.resolve(query, "v1:agents:agent", ids).then(() => {
    assert.equal(calls.length, 1, `expected one read, got ${calls.length}`);
    // ...and it asked for the DISTINCT ids only.
    const asked = [...calls[0]!.call.matchAll(/"([^"]+)"/g)].map((m) => m[1]);
    assert.deepEqual(asked.sort(), ["agent:a1", "agent:a2", "agent:a3"]);
    // Membership, not an or-chain: the or-chain grows the parsed expression
    // linearly in the id count.
    assert.match(calls[0]!.call, /id in \[/);
  });
});

test("a second ask for cached ids issues no read at all", async () => {
  const { query, calls } = fakeQuery(AGENTS);
  const cache = new LookupCache();
  await cache.resolve(query, "v1:agents:agent", ["agent:a1", "agent:a2"]);
  assert.equal(calls.length, 1);
  await cache.resolve(query, "v1:agents:agent", ["agent:a1", "agent:a2"]);
  assert.equal(calls.length, 1, "a cached set must not be re-read");
});

test("concurrent asks for the same set share ONE read", async () => {
  // Without this, a render issues one read per asker for an answer already in
  // flight -- which is a page stampeding its own cluster on mount.
  const fake = fakeQuery(AGENTS);
  fake.hold();
  const cache = new LookupCache();

  const a = cache.resolve(fake.query, "v1:agents:agent", ["agent:a1", "agent:a2"]);
  const b = cache.resolve(fake.query, "v1:agents:agent", ["agent:a2", "agent:a1"]);
  const c = cache.resolve(fake.query, "v1:agents:agent", ["agent:a1", "agent:a2"]);
  fake.release();
  await Promise.all([a, b, c]);

  assert.equal(fake.calls.length, 1, `three concurrent asks issued ${fake.calls.length} reads`);
});

test("a row the read did not return is cached as ABSENT, and asked for once", async () => {
  // Deleted, or filtered out by the caller's row authz -- indistinguishable
  // here by design, because an authz-filtered read returns fewer rows rather
  // than an error, and the cell renders the id either way.
  const { query, calls } = fakeQuery(AGENTS);
  const cache = new LookupCache();

  await cache.resolve(query, "v1:agents:agent", ["agent:a1", "agent:gone"]);
  assert.equal(cache.get("v1:agents:agent", "agent:gone"), null);
  assert.notEqual(cache.get("v1:agents:agent", "agent:a1"), null);

  // Cached, so a re-render does not re-ask. Without this the WORST case --
  // a page pointing at a deleted row -- re-reads on every render forever.
  await cache.resolve(query, "v1:agents:agent", ["agent:gone"]);
  assert.equal(calls.length, 1);
});

test("a failed read caches nothing", async () => {
  // A transport error is not evidence that a row is absent. Caching it as one
  // would turn a blip into a permanently wrong cell.
  let fail = true;
  const query = {
    executeNamed: async () => {
      if (fail) throw new Error("stream closed");
      return new Result({ bundle: { nodes: [AGENTS["agent:a1"]] as never }, meta: { cursor: "" } });
    },
  } as unknown as QueryClient;

  const cache = new LookupCache();
  await assert.rejects(() => cache.resolve(query, "v1:agents:agent", ["agent:a1"]));
  assert.equal(cache.has("v1:agents:agent", "agent:a1"), false);

  fail = false;
  await cache.resolve(query, "v1:agents:agent", ["agent:a1"]);
  assert.equal(cache.has("v1:agents:agent", "agent:a1"), true);
});

test("the three cache states are distinguishable", () => {
  // A caller renders differently for each: the value, the id, and
  // nothing-yet. Collapsing "not asked" and "absent" would make the first
  // paint indistinguishable from a deleted target.
  const cache = new LookupCache();
  assert.equal(cache.get("v1:agents:agent", "agent:a1"), undefined);
  assert.equal(cache.has("v1:agents:agent", "agent:a1"), false);
});

test("the cache is bounded, so a long scroll does not retain every target row", async () => {
  const many: Record<string, Record<string, unknown>> = {};
  for (let i = 0; i < 30; i += 1) {
    many[`agent:x${i}`] = { id: `agent:x${i}`, concept: "v1:agents:agent", payload: {} };
  }
  const { query } = fakeQuery(many);
  const cache = new LookupCache(10);

  for (let i = 0; i < 30; i += 1) {
    await cache.resolve(query, "v1:agents:agent", [`agent:x${i}`]);
  }
  // The oldest are gone and the newest are held. The bound is what matters;
  // which ten survive is an eviction-policy detail.
  assert.equal(cache.has("v1:agents:agent", "agent:x0"), false);
  assert.equal(cache.has("v1:agents:agent", "agent:x29"), true);
});
