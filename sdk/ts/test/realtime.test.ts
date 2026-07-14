// Mock-dispatcher + fake-WebSocket tests for the realtime media
// surface (polyphonRoomToken + AudioClient).

import test from "node:test";
import assert from "node:assert/strict";

import { polyphonRoomToken } from "../src/realtime/polyphonRoomToken.js";
import { AudioClient, dialAudio } from "../src/realtime/audio.js";
import type { Dispatcher } from "../src/client/dispatcher.js";
import type { ClientMessage, ServerMessage } from "../src/client/wire.js";

// ---------------------------------------------------------------------
// polyphonRoomToken -- rides the main dispatcher (sendAndWait)
// ---------------------------------------------------------------------

class MockDispatcher {
  readonly sent: Array<{ msg: ClientMessage; messageId: string }> = [];
  private pendingReplies = new Map<string, (msg: ServerMessage) => void>();
  private streams = new Map<string, (msg: ServerMessage) => void>();
  private nextId = 0;

  send(msg: ClientMessage): string {
    const id = msg.messageId ?? `mock-${this.nextId++}`;
    this.sent.push({ msg: { ...msg, messageId: id }, messageId: id });
    return id;
  }

  async sendAndWait(msg: ClientMessage, signal?: AbortSignal): Promise<ServerMessage> {
    const id = msg.messageId ?? `mock-${this.nextId++}`;
    this.sent.push({ msg: { ...msg, messageId: id }, messageId: id });
    return new Promise<ServerMessage>((resolve, reject) => {
      if (signal?.aborted) {
        reject(new Error("aborted"));
        return;
      }
      this.pendingReplies.set(id, resolve);
      if (signal) {
        signal.addEventListener(
          "abort",
          () => {
            this.pendingReplies.delete(id);
            reject(new Error("aborted"));
          },
          { once: true },
        );
      }
    });
  }

  registerStream(requestId: string, handler: (msg: ServerMessage) => void): () => void {
    this.streams.set(requestId, handler);
    return () => {
      if (this.streams.get(requestId) === handler) this.streams.delete(requestId);
    };
  }

  reply(payload: Record<string, unknown>): void {
    const last = this.sent.at(-1);
    if (!last) throw new Error("MockDispatcher.reply: nothing sent yet");
    const resolver = this.pendingReplies.get(last.messageId);
    if (!resolver) throw new Error(`MockDispatcher.reply: no pending entry for ${last.messageId}`);
    this.pendingReplies.delete(last.messageId);
    resolver({ correlateTo: last.messageId, ...payload } as ServerMessage);
  }

  lastSent(): ClientMessage {
    return this.sent.at(-1)!.msg;
  }

  lastRequestId(): string {
    const msg = this.lastSent() as unknown as Record<string, unknown>;
    for (const v of Object.values(msg)) {
      if (v && typeof v === "object" && "requestId" in v) {
        const rid = (v as { requestId?: string }).requestId;
        if (typeof rid === "string" && rid) return rid;
      }
    }
    throw new Error("MockDispatcher.lastRequestId: no requestId found");
  }

  asDispatcher(): Dispatcher {
    return this as unknown as Dispatcher;
  }
}

test("polyphonRoomToken -- happy path", async () => {
  const mock = new MockDispatcher();
  const promise = polyphonRoomToken(mock.asDispatcher(), {
    scopeId: "spc-1",
    participantId: "ptp-1",
    displayName: "Alice",
  });
  const sent = mock.lastSent() as unknown as {
    polyphonRoomToken?: { scopeId?: string; displayName?: string };
  };
  assert.equal(sent.polyphonRoomToken?.scopeId, "spc-1");
  assert.equal(sent.polyphonRoomToken?.displayName, "Alice");

  mock.reply({
    polyphonRoomTokenResult: {
      requestId: mock.lastRequestId(),
      token: "lk-jwt-redacted",
      roomName: "space-spc-1",
      livekitUrl: "wss://livekit.example",
      expiresAt: 1_700_000_000,
    },
  });
  const r = await promise;
  assert.equal(r.token, "lk-jwt-redacted");
  assert.equal(r.roomName, "space-spc-1");
  assert.equal(r.livekitUrl, "wss://livekit.example");
  assert.equal(r.expiresAt, 1_700_000_000);
});

test("polyphonRoomToken -- expiresAt as string decoded to number", async () => {
  const mock = new MockDispatcher();
  const promise = polyphonRoomToken(mock.asDispatcher(), {
    scopeId: "spc",
    participantId: "ptp",
    displayName: "x",
  });
  mock.reply({
    polyphonRoomTokenResult: {
      requestId: mock.lastRequestId(),
      token: "t",
      roomName: "r",
      livekitUrl: "wss://x",
      expiresAt: "1700000000",
    },
  });
  const r = await promise;
  assert.equal(r.expiresAt, 1700000000);
});

test("polyphonRoomToken -- rejects missing required args", async () => {
  const mock = new MockDispatcher();
  await assert.rejects(
    () =>
      polyphonRoomToken(mock.asDispatcher(), {
        scopeId: "",
        participantId: "ptp",
        displayName: "x",
      }),
    /scopeId is required/,
  );
});

test("polyphonRoomToken -- throws on QueryError", async () => {
  const mock = new MockDispatcher();
  const promise = polyphonRoomToken(mock.asDispatcher(), {
    scopeId: "spc",
    participantId: "ptp",
    displayName: "x",
  });
  mock.reply({ queryError: { requestId: mock.lastRequestId(), error: { message: "boom" } } });
  await assert.rejects(promise, /polyphonRoomToken: boom/);
});

// ---------------------------------------------------------------------
// AudioClient -- separate /memql/audio WebSocket
// ---------------------------------------------------------------------

// FakeWebSocket implements just enough of the DOM WebSocket interface
// for AudioClient to round-trip. Inbound frames are injected via
// `pushServer(...)`.
class FakeWebSocket {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSING = 2;
  static readonly CLOSED = 3;

  // Instance constants mirroring the static set (the DOM WebSocket
  // exposes both).
  readonly CONNECTING = 0;
  readonly OPEN = 1;
  readonly CLOSING = 2;
  readonly CLOSED = 3;

  url: string;
  readyState: number = FakeWebSocket.CONNECTING;
  binaryType: "blob" | "arraybuffer" = "blob";
  bufferedAmount = 0;
  extensions = "";
  protocol = "";
  // The DOM API exposes onmessage/onopen/onclose/onerror too; the
  // SDK only uses addEventListener. We track listeners here.
  private listeners: Record<string, Set<(ev: unknown) => void>> = {};

  // Captured outbound frames (json strings) for assertions.
  readonly outbound: string[] = [];

  constructor(url: string) {
    this.url = url;
    // Defer the "open" notification so callers have a chance to
    // attach listeners before it fires.
    queueMicrotask(() => {
      this.readyState = FakeWebSocket.OPEN;
      this.dispatch("open", { type: "open" });
    });
  }

  addEventListener(type: string, fn: (ev: unknown) => void): void {
    if (!this.listeners[type]) this.listeners[type] = new Set();
    this.listeners[type].add(fn);
  }
  removeEventListener(type: string, fn: (ev: unknown) => void): void {
    this.listeners[type]?.delete(fn);
  }
  dispatchEvent(_ev: unknown): boolean {
    return true;
  }

  send(data: string): void {
    this.outbound.push(data);
  }

  close(_code?: number, _reason?: string): void {
    if (this.readyState >= FakeWebSocket.CLOSING) return;
    this.readyState = FakeWebSocket.CLOSED;
    this.dispatch("close", { type: "close", code: _code ?? 1000, reason: _reason ?? "" });
  }

  // Test helper: simulate an inbound message.
  pushServer(payload: unknown): void {
    const data = typeof payload === "string" ? payload : JSON.stringify(payload);
    this.dispatch("message", { type: "message", data });
  }

  // Test helper: read the last sent envelope as a parsed object.
  lastSent<T = Record<string, unknown>>(): T {
    if (this.outbound.length === 0) throw new Error("FakeWebSocket: nothing sent yet");
    return JSON.parse(this.outbound[this.outbound.length - 1]!) as T;
  }

  private dispatch(type: string, ev: unknown): void {
    const set = this.listeners[type];
    if (!set) return;
    for (const fn of set) fn(ev);
  }
}

// Install a globalThis.WebSocket shim so `AudioClient.dial` can use
// the default factory. Each test gets a fresh socket by reaching
// through the singleton.
let activeSocket: FakeWebSocket | null = null;
(globalThis as unknown as { WebSocket: typeof FakeWebSocket }).WebSocket = FakeWebSocket;

async function dialTestAudio(): Promise<{
  client: AudioClient;
  socket: FakeWebSocket;
  protocols: string[] | undefined;
}> {
  // Use a factory we control so we can grab the socket reference.
  let socket: FakeWebSocket | null = null;
  let protocols: string[] | undefined;
  const client = await dialAudio({
    endpoint: "wss://test.local/memql/audio",
    auth: { bearer: "test-bearer" },
    webSocketFactory: (url, protos) => {
      socket = new FakeWebSocket(url);
      protocols = protos;
      activeSocket = socket;
      return socket as unknown as WebSocket;
    },
  });
  if (!socket) throw new Error("dialTestAudio: no socket captured");
  return { client, socket, protocols };
}

test("AudioClient.dial -- bearer travels as subprotocol, never on the URL", async () => {
  const { client, socket, protocols } = await dialTestAudio();
  // #2511: the credential rides Sec-WebSocket-Protocol, and the URL stays
  // free of live tokens.
  assert.deepEqual(protocols, ["bearer", "test-bearer"]);
  assert.ok(!socket.url.includes("test-bearer"), `url leaks the bearer: ${socket.url}`);
  assert.ok(socket.url.startsWith("wss://test.local/memql/audio"));
  client.close();
});

test("AudioClient.transcribe -- start + chunk + final transcription + ended", async () => {
  const { client, socket } = await dialTestAudio();
  const stream = client.transcribe({
    partitionId: "spc-1",
    participantId: "ptp-1",
    format: "opus",
    sampleRate: 48000,
    channels: 1,
  });
  // Initial "start" frame went out before any chunks.
  const startFrame = socket.lastSent<{ type: string; streamId: string; format: string }>();
  assert.equal(startFrame.type, "start");
  assert.equal(startFrame.format, "opus");
  assert.equal(startFrame.streamId, stream.streamId);

  // Push two chunks.
  stream.pushChunk("AAAA");
  stream.pushChunk("BBBB");
  const chunk2 = socket.lastSent<{ type: string; sequence: number; audio: string }>();
  assert.equal(chunk2.type, "chunk");
  assert.equal(chunk2.sequence, 1);
  assert.equal(chunk2.audio, "BBBB");

  // Collect events.
  const collected: Array<string> = [];
  const consume = (async () => {
    for await (const ev of stream.events) {
      collected.push(ev.isFinal ? `final:${ev.text}` : `partial:${ev.text}`);
    }
  })();

  // Server sends a partial, a final, then ended.
  socket.pushServer({
    type: "transcription",
    streamId: stream.streamId,
    text: "hello",
    isFinal: false,
    confidence: 0.7,
  });
  socket.pushServer({
    type: "transcription",
    streamId: stream.streamId,
    text: "hello world",
    isFinal: true,
    confidence: 0.92,
    utteranceId: "utt-1",
    durationMs: 1500,
    words: [{ word: "hello" }],
  });
  socket.pushServer({ type: "ended", streamId: stream.streamId });

  await consume;
  assert.deepEqual(collected, ["partial:hello", "final:hello world"]);
  client.close();
});

test("AudioClient.transcribe -- error frame surfaces as iterator throw", async () => {
  const { client, socket } = await dialTestAudio();
  const stream = client.transcribe({
    partitionId: "spc",
    participantId: "ptp",
    format: "pcm16",
    sampleRate: 16000,
    channels: 1,
  });
  socket.pushServer({
    type: "error",
    streamId: stream.streamId,
    error: { code: "stt_failed", message: "stt provider unreachable" },
  });
  await assert.rejects(
    (async () => {
      for await (const _ev of stream.events) {
        /* ignored */
      }
    })(),
    /AudioClient.transcribe: stt provider unreachable/,
  );
  client.close();
});

test("AudioClient.synthesize -- tts_started + tts_chunk + tts_ended", async () => {
  const { client, socket } = await dialTestAudio();
  const stream = client.synthesize({
    text: "Hello",
    voice: "nova",
    format: "wav",
    sampleRate: 24000,
  });
  const startFrame = socket.lastSent<{ type: string; text: string; voice: string }>();
  assert.equal(startFrame.type, "synthesize");
  assert.equal(startFrame.text, "Hello");
  assert.equal(startFrame.voice, "nova");

  const events: string[] = [];
  const consume = (async () => {
    for await (const ev of stream.events) events.push(ev.kind);
  })();

  socket.pushServer({
    type: "tts_started",
    requestId: stream.requestId,
    format: "wav",
    sampleRate: 24000,
    text: "Hello",
  });
  socket.pushServer({
    type: "tts_chunk",
    requestId: stream.requestId,
    audio: "AAAA",
    format: "wav",
    sampleRate: 24000,
    sequence: 0,
    done: false,
  });
  socket.pushServer({
    type: "tts_chunk",
    requestId: stream.requestId,
    audio: "BBBB",
    format: "wav",
    sampleRate: 24000,
    sequence: 1,
    done: true,
  });
  socket.pushServer({ type: "tts_ended", requestId: stream.requestId });

  await consume;
  assert.deepEqual(events, ["started", "chunk", "chunk", "ended"]);
  client.close();
});

test("AudioClient.transcribe -- end() sends end frame", async () => {
  const { client, socket } = await dialTestAudio();
  const stream = client.transcribe({
    partitionId: "spc",
    participantId: "ptp",
    format: "pcm16",
    sampleRate: 16000,
    channels: 1,
  });
  stream.end();
  const endFrame = socket.lastSent<{ type: string; streamId: string; cancelled?: boolean }>();
  assert.equal(endFrame.type, "end");
  assert.equal(endFrame.streamId, stream.streamId);
  assert.equal(endFrame.cancelled, undefined);
  client.close();
});

test("AudioClient.transcribe -- cancel() sends end frame with cancelled=true", async () => {
  const { client, socket } = await dialTestAudio();
  const stream = client.transcribe({
    partitionId: "spc",
    participantId: "ptp",
    format: "pcm16",
    sampleRate: 16000,
    channels: 1,
  });
  stream.cancel();
  const endFrame = socket.lastSent<{ type: string; cancelled?: boolean }>();
  assert.equal(endFrame.type, "end");
  assert.equal(endFrame.cancelled, true);
  client.close();
});

test("AudioClient -- socket close fails in-flight iterators", async () => {
  const { client, socket } = await dialTestAudio();
  const stream = client.transcribe({
    partitionId: "spc",
    participantId: "ptp",
    format: "pcm16",
    sampleRate: 16000,
    channels: 1,
  });

  // Start consuming; the iterator parks on a waiter.
  const consume = (async () => {
    for await (const _ev of stream.events) {
      /* ignored */
    }
  })();

  // Server-side socket close (e.g. backend OOM) should terminate
  // the iterator via the error path.
  socket.close(1011, "internal error");

  await assert.rejects(consume, /AudioClient: socket closed/);
  // Avoid eslint complaints about unused activeSocket.
  void activeSocket;
  client.close();
});

test("AudioClient -- untargeted error frame fails all in-flight handlers", async () => {
  const { client, socket } = await dialTestAudio();

  // Two transcribe streams + one synthesize stream in flight.
  const t1 = client.transcribe({
    partitionId: "spc",
    participantId: "ptp",
    format: "pcm16",
    sampleRate: 16000,
    channels: 1,
  });
  const t2 = client.transcribe({
    partitionId: "spc",
    participantId: "ptp",
    format: "pcm16",
    sampleRate: 16000,
    channels: 1,
  });
  const s1 = client.synthesize({ text: "hi" });

  const consumeT1 = (async () => {
    for await (const _ev of t1.events) {
      /* drain */
    }
  })();
  const consumeT2 = (async () => {
    for await (const _ev of t2.events) {
      /* drain */
    }
  })();
  const sawSynthEnd = (async () => {
    for await (const ev of s1.events) {
      if (ev.kind === "ended") return ev;
    }
    return null;
  })();

  // Server emits an error frame with NO streamId / requestId --
  // connection-level fault (auth, server shutdown, malformed
  // handshake). Every in-flight handler should learn immediately.
  socket.pushServer({
    type: "error",
    error: { code: "unauthorized", message: "bearer token expired" },
  });

  await assert.rejects(consumeT1, /AudioClient: bearer token expired/);
  await assert.rejects(consumeT2, /AudioClient: bearer token expired/);
  const synthEnd = await sawSynthEnd;
  assert.ok(synthEnd, "synthesize iterator must yield an ended event");
  assert.match(synthEnd!.error, /AudioClient: bearer token expired/);

  client.close();
});
