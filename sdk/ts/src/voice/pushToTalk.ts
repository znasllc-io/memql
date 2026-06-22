// pushToTalk mirrors sdk/go/voice/pushtotalk.go. It opens a streaming
// transcription session, pumps audio bytes from a ReadableStream as
// AiTranscribeStreamChunk envelopes, surfaces deltas via onPartial,
// and resolves with the final transcript on completion.
//
// The SDK owns the wire protocol; the caller owns the audio source --
// terminal apps wire a Web Audio buffer, browsers wire a MediaRecorder
// (helpers like `recorderToReadableStream` may live alongside this
// file when the first browser consumer needs them).

import type { Dispatcher } from "../client/dispatcher.js";
import { newShortId } from "../client/id.js";
import {
  readServerPayload,
  type ServerMessage,
  type AiTranscribeStreamDeltaPayload,
} from "../client/wire.js";

export interface AudioFormat {
  encoding: "pcm16" | "opus" | "webm" | "wav" | (string & {});
  sampleRate: number;
  channels: number;
}

export interface PartialTranscript {
  text: string;
  isFinal: boolean;
  confidence: number;
}

export interface FinalTranscript {
  text: string;
  durationMs: number;
  provider: string;
}

export interface PushToTalkOptions {
  audio: AudioFormat;
  // ISO-639-1 language hint, e.g. "en". Empty defers to the provider.
  language?: string;
  // Provider override (e.g. "openai-realtime", "openai-whisper"). Empty
  // uses the cluster default.
  provider?: string;
  // Called on every delta. Safe to be undefined.
  onPartial?: (p: PartialTranscript) => void;
  // Cancel the session mid-stream.
  signal?: AbortSignal;
  // For consumers that prefer pull-based delta streaming over the
  // onPartial callback. Resolves to an AsyncIterable that yields
  // PartialTranscripts and ends when Complete lands or the session
  // aborts. When set, onPartial is still invoked first (both work
  // together).
  yieldDeltas?: boolean;
}

// pushToTalk resolves with the FinalTranscript when the server emits
// Complete. Throws on session-level failure or AbortSignal cancellation.
export async function pushToTalk(
  dispatcher: Dispatcher,
  audio: ReadableStream<Uint8Array>,
  opts: PushToTalkOptions,
): Promise<FinalTranscript> {
  if (!dispatcher) throw new Error("pushToTalk: dispatcher is required");
  if (!audio) throw new Error("pushToTalk: audio stream is required");
  if (!opts.audio?.encoding || !opts.audio.sampleRate || !opts.audio.channels) {
    throw new Error(
      "pushToTalk: audio format must specify encoding + sampleRate + channels",
    );
  }
  const requestId = newShortId();
  const session = new TranscriptionSession(dispatcher, requestId, opts);
  return session.run(audio);
}

class TranscriptionSession {
  private readonly dispatcher: Dispatcher;
  private readonly requestId: string;
  private readonly opts: PushToTalkOptions;

  constructor(dispatcher: Dispatcher, requestId: string, opts: PushToTalkOptions) {
    this.dispatcher = dispatcher;
    this.requestId = requestId;
    this.opts = opts;
  }

  async run(audio: ReadableStream<Uint8Array>): Promise<FinalTranscript> {
    let resolveFinal: (t: FinalTranscript) => void;
    let rejectFinal: (err: Error) => void;
    const finalPromise = new Promise<FinalTranscript>((resolve, reject) => {
      resolveFinal = resolve;
      rejectFinal = reject;
    });

    const unregister = this.dispatcher.registerStream(this.requestId, (msg: ServerMessage) => {
      const payload = readServerPayload(msg);
      if (payload?.kind === "aiTranscribeStreamDelta") {
        if (this.opts.onPartial) {
          this.opts.onPartial(deltaToPartial(payload.value));
        }
      } else if (payload?.kind === "aiTranscribeStreamComplete") {
        resolveFinal({
          text: payload.value.text ?? "",
          durationMs: numberFromWire(payload.value.durationMs),
          provider: payload.value.provider ?? "",
        });
      }
    });

    const abortListener = () => {
      // Best-effort abort -- the dispatcher may be torn down already.
      try {
        this.sendEnd(true);
      } catch {
        /* ignore */
      }
      rejectFinal(new Error("pushToTalk: aborted"));
    };
    if (this.opts.signal) {
      if (this.opts.signal.aborted) {
        unregister();
        throw new Error("pushToTalk: aborted before start");
      }
      this.opts.signal.addEventListener("abort", abortListener, { once: true });
    }

    try {
      this.sendStart();
      // Drive the audio pump in parallel with the reply listener.
      const pumpPromise = this.pumpAudio(audio);
      pumpPromise.catch((err) => {
        // Audio pump failed -- best-effort abort the session and
        // surface the error.
        try {
          this.sendEnd(true);
        } catch {
          /* ignore */
        }
        rejectFinal(err instanceof Error ? err : new Error(String(err)));
      });
      return await finalPromise;
    } finally {
      unregister();
      if (this.opts.signal) {
        this.opts.signal.removeEventListener("abort", abortListener);
      }
    }
  }

  private sendStart(): void {
    this.dispatcher.send({
      aiTranscribeStreamStart: {
        requestId: this.requestId,
        format: this.opts.audio.encoding,
        sampleRate: this.opts.audio.sampleRate,
        channels: this.opts.audio.channels,
        ...(this.opts.language ? { languageHint: this.opts.language } : {}),
        ...(this.opts.provider ? { provider: this.opts.provider } : {}),
      },
    });
  }

  private sendChunk(audio: Uint8Array): void {
    this.dispatcher.send({
      aiTranscribeStreamChunk: {
        requestId: this.requestId,
        audio: bytesToBase64(audio),
      },
    });
  }

  private sendEnd(cancel: boolean): void {
    this.dispatcher.send({
      aiTranscribeStreamEnd: {
        requestId: this.requestId,
        ...(cancel ? { cancel: true } : {}),
      },
    });
  }

  private async pumpAudio(audio: ReadableStream<Uint8Array>): Promise<void> {
    const reader = audio.getReader();
    try {
      for (;;) {
        const { value, done } = await reader.read();
        if (done) {
          this.sendEnd(false);
          return;
        }
        if (value && value.length > 0) this.sendChunk(value);
      }
    } finally {
      try {
        reader.releaseLock();
      } catch {
        /* ignore */
      }
    }
  }
}

function deltaToPartial(d: AiTranscribeStreamDeltaPayload): PartialTranscript {
  return {
    text: d.text ?? "",
    isFinal: d.isFinal === true,
    confidence: typeof d.confidence === "number" ? d.confidence : 0,
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

function bytesToBase64(bytes: Uint8Array): string {
  // Prefer Node's Buffer when present (no @types/node dependency --
  // probe via globalThis so a browser bundle doesn't trip on the
  // bare `Buffer` identifier). Fall back to btoa for browsers.
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
  throw new Error("pushToTalk: no base64 encoder available (need Buffer or btoa)");
}
