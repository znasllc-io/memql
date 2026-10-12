// Mock-dispatcher tests for the keyset-cursor surface on QueryClient
// (memql#1985 / 5.12). The READ-back side (Result.meta()?.cursor) was
// already exposed; these lock in the INBOUND side -- executeNamed /
// executeRaw forwarding opts.cursor onto the ExecuteQueryMsg.cursor wire
// field -- so the frontend "load more" path (memql#1974 / 5.10) can walk a
// paginated set with no dup / no gap.

import test from "node:test";
import assert from "node:assert/strict";

import { QueryClient, refusalSentence } from "../src/client/query.js";
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

// ---------------------------------------------------------------------
// refusalSentence: the sentence a person reads out of a refused call
// ---------------------------------------------------------------------
//
// A builtin's refusal reaches a client in three frames: the server wraps an
// internal engine error ("MemQL engine failed to execute query. Details: ",
// buildEngineErrorMessage in component/grpc/server.go), the Go error may open
// with its package ("compose: "), and executeNamed puts the call's name in
// front. These are the words as they really arrive.

const ENGINE = "MemQL engine failed to execute query. Details: ";

test("executeNamed -- a refusal arrives in its frames, and refusalSentence leaves the sentence", async () => {
  const mock = new MockDispatcher();
  const qc = new QueryClient(mock.asDispatcher());
  const promise = qc.executeNamed("composeMaterialize", 'builtin composeMaterialize(name: "Notes 10/11", format: "markdown")');
  mock.reply({ queryError: { requestId: "r1", error: { message: ENGINE + "Enter a name without slashes." } } });
  const refused = await promise.then(() => null, (error: unknown) => error);
  assert.ok(refused instanceof Error);
  assert.equal(refused.message, "composeMaterialize: " + ENGINE + "Enter a name without slashes.");
  assert.equal(refusalSentence(refused, "composeMaterialize"), "Enter a name without slashes.");
  // The caller need not know the call: a name in front of the engine's frame can only be the call's.
  assert.equal(refusalSentence(refused), "Enter a name without slashes.");
});

test("refusalSentence -- takes off the call, the engine's frame and a Go package, once each", () => {
  const cases: [string, string | undefined, string][] = [
    ["composeMaterialize: " + ENGINE + "compose: the sources could not be read", "composeMaterialize", "the sources could not be read"],
    ["composeCancel: " + ENGINE + "compose: no composition with that id is readable by you", undefined, "no composition with that id is readable by you"],
    [ENGINE + "That folder isn't available. Choose another folder.", undefined, "That folder isn't available. Choose another folder."],
    ["libraryAddDocumentComment: the document changed; reopen it", "libraryAddDocumentComment", "the document changed; reopen it"],
    ["compose: a sentence a Go package still opens", undefined, "a sentence a Go package still opens"],
    // One package name comes off, not a second one inside the sentence.
    ["archiveComposition: " + ENGINE + "compose: library: refused", undefined, "library: refused"],
    ["  composeRunRecipe: " + ENGINE + "compose: that recipe is archived  ", "composeRunRecipe", "that recipe is archived"],
  ];
  for (const [message, construct, sentence] of cases) {
    assert.equal(refusalSentence(new Error(message), construct), sentence, message);
  }
});

test("refusalSentence -- leaves alone what is not one of its frames", () => {
  const cases: [string, string | undefined][] = [
    ["Note: the document changed", "libraryAddDocumentComment"],              // a capitalised word and a colon is a sentence
    ["the libraryAddDocumentComment: call failed", "libraryAddDocumentComment"], // not at the front
    ["libraryAddDocumentComment:no space", "libraryAddDocumentComment"],         // not how executeNamed words it
    ["composeCancel: the cluster did not answer", undefined],                    // a name, unknown, not before the engine's frame
    ["Materializer did not confirm a composition. Check its history before retrying.", "composeMaterialize"],
    ["invalid argument: composeMaterialize() statement cannot be empty", undefined], // two words: not a package
  ];
  for (const [message, construct] of cases) {
    assert.equal(refusalSentence(new Error(message), construct), message, message);
  }
  // The construct's own name comes off once, from the front.
  assert.equal(refusalSentence(new Error("libraryAddDocumentComment: libraryAddDocumentComment: twice"), "libraryAddDocumentComment"), "libraryAddDocumentComment: twice");
  // A frame with nothing inside it is the whole of what the cluster said, and is kept.
  assert.equal(refusalSentence(new Error("composeCancel: MemQL engine failed to execute query."), "composeCancel"), "MemQL engine failed to execute query.");
  // Something that is not an Error is read as text.
  assert.equal(refusalSentence(ENGINE + "compose: refused"), "refused");
});
