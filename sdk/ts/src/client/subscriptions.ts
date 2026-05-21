// SubscriptionManager wraps the subscribe / event-fanout protocol.
// subscribe(pattern, handler) returns an unsubscribe function -- the
// shape the README spec promises and that the acceptance criteria on
// memql#116 explicitly call out.
//
// Mirrors sdk/go/client/subscriptions.go, except the handler-callback
// shape is more idiomatic for browsers than the channel-per-sub model
// the Go SDK uses.

import type { Dispatcher, Unregister } from "./dispatcher.js";
import { newShortId } from "./id.js";
import { eventFromWire, subscriptionKindWire, type Event, type SubscriptionKind } from "./types.js";
import { readServerPayload } from "./wire.js";

export type EventHandler = (event: Event) => void;

export interface SubscribeOptions {
  // Defaults to "graph_events" -- the catch-all for graph
  // node.created / .updated / .deleted streams that drives every
  // reactive UI on memQL. Consumers can specify a narrower kind
  // (telemetry, automation_events, etc.) when they only want one
  // family.
  kind?: SubscriptionKind;
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

  // subscribe registers a handler for events matching `pattern` and
  // returns an unsubscribe function. Pattern grammar is the
  // server-side string (e.g. `graph.node.created.*.v1:cognition:utterance`);
  // the SDK passes it through verbatim as the filter field on the
  // SubscribeMsg envelope.
  subscribe(pattern: string, handler: EventHandler, opts: SubscribeOptions = {}): Unregister {
    if (this.stopped) throw new Error("subscription manager stopped");
    const subscriptionId = newShortId();
    const kind = subscriptionKindWire(opts.kind ?? "graph_events");
    this.subs.set(subscriptionId, handler);
    this.dispatcher.send({
      subscribe: { subscriptionId, kind, filter: pattern },
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
