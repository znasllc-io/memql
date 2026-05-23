// siSpeech wraps MemqlClientMessage.si_speech (text-to-speech).
// One-shot request/reply: SISpeechMsg -> SISpeechResult carrying
// the synthesized audio bytes (decoded from protojson base64).

import type { Dispatcher } from "../client/dispatcher.js";
import { newShortId } from "../client/id.js";
import { readServerPayload } from "../client/wire.js";

export interface SiSpeechOptions {
  voice?: string;
  format?: string; // "wav" | "mp3" | "ogg" | ...
  provider?: string;
  signal?: AbortSignal;
}

export interface SiSpeechResult {
  audio: Uint8Array;
  format: string;
}

// siSpeech synthesizes audio for the given input string. Throws on
// QueryError, transport failure, or abort.
export async function siSpeech(
  dispatcher: Dispatcher,
  input: string,
  opts: SiSpeechOptions = {},
): Promise<SiSpeechResult> {
  if (!dispatcher) throw new Error("siSpeech: dispatcher is required");
  if (typeof input !== "string" || input.length === 0) {
    throw new Error("siSpeech: input string must be non-empty");
  }

  const requestId = newShortId();
  const reply = await dispatcher.sendAndWait(
    {
      siSpeech: {
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
    throw new Error(`siSpeech: ${payload.value.error?.message ?? "(no message)"}`);
  }
  if (payload?.kind !== "siSpeechResult") {
    throw new Error("siSpeech: unexpected reply envelope");
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
  throw new Error("siSpeech: no base64 decoder available (need Buffer or atob)");
}
