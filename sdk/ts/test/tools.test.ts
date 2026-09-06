// Mock-dispatcher tests for the outbound tools surface: listTools /
// callTool.

import test from "node:test";
import assert from "node:assert/strict";

import { listTools, callTool } from "../src/tools/outbound.js";
import type { Dispatcher } from "../src/client/dispatcher.js";
import type { ClientMessage, ServerMessage } from "../src/client/wire.js";

class MockDispatcher {
  readonly sent: Array<{ msg: ClientMessage; messageId: string }> = [];
  private pendingReplies = new Map<string, (msg: ServerMessage) => void>();
  private eventListeners = new Set<(msg: ServerMessage) => void>();
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

  addEventListener(handler: (msg: ServerMessage) => void): () => void {
    this.eventListeners.add(handler);
    return () => this.eventListeners.delete(handler);
  }

  registerStream(_requestId: string, _handler: (msg: ServerMessage) => void): () => void {
    return () => {};
  }

  // Test helpers
  reply(payload: Record<string, unknown>): void {
    const last = this.sent.at(-1);
    if (!last) throw new Error("MockDispatcher.reply: nothing sent yet");
    const resolver = this.pendingReplies.get(last.messageId);
    if (!resolver) throw new Error(`MockDispatcher.reply: no pending entry for ${last.messageId}`);
    this.pendingReplies.delete(last.messageId);
    resolver({ correlateTo: last.messageId, ...payload } as ServerMessage);
  }

  push(payload: Record<string, unknown>): void {
    const msg = payload as unknown as ServerMessage;
    for (const fn of this.eventListeners) fn(msg);
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

// ---------------------------------------------------------------------
// listTools
// ---------------------------------------------------------------------

test("listTools -- happy path returns the catalog + cursor", async () => {
  const mock = new MockDispatcher();
  const promise = listTools(mock.asDispatcher());
  mock.reply({
    listToolsResult: {
      requestId: mock.lastRequestId(),
      tools: [
        { name: "uiClick", description: "Click", inputSchema: "{}", scopes: ["highlight"] },
        { name: "createSpace", description: "Make a space", inputSchema: "{}", scopes: ["create"] },
      ],
      nextCursor: "page-2",
    },
  });
  const r = await promise;
  assert.equal(r.tools.length, 2);
  assert.equal(r.tools[0]!.name, "uiClick");
  assert.deepEqual(r.tools[0]!.scopes, ["highlight"]);
  assert.equal(r.nextCursor, "page-2");
});

test("listTools -- forwards cursor on follow-up page", async () => {
  const mock = new MockDispatcher();
  const promise = listTools(mock.asDispatcher(), { cursor: "page-2" });
  const sent = mock.lastSent() as unknown as { listTools?: { cursor?: string } };
  assert.equal(sent.listTools?.cursor, "page-2");
  mock.reply({ listToolsResult: { requestId: mock.lastRequestId(), tools: [], nextCursor: "" } });
  const r = await promise;
  assert.equal(r.tools.length, 0);
  assert.equal(r.nextCursor, "");
});

test("listTools -- queryError throws", async () => {
  const mock = new MockDispatcher();
  const promise = listTools(mock.asDispatcher());
  mock.reply({ queryError: { requestId: mock.lastRequestId(), error: { message: "denied" } } });
  await assert.rejects(promise, /listTools: denied/);
});

// ---------------------------------------------------------------------
// callTool
// ---------------------------------------------------------------------

test("callTool -- forwards args + returns content", async () => {
  const mock = new MockDispatcher();
  const promise = callTool(mock.asDispatcher(), {
    name: "createSpace",
    arguments: { title: "Brainstorm", architecture: "polyphon" },
  });
  const sent = mock.lastSent() as unknown as { callTool?: { name?: string; arguments?: Record<string, unknown> } };
  assert.equal(sent.callTool?.name, "createSpace");
  assert.deepEqual(sent.callTool?.arguments, { title: "Brainstorm", architecture: "polyphon" });

  mock.reply({
    callToolResult: {
      requestId: mock.lastRequestId(),
      content: [{ type: "text", text: "Created space spc-1" }],
      isError: false,
    },
  });
  const r = await promise;
  assert.equal(r.isError, false);
  assert.equal(r.content.length, 1);
  assert.equal(r.content[0]!.text, "Created space spc-1");
});

test("callTool -- isError rides the result (no throw)", async () => {
  const mock = new MockDispatcher();
  const promise = callTool(mock.asDispatcher(), { name: "x" });
  mock.reply({
    callToolResult: {
      requestId: mock.lastRequestId(),
      content: [{ type: "text", text: "permission denied" }],
      isError: true,
    },
  });
  const r = await promise;
  assert.equal(r.isError, true);
  assert.equal(r.content[0]!.text, "permission denied");
});

test("callTool -- rejects missing name", async () => {
  const mock = new MockDispatcher();
  await assert.rejects(() => callTool(mock.asDispatcher(), { name: "" }), /name is required/);
});
