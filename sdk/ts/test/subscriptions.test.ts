// Structured graph subscription surface (memql#2460). subscribeGraph()
// sends concept + actions and NEVER a free-text topic string; the server
// composes the bus topic. subscribe(pattern) survives only for the
// non-graph kinds and throws for graph_events.

import test from "node:test";
import assert from "node:assert/strict";

import { SubscriptionManager } from "../src/client/subscriptions.js";
import type { Dispatcher } from "../src/client/dispatcher.js";
import type { ClientMessage, ServerMessage, SubscribePayload } from "../src/client/wire.js";
import type { Event } from "../src/client/types.js";

class MockDispatcher {
  readonly sent: ClientMessage[] = [];
  private listeners: Array<(msg: ServerMessage) => void> = [];

  send(msg: ClientMessage): string {
    this.sent.push(msg);
    return "mock-id";
  }

  addEventListener(handler: (msg: ServerMessage) => void): () => void {
    this.listeners.push(handler);
    return () => {};
  }

  registerStream(): () => void {
    return () => {};
  }

  async sendAndWait(): Promise<ServerMessage> {
    throw new Error("unused");
  }

  emit(msg: ServerMessage): void {
    for (const l of this.listeners) l(msg);
  }

  lastSubscribe(): SubscribePayload {
    const msg = this.sent.at(-1) as unknown as { subscribe?: SubscribePayload };
    if (!msg?.subscribe) throw new Error("last message was not a subscribe");
    return msg.subscribe;
  }

  lastUnsubscribe(): { subscriptionId: string } {
    const msg = this.sent.at(-1) as unknown as { unsubscribe?: { subscriptionId: string } };
    if (!msg?.unsubscribe) throw new Error("last message was not an unsubscribe");
    return msg.unsubscribe;
  }

  asDispatcher(): Dispatcher {
    return this as unknown as Dispatcher;
  }
}

test("subscribeGraph sends structured concept + actions, no filter", () => {
  const d = new MockDispatcher();
  const sm = new SubscriptionManager(d.asDispatcher());
  sm.subscribeGraph(() => {}, {
    concept: "v1:cognition:utterance",
    actions: ["created", "updated"],
  });
  const sub = d.lastSubscribe();
  assert.equal(sub.kind, "SUBSCRIPTION_KIND_GRAPH_EVENTS");
  assert.equal(sub.concept, "v1:cognition:utterance");
  assert.deepEqual(sub.actions, ["GRAPH_NODE_ACTION_CREATED", "GRAPH_NODE_ACTION_UPDATED"]);
  assert.equal(sub.filter, undefined, "graph subscribe must not send a free-text filter");
});

test("subscribeGraph with no options means all concepts + all actions", () => {
  const d = new MockDispatcher();
  const sm = new SubscriptionManager(d.asDispatcher());
  sm.subscribeGraph(() => {});
  const sub = d.lastSubscribe();
  assert.equal(sub.kind, "SUBSCRIPTION_KIND_GRAPH_EVENTS");
  assert.equal(sub.concept, undefined);
  assert.equal(sub.actions, undefined);
  assert.equal(sub.filter, undefined);
});

test("compact graph subscription survives reconnect replay", () => {
  const d = new MockDispatcher();
  const sm = new SubscriptionManager(d.asDispatcher());
  sm.subscribeGraph(() => {}, { concept: "v1:work:run", idOnly: true });
  assert.deepEqual(d.lastSubscribe().config, { idOnly: true });
  sm.replay();
  assert.deepEqual(d.lastSubscribe().config, { idOnly: true });
});

test("subscribe rejects graph_events (default and explicit)", () => {
  const d = new MockDispatcher();
  const sm = new SubscriptionManager(d.asDispatcher());
  assert.throws(() => sm.subscribe("node.created.#", () => {}), /subscribeGraph/);
  assert.throws(
    () => sm.subscribe("node.created.#", () => {}, { kind: "graph_events" }),
    /subscribeGraph/,
  );
});

test("subscribe forwards a non-graph free-text filter", () => {
  const d = new MockDispatcher();
  const sm = new SubscriptionManager(d.asDispatcher());
  sm.subscribe("automation.#", () => {}, { kind: "automation_events" });
  const sub = d.lastSubscribe();
  assert.equal(sub.kind, "SUBSCRIPTION_KIND_AUTOMATION_EVENTS");
  assert.equal(sub.filter, "automation.#");
  assert.equal(sub.concept, undefined);
  assert.equal(sub.actions, undefined);
});

test("subscribeGraph routes matching events to the handler", () => {
  const d = new MockDispatcher();
  const sm = new SubscriptionManager(d.asDispatcher());
  const got: Event[] = [];
  sm.subscribeGraph((ev) => got.push(ev), {
    concept: "v1:cognition:utterance",
    actions: ["created"],
  });
  const sub = d.lastSubscribe();
  d.emit({
    event: {
      subscriptionId: sub.subscriptionId,
      kind: "EVENT_KIND_NODE_CREATED",
      payload: { concept: "v1:cognition:utterance", id: "utt-1" },
    },
  } as ServerMessage);
  assert.equal(got.length, 1);
  assert.equal(got[0]?.kind, "NODE_CREATED");
  assert.equal(got[0]?.payload?.concept, "v1:cognition:utterance");
});

test("subscribeGraph unsubscribe stops delivery and sends an unsubscribe", () => {
  const d = new MockDispatcher();
  const sm = new SubscriptionManager(d.asDispatcher());
  const got: Event[] = [];
  const unsub = sm.subscribeGraph((ev) => got.push(ev), {
    concept: "v1:cognition:utterance",
  });
  const sub = d.lastSubscribe();
  unsub();
  assert.equal(d.lastUnsubscribe().subscriptionId, sub.subscriptionId);
  d.emit({
    event: {
      subscriptionId: sub.subscriptionId,
      kind: "EVENT_KIND_NODE_CREATED",
      payload: { concept: "v1:cognition:utterance", id: "utt-2" },
    },
  } as ServerMessage);
  assert.equal(got.length, 0, "no delivery after unsubscribe");
});

// payload_omitted -- the ID-ONLY notification (memql#4309).
//
// A `granted`-tier row cannot be decided at fan-out (its predicate is a
// relationship spec needing a join), so the engine sends the row's
// identity with the flag set and the client re-reads through the
// authorized read path. Silently dropping such an event instead would
// make a granted concept's live feed die without a trace.

test("an id-only event surfaces payloadOmitted and carries only the identity", () => {
  const d = new MockDispatcher();
  const sm = new SubscriptionManager(d.asDispatcher());
  const got: Event[] = [];
  sm.subscribeGraph((e) => got.push(e), { concept: "v1:cognition:utterance" });
  const subscriptionId = d.lastSubscribe().subscriptionId;

  d.emit({
    event: {
      subscriptionId,
      kind: "EVENT_KIND_NODE_CREATED",
      payload: { id: "abc", concept: "v1:cognition:utterance", topic: "graph.node.created" },
      payloadOmitted: true,
    },
  } as unknown as ServerMessage);

  assert.equal(got.length, 1);
  const idOnly = got[0]!;
  assert.equal(idOnly.payloadOmitted, true, "the flag did not reach the consumer");
  assert.equal(idOnly.payload?.id, "abc", "the identity a re-read needs was lost");
  assert.equal(idOnly.payload?.body, undefined, "an id-only event must not carry the row");
});

// protojson omits a false bool, so the wire field is ABSENT on every
// ordinary event. It must normalise to `false`, not `undefined` -- a
// consumer writing `if (e.payloadOmitted)` is fine either way, but one
// writing `e.payloadOmitted === false` is not, and the type says boolean.
test("an ordinary event reports payloadOmitted false, not undefined", () => {
  const d = new MockDispatcher();
  const sm = new SubscriptionManager(d.asDispatcher());
  const got: Event[] = [];
  sm.subscribeGraph((e) => got.push(e), {});
  const subscriptionId = d.lastSubscribe().subscriptionId;

  d.emit({
    event: {
      subscriptionId,
      kind: "EVENT_KIND_NODE_CREATED",
      payload: { id: "abc", body: "the whole row" },
    },
  } as unknown as ServerMessage);

  assert.equal(got.length, 1);
  const full = got[0]!;
  assert.equal(full.payloadOmitted, false);
  assert.equal(full.payload?.body, "the whole row", "a full-payload event was altered");
});


test("identical graph readers share one wire subscription across replay and independent release", () => {
  const d = new MockDispatcher();
  const manager = new SubscriptionManager(d.asDispatcher());
  const calls: [number, number] = [0, 0];
  const first = manager.subscribeGraph(() => { calls[0]++; throw new Error("consumer failure"); }, { concept: "v1:work:run", actions: ["updated", "created"] });
  const second = manager.subscribeGraph(() => { calls[1]++; }, { concept: "v1:work:run", actions: ["created", "updated", "created"] });
  assert.equal(manager.activeCount, 1);
  assert.equal(d.sent.length, 1);
  const id = d.lastSubscribe().subscriptionId;
  const event = { event: { subscriptionId: id, kind: "EVENT_KIND_NODE_UPDATED", payload: { id: "run" } } } as unknown as ServerMessage;
  d.emit(event);
  assert.deepEqual(calls, [1, 1], "one consumer's exception cannot suppress another");
  assert.equal(manager.replay(), 1);
  first(); first();
  assert.equal(manager.activeCount, 1);
  d.emit(event);
  assert.deepEqual(calls, [1, 2]);
  second();
  assert.equal(manager.activeCount, 0);
  assert.equal(d.lastUnsubscribe().subscriptionId, id);
});

test("graph sharing preserves concept, action and authorization-payload distinctions", () => {
  const d = new MockDispatcher();
  const manager = new SubscriptionManager(d.asDispatcher());
  for (const opts of [{ concept: "a" }, { concept: "b" }, { concept: "a", idOnly: true }, { concept: "a", actions: ["created" as const] }]) {
    manager.subscribeGraph(() => {}, opts);
  }
  assert.equal(manager.activeCount, 4);
  manager.stop();
  assert.equal(manager.activeCount, 0);
  assert.throws(() => manager.subscribeGraph(() => {}, { concept: "a" }), /stopped/);
});


test("the omitted graph action filter shares the explicit all-actions subscription", () => {
  const d = new MockDispatcher();
  const manager = new SubscriptionManager(d.asDispatcher());
  const implicit = manager.subscribeGraph(() => {}, { concept: "v1:work:run" });
  const explicit = manager.subscribeGraph(() => {}, { concept: "v1:work:run", actions: ["created", "updated", "deleted"] });
  assert.equal(manager.activeCount, 1);
  assert.equal(d.sent.length, 1);
  implicit();
  assert.equal(manager.activeCount, 1);
  explicit();
  assert.equal(manager.activeCount, 0);
});

test("graph readers registered while offline survive replay and release cleanly", () => {
  const d = new MockDispatcher();
  const manager = new SubscriptionManager(d.asDispatcher());
  const send = d.send.bind(d);
  d.send = () => { throw new Error("socket not open (readyState=3)"); };
  let changes = 0;
  const first = manager.subscribeGraph(() => { changes++; }, { concept: "v1:work:run" });
  const second = manager.subscribeGraph(() => { changes++; }, { concept: "v1:work:run" });
  assert.equal(manager.activeCount, 1, "offline consumers still share one retained registration");
  d.send = send;
  assert.equal(manager.replay(), 1);
  const id = d.lastSubscribe().subscriptionId;
  d.emit({ event: { subscriptionId: id, kind: "EVENT_KIND_NODE_UPDATED", payload: { id: "run" } } } as unknown as ServerMessage);
  assert.equal(changes, 2, "recovery restores every consumer, not an empty fanout");
  first(); second();
  assert.equal(manager.activeCount, 0);
  assert.equal(d.lastUnsubscribe().subscriptionId, id);
});
