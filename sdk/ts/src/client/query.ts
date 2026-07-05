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
  conceptsFromWire,
  Result,
  resultFromQueryPayload,
  type AccessSummary,
  type Concept,
} from "./types.js";
import { newShortId } from "./id.js";
import { readServerPayload } from "./wire.js";

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
