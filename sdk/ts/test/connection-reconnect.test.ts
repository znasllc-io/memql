// SDK-owned auto-reconnect with resubscribe (memql#4537).
//
// The connection used to be one-shot: dial was a static, close was terminal,
// and stop() just cleared the handler map. Every consumer papered over that
// differently -- the portal with a manual Retry button whose resubscription
// worked only because a React effect re-ran on a new manager identity. Every
// second of a drop was a window where events were lost and half the live
// surfaces silently kept stale rows.
//
// What these tests pin is the contract, not the timing: a drop is recovered
// without the consumer doing anything, subscriptions come back on the new
// stream with their original ids, the cycle notification fires AFTER the
// replay, and a deliberate close never reconnects.

import test from "node:test";
import assert from "node:assert/strict";

import { Connection, backoffDelayMs } from "../src/client/connection.js";
import type { ConnectionStatusEvent } from "../src/client/connection.js";
import type { ServerMessage } from "../src/client/wire.js";

type Frame = { messageId?: string } & Record<string, unknown>;

class FakeWebSocket {
  static readonly OPEN = 1;
  readonly CONNECTING = 0;
  readonly OPEN = 1;
  readonly CLOSING = 2;
  readonly CLOSED = 3;

  readyState = 0;
  binaryType: "blob" | "arraybuffer" = "blob";
  readonly outbound: string[] = [];
  private listeners: Record<string, Set<(ev: unknown) => void>> = {};

  constructor(readonly url: string) {
    queueMicrotask(() => {
      this.readyState = 1;
      this.dispatch("open", { type: "open" });
    });
  }
  addEventListener(type: string, fn: (ev: unknown) => void): void {
    (this.listeners[type] ??= new Set()).add(fn);
  }
  removeEventListener(type: string, fn: (ev: unknown) => void): void {
    this.listeners[type]?.delete(fn);
  }
  dispatchEvent(): boolean {
    return true;
  }
  send(data: string): void {
    this.outbound.push(data);
  }
  close(): void {
    if (this.readyState >= 2) return;
    this.readyState = 3;
    this.dispatch("close", { type: "close", code: 1000, reason: "" });
  }
  // drop simulates the server / network going away, which is a close the
  // client did not ask for.
  drop(): void {
    this.readyState = 3;
    this.dispatch("close", { type: "close", code: 1006, reason: "abnormal" });
  }
  pushServer(payload: unknown): void {
    this.dispatch("message", { type: "message", data: JSON.stringify(payload) });
  }
  frames(): Frame[] {
    return this.outbound.map((f) => JSON.parse(f) as Frame);
  }
  private dispatch(type: string, ev: unknown): void {
    for (const fn of this.listeners[type] ?? []) fn(ev);
  }
}

(globalThis as unknown as { WebSocket: typeof FakeWebSocket }).WebSocket = FakeWebSocket;

// A scripted dialer: every dial produces a socket the test can reach, and the
// harness answers the handshake automatically so a reconnect completes without
// a live server.
function scriptedDialer(): { sockets: FakeWebSocket[]; factory: (url: string) => WebSocket } {
  const sockets: FakeWebSocket[] = [];
  const factory = (url: string): WebSocket => {
    const s = new FakeWebSocket(url);
    sockets.push(s);
    void answerHandshake(s);
    return s as unknown as WebSocket;
  };
  return { sockets, factory };
}

async function answerHandshake(socket: FakeWebSocket): Promise<void> {
  for (let i = 0; i < 2000; i++) {
    for (const frame of socket.frames()) {
      if (frame["clientHello"] !== undefined) {
        socket.pushServer({
          correlateTo: frame.messageId,
          serverHello: { nodeId: "n1", version: "v1" },
        } as unknown as ServerMessage);
        return;
      }
    }
    await new Promise((r) => setTimeout(r, 1));
  }
}

async function waitFor(predicate: () => boolean, what: string): Promise<void> {
  for (let i = 0; i < 3000; i++) {
    if (predicate()) return;
    await new Promise((r) => setTimeout(r, 1));
  }
  throw new Error(`timed out waiting for ${what}`);
}

function subscribeFrames(socket: FakeWebSocket): Array<{ subscriptionId: string; concept?: string }> {
  return socket
    .frames()
    .map((f) => f["subscribe"] as { subscriptionId: string; concept?: string } | undefined)
    .filter((s): s is { subscriptionId: string; concept?: string } => s !== undefined);
}

// ---------------------------------------------------------------------
// backoff -- pure, so it is pinned without a clock
// ---------------------------------------------------------------------

test("backoff is exponential, capped, and drawn with FULL jitter", () => {
  // Full jitter: uniform over [0, capped). The upper bound doubles per
  // attempt until the ceiling; the lower bound stays 0, which is what
  // decorrelates a fleet all dropped by one node restart.
  assert.equal(backoffDelayMs(0, 1000, 30_000, () => 0.999), 999);
  assert.equal(backoffDelayMs(1, 1000, 30_000, () => 0.999), 1998);
  assert.equal(backoffDelayMs(5, 1000, 30_000, () => 0.999), 29_970);
  assert.equal(backoffDelayMs(50, 1000, 30_000, () => 0.999), 29_970, "capped, not overflowed");
  assert.equal(backoffDelayMs(3, 1000, 30_000, () => 0), 0, "the draw can be immediate");
});

// ---------------------------------------------------------------------
// the loop
// ---------------------------------------------------------------------

test("a dropped stream reconnects and replays every subscription", { timeout: 8000 }, async () => {
  const { sockets, factory } = scriptedDialer();
  const conn = await Connection.dial({
    endpoint: "wss://example.test/memql/ws",
    webSocketFactory: factory,
    reconnect: { enabled: true, initialDelayMs: 1, maxDelayMs: 2 },
  });
  try {
    const received: string[] = [];
    conn.subscriptions.subscribeGraph((ev) => received.push(String(ev.payload?.["id"] ?? "")), {
      concept: "v1:worker:registration",
    });
    conn.subscriptions.subscribeGraph(() => {}, { concept: "v1:workbench:workspace" });

    const first = sockets[0]!;
    const opened = subscribeFrames(first);
    assert.equal(opened.length, 2, "both subscriptions went out on the first stream");

    let cycles = 0;
    conn.onConnectionCycle(() => cycles++);

    first.drop();
    await waitFor(() => sockets.length === 2, "a redial");
    const second = sockets[1]!;
    await waitFor(() => subscribeFrames(second).length === 2, "the replay");

    // Same ids, so the handler map is still valid on the new stream.
    assert.deepEqual(
      subscribeFrames(second).map((s) => s.subscriptionId).sort(),
      opened.map((s) => s.subscriptionId).sort(),
    );
    assert.deepEqual(
      subscribeFrames(second).map((s) => s.concept).sort(),
      ["v1:workbench:workspace", "v1:worker:registration"],
    );

    await waitFor(() => cycles === 1, "the connection-cycle notification");
    assert.equal(conn.status, "connected");

    // And the handlers registered before the drop still fire on the new stream.
    const subId = opened[0]!.subscriptionId;
    second.pushServer({
      event: { subscriptionId: subId, kind: "EVENT_KIND_NODE_CREATED", payload: { id: "after" } },
    } as unknown as ServerMessage);
    assert.deepEqual(received, ["after"]);
  } finally {
    conn.close();
  }
});

test("the cycle notification fires AFTER the replay, never before", { timeout: 8000 }, async () => {
  // A store re-seeds on this notification. If it fired first, the re-seed's
  // read would race its own subscription -- the read-then-subscribe hole the
  // ordering contract exists to close (memql#4536).
  const { sockets, factory } = scriptedDialer();
  const conn = await Connection.dial({
    endpoint: "wss://example.test/memql/ws",
    webSocketFactory: factory,
    reconnect: { enabled: true, initialDelayMs: 1, maxDelayMs: 2 },
  });
  try {
    conn.subscriptions.subscribeGraph(() => {}, { concept: "v1:worker:registration" });
    let replayedAtCycle = -1;
    conn.onConnectionCycle(() => {
      replayedAtCycle = subscribeFrames(sockets[1]!).length;
    });
    sockets[0]!.drop();
    await waitFor(() => replayedAtCycle >= 0, "the cycle notification");
    assert.equal(replayedAtCycle, 1, "the subscription was already on the wire");
  } finally {
    conn.close();
  }
});

test("status transitions are observable and done() waits for FINAL close", { timeout: 8000 }, async () => {
  const { sockets, factory } = scriptedDialer();
  const conn = await Connection.dial({
    endpoint: "wss://example.test/memql/ws",
    webSocketFactory: factory,
    reconnect: { enabled: true, initialDelayMs: 1, maxDelayMs: 2 },
  });
  const seen: ConnectionStatusEvent[] = [];
  conn.onStatusChange((ev) => seen.push({ ...ev }));
  assert.equal(seen[0]?.status, "connected", "a late subscriber is told the current state");

  let doneResolved = false;
  void conn.done().then(() => {
    doneResolved = true;
  });

  sockets[0]!.drop();
  await waitFor(() => seen.some((e) => e.status === "reconnecting"), "a reconnecting status");
  await waitFor(() => sockets.length === 2 && conn.status === "connected", "recovery");

  // The whole point: a transport drop the SDK recovers from is NOT the end of
  // the connection, and telling consumers it was is how a self-healing stream
  // still produced a disconnected screen.
  await new Promise((r) => setTimeout(r, 20));
  assert.equal(doneResolved, false, "done() must not fire on a recovered drop");

  conn.close();
  await waitFor(() => doneResolved, "done() after a deliberate close");
  assert.equal(conn.status, "disconnected");
});

test("a deliberate close never reconnects", { timeout: 8000 }, async () => {
  const { sockets, factory } = scriptedDialer();
  const conn = await Connection.dial({
    endpoint: "wss://example.test/memql/ws",
    webSocketFactory: factory,
    reconnect: { enabled: true, initialDelayMs: 1, maxDelayMs: 2 },
  });
  conn.close();
  await new Promise((r) => setTimeout(r, 30));
  assert.equal(sockets.length, 1, "close() must not produce a redial");
  assert.equal(conn.status, "disconnected");
});

test("an exhausted attempt budget ends the connection rather than retrying forever", { timeout: 8000 }, async () => {
  // A dialer that fails every dial after the first: the socket never opens.
  const sockets: FakeWebSocket[] = [];
  let live = true;
  const factory = (url: string): WebSocket => {
    const s = new FakeWebSocket(url);
    sockets.push(s);
    if (live) void answerHandshake(s);
    else queueMicrotask(() => s.drop());
    return s as unknown as WebSocket;
  };
  const conn = await Connection.dial({
    endpoint: "wss://example.test/memql/ws",
    webSocketFactory: factory,
    reconnect: { enabled: true, initialDelayMs: 1, maxDelayMs: 2, maxAttempts: 2 },
  });
  try {
    let doneResolved = false;
    void conn.done().then(() => {
      doneResolved = true;
    });
    live = false;
    sockets[0]!.drop();
    await waitFor(() => doneResolved, "done() after the budget is spent");
    assert.equal(conn.status, "disconnected");
    assert.ok(sockets.length <= 3, `stopped retrying: ${sockets.length} dials`);
  } finally {
    conn.close();
  }
});

test("without the opt-in, nothing changes: one dial, done() on close", { timeout: 8000 }, async () => {
  const { sockets, factory } = scriptedDialer();
  const conn = await Connection.dial({
    endpoint: "wss://example.test/memql/ws",
    webSocketFactory: factory,
  });
  let doneResolved = false;
  void conn.done().then(() => {
    doneResolved = true;
  });
  sockets[0]!.drop();
  await waitFor(() => doneResolved, "done() on a drop with reconnect off");
  assert.equal(sockets.length, 1, "no redial without the opt-in");
});
