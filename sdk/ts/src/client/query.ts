// QueryClient is the typed entry point for query / mutation / logic
// dispatch. Consumers go through the generated typed methods on this
// class (see generated_*.ts) -- raw DSL strings are forbidden by the
// named-primitive contract (sdk/go/CLAUDE.md rule #1).
//
// listConcepts and getMyAccess are hand-rolled escapes for surfaces
// that have no DSL counterpart (admin / settings panes). Anything
// else lives in the DSL and reaches the SDK via sdk-gen.

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
        executeQuery: { requestId: reqId, query: call },
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
        executeQuery: { requestId: reqId, query: call },
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
