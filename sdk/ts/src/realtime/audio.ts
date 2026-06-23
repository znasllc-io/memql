// Browser-side client for the `/memql/audio` WebSocket path. This
// is a SEPARATE transport from `/memql/ws` -- a simpler streaming-
// STT + streaming-TTS protocol that predates the gRPC stream and
// is still used by the CoPresent SPA for the per-space "speak to
// add an utterance" affordance and for live agent TTS playback.
//
// Auth: same `bearer_token` / `guest_token` / `worker_token` query
// string the main Connection uses. Browsers can't set custom
// headers on the WebSocket upgrade so the stamping is identical.
//
// Wire shape (mirrors component/server/audiows/messages.go):
//
//   client -> server:
//     { type: "start",      streamId, partitionId, participantId,
//                            format, sampleRate, channels, languageHint? }
//     { type: "chunk",      streamId, audio (base64), sequence }
//     { type: "end",        streamId, cancelled? }
//     { type: "synthesize", requestId, text, voice?, format?,
//                            sampleRate?, partitionId?, participantId? }
//
//   server -> client:
//     { type: "started",       streamId }
//     { type: "transcription", streamId, text, isFinal,
//                               confidence?, words?, utteranceId?, durationMs? }
//     { type: "ended",         streamId, cancelled? }
//     { type: "error",         streamId?, error: { code, message } }
//     { type: "tts_started",   requestId, format, sampleRate,
//                               partitionId?, participantId?, text }
//     { type: "tts_chunk",     requestId, audio (base64), format,
//                               sampleRate, sequence, done,
//                               partitionId, participantId, text }
//     { type: "tts_ended",     requestId, partitionId?, participantId?,
//                               cancelled?, error? }

import { newShortId } from "../client/id.js";

export interface AudioConnectionAuth {
  bearer?: string;
  guestToken?: string;
  workerToken?: string;
}

export interface DialAudioOptions {
  // wss://host/memql/audio OR a path resolved against the document
  // origin in a browser. The default Connection.dial endpoint
  // pattern (".../memql/ws") swapped for ".../memql/audio".
  endpoint: string;
  auth?: AudioConnectionAuth;
  webSocketFactory?: (url: string) => WebSocket;
  logger?: Pick<Console, "warn" | "info" | "error"> | null;
  // Override the open-handshake timeout (default 5_000 ms).
  handshakeTimeoutMs?: number;
}

// ---------- Transcribe (mic -> text) ----------

export type AudioFormat = "pcm16" | "opus" | "webm" | "wav" | (string & {});

export interface TranscribeStreamOptions {
  partitionId: string;
  participantId: string;
  format: AudioFormat;
  sampleRate: number;
  channels: number;
  languageHint?: string;
  // Optional pre-seeded streamId for correlation with the server's
  // structured-log lines / utterance row. Auto-generated when absent.
  streamId?: string;
}

export interface TranscribePartial {
  streamId: string;
  text: string;
  isFinal: false;
  confidence: number;
}

export interface TranscribeFinal {
  streamId: string;
  text: string;
  isFinal: true;
  confidence: number;
  utteranceId: string;
  durationMs: number;
  words: unknown[];
}

export type TranscribeEvent = TranscribePartial | TranscribeFinal;

export interface TranscribeStream {
  // Caller-known streamId; useful for cross-referencing logs.
  streamId: string;
  // Push a base64-encoded chunk of audio. Sequence numbers are
  // auto-incremented; the server uses them only for ordering.
  pushChunk(audioBase64: string): void;
  // Same as pushChunk but accepts raw bytes and base64-encodes
  // them for you. Convenience for browsers wiring up
  // MediaRecorder.
  pushBytes(audio: Uint8Array): void;
  // Async iterable of transcription events. Yields interim
  // partials first, then a single final with isFinal=true. The
  // iterable completes when the server emits "ended" OR the
  // caller calls end() / cancel().
  events: AsyncIterable<TranscribeEvent>;
  // Mark the stream complete; the server may still emit one
  // additional final transcription before "ended".
  end(): void;
  // Abort the stream; server transitions to "ended" with
  // cancelled=true and no more transcriptions arrive.
  cancel(): void;
}

// ---------- Synthesize (text -> audio) ----------

export interface SynthesizeOptions {
  text: string;
  voice?: string;
  format?: AudioFormat;
  sampleRate?: number;
  partitionId?: string;
  participantId?: string;
  // Optional pre-seeded requestId. Auto-generated when absent.
  requestId?: string;
}

export interface SynthesizeStartedEvent {
  kind: "started";
  requestId: string;
  format: AudioFormat;
  sampleRate: number;
  text: string;
  partitionId: string;
  participantId: string;
}

export interface SynthesizeChunkEvent {
  kind: "chunk";
  requestId: string;
  audioBase64: string;
  format: AudioFormat;
  sampleRate: number;
  sequence: number;
  done: boolean;
}

export interface SynthesizeEndedEvent {
  kind: "ended";
  requestId: string;
  cancelled: boolean;
  error: string;
}

export type SynthesizeEvent = SynthesizeStartedEvent | SynthesizeChunkEvent | SynthesizeEndedEvent;

export interface SynthesizeStream {
  requestId: string;
  events: AsyncIterable<SynthesizeEvent>;
  // No client-side cancel for TTS today -- the server completes
  // the synthesis once it starts. Closing the AudioClient (or
  // the underlying socket) is the only way to stop in-flight TTS.
}

// ---------- AudioClient ----------

const DEFAULT_HANDSHAKE_TIMEOUT_MS = 5_000;

interface ServerEnvelope {
  type?: string;
  streamId?: string;
  requestId?: string;
  text?: string;
  isFinal?: boolean;
  confidence?: number;
  words?: unknown[];
  utteranceId?: string;
  durationMs?: number | string;
  cancelled?: boolean;
  error?: { code?: string; message?: string } | string;
  audio?: string;
  format?: string;
  sampleRate?: number;
  sequence?: number;
  done?: boolean;
  partitionId?: string;
  participantId?: string;
}

export class AudioClient {
  private readonly socket: WebSocket;
  private readonly logger: DialAudioOptions["logger"];
  private closed = false;
  // Per-stream + per-request inbound routing.
  private readonly transcribeHandlers = new Map<string, (e: ServerEnvelope) => void>();
  private readonly synthesizeHandlers = new Map<string, (e: ServerEnvelope) => void>();

  private constructor(socket: WebSocket, opts: DialAudioOptions) {
    this.socket = socket;
    this.logger = opts.logger ?? null;
    // Force ArrayBuffer for binary frames so the Blob.text() path
    // in handleMessage stays a defensive net rather than the
    // normal hot path -- async .text() resolution can otherwise
    // reorder frames vs intervening string frames on mixed streams.
    socket.binaryType = "arraybuffer";
    socket.addEventListener("message", this.handleMessage);
    socket.addEventListener("close", this.handleClose);
  }

  static async dial(opts: DialAudioOptions): Promise<AudioClient> {
    const url = resolveAudioEndpoint(opts.endpoint, opts.auth);
    const factory = opts.webSocketFactory ?? defaultWebSocketFactory;
    const socket = factory(url);
    await waitForOpen(socket, opts.handshakeTimeoutMs ?? DEFAULT_HANDSHAKE_TIMEOUT_MS);
    return new AudioClient(socket, opts);
  }

  transcribe(opts: TranscribeStreamOptions): TranscribeStream {
    if (this.closed) throw new Error("AudioClient.transcribe: client is closed");
    if (!opts.partitionId) throw new Error("AudioClient.transcribe: partitionId is required");
    if (!opts.participantId) throw new Error("AudioClient.transcribe: participantId is required");
    if (!opts.format) throw new Error("AudioClient.transcribe: format is required");
    if (!opts.sampleRate) throw new Error("AudioClient.transcribe: sampleRate is required");
    if (!opts.channels) throw new Error("AudioClient.transcribe: channels is required");

    const streamId = opts.streamId ?? newShortId();
    const queue: TranscribeEvent[] = [];
    const waiters: Array<() => void> = [];
    let closed = false;
    let streamError: Error | null = null;
    let sequence = 0;

    const notify = () => {
      while (waiters.length > 0) {
        const w = waiters.shift();
        if (w) w();
      }
    };
    const closeStream = (err: Error | null) => {
      if (closed) return;
      closed = true;
      streamError = err;
      this.transcribeHandlers.delete(streamId);
      notify();
    };

    this.transcribeHandlers.set(streamId, (msg) => {
      if (msg.type === "transcription") {
        if (msg.isFinal === true) {
          queue.push({
            streamId,
            text: msg.text ?? "",
            isFinal: true,
            confidence: typeof msg.confidence === "number" ? msg.confidence : 0,
            utteranceId: msg.utteranceId ?? "",
            durationMs: numberFromWire(msg.durationMs),
            words: Array.isArray(msg.words) ? msg.words : [],
          });
        } else {
          queue.push({
            streamId,
            text: msg.text ?? "",
            isFinal: false,
            confidence: typeof msg.confidence === "number" ? msg.confidence : 0,
          });
        }
        notify();
        return;
      }
      if (msg.type === "ended") {
        closeStream(null);
        return;
      }
      if (msg.type === "error") {
        const m = typeof msg.error === "object" ? msg.error?.message ?? "(no message)" : String(msg.error);
        closeStream(new Error(`AudioClient.transcribe: ${m}`));
        return;
      }
      // "started" + anything else: ignore for now.
    });

    this.rawSend({
      type: "start",
      streamId,
      partitionId: opts.partitionId,
      participantId: opts.participantId,
      format: opts.format,
      sampleRate: opts.sampleRate,
      channels: opts.channels,
      ...(opts.languageHint ? { languageHint: opts.languageHint } : {}),
    });

    const events: AsyncIterable<TranscribeEvent> = {
      [Symbol.asyncIterator](): AsyncIterator<TranscribeEvent> {
        return {
          async next() {
            while (queue.length === 0 && !closed) {
              await new Promise<void>((r) => waiters.push(r));
            }
            if (queue.length > 0) return { value: queue.shift()!, done: false };
            if (streamError) throw streamError;
            return { value: undefined, done: true };
          },
          async return() {
            closeStream(streamError);
            return { value: undefined, done: true };
          },
        };
      },
    };

    return {
      streamId,
      pushChunk: (audioBase64) => {
        if (closed) return;
        this.rawSend({ type: "chunk", streamId, audio: audioBase64, sequence: sequence++ });
      },
      pushBytes: (audio) => {
        if (closed) return;
        this.rawSend({
          type: "chunk",
          streamId,
          audio: bytesToBase64(audio),
          sequence: sequence++,
        });
      },
      events,
      end: () => {
        if (closed) return;
        this.rawSend({ type: "end", streamId });
      },
      cancel: () => {
        if (closed) return;
        this.rawSend({ type: "end", streamId, cancelled: true });
      },
    };
  }

  synthesize(opts: SynthesizeOptions): SynthesizeStream {
    if (this.closed) throw new Error("AudioClient.synthesize: client is closed");
    if (!opts.text) throw new Error("AudioClient.synthesize: text is required");

    const requestId = opts.requestId ?? newShortId();
    const queue: SynthesizeEvent[] = [];
    const waiters: Array<() => void> = [];
    let closed = false;
    let streamError: Error | null = null;

    const notify = () => {
      while (waiters.length > 0) {
        const w = waiters.shift();
        if (w) w();
      }
    };
    const closeStream = (err: Error | null) => {
      if (closed) return;
      closed = true;
      streamError = err;
      this.synthesizeHandlers.delete(requestId);
      notify();
    };

    this.synthesizeHandlers.set(requestId, (msg) => {
      if (msg.type === "tts_started") {
        queue.push({
          kind: "started",
          requestId,
          format: (msg.format ?? "") as AudioFormat,
          sampleRate: typeof msg.sampleRate === "number" ? msg.sampleRate : 0,
          text: msg.text ?? "",
          partitionId: msg.partitionId ?? "",
          participantId: msg.participantId ?? "",
        });
        notify();
        return;
      }
      if (msg.type === "tts_chunk") {
        queue.push({
          kind: "chunk",
          requestId,
          audioBase64: msg.audio ?? "",
          format: (msg.format ?? "") as AudioFormat,
          sampleRate: typeof msg.sampleRate === "number" ? msg.sampleRate : 0,
          sequence: typeof msg.sequence === "number" ? msg.sequence : 0,
          done: msg.done === true,
        });
        notify();
        return;
      }
      if (msg.type === "tts_ended") {
        const errMsg = typeof msg.error === "string" ? msg.error : "";
        queue.push({
          kind: "ended",
          requestId,
          cancelled: msg.cancelled === true,
          error: errMsg,
        });
        notify();
        closeStream(null);
        return;
      }
    });

    this.rawSend({
      type: "synthesize",
      requestId,
      text: opts.text,
      ...(opts.voice ? { voice: opts.voice } : {}),
      ...(opts.format ? { format: opts.format } : {}),
      ...(opts.sampleRate ? { sampleRate: opts.sampleRate } : {}),
      ...(opts.partitionId ? { partitionId: opts.partitionId } : {}),
      ...(opts.participantId ? { participantId: opts.participantId } : {}),
    });

    const events: AsyncIterable<SynthesizeEvent> = {
      [Symbol.asyncIterator](): AsyncIterator<SynthesizeEvent> {
        return {
          async next() {
            while (queue.length === 0 && !closed) {
              await new Promise<void>((r) => waiters.push(r));
            }
            if (queue.length > 0) return { value: queue.shift()!, done: false };
            if (streamError) throw streamError;
            return { value: undefined, done: true };
          },
          async return() {
            closeStream(streamError);
            return { value: undefined, done: true };
          },
        };
      },
    };

    return { requestId, events };
  }

  close(): void {
    if (this.closed) return;
    this.closed = true;
    this.socket.removeEventListener("message", this.handleMessage);
    this.socket.removeEventListener("close", this.handleClose);
    try {
      this.socket.close(1000, "client closing");
    } catch {
      /* ignore */
    }
  }

  done(): Promise<void> {
    if (this.closed || this.socket.readyState >= WebSocket.CLOSING) {
      return Promise.resolve();
    }
    return new Promise<void>((resolve) => {
      const onClose = () => {
        this.socket.removeEventListener("close", onClose);
        resolve();
      };
      this.socket.addEventListener("close", onClose);
    });
  }

  private rawSend(msg: unknown): void {
    if (this.socket.readyState !== WebSocket.OPEN) {
      throw new Error(`AudioClient: socket not open (readyState=${this.socket.readyState})`);
    }
    this.socket.send(JSON.stringify(msg));
  }

  private readonly handleMessage = (ev: MessageEvent): void => {
    let data: string;
    if (typeof ev.data === "string") {
      data = ev.data;
    } else if (ev.data instanceof ArrayBuffer) {
      data = new TextDecoder().decode(new Uint8Array(ev.data));
    } else if (ev.data instanceof Blob) {
      ev.data
        .text()
        .then((t) => this.route(t))
        .catch((err) => this.logger?.warn?.("memql audio: failed to read blob frame", err));
      return;
    } else {
      this.logger?.warn?.("memql audio: unsupported frame type", ev.data);
      return;
    }
    this.route(data);
  };

  private route(data: string): void {
    let msg: ServerEnvelope;
    try {
      msg = JSON.parse(data) as ServerEnvelope;
    } catch (err) {
      this.logger?.warn?.("memql audio: malformed JSON frame", err);
      return;
    }
    // Transcribe family routes by streamId; synthesize family by
    // requestId. "error" without either id is logged at warn.
    if (msg.streamId) {
      const h = this.transcribeHandlers.get(msg.streamId);
      if (h) {
        h(msg);
        return;
      }
    }
    if (msg.requestId) {
      const h = this.synthesizeHandlers.get(msg.requestId);
      if (h) {
        h(msg);
        return;
      }
    }
    if (msg.type === "error") {
      // Untargeted error -- no streamId / requestId means the server
      // is signalling a connection-level fault (auth, malformed
      // handshake, server shutdown). Treat as terminal: fail every
      // in-flight handler so consumer iterators learn immediately
      // instead of waiting for the eventual socket close.
      const m = typeof msg.error === "object" ? msg.error?.message ?? "(no message)" : String(msg.error);
      this.logger?.warn?.("memql audio: untargeted error frame, terminating in-flight streams", m);
      const errMsg = `AudioClient: ${m}`;
      for (const h of this.transcribeHandlers.values()) {
        h({ type: "error", error: { message: errMsg } });
      }
      for (const h of this.synthesizeHandlers.values()) {
        h({ type: "tts_ended", error: errMsg });
      }
      this.transcribeHandlers.clear();
      this.synthesizeHandlers.clear();
    }
  }

  private readonly handleClose = (): void => {
    if (this.closed) return;
    this.closed = true;
    // Fail every in-flight handler so consumer iterators terminate.
    const transportErr = new Error("AudioClient: socket closed");
    for (const h of this.transcribeHandlers.values()) {
      h({ type: "error", error: { message: transportErr.message } });
    }
    for (const h of this.synthesizeHandlers.values()) {
      h({ type: "tts_ended", error: transportErr.message });
    }
    this.transcribeHandlers.clear();
    this.synthesizeHandlers.clear();
  };
}

// ---------- helpers ----------

function defaultWebSocketFactory(url: string): WebSocket {
  if (typeof WebSocket === "undefined") {
    throw new Error(
      "memql sdk: no global WebSocket available -- pass webSocketFactory or run in a browser / Node 22+",
    );
  }
  return new WebSocket(url);
}

function waitForOpen(socket: WebSocket, timeoutMs: number): Promise<void> {
  if (socket.readyState === WebSocket.OPEN) return Promise.resolve();
  return new Promise<void>((resolve, reject) => {
    const timer = setTimeout(() => {
      cleanupAndClose();
      reject(new Error(`AudioClient: open handshake timed out after ${timeoutMs}ms`));
    }, timeoutMs);
    const onOpen = () => {
      cleanup();
      resolve();
    };
    const onError = (ev: Event) => {
      cleanupAndClose();
      reject(new Error(`AudioClient: open failed: ${(ev as ErrorEvent).message ?? "error"}`));
    };
    const onClose = (ev: CloseEvent) => {
      // Socket is already closed; just remove listeners + clear timer.
      cleanup();
      reject(new Error(`AudioClient: closed before open: code=${ev.code} reason=${ev.reason}`));
    };
    const cleanup = () => {
      clearTimeout(timer);
      socket.removeEventListener("open", onOpen);
      socket.removeEventListener("error", onError);
      socket.removeEventListener("close", onClose);
    };
    // Rejection paths additionally close the socket so a still-
    // CONNECTING socket doesn't leak in Node-style runtimes that
    // don't GC unreferenced sockets aggressively. Idempotent under
    // a socket already in CLOSING / CLOSED state.
    const cleanupAndClose = () => {
      cleanup();
      try {
        socket.close();
      } catch {
        /* ignore */
      }
    };
    socket.addEventListener("open", onOpen);
    socket.addEventListener("error", onError);
    socket.addEventListener("close", onClose);
  });
}

// resolveAudioEndpoint mirrors Connection.resolveEndpoint -- same
// query-string auth (browsers can't set Authorization on the
// upgrade).
function resolveAudioEndpoint(endpoint: string, auth: AudioConnectionAuth | undefined): string {
  let url: URL;
  if (/^wss?:\/\//i.test(endpoint)) {
    url = new URL(endpoint);
  } else if (typeof globalThis.location !== "undefined") {
    const loc = globalThis.location;
    const proto = loc.protocol === "https:" ? "wss:" : "ws:";
    url = new URL(endpoint, `${proto}//${loc.host}`);
  } else {
    throw new Error(`AudioClient: cannot resolve relative endpoint ${endpoint} without a window.location`);
  }
  if (auth?.guestToken) url.searchParams.set("guest_token", auth.guestToken);
  if (auth?.workerToken) url.searchParams.set("worker_token", auth.workerToken);
  if (auth?.bearer) url.searchParams.set("bearer_token", auth.bearer);
  return url.toString();
}

function bytesToBase64(bytes: Uint8Array): string {
  const g = globalThis as unknown as {
    Buffer?: { from(b: Uint8Array): { toString(enc: string): string } };
    btoa?: (s: string) => string;
  };
  if (g.Buffer) return g.Buffer.from(bytes).toString("base64");
  if (g.btoa) {
    let binary = "";
    for (const b of bytes) binary += String.fromCharCode(b);
    return g.btoa(binary);
  }
  throw new Error("AudioClient: no base64 encoder available (need Buffer or btoa)");
}

function numberFromWire(v: number | string | undefined): number {
  if (typeof v === "number") return v;
  if (typeof v === "string" && v) {
    const n = Number(v);
    return Number.isFinite(n) ? n : 0;
  }
  return 0;
}

// Convenience alias matching the Connection.dial naming.
export const dialAudio = AudioClient.dial.bind(AudioClient);
