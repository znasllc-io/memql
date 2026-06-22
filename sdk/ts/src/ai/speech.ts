// aiSpeech wraps MemqlClientMessage.ai_speech (text-to-speech).
// One-shot request/reply: AiSpeechMsg -> AiSpeechResult carrying
// the synthesized audio bytes (decoded from protojson base64).

import type { Dispatcher } from "../client/dispatcher.js";
import { newShortId } from "../client/id.js";
import { readServerPayload } from "../client/wire.js";

export interface AiSpeechOptions {
  voice?: string;
  format?: string; // "wav" | "mp3" | "ogg" | ...
  provider?: string;
  signal?: AbortSignal;
}

export interface AiSpeechResult {
  audio: Uint8Array;
  format: string;
}

// aiSpeech synthesizes audio for the given input string. Throws on
// QueryError, transport failure, or abort.
export async function aiSpeech(
  dispatcher: Dispatcher,
  input: string,
  opts: AiSpeechOptions = {},
): Promise<AiSpeechResult> {
  if (!dispatcher) throw new Error("aiSpeech: dispatcher is required");
  if (typeof input !== "string" || input.length === 0) {
    throw new Error("aiSpeech: input string must be non-empty");
  }

  const requestId = newShortId();
  const reply = await dispatcher.sendAndWait(
    {
      aiSpeech: {
        requestId,
        input,
        ...(opts.voice ? { voice: opts.voice } : {}),
        ...(opts.format ? { format: opts.format } : {}),
        ...(opts.provider ? { provider: opts.provider } : {}),
      },
    },
    opts.signal,
  );

  const payload = readServerPayload(reply);
  if (payload?.kind === "queryError") {
    throw new Error(`aiSpeech: ${payload.value.error?.message ?? "(no message)"}`);
  }
  if (payload?.kind !== "aiSpeechResult") {
    throw new Error("aiSpeech: unexpected reply envelope");
  }

  return {
    audio: base64ToBytes(payload.value.audio ?? ""),
    format: payload.value.format ?? "",
  };
}

function base64ToBytes(b64: string): Uint8Array {
  if (!b64) return new Uint8Array(0);
  // Prefer Node's Buffer when present (no @types/node dep -- probe
  // via globalThis so a browser bundle doesn't trip on the bare
  // `Buffer` identifier). Fall back to atob for browsers.
  const g = globalThis as unknown as {
    Buffer?: { from(s: string, enc: string): Uint8Array };
    atob?: (s: string) => string;
  };
  if (g.Buffer) return new Uint8Array(g.Buffer.from(b64, "base64"));
  if (g.atob) {
    const bin = g.atob(b64);
    const out = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i += 1) out[i] = bin.charCodeAt(i);
    return out;
  }
  throw new Error("aiSpeech: no base64 decoder available (need Buffer or atob)");
}
