// subscribeConceptRegistry -- the follow-mode registry-delta stream (memql#4238).
//
// A follow subscription is a MULTI-FRAME exchange keyed by request id, so it
// rides the dispatcher's STREAM tier (registerStream / streamRequestId), not the
// event fanout -- the routed side of the ledger in
// sdk/go/client/dispatcher_stream_routing_test.go. These tests drive a mock
// dispatcher that records sends and delivers frames to the registered stream,
// and assert: the follow request shape, that the stream is registered under the
// request id BEFORE the send, snapshot + incremental decode (including the
// uint64 generation string), the queryError refusal path, and that unsubscribe
// removes the listener and sends the UnsubscribeMsg for the id the snapshot
// carried.

import test from "node:test";
import assert from "node:assert/strict";

import { QueryClient } from "../src/client/query.js";
import type { Dispatcher } from "../src/client/dispatcher.js";
import type { ClientMessage, ServerMessage } from "../src/client/wire.js";
import type { ConceptRegistryDelta } from "../src/client/types.js";

class MockDispatcher {
  readonly sent: ClientMessage[] = [];
  // Registration order matters: the stream must exist before the subscribe is
  // sent, or the snapshot races it. `sentWhenRegistered` records how many
  // messages had been sent at registration time so a test can assert that.
  private streams = new Map<string, (msg: ServerMessage) => void>();
  sentWhenRegistered = -1;

  send(msg: ClientMessage): string {
    this.sent.push(msg);
    return "mock-id";
  }

  registerStream(requestId: string, handler: (msg: ServerMessage) => void): () => void {
    this.streams.set(requestId, handler);
    this.sentWhenRegistered = this.sent.length;
    return () => {
      if (this.streams.get(requestId) === handler) this.streams.delete(requestId);
    };
  }

  // emit delivers to the stream registered for the frame's request id, exactly
  // as Dispatcher.route does -- a frame for an unregistered id goes nowhere.
  emit(msg: ServerMessage): void {
    const m = msg as unknown as Record<string, { requestId?: string } | undefined>;
    const reqId = m.conceptsRegistryDelta?.requestId ?? m.queryError?.requestId ?? "";
    this.streams.get(reqId)?.(msg);
  }

  listenerCount(): number {
    return this.streams.size;
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
  // Registered before the send: a snapshot that arrives synchronously with the
  // subscribe must still have a listener waiting for it.
  assert.equal(d.sentWhenRegistered, 0, "the stream must be registered before the subscribe is sent");
  assert.equal(d.listenerCount(), 1);
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

test("a delta for another subscription's request id is never delivered here", () => {
  const d = new MockDispatcher();
  const qc = new QueryClient(d.asDispatcher());
  const got: ConceptRegistryDelta[] = [];
  qc.subscribeConceptRegistry((x) => got.push(x));

  d.emit(delta("some-other-request", { generation: "1", reset: true }));
  assert.equal(got.length, 0, "a delta for another request id must not reach this subscription");
});

test("surfaces a queryError refusal through onError instead of hanging", () => {
  const d = new MockDispatcher();
  const qc = new QueryClient(d.asDispatcher());
  const got: ConceptRegistryDelta[] = [];
  const errors: string[] = [];
  qc.subscribeConceptRegistry((x) => got.push(x), { onError: (e) => errors.push(e.message) });
  const reqId = d.followRequestId();

  // The engine refuses a follow on a node with no engine; that queryError
  // carries this request id, so it routes to this stream.
  d.emit({
    queryError: {
      requestId: reqId,
      error: { message: "concept registry follow requires an engine on this node" },
    },
  } as unknown as ServerMessage);

  assert.equal(got.length, 0);
  assert.equal(errors.length, 1);
  assert.match(errors[0] ?? "", /concept registry follow requires an engine on this node/);
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
