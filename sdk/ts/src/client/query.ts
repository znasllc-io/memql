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

// The frame the server puts around an engine error it classifies as internal
// (buildEngineErrorMessage, component/grpc/server.go), which is most refusals a
// builtin or a mutation words for a person.
const ENGINE_FRAME = "MemQL engine failed to execute query.";
const ENGINE_DETAILS = /^Details:\s*/;
// A call's name as executeNamed puts it in front of a refusal: an identifier, a
// colon and a space.
const CALL_NAME = /^[A-Za-z_][A-Za-z0-9_]*: /;
// A Go package's name in front of its error ("compose: ..."): one all-lowercase
// word and a colon, which a sentence written for a person never opens with.
const PACKAGE_NAME = /^[a-z][a-z0-9]*: /;

/**
 * The sentence a person reads out of a refused call.
 *
 * A refusal reaches a client in up to three frames, none of which is the
 * refusal: executeNamed puts the call's name in front (`composeMaterialize: `),
 * the server wraps an internal engine error ("MemQL engine failed to execute
 * query. Details: "), and a Go error may still open with its package's name
 * (`compose: `). Shown as it came, a person reads two technical prefixes before
 * the one thing that tells them what to do. This takes all three off, once each
 * and only from the front, and leaves the rest as the cluster wrote it.
 *
 * Pass `construct`, the name the call was made with, where it is known: then
 * exactly that name comes off. Without it a name comes off only in front of the
 * engine's frame, where it can only be the call's. A frame with nothing inside
 * it is kept, because it is then the whole of what the cluster said.
 */
export function refusalSentence(error: unknown, construct?: string): string {
  let text = (error instanceof Error ? error.message : String(error)).trim();
  if (construct && text === `${construct}:`) return "";
  const named = construct ? `${construct}: ` : "";
  if (named && text.startsWith(named)) {
    text = text.slice(named.length);
  } else {
    const call = CALL_NAME.exec(text);
    if (call && text.slice(call[0].length).startsWith(ENGINE_FRAME)) text = text.slice(call[0].length);
  }
  if (text.startsWith(ENGINE_FRAME)) {
    const inside = text.slice(ENGINE_FRAME.length).trim().replace(ENGINE_DETAILS, "");
    if (inside !== "") text = inside;
  }
  return text.replace(PACKAGE_NAME, "");
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
  // Unlike the catalog read this is not request/response -- the engine keeps
  // pushing frames on this request id until the subscription ends -- so it
  // returns a handle rather than a Promise. Pass opts.onError to be told when
  // the engine refuses the follow.
  subscribeConceptRegistry(
    onDelta: (delta: ConceptRegistryDelta) => void,
    opts: { signal?: AbortSignal; onError?: (err: Error) => void } = {},
  ): ConceptRegistryFollow {
    const reqId = newShortId();
    let subscriptionId = "";
    let closed = false;

    // registerStream, not addEventListener: a follow subscription is a
    // MULTI-FRAME exchange keyed by request id (snapshot, then one frame per
    // registry change), which is the tier the dispatcher routes those to --
    // see streamRequestId in wire.ts and the routed/unrouted ledger in
    // sdk/go/client/dispatcher_stream_routing_test.go. Registered BEFORE the
    // send so the snapshot cannot arrive before there is a listener for it.
    const remove = this.dispatcher.registerStream(reqId, (msg) => {
      const payload = readServerPayload(msg);
      // The engine refuses a follow on a node with no engine, and that
      // queryError carries this request id -- so it lands here rather than
      // vanishing. Surfacing it is what keeps a caller from waiting forever on
      // a snapshot that is never coming.
      if (payload?.kind === "queryError") {
        opts.onError?.(
          new Error(
            `subscribeConceptRegistry: ${payload.value.error?.message ?? "(no message)"}`,
          ),
        );
        return;
      }
      if (payload?.kind !== "conceptsRegistryDelta") return;
      const v = payload.value;
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
