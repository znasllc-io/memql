// pushToTalk's first tests (memql#4747). The streaming-transcription
// client shipped with none, and the two behaviours pinned here are the
// ones whose absence is SILENT rather than loud:
//
//  - a refused session must REJECT. Every Start precondition the server
//    checks answers with a correlated queryError, and the commonest of
//    them ("streaming transcription is not configured") is what a cluster
//    with no voice node returns. Without the branch the caller awaits a
//    promise that never settles, which renders as a microphone that stays
//    lit forever.
//  - a finished session must stop READING the caller's stream. The reply
//    listener unregistering does not end the pump, so a session that
//    aborted or was refused went on sending chunks -- in a browser, a
//    microphone still being read after the person let go.
//
// Run via `npm test` in sdk/ts (compiles to dist-test/, then node --test).
//
// Every case carries an explicit timeout. The regression they exist to catch
// is a promise that never settles, and an unbounded `assert.rejects` against
// one hangs the suite instead of failing it -- a red build that never goes
// red is the same as no test at all.

import test from "node:test";
import assert from "node:assert/strict";

import { pushToTalk } from "../src/voice/pushToTalk.js";
import type { Dispatcher } from "../src/client/dispatcher.js";
import type { ClientMessage, ServerMessage } from "../src/client/wire.js";

class MockDispatcher {
  readonly sent: ClientMessage[] = [];
  private streams = new Map<string, (msg: ServerMessage) => void>();
  private nextId = 0;

  send(msg: ClientMessage): string {
    const id = msg.messageId ?? `mock-${this.nextId++}`;
    this.sent.push({ ...msg, messageId: id });
    return id;
  }

  async sendAndWait(): Promise<ServerMessage> {
    throw new Error("pushToTalk must not use sendAndWait");
  }

  registerStream(requestId: string, handler: (msg: ServerMessage) => void): () => void {
    this.streams.set(requestId, handler);
    return () => {
      if (this.streams.get(requestId) === handler) this.streams.delete(requestId);
    };
  }

  /** The request id the session minted, read off the Start envelope. */
  requestId(): string {
    for (const msg of this.sent) {
      const start = (msg as unknown as Record<string, { requestId?: string }>)
        .aiTranscribeStreamStart;
      if (start?.requestId) return start.requestId;
    }
    throw new Error("MockDispatcher: no Start sent");
  }

  frame(payload: Record<string, unknown>): void {
    const handler = this.streams.get(this.requestId());
    if (!handler) throw new Error("MockDispatcher: no listener registered");
    handler(payload as unknown as ServerMessage);
  }

  /** Envelope kinds sent, in order -- e.g. ["aiTranscribeStreamStart", ...]. */
  kinds(): string[] {
    return this.sent.map((msg) => {
      const keys = Object.keys(msg).filter((k) => k !== "messageId");
      return keys[0] ?? "";
    });
  }

  listenerCount(): number {
    return this.streams.size;
  }
}

/** A stream that never ends on its own -- the shape a live microphone has. */
function openMicStream(): {
  stream: ReadableStream<Uint8Array>;
  push: (bytes: Uint8Array) => void;
  cancelled: () => boolean;
} {
  let controller: ReadableStreamDefaultController<Uint8Array> | null = null;
  let wasCancelled = false;
  const stream = new ReadableStream<Uint8Array>({
    start(c) {
      controller = c;
    },
    cancel() {
      wasCancelled = true;
    },
  });
  return {
    stream,
    push: (bytes) => controller?.enqueue(bytes),
    cancelled: () => wasCancelled,
  };
}

/** Let the microtask/macrotask queue drain so the pump can run. */
const settle = () => new Promise((resolve) => setTimeout(resolve, 0));

test("a refused session rejects with the server's own sentence", { timeout: 5000 }, async () => {
  const dispatcher = new MockDispatcher();
  const mic = openMicStream();

  const session = pushToTalk(dispatcher as unknown as Dispatcher, mic.stream, {
    audio: { encoding: "pcm16", sampleRate: 16000, channels: 1 },
  });
  await settle();

  dispatcher.frame({
    queryError: {
      requestId: dispatcher.requestId(),
      error: { message: "streaming transcription is not configured" },
    },
  });

  await assert.rejects(session, /streaming transcription is not configured/);
});

test("a refused session stops reading the microphone", { timeout: 5000 }, async () => {
  const dispatcher = new MockDispatcher();
  const mic = openMicStream();

  const session = pushToTalk(dispatcher as unknown as Dispatcher, mic.stream, {
    audio: { encoding: "pcm16", sampleRate: 16000, channels: 1 },
  });
  await settle();
  assert.equal(mic.cancelled(), false, "the pump reads while the session is open");

  dispatcher.frame({
    queryError: { requestId: dispatcher.requestId(), error: { message: "no" } },
  });
  await assert.rejects(session);
  await settle();

  assert.equal(mic.cancelled(), true, "the audio source is cancelled when the session ends");
  assert.equal(dispatcher.listenerCount(), 0, "the reply listener is unregistered");
});

test("an aborted session stops reading the microphone", { timeout: 5000 }, async () => {
  const dispatcher = new MockDispatcher();
  const mic = openMicStream();
  const abort = new AbortController();

  const session = pushToTalk(dispatcher as unknown as Dispatcher, mic.stream, {
    audio: { encoding: "pcm16", sampleRate: 16000, channels: 1 },
    signal: abort.signal,
  });
  await settle();

  abort.abort();
  await assert.rejects(session, /aborted/);
  await settle();

  assert.equal(mic.cancelled(), true);
  assert.ok(dispatcher.kinds().includes("aiTranscribeStreamEnd"), "the server is told to cancel");
});

test("partials stream and Complete resolves the final transcript", { timeout: 5000 }, async () => {
  const dispatcher = new MockDispatcher();
  const mic = openMicStream();
  const partials: string[] = [];

  const session = pushToTalk(dispatcher as unknown as Dispatcher, mic.stream, {
    audio: { encoding: "pcm16", sampleRate: 16000, channels: 1 },
    onPartial: (p) => partials.push(p.text),
  });
  await settle();

  mic.push(new Uint8Array([1, 2, 3, 4]));
  await settle();

  const requestId = dispatcher.requestId();
  // Deltas carry the FULL accumulated text, never an increment.
  dispatcher.frame({ aiTranscribeStreamDelta: { requestId, text: "open" } });
  dispatcher.frame({ aiTranscribeStreamDelta: { requestId, text: "open the fleet" } });
  dispatcher.frame({
    aiTranscribeStreamComplete: {
      requestId,
      text: "open the fleet",
      durationMs: "1200",
      provider: "openai-realtime",
    },
  });

  const final = await session;
  assert.deepEqual(partials, ["open", "open the fleet"]);
  assert.equal(final.text, "open the fleet");
  assert.equal(final.durationMs, 1200, "durationMs arrives as a protojson string");
  assert.equal(final.provider, "openai-realtime");
  assert.ok(dispatcher.kinds().includes("aiTranscribeStreamChunk"), "audio was pumped");
});

test("the Start envelope declares the format the wire expects", { timeout: 5000 }, async () => {
  const dispatcher = new MockDispatcher();
  const mic = openMicStream();
  const abort = new AbortController();

  const session = pushToTalk(dispatcher as unknown as Dispatcher, mic.stream, {
    audio: { encoding: "pcm16", sampleRate: 16000, channels: 1 },
    signal: abort.signal,
  });
  await settle();

  const start = (dispatcher.sent[0] as unknown as Record<string, Record<string, unknown>>)
    .aiTranscribeStreamStart;
  assert.equal(start?.format, "pcm16");
  assert.equal(start?.sampleRate, 16000);
  assert.equal(start?.channels, 1);

  abort.abort();
  await assert.rejects(session);
});
