// Mock-dispatcher tests for the SI surface (aiChat / aiSpeech /
// aiTranscribe / aiSuggest + aiChatStream). The mock fakes
// Dispatcher's send / sendAndWait / registerStream paths so we can
// drive replies into the SDK without spinning up a real WebSocket.
//
// Run via `npm test` (compiles to dist-test/ first, then `node
// --test`).

import test from "node:test";
import assert from "node:assert/strict";

import { aiChat, aiChatStream } from "../src/ai/chat.js";
import { aiSpeech } from "../src/ai/speech.js";
import { aiTranscribe } from "../src/ai/transcribe.js";
import { aiSuggest } from "../src/ai/suggest.js";
import type { Dispatcher } from "../src/client/dispatcher.js";
import type { ClientMessage, ServerMessage } from "../src/client/wire.js";

interface SentMessage {
  msg: ClientMessage;
  messageId: string;
}

class MockDispatcher {
  readonly sent: SentMessage[] = [];
  // sendAndWait pending replies keyed by messageId.
  private pendingReplies = new Map<string, (msg: ServerMessage) => void>();
  // registerStream listeners keyed by requestId.
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

  // Drive a server-to-client correlateTo reply for the most-recently
  // sent message. Test helper, not part of the real Dispatcher
  // surface.
  reply(payload: Record<string, unknown>): void {
    const last = this.sent.at(-1);
    if (!last) throw new Error("MockDispatcher.reply: nothing sent yet");
    const resolver = this.pendingReplies.get(last.messageId);
    if (!resolver) throw new Error(`MockDispatcher.reply: no pending entry for ${last.messageId}`);
    this.pendingReplies.delete(last.messageId);
    resolver({ correlateTo: last.messageId, ...payload } as ServerMessage);
  }

  // Drive a streaming frame keyed by requestId.
  streamFrame(requestId: string, payload: Record<string, unknown>): void {
    const handler = this.streams.get(requestId);
    if (!handler) throw new Error(`MockDispatcher.streamFrame: no listener for ${requestId}`);
    handler({ correlateTo: this.sent.at(-1)?.messageId, ...payload } as ServerMessage);
  }

  // Most-recently sent envelope (after id stamping).
  lastSent(): ClientMessage {
    const last = this.sent.at(-1);
    if (!last) throw new Error("MockDispatcher.lastSent: nothing sent yet");
    return last.msg;
  }

  // Most-recent requestId visible on the sent payload, regardless of
  // which oneof slot it lives under.
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
    // The SI surface uses send / sendAndWait / registerStream only.
    return this as unknown as Dispatcher;
  }
}

test("aiChat -- one-shot round trip resolves with assistant message", async () => {
  const mock = new MockDispatcher();
  const promise = aiChat(
    mock.asDispatcher(),
    [{ role: "user", content: "Hi there" }],
    { provider: "chat54Mini" },
  );

  const sent = mock.lastSent() as unknown as { aiChat?: { messages?: unknown[]; provider?: string; stream?: boolean; requestId?: string } };
  assert.ok(sent.aiChat, "aiChat payload present");
  assert.equal(sent.aiChat?.provider, "chat54Mini");
  assert.equal(sent.aiChat?.stream, undefined, "stream flag omitted on one-shot");
  assert.equal(sent.aiChat?.messages?.length, 1);

  mock.reply({
    aiChatResult: {
      requestId: mock.lastRequestId(),
      message: { role: "assistant", content: "Hello!" },
    },
  });

  const result = await promise;
  assert.equal(result.message.role, "assistant");
  assert.equal(result.message.content, "Hello!");
  assert.equal(result.provider, "chat54Mini");
});

test("aiChat -- throws on QueryError reply", async () => {
  const mock = new MockDispatcher();
  const promise = aiChat(mock.asDispatcher(), [{ role: "user", content: "Hi" }]);
  mock.reply({ queryError: { requestId: mock.lastRequestId(), error: { message: "boom" } } });
  await assert.rejects(promise, /aiChat: boom/);
});

test("aiChat -- rejects empty messages array", async () => {
  const mock = new MockDispatcher();
  await assert.rejects(
    () => aiChat(mock.asDispatcher(), []),
    /messages array must be non-empty/,
  );
});

test("aiChat -- propagates AbortSignal", async () => {
  const mock = new MockDispatcher();
  const ac = new AbortController();
  const promise = aiChat(
    mock.asDispatcher(),
    [{ role: "user", content: "Hi" }],
    { signal: ac.signal },
  );
  ac.abort();
  await assert.rejects(promise);
});

test("aiChatStream -- chunks land in deltas iterator, final result resolves", async () => {
  const mock = new MockDispatcher();
  const handle = aiChatStream(mock.asDispatcher(), [{ role: "user", content: "Hello" }]);

  const sent = mock.lastSent() as unknown as { aiChat?: { stream?: boolean; requestId?: string } };
  assert.equal(sent.aiChat?.stream, true);
  const requestId = mock.lastRequestId();

  // Drive 3 chunks then the terminal result.
  const collected: string[] = [];
  const consume = (async () => {
    for await (const d of handle.deltas) {
      if (d.textDelta) collected.push(d.textDelta);
    }
  })();

  mock.streamFrame(requestId, { aiChunk: { requestId, index: 0, textDelta: "Hel" } });
  mock.streamFrame(requestId, { aiChunk: { requestId, index: 1, textDelta: "lo " } });
  mock.streamFrame(requestId, { aiChunk: { requestId, index: 2, textDelta: "world" } });
  mock.streamFrame(requestId, {
    aiChatResult: {
      requestId,
      message: { role: "assistant", content: "Hello world" },
    },
  });

  const result = await handle.result;
  await consume;

  assert.deepEqual(collected, ["Hel", "lo ", "world"]);
  assert.equal(result.message.content, "Hello world");
});

test("aiChatStream -- queryError surfaces on result + iterator", async () => {
  const mock = new MockDispatcher();
  const handle = aiChatStream(mock.asDispatcher(), [{ role: "user", content: "x" }]);
  const requestId = mock.lastRequestId();

  mock.streamFrame(requestId, {
    queryError: { requestId, error: { message: "provider down" } },
  });

  await assert.rejects(handle.result, /aiChatStream: provider down/);
});

test("aiSpeech -- decodes base64 audio + format", async () => {
  const mock = new MockDispatcher();
  const promise = aiSpeech(mock.asDispatcher(), "speak this", {
    voice: "alto",
    format: "wav",
  });

  const sent = mock.lastSent() as unknown as { aiSpeech?: { input?: string; voice?: string; format?: string } };
  assert.equal(sent.aiSpeech?.input, "speak this");
  assert.equal(sent.aiSpeech?.voice, "alto");

  const audioBytes = new Uint8Array([0x52, 0x49, 0x46, 0x46]); // "RIFF"
  const b64 = Buffer.from(audioBytes).toString("base64");
  mock.reply({
    aiSpeechResult: {
      requestId: mock.lastRequestId(),
      audio: b64,
      format: "wav",
    },
  });

  const result = await promise;
  assert.deepEqual(Array.from(result.audio), Array.from(audioBytes));
  assert.equal(result.format, "wav");
});

test("aiSpeech -- rejects empty input", async () => {
  const mock = new MockDispatcher();
  await assert.rejects(
    () => aiSpeech(mock.asDispatcher(), ""),
    /input string must be non-empty/,
  );
});

test("aiTranscribe -- encodes audio + returns text", async () => {
  const mock = new MockDispatcher();
  const audio = new Uint8Array([1, 2, 3, 4, 5]);
  const promise = aiTranscribe(mock.asDispatcher(), audio, { mimeType: "audio/wav" });

  const sent = mock.lastSent() as unknown as { aiTranscribe?: { audio?: string; mimeType?: string } };
  assert.equal(sent.aiTranscribe?.mimeType, "audio/wav");
  // Decode the base64-encoded audio in the sent envelope and confirm
  // it round-trips back to the original bytes.
  const decoded = Buffer.from(sent.aiTranscribe?.audio ?? "", "base64");
  assert.deepEqual(Array.from(decoded), Array.from(audio));

  mock.reply({
    aiTranscribeResult: { requestId: mock.lastRequestId(), text: "hello world" },
  });

  const result = await promise;
  assert.equal(result.text, "hello world");
});

test("aiTranscribe -- rejects empty audio", async () => {
  const mock = new MockDispatcher();
  await assert.rejects(
    () => aiTranscribe(mock.asDispatcher(), new Uint8Array(0)),
    /audio bytes must be non-empty/,
  );
});

test("aiSuggest -- forwards domain + payload, returns structured result", async () => {
  const mock = new MockDispatcher();
  const promise = aiSuggest(
    mock.asDispatcher(),
    "spaceTitle",
    { description: "a brainstorm session" },
  );

  const sent = mock.lastSent() as unknown as { aiSuggest?: { domain?: string; payload?: Record<string, unknown> } };
  assert.equal(sent.aiSuggest?.domain, "spaceTitle");
  assert.deepEqual(sent.aiSuggest?.payload, { description: "a brainstorm session" });

  mock.reply({
    aiSuggestResult: {
      requestId: mock.lastRequestId(),
      domain: "spaceTitle",
      result: { title: "Brainstorm session" },
    },
  });

  const result = await promise;
  assert.equal(result.domain, "spaceTitle");
  assert.deepEqual(result.result, { title: "Brainstorm session" });
});

test("aiSuggest -- rejects empty domain", async () => {
  const mock = new MockDispatcher();
  await assert.rejects(
    () => aiSuggest(mock.asDispatcher(), "", {}),
    /domain string must be non-empty/,
  );
});
