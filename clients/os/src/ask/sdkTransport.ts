// Ask's real transport (spec D6): sdk-core ai chat streaming over the
// shell's one connection, behind the same interface the PR A stub filled.
// The context tag rides as a labelled system line, so an app-scoped
// question carries its scope without the surface changing shape.

import { aiChatStream, type AiChatMessage } from "@znasllc-io/memql-sdk-core/ai";
import { QueryClient } from "@znasllc-io/memql-sdk-core/client";
import { flatten } from "../kit/rows";
import type { PolicyDraft } from "../apps/fleet/PolicyEditor";
import type { Dispatcher } from "@znasllc-io/memql-sdk-core/client";

import type { AskCallbacks, AskHandle, AskTransport } from "./askController";

export type AskStreamFn = (
  dispatcher: Dispatcher,
  messages: AiChatMessage[],
  opts: { signal?: AbortSignal },
) => { deltas: AsyncIterable<{ textDelta?: string }>; result: Promise<unknown> };

export class SdkAskTransport implements AskTransport {
  constructor(
    private readonly dispatcher: () => Dispatcher | null,
    private readonly stream: AskStreamFn = aiChatStream,
  ) {}

  ask(prompt: string, context: string | null, on: AskCallbacks): AskHandle {
    const dispatcher = this.dispatcher();
    if (!dispatcher) {
      // Honest, in-surface, retryable -- never a toast (spec C).
      queueMicrotask(() => on.error("Not connected to the cluster yet."));
      return { cancel: () => {} };
    }

    const abort = new AbortController();
    if (isPolicyAuthoringRequest(prompt, context)) {
      void new QueryClient(dispatcher).routingPolicyDescribe({ sentence: prompt }, { signal: abort.signal }).then(result => {
        if (abort.signal.aborted) return;
        const first = [...result.rows()][0];
        if (!first) throw new Error("The cluster returned no policy proposal.");
        const row = flatten(first as Record<string, unknown>);
        if (typeof row.name !== "string" || typeof row.revision !== "number" || (row.action !== "save" && row.action !== "reset")) throw new Error("The cluster returned an incomplete policy proposal.");
        const proposal: PolicyDraft = { name: row.name, action: row.action, existing: row.existing === true, revision: row.revision, description: typeof row.description === "string" ? row.description : "", primary: typeof row.primary === "string" ? row.primary : "", fallbacks: Array.isArray(row.fallbacks) ? row.fallbacks as string[] : [], explanation: typeof row.explanation === "string" ? row.explanation : "" };
        on.policyProposal?.(proposal);
        on.delta("Review the proposed policy below. Nothing has been saved yet.");
        on.done();
      }).catch(error => { if (!abort.signal.aborted) on.error(error instanceof Error ? error.message : String(error)); });
      return { cancel: () => abort.abort() };
    }

    const messages: AiChatMessage[] = [
      ...(context ? [{ role: "system", content: `Context: ${context}` }] : []),
      { role: "user", content: prompt },
    ];

    void (async () => {
      try {
        const handle = this.stream(dispatcher, messages, { signal: abort.signal });
        let settled = false;
        let answer = "";
        // Observe failure concurrently: a failed result must not wait behind
        // an iterator whose peer has gone away.
        const result = handle.result.then(
          (value) => { settled = true; return value; },
          (error: unknown) => { settled = true; throw error; },
        );
        const consume = async () => {
          for await (const delta of handle.deltas) {
            if (abort.signal.aborted) return;
            if (delta.textDelta) { answer += delta.textDelta; on.delta(delta.textDelta); }
          }
          // sdk-core closes deltas only with a terminal result/error. An
          // earlier end is a broken stream, not a slow cold model.
          if (!abort.signal.aborted && !settled) throw new Error("The reply stream ended before completion. Try again.");
        };
        const [, terminal] = await Promise.all([consume(), result]);
        if (abort.signal.aborted) return;
        if (!answer.trim() && terminal && typeof terminal === "object" && "message" in terminal) {
          const message = terminal.message;
          if (message && typeof message === "object" && "content" in message && typeof message.content === "string") {
            answer = message.content;
            if (answer) on.delta(answer);
          }
        }
        if (!answer.trim()) throw new Error("The cluster finished without an answer. Try again.");
        on.done();
      } catch (err) {
        if (abort.signal.aborted) return;
        abort.abort();
        on.error(err instanceof Error ? err.message : "The cluster did not answer.");
      }
    })();

    return { cancel: () => abort.abort() };
  }
}

/** Explicit composition context accepts any wording. Outside it, only an
 * explicit policy-authoring request enters the draft-only compiler. */
export function isPolicyAuthoringRequest(prompt: string, context: string | null): boolean {
  return context?.includes("action:routing-policy") === true || (/\bpolic(?:y|ies)\b/i.test(prompt) && /\b(create|define|edit|change|customize|restore|reset|make|configure)\b/i.test(prompt));
}
