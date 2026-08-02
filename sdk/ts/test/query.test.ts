// Mock-dispatcher tests for the keyset-cursor surface on QueryClient
// (memql#1985 / 5.12). The READ-back side (Result.meta()?.cursor) was
// already exposed; these lock in the INBOUND side -- executeNamed /
// executeRaw forwarding opts.cursor onto the ExecuteQueryMsg.cursor wire
// field -- so the frontend "load more" path (memql#1974 / 5.10) can walk a
// paginated set with no dup / no gap.

import test from "node:test";
import assert from "node:assert/strict";

import { QueryClient } from "../src/client/query.js";
import type { Dispatcher } from "../src/client/dispatcher.js";
import type {
  ClientMessage,
  ServerMessage,
  ExecuteQueryPayload,
} from "../src/client/wire.js";

class MockDispatcher {
  readonly sent: Array<{ msg: ClientMessage; messageId: string }> = [];
  private pendingReplies = new Map<string, (msg: ServerMessage) => void>();
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
    });
  }

  addEventListener(_handler: (msg: ServerMessage) => void): () => void {
    return () => {};
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

  lastExecuteQuery(): ExecuteQueryPayload {
    const msg = this.sent.at(-1)!.msg as unknown as {
      executeQuery?: ExecuteQueryPayload;
    };
    if (!msg.executeQuery) throw new Error("last message was not an executeQuery");
    return msg.executeQuery;
  }

  asDispatcher(): Dispatcher {
    return this as unknown as Dispatcher;
  }
}

// ---------------------------------------------------------------------
// Inbound cursor: executeNamed forwards opts.cursor onto the wire
// ---------------------------------------------------------------------

test("executeNamed -- forwards opts.cursor onto ExecuteQueryMsg.cursor", async () => {
  const mock = new MockDispatcher();
  const qc = new QueryClient(mock.asDispatcher());

  const promise = qc.executeNamed("spaceUtterances", 'spaceUtterances({"partitionId":"s1"})', {
    cursor: "opaque-cursor-token",
  });
  mock.reply({
    queryResult: {
      requestId: "r1",
      result: { data: [{ id: "u1" }], meta: { cursor: "next-page" } },
    },
  });
  const result = await promise;

  // INBOUND: the cursor rode the request.
  assert.equal(mock.lastExecuteQuery().cursor, "opaque-cursor-token");
  // READ-BACK: the response cursor is exposed via meta().
  assert.equal(result.meta()?.cursor, "next-page");
  assert.equal(result.rows().length, 1);
});

test("executeNamed -- omits the cursor field entirely when unset (first page)", async () => {
  const mock = new MockDispatcher();
  const qc = new QueryClient(mock.asDispatcher());

  const promise = qc.executeNamed("queryActiveSpaces", "queryActiveSpaces({})");
  mock.reply({
    queryResult: { requestId: "r1", result: { data: [], meta: {} } },
  });
  await promise;

  const payload = mock.lastExecuteQuery();
  assert.equal("cursor" in payload, false, "cursor must be absent on a first-page request");
});

test("executeNamed -- omits the cursor field for an empty-string cursor", async () => {
  const mock = new MockDispatcher();
  const qc = new QueryClient(mock.asDispatcher());

  const promise = qc.executeNamed("queryActiveSpaces", "queryActiveSpaces({})", { cursor: "" });
  mock.reply({
    queryResult: { requestId: "r1", result: { data: [] } },
  });
  await promise;

  assert.equal("cursor" in mock.lastExecuteQuery(), false);
});

test("executeNamed -- exhausted page mints no nextCursor (meta cursor empty)", async () => {
  const mock = new MockDispatcher();
  const qc = new QueryClient(mock.asDispatcher());

  const promise = qc.executeNamed("spaceUtterances", 'spaceUtterances({"partitionId":"s1"})', {
    cursor: "last-page-cursor",
  });
  mock.reply({
    queryResult: { requestId: "r1", result: { data: [{ id: "u9" }], meta: { cursor: "", hasMore: false } } },
  });
  const result = await promise;

  assert.equal(mock.lastExecuteQuery().cursor, "last-page-cursor");
  assert.equal(result.meta()?.cursor, "");
  assert.equal(result.meta()?.hasMore, false);
});

// ---------------------------------------------------------------------
// executeRaw mirrors the same inbound-cursor wiring
// ---------------------------------------------------------------------

test("executeRaw -- forwards opts.cursor onto the wire", async () => {
  const mock = new MockDispatcher();
  const qc = new QueryClient(mock.asDispatcher());

  const promise = qc.executeRaw('paginate(sort(spaceUtterances({"partitionId":"s1"}), "createdAt", "desc"), 50)', {
    cursor: "raw-cursor",
  });
  mock.reply({
    queryResult: { requestId: "r1", result: { data: [] } },
  });
  await promise;

  assert.equal(mock.lastExecuteQuery().cursor, "raw-cursor");
});
