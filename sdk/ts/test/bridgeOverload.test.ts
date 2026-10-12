// What a call does when the bridge refuses it because its queue is full.
//
// The dispatcher finds the call a reply belongs to by one thing: the reply's
// correlateTo, the message id the dispatcher gave the call. A frame that names no
// pending message goes to the event listeners, and the call it was meant for stays
// pending until its caller gives up. The bridge's refusal for a full queue
// ("too many concurrent MemQL queries") carried only the request id, so a call it
// refused never settled. The bridge now answers by the message id (the Go test of
// the bridge, TestAnOverloadedBridgeAnswersTheRequestItRefused, drives a real bridge
// into overload and shows this frame); this test is the SDK's side of that: the
// call receives the refusal and settles with its sentence.
//
// The FakeWebSocket + handshake plumbing mirrors connection-hello.test.ts.

import { test } from "node:test";
import assert from "node:assert/strict";

import { Connection } from "../src/client/connection.js";

type Frame = {
  messageId?: string;
  clientHello?: unknown;
  executeQuery?: { requestId?: string; query?: string };
};

class FakeWebSocket {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSING = 2;
  static readonly CLOSED = 3;

  readyState = FakeWebSocket.OPEN;
  outbound: string[] = [];
  private listeners: Record<string, ((ev: unknown) => void)[]> = {};

  constructor(public url: string) {}

  addEventListener(type: string, fn: (ev: unknown) => void): void {
    (this.listeners[type] ??= []).push(fn);
  }
  removeEventListener(type: string, fn: (ev: unknown) => void): void {
    this.listeners[type] = (this.listeners[type] ?? []).filter((f) => f !== fn);
  }
  send(data: string): void {
    this.outbound.push(data);
  }
  close(): void {
    this.readyState = FakeWebSocket.CLOSED;
    this.dispatch("close", { type: "close", code: 1000, reason: "test" });
  }
  pushServer(payload: unknown): void {
    this.dispatch("message", { type: "message", data: JSON.stringify(payload) });
  }
  private dispatch(type: string, ev: unknown): void {
    for (const fn of this.listeners[type] ?? []) fn(ev);
  }
}

// nextOutbound waits for the first frame the client has written that matches.
async function nextOutbound(socket: FakeWebSocket, wanted: (frame: Frame) => boolean): Promise<Frame> {
  for (let i = 0; i < 3000; i++) {
    for (const raw of socket.outbound) {
      const frame = JSON.parse(raw) as Frame;
      if (wanted(frame)) return frame;
    }
    await new Promise((r) => setTimeout(r, 1));
  }
  throw new Error("nextOutbound: the frame never arrived");
}

async function dial(): Promise<{ conn: Connection; socket: FakeWebSocket }> {
  let socket!: FakeWebSocket;
  const dialP = Connection.dial({
    endpoint: "wss://test.local/memql/ws",
    webSocketFactory: (url) => {
      socket = new FakeWebSocket(url);
      return socket as unknown as WebSocket;
    },
  });
  const hello = await nextOutbound(await ready(() => socket), (f) => f.clientHello !== undefined);
  socket.pushServer({ correlateTo: hello.messageId, serverHello: { nodeId: "bff", version: "v1" } });
  return { conn: await dialP, socket };
}

async function ready(socket: () => FakeWebSocket | undefined): Promise<FakeWebSocket> {
  for (let i = 0; i < 3000; i++) {
    const s = socket();
    if (s) return s;
    await new Promise((r) => setTimeout(r, 1));
  }
  throw new Error("ready: the connection never opened a socket");
}

test("a call the bridge refused for overload settles with the bridge's sentence", async () => {
  const { conn, socket } = await dial();
  try {
    const call = conn.query.executeNamed("templates", "templates()");
    // What the call comes to: its error, or that it is still pending after a
    // refusal that should have settled it. The handler is attached first, so
    // nothing can reject the call unobserved.
    let giveUp: ReturnType<typeof setTimeout> | undefined;
    const outcome = Promise.race([
      call.then(
        () => "resolved",
        (err: Error) => err.message,
      ),
      new Promise<string>((resolve) => {
        giveUp = setTimeout(() => resolve("still pending: the refusal named no call"), 500);
      }),
    ]).finally(() => clearTimeout(giveUp));

    const sent = await nextOutbound(socket, (f) => f.executeQuery !== undefined);
    assert.ok(sent.messageId, "the call went out with a message id");

    // The refusal as the bridge writes it: named by the request id, and by the
    // message id the call is waiting for.
    socket.pushServer({
      messageId: "bridge-1",
      correlateTo: sent.messageId,
      queryError: {
        requestId: sent.executeQuery?.requestId,
        error: { code: "ResourceExhausted", message: "too many concurrent MemQL queries", metadata: {} },
      },
    });

    assert.equal(await outcome, "templates: too many concurrent MemQL queries");
  } finally {
    conn.close();
  }
});
