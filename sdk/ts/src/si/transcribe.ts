// siTranscribe wraps MemqlClientMessage.si_transcribe (one-shot STT).
// The streaming counterpart lives in ../voice/pushToTalk.ts.
// SITranscribeMsg.audio is `string` in the proto -- the caller hands
// over raw bytes and we base64-encode them for the protojson envelope.

import type { Dispatcher } from "../client/dispatcher.js";
import { newShortId } from "../client/id.js";
import { readServerPayload } from "../client/wire.js";

export interface SiTranscribeOptions {
  mimeType?: string;
  signal?: AbortSignal;
}

export interface SiTranscribeResult {
  text: string;
}

// siTranscribe accepts the audio bytes (raw, not pre-encoded) and
// returns the transcribed text. Throws on QueryError, transport
// failure, or abort.
export async function siTranscribe(
  dispatcher: Dispatcher,
  audio: Uint8Array,
  opts: SiTranscribeOptions = {},
): Promise<SiTranscribeResult> {
  if (!dispatcher) throw new Error("siTranscribe: dispatcher is required");
  if (!(audio instanceof Uint8Array) || audio.length === 0) {
    throw new Error("siTranscribe: audio bytes must be non-empty");
  }

  const requestId = newShortId();
  const reply = await dispatcher.sendAndWait(
    {
      siTranscribe: {
        requestId,
        audio: bytesToBase64(audio),
        ...(opts.mimeType ? { mimeType: opts.mimeType } : {}),
      },
    },
    opts.signal,
  );

  const payload = readServerPayload(reply);
  if (payload?.kind === "queryError") {
    throw new Error(`siTranscribe: ${payload.value.error?.message ?? "(no message)"}`);
  }
  if (payload?.kind !== "siTranscribeResult") {
    throw new Error("siTranscribe: unexpected reply envelope");
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
  throw new Error("siTranscribe: no base64 encoder available (need Buffer or btoa)");
}
