// QueryClient is the base dispatch surface for query / mutation /
// logic calls. The per-product SDK package
// generates typed methods from its DSL and layers them onto this
// class via `declare module` + prototype augmentation; consumers call
// those typed methods, never raw DSL strings (the named-primitive
// contract, sdk/go/CLAUDE.md rule #1). executeNamed is the entry
// point every generated method dispatches through.
//
// listConcepts and getMyAccess are hand-rolled escapes for surfaces
// that have no DSL counterpart (admin / settings panes).

import type { Dispatcher } from "./dispatcher.js";
import {
  accessSummaryFromWire,
  conceptRegistryDeltaFromWire,
  conceptsFromWire,
  Result,
  resultFromQueryPayload,
  type AccessSummary,
  type Concept,
  type ConceptRegistryDelta,
  type DomainSubscription,
} from "./types.js";
import { newShortId } from "./id.js";
import { readServerPayload } from "./wire.js";

// ConceptRegistryFollow is the handle returned by subscribeConceptRegistry: call
// unsubscribe to stop delivery (removes the local listener and, if the snapshot
// arrived, sends an UnsubscribeMsg for its subscription id). Closing the
// connection also stops delivery, so a caller that tears the connection down
// need not call this.
export interface ConceptRegistryFollow {
  unsubscribe: () => void;
}

export interface QueryCallOptions {
  signal?: AbortSignal;
  /**
   * Opaque keyset continuation cursor (memql#1985 / 5.12). When set, the
   * engine continues the query from the encoded position instead of an
   * offset scan. Obtain it from a prior response's `Result.meta()?.cursor`
   * (minted on a full page; empty when the set is exhausted) and pass it
   * back to walk the set with no dup / no gap. The cursor is opaque +
   * bound to the query's `sort` ordering, and carries no server session
   * state, so the SAME cursor resolves on ANY replica it is replayed
   * against. Empty / unset is a first page (the existing behaviour).
   *
   * Rides `ExecuteQueryMsg.cursor` (proto field 5); the read-back side is
   * already exposed via `Result.meta()?.cursor`.
   */
  cursor?: string;
}

export class QueryClient {
  constructor(private readonly dispatcher: Dispatcher) {}

  // executeRaw runs a MemQL query string. UNEXPORTED-equivalent: the
  // surface is package-internal -- consumers must go through the
  // generated typed methods. The TS surface markers are weaker than
  // Go's lowercase convention, so we lean on the named-primitive
  // contract being explicit in the readme + reviewed at PR time.
  async executeRaw(call: string, opts: QueryCallOptions = {}): Promise<unknown> {
    const reqId = newShortId();
    const resp = await this.dispatcher.sendAndWait(
      {
        executeQuery: {
          requestId: reqId,
          query: call,
          ...(opts.cursor ? { cursor: opts.cursor } : {}),
        },
      },
      opts.signal,
    );
    const payload = readServerPayload(resp);
    if (payload?.kind === "queryError") {
      throw new Error(
        `query error: ${payload.value.error?.message ?? "(no message)"}`,
      );
    }
    if (payload?.kind === "queryResult") {
      return payload.value.result ?? null;
    }
    return null;
  }

  // executeNamed is the entry point every generated typed method
  // dispatches through. Returns a Result wrapper around the engine
  // reply.
  async executeNamed(name: string, call: string, opts: QueryCallOptions = {}): Promise<Result> {
    const reqId = newShortId();
    const resp = await this.dispatcher.sendAndWait(
      {
        executeQuery: {
          requestId: reqId,
          query: call,
          ...(opts.cursor ? { cursor: opts.cursor } : {}),
        },
      },
      opts.signal,
    );
    const payload = readServerPayload(resp);
    if (payload?.kind === "queryError") {
      throw new Error(`${name}: ${payload.value.error?.message ?? "(no message)"}`);
    }
    if (payload?.kind === "queryResult") {
      return resultFromQueryPayload(payload.value);
    }
    return resultFromQueryPayload(null);
  }

  async listConcepts(opts: QueryCallOptions = {}): Promise<Concept[]> {
    const resp = await this.dispatcher.sendAndWait(
      { conceptsList: {} },
      opts.signal,
    );
    const payload = readServerPayload(resp);
    if (payload?.kind === "conceptsListResult") {
      return conceptsFromWire(payload.value.concepts);
    }
    return [];
  }

  // subscriptionCatalog asks the engine which CDC filters exist, grouped by
  // domain (ConceptsSubscribeMsg). This is a one-shot CATALOG read -- the
  // engine's reply model has no registry-delta stream (memql#4233) -- so a
  // client discovers per-domain filters here and subscribes to row-level CDC
  // with SubscriptionManager; it does NOT get told when the registry itself
  // changes.
  async subscriptionCatalog(
    domains: string[] = [],
    opts: QueryCallOptions = {},
  ): Promise<DomainSubscription[]> {
    const resp = await this.dispatcher.sendAndWait(
      { conceptsSubscribe: domains.length > 0 ? { domains } : {} },
      opts.signal,
    );
    const payload = readServerPayload(resp);
    if (payload?.kind === "queryError") {
      throw new Error(
        `subscriptionCatalog: ${payload.value.error?.message ?? "(no message)"}`,
      );
    }
    if (payload?.kind !== "conceptsSubscribeResult") {
      return [];
    }
    const out: DomainSubscription[] = [];
    for (const d of payload.value.domains ?? []) {
      if (typeof d.domain !== "string" || d.domain === "") continue;
      out.push({
        domain: d.domain,
        filters: (d.filters ?? []).filter((f): f is string => typeof f === "string"),
      });
    }
    return out;
  }

  // subscribeConceptRegistry opens a follow-mode concept subscription
  // (memql#4238): the engine sends a snapshot (reset=true, the whole registry)
  // and then live add/remove deltas, so a client's concept registry stays
  // current without a reconnect. onDelta is called for EVERY delta, snapshot
  // included -- apply a reset by replacing the registry, and an incremental by
  // upserting `added` (by id) and dropping `removed`. Track `generation`: a gap
  // means a delta was missed (a slow consumer dropped one), so unsubscribe and
  // re-subscribe to re-snapshot.
  //
  // Unlike the catalog read this is not request/response -- deltas arrive on the
  // shared event fanout, matched to this subscription by the request id -- so it
  // returns a handle rather than a Promise.
  subscribeConceptRegistry(
    onDelta: (delta: ConceptRegistryDelta) => void,
    opts: { signal?: AbortSignal } = {},
  ): ConceptRegistryFollow {
    const reqId = newShortId();
    let subscriptionId = "";
    let closed = false;

    const remove = this.dispatcher.addEventListener((msg) => {
      const payload = readServerPayload(msg);
      if (payload?.kind !== "conceptsRegistryDelta") return;
      const v = payload.value;
      if (v.requestId !== reqId) return;
      if (typeof v.subscriptionId === "string" && v.subscriptionId !== "") {
        subscriptionId = v.subscriptionId;
      }
      onDelta(conceptRegistryDeltaFromWire(v));
    });

    this.dispatcher.send({ conceptsSubscribe: { requestId: reqId, follow: true } });

    const unsubscribe = (): void => {
      if (closed) return;
      closed = true;
      remove();
      // Only after the snapshot did the server tell us the subscription id; if
      // we tear down before it arrives, closing the connection is what stops
      // the (not-yet-started) delivery.
      if (subscriptionId !== "") {
        this.dispatcher.send({ unsubscribe: { subscriptionId } });
      }
    };

    if (opts.signal) {
      if (opts.signal.aborted) unsubscribe();
      else opts.signal.addEventListener("abort", unsubscribe, { once: true });
    }

    return { unsubscribe };
  }

  async getMyAccess(opts: QueryCallOptions = {}): Promise<AccessSummary | null> {
    const resp = await this.dispatcher.sendAndWait(
      { myAccess: {} },
      opts.signal,
    );
    const payload = readServerPayload(resp);
    if (payload?.kind === "queryError") {
      throw new Error(`my access: ${payload.value.error?.message ?? "(no message)"}`);
    }
    if (payload?.kind === "myAccessResult") {
      return accessSummaryFromWire(payload.value);
    }
    return null;
  }
}
