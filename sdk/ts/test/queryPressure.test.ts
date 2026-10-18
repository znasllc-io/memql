import { test } from "node:test";
import assert from "node:assert/strict";
import { Dispatcher } from "../src/client/dispatcher.js";
import { QueryClient } from "../src/client/query.js";

type Frame = { messageId: string; executeQuery?: { requestId: string; query: string }; cancelRequest?: { requestId: string } };
class Socket extends EventTarget {
  readyState = 1;
  sent: Frame[] = [];
  send(raw: string) { this.sent.push(JSON.parse(raw) as Frame); }
  reply(frame: Frame) {
    this.dispatchEvent(new MessageEvent("message", { data: JSON.stringify({
      correlateTo: frame.messageId, queryResult: { requestId: frame.executeQuery?.requestId, done: true, result: [] },
    }) }));
  }
  queries() { return this.sent.filter(f => f.executeQuery); }
}
function setup() {
  const socket = new Socket();
  const dispatcher = new Dispatcher({ socket: socket as unknown as WebSocket, supervised: true });
  return { socket, dispatcher, query: new QueryClient(dispatcher) };
}

test("all query clients sharing a connection admit only four server queries", async () => {
  const { socket, dispatcher, query } = setup();
  const other = new QueryClient(dispatcher);
  const calls = Array.from({ length: 40 }, (_, i) => (i % 2 ? query : other).executeNamed("read", `read(${i})`));
  const outcomes = Promise.allSettled(calls);
  try {
    assert.equal(socket.queries().length, 4, "a burst must wait in the client, not fill the bridge queue");
    for (let i = 0; i < 40; i++) {
      const frame = socket.queries()[i];
      assert.ok(frame);
      socket.reply(frame);
      await Promise.resolve();
      assert.ok(socket.queries().length - i - 1 <= 4);
    }
    assert.ok((await outcomes).every(v => v.status === "fulfilled"));
  } finally { dispatcher.stop(); await outcomes; }
});

test("cancelling a sent query cancels server work and holds admission until it settles", async () => {
  const { socket, dispatcher, query } = setup();
  const controllers = Array.from({ length: 5 }, () => new AbortController());
  const calls = controllers.map((controller, i) => query.executeNamed("read", `read(${i})`, { signal: controller.signal }).catch(e => e));
  try {
    controllers[0]!.abort();
    assert.match(String(await calls[0]), /aborted/);
    assert.deepEqual(socket.sent.filter(f => f.cancelRequest).map(f => f.cancelRequest?.requestId), [socket.queries()[0]!.executeQuery!.requestId]);
    assert.equal(socket.queries().length, 4, "aborting locally does not mean the server has stopped");
    const events: unknown[] = [];
    dispatcher.addEventListener(m => events.push(m));
    socket.reply(socket.queries()[0]!);
    await Promise.resolve();
    assert.equal(socket.queries().length, 5);
    assert.equal(events.length, 0, "the abandoned reply still belongs to its request");
  } finally { dispatcher.stop(); await Promise.all(calls); }
});

test("repeated invalidations cancel queued reads before they reach the server", async () => {
  const { socket, dispatcher, query } = setup();
  const calls = Array.from({ length: 4 }, () => query.executeRaw("hold()").catch(e => e));
  try {
    for (let i = 0; i < 200; i++) {
      const controller = new AbortController();
      const stale = query.executeRaw("refresh()", { signal: controller.signal }).catch(e => e);
      controller.abort();
      await stale;
    }
    assert.equal(socket.queries().length, 4, "cancelled refreshes must not accumulate server work");
    assert.equal(socket.sent.filter(f => f.cancelRequest).length, 0, "unsent queries need no server cancellation");
  } finally { dispatcher.stop(); await Promise.all(calls); }
});

test("disconnect rejects queued calls without replaying them on the replacement transport", async () => {
  const { socket, dispatcher, query } = setup();
  const calls = Array.from({ length: 10 }, () => query.executeRaw("writeOnce()").catch(e => e));
  socket.readyState = 3;
  socket.dispatchEvent(new Event("close"));
  assert.ok((await Promise.all(calls)).every(e => e instanceof Error));
  const replacement = new Socket();
  dispatcher.rebind(replacement as unknown as WebSocket);
  assert.equal(replacement.sent.length, 0);
  const fresh = query.executeRaw("fresh()");
  replacement.reply(replacement.queries()[0]!);
  await fresh;
  dispatcher.stop();
});

test("sustained background refreshes leave capacity for actions and control messages", async () => {
  const { socket, dispatcher, query } = setup();
  const calls = Array.from({ length: 30 }, () => query.executeRaw("refresh()", { priority: "background" }).catch(e => e));
  try {
    assert.equal(socket.queries().length, 2);
    const action = query.executeRaw("saveOnce()");
    calls.push(action);
    const frame = socket.queries().at(-1)!;
    assert.equal(frame.executeQuery?.query, "saveOnce()", "an action must not wait behind the refresh backlog");
    socket.reply(frame);
    await action;
    const control = dispatcher.sendAndWait({ conceptsList: {} }).catch(e => e);
    calls.push(control);
    assert.ok(socket.sent.some(f => "conceptsList" in f), "control traffic bypasses query admission");
  } finally { dispatcher.stop(); await Promise.all(calls); }
});

test("a runaway producer has a bounded client queue and never overfills the wire", async () => {
  const { socket, dispatcher, query } = setup();
  const calls = Array.from({ length: 68 }, () => query.executeRaw("read()").catch(e => e));
  try {
    const refusal = await query.executeRaw("excess()").catch(e => e);
    assert.match(String(refusal), /Too many queries are waiting/);
    assert.equal(socket.queries().length, 4);
  } finally { dispatcher.stop(); await Promise.all(calls); }
});
