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

const request = { version: "1" as const, domain: "d.test", kind: "query", name: "q" };

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
