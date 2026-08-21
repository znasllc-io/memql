// subscribeConceptRegistry -- the follow-mode registry-delta stream (memql#4238).
//
// Deltas arrive on the shared event fanout, matched to the subscription by
// request id, so these tests drive a mock dispatcher that records sends and
// emits server messages, and assert: the follow request shape, snapshot +
// incremental decode (including the uint64 generation string), request-id
// filtering, and that unsubscribe removes the listener and sends the
// UnsubscribeMsg for the id the snapshot carried.

import test from "node:test";
import assert from "node:assert/strict";

import { QueryClient } from "../src/client/query.js";
import type { Dispatcher } from "../src/client/dispatcher.js";
import type { ClientMessage, ServerMessage } from "../src/client/wire.js";
import type { ConceptRegistryDelta } from "../src/client/types.js";

class MockDispatcher {
  readonly sent: ClientMessage[] = [];
  private listeners = new Set<(msg: ServerMessage) => void>();

  send(msg: ClientMessage): string {
    this.sent.push(msg);
    return "mock-id";
  }

  addEventListener(handler: (msg: ServerMessage) => void): () => void {
    this.listeners.add(handler);
    return () => this.listeners.delete(handler);
  }

  emit(msg: ServerMessage): void {
    for (const l of [...this.listeners]) l(msg);
  }

  listenerCount(): number {
    return this.listeners.size;
  }

  // The request id the follow subscribe was sent with.
  followRequestId(): string {
    const m = this.sent.find(
      (s) => (s as { conceptsSubscribe?: { follow?: boolean } }).conceptsSubscribe?.follow,
    ) as { conceptsSubscribe?: { requestId?: string } } | undefined;
    return m?.conceptsSubscribe?.requestId ?? "";
  }

  asDispatcher(): Dispatcher {
    return this as unknown as Dispatcher;
  }
}

function delta(reqId: string, body: Record<string, unknown>): ServerMessage {
  return { conceptsRegistryDelta: { requestId: reqId, ...body } } as unknown as ServerMessage;
}

test("sends a follow subscribe with a request id and follow=true", () => {
  const d = new MockDispatcher();
  const qc = new QueryClient(d.asDispatcher());
  qc.subscribeConceptRegistry(() => {});

  assert.equal(d.sent.length, 1);
  const sub = (d.sent[0] as { conceptsSubscribe?: { follow?: boolean; requestId?: string } })
    .conceptsSubscribe;
  assert.ok(sub, "must send a conceptsSubscribe");
  assert.equal(sub?.follow, true);
  assert.ok(sub?.requestId && sub.requestId.length > 0, "must carry a request id");
});

test("decodes the reset snapshot and incremental deltas, parsing the uint64 generation string", () => {
  const d = new MockDispatcher();
  const qc = new QueryClient(d.asDispatcher());
  const got: ConceptRegistryDelta[] = [];
  qc.subscribeConceptRegistry((x) => got.push(x));
  const reqId = d.followRequestId();

  // Snapshot: reset=true, generation as a protojson uint64 STRING.
  d.emit(
    delta(reqId, {
      generation: "7",
      reset: true,
      subscriptionId: "sub-1",
      added: [{ id: "v1:cognition:space", version: "v1", domain: "cognition", entity: "space" }],
    }),
  );
  // Incremental add.
  d.emit(
    delta(reqId, {
      generation: "8",
      added: [{ id: "v1:trainingns:widget", version: "v1", domain: "trainingns", entity: "widget" }],
    }),
  );
  // Incremental remove.
  d.emit(delta(reqId, { generation: "9", removed: ["v1:trainingns:widget"] }));

  assert.equal(got.length, 3);
  const [snap, add, rem] = got;
  assert.ok(snap && add && rem);

  assert.equal(snap.reset, true);
  assert.equal(snap.generation, 7);
  assert.deepEqual(
    snap.added.map((c) => c.id),
    ["v1:cognition:space"],
  );

  assert.equal(add.reset, false);
  assert.equal(add.generation, 8);
  assert.equal(add.added[0]?.domain, "trainingns");

  assert.equal(rem.generation, 9);
  assert.deepEqual(rem.removed, ["v1:trainingns:widget"]);
});

test("ignores deltas addressed to a different subscription's request id", () => {
  const d = new MockDispatcher();
  const qc = new QueryClient(d.asDispatcher());
  const got: ConceptRegistryDelta[] = [];
  qc.subscribeConceptRegistry((x) => got.push(x));

  d.emit(delta("some-other-request", { generation: "1", reset: true }));
  assert.equal(got.length, 0, "a delta for another request id must be ignored");
});

test("unsubscribe removes the listener and sends UnsubscribeMsg for the snapshot's id", () => {
  const d = new MockDispatcher();
  const qc = new QueryClient(d.asDispatcher());
  const got: ConceptRegistryDelta[] = [];
  const follow = qc.subscribeConceptRegistry((x) => got.push(x));
  const reqId = d.followRequestId();

  d.emit(delta(reqId, { generation: "1", reset: true, subscriptionId: "sub-42" }));
  assert.equal(d.listenerCount(), 1);

  follow.unsubscribe();
  assert.equal(d.listenerCount(), 0, "unsubscribe must remove the event listener");

  const unsub = d.sent.find((s) => (s as { unsubscribe?: unknown }).unsubscribe) as
    | { unsubscribe?: { subscriptionId?: string } }
    | undefined;
  assert.ok(unsub, "unsubscribe must send an UnsubscribeMsg");
  assert.equal(unsub?.unsubscribe?.subscriptionId, "sub-42");

  // Post-unsubscribe deltas are not delivered.
  d.emit(delta(reqId, { generation: "2", added: [] }));
  assert.equal(got.length, 1);
});

test("unsubscribe before the snapshot removes the listener and sends no UnsubscribeMsg", () => {
  const d = new MockDispatcher();
  const qc = new QueryClient(d.asDispatcher());
  const follow = qc.subscribeConceptRegistry(() => {});

  follow.unsubscribe();
  assert.equal(d.listenerCount(), 0);
  const unsub = d.sent.find((s) => (s as { unsubscribe?: unknown }).unsubscribe);
  assert.equal(unsub, undefined, "no subscription id yet, so no UnsubscribeMsg");

  // Idempotent.
  follow.unsubscribe();
});

test("an AbortSignal unsubscribes", () => {
  const d = new MockDispatcher();
  const qc = new QueryClient(d.asDispatcher());
  const ac = new AbortController();
  qc.subscribeConceptRegistry(() => {}, { signal: ac.signal });
  assert.equal(d.listenerCount(), 1);
  ac.abort();
  assert.equal(d.listenerCount(), 0, "aborting the signal must unsubscribe");
});
