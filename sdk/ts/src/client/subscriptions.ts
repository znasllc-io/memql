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

// SubscribeFields is the wire shape of one registered subscription, RETAINED
// so it can be replayed on a fresh stream after a reconnect (memql#4537).
//
// Retaining the spec -- not just the handler -- is the whole change: before
// this, resubscription after a redial worked only because a consumer happened
// to re-run the code that subscribed (in the portal, a React effect keyed on
// the new manager's identity). That is emergent, not contractual, and a
// consumer without that accident simply went deaf.
interface SubscribeFields {
  kind: ReturnType<typeof subscriptionKindWire>;
  filter?: string;
  concept?: string;
  actions?: ReturnType<typeof graphActionWire>[];
}

interface SubscriptionRecord {
  handler: EventHandler;
  fields: SubscribeFields;
}

export class SubscriptionManager {
  private readonly dispatcher: Dispatcher;
  private readonly subs = new Map<string, SubscriptionRecord>();
  private readonly deliveryObservers = new Set<EventHandler>();
  private readonly eventUnregister: Unregister;
  private stopped = false;

  constructor(dispatcher: Dispatcher) {
    this.dispatcher = dispatcher;
    this.eventUnregister = dispatcher.addEventListener((msg) => {
      const payload = readServerPayload(msg);
      if (payload?.kind !== "event") return;
      const event = eventFromWire(payload.value);
      // Stream-wide observers FIRST, and unconditionally -- see onDelivery.
      for (const observe of [...this.deliveryObservers]) {
        try {
          observe(event);
        } catch {
          // Isolated for the same reason consumer handlers are.
        }
      }
      const record = this.subs.get(event.subscriptionId);
      if (!record) return;
      try {
        record.handler(event);
      } catch {
        // Consumer handlers are isolated -- a thrown error is the
        // consumer's bug, not ours. Drop silently to avoid breaking
        // unrelated subscriptions.
      }
    });
  }

  // onDelivery observes EVERY notification on the stream, matched to a
  // subscription or not, acks included (memql#4538).
  //
  // It exists because CONTINUITY IS A PROPERTY OF THE STREAM, not of one
  // subscription. `seq` numbers every notification the server writes on the
  // socket and `gap_before` lands on whichever notification happens to be
  // delivered first after a drop -- so a per-subscription reader sees a
  // sequence full of holes that are not its own, and a consumer whose events
  // were the ones dropped may never see the flag at all. Both mistakes point
  // the same way: they make a correct client either re-seed constantly or not
  // at all.
  //
  // One observer per stream, watching every delivery, is the only vantage
  // point from which those two fields say what they mean. LiveStore is that
  // observer; a consumer folding rows by hand wants this too.
  onDelivery(handler: EventHandler): Unregister {
    this.deliveryObservers.add(handler);
    return () => this.deliveryObservers.delete(handler);
  }

  // replay re-sends every live subscription on a freshly redialed stream
  // (memql#4537). Called by the Connection after a successful reconnect,
  // BEFORE consumers are told the cycle happened -- so a consumer that
  // re-seeds on the cycle notification is already subscribed when its read
  // goes out, which is the subscribe-then-read ordering the whole contract
  // rests on (memql#4536, design D2).
  //
  // Subscription IDS ARE REUSED. They are client-minted and scoped to a
  // stream, so the new server session has never seen them, and reusing them
  // keeps every handler registration valid -- minting fresh ids would orphan
  // the handler map and silently deliver nothing.
  //
  // Returns how many were replayed, which is what a test asserts on.
  replay(): number {
    if (this.stopped) return 0;
    let sent = 0;
    for (const [subscriptionId, record] of this.subs) {
      try {
        this.dispatcher.send({ subscribe: { subscriptionId, ...record.fields } });
        sent++;
      } catch {
        // The fresh socket died between redial and replay. The reconnect
        // loop is already watching for that; another attempt replays this
        // same map.
      }
    }
    return sent;
  }

  // activeCount is the number of live subscriptions. Exposed for consumers
  // (and tests) that reason about what a reconnect will replay.
  get activeCount(): number {
    return this.subs.size;
  }

  // subscribeGraph opens a STRUCTURED graph subscription and returns an
  // unsubscribe function. The server composes the bus topic from
  // opts.concept + opts.actions (memql#2460), so the client never writes a
  // `graph.node.<action>.<concept>` string. This is the graph counterpart
  // of subscribe(); it is what every reactive UI on MemQL uses.
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
  private register(handler: EventHandler, fields: SubscribeFields): Unregister {
    if (this.stopped) throw new Error("subscription manager stopped");
    const subscriptionId = newShortId();
    this.subs.set(subscriptionId, { handler, fields });
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
    this.deliveryObservers.clear();
  }
}
