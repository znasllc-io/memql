import test from "node:test";
import assert from "node:assert/strict";

import { PackClient } from "../src/pack/pack.js";
import type { Dispatcher } from "../src/client/dispatcher.js";
import type { ClientMessage, ServerMessage } from "../src/client/wire.js";

// Local stand-in, as every client test in this directory keeps its own.
class MockDispatcher {
  readonly sent: Array<{ msg: ClientMessage; messageId: string }> = [];
  private pendingReplies = new Map<string, (msg: ServerMessage) => void>();
  private nextId = 0;

  send(msg: ClientMessage): string {
    const id = msg.messageId ?? `mock-${this.nextId++}`;
    this.sent.push({ msg: { ...msg, messageId: id }, messageId: id });
    return id;
  }

  async sendAndWait(msg: ClientMessage): Promise<ServerMessage> {
    const id = msg.messageId ?? `mock-${this.nextId++}`;
    this.sent.push({ msg: { ...msg, messageId: id }, messageId: id });
    return new Promise<ServerMessage>((resolve) => {
      this.pendingReplies.set(id, resolve);
    });
  }

  registerStream(_requestId: string, _handler: (msg: ServerMessage) => void): () => void {
    return () => {};
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
    const last = this.sent.at(-1);
    if (!last) throw new Error("MockDispatcher.lastSent: nothing sent yet");
    return last.msg;
  }
}

function newClient(): { mock: MockDispatcher; client: PackClient } {
  const mock = new MockDispatcher();
  return { mock, client: new PackClient(mock as unknown as Dispatcher) };
}

test("listDomains sends listPackDomains with a requestId and maps the reply", async () => {
  const { mock, client } = newClient();
  const pending = client.listDomains();
  const sent = mock.lastSent() as unknown as Record<string, { requestId?: string }>;
  assert.ok(sent.listPackDomains, "envelope must carry a listPackDomains payload");
  assert.ok((sent.listPackDomains.requestId ?? "").length > 0);
  mock.reply({
    listPackDomainsResult: {
      requestId: sent.listPackDomains.requestId,
      domains: [{ name: "cognition", origin: "embedded", fileCount: 7 }, { name: "acme" }],
    },
  });
  assert.deepEqual(await pending, [
    { name: "cognition", origin: "embedded", fileCount: 7 },
    { name: "acme", origin: "", fileCount: 0 },
  ]);
});

test("listFiles carries the domain and coerces an int64 size from string or number", async () => {
  const { mock, client } = newClient();
  const pending = client.listFiles("cognition");
  const sent = mock.lastSent() as unknown as Record<string, { requestId?: string; domain?: string }>;
  assert.equal(sent.listPackFiles?.domain, "cognition");
  mock.reply({
    listPackFilesResult: {
      requestId: sent.listPackFiles?.requestId,
      domain: "cognition",
      files: [{ path: "queries.memql", size: "1204" }, { path: "prompts/x.tmpl", size: 33 }, { path: "shapes.memql" }],
    },
  });
  assert.deepEqual(await pending, [
    { path: "queries.memql", size: 1204 },
    { path: "prompts/x.tmpl", size: 33 },
    { path: "shapes.memql", size: 0 },
  ]);
});

test("readFile carries domain and path and returns the source with its origin", async () => {
  const { mock, client } = newClient();
  const pending = client.readFile("cognition", "queries.memql");
  const sent = mock.lastSent() as unknown as Record<string, { requestId?: string; domain?: string; path?: string }>;
  assert.equal(sent.readPackFile?.domain, "cognition");
  assert.equal(sent.readPackFile?.path, "queries.memql");
  mock.reply({
    readPackFileResult: {
      requestId: sent.readPackFile?.requestId,
      domain: "cognition",
      path: "queries.memql",
      source: "query space spaces {}\n",
      origin: "embedded",
      found: true,
    },
  });
  assert.deepEqual(await pending, {
    domain: "cognition",
    path: "queries.memql",
    source: "query space spaces {}\n",
    origin: "embedded",
    found: true,
  });
});

// A missing file is a normal answer, not an error: the engine replies
// found=false with no wire error, exactly as sdk/go/pack documents.
test("readFile resolves found=false rather than throwing for a missing file", async () => {
  const { mock, client } = newClient();
  const pending = client.readFile("cognition", "nope.memql");
  const sent = mock.lastSent() as unknown as Record<string, { requestId?: string }>;
  mock.reply({ readPackFileResult: { requestId: sent.readPackFile?.requestId, domain: "cognition", path: "nope.memql" } });
  assert.deepEqual(await pending, { domain: "cognition", path: "nope.memql", source: "", origin: "", found: false });
});

test("a queryError reply throws with the engine's message, naming the call", async () => {
  const { mock, client } = newClient();
  const pending = client.listFiles("cognition");
  mock.reply({ queryError: { requestId: "r", error: { message: "not permitted" } } });
  await assert.rejects(pending, /listFiles: not permitted/);
});

test("an unexpected reply envelope throws rather than resolving empty", async () => {
  const { mock, client } = newClient();
  const pending = client.listDomains();
  mock.reply({ listConstructsResult: { requestId: "r", constructs: [] } });
  await assert.rejects(pending, /listDomains: unexpected reply envelope/);
});
