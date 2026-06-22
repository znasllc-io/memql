// siChat / siChatStream wrap MemqlClientMessage.si_chat. The
// non-streaming path is a single correlateTo round-trip resolving
// to AiChatResult. The streaming path opens a registerStream
// session keyed by requestId so the interleaved AiStreamChunk
// frames flow to the caller as deltas before the terminal
// AiChatResult lands.

import type { Dispatcher } from "../client/dispatcher.js";
import { newShortId } from "../client/id.js";
import {
  readServerPayload,
  type ServerMessage,
  type AiChatMessageWire,
  type AiStreamChunkPayload,
} from "../client/wire.js";

export interface SiChatMessage {
  role: string;
  content: string;
  name?: string;
}

export interface SiChatResult {
  message: SiChatMessage;
  provider: string;
}

export interface SiChatOptions {
  provider?: string;
  signal?: AbortSignal;
}

export interface SiChatStreamDelta {
  index: number;
  done: boolean;
  // Exactly one of textDelta / jsonDelta / metadata is set per
  // chunk, mirroring the AiStreamChunk oneof.
  textDelta?: string;
  jsonDelta?: Record<string, unknown>;
  metadata?: Record<string, unknown>;
}

export interface SiChatStreamHandle {
  // deltas yields every AiStreamChunk frame in order and ends
  // when the terminal AiChatResult lands (or the session aborts).
  deltas: AsyncIterable<SiChatStreamDelta>;
  // result resolves with the assembled AiChatResult after the
  // delta stream ends. Throws on session error or abort.
  result: Promise<SiChatResult>;
}

// siChat issues a non-streaming chat completion (single
// AiChatResult reply). Throws on QueryError or transport failure.
export async function siChat(
  dispatcher: Dispatcher,
  messages: SiChatMessage[],
  opts: SiChatOptions = {},
): Promise<SiChatResult> {
  if (!dispatcher) throw new Error("siChat: dispatcher is required");
  if (!Array.isArray(messages) || messages.length === 0) {
    throw new Error("siChat: messages array must be non-empty");
  }

  const requestId = newShortId();
  const reply = await dispatcher.sendAndWait(
    {
      siChat: {
        requestId,
        messages: messages.map(toWireMessage),
        ...(opts.provider ? { provider: opts.provider } : {}),
        // Always non-streaming on this surface; callers wanting
        // deltas use siChatStream.
      },
    },
    opts.signal,
  );

  const payload = readServerPayload(reply);
  if (payload?.kind === "queryError") {
    throw new Error(`siChat: ${payload.value.error?.message ?? "(no message)"}`);
  }
  if (payload?.kind !== "siChatResult") {
    throw new Error("siChat: unexpected reply envelope");
  }
  return {
    message: fromWireMessage(payload.value.message),
    provider: opts.provider ?? "",
  };
}

// siChatStream issues a streaming chat completion. The returned
// handle exposes the interleaved chunk frames as an async iterable
// and resolves to the final AiChatResult once the assembled reply
// lands. Cancel mid-stream via opts.signal.
export function siChatStream(
  dispatcher: Dispatcher,
  messages: SiChatMessage[],
  opts: SiChatOptions = {},
): SiChatStreamHandle {
  if (!dispatcher) throw new Error("siChatStream: dispatcher is required");
  if (!Array.isArray(messages) || messages.length === 0) {
    throw new Error("siChatStream: messages array must be non-empty");
  }

  const requestId = newShortId();
  const queue: SiChatStreamDelta[] = [];
  const waiters: Array<() => void> = [];
  let streamClosed = false;
  let streamError: Error | null = null;
  let finalResult: SiChatResult | null = null;
  // Definite-assignment: the Promise executor runs synchronously
  // and assigns both before any caller can reach closeStream.
  let resolveResult!: (r: SiChatResult) => void;
  let rejectResult!: (err: Error) => void;
  const resultPromise = new Promise<SiChatResult>((resolve, reject) => {
    resolveResult = resolve;
    rejectResult = reject;
  });

  const notify = () => {
    while (waiters.length > 0) {
      const w = waiters.shift();
      if (w) w();
    }
  };

  const closeStream = (err: Error | null) => {
    if (streamClosed) return;
    streamClosed = true;
    streamError = err;
    if (err) rejectResult(err);
    else if (finalResult) resolveResult(finalResult);
    else rejectResult(new Error("siChatStream: ended before result"));
    notify();
  };

  const unregister = dispatcher.registerStream(requestId, (msg: ServerMessage) => {
    const payload = readServerPayload(msg);
    if (payload?.kind === "siChunk") {
      queue.push(chunkToDelta(payload.value));
      notify();
      return;
    }
    if (payload?.kind === "siChatResult") {
      finalResult = {
        message: fromWireMessage(payload.value.message),
        provider: opts.provider ?? "",
      };
      closeStream(null);
      return;
    }
    if (payload?.kind === "queryError") {
      closeStream(new Error(`siChatStream: ${payload.value.error?.message ?? "(no message)"}`));
      return;
    }
  });

  const abortListener = () => {
    closeStream(new Error("siChatStream: aborted"));
  };

  const aborted = opts.signal?.aborted === true;
  if (opts.signal && !aborted) {
    opts.signal.addEventListener("abort", abortListener, { once: true });
  }

  if (aborted) {
    unregister();
    closeStream(new Error("siChatStream: aborted before start"));
  } else {
    // Fire the request without waiting for correlateTo -- the chunks
    // ride registerStream via requestId routing instead.
    try {
      dispatcher.send({
        siChat: {
          requestId,
          messages: messages.map(toWireMessage),
          stream: true,
          ...(opts.provider ? { provider: opts.provider } : {}),
        },
      });
    } catch (err) {
      unregister();
      closeStream(err instanceof Error ? err : new Error(String(err)));
    }
  }

  const cleanup = () => {
    unregister();
    if (opts.signal) opts.signal.removeEventListener("abort", abortListener);
  };

  resultPromise.finally(cleanup).catch(() => {
    /* swallow: caller observes via result/deltas */
  });

  const deltas: AsyncIterable<SiChatStreamDelta> = {
    [Symbol.asyncIterator](): AsyncIterator<SiChatStreamDelta> {
      return {
        async next() {
          while (queue.length === 0 && !streamClosed) {
            await new Promise<void>((r) => waiters.push(r));
          }
          if (queue.length > 0) {
            return { value: queue.shift()!, done: false };
          }
          if (streamError) throw streamError;
          return { value: undefined, done: true };
        },
        async return() {
          closeStream(streamError ?? new Error("siChatStream: iterator returned early"));
          return { value: undefined, done: true };
        },
      };
    },
  };

  return { deltas, result: resultPromise };
}

function toWireMessage(m: SiChatMessage): AiChatMessageWire {
  return {
    role: m.role,
    content: m.content,
    ...(m.name ? { name: m.name } : {}),
  };
}

function fromWireMessage(m: AiChatMessageWire | undefined): SiChatMessage {
  return {
    role: m?.role ?? "",
    content: m?.content ?? "",
    ...(m?.name ? { name: m.name } : {}),
  };
}

function chunkToDelta(c: AiStreamChunkPayload): SiChatStreamDelta {
  return {
    index: numberFromWire(c.index),
    done: c.done === true,
    ...(typeof c.textDelta === "string" ? { textDelta: c.textDelta } : {}),
    ...(c.jsonDelta ? { jsonDelta: c.jsonDelta } : {}),
    ...(c.metadata ? { metadata: c.metadata } : {}),
  };
}

function numberFromWire(v: string | number | undefined): number {
  if (typeof v === "number") return v;
  if (typeof v === "string" && v) {
    const n = Number(v);
    return Number.isFinite(n) ? n : 0;
  }
  return 0;
}
