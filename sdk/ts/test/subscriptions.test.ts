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
