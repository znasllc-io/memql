// siSuggest wraps MemqlClientMessage.si_suggest. The server-side
// domain switch (spaces / spaceTitle / agents / groups /
// groupDescription / *CardSummary / knowledge) reads its required
// fields out of the structured payload; this surface stays domain-
// agnostic and returns the structured result verbatim.

import type { Dispatcher } from "../client/dispatcher.js";
import { newShortId } from "../client/id.js";
import { readServerPayload } from "../client/wire.js";

export interface SiSuggestOptions {
  signal?: AbortSignal;
}

export interface SiSuggestResult {
  domain: string;
  result: Record<string, unknown>;
}

// siSuggest calls the SI suggest handler for the given domain with
// the structured payload. Throws on QueryError, transport failure,
// or abort.
export async function siSuggest(
  dispatcher: Dispatcher,
  domain: string,
  payload: Record<string, unknown>,
  opts: SiSuggestOptions = {},
): Promise<SiSuggestResult> {
  if (!dispatcher) throw new Error("siSuggest: dispatcher is required");
  if (typeof domain !== "string" || domain.length === 0) {
    throw new Error("siSuggest: domain string must be non-empty");
  }

  const requestId = newShortId();
  const reply = await dispatcher.sendAndWait(
    {
      siSuggest: {
        requestId,
        domain,
        payload: payload ?? {},
      },
    },
    opts.signal,
  );

  const replyPayload = readServerPayload(reply);
  if (replyPayload?.kind === "queryError") {
    throw new Error(`siSuggest: ${replyPayload.value.error?.message ?? "(no message)"}`);
  }
  if (replyPayload?.kind !== "siSuggestResult") {
    throw new Error("siSuggest: unexpected reply envelope");
  }
  return {
    domain: replyPayload.value.domain ?? domain,
    result: replyPayload.value.result ?? {},
  };
}
