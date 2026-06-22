// aiTranscribe wraps MemqlClientMessage.ai_transcribe (one-shot STT).
// The streaming counterpart lives in ../voice/pushToTalk.ts.
// AiTranscribeMsg.audio is `string` in the proto -- the caller hands
// over raw bytes and we base64-encode them for the protojson envelope.

import type { Dispatcher } from "../client/dispatcher.js";
import { newShortId } from "../client/id.js";
import { readServerPayload } from "../client/wire.js";

export interface AiTranscribeOptions {
  mimeType?: string;
  signal?: AbortSignal;
}

export interface AiTranscribeResult {
  text: string;
}

// aiTranscribe accepts the audio bytes (raw, not pre-encoded) and
// returns the transcribed text. Throws on QueryError, transport
// failure, or abort.
export async function aiTranscribe(
  dispatcher: Dispatcher,
  audio: Uint8Array,
  opts: AiTranscribeOptions = {},
): Promise<AiTranscribeResult> {
  if (!dispatcher) throw new Error("aiTranscribe: dispatcher is required");
  if (!(audio instanceof Uint8Array) || audio.length === 0) {
    throw new Error("aiTranscribe: audio bytes must be non-empty");
  }

  const requestId = newShortId();
  const reply = await dispatcher.sendAndWait(
    {
      aiTranscribe: {
        requestId,
        audio: bytesToBase64(audio),
        ...(opts.mimeType ? { mimeType: opts.mimeType } : {}),
      },
    },
    opts.signal,
  );

  const payload = readServerPayload(reply);
  if (payload?.kind === "queryError") {
    throw new Error(`aiTranscribe: ${payload.value.error?.message ?? "(no message)"}`);
  }
  if (payload?.kind !== "aiTranscribeResult") {
    throw new Error("aiTranscribe: unexpected reply envelope");
  }
  return { text: payload.value.text ?? "" };
}

function bytesToBase64(bytes: Uint8Array): string {
  const g = globalThis as unknown as {
    Buffer?: { from(bytes: Uint8Array): { toString(enc: string): string } };
    btoa?: (s: string) => string;
  };
  if (g.Buffer) return g.Buffer.from(bytes).toString("base64");
  if (g.btoa) {
    let binary = "";
    for (const b of bytes) binary += String.fromCharCode(b);
    return g.btoa(binary);
  }
  throw new Error("aiTranscribe: no base64 encoder available (need Buffer or btoa)");
}
