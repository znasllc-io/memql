// Connection dial auth carry (memql#2511): bearer and guest credentials
// travel as WebSocket subprotocols (["bearer"|"guest", token]) and never on
// the URL. The FakeWebSocket + handshake plumbing mirrors
// connection-autorotate.test.ts.

import { test } from "node:test";
import assert from "node:assert/strict";

import { Connection } from "../src/client/connection.js";

type Frame = { messageId?: string; clientHello?: unknown };

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

(globalThis as unknown as { WebSocket: typeof FakeWebSocket }).WebSocket = FakeWebSocket;

async function completeHello(socket: FakeWebSocket): Promise<void> {
  for (let i = 0; i < 3000; i++) {
    for (const raw of socket.outbound) {
      const msg = JSON.parse(raw) as Frame;
      if (msg.clientHello !== undefined) {
        socket.pushServer({
          correlateTo: msg.messageId,
          serverHello: { nodeId: "n1", version: "test" },
        });
        return;
      }
    }
    await new Promise((r) => setTimeout(r, 1));
  }
  throw new Error("completeHello: clientHello never arrived");
}

async function dialWith(auth: { bearer?: string; guestToken?: string; workerToken?: string }): Promise<{
  conn: Connection;
  socket: FakeWebSocket;
  protocols: string[] | undefined;
}> {
  let socket!: FakeWebSocket;
  let protocols: string[] | undefined;
  const dialP = Connection.dial({
    endpoint: "wss://test.local/memql/ws",
    auth,
    webSocketFactory: (url, protos) => {
      socket = new FakeWebSocket(url);
      protocols = protos;
      return socket as unknown as WebSocket;
    },
  });
  await completeHello(socket);
  const conn = await dialP;
  return { conn, socket, protocols };
}

test("Connection.dial -- bearer travels as subprotocol, never on the URL (#2511)", async () => {
  const { conn, socket, protocols } = await dialWith({ bearer: "test-bearer-jwt" });
  try {
    assert.deepEqual(protocols, ["bearer", "test-bearer-jwt"]);
    assert.ok(!socket.url.includes("test-bearer-jwt"), `url leaks the bearer: ${socket.url}`);
    assert.ok(!socket.url.includes("bearer_token"), `url carries the deprecated param: ${socket.url}`);
  } finally {
    conn.close();
  }
});

test("Connection.dial -- guest token travels as subprotocol, never on the URL (#2511)", async () => {
  const { conn, socket, protocols } = await dialWith({ guestToken: "invite-abc" });
  try {
    assert.deepEqual(protocols, ["guest", "invite-abc"]);
    assert.ok(!socket.url.includes("invite-abc"), `url leaks the guest token: ${socket.url}`);
  } finally {
    conn.close();
  }
});

test("Connection.dial -- no auth offers no subprotocols", async () => {
  const { conn, protocols } = await dialWith({});
  try {
    assert.equal(protocols, undefined);
  } finally {
    conn.close();
  }
});
