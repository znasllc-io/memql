import { startAskVoice, type AskVoiceOptions } from "@znasllc-io/memql-sdk-core/voice";
import { aiChatStream, type AiChatOptions, type AiChatMessage } from "@znasllc-io/memql-sdk-core/ai";
import { QueryClient, type Dispatcher } from "@znasllc-io/memql-sdk-core/client";
import { flatten } from "../kit/rows";
import type { AskCallbacks, AskHandle, AskTransport } from "./askController";
import type { AskActivity, AskConversationStore, AskTurn, ConversationSummary } from "./conversationSession";

export type AskStreamFn = (dispatcher: Dispatcher, messages: AiChatMessage[], opts: AiChatOptions) => { deltas: AsyncIterable<{ textDelta?: string; metadata?: Record<string, unknown> }>; result: Promise<unknown> };

export class SdkAskTransport implements AskTransport {
  readonly conversations: AskConversationStore;
  constructor(private readonly dispatcher: () => Dispatcher | null, private readonly stream: AskStreamFn = aiChatStream) {
    const query = () => { const dispatcher = this.dispatcher(); if (!dispatcher) throw new Error("Not connected to the cluster."); return new QueryClient(dispatcher); };
    this.conversations = {
      list: async () => {
        const items: ConversationSummary[] = [];
        const seen = new Set<string>(); let cursor = "";
        do {
          const result = await query().myAskConversations({}, cursor ? { cursor } : {});
          items.push(...result.rows().map(row => flatten(row) as unknown as ConversationSummary));
          cursor = result.meta()?.cursor ?? "";
          if (cursor && seen.has(cursor)) throw new Error("Conversation history could not advance. Try again.");
          seen.add(cursor);
        } while (cursor);
        return items;
      },
      create: async () => {
        const result = await query().createAskConversation({ title: "New conversation" });
        const row = result.rows()[0];
        if (!row) throw new Error("The conversation could not be created.");
        return flatten(row) as unknown as ConversationSummary;
      },
      read: async conversationId => {
        const result = await query().askConversationById({ conversationId });
        const row = result.rows()[0];
        if (!row) throw new Error("This conversation is unavailable.");
        const transcript = flatten(row).transcript as { turns?: AskTurn[] } | undefined;
        return transcript?.turns ?? [];
      },
    };
  }

  startVoice = (options: AskVoiceOptions, signal: AbortSignal) => {
    const dispatcher = this.dispatcher();
    if (!dispatcher) return Promise.reject(new Error("Not connected to the cluster."));
    return startAskVoice(dispatcher, options, signal);
  };

  ask(prompt: string, context: string | null, on: AskCallbacks, options?: { conversationId: string; turnId: string }): AskHandle {
    const dispatcher = this.dispatcher();
    const abort = new AbortController();
    if (!dispatcher || !options) {
      queueMicrotask(() => on.error(!dispatcher ? "Not connected to the cluster." : "Start a conversation before sending a message."));
      return { cancel: () => abort.abort() };
    }
    void (async () => {
      try {
        const handle = this.stream(dispatcher, [{ role: "user", content: prompt }], { signal: abort.signal, conversationId: options.conversationId, requestId: options.turnId, pageContext: context ?? "" });
        let settled = false;
        let answer = "";
        const result = handle.result.then(value => { settled = true; return value; }, error => { settled = true; throw error; });
        const consume = async () => {
          for await (const chunk of handle.deltas) {
            if (abort.signal.aborted) return;
            if (chunk.textDelta) { answer += chunk.textDelta; on.delta(chunk.textDelta); }
            if (chunk.metadata?.ask) on.activity?.(chunk.metadata.ask as AskActivity);
          }
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
        if (!answer.trim()) throw new Error("MemQL finished without an answer.");
        on.done();
      } catch (error) {
        if (abort.signal.aborted) return;
        abort.abort();
        on.error(error instanceof Error ? error.message : String(error));
      }
    })();
    return { cancel: () => abort.abort() };
  }
}
