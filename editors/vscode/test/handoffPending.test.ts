import test from "node:test";
import assert from "node:assert/strict";

import { PENDING_HANDOFF_KEY, PENDING_HANDOFF_TTL_MS, storePending, takePending } from "../src/handoff/pending.js";

function memento() {
  const store = new Map<string, unknown>();
  return {
    get<T>(key: string): T | undefined {
      return store.get(key) as T | undefined;
    },
    update(key: string, value: unknown): Thenable<void> {
      if (value === undefined) store.delete(key);
      else store.set(key, value);
      return Promise.resolve();
    },
    has: (key: string) => store.has(key),
  };
}

const request = { version: "1" as const, target: "construct" as const, domain: "d.test", kind: "query", name: "q" };
const artifact = {
  version: "1" as const,
  target: "artifact" as const,
  domain: "d.test",
  kind: "artifact" as const,
  id: "v1:library:artifact:abc",
};

test("a pending handoff is taken exactly once", async () => {
  const m = memento();
  await storePending(m, request, 1000);
  assert.deepEqual(await takePending(m, 2000), request);
  assert.equal(await takePending(m, 2001), undefined);
  assert.equal(m.has(PENDING_HANDOFF_KEY), false);
});

test("an expired handoff is dropped, not replayed", async () => {
  const m = memento();
  await storePending(m, request, 1000);
  assert.equal(await takePending(m, 1000 + PENDING_HANDOFF_TTL_MS + 1), undefined);
  assert.equal(m.has(PENDING_HANDOFF_KEY), false);
});

test("garbage in the memento is ignored", async () => {
  const m = memento();
  await m.update(PENDING_HANDOFF_KEY, { nope: true });
  assert.equal(await takePending(m, 5), undefined);
});

test("an artifact request survives the park and is replayed whole", async () => {
  // The parked request is JSON that outlived the process that wrote it, and the
  // replay composes a link from it -- so an `id` dropped on the way through
  // would replay as a link with no address at all (memql#4748).
  const m = memento();
  await storePending(m, artifact, 1000);
  assert.deepEqual(await takePending(m, 2000), artifact);
});

test("a request whose target and address disagree is not a request", async () => {
  // Validated against the shape of the WHOLE union, not a common subset: an
  // `artifact` target with no id is not a construct request with a field
  // missing, and replaying it as one would open something nobody asked for.
  const m = memento();
  const cases = [
    { request: { version: "1", target: "artifact", domain: "d", kind: "artifact" }, storedAt: 1000 },
    { request: { version: "1", target: "artifact", domain: "d", kind: "query", id: "x" }, storedAt: 1000 },
    { request: { version: "1", target: "construct", domain: "d", kind: "query", id: "x" }, storedAt: 1000 },
    // A build predating `target` parked this. Two-minute TTL, so the only way
    // it exists is an upgrade inside that window; dropping it is the honest
    // answer, and the operator clicks the link again.
    { request: { version: "1", domain: "d", kind: "query", name: "q" }, storedAt: 1000 },
  ];
  for (const value of cases) {
    await m.update(PENDING_HANDOFF_KEY, value);
    assert.equal(await takePending(m, 1001), undefined, JSON.stringify(value));
  }
});
