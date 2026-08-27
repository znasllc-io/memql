// Ask's real transport (spec D6): sdk-core ai chat streaming over the
// shell's one connection, behind the same interface the PR A stub filled.
// The context tag rides as a labelled system line, so an app-scoped
// question carries its scope without the surface changing shape.

import { aiChatStream, type AiChatMessage } from "@znasllc-io/memql-sdk-core/ai";
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
    const messages: AiChatMessage[] = [
      ...(context ? [{ role: "system", content: `Context: ${context}` }] : []),
      { role: "user", content: prompt },
    ];

    void (async () => {
      try {
        const handle = this.stream(dispatcher, messages, { signal: abort.signal });
        for await (const delta of handle.deltas) {
          if (abort.signal.aborted) return;
          if (delta.textDelta) on.delta(delta.textDelta);
        }
        await handle.result;
        if (!abort.signal.aborted) on.done();
      } catch (err) {
        if (abort.signal.aborted) return;
        on.error(err instanceof Error ? err.message : "The cluster did not answer.");
      }
    })();

    return { cancel: () => abort.abort() };
  }
}
