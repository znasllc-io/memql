// SubscriptionManager wraps the subscribe / event-fanout protocol.
// subscribe(pattern, handler) returns an unsubscribe function -- the
// shape the README spec promises and that the acceptance criteria on
// memql#116 explicitly call out.
//
// Mirrors sdk/go/client/subscriptions.go, except the handler-callback
// shape is more idiomatic for browsers than the channel-per-sub model
// the Go SDK uses.
//
// Graph subscriptions are STRUCTURED (memql#2460): the client sends a
// concept type id + CDC action verbs via subscribeGraph(), and the SERVER
// composes the `graph.node.<action>.<concept>` bus topic. The topic
// grammar never appears on the client wire, so a future grammar change is
// no longer a wire change. The legacy free-text subscribe(pattern) path
// survives only for the NON-graph subscription kinds; the server rejects a
// free-text filter for graph_events.

import type { Dispatcher, Unregister } from "./dispatcher.js";
import { newShortId } from "./id.js";
import {
  eventFromWire,
  graphActionWire,
  subscriptionKindWire,
  type Event,
  type GraphAction,
  type SubscriptionKind,
} from "./types.js";
import { readServerPayload } from "./wire.js";

export type EventHandler = (event: Event) => void;

export interface SubscribeOptions {
  // The NON-graph subscription kind (telemetry / automation_events / ...).
  // Graph subscriptions must use subscribeGraph(); passing "graph_events"
  // here throws, because the free-text `pattern` is not a valid graph
  // subscribe surface anymore (memql#2460).
  kind?: SubscriptionKind;
}

// GraphSubscribeOptions selects which graph CDC events a structured graph
// subscription receives. Both fields are optional:
//   - concept: canonical concept TYPE id (e.g. "v1:cognition:utterance").
//     Omit for all concepts. Import the id from the generated SDK
//     (`Concepts.COGNITION_UTTERANCE`) -- never hand-write a topic string.
//   - actions: the CDC verbs to receive. Omit / empty for all actions.
export interface GraphSubscribeOptions {
  concept?: string;
  actions?: GraphAction[];
}

export class SubscriptionManager {
  private readonly dispatcher: Dispatcher;
  private readonly subs = new Map<string, EventHandler>();
  private readonly eventUnregister: Unregister;
  private stopped = false;

  constructor(dispatcher: Dispatcher) {
    this.dispatcher = dispatcher;
    this.eventUnregister = dispatcher.addEventListener((msg) => {
      const payload = readServerPayload(msg);
      if (payload?.kind !== "event") return;
      const handler = this.subs.get(payload.value.subscriptionId);
      if (!handler) return;
      try {
        handler(eventFromWire(payload.value));
      } catch {
        // Consumer handlers are isolated -- a thrown error is the
        // consumer's bug, not ours. Drop silently to avoid breaking
        // unrelated subscriptions.
      }
    });
  }

  // subscribeGraph opens a STRUCTURED graph subscription and returns an
  // unsubscribe function. The server composes the bus topic from
  // opts.concept + opts.actions (memql#2460), so the client never writes a
  // `graph.node.<action>.<concept>` string. This is the graph counterpart
  // of subscribe(); it is what every reactive UI on memQL uses.
  subscribeGraph(handler: EventHandler, opts: GraphSubscribeOptions = {}): Unregister {
    const actions = (opts.actions ?? []).map(graphActionWire);
    return this.register(handler, {
      kind: subscriptionKindWire("graph_events"),
      concept: opts.concept,
      actions: actions.length > 0 ? actions : undefined,
    });
  }

  // subscribe registers a handler for events matching a free-text bus
  // `pattern` and returns an unsubscribe function. It is retained for the
  // NON-graph subscription kinds (telemetry / automation_events / ...);
  // opts.kind is required and must NOT be "graph_events" -- graph
  // subscriptions go through subscribeGraph() (memql#2460).
  subscribe(pattern: string, handler: EventHandler, opts: SubscribeOptions = {}): Unregister {
    const kind = opts.kind ?? "graph_events";
    if (kind === "graph_events") {
      throw new Error(
        "graph subscriptions are structured; use subscribeGraph({ concept, actions }) instead of subscribe(pattern) (memql#2460)",
      );
    }
    return this.register(handler, {
      kind: subscriptionKindWire(kind),
      filter: pattern,
    });
  }

  // register stamps a fresh subscription id, wires the handler, and sends
  // the SubscribeMsg envelope. Shared by subscribe + subscribeGraph.
  private register(
    handler: EventHandler,
    fields: {
      kind: ReturnType<typeof subscriptionKindWire>;
      filter?: string;
      concept?: string;
      actions?: ReturnType<typeof graphActionWire>[];
    },
  ): Unregister {
    if (this.stopped) throw new Error("subscription manager stopped");
    const subscriptionId = newShortId();
    this.subs.set(subscriptionId, handler);
    this.dispatcher.send({
      subscribe: { subscriptionId, ...fields },
    });
    let unsubscribed = false;
    return () => {
      if (unsubscribed) return;
      unsubscribed = true;
      this.subs.delete(subscriptionId);
      try {
        this.dispatcher.send({ unsubscribe: { subscriptionId } });
      } catch {
        // Already torn down on the wire; nothing to do.
      }
    };
  }

  stop(): void {
    if (this.stopped) return;
    this.stopped = true;
    this.eventUnregister();
    this.subs.clear();
  }
}
